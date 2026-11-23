package sessions

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"petal/internal/embeddings"
)

// Run against a real local embedding model so paraphrase ranking is meaningful.
func TestLocalProjectSemanticSearch(t *testing.T) {
	if os.Getenv("PETAL_EVAL_EMBEDDINGS") != "1" {
		t.Skip("requires local Ollama all-minilm")
	}
	for _, cfg := range []embeddings.Settings{{Provider: "ollama"}, {Provider: "openai-compatible", BaseURL: "http://127.0.0.1:11434/v1", Model: "all-minilm:latest"}} {
		t.Run(cfg.Provider, func(t *testing.T) { evaluateProjectSearch(t, cfg) })
	}
}
func evaluateProjectSearch(t *testing.T, cfg embeddings.Settings) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	const target = "The airline reimbursed the canceled flight."
	for i, account := range []string{"work", "work", "personal"} {
		id := fmt.Sprint(i)
		if _, err := Create(cwd, Record{ID: id, Account: account}); err != nil {
			t.Fatal(err)
		}
		body := target
		if i == 1 {
			body = "Plant the tulip bulbs before the first frost."
		}
		if err := SaveTranscript(cwd, id, []Message{{Role: "assistant", Body: body}}, false); err != nil {
			t.Fatal(err)
		}
	}
	prepare := func(account, query string) *ProjectSearch {
		t.Helper()
		s := PrepareProjectSearch(t.Context(), cwd, account, query, cfg)
		if s == nil {
			t.Fatal("missing query vector", query)
		}
		return s
	}
	started := time.Now()
	s := prepare("work", "airfare refund")
	hits, cold, err := s.Matches(t.Context())
	if err != nil || len(hits) != 0 || cold.Window != 2 || cold.Indexed != 0 {
		t.Fatal(cold, hits, err)
	}
	if _, err := s.Backfill(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	hits, warm, err := s.Matches(t.Context())
	if err != nil || len(hits) != 1 || hits[0].Account != "work" || hits[0].Snippet != target || warm.Indexed != 2 {
		t.Fatal(warm, hits, err)
	}
	t.Log("cold query + backfill + result:", time.Since(started))
	s = prepare("", "airfare refund")
	if _, err = s.Backfill(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	hits, warm, err = s.Matches(t.Context())
	if err != nil || len(hits) != 2 || warm.Indexed != 3 {
		t.Fatal(warm, hits, err)
	}
	for _, q := range []string{`"airfare refund"`, "ERR_FLIGHT_902"} {
		if prepareExact := PrepareProjectSearch(t.Context(), cwd, "", q, cfg); prepareExact != nil {
			t.Fatal(q)
		}
	}
	lexical := prepare("work", "canceled flight")
	hits, _, err = lexical.Matches(t.Context())
	if err != nil || len(hits) != 0 {
		t.Fatal("duplicated keyword hit", hits, err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if _, _, err = s.Matches(canceled); err == nil {
		t.Fatal("ignored cancellation")
	}
	for _, cfg := range []embeddings.Settings{{Provider: "off"}, {Provider: "ollama", BaseURL: "http://127.0.0.1:1"}} {
		if PrepareProjectSearch(t.Context(), cwd, "", "airfare refund", cfg) != nil {
			t.Fatal("expected keyword fallback")
		}
	}
	unrelated := prepare("", "medieval tapestry weaving")
	hits, _, err = unrelated.Matches(t.Context())
	if err != nil || len(hits) != 0 {
		t.Fatal(hits, err)
	}
}

func TestProjectMatchesDeduplicatesBeforeTopTen(t *testing.T) {
	// All scores tie. The last row is a keyword hit and would otherwise
	// displace the oldest of the ten semantic-only rows.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := Create(cwd, Record{ID: "thread", Account: "work"}); err != nil {
		t.Fatal(err)
	}
	var messages []Message
	for range 10 {
		messages = append(messages, Message{Role: "assistant", Body: "related"})
	}
	messages = append(messages, Message{Role: "assistant", Body: "needle"})
	if err := SaveTranscript(cwd, "thread", messages, false); err != nil {
		t.Fatal(err)
	}
	transcript, err := Transcript(cwd, "thread")
	if err != nil || len(transcript) != 11 {
		t.Fatalf("expected eleven transcript rows, got %d: %v", len(transcript), err)
	}
	db, err := openStore(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const digest = "test-model"
	vector := binary.LittleEndian.AppendUint32(nil, math.Float32bits(1))
	_, err = db.Exec(`INSERT INTO message_embeddings(reference,digest,vector) SELECT rowid,?,? FROM messages`, digest, vector)
	if err != nil {
		t.Fatal(err)
	}

	search := ProjectSearch{cwd: cwd, account: "work", digest: digest, vector: []float32{1}, expression: `"needle"`}
	hits, coverage, err := search.Matches(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 10 || hits[9].Row != transcript[0].Rowid {
		t.Fatalf("expected ten semantic-only matches including the oldest row, got %+v", hits)
	}
	if coverage.Indexed != 11 || coverage.Window != 11 {
		t.Fatalf("keyword hit should count toward coverage, got %+v", coverage)
	}
}
