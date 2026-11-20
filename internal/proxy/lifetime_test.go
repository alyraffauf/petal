package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestExitedAdapterWithInheritedOutput(t *testing.T) {
	// The adapter's helper keeps stdout open after the final initialize reply.
	lifetimeDir := t.TempDir()
	script := `read -r request
(while [ -d "$1" ]; do sleep 0.1; done) &
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'`
	p := proxy{embedded: true, lifetime: t.Context(), Config: routing.Config{
		Model: "test", Accounts: map[string]routing.Account{"local": {Models: map[string][]string{"test": {"sh", "-c", script, "adapter", lifetimeDir}}}},
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b, err := p.startBackend(ctx, "local", json.RawMessage(`{"protocolVersion":1}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	select {
	case <-b.conn.Done():
	case <-ctx.Done():
		t.Fatal("exited adapter left the connection waiting for its helper")
	}
}

func TestDelegatedCancellationStopsOnlySelectedAdapterGroup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	deadline, _ := ctx.Deadline()
	if err := listener.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	account := delegationRoutingAccount(exe, "echo 100", "")
	account.Models["sibling"] = account.Models["m"]
	account.Env["PETAL_DELEGATED_HELPER_ADDR"] = listener.Addr().String()
	p := &proxy{embedded: true, lifetime: ctx, Config: routing.Config{
		Delegation: true, Accounts: map[string]routing.Account{"local": account},
	}}
	p.configureDelegation()
	cwd := t.TempDir()
	if _, err := sessions.Create(cwd, sessions.Record{ID: "parent", Model: "m"}); err != nil {
		t.Fatal(err)
	}

	start := func(model string) (sessions.ChildBinding, *net.TCPConn) {
		t.Helper()
		child, err := p.delegationRuntime.Delegate(ctx, "parent", cwd, model, "wait")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := listener.AcceptTCP()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetReadDeadline(deadline)
		var ready [1]byte
		if _, err := io.ReadFull(conn, ready[:]); err != nil {
			t.Fatalf("%s helper failed to start: %v", model, err)
		}
		return sessions.ChildBinding{ParentID: "parent", ChildID: child.ID}, conn
	}
	cancelChild := func(binding sessions.ChildBinding, conn *net.TCPConn) {
		t.Helper()
		result, err := sessions.CancelChild(ctx, cwd, binding)
		if err != nil || result == nil || result.Status != "cancelled" {
			t.Fatalf("cancel %s returned %+v, %v", binding.ChildID, result, err)
		}
		var data [1]byte
		if _, err := conn.Read(data[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("confirmed cancellation left %s's helper alive: %v", binding.ChildID, err)
		}
	}
	selected, selectedConn := start("m")
	sibling, siblingConn := start("sibling")
	cancelChild(selected, selectedConn)

	siblingConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	var data [1]byte
	var timeout net.Error
	if _, err := siblingConn.Read(data[:]); !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("cancelling %s closed its sibling's helper: %v", selected.ChildID, err)
	}
	siblingConn.SetReadDeadline(deadline)
	cancelChild(sibling, siblingConn)
}

func startDelegatedHelper(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	file, err := conn.(*net.TCPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.Command("/bin/sh", "-c", "printf r >&3; exec sleep 15")
	cmd.ExtraFiles = []*os.File{file}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
}
