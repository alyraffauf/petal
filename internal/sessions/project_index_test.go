package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"petal/internal/embeddings"
)

type projectIndexFixture struct {
	cwd         string
	db          *sql.DB
	cfg         embeddings.Settings
	tag         string
	model       string
	dimensions  int
	beforeReply func(*http.Request)
}

func newProjectIndexFixture(t *testing.T, provider string) *projectIndexFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &projectIndexFixture{
		cwd: t.TempDir(), tag: "a", model: "all-minilm:latest", dimensions: 2,
		cfg: embeddings.Settings{Provider: provider, BaseURL: "http://127.0.0.1:11434", Model: "all-minilm:latest"},
	}
	for _, account := range []string{"work", "personal"} {
		if _, err := Create(f.cwd, Record{ID: account, Account: account}); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	f.db, err = openStore(f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	old := embeddingClient
	t.Cleanup(func() { embeddingClient = old })
	embeddingClient = &http.Client{Transport: historyTransport(f.respond)}
	return f
}

const projectIndexQuery = "find concept"

func (f *projectIndexFixture) respond(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/api/tags" {
		return tagReply(f.tag), nil
	}
	defer r.Body.Close()
	var request struct{ Input []string }
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return nil, err
	}
	var native [][]float32
	var compatible []map[string]any
	for i, input := range request.Input {
		vector := make([]float32, f.dimensions)
		if input == projectIndexQuery || strings.HasPrefix(input, "relevant") {
			vector[0] = 1
		} else {
			vector[1] = 1
		}
		native = append(native, vector)
		compatible = append(compatible, map[string]any{"index": i, "embedding": vector})
	}
	payload, err := json.Marshal(map[string]any{"model": f.model, "embeddings": native, "data": compatible})
	if err != nil {
		return nil, err
	}
	if len(request.Input) != 1 || request.Input[0] != projectIndexQuery {
		if f.beforeReply != nil {
			f.beforeReply(r)
		}
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
}

func (f *projectIndexFixture) prepare(t *testing.T, account string) *ProjectSearch {
	t.Helper()
	s := PrepareProjectSearch(t.Context(), f.cwd, account, projectIndexQuery, f.cfg)
	if s == nil {
		t.Fatal("missing query embedding")
	}
	return s
}

func (f *projectIndexFixture) append(t *testing.T, account, body string) {
	t.Helper()
	if err := SaveTranscript(f.cwd, account, []Message{{Role: "assistant", Body: body}}, false); err != nil {
		t.Fatal(err)
	}
}

func (f *projectIndexFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestProjectBackfillRejectsDifferentQueryModel(t *testing.T) {
	for _, change := range []string{"ollama tag", "ollama during embed", "compatible model", "compatible dimensions"} {
		t.Run(change, func(t *testing.T) {
			provider := "openai-compatible"
			if strings.HasPrefix(change, "ollama") {
				provider = "ollama"
			}
			f := newProjectIndexFixture(t, provider)
			f.append(t, "work", "relevant document")
			search := f.prepare(t, "work")
			switch change {
			case "ollama tag":
				f.tag = "b"
			case "ollama during embed":
				f.beforeReply = func(*http.Request) { f.tag = "b" }
			case "compatible model":
				f.model = "other-model"
			case "compatible dimensions":
				f.dimensions = 3
			}
			if more, err := search.Backfill(t.Context(), time.Hour); err == nil || more {
				t.Fatalf("changed model returned more=%v, err=%v", more, err)
			}
			var count int
			if err := f.db.QueryRow("SELECT count(*) FROM message_embeddings").Scan(&count); err != nil || count != 0 {
				t.Fatalf("mixed-model batch wrote %d embeddings, err=%v", count, err)
			}
			f.beforeReply = nil
			current := f.prepare(t, "work")
			if more, err := current.Backfill(t.Context(), time.Hour); err != nil || more {
				t.Fatalf("retry with current model returned more=%v, err=%v", more, err)
			}
			if hits, coverage, err := current.Matches(t.Context()); err != nil || len(hits) != 1 || coverage.Indexed != 1 {
				t.Fatalf("retry returned hits=%v, coverage=%+v, err=%v", hits, coverage, err)
			}
		})
	}
}
func TestProjectBackfillRetriesChangedShortBatch(t *testing.T) {
	for _, change := range []string{"edit", "append"} {
		t.Run(change, func(t *testing.T) {
			f := newProjectIndexFixture(t, "openai-compatible")
			f.append(t, "work", "unrelated document")
			search := f.prepare(t, "work")
			f.beforeReply = func(*http.Request) {
				f.beforeReply = nil
				if change == "edit" {
					f.exec(t, "UPDATE messages SET body='relevant replacement'")
				} else {
					f.append(t, "work", "relevant new document")
				}
			}
			if more, err := search.Backfill(t.Context(), 0); err != nil || !more {
				t.Fatalf("bounded backfill returned more=%v, err=%v", more, err)
			}
			if more, err := search.Backfill(t.Context(), time.Hour); err != nil || more {
				t.Fatalf("retry returned more=%v, err=%v", more, err)
			}
			hits, coverage, err := search.Matches(t.Context())
			if err != nil || coverage.Indexed != coverage.Window || len(hits) != 1 || !strings.HasPrefix(hits[0].Snippet, "relevant") {
				t.Fatalf("retry returned hits=%v, coverage=%+v, err=%v", hits, coverage, err)
			}
		})
	}
}

func TestProjectBackfillCancellationReleasesIndexSlot(t *testing.T) {
	f := newProjectIndexFixture(t, "openai-compatible")
	f.append(t, "work", "relevant document")
	search := f.prepare(t, "work")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.beforeReply = func(*http.Request) { cancel() }
	if more, err := search.Backfill(ctx, time.Hour); more || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backfill returned more=%v, err=%v", more, err)
	}
	if hits, coverage, err := search.Matches(t.Context()); err != nil || len(hits) != 0 || coverage.Indexed != 0 {
		t.Fatalf("canceled batch returned hits=%v, coverage=%+v, err=%v", hits, coverage, err)
	}
	f.beforeReply = nil
	retry, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	if more, err := search.Backfill(retry, time.Hour); more || err != nil {
		t.Fatalf("retry returned more=%v, err=%v", more, err)
	}
	if hits, coverage, err := search.Matches(t.Context()); err != nil || len(hits) != 1 || coverage.Indexed != 1 {
		t.Fatalf("retry returned hits=%v, coverage=%+v, err=%v", hits, coverage, err)
	}
}
