package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
)

// The pinned v1 draft replaces the entire plan by ID, including empty item
// lists. Removing one plan must leave every other plan and the transcript intact.
func TestClientPlanReplacementAndRemoval(t *testing.T) {
	h := &harness{events: make(chan tea.Msg, 1)}
	m := &terminal{root: bloom.New(), h: h, ctx: t.Context()}
	m.Messages = []entry{{"assistant", "answer"}}
	update := func(raw string, want ...entry) {
		t.Helper()
		_, err := h.callback(t.Context(), "session/update", json.RawMessage(`{"sessionId":"s","update":`+raw+`}`))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case e := <-h.events:
			m.Update(e)
		default:
			t.Fatal("plan update did not reach the model")
		}
		if len(m.planRows) != len(want) || (len(want) > 0 && !reflect.DeepEqual(m.planRows, want)) {
			t.Fatalf("plans = %+v, want %+v", m.planRows, want)
		}
		if len(m.Messages) != 1 || m.Messages[0].Text != "answer" {
			t.Fatalf("plan modified transcript: %+v", m.Messages)
		}
	}

	first := entry{"system", "**Plan**\n- Inspect · pending"}
	second := entry{"system", "**Plan**\n## Follow up"}
	empty := entry{"system", "**Plan**\n"}
	update(`{"sessionUpdate":"plan_update","plan":{"type":"items","planId":"first","entries":[{"content":"Inspect","priority":"high","status":"pending"}]}}`, first)
	update(`{"sessionUpdate":"plan_update","plan":{"type":"markdown","planId":"second","content":"## Follow up","path":"/work/PLAN.md"}}`, first, second)
	update(`{"sessionUpdate":"plan_update","plan":{"type":"items","planId":"first","entries":[]}}`, empty, second)
	update(`{"sessionUpdate":"plan_removed","planId":"first"}`, second)
	update(`{"sessionUpdate":"plan_removed","planId":"second"}`)
	update(`{"sessionUpdate":"plan","entries":[{"content":"Inspect","priority":"high","status":"pending"}]}`, first)
	update(`{"sessionUpdate":"plan","entries":[]}`, empty)
}

// TestClientWorkspaceAdapter is the stdio peer for the client round trip.
func TestClientWorkspaceAdapter(t *testing.T) {
	if os.Getenv("PETAL_CLIENT_WORKSPACE_FIXTURE") != "1" {
		return
	}
	ready := make(chan struct{})
	var conn *acp.Connection
	var cwd string
	conn = acp.NewConnection(func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		<-ready
		switch method {
		case "initialize":
			var init acp.InitializeRequest
			if err := json.Unmarshal(params, &init); err != nil {
				return nil, acp.NewInvalidParams(err.Error())
			}
			caps := init.ClientCapabilities
			if init.ProtocolVersion != 1 || caps.Plan == nil || !caps.Fs.ReadTextFile || !caps.Fs.WriteTextFile || !caps.Terminal || caps.Session == nil || caps.Session.Compaction == nil || caps.Session.Notices == nil || caps.Session.ConfigOptions == nil || caps.Session.ConfigOptions.Boolean == nil || caps.Elicitation == nil || caps.Elicitation.Url == nil || caps.Elicitation.Form == nil || caps.Subagents == nil {
				return nil, acp.NewInvalidParams("missing client capabilities")
			}
			return acp.InitializeResponse{ProtocolVersion: 1}, nil
		case "session/new":
			var req acp.NewSessionRequest
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, acp.NewInvalidParams(err.Error())
			}
			cwd = req.Cwd
			return map[string]string{"sessionId": "workspace"}, nil
		case "session/prompt":
			var req acp.PromptRequest
			if err := json.Unmarshal(params, &req); err != nil || req.SessionId != "workspace" {
				return nil, acp.NewInvalidParams("invalid prompt session")
			}
			if err := workspaceRoundTrip(ctx, conn, cwd, req.SessionId); err != nil {
				return nil, acp.NewInternalError(err.Error())
			}
			return map[string]string{"stopReason": "end_turn"}, nil
		default:
			return nil, acp.NewMethodNotFound(method)
		}
	}, os.Stdout, os.Stdin)
	close(ready)
	<-conn.Done()
}

