//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"petal/internal/localdaemon"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

func TestMain(m *testing.M) {
	if os.Getenv("PETAL_PTY_ROLE") != "" && len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			if err := os.WriteFile(os.Getenv("PETAL_PTY_PID"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
				panic(err)
			}
			main()
			return
		case "delegation-mcp":
			main()
			return
		}
	}
	os.Exit(m.Run())
}

func ptyEnvironment(t *testing.T, workspace, configPath string) []string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "petal-pty-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	endpoint, err := localdaemon.DiscoverProfile(root, configPath)
	if err != nil {
		var stat unix.Stat_t
		if runtime.GOOS == "linux" && os.Getenv("NIX_BUILD_TOP") != "" &&
			unix.Stat("/", &stat) == nil && stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			t.Skip("terminal daemon requires a trusted runtime path outside the Nix sandbox")
		}
		t.Fatal(err)
	}
	pidPath := filepath.Join(root, "daemon.pid")
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidPath)
		if os.IsNotExist(err) {
			return
		}
		pid, decodeErr := strconv.Atoi(string(raw))
		if err != nil || decodeErr != nil || pid <= 1 {
			t.Errorf("invalid daemon pid %q: %v", raw, err)
			return
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			conn, err := endpoint.Dial(ctx)
			if err != nil {
				break
			}
			conn.Close()
			select {
			case <-ctx.Done():
				t.Errorf("daemon %d did not close its socket", pid)
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	return append(os.Environ(), "TERM=xterm-256color", "HOME="+workspace, "XDG_STATE_HOME="+workspace,
		"XDG_CONFIG_HOME="+workspace, "XDG_CACHE_HOME="+workspace, "XDG_RUNTIME_DIR="+root, "TMPDIR="+root,
		"PATH="+workspace, "PETAL_PTY_ROLE=client", "PETAL_PTY_CONFIG="+configPath, "PETAL_PTY_PID="+pidPath)
}

// Start the CLI entry point and its ACP subprocess from the test executable.
// This exercises terminal negotiation and the real adapter transport offline.
func TestPTYChild(t *testing.T) {
	switch os.Getenv("PETAL_PTY_ROLE") {
	case "client":
		os.Args = []string{"petal"}
		if path := os.Getenv("PETAL_PTY_CONFIG"); path != "" {
			os.Args = append(os.Args, "--config", path)
		}
		if os.Getenv("PETAL_PTY_RESUME") == "1" {
			os.Args = append(os.Args, "--resume")
		}
		main()
	case "server":
		os.Args = []string{"petal", "acp", "--config", os.Getenv("PETAL_PTY_CONFIG"), "--account", "missing"}
		main()
	case "adapter":
		runPTYAdapter(t)
	case "live-fixture":
		runLivePTYAdapter(t, false)
	case "live-worker":
		runLivePTYAdapter(t, true)
	}
}

func runPTYAdapter(t *testing.T) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 12<<20)
	encoder := json.NewEncoder(os.Stdout)
	send := func(fields map[string]any) {
		fields["jsonrpc"] = "2.0"
		if err := encoder.Encode(fields); err != nil {
			t.Fatal(err)
		}
	}
	var activePrompt, pendingSetup int
	var approvedSetup bool
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
			Result struct{ Outcome struct{ OptionID string } }
			Params struct {
				Prompt []struct {
					Type, Text string
					Resource   struct{ Blob string }
				}
				SessionID string
				ConfigID  string
				Value     string
			}
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		if request.Method == "" && request.ID == 991 && pendingSetup != 0 {
			if request.Result.Outcome.OptionID != "allow" {
				t.Fatal("setup permission was declined")
			}
			request.Method, request.ID = "session/new", pendingSetup
			pendingSetup, approvedSetup = 0, true
		}
		result := map[string]any{}
		switch request.Method {
		case "initialize":
			result["protocolVersion"] = 1
			if os.Getenv("PETAL_PTY_RESUME_WITHOUT_REPLAY") == "1" {
				result["agentCapabilities"] = json.RawMessage(`{"sessionCapabilities":{"resume":{}},"promptCapabilities":{"embeddedContext":true}}`)
			} else {
				result["agentCapabilities"] = map[string]bool{"loadSession": true}
			}
			result["_meta"] = map[string]any{"steering": map[string]bool{"supported": true}}
		case "session/new":
			if os.Getenv("PETAL_PTY_SETUP_PERMISSION") == "1" && !approvedSetup {
				pendingSetup = request.ID
				send(map[string]any{"id": 991, "method": "session/request_permission", "params": map[string]any{
					"sessionId": "pty-thread",
					"toolCall":  map[string]string{"toolCallId": "setup", "title": "Approve setup", "status": "pending"},
					"options":   []map[string]string{{"optionId": "allow", "name": "Allow", "kind": "allow_once"}},
				}})
				continue
			}
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
		case "session/resume":
			if request.Params.SessionID != "pty-thread" {
				t.Fatalf("wrong resume ID %q", request.Params.SessionID)
			}
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
			block := request.Params.Prompt[0]
			if block.Type == "resource" {
				if blob := block.Resource.Blob; len(blob) != base64.StdEncoding.EncodedLen(4<<20) || !strings.HasPrefix(blob, "AAEC") {
					t.Fatal("large inline attachment did not reach the adapter")
				}
				send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "pty-thread", "update": map[string]any{
					"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "attachment-received"},
				}}})
				send(map[string]any{"id": request.ID, "result": map[string]string{"stopReason": "end_turn"}})
				continue
			}
			if block.Text == "/status-report build" {
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
	cmd.Env = ptyEnvironment(t, dir, configPath)
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
	await("alternate…")
	if err := os.Mkdir(filepath.Join(dir, "second"), 0700); err != nil {
		t.Fatal(err)
	}
	write("/new " + dir + "/second/../second\r")
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
