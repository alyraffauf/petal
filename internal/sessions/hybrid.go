package sessions

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"
)

var embeddingClient = &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
var errEmbeddingProvider = errors.New("local embedding provider unavailable")
var embeddingSlot = make(chan struct{}, 1)

const historyModel = "all-minilm:latest"

func modelDigest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:11434/api/tags", nil)
	if err != nil {
		return "", err
	}
	res, err := embeddingClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var tags struct {
		Models []struct{ Name, Digest string }
	}
	if res.StatusCode == 200 && json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tags) == nil {
		for _, m := range tags.Models {
			if m.Name == historyModel && len(m.Digest) == 64 {
				if _, err := hex.DecodeString(m.Digest); err == nil {
					return m.Digest, nil
				}
			}
		}
	}
	return "", errEmbeddingProvider
}

func embedHistory(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": historyModel, "input": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:11434/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := embeddingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errEmbeddingProvider
	}
	var reply struct {
		Model      string
		Embeddings [][]float32
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&reply); err != nil {
		return nil, err
	}
	if reply.Model != historyModel {
		return nil, errEmbeddingProvider
	}
	if len(reply.Embeddings) != len(inputs) {
		return nil, errors.New("incomplete embeddings")
	}
	for _, v := range reply.Embeddings {
		if len(v) != 384 {
			return nil, errors.New("unexpected embedding dimensions")
		}
	}
	return reply.Embeddings, nil
}

// Backfill eight missing vectors per call. Compare the input again at write
// time because a transcript can change while Ollama is running.
const window = historyScope + `, recent AS (SELECT rowid AS ref FROM messages WHERE session_id IN (SELECT id FROM scope) ORDER BY rowid DESC LIMIT 256) `

func backfill(ctx context.Context, db *sql.DB, parent, query, digest string) ([]float32, error) {
	rows, err := db.QueryContext(ctx, window+`SELECT ref,substr(body,1,1024) FROM recent JOIN messages ON rowid=ref LEFT JOIN message_embeddings ON reference=ref AND digest=? WHERE reference IS NULL ORDER BY ref DESC LIMIT 8`, parent, digest)
	if err != nil {
		return nil, err
	}
	var ids []int64
	inputs := []string{query}
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		inputs = append(inputs, body)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	vectors, err := embedHistory(ctx, inputs)
	if err != nil {
		return nil, errors.Join(errEmbeddingProvider, err)
	}
	current, err := modelDigest(ctx)
	if err != nil || current != digest {
		return nil, errEmbeddingProvider
	}
	for i, id := range ids {
		buf := new(bytes.Buffer)
		binary.Write(buf, binary.LittleEndian, vectors[i+1])
		_, err = db.ExecContext(ctx, historyScope+`INSERT OR REPLACE INTO message_embeddings(reference,vector,digest) SELECT rowid,?,? FROM messages WHERE rowid=? AND session_id IN (SELECT id FROM scope) AND substr(body,1,1024)=?`, parent, buf.Bytes(), digest, id, inputs[i+1])
		if err != nil {
			return nil, err
		}
	}
	return vectors[0], nil
}