func workspaceRoundTrip(ctx context.Context, conn *acp.Connection, cwd string, id acp.SessionId) error {
	path := filepath.Join(cwd, "notes.txt")
	_, err := acp.SendRequest[acp.WriteTextFileResponse](conn, ctx, "fs/write_text_file",
		acp.WriteTextFileRequest{SessionId: id, Path: path, Content: "first\nsecond\nthird"})
	if err != nil {
		return err
	}
	line, limit := int64(2), int64(1)
	read, err := acp.SendRequest[acp.ReadTextFileResponse](conn, ctx, "fs/read_text_file",
		acp.ReadTextFileRequest{SessionId: id, Path: path, Line: &line, Limit: &limit})
	if err != nil {
		return err
	}
	if read.Content != "second\n" {
		return fmt.Errorf("unexpected ranged file result %q", read.Content)
	}
	term, err := acp.SendRequest[acp.CreateTerminalResponse](conn, ctx, "terminal/create",
		acp.CreateTerminalRequest{SessionId: id, Command: "printf", Args: []string{"fixture-output"}})
	if err != nil {
		return err
	}
	terminalParams := map[string]any{"sessionId": id, "terminalId": term.TerminalId}
	if _, err := acp.SendRequest[acp.TerminalExitStatus](conn, ctx, "terminal/wait_for_exit", terminalParams); err != nil {
		return err
	}
	output, err := acp.SendRequest[acp.TerminalOutputResponse](conn, ctx, "terminal/output", terminalParams)
	if err != nil {
		return err
	}
	if output.Output != "fixture-output" || output.Truncated {
		return fmt.Errorf("unexpected terminal output %+v", output)
	}
	if _, err := acp.SendRequest[acp.ReleaseTerminalResponse](conn, ctx, "terminal/release", terminalParams); err != nil {
		return err
	}
	permission, err := acp.SendRequest[struct {
		Outcome struct{ Outcome, OptionID string }
	}](conn, ctx, "session/request_permission", map[string]any{
		"sessionId": id, "toolCall": map[string]any{"toolCallId": "tool", "title": "Fixture step", "rawInput": map[string]string{"command": "inspect fixture"}},
		"options": []map[string]string{{"optionId": "approve", "name": "Allow once", "kind": "allow_once"}},
	})
	if err != nil {
		return err
	}
	if permission.Outcome.Outcome != "cancelled" || permission.Outcome.OptionID != "" {
		return fmt.Errorf("expected cancelled permission, got %+v", permission)
	}
	return nil
}

func TestClientWorkspaceCallbacksOverACP(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := routing.Config{Model: "m", PinnedAccount: "local", Accounts: map[string]routing.Account{
		"local": {Models: map[string][]string{"m": {exe, "-test.run=^TestClientWorkspaceAdapter$"}}, Env: map[string]string{"PETAL_CLIENT_WORKSPACE_FIXTURE": "1"}},
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h := startHarness(ctx, cfg)
	defer h.close()
	if _, err := h.open(ctx, cfg, cwd, false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		result, err := acp.SendRequest[acp.PromptResponse](h.conn, ctx, "session/prompt",
			acp.PromptRequest{SessionId: acp.SessionId(h.id), Prompt: []acp.ContentBlock{acp.TextBlock("read notes")}})
		if err == nil && result.StopReason != "end_turn" {
			err = fmt.Errorf("unexpected stop reason %q", result.StopReason)
		}
		done <- err
	}()
	for _, step := range []struct {
		title string
		reply int
	}{{"Write ", 0}, {"Run ", 0}, {"inspect fixture", -1}} {
		select {
		case msg := <-h.events:
			q, ok := msg.(event)
			if !ok || q.ask == nil || !strings.Contains(q.text, step.title) {
				t.Fatalf("unexpected callback event %+v", msg)
			}
			q.ask.reply <- step.reply
		case err := <-done:
			t.Fatalf("prompt ended before %s: %v", step.title, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
