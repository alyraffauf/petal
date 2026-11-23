//go:build linux || darwin

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"petal/internal/livesession"
	"petal/internal/routing"
)

func TestTerminalRegistersCurrentProjectAndAccount(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(workspace, "config.toml")
	source := fmt.Sprintf(`[history.embeddings]
provider="off"
[[accounts]]
name="work"
[[accounts.env]]
name="PETAL_PTY_ROLE"
value="adapter"
[[accounts.models]]
id="fixture"
command=[%q, "-test.run=^TestPTYChild$"]
`, exe)
	if err := os.WriteFile(config, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(workspace, "other")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	env := ptyEnvironment(t, workspace, config)
	t.Setenv("XDG_STATE_HOME", workspace)
	endpoint, err := fixtureEndpoint(env, config, workspace)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", endpoint.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(endpoint.SocketPath(), 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	registered := make(chan livesession.OpenOptions, 8)
	server := livesession.NewServer(ctx, func(opts livesession.OpenOptions) (routing.Config, error) {
		if opts.Handle == "" {
			select {
			case registered <- opts:
			case <-ctx.Done():
			}
		}
		return routing.Load(config, opts.Model, opts.Account)
	})
	done := make(chan struct{})
	disconnected := make(chan int, 8)
	go func() {
		defer close(done)
		var peers sync.WaitGroup
		defer peers.Wait()
		for id := 0; ; id++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			peers.Go(func() {
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				server.Handler()(ctx, conn)
				disconnected <- id
			})
		}
	}()
	defer func() {
		cancel()
		listener.Close()
		<-done
		server.Close()
	}()
	expectScope := func(cwd, account string) {
		t.Helper()
		select {
		case opts := <-registered:
			if opts.Cwd != cwd || opts.Account != account {
				t.Fatalf("registered %q / %q, want %q / %q", opts.Cwd, opts.Account, cwd, account)
			}
		case <-ctx.Done():
			t.Fatalf("missing registration for %q / %q", cwd, account)
		}
	}
	terminal := startTerminal(t, ctx, exe, workspace, env)
	terminal.await(t, ctx, "control center")
	expectScope(workspace, "")
	terminal.write(t, "\r")
	terminal.await(t, ctx, "choose a model")
	terminal.write(t, "\r")
	expectScope(workspace, "")
	terminal.await(t, ctx, "ready")
	select {
	case id := <-disconnected:
		if id != 0 {
			t.Fatalf("first disconnected peer was %d, want startup picker", id)
		}
	case <-ctx.Done():
		t.Fatal("startup picker kept its registration after entering chat")
	}
	terminal.write(t, "/new other\r")
	expectScope(project, "")
	terminal.await(t, ctx, "ready")
	terminal.write(t, "/new\r")
	terminal.await(t, ctx, "choose a model")
	terminal.write(t, "\t")
	terminal.await(t, ctx, "unmetered")
	terminal.write(t, "\x1b[B\r")
	expectScope(project, "work")
	terminal.await(t, ctx, "ready")
	terminal.detach(t)
}
