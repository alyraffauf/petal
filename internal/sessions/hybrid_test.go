package sessions

import (
	"errors"
	"net/http"
	"os"
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
	for _, c := range []struct{ q, want string }{{"ERR_SOCKET_901", docs[0]}, {"KEY_ROTATE_712", docs[3]}, {"car battery replacement", docs[1]}, {"storing extra soup for another meal", docs[2]}, {"lunar volcanic geology", ""}, {"medieval tapestry weaving", ""}} {
		f, err := SearchHistory(t.Context(), cwd, "p", c.q, 0)
		if err != nil {
			t.Fatal(err)
		}
		h, err := SearchHybridHistory(t.Context(), cwd, "p", c.q)
		if err != nil || h.Retrieval != "hybrid" {
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
	old := embeddingClient
	embeddingClient = &http.Client{Transport: unavailableProvider{}}
	defer func() { embeddingClient = old }()
	h, err = SearchHybridHistory(t.Context(), cwd, "p", "replacement")
	if err != nil || h.Retrieval != "fts" || len(h.Hits) != 1 {
		t.Fatal(h, err)
	}
}
