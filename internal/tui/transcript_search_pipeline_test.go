package tui

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"petal/internal/embeddings"
	"petal/internal/livesession"
	"petal/internal/routing"
)

func TestTranscriptSearchCommandPipeline(t *testing.T) {
	t.Run("daemon", func(t *testing.T) { testTranscriptSearchCommandPipeline(t, false) })
	t.Run("legacy", func(t *testing.T) { testTranscriptSearchCommandPipeline(t, true) })
}

func testTranscriptSearchCommandPipeline(t *testing.T, legacyIndex bool) {
	fixture := newPickerSearchFixture(t)
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			http.Error(w, "fixture outage", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/embeddings" || r.Method != http.MethodPost {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		var input struct{ Input []string }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type vector struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		reply := struct {
			Model string   `json:"model"`
			Data  []vector `json:"data"`
		}{Model: "fixture"}
		for i := range input.Input {
			reply.Data = append(reply.Data, vector{Index: i, Embedding: []float32{1}})
		}
		if err := json.NewEncoder(w).Encode(reply); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	p := picker{width: 78, height: 9, legacyIndex: legacyIndex}
	p.center.cwd = fixture.cwd
	p.cfg.History.Embeddings = embeddings.Settings{Provider: "openai-compatible", BaseURL: server.URL, Model: "fixture"}
	p.openTranscriptSearch()
	s := p.transcripts
	s.input.SetValue("fixture marigold")
	send := func(msg tea.Msg) tea.Cmd {
		_, cmd := p.Update(msg)
		return cmd
	}
	deliver := func(cmd tea.Cmd) tea.Cmd {
		t.Helper()
		if cmd == nil {
			t.Fatal("missing pipeline command")
		}
		return send(cmd())
	}
	keyword := deliver(s.request(0))
	semantic := deliver(keyword)
	if len(s.hits) != 1 || s.hits[0].Semantic {
		t.Fatalf("expected one keyword result, got %v", s.hits)
	}
	send(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.reader == nil {
		t.Fatal("could not open the keyword hit")
	}
	backfill := deliver(semantic)
	if !legacyIndex {
		ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
		defer stop()
		cfg := p.cfg
		daemon := livesession.NewServer(ctx, func(livesession.OpenOptions) (routing.Config, error) {
			return cfg, nil
		})
		defer daemon.Close()
		a, b := net.Pipe()
		peerDone := make(chan struct{})
		go func() {
			defer close(peerDone)
			defer b.Close()
			daemon.Handler()(ctx, b)
		}()
		client, err := livesession.Connect(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			client.Close()
			<-peerDone
		}()
		if _, err := client.ListProject(ctx, fixture.cwd, ""); err != nil {
			t.Fatal(err)
		}
	}
	for remaining := 8; backfill != nil; remaining-- {
		if remaining == 0 {
			t.Fatal("semantic backfill did not finish")
		}
		backfill = deliver(backfill)
	}
	if s.reader == nil || s.status != "" {
		t.Fatalf("refresh changed the open reader or left status %q", s.status)
	}
	send(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(s.hits) < 2 || s.hits[0].Semantic || !s.hits[1].Semantic || s.index != 0 {
		t.Fatalf("expected keyword selection and semantic results, got index %d and hits %v", s.index, s.hits)
	}
	send(tea.KeyPressMsg{Code: tea.KeyEscape})

	unavailable.Store(true)
	p.openTranscriptSearch()
	s = p.transcripts
	s.input.SetValue("fixture marigold")
	semantic = deliver(deliver(s.request(0)))
	if next := deliver(semantic); next != nil || len(s.hits) != 1 || s.hits[0].Semantic || s.status != "" {
		t.Fatalf("provider outage lost the keyword result or kept searching: hits=%v status=%q", s.hits, s.status)
	}
	send(tea.KeyPressMsg{Code: tea.KeyEscape})
}

func TestTranscriptSearchCancelsEmbedding(t *testing.T) {
	t.Run("superseded", func(t *testing.T) { testTranscriptSearchCancelsEmbedding(t, false) })
	t.Run("quit", func(t *testing.T) { testTranscriptSearchCancelsEmbedding(t, true) })
}

func testTranscriptSearchCancelsEmbedding(t *testing.T, quit bool) {
	fixture := newPickerSearchFixture(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(server.Close)
	p := picker{}
	p.cfg.History.Embeddings = embeddings.Settings{Provider: "openai-compatible", BaseURL: server.URL, Model: "fixture"}
	p.center.cwd = fixture.cwd
	p.openTranscriptSearch()
	s := p.transcripts
	s.input.SetValue("fixture marigold")

	_, keyword := p.Update(s.request(0)())
	if keyword == nil {
		t.Fatal("missing keyword command")
	}
	_, embedding := p.Update(keyword())
	if embedding == nil {
		t.Fatal("missing embedding command")
	}
	t.Cleanup(s.cancel)
	completed := make(chan tea.Msg, 1)
	go func() { completed <- embedding() }()

	timeout, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	select {
	case <-started:
	case <-timeout.Done():
		t.Fatal("embedding command never reached server")
	}
	if quit {
		if _, cmd := p.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
			t.Fatal("missing quit command")
		}
	} else {
		s.input.SetValue("different query")
		if s.request(0) == nil {
			t.Fatal("missing replacement query")
		}
	}
	select {
	case <-canceled:
	case <-timeout.Done():
		t.Fatal("embedding request continued after leaving the query")
	}

	select {
	case old := <-completed:
		if !quit {
			status := s.status
			if _, cmd := p.Update(old); cmd != nil || len(s.hits) != 0 || s.status != status {
				t.Fatal("superseded completion changed replacement query")
			}
		}
	case <-timeout.Done():
		t.Fatal("canceled embedding command did not return")
	}
}
