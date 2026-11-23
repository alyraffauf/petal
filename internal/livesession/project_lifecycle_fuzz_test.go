//go:build linux || darwin

package livesession

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"petal/internal/routing"
)

func FuzzProjectWorkerLifetimes(f *testing.F) {
	// Register, repeat, switch scopes, disconnect, and retain during a drain.
	f.Add([]byte{0, 0, 8, 16, 24, 12, 4, 0})
	f.Add([]byte{0, 1, 8, 9, 16, 24})
	f.Add([]byte{16, 24, 17, 25, 21, 29})
	f.Fuzz(func(t *testing.T, trace []byte) {
		if len(trace) > 32 {
			return
		}
		checkProjectWorkerLifetimes(t, trace, false)
	})
}

func TestProjectWorkerLifetimeProperties(t *testing.T) {
	random := rand.New(rand.NewPCG(5282, 36248))
	for trial, trace := range [][]byte{
		{0, 32, 36, 16, 40, 44, 0},
		{32, 36, 0, 4, 8, 40, 12, 44},
	} {
		t.Run(fmt.Sprintf("detached-%d", trial), func(t *testing.T) {
			checkProjectWorkerLifetimes(t, trace, true)
		})
	}
	for trial := range 24 {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			trace := make([]byte, 16)
			for i := range trace {
				trace[i] = uint8(random.IntN(48))
			}
			checkProjectWorkerLifetimes(t, trace, true)
		})
	}
}

func checkProjectWorkerLifetimes(t *testing.T, trace []byte, allowSession bool) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	type scope struct{ cwd, account string }
	first, second, gate := t.TempDir(), t.TempDir(), t.TempDir()
	scopes := []scope{{first, "fake"}, {first, "other"}, {second, "fake"}, {second, "other"}}
	for i, scope := range scopes {
		saveIndexMessage(t, scope.cwd, scope.account, fmt.Sprintf("archive %d", i))
	}
	saveIndexMessage(t, gate, "gate", "gate")
	started, unblock := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	var mu sync.Mutex
	var embedded []string
	settings := indexingProvider(t, func(r *http.Request, req embeddingRequest) {
		if slices.Contains(req.Input, "gate") {
			gateOnce.Do(func() { close(started) })
			select {
			case <-unblock:
			case <-r.Context().Done():
			case <-ctx.Done():
			}
			return
		}
		mu.Lock()
		embedded = append(embedded, req.Input...)
		mu.Unlock()
	})
	server := NewServer(ctx, func(opts OpenOptions) (routing.Config, error) {
		cfg := routing.Config{Model: opts.Model, PinnedAccount: opts.Account, Accounts: map[string]routing.Account{
			"fake": {
				Models: map[string][]string{"fixture": {executable, "-test.run=^TestLiveAdapterFixture$"}},
				Env:    map[string]string{"PETAL_LIVE_FIXTURE": "1", "PETAL_LIVE_MODEL": "fixture"},
			},
		}}
		cfg.History.Embeddings = settings
		return cfg, nil
	})
	defer server.Close()
	defer cancel()
	gatePeer := dialIndexingPeer(t, ctx, server)
	if _, err := gatePeer.ListProject(ctx, gate, "gate"); err != nil {
		t.Fatal(err)
	}
	awaitIndexSignal(t, ctx, started)

	// The model records logical owners and all scopes that owe a final pass.
	// Hold the provider slot so each registered scope has pending work.
	var peers [4]indexingPeer
	var pickerScopes [4]scope
	var owed [4]bool
	var controller indexingPeer
	var handle string
	checkOwners := func(step int) {
		t.Helper()
		want := map[scope]int{}
		for _, registered := range pickerScopes {
			if registered.cwd != "" {
				want[registered]++
			}
		}
		if handle != "" {
			want[scopes[0]]++
		}
		server.mu.Lock()
		defer server.mu.Unlock()
		for _, target := range scopes {
			worker := server.indexes[projectIndexKey{target.cwd, target.account, settings}]
			if worker == nil && want[target] == 0 {
				continue
			}
			if worker == nil || worker.users != want[target] {
				t.Fatalf("trace %v step %d scope %v: worker %+v, want %d owners", trace, step, target, worker, want[target])
			}
		}
	}
	for step, event := range trace {
		slot := int(event % 4)
		op := event / 4 % 12
		if !allowSession {
			op %= 8
		}
		switch {
		case op < 4:
			if peers[slot].Client == nil {
				peers[slot] = dialIndexingPeer(t, ctx, server)
			}
			target := scopes[op]
			if _, err := peers[slot].ListProject(ctx, target.cwd+"/.", target.account); err != nil {
				t.Fatal(err)
			}
			pickerScopes[slot], owed[op] = target, true
		case op < 8:
			if peers[slot].Client != nil {
				peers[slot].disconnect(t, ctx)
				peers[slot], pickerScopes[slot] = indexingPeer{}, scope{}
			}
		case op == 8 && allowSession && handle == "":
			controller = dialIndexingPeer(t, ctx, server)
			handle = NewHandle()
			openSession(t, ctx, controller.Client, OpenOptions{Handle: handle, Cwd: first, Account: "fake", Model: "fixture"}, 1)
			owed[0] = true
		case op == 9 && controller.Client != nil:
			controller.disconnect(t, ctx)
			controller = indexingPeer{}
		case op == 10 && handle != "" && controller.Client == nil:
			controller = dialIndexingPeer(t, ctx, server)
			takeControl(t, ctx, controller.Client, AttachOptions{Handle: handle, Control: true})
		case op == 11 && handle != "":
			if controller.Client == nil {
				controller = dialIndexingPeer(t, ctx, server)
				takeControl(t, ctx, controller.Client, AttachOptions{Handle: handle, Control: true})
			}
			if err := controller.CloseSession(ctx); err != nil {
				t.Fatal(err)
			}
			// CloseSession acknowledges before retirement releases the index.
			for {
				_, err := controller.Open(ctx, OpenOptions{Handle: handle, Cwd: first, Account: "fake", Model: "fixture"})
				if !hasCode(err, Closed) {
					t.Fatalf("closed handle returned %v", err)
				}
				server.mu.Lock()
				_, retiring := server.live[handle]
				server.mu.Unlock()
				if !retiring {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
			}
			controller.disconnect(t, ctx)
			controller, handle = indexingPeer{}, ""
		}
		checkOwners(step)
	}
	if handle != "" {
		if controller.Client == nil {
			controller = dialIndexingPeer(t, ctx, server)
			takeControl(t, ctx, controller.Client, AttachOptions{Handle: handle, Control: true})
		}
		if err := controller.CloseSession(ctx); err != nil {
			t.Fatal(err)
		}
		controller.disconnect(t, ctx)
	}
	for _, peer := range peers {
		if peer.Client != nil {
			peer.disconnect(t, ctx)
		}
	}
	gatePeer.disconnect(t, ctx)
	close(unblock)
	waitForIndexWorkers(t, ctx, server)
	var want []string
	for i, needed := range owed {
		if needed {
			want = append(want, fmt.Sprintf("archive %d", i))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(embedded)
	if !slices.Equal(embedded, want) {
		t.Fatalf("trace %v embedded %v, want %v", trace, embedded, want)
	}
}
