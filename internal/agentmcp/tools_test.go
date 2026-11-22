package agentmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"testing"

	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestHistorySearchAdvertisement(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var list struct {
			Tools []struct{ Name string }
		}
		if err := json.Unmarshal(delegationToolList(enabled), &list); err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tool := range list.Tools {
			names[tool.Name] = true
		}
		if !names["search"] || names["recall"] != enabled {
			t.Fatalf("embeddings enabled=%v: %v", enabled, names)
		}
	}
}

func TestConfiguredEmbeddingSearch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PETAL_TEST_EMBED_KEY", "fixture")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("incorrect endpoint or credential")
			http.Error(w, "denied", 401)
			return
		}
		var req struct {
			Model string
			Input []string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "MixedCase" || (len(req.Input) != 1 || (req.Input[0] != "battery" && req.Input[0] != "vehicle")) {
			t.Errorf("unexpected request: %+v", req)
			http.Error(w, "bad request", 400)
			return
		}
		io.WriteString(w, `{"model":"MixedCase-v2","data":[{"index":0,"embedding":[0,2]}]}`)
	}))
	defer srv.Close()
	cwd := t.TempDir()
	file := filepath.Join(cwd, "config.toml")
	if err := os.WriteFile(file, []byte(fmt.Sprintf("[history.embeddings]\nprovider='openai-compatible'\nbase_url='%s/v1'\nmodel='MixedCase'\napi_key_env='PETAL_TEST_EMBED_KEY'\n", srv.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := routing.Load(file, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessions.Create(cwd, sessions.Record{ID: "p", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err = sessions.SaveTranscript(cwd, "p", []sessions.Message{{Role: "user", Body: "vehicle"}}, false); err != nil {
		t.Fatal(err)
	}
	endpoint := server{cwd: cwd}
	tools := Tools{Embeddings: cfg.History.Embeddings}
	record := sessions.Record{ID: "p"}
	query := json.RawMessage(`{"query":"battery"}`)
	value, _, err := endpoint.invoke(t.Context(), tools, record, "search", query)
	if err != nil || len(value.(sessions.HistoryMatches).Hits) != 0 {
		t.Fatalf("unexpected keyword result: %v, %v", value, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); sessions.IndexHistory(ctx, cwd, "p", tools.Embeddings) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(3 * time.Second); ; {
		value, _, err = endpoint.invoke(t.Context(), tools, record, "recall", query)
		if err != nil || len(value.(sessions.HistoryMatches).Hits) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	page := value.(sessions.HistoryMatches)
	if page.Retrieval != "hybrid" || len(page.Hits) != 1 || page.Hits[0].Snippet != "vehicle" || page.Hits[0].Source != "semantic" {
		t.Fatal(page)
	}
}
