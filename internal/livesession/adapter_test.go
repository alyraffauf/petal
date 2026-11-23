//go:build linux || darwin

package livesession

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type adapterFrame struct {
	ID     json.RawMessage
	Method string
	Params map[string]json.RawMessage
	Result json.RawMessage
	Error  json.RawMessage
}

type adapterFixture struct {
	t   *testing.T
	in  *bufio.Scanner
	out *json.Encoder
	cwd string
	id  string
	v2  bool
}

func TestLiveAdapterFixture(t *testing.T) {
	version := os.Getenv("PETAL_LIVE_FIXTURE")
	if version == "" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	adapter := adapterFixture{t: t, in: bufio.NewScanner(os.Stdin), out: json.NewEncoder(os.Stdout), cwd: cwd, id: "thread", v2: version == "2"}
	if os.Getenv("PETAL_LIVE_MODEL") == "child" {
		adapter.id = "child"
	}
	awaitingStart := false
	for adapter.in.Scan() {
		frame := adapter.frame()
		switch frame.Method {
		case "initialize":
			if adapter.v2 {
				adapter.reply(frame.ID, json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"1"},"capabilities":{"session":{"fork":{},"list":{},"mcp":{"stdio":{}}}}}`))
			} else {
				adapter.reply(frame.ID, json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"sessionCapabilities":{"fork":{},"close":{},"list":{}}}}`))
			}
		case "session/fork":
			adapter.reply(frame.ID, map[string]string{"sessionId": "forked"})
		case "session/new", "session/load", "session/resume":
			if id := field(frame.Params, "sessionId"); id != "" {
				adapter.id = id
			}
			supplied, err := filepath.EvalSymlinks(field(frame.Params, "cwd"))
			resolved, resolveErr := filepath.EvalSymlinks(cwd)
			if err != nil || resolveErr != nil || supplied != resolved {
				t.Fatalf("adapter cwd %s, supplied %s", cwd, frame.Params["cwd"])
			}
			if frame.Method == "session/resume" {
				_, activeErr := os.Stat(filepath.Join(cwd, "resume-running"))
				if activeErr == nil {
					adapter.text("resumed-history")
				}
				adapter.update(json.RawMessage(`{"sessionUpdate":"replay_complete"}`))
				if activeErr == nil {
					adapter.state("running", "")
					adapter.text("resumed-active")
				} else {
					adapter.state("idle", "")
				}
			}
			adapter.reply(frame.ID, map[string]any{"sessionId": adapter.id, "fixtureMCP": frame.Params["mcpServers"]})
		case "session/prompt":
			if adapter.v2 && strings.Contains(string(frame.Params["prompt"]), "reply-before-running") {
				adapter.reply(frame.ID, map[string]string{"messageId": "acknowledged"})
				adapter.state("idle", "")
				awaitingStart = true
			} else {
				adapter.prompt(frame)
			}
		case "session/list":
			if awaitingStart {
				awaitingStart = false
				adapter.state("running", "")
				adapter.state("idle", "end_turn")
			} else {
				adapter.update(map[string]string{"sessionUpdate": "notice", "message": "list waiting for prompt"})
				prompt := adapter.read()
				if prompt.Method != "session/prompt" {
					t.Fatalf("expected prompt while listing, got %s", prompt.Method)
				}
				adapter.prompt(prompt)
			}
			adapter.reply(frame.ID, json.RawMessage(`{"sessions":[]}`))
		case "session/cancel":
			adapter.text("adapter-cancelled")
			adapter.state("idle", "cancelled")
		case "session/close":
			adapter.reply(frame.ID, map[string]string{})
		default:
			t.Fatalf("unexpected ACP frame %+v", frame)
		}
	}
	if err := adapter.in.Err(); err != nil {
		t.Fatal(err)
	}
}

func field(params map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(params[key], &value)
	return value
}

func (a adapterFixture) frame() adapterFrame {
	var result adapterFrame
	if err := json.Unmarshal(a.in.Bytes(), &result); err != nil {
		a.t.Fatalf("invalid frame: %v", err)
	}
	return result
}

func (a adapterFixture) read() adapterFrame {
	if !a.in.Scan() {
		a.t.Fatalf("read ACP frame: %v", a.in.Err())
	}
	return a.frame()
}

func (a adapterFixture) send(values map[string]any) {
	values["jsonrpc"] = "2.0"
	if err := a.out.Encode(values); err != nil {
		a.t.Fatal(err)
	}
}

func (a adapterFixture) reply(id json.RawMessage, result any) {
	a.send(map[string]any{"id": id, "result": result})
}

func (a adapterFixture) update(update any) {
	a.send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": a.id, "update": update}})
}

func (a adapterFixture) text(text string) {
	a.update(map[string]any{
		"sessionUpdate": "agent_message_chunk", "messageId": "assistant",
		"content": map[string]string{"type": "text", "text": text},
	})
}

func (a adapterFixture) state(state, reason string) {
	a.update(map[string]string{"sessionUpdate": "state_update", "state": state, "stopReason": reason})
}

