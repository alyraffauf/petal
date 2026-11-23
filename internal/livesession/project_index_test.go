//go:build linux || darwin

package livesession

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"petal/internal/embeddings"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestProjectIndexDrainsAfterLastPickerDisconnects(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	for _, account := range []string{"work", "personal"} {
		if _, err := sessions.Create(cwd, sessions.Record{ID: account, Account: account}); err != nil {
			t.Fatal(err)
		}
		if err := sessions.SaveTranscript(cwd, account, []sessions.Message{{Role: "user", Body: account}}, false); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	inputs := make(chan string, 4)
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Input []string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Input) != 1 {
			t.Errorf("unexpected embedding batch %v: %v", request.Input, err)
			http.Error(w, "bad input", 400)
			return
		}
		inputs <- request.Input[0]
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-ctx.Done():
			return
		}
		_, _ = w.Write([]byte(`{"model":"fixture","data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer provider.Close()
	cfg := routing.Config{}
	cfg.History.Embeddings = embeddings.Settings{Provider: "openai-compatible", BaseURL: provider.URL, Model: "fixture"}
	server := NewServer(ctx, func(OpenOptions) (routing.Config, error) { return cfg, nil })
	defer server.Close()
	connect := func() *Client {
		a, b := net.Pipe()
		go func() { defer b.Close(); server.Handler()(ctx, b) }()
		c, err := Connect(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.ListProject(ctx, cwd, "work"); err != nil {
			t.Fatal(err)
		}
		return c
	}
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-inputs:
			if got != want {
				t.Fatalf("embedded %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	a, b := connect(), connect()
	expect("work")
	if err := sessions.SaveTranscript(cwd, "work", []sessions.Message{{Role: "user", Body: "last message"}}, false); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b.Close()
	close(release)
	expect("last message")
}

type indexingPeer struct {
	*Client
	done chan struct{}
}

func dialIndexingPeer(t *testing.T, ctx context.Context, server *Server) indexingPeer {
	t.Helper()
	local, remote := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer remote.Close()
		server.Handler()(ctx, remote)
	}()
	t.Cleanup(func() { local.Close() })
	client, err := Connect(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	return indexingPeer{client, done}
}

func (p indexingPeer) disconnect(t *testing.T, ctx context.Context) {
	t.Helper()
	p.Close()
	select {
	case <-p.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func saveIndexMessage(t *testing.T, cwd, account, body string) {
	t.Helper()
	if _, err := sessions.Create(cwd, sessions.Record{ID: account, Account: account}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SaveTranscript(cwd, account, []sessions.Message{{Role: "user", Body: body}}, false); err != nil {
		t.Fatal(err)
	}
}

type embeddingRequest struct {
	Model string
	Input []string
}

func indexingProvider(t *testing.T, observe func(*http.Request, embeddingRequest)) embeddings.Settings {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		observe(r, request)
		data := make([]map[string]any, len(request.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float32{1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "data": data})
	}))
	t.Cleanup(provider.Close)
	return embeddings.Settings{Provider: "openai-compatible", BaseURL: provider.URL, Model: "fixture"}
}

func awaitIndexSignal(t *testing.T, ctx context.Context, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitForIndexWorkers(t *testing.T, ctx context.Context, server *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		server.workers.Wait()
		close(done)
	}()
	awaitIndexSignal(t, ctx, done)
}

func TestProjectWorkersPreserveEmbeddingsForDifferentSettings(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	saveIndexMessage(t, cwd, "work", "archive message")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	allowFirst, allowSecond := make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	settings := indexingProvider(t, func(r *http.Request, req embeddingRequest) {
		if len(req.Input) != 1 || req.Input[0] != "archive message" {
			return
		}
		started, allow, once := firstStarted, allowFirst, &firstOnce
		if req.Model == "second" {
			started, allow, once = secondStarted, allowSecond, &secondOnce
		}
		once.Do(func() { close(started) })
		select {
		case <-allow:
		case <-r.Context().Done():
		case <-ctx.Done():
		}
	})
	var reloaded atomic.Bool
	server := NewServer(ctx, func(OpenOptions) (routing.Config, error) {
		config := settings
		if reloaded.Load() {
			config.Model = "second"
		}
		return routing.Config{History: routing.HistorySettings{Embeddings: config}}, nil
	})
	defer server.Close()
	defer cancel()

	first := dialIndexingPeer(t, ctx, server)
	if _, err := first.ListProject(ctx, cwd, "work"); err != nil {
		t.Fatal(err)
	}
	awaitIndexSignal(t, ctx, firstStarted)
	reloaded.Store(true)
	second := dialIndexingPeer(t, ctx, server)
	if _, err := second.ListProject(ctx, cwd, "work"); err != nil {
		t.Fatal(err)
	}
	close(allowFirst)
	awaitIndexSignal(t, ctx, secondStarted)
	first.disconnect(t, ctx)
	second.disconnect(t, ctx)
	close(allowSecond)
	waitForIndexWorkers(t, ctx, server)

	for _, model := range []string{"fixture", "second"} {
		config := settings
		config.Model = model
		search := sessions.PrepareProjectSearch(ctx, cwd, "work", "semantic query", config)
		if search == nil {
			t.Fatalf("no search for model %q", model)
		}
		hits, coverage, err := search.Matches(ctx)
		if err != nil || coverage.Indexed != 1 || len(hits) != 1 || hits[0].Snippet != "archive message" {
			t.Errorf("model %q after both workers drained: hits %v, coverage %+v, error %v", model, hits, coverage, err)
		}
	}
}

func TestProjectIndexDrainsWorkSavedAfterTheLastScan(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd, gate := t.TempDir(), t.TempDir()
	saveIndexMessage(t, cwd, "work", "initial message")
	saveIndexMessage(t, gate, "gate", "hold the slot")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	initial, held, unblock := make(chan struct{}), make(chan struct{}), make(chan struct{})
	settings := indexingProvider(t, func(r *http.Request, req embeddingRequest) {
		switch req.Input[0] {
		case "initial message":
			close(initial)
		case "hold the slot":
			close(held)
			select {
			case <-unblock:
			case <-r.Context().Done():
			case <-ctx.Done():
			}
		}
	})
	server := NewServer(ctx, func(OpenOptions) (routing.Config, error) {
		return routing.Config{History: routing.HistorySettings{Embeddings: settings}}, nil
	})
	defer server.Close()
	defer cancel()
	picker := dialIndexingPeer(t, ctx, server)
	if _, err := picker.ListProject(ctx, cwd, "work"); err != nil {
		t.Fatal(err)
	}
	awaitIndexSignal(t, ctx, initial)
	blocker := dialIndexingPeer(t, ctx, server)
	if _, err := blocker.ListProject(ctx, gate, "gate"); err != nil {
		t.Fatal(err)
	}
	// The other workspace cannot make a request until the first scan has
	// released the indexing slot.
	awaitIndexSignal(t, ctx, held)
	if err := sessions.SaveTranscript(cwd, "work", []sessions.Message{{Role: "user", Body: "final message"}}, false); err != nil {
		t.Fatal(err)
	}
	picker.disconnect(t, ctx)
	blocker.disconnect(t, ctx)
	close(unblock)
	waitForIndexWorkers(t, ctx, server)
	search := sessions.PrepareProjectSearch(ctx, cwd, "work", "semantic query", settings)
	if search == nil {
		t.Fatal("query embedding unavailable")
	}
	_, coverage, err := search.Matches(ctx)
	if err != nil || coverage.Indexed != 2 || coverage.Window != 2 {
		t.Fatalf("after the final drain: coverage %+v, error %v", coverage, err)
	}
}

func TestProjectWorkerShutdownCancelsProviderAndReleasesSlot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd, next := t.TempDir(), t.TempDir()
	saveIndexMessage(t, cwd, "work", "block until cancellation")
	saveIndexMessage(t, next, "work", "next worker")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	blocked, canceled, indexed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	settings := indexingProvider(t, func(r *http.Request, req embeddingRequest) {
		switch req.Input[0] {
		case "block until cancellation":
			close(blocked)
			select {
			case <-r.Context().Done():
				close(canceled)
			case <-ctx.Done():
			}
		case "next worker":
			close(indexed)
		}
	})
	resolve := func(OpenOptions) (routing.Config, error) {
		return routing.Config{History: routing.HistorySettings{Embeddings: settings}}, nil
	}
	server, replacement := NewServer(ctx, resolve), NewServer(ctx, resolve)
	defer server.Close()
	defer replacement.Close()
	defer cancel()
	picker := dialIndexingPeer(t, ctx, server)
	if _, err := picker.ListProject(ctx, cwd, "work"); err != nil {
		t.Fatal(err)
	}
	awaitIndexSignal(t, ctx, blocked)
	// A provider request must not hold the server lock needed by picker RPCs.
	if _, err := picker.ListProject(ctx, cwd, "work"); err != nil {
		t.Fatal(err)
	}
	replacementPicker := dialIndexingPeer(t, ctx, replacement)
	if _, err := replacementPicker.ListProject(ctx, next, "work"); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		server.Close()
		close(stopped)
	}()
	awaitIndexSignal(t, ctx, canceled)
	awaitIndexSignal(t, ctx, stopped)
	awaitIndexSignal(t, ctx, indexed)
	picker.disconnect(t, ctx)
	replacementPicker.disconnect(t, ctx)
	waitForIndexWorkers(t, ctx, replacement)
}
func TestProjectRegistrationWithLegacyDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan error, 1)
	const legacyHello = `{"version":1,"instance":"old-daemon"}`
	go func() {
		defer b.Close()
		reader := readFrames(b)
		var req request
		if err := reader.read(&req); err != nil {
			done <- err
			return
		}
		if err := writeFrame(b, response{ID: req.ID, Result: json.RawMessage(legacyHello)}, time.Time{}); err != nil {
			done <- err
			return
		}
		if err := reader.read(&req); err != nil {
			done <- err
			return
		}
		if req.Method != "list" || string(req.Params) != "null" {
			done <- fmt.Errorf("legacy daemon received %s with %s", req.Method, req.Params)
			return
		}
		done <- writeFrame(b, response{ID: req.ID, Result: json.RawMessage(`[{"handle":"saved"}]`)}, time.Time{})
	}()
	client, err := Connect(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.SupportsProjectIndex() {
		t.Fatal("legacy hello advertised support for indexing")
	}
	live, err := client.ListProject(ctx, t.TempDir(), "work")
	if err != nil || len(live) != 1 || live[0].Handle != "saved" {
		t.Fatalf("legacy listing returned %v, %v", live, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
