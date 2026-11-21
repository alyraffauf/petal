package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

type unavailableProvider struct{}

func (unavailableProvider) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("offline")
}

func TestLocalHybridEvaluation(t *testing.T) {
	if os.Getenv("PETAL_EVAL_EMBEDDINGS") != "1" {
		t.Skip("requires local Ollama all-minilm")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PETAL_HISTORY_EMBEDDINGS", "1")
	cwd := t.TempDir()
	for _, id := range []string{"p", "child", "other"} {
		if _, err := Create(cwd, Record{ID: id, Model: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openStore(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("INSERT INTO child_bindings(child_id,parent_id) VALUES ('child','p')"); err != nil {
		t.Fatal(err)
	}
	docs := []string{"Retry ERR_SOCKET_901 when connecting.", "The automobile needs a new battery.", "Freeze the leftover soup for dinner tomorrow.", "Rotate the database encryption keys using KEY_ROTATE_712."}
	for i, body := range docs {
		session := "p"
		if i == 1 {
			session = "child"
		}
		if err := SaveTranscript(cwd, session, []Message{{Role: "tool", Body: body}}, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveTranscript(cwd, "other", []Message{{Role: "tool", Body: "car battery replacement"}}, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ q, want string }{{"ERR_SOCKET_901", docs[0]}, {"KEY_ROTATE_712", docs[3]}, {"ERR_SOCKET_902", ""}, {"car battery replacement", docs[1]}, {"storing extra soup for another meal", docs[2]}, {"lunar volcanic geology", ""}, {"medieval tapestry weaving", ""}} {
		f, err := SearchHistory(t.Context(), cwd, "p", c.q, 0)
		if err != nil {
			t.Fatal(err)
		}
		h, err := SearchHybridHistory(t.Context(), cwd, "p", c.q)
		wantRetrieval := "hybrid"
		if strings.Contains(c.q, "_") {
			wantRetrieval = "fts"
		}
		if err != nil || h.Retrieval != wantRetrieval {
			t.Fatal(h, err)
		}
		t.Logf("%q FTS=%v hybrid=%v", c.q, f.Hits, h.Hits)
		if c.want == "" {
			if len(h.Hits) > 0 {
				t.Fatal(h)
			}
			continue
		}
		if len(h.Hits) == 0 {
			t.Fatal("missing", c.want)
		}
		p, err := ReadHistory(t.Context(), cwd, "p", h.Hits[0].Reference, 0)
		if err != nil || p.Text != c.want {
			t.Fatal(p, err)
		}
	}
	count := func(want int) {
		t.Helper()
		var n int
		if err := db.QueryRow("SELECT count(*) FROM message_embeddings").Scan(&n); err != nil || n != want {
			t.Fatal(n, want, err)
		}
	}
	count(4)
	if _, err = db.Exec("UPDATE messages SET body='replacement' WHERE body=?", docs[1]); err != nil {
		t.Fatal(err)
	}
	count(3)
	if _, err = db.Exec("DELETE FROM messages WHERE body=?", docs[0]); err != nil {
		t.Fatal(err)
	}
	count(2)
	h, err := SearchHybridHistory(t.Context(), cwd, "p", "car battery replacement")
	if err != nil || h.Retrieval != "hybrid" {
		t.Fatal(h, err)
	}
	count(3)
	// Force a cold window with an older semantic answer and a recent literal distractor.
	if _, err = db.Exec("DELETE FROM message_embeddings"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		body := fmt.Sprintf("Record the tide height for day %d.", i)
		if i == 0 {
			body = "The search index lists storing extra soup for another meal as an example query."
		}
		if err := SaveTranscript(cwd, "p", []Message{{Role: "tool", Body: body}}, true); err != nil {
			t.Fatal(err)
		}
	}
	q := "storing extra soup for another meal"
	cold, err := SearchHybridHistory(t.Context(), cwd, "p", q)
	if err != nil || cold.Coverage == nil || *cold.Coverage != (HistoryCoverage{Indexed: 8, Window: 11, Partial: true}) || cold.Next == "" {
		t.Fatal(cold, err)
	}
	for _, hit := range cold.Hits {
		if hit.Snippet == docs[2] {
			t.Fatalf("unindexed answer returned: %v", hit)
		}
	}
	warm, err := SearchHybridHistory(t.Context(), cwd, "p", q)
	if err != nil || warm.Coverage == nil || warm.Coverage.Partial || warm.Coverage.Indexed != 11 || len(warm.Hits) < 2 || warm.Hits[0].Source != "keyword" {
		t.Fatal(warm, err)
	}
	found := false
	for _, hit := range warm.Hits {
		if hit.Snippet == docs[2] && hit.Source == "semantic" {
			found = true
		}
	}
	if !found {
		t.Fatal(warm)
	}
	t.Logf("cold=%+v warm=%+v hits=%v", cold.Coverage, warm.Coverage, warm.Hits)
	old := embeddingClient
	embeddingClient = &http.Client{Transport: unavailableProvider{}}
	defer func() { embeddingClient = old }()
	h, err = SearchHybridHistory(t.Context(), cwd, "p", "replacement")
	if err != nil || h.Retrieval != "fts" || len(h.Hits) != 1 {
		t.Fatal(h, err)
	}
}

type historyTransport func(*http.Request) (*http.Response, error)

func (f historyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHistoryEmbeddingBoundary(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	prefix := strings.Repeat("猫", 1024)
	for _, entry := range []struct{ id, body string }{{"parent", "parent text"}, {"child", prefix + "private suffix"}, {"other", "unrelated secret"}} {
		if _, err := Create(cwd, Record{ID: entry.id, Model: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := SaveTranscript(cwd, entry.id, []Message{{Role: "tool", Body: entry.body}}, true); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openStore(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("INSERT INTO child_bindings(child_id,parent_id) VALUES ('child','parent')"); err != nil {
		t.Fatal(err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect") }))
	defer redirect.Close()
	client := *embeddingClient
	calls := 0
	client.Transport = historyTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "http://127.0.0.1:11434/api/embed" || r.Method != "POST" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, map[string]any{"model": "all-minilm:latest", "input": []any{"parent", prefix, "parent text"}}) {
			t.Fatal(got)
		}
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": {redirect.URL}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	old := embeddingClient
	embeddingClient = &client
	t.Cleanup(func() { embeddingClient = old })
	t.Setenv("PETAL_HISTORY_EMBEDDINGS", "0")
	if _, err := SearchHybridHistory(t.Context(), cwd, "parent", "parent"); err == nil {
		t.Fatal("disabled hybrid accepted")
	}
	t.Setenv("PETAL_HISTORY_EMBEDDINGS", "1")
	if _, err := SearchHistory(t.Context(), cwd, "parent", "parent", 0); err != nil || calls != 0 {
		t.Fatal(err, calls)
	}
	page, err := SearchHybridHistory(t.Context(), cwd, "parent", "parent")
	if err != nil || calls != 1 || page.Retrieval != "fts" || len(page.Hits) != 1 {
		t.Fatal(page, err, calls)
	}
}