func (a adapterFixture) request(method string, params map[string]any) json.RawMessage {
	params["sessionId"] = a.id
	a.send(map[string]any{"id": "callback", "method": method, "params": params})
	res := a.read()
	if string(res.ID) != `"callback"` || res.Error != nil {
		a.t.Fatalf("%s response = %+v", method, res)
	}
	return res.Result
}

func (a adapterFixture) waitForRelease() {
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(a.cwd, "release")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			a.t.Fatal("fixture release was never created")
		}
		time.Sleep(time.Millisecond)
	}
}

func (a adapterFixture) prompt(frame adapterFrame) {
	var prompt []struct{ Text string }
	if json.Unmarshal(frame.Params["prompt"], &prompt) != nil || len(prompt) != 1 || field(frame.Params, "sessionId") != a.id {
		a.t.Fatal("invalid fixture prompt")
	}
	text := prompt[0].Text
	if a.v2 {
		a.state("running", "")
		a.update(map[string]any{
			"sessionUpdate": "user_message", "messageId": text, "content": frame.Params["prompt"],
		})
		a.reply(frame.ID, map[string]string{"messageId": text})
	}
	if text == "exit" {
		os.Exit(0)
	}
	if text == "hold for steering" {
		a.text("ready for steering")
		for {
			next := a.read()
			if next.Method == "session/cancel" {
				break
			}
			if next.Method != "_session/steering" {
				a.t.Fatalf("unexpected call: %s", next.Method)
			}
			outcome := "injected"
			if strings.Contains(string(next.Params["prompt"]), "defer input") {
				outcome = "promptRequired"
			}
			a.reply(next.ID, map[string]string{"outcome": outcome})
		}
		a.reply(frame.ID, map[string]string{"stopReason": "cancelled"})
		return
	}
	if text == "cancel" {
		cancel := a.read()
		if cancel.Method != "session/cancel" || field(cancel.Params, "sessionId") != a.id {
			a.t.Fatalf("prompt was overtaken by %+v", cancel)
		}
		a.text("adapter-cancelled")
		if a.v2 {
			a.state("idle", "cancelled")
		} else {
			a.reply(frame.ID, map[string]string{"stopReason": "cancelled"})
		}
		return
	}
	switch {
	case text == "list overlap":
		a.text("prompt during list")
	case a.id == "child":
		a.text("child-work")
	case strings.Contains(text, "child-work") || strings.Contains(text, "activity"):
		a.text("delegation-wake")
		if a.v2 {
			if cancel := a.read(); cancel.Method != "session/cancel" || field(cancel.Params, "sessionId") != a.id {
				a.t.Fatalf("expected cancellation of the wake, got %+v", cancel)
			}
			a.text("wake-cancelled")
		}
	default:
		a.text("before ")
		a.waitForRelease()
		if text != "delegate" {
			a.permission()
			if !a.v2 {
				a.workspace()
			}
			a.text("after")
		}
	}
	if a.v2 {
		a.state("idle", "end_turn")
	} else {
		a.reply(frame.ID, map[string]string{"stopReason": "end_turn"})
	}
}

func (a adapterFixture) permission() {
	tool := map[string]string{"toolCallId": "fixture", "title": "Fixture action", "status": "pending"}
	params := map[string]any{
		"toolCall": tool,
		"options":  []map[string]string{{"optionId": "allow", "name": "Allow", "kind": "allow_once"}, {"optionId": "deny", "name": "Deny", "kind": "reject_once"}},
	}
	if a.v2 {
		delete(params, "toolCall")
		params["title"] = "Fixture action"
		params["subject"] = map[string]any{"type": "tool_call", "toolCall": tool}
	}
	result := a.request("session/request_permission", params)
	var response struct{ Outcome struct{ OptionID string } }
	if json.Unmarshal(result, &response) != nil || response.Outcome.OptionID != "allow" {
		a.t.Fatalf("permission was not approved: %s", result)
	}
}

func (a adapterFixture) workspace() {
	content := a.request("fs/read_text_file", map[string]any{"path": filepath.Join(a.cwd, "source")})
	var read struct{ Content string }
	if err := json.Unmarshal(content, &read); err != nil || read.Content != "workspace contents" {
		a.t.Fatalf("read result = %s, %v", content, err)
	}
	a.request("fs/write_text_file", map[string]any{"path": filepath.Join(a.cwd, "written"), "content": read.Content})
	terminal := a.request("terminal/create", map[string]any{"command": "/bin/sh", "args": []string{"-c", "cat source"}})
	var created struct{ TerminalID string }
	if err := json.Unmarshal(terminal, &created); err != nil || created.TerminalID == "" {
		a.t.Fatalf("terminal creation result = %s, %v", terminal, err)
	}
	args := map[string]any{"terminalId": created.TerminalID}
	a.request("terminal/wait_for_exit", args)
	output := a.request("terminal/output", args)
	var ran struct{ Output string }
	if err := json.Unmarshal(output, &ran); err != nil || ran.Output != "workspace contents" {
		a.t.Fatalf("terminal output = %s, %v", output, err)
	}
	a.request("terminal/release", args)
}
