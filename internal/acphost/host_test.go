package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestWorkspaceAndTerminalLifecycle(t *testing.T) {
	cwd := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	approvals := 0
	h, err := Open(ctx, cwd, func(context.Context, string) error { approvals++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	call := func(method string, p any) any {
		t.Helper()
		raw, _ := json.Marshal(p)
		r, err := h.Call(ctx, method, raw)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	path := filepath.Join(cwd, "test", "notes")
	call("fs/write_text_file", map[string]any{"path": path, "content": "first\nsecond\nthird"})
	r := call("fs/read_text_file", map[string]any{"path": path, "line": 2, "limit": 1}).(acp.ReadTextFileResponse)
	if r.Content != "second\n" {
		t.Fatalf("read range: %q", r.Content)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(cwd, "outside")); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"path": filepath.Join(cwd, "outside", "x"), "content": "x"})
	if _, err = h.Call(ctx, "fs/write_text_file", raw); err == nil {
		t.Fatal("followed escaping symlink")
	}
	id := call("terminal/create", map[string]any{"sessionId": "original", "command": "sh", "args": []string{"-c", "printf abcdef; exit 7"}, "outputByteLimit": 3}).(acp.CreateTerminalResponse).TerminalId
	raw, _ = json.Marshal(map[string]any{"sessionId": "other", "terminalId": id})
	var requestError *acp.RequestError
	if _, err = h.Call(ctx, "terminal/release", raw); !errors.As(err, &requestError) || requestError.Code != -32002 {
		t.Fatalf("cross-session release: %v", err)
	}
	params := map[string]any{"sessionId": "original", "terminalId": id}
	call("terminal/wait_for_exit", params)
	out := call("terminal/output", params).(acp.TerminalOutputResponse)
	if !out.Truncated || out.Output != "def" || out.ExitStatus == nil || *out.ExitStatus.ExitCode != 7 {
		t.Fatalf("terminal output: %+v", out)
	}
	view := h.OutputView("original", string(id))
	if view == nil || h.OutputView("other", string(id)) != nil {
		t.Fatal("terminal attachment did not respect session ownership")
	}
	call("terminal/release", params)
	if view().Output != "def" || h.OutputView("original", string(id)) != nil {
		t.Fatal("released terminal should remain visible only through the existing attachment")
	}
	raw, _ = json.Marshal(params)
	if _, err = h.Call(ctx, "terminal/output", raw); err == nil {
		t.Fatal("released terminal still accessible")
	}
	if approvals != 3 {
		t.Fatalf("expected approvals for writes and launch, got %d", approvals)
	}
}

func TestKillRetainsTerminalUntilRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := Open(ctx, t.TempDir(), func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	created, err := h.Call(ctx, "terminal/create", json.RawMessage(`{"sessionId":"s","command":"sleep","args":["30"]}`))
	if err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{"sessionId": "s", "terminalId": created.(acp.CreateTerminalResponse).TerminalId})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(ctx, "terminal/kill", params); err != nil {
		t.Fatal(err)
	}
	exited, err := h.Call(ctx, "terminal/wait_for_exit", params)
	if err != nil {
		t.Fatal(err)
	}
	if status := exited.(*acp.TerminalExitStatus); status.Signal == nil || status.ExitCode != nil {
		t.Fatalf("expected signal exit, got %+v", status)
	}
	if _, err := h.Call(ctx, "terminal/release", params); err != nil {
		t.Fatal(err)
	}
}
