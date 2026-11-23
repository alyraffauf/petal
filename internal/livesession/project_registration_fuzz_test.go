//go:build linux || darwin

package livesession

import (
	"context"
	"maps"
	"math/rand/v2"
	"net"
	"path/filepath"
	"testing"
	"time"

	"petal/internal/embeddings"
	"petal/internal/routing"
)

var projectRegistrationTraces = [][]byte{
	{0, 1, 4, 2, 10, 6, 3},
	{0x00, 0x10, 0x20, 0x10, 0x03, 0x21, 0x14, 0xd0, 0x23},
}

func checkProjectRegistrations(t *testing.T, trace []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fixture := t.TempDir()
	settings, err := (embeddings.Settings{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(ctx, func(opts OpenOptions) (routing.Config, error) {
		var cfg routing.Config
		cfg.History.Embeddings = settings
		if opts.Account == "off" {
			cfg.History.Embeddings = embeddings.Settings{Provider: "off"}
		}
		return cfg, nil
	})
	defer server.Close()
	type frontend struct {
		client *Client
		done   <-chan struct{}
	}
	var clients [3]frontend
	var scopes [3]projectIndexKey
	defer func() {
		for _, client := range clients {
			if client.client != nil {
				client.client.Close()
				<-client.done
			}
		}
	}()
	for step, op := range trace[:min(len(trace), 64)] {
		slot := int((op>>4)&3) % len(clients)
		front := &clients[slot]
		if front.client == nil {
			a, b := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer b.Close()
				server.Handler()(ctx, b)
			}()
			client, err := Connect(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			*front = frontend{client, done}
		}
		switch op & 3 {
		case 0:
			projects := []string{"one", "two", "three", "one"}
			accounts := []string{"", "work", "personal", "off"}
			cwd := filepath.Join(fixture, projects[(op>>2)&3])
			account := accounts[op>>6]
			if _, err := front.client.ListProject(ctx, cwd, account); err != nil {
				t.Fatal(err)
			}
			scopes[slot] = projectIndexKey{}
			if account != "off" {
				scopes[slot] = projectIndexKey{cwd, account, settings}
			}
		case 1:
			if _, err := front.client.List(ctx); err != nil {
				t.Fatal(err)
			}
		case 2:
			if _, err := front.client.ListProject(ctx, "relative", "work"); err == nil {
				t.Fatal("invalid scope accepted")
			}
		case 3:
			front.client.Close()
			<-front.done
			*front = frontend{}
			scopes[slot] = projectIndexKey{}
		}

		want := make(map[projectIndexKey]int)
		for _, scope := range scopes {
			if scope.cwd != "" {
				want[scope]++
			}
		}
		got := make(map[projectIndexKey]int)
		server.mu.Lock()
		for key, worker := range server.indexes {
			if worker.users != 0 {
				got[key] = worker.users
			}
		}
		server.mu.Unlock()
		if !maps.Equal(got, want) {
			t.Fatalf("step %d trace %x: got %v, want %v", step, trace, got, want)
		}
	}
}

func TestProjectRegistrationModel(t *testing.T) {
	for _, trace := range projectRegistrationTraces {
		checkProjectRegistrations(t, trace)
	}
	for seed := uint64(0); seed < 40; seed++ {
		random := rand.New(rand.NewPCG(seed, 0))
		trace := make([]byte, 32)
		for i := range trace {
			trace[i] = byte(random.Uint32())
		}
		checkProjectRegistrations(t, trace)
	}
}

func FuzzProjectRegistrationModel(f *testing.F) {
	for _, trace := range projectRegistrationTraces {
		f.Add(trace)
	}
	f.Fuzz(checkProjectRegistrations)
}
