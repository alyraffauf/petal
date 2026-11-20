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
			continue
		case "_session/steering":
			if activePrompt == 0 || request.Params.Prompt[0].Text != "revised" {
				t.Fatal("expected steering during active turn")
			}
			send(map[string]any{"id": request.ID, "result": map[string]string{"outcome": "injected"}})
			send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "steering-confirmed"}}}})
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
	config := fmt.Sprintf("[accounts.local.env]\nPETAL_PTY_ROLE='adapter'\n[accounts.local.models]\nfixture=[%q, '-test.run=^TestPTYChild$']\n", exe)
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
	await("Control center")
	write("\r")
	await("Choose a model")
	write("\r")
	await("Ready")
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
	await("Alternate")
	write("\r")
	await("Ask alternate")
	write("/config\r")
	await("Model · Alternate")
	write("\x1b")
	await("Ask alternate")
	write("\x1b[200~first line\nsecond line\x1b[201~")
	await("second line")
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 14, Cols: 54}); err != nil {
		t.Fatal(err)
	}
	write("\r")
	await("holding-turn")
	write("revised\r")
	await("steering-confirmed")
	write("\x1bOR") // xterm F3
	await("Conversation")
	write("\x1bOR")
	write("/sessions\r")
	await("Conversation turns")
	write("\x1b")
	await("Ask alternate")
	write("/new\r")
	await("Choose a model")
	write("\x1bOQ") // F2 opens the control center from the model picker.
	await("New session")
	write("/status-report")
	await("status-report")
	write("\r")
	await("Restore alternate")
	write("1\r")
	await("Ready")
	write("\x1b[5~") // Page up past the settings confirmation in the short viewport.
	await("restored transcript")
}
