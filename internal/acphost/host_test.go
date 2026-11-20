package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func TestWorkspaceAndTerminalLifecycle(t *testing.T) {
	cwd := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := Open(ctx, cwd, func(context.Context, string) error { return nil })
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

func TestCanceledApprovalPreventsWorkspaceMutation(t *testing.T) {
	for _, method := range []string{"fs/write_text_file", "terminal/create"} {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dir := t.TempDir()
			approved := false
			h, err := Open(t.Context(), dir, func(context.Context, string) error {
				approved = true
				cancel()
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()

			params := map[string]any{"path": filepath.Join(dir, "created", "file"), "content": "content", "command": "mkdir", "args": []string{"created"}}
			raw, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.Call(ctx, method, raw); !errors.Is(err, context.Canceled) || !approved {
				t.Fatalf("expected cancellation after approval, got %v (approved %v)", err, approved)
			}
			if _, err := os.Stat(filepath.Join(dir, "created")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled request touched workspace: %v", err)
			}
		})
	}
}

func TestTerminalOutputFitsACPFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, err := Open(ctx, t.TempDir(), func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	created, err := h.Call(ctx, "terminal/create", json.RawMessage(`{"sessionId":"s","command":"head","args":["-c","2097152","/dev/zero"],"outputByteLimit":9223372036854775807}`))
	if err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{"sessionId": "s", "terminalId": created.(acp.CreateTerminalResponse).TerminalId})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(ctx, "terminal/wait_for_exit", params); err != nil {
		t.Fatal(err)
	}
	result, err := h.Call(ctx, "terminal/output", params)
	if err != nil {
		t.Fatal(err)
	}
	out := result.(acp.TerminalOutputResponse)
	if !out.Truncated || len(out.Output) < 1<<20 || strings.Trim(out.Output, "\x00") != "" ||
		out.ExitStatus == nil || out.ExitStatus.ExitCode == nil || *out.ExitStatus.ExitCode != 0 {
		t.Fatalf("unexpected output: %d bytes, truncated %v, exit %+v", len(out.Output), out.Truncated, out.ExitStatus)
	}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": out})
	if err != nil {
		t.Fatal(err)
	}
	if len(frame)+1 >= 10<<20 {
		t.Fatalf("encoded terminal response exceeds the SDK frame limit: %d bytes", len(frame))
	}
}

func TestTerminalExitWithInheritedOutput(t *testing.T) {
	for _, mode := range []string{"exit", "release", "close", "drain"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// The helper inherits the terminal's pipes and keeps this extra
			// connection open so the test can observe its death without PID polling.
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			deadline, _ := ctx.Deadline()
			if err := listener.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			h, err := Open(ctx, t.TempDir(), func(context.Context, string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{
				"sessionId": "s", "command": "sh",
				"args": []string{"-c", `ACPHOST_TEST_HELPER_MODE="$2" "$1" -test.run='^TestTerminalOutputHelper$' &
while [ ! -e helper-ready ]; do sleep 0.01; done
if [ "$2" = release ] || [ "$2" = close ]; then wait; fi
printf out; printf err >&2; exit 7`, "sh", exe, mode},
				"env": []acp.EnvVariable{{Name: "ACPHOST_TEST_HELPER_ADDR", Value: listener.Addr().String()}},
			})
			if err != nil {
				t.Fatal(err)
			}
			created, err := h.Call(ctx, "terminal/create", raw)
			if err != nil {
				t.Fatal(err)
			}
			id := created.(acp.CreateTerminalResponse).TerminalId
			conn, err := listener.AcceptTCP()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			params, err := json.Marshal(map[string]any{"sessionId": "s", "terminalId": id})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "exit", "drain":
				if _, err := h.Call(ctx, "terminal/wait_for_exit", params); err != nil {
					t.Fatal(err)
				}
				view := h.OutputView("s", string(id))
				if out := view(); out.Output != "outerr" || out.ExitStatus == nil ||
					out.ExitStatus.ExitCode == nil || *out.ExitStatus.ExitCode != 7 || out.ExitStatus.Signal != nil {
					t.Fatalf("unexpected terminal output: %+v", out)
				}
			case "release":
				if _, err := h.Call(ctx, "terminal/release", params); err != nil {
					t.Fatal(err)
				}
			case "close":
				h.Close()
			}
			if mode == "drain" {
				// The helper's live connection kept the inherited pipe open
				// through wait_for_exit. Closing the fixture socket ends it.
				return
			}
			if n, err := conn.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("%s did not stop the helper: read %d bytes, %v", mode, n, err)
			}
		})
	}
}

func TestTerminalOutputHelper(t *testing.T) {
	addr := os.Getenv("ACPHOST_TEST_HELPER_ADDR")
	if addr == "" {
		return
	}
	if os.Getenv("ACPHOST_TEST_HELPER_MODE") == "drain" {
		if _, err := syscall.Setsid(); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := os.WriteFile("helper-ready", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatal(err)
	}
}
