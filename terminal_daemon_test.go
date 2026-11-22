//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
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

	"petal/internal/livesession"
	"petal/internal/localdaemon"
	"petal/internal/sessions"
)

type terminalProcess struct {
	cmd *exec.Cmd
	tty *os.File
	out chan string
	buf string
}

func startTerminal(t *testing.T, ctx context.Context, exe, cwd string, env []string) *terminalProcess {
	t.Helper()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPTYChild$")
	cmd.Dir, cmd.Env = cwd, env
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 32)
	go func() {
		defer close(out)
		buf := make([]byte, 8192)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				select {
				case out <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	p := &terminalProcess{cmd: cmd, tty: tty, out: out}
	t.Cleanup(func() {
		tty.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return p
}

func (p *terminalProcess) write(t *testing.T, input string) {
	t.Helper()
	if _, err := p.tty.WriteString(input); err != nil {
		t.Fatal(err)
	}
}

func (p *terminalProcess) await(t *testing.T, ctx context.Context, marker string) {
	t.Helper()
	for {
		plain := ansi.Strip(p.buf)
		if strings.Contains(plain, marker) {
			p.buf = ""
			return
		}
		select {
		case part, ok := <-p.out:
			if !ok {
				t.Fatalf("terminal exited waiting for %q: %q", marker, plain)
			}
			p.buf += part
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %q", marker, plain)
		}
	}
}

func (p *terminalProcess) detach(t *testing.T) {
	t.Helper()
	p.write(t, "\x03")
	if err := p.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalDaemonReattachesPendingPermissionAndChild(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(workspace, "config.toml")
			source := fmt.Sprintf("acp_v2=%t\nstream_responses=true\n", version == 2)
			for _, model := range []string{"fixture", "worker"} {
				source += fmt.Sprintf(`[[accounts]]
name=%q
[[accounts.env]]
name="PETAL_PTY_ROLE"
value=%q
[[accounts.env]]
name="PETAL_PTY_ACP_VERSION"
value=%q
[[accounts.models]]
id=%q
command=[%q, "-test.run=^TestPTYChild$"]
`, model, "live-"+model, fmt.Sprint(version), model, exe)
			}
			if err := os.WriteFile(config, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			env := ptyEnvironment(t, workspace, config)
			// Resolve the explicit path in the launching cwd. The daemon has
			// process cwd / and the adapter must run in the session workspace.
			env = append(env, "PETAL_PTY_CONFIG=config.toml")
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			first := startTerminal(t, ctx, exe, workspace, env)
			first.await(t, ctx, "control center")
			first.write(t, "\r")
			first.await(t, ctx, "choose a model")
			first.write(t, "\r")
			first.await(t, ctx, "ready")
			first.write(t, "Original task\r")
			first.await(t, ctx, "Approve fixture")
			first.detach(t)

			root := ""
			for _, value := range env {
				if candidate, ok := strings.CutPrefix(value, "XDG_RUNTIME_DIR="); ok {
					root = candidate
				}
			}
			endpoint, err := localdaemon.DiscoverProfile(root, config)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := endpoint.Dial(ctx)
			if err != nil {
				t.Fatalf("daemon died when client detached: %v", err)
			}
			observer, err := livesession.Connect(ctx, conn)
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			list, err := observer.List(ctx)
			if err != nil || len(list) != 1 || list[0].Cwd != workspace {
				t.Fatalf("expected one live parent in %q: %+v, %v", workspace, list, err)
			}
			before, err := observer.Attach(ctx, livesession.AttachOptions{Handle: list[0].Handle})
			if err != nil || len(before.Pending) != 1 || !before.Running {
				t.Fatalf("permission and turn did not survive detach: %+v, %v", before, err)
			}
			t.Setenv("XDG_STATE_HOME", workspace)
			watch, err := sessions.WatchChildStatuses(workspace)
			if err != nil {
				t.Fatal(err)
			}
			stopWatch := context.AfterFunc(ctx, watch.Close)
			defer stopWatch()
			defer watch.Close()
			if err := os.WriteFile(filepath.Join(workspace, "release-child"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			for {
				statuses, err := watch.Next()
				if err != nil {
					t.Fatal(err)
				}
				complete := false
				for binding, status := range statuses {
					complete = complete || binding.ParentID == before.SessionID && status.Unread
				}
				if complete {
					break
				}
			}

			second := startTerminal(t, ctx, exe, workspace, env)
			second.await(t, ctx, "Approve fixture")
			second.write(t, "1\r")
			second.await(t, ctx, "child-followup-confirmed")
			cursor := before.Head
			followupSeen := false
			for {
				event, err := observer.Next(ctx, cursor)
				if err != nil {
					t.Fatal(err)
				}
				cursor = event.Sequence
				followupSeen = followupSeen || strings.Contains(string(event.Params), "child-followup-confirmed")
				if followupSeen && (version == 1 && event.Kind == livesession.CallFinished && event.Method == "session/prompt" ||
					version == 2 && strings.Contains(string(event.Params), `"state":"idle"`)) {
					break
				}
			}
			second.detach(t)

			after, err := observer.Snapshot(ctx)
			if err != nil || before.Handle != after.Handle || before.SessionID != after.SessionID || after.Running || len(after.Pending) != 0 {
				t.Fatalf("same conversation should finish with no pending question: %+v, %v", after, err)
			}
			rows, err := sessions.Transcript(workspace, after.SessionID)
			user, followup := 0, 0
			for _, row := range rows {
				if row.Role == "user" && row.Body == "Original task" {
					user++
				}
				if row.Role == "assistant" && strings.Contains(row.Body, "child-followup-confirmed") {
					followup++
				}
			}
			if err != nil || user != 1 || followup != 1 {
				t.Fatalf("expected one archived user prompt and child follow-up: %+v, %v", rows, err)
			}
		})
	}
}

func TestTerminalDaemonReopensArchive(t *testing.T) {
	workspace := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(workspace, "petal")
	if err := os.Mkdir(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`[[accounts]]
name="fixture"
[[accounts.env]]
name="PETAL_PTY_ROLE"
value="adapter"
[[accounts.env]]
name="PETAL_PTY_RESUME_WITHOUT_REPLAY"
value="1"
[[accounts.env]]
name="PETAL_PTY_SETUP_PERMISSION"
value="1"
[[accounts.models]]
id="fixture"
command=[%q, "-test.run=^TestPTYChild$"]
`, exe)
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	env := ptyEnvironment(t, workspace, "")
	first := startTerminal(t, ctx, exe, workspace, env)
	first.await(t, ctx, "control center")
	first.write(t, "\r")
	first.await(t, ctx, "choose a model")
	first.write(t, "\r")
	first.await(t, ctx, "Approve setup")
	first.write(t, "1\r")
	first.await(t, ctx, "ready")
	first.write(t, "/status-report build\r")
	first.await(t, ctx, "status-command-confirmed")
	if err := os.WriteFile(filepath.Join(workspace, "payload.bin"), bytes.Repeat([]byte{0, 1, 2, 3}, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	first.write(t, "/attach payload.bin\r")
	first.await(t, ctx, "attachment-received")
	first.write(t, "/close\r")
	if err := first.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	second := startTerminal(t, ctx, exe, workspace, append(env, "PETAL_PTY_RESUME=1"))
	second.await(t, ctx, "control center")
	second.write(t, "\r")
	second.await(t, ctx, "status-command-confirmed")
	second.detach(t)
}

type terminalAdapterFrame struct {
	ID     json.RawMessage
	Method string
	Params struct {
		Cwd, SessionID string
		Prompt         []struct{ Text string }
		MCPServers     []struct {
			Name, Command string
			Args          []string
			Env           []struct{ Name, Value string }
		}
	}
	Result struct{ Outcome struct{ OptionID string } }
}

func runLivePTYAdapter(t *testing.T, child bool) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	v2 := os.Getenv("PETAL_PTY_ACP_VERSION") == "2"
	id := "parent-session"
	if child {
		id = "child-session"
	}
	in, out := bufio.NewScanner(os.Stdin), json.NewEncoder(os.Stdout)
	send := func(frame map[string]any) {
		frame["jsonrpc"] = "2.0"
		if err := out.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	reply := func(id json.RawMessage, result any) { send(map[string]any{"id": id, "result": result}) }
	update := func(fields map[string]any) {
		send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": id, "update": fields}})
	}
	state := func(state string) {
		if v2 {
			update(map[string]any{"sessionUpdate": "state_update", "state": state, "stopReason": "end_turn"})
		}
	}
	message := func(role, key, text string) {
		kind := role + "_message_chunk"
		update(map[string]any{"sessionUpdate": kind, "messageId": key, "content": map[string]string{"type": "text", "text": text}})
	}
	var delegate func()
	var promptID json.RawMessage
	for in.Scan() {
		var frame terminalAdapterFrame
		if err := json.Unmarshal(in.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		switch frame.Method {
		case "initialize":
			if v2 {
				reply(frame.ID, json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"1"},"capabilities":{"session":{"mcp":{"stdio":{}}}}}`))
			} else {
				reply(frame.ID, json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}`))
			}
		case "session/new":
			resolved, err := filepath.EvalSymlinks(frame.Params.Cwd)
			actual, actualErr := filepath.EvalSymlinks(cwd)
			if err != nil || actualErr != nil || resolved != actual {
				t.Fatalf("adapter cwd %q, request cwd %q", cwd, frame.Params.Cwd)
			}
			for _, server := range frame.Params.MCPServers {
				if server.Name != "petal" {
					continue
				}
				delegate = func() {
					cmd := exec.Command(server.Command, server.Args...)
					cmd.Env = os.Environ()
					for _, env := range server.Env {
						cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
					}
					runPTYDelegation(t, cmd)
				}
			}
			reply(frame.ID, map[string]string{"sessionId": id})
		case "session/prompt":
			if len(frame.Params.Prompt) != 1 || frame.Params.SessionID != id {
				t.Fatalf("invalid prompt: %+v", frame)
			}
			input := frame.Params.Prompt[0].Text
			state("running")
			if v2 {
				if !strings.Contains(input, "Petal delegation update:") {
					message("user", "user-input", input)
				}
				reply(frame.ID, map[string]string{"messageId": "user-input"})
			}
			switch {
			case child:
				waitForChildRelease(t, cwd)
				message("agent", "child", "child-receipt")
			case strings.Contains(input, "Petal delegation update:"):
				message("agent", "followup", "child-followup-confirmed")
			default:
				if delegate == nil || input != "Original task" {
					t.Fatal("missing delegation endpoint or original user input")
				}
				delegate()
				message("agent", "partial", "before-detach")
				promptID = frame.ID
				state("requires_action")
				tool := map[string]string{"toolCallId": "fixture", "title": "Approve fixture", "status": "pending"}
				params := map[string]any{
					"sessionId": id,
					"options":   []map[string]string{{"optionId": "allow", "name": "Allow", "kind": "allow_once"}},
				}
				if v2 {
					params["title"] = "Approve fixture"
					params["subject"] = map[string]any{"type": "tool_call", "toolCall": tool}
				} else {
					params["toolCall"] = tool
				}
				send(map[string]any{"id": "permission", "method": "session/request_permission", "params": params})
				continue
			}
			state("idle")
			if !v2 {
				reply(frame.ID, map[string]string{"stopReason": "end_turn"})
			}
		case "":
			if string(frame.ID) != `"permission"` || frame.Result.Outcome.OptionID != "allow" {
				t.Fatalf("permission reply %+v", frame)
			}
			state("running")
			message("agent", "approved", "permission-confirmed")
			state("idle")
			if !v2 {
				reply(promptID, map[string]string{"stopReason": "end_turn"})
			}
		default:
			t.Fatalf("unexpected method %s", frame.Method)
		}
	}
	if err := in.Err(); err != nil {
		t.Fatal(err)
	}
}

func waitForChildRelease(t *testing.T, cwd string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(filepath.Join(cwd, "release-child")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("test never released the child")
}

func runPTYDelegation(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cmd.Wait() }()
	reader := bufio.NewScanner(out)
	request := func(raw string, response bool) {
		t.Helper()
		if _, err := fmt.Fprintln(in, raw); err != nil {
			t.Fatal(err)
		}
		if response && (!reader.Scan() || bytes.Contains(reader.Bytes(), []byte(`"isError":true`)) ||
			bytes.Contains(reader.Bytes(), []byte(`"error":`))) {
			t.Fatalf("MCP response %s: %v", reader.Bytes(), reader.Err())
		}
	}
	request(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`, true)
	request(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, false)
	request(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delegate","arguments":{"model":"worker","task":"collect receipt"}}}`, true)
}
