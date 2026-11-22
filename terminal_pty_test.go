//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

// Start the CLI entry point and its ACP subprocess from the test executable.
// This exercises terminal negotiation and the real adapter transport offline.
func TestPTYChild(t *testing.T) {
	switch os.Getenv("PETAL_PTY_ROLE") {
	case "client":
		os.Args = []string{"petal", "--config", os.Getenv("PETAL_PTY_CONFIG")}
		main()
	case "server":
		os.Args = []string{"petal", "acp", "--config", os.Getenv("PETAL_PTY_CONFIG"), "--account", "missing"}
		main()
	case "adapter":
		runPTYAdapter(t)
	}
}

func runPTYAdapter(t *testing.T) {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	send := func(fields map[string]any) {
		fields["jsonrpc"] = "2.0"
		if err := encoder.Encode(fields); err != nil {
			t.Fatal(err)
		}
	}
	var activePrompt int
	model := "baseline"
	modelSettings := func() []any {
		return []any{map[string]any{
			"id": "model", "name": "Model", "type": "select", "category": "model", "currentValue": model,
			"options": []any{
				map[string]string{"value": "baseline", "name": "Baseline"},
				map[string]string{"value": "alternate", "name": "Alternate", "description": "Second model"},
			},
		}}
	}
	for scanner.Scan() {
		var request struct {
			ID     int
			Method string
			Params struct {
				Prompt    []struct{ Text string }
				SessionID string
				ConfigID  string
				Value     string
			}
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		result := map[string]any{}
		switch request.Method {
		case "initialize":
			result["protocolVersion"] = 1
			result["agentCapabilities"] = map[string]bool{"loadSession": true}
			result["_meta"] = map[string]any{"steering": map[string]bool{"supported": true}}
		case "session/new":
			send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{
				"sessionUpdate": "available_commands_update", "availableCommands": []any{
					map[string]any{"name": "status-report", "description": "Inspect status", "input": map[string]string{"hint": "subject"}},
				},
			}}})
			result["sessionId"] = "pty-thread"
			result["configOptions"] = modelSettings()
		case "session/set_config_option":
			if request.Params.SessionID != "pty-thread" || request.Params.ConfigID != "model" || request.Params.Value != "alternate" {
				t.Fatalf("expected an in-session model change, got %+v", request.Params)
			}
			model = request.Params.Value
			result["configOptions"] = modelSettings()
		case "session/load":
			result["configOptions"] = modelSettings()
			if request.Params.SessionID != "pty-thread" {
				t.Fatalf("wrong resume ID %q", request.Params.SessionID)
			}
			// Real adapters replay saved turns in bursts larger than the SDK's
			// notification queue. The final message must still be reachable.
			for range 2048 {
				send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": "earlier", "content": map[string]string{"type": "text", "text": "."}}}})
			}
			send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "restored transcript"}}}})
		case "session/prompt":
			if request.Params.Prompt[0].Text == "/status-report build" {
				send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "status-command-confirmed"}}}})
				send(map[string]any{"id": request.ID, "result": map[string]string{"stopReason": "end_turn"}})
				continue
			}
			activePrompt = request.ID
			if request.Params.Prompt[0].Text != "first line\nsecond line" {
				t.Fatal("multiline paste changed")
			}
			send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool1", "title": "holding-turn", "status": "completed"}}})
			send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{
				"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "partial-reply-visible"},
			}}})
			continue
		case "_session/steering":
			if activePrompt == 0 || request.Params.Prompt[0].Text != "revised" {
				t.Fatal("expected steering during active turn")
			}
			send(map[string]any{"id": request.ID, "result": map[string]string{"outcome": "injected"}})
			send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "steered"}}}})
			send(map[string]any{"id": activePrompt, "result": map[string]string{"stopReason": "end_turn"}})
			continue
		default:
			t.Fatalf("unexpected request: %s", request.Method)
		}
		send(map[string]any{"id": request.ID, "result": result})
	}
}

func TestTerminalPasteResizeAndSteer(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	config := fmt.Sprintf("stream_responses=true\n[[accounts]]\nname='local'\n[[accounts.env]]\nname='PETAL_PTY_ROLE'\nvalue='adapter'\n[[accounts.models]]\nid='fixture'\ncommand=[%q, '-test.run=^TestPTYChild$']\n", exe)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPTYChild$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "HOME="+dir, "XDG_STATE_HOME="+dir, "PATH="+dir, "PETAL_PTY_ROLE=client", "PETAL_PTY_CONFIG="+configPath)
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 90})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); tty.Close(); cmd.Wait() }()
	output := make(chan string)
	go func() {
		buf := make([]byte, 8192)
		defer close(output)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				select {
				case output <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var pending string
	await := func(marker string) {
		t.Helper()
		for {
			plain := ansi.Strip(pending)
			if strings.Contains(plain, marker) {
				pending = ""
				return
			}
			select {
			case chunk, ok := <-output:
				if !ok {
					t.Fatalf("terminal closed waiting for %q: %q", marker, plain)
				}
				pending += chunk
			case <-ctx.Done():
				t.Fatalf("waiting for %q: %q", marker, plain)
			}
		}
	}
	write := func(input string) {
		t.Helper()
		if _, err := tty.WriteString(input); err != nil {
			t.Fatal(err)
		}
	}
	await("control center")
	write("\r")
	await("choose a model")
	write("\r")
	await("ready")
	write("/stat")
	await("Inspect status")
	write("\r")
	await("↵ Send")
	write("build\r")
	await("status-command-confirmed")
	write("/model\r")
	await("Baseline (current)")
	write("\x1b")
	await("Ask baseline")
	write("/model\r")
	await("Second model")
	write("Alternate")
	write("\r")
	await("alternate…")
	write("/config\r")
	await("Model · Alternate")
	write("\x1b")
	await("alternate…")
	write("\x1b[200~first line\nsecond line\x1b[201~")
	await("second line")
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 14, Cols: 54}); err != nil {
		t.Fatal(err)
	}
	write("\r")
	await("partial-reply-visible")
	write("revised\r")
	await("steered")
	write("\x1bOR") // xterm F3
	await("conversation")
	write("\x1bOR")
	write("/sessions\r")
	await("conversation turns")
	write("\x1b")
	await("alternate…")
	write("/new\r")
	await("choose a model")
	write("\x1bOQ") // F2 opens the control center from the model picker.
	await("new session")
	write("/status-report")
	await("status-report")
	write("\r")
	await("Restore alternate")
	write("1\r")
	await("ready")
	write("\x1b[5~") // Page up past the settings confirmation in the short viewport.
	await("restored transcript")
	if err := os.Mkdir(filepath.Join(dir, "second"), 0700); err != nil {
		t.Fatal(err)
	}
	write("/new second\r")
	await("ready")
	write("/config\r")
	await("Model · Alternate")
}

func TestACPCLIRejectsUnrunnableDefaultAccount(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[[accounts]]\nname='local'\n[[accounts.models]]\nid='fixture'\ncommand=['unused']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPTYChild$")
	cmd.Env = append(os.Environ(), "PATH="+dir, "PETAL_PTY_ROLE=server", "PETAL_PTY_CONFIG="+configPath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 1 || len(output) != 0 || !strings.Contains(stderr.String(), "missing") {
		t.Fatalf("expected a startup error identifying the pin on stderr, got %v, stdout %q, stderr %q", err, output, stderr.String())
	}
}
