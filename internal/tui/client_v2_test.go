package tui

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestV2ClientAdapter(t *testing.T) {
	fixture := os.Getenv("PETAL_V2_CLIENT_FIXTURE")
	if fixture == "" {
		return
	}
	ready := make(chan struct{})
	var conn *acp.Connection
	conn = acp.NewConnection(func(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
		<-ready
		invalid := func(message string) (any, *acp.RequestError) {
			return nil, acp.NewInvalidParams(message)
		}
		notify := func(update string) *acp.RequestError {
			params := json.RawMessage(`{"sessionId":"native","update":` + update + `}`)
			if err := conn.SendNotification(ctx, "session/update", params); err != nil {
				return acp.NewInternalError(err.Error())
			}
			return nil
		}
		switch method {
		case "initialize":
			var params struct {
				ProtocolVersion int
				Info            struct{ Name string }
				Capabilities    struct {
					Auth struct{ Terminal *struct{} }
				}
			}
			if json.Unmarshal(raw, &params) != nil {
				return invalid("invalid initialize")
			}
			if (fixture == "legacy" || fixture == "no-sessions") && params.ProtocolVersion == 1 {
				return json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"promptCapabilities":{"image":true}}}`), nil
			}
			if params.ProtocolVersion != 2 || params.Info.Name != "petal" || params.Capabilities.Auth.Terminal == nil {
				return invalid("missing v2 client capabilities")
			}
			if fixture == "legacy" {
				return nil, acp.NewRequestError(-32022, "Unsupported protocol version", nil)
			}
			if fixture == "no-sessions" {
				return json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"1"},"capabilities":{"session":null}}`), nil
			}
			return json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"1"},"capabilities":{"session":{"prompt":{"image":{},"audio":null},"mcp":{"stdio":{},"http":{}}}}}`), nil
		case "session/new", "session/resume":
			var params struct {
				SessionID  string
				ReplayFrom struct{ Type string }
				MCPServers []struct{ Name, Type string }
			}
			if json.Unmarshal(raw, &params) != nil {
				return invalid("invalid session parameters")
			}
			if fixture == "legacy" || fixture == "no-sessions" {
				return json.RawMessage(`{"sessionId":"native"}`), nil
			}
			if len(params.MCPServers) != 2 || params.MCPServers[0].Type != "stdio" || params.MCPServers[1].Type != "http" {
				return invalid("MCP transports were not forwarded")
			}
			if method == "session/new" {
				return json.RawMessage(`{"sessionId":"native"}`), nil
			}
			if params.SessionID != "native" || params.ReplayFrom.Type != "start" {
				return invalid("expected full replay of the native session")
			}
			if fixture == "resume" {
				for _, update := range []string{
					`{"sessionUpdate":"user_message","messageId":"user","content":[{"type":"text","text":"searchable question"}]}`,
					`{"sessionUpdate":"tool_call_update","toolCallId":"tool","title":"Inspect","status":"completed"}`,
				} {
					if err := notify(update); err != nil {
						return nil, err
					}
				}
			}
			if fixture != "truncated" {
				if err := notify(`{"sessionUpdate":"replay_complete"}`); err != nil {
					return nil, err
				}
			}
			if err := notify(`{"sessionUpdate":"state_update","state":"running"}`); err != nil {
				return nil, err
			}
			return json.RawMessage(`{}`), nil
		default:
			return nil, acp.NewMethodNotFound(method)
		}
	}, os.Stdout, os.Stdin)
	close(ready)
	<-conn.Done()
}

func TestV2ClientNegotiatesAndReplaysThroughProxy(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"resume", "empty", "truncated", "legacy", "no-sessions"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cwd := t.TempDir()
			cfg := routing.Config{
				ACPv2: true, Model: "model", PinnedAccount: "local",
				Accounts: map[string]routing.Account{"local": {
					Models: map[string][]string{"model": {exe, "-test.run=^TestV2ClientAdapter$"}},
					Env:    map[string]string{"PETAL_V2_CLIENT_FIXTURE": mode},
				}},
			}
			resume := mode != "legacy" && mode != "no-sessions"
			const id = `["local","model","native"]`
			if resume {
				cfg.MCPServers = []routing.MCPServer{
					{Name: "stdio", Command: exe},
					{Name: "http", URL: "https://example.test/mcp", Headers: []routing.MCPHeader{{Name: "X-Test", FromEnv: "PATH"}}},
				}
				if _, err := sessions.Create(cwd, sessions.Record{
					ID: id, Account: "local", Model: "model", Opened: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
				if err := sessions.SaveTranscript(cwd, id, []sessions.Message{{Role: "user", Body: "stale"}}, true); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			h := startHarness(ctx, cfg)
			defer h.close()
			if resume {
				h.id = id
			}
			account, err := h.open(ctx, cfg, cwd, resume)
			if (err != nil) != (mode == "truncated") {
				t.Fatalf("open: %v", err)
			}
			if h.v2.Load() != resume || !h.promptCaps.Image && mode != "truncated" {
				t.Fatalf("negotiated v2=%t, prompt capabilities %+v", h.v2.Load(), h.promptCaps)
			}
			if !resume {
				return
			}
			m := &terminal{root: bloom.New(), h: h, ctx: ctx, cfg: cfg}
			m.Cwd = cwd
			h.events <- opened{account, err}
			for len(h.events) > 0 {
				m.Update(m.listen())
			}
			var want []entry
			switch mode {
			case "resume":
				want = []entry{{"user", "searchable question"}, {"tool", "**Inspect** · completed"}}
			case "truncated":
				want = []entry{{"user", "stale"}}
			}
			if mode != "truncated" && (!slices.Equal(m.Messages, want) || !m.Busy) {
				t.Fatalf("visible replay = %+v, busy = %t", m.Messages, m.Busy)
			}
			rows, err := sessions.Transcript(cwd, id)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "truncated" {
				want = append([]entry{{"user", "stale"}}, want...)
			}
			if len(rows) != len(want) {
				t.Fatalf("indexed rows = %+v, want %+v", rows, want)
			}
			for i, row := range rows {
				if row.Role != want[i].Role || row.Body != want[i].Text {
					t.Fatalf("row %d = %+v, want %+v", i, row, want[i])
				}
			}
		})
	}
}

func TestV2PromptAcceptanceWaitsForIdle(t *testing.T) {
	h := &harness{events: make(chan tea.Msg, 1)}
	h.v2.Store(true)
	m := &terminal{root: bloom.New(), h: h, ctx: t.Context()}
	m.connection = connected
	m.outbox = []promptMessage{{text: "first"}, {text: "second"}}
	if m.deliver() == nil || len(m.outbox) != 1 || len(m.Messages) != 0 {
		t.Fatal("first submission should start with no local transcript echo")
	}
	m.Update(promptV2Result{})
	m.Update(updateV2{SessionUpdate: "state_update", State: "idle"})
	if !m.Busy || len(m.outbox) != 1 {
		t.Fatal("acceptance and a repeated idle snapshot released the queued prompt")
	}
	m.Update(updateV2{SessionUpdate: "state_update", State: "running"})
	m.Update(outputBatch{events: []event{{v2Update: json.RawMessage(
		`{"sessionUpdate":"user_message","messageId":"first","content":[{"type":"text","text":"first"}]}`,
	)}}})
	m.Update(updateV2{SessionUpdate: "state_update", State: "idle", StopReason: "end_turn"})
	if !m.Busy || len(m.outbox) != 0 || !reflect.DeepEqual(m.Messages, []entry{{"user", "first"}}) {
		t.Fatalf("after idle: busy=%t queue=%v transcript=%v", m.Busy, m.outbox, m.Messages)
	}
}

func TestV2PeerCancellationDismissesPermission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	agentIn, clientOut := io.Pipe()
	clientIn, agentOut := io.Pipe()
	defer clientOut.Close()
	defer agentOut.Close()

	h := &harness{lifetime: ctx, events: make(chan tea.Msg, 4), permissions: make(chan struct{}, 1)}
	h.v2.Store(true)
	h.conn = acp.NewConnection(h.callback, clientOut, clientIn)
	encoder := json.NewEncoder(agentOut)
	if err := encoder.Encode(json.RawMessage(`{"jsonrpc":"2.0","id":"permit","method":"session/request_permission","params":{
		"sessionId":"session","title":"Run command","description":"Inspect workspace",
		"subject":{"type":"command","command":"pwd","cwd":"/workspace"},
		"options":[{"optionId":"yes","name":"Allow once","kind":"allow_once"}]
	}}`)); err != nil {
		t.Fatal(err)
	}
	var question *permission
	select {
	case msg := <-h.events:
		e, ok := msg.(event)
		if !ok || e.ask == nil || !strings.Contains(e.text, "pwd") {
			t.Fatalf("unexpected permission event: %+v", msg)
		}
		question = e.ask
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := encoder.Encode(json.RawMessage(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"permit"}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-h.events:
		if msg != (dismissedQuestion{question}) {
			t.Fatalf("unexpected dismissal: %+v", msg)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	response := make(chan json.RawMessage, 1)
	go func() {
		var raw json.RawMessage
		_ = json.NewDecoder(agentIn).Decode(&raw)
		response <- raw
	}()
	select {
	case raw := <-response:
		var reply struct {
			ID     string
			Result struct{ Outcome struct{ Outcome string } }
		}
		if err := json.Unmarshal(raw, &reply); err != nil || reply.ID != "permit" || reply.Result.Outcome.Outcome != "cancelled" {
			t.Fatalf("invalid cancellation reply: %s", raw)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestV2CancellationFollowsPromptOnTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	agentIn, clientOut := io.Pipe()
	clientIn, agentOut := io.Pipe()
	defer clientOut.Close()
	defer agentOut.Close()

	h := &harness{id: "session", events: make(chan tea.Msg, 1)}
	h.v2.Store(true)
	h.conn = acp.NewConnection(h.callback, promptWriterV2{clientOut, h}, clientIn)
	m := &terminal{root: bloom.New(), h: h, ctx: ctx}
	m.connection = connected
	m.outbox = []promptMessage{{content: []map[string]any{{"type": "text", "text": "send"}}}}
	submit := m.deliver()
	stop := m.cancelForegroundV2()
	if submit == nil || stop == nil {
		t.Fatal("expected prompt and cancellation commands")
	}
	stopped := make(chan tea.Msg, 1)
	go func() { stopped <- stop() }()
	go submit()

	type request struct {
		ID     json.RawMessage
		Method string
	}
	requests := make(chan request, 2)
	go func() {
		decoder := json.NewDecoder(agentIn)
		for range cap(requests) {
			var message request
			if decoder.Decode(&message) != nil {
				return
			}
			requests <- message
		}
	}()
	var prompt request
	for i, method := range []string{"session/prompt", "session/cancel"} {
		select {
		case message := <-requests:
			if message.Method != method {
				t.Fatalf("wire frame %d was %q, want %q", i, message.Method, method)
			}
			if i == 0 {
				prompt = message
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	select {
	case msg := <-stopped:
		if result := msg.(cancelV2Result); result.err != nil {
			t.Fatal(result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	body := json.RawMessage(`{"jsonrpc":"2.0","id":` + string(prompt.ID) + `,"error":{"code":-32800,"message":"Request cancelled"}}`)
	if err := json.NewEncoder(agentOut).Encode(body); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-h.events:
		if result := msg.(promptV2Result); !promptCancelledV2(result.err) {
			t.Fatalf("prompt outcome: %v", result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestV2LateCancelResultKeepsDisconnectedStatus(t *testing.T) {
	h := &harness{events: make(chan tea.Msg, 1)}
	h.v2.Store(true)
	m := &terminal{root: bloom.New(), h: h, ctx: t.Context()}
	m.Update(opened{})
	m.Update(updateV2{SessionUpdate: "state_update", State: "running"})
	if m.cancelForegroundV2() == nil {
		t.Fatal("expected a cancellation command")
	}
	m.Update(disconnectedV2{})
	m.Update(cancelV2Result{})
	if m.Busy || m.Status != "disconnected" {
		t.Fatalf("late cancellation result changed disconnected state: %t, %s", m.Busy, m.Status)
	}
}