func scoreHistory(ctx context.Context, db historyReader, parent, digest string, queryVector []float32, coverage *HistoryCoverage) ([]HistoryMatch, error) {
	rows, err := db.QueryContext(ctx, window+`SELECT ref,session_id,role,substr(body,1,512),vector FROM recent JOIN messages ON messages.rowid=ref LEFT JOIN message_embeddings ON reference=ref AND digest=? ORDER BY ref DESC`, parent, digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		hit   HistoryMatch
		score float64
	}
	var ranked []scored
	for rows.Next() {
		var hit HistoryMatch
		var blob []byte
		if err := rows.Scan(&hit.Reference, &hit.Session, &hit.Role, &hit.Snippet, &blob); err != nil {
			return nil, err
		}
		coverage.Window++
		if len(blob) != 384*4 {
			continue
		}
		coverage.Indexed++
		hit.Source = "semantic"
		var score float64
		// Ollama /api/embed returns unit vectors.
		for i, q := range queryVector {
			score += float64(q) * float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:])))
		}
		if score >= 0.4 {
			ranked = append(ranked, scored{hit, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	var hits []HistoryMatch
	for _, r := range ranked {
		hits = append(hits, r.hit)
	}
	return hits, rows.Err()
}

func HybridHistoryEnabled() bool { return os.Getenv("PETAL_HISTORY_EMBEDDINGS") == "1" }

// Structured identifiers need lexical evidence even when nearby codes embed similarly.
var historyIdentifier = regexp.MustCompile(`[[:alnum:]]_[[:alnum:]]|[[:alpha:]][[:digit:]]|[[:digit:]][[:alpha:]]|[[:alnum:]]-[[:digit:]]|#[[:digit:]]|\b[[:digit:]]+\b`)

func optionalHistoryVector(ctx context.Context, db *sql.DB, parent, query string) ([]float32, string, error) {
	select {
	case embeddingSlot <- struct{}{}:
		defer func() { <-embeddingSlot }()
	default:
		return nil, "", nil
	}
	// Reserve the final tenth of the remaining budget for the scoped read.
	deadline, _ := ctx.Deadline()
	optional, stop := context.WithDeadline(ctx, deadline.Add(-time.Until(deadline)/10))
	defer stop()
	digest, err := modelDigest(optional)
	if err != nil {
		return nil, "", ctx.Err()
	}
	// This handle belongs to this search. Yield immediately to an archive writer.
	if _, err = db.ExecContext(optional, "PRAGMA busy_timeout=0"); err != nil {
		if optional.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", err
	}
	vector, err := backfill(optional, db, parent, query, digest)
	var sqliteErr *sqlite.Error
	busy := errors.As(err, &sqliteErr) && sqliteErr.Code()&255 == sqlite3.SQLITE_BUSY
	if busy || errors.Is(err, errEmbeddingProvider) || optional.Err() != nil {
		return nil, "", ctx.Err()
	}
	return vector, digest, err
}

func SearchHybridHistory(ctx context.Context, cwd, parent, query string) (HistoryMatches, error) {
	return searchHybridHistory(ctx, cwd, parent, query, 10*time.Second)
}

func searchHybridHistory(caller context.Context, cwd, parent, query string, budget time.Duration) (HistoryMatches, error) {
	ctx, cancel := context.WithTimeout(caller, budget)
	defer cancel()
	var page HistoryMatches
	if !HybridHistoryEnabled() {
		return page, errors.New("enable hybrid history with PETAL_HISTORY_EMBEDDINGS=1")
	}
	if utf8.RuneCountInString(query) > 512 {
		return page, errors.New("invalid history query length")
	}
	expr := matchExpression(query)
	if expr == "" {
		return page, errors.New("invalid history query")
	}
	db, err := openStoreContext(ctx, cwd)
	if err != nil {
		return page, err
	}
	defer db.Close()
	var digest string
	var vector []float32
	if !historyIdentifier.MatchString(query) {
		vector, digest, err = optionalHistoryVector(ctx, db, parent, query)
		if err != nil {
			return page, err
		}
	}
	if err := caller.Err(); err != nil {
		return page, err
	}
	// Optional cache writes yield immediately. Give the authoritative read
	// the remaining request budget to wait for a concurrent writer.
	deadline, _ := ctx.Deadline()
	waitMS := max(int64(0), time.Until(deadline).Milliseconds())
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout="+strconv.FormatInt(waitMS, 10)); err != nil {
		return page, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	page, err = searchHistory(ctx, tx, parent, expr, 0)
	if err != nil {
		return page, err
	}
	page.Retrieval = "fts"
	page.NextBefore = 0
	for i := range page.Hits {
		page.Hits[i].Source = "keyword"
	}
	if vector == nil {
		return page, ctx.Err()
	}
	var coverage HistoryCoverage
	semantic, err := scoreHistory(ctx, tx, parent, digest, vector, &coverage)
	if err != nil {
		return page, err
	}
	coverage.Partial = coverage.Indexed < coverage.Window
	page.Coverage = &coverage
	if coverage.Partial {
		page.Next = "Repeat this hybrid query to index up to eight more messages in the recent window."
	}
	page.Retrieval = "hybrid"
	seen := map[int64]bool{}
	lexical := page.Hits
	page.Hits = []HistoryMatch{}
	for i := 0; i < len(lexical) || i < len(semantic); i++ {
		for _, list := range [][]HistoryMatch{lexical, semantic} {
			if i < len(list) && !seen[list[i].Reference] {
				hit := list[i]
				seen[hit.Reference] = true
				page.Hits = append(page.Hits, hit)
				if len(page.Hits) == 10 {
					return page, ctx.Err()
				}
			}
		}
	}
	return page, ctx.Err()
}
