package sessions

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"testing"
)

const projectRetrievalDigest = "test-model"

type projectRetrievalRow struct {
	hit     Hit
	keyword bool
	indexed bool
	score   float32
}

func FuzzProjectRetrieval(f *testing.F) {
	f.Add(uint8(0), append(bytes.Repeat([]byte{0x1c}, 10), 0x3c))
	f.Add(uint8(1), append(bytes.Repeat([]byte{0x3c}, 42), 0x1c, 0x1d, 0x1e))
	f.Add(uint8(2), []byte{0x15, 0x19, 0x1d, 0x3d, 0x1e, 0x42, 0x83})
	f.Add(uint8(0), []byte{0x10, 0x1e, 0x14, 0x0c, 0x08, 0x5c, 0x9c, 0xdc})
	f.Add(uint8(3), []byte{0x1c})
	f.Add(uint8(0), []byte{})
	f.Fuzz(checkProjectRetrieval)
}

func TestProjectRetrievalModel(t *testing.T) {
	random := rand.New(rand.NewSource(71))
	for trial := 0; trial < 48; trial++ {
		trace := make([]byte, random.Intn(81))
		random.Read(trace)
		scope := uint8(random.Intn(4))
		t.Run(strconv.Itoa(trial), func(t *testing.T) {
			checkProjectRetrieval(t, scope, trace)
		})
	}
}

func checkProjectRetrieval(t *testing.T, scope uint8, trace []byte) {
	t.Helper()
	trace = trace[:min(len(trace), 80)]
	account := []string{"", "work", "personal", "missing"}[scope%4]
	cwd, rows := populateProjectRetrieval(t, trace)
	search := ProjectSearch{cwd: cwd, account: account, digest: projectRetrievalDigest, vector: []float32{1, 0}, expression: `"needle"`}
	hits, coverage, err := search.Matches(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	var semantic []projectRetrievalRow
	var lexical []int64
	window, indexed := 0, 0
	for _, row := range rows {
		if account != "" && account != row.hit.Account {
			continue
		}
		window++
		if row.keyword {
			lexical = append(lexical, row.hit.Row)
		}
		if row.indexed {
			indexed++
			// The fixtures include scores below and above the cutoff.
			// Compare in float64, the precision used for the cutoff.
			if !row.keyword && float64(row.score) >= 0.4 {
				semantic = append(semantic, row)
			}
		}
	}
	slices.Reverse(semantic)
	sort.SliceStable(semantic, func(i, j int) bool { return semantic[i].score > semantic[j].score })
	semantic = semantic[:min(len(semantic), 10)]
	want := make([]Hit, 0, len(semantic))
	for _, row := range semantic {
		want = append(want, row.hit)
	}
	if coverage.Window != window || coverage.Indexed != indexed || !slices.Equal(hits, want) {
		t.Fatalf("scope %q trace %x: coverage %+v, want %d/%d indexed; hits %+v, want %+v", account, trace, coverage, indexed, window, hits, want)
	}

	// Equal lexical bodies tie at the same FTS rank. Each page should continue
	// the descending row order, even when semantic results fill the first page.
	slices.Reverse(lexical)
	const pageSize = 40
	for offset := 0; offset <= len(lexical)+pageSize; offset += pageSize {
		page, err := Search(cwd, account, "needle", offset, pageSize+1)
		if err != nil {
			t.Fatal(err)
		}
		var references []int64
		for _, hit := range page {
			references = append(references, hit.Row)
		}
		expected := lexical[min(offset, len(lexical)):min(offset+pageSize+1, len(lexical))]
		if !slices.Equal(references, expected) {
			t.Fatalf("scope %q offset %d trace %x: lexical rows %v, want %v", account, offset, trace, references, expected)
		}
	}
}

func populateProjectRetrieval(t *testing.T, trace []byte) (string, []projectRetrievalRow) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	accounts := []string{"work", "work", "personal", ""}
	for i, account := range accounts {
		_, err := Create(cwd, Record{ID: strconv.Itoa(i), Title: "Session " + strconv.Itoa(i), Account: account, Model: "test"})
		if err != nil {
			t.Fatal(err)
		}
	}
	db, err := openStore(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	scores := []float32{-1, 0, 0.39, 0.4, 0.5, 0.6, 0.8, 1}
	var rows []projectRetrievalRow
	for _, flags := range trace {
		session := strconv.Itoa(int(flags & 3))
		keyword := flags&0x20 != 0
		body := "related"
		if keyword {
			body = "needle"
		}
		role := "assistant"
		if flags&4 != 0 {
			role = "user"
		}
		var ref int64
		err := tx.QueryRow(`INSERT INTO messages(session_id,role,body) VALUES (?,?,?) RETURNING rowid`, session, role, body).Scan(&ref)
		if err != nil {
			t.Fatal(err)
		}
		score := scores[(flags>>2)&7]
		if flags>>6 != 3 {
			digest := projectRetrievalDigest
			vector := []float32{score, float32(math.Sqrt(1 - float64(score)*float64(score)))}
			if flags>>6 == 1 {
				digest = "previous-model"
			}
			if flags>>6 == 2 {
				vector = vector[:1]
			}
			buf := new(bytes.Buffer)
			if err := binary.Write(buf, binary.LittleEndian, vector); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`INSERT INTO message_embeddings(reference,digest,vector) VALUES (?,?,?)`, ref, digest, buf.Bytes()); err != nil {
				t.Fatal(err)
			}
		}
		rows = append(rows, projectRetrievalRow{
			hit: Hit{Semantic: true, Row: ref, Session: session, Title: fmt.Sprintf("Session %s", session),
				Model: "test", Account: accounts[flags&3], Role: role, Snippet: body},
			keyword: keyword, indexed: flags>>6 == 0, score: score,
		})
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return cwd, rows
}
