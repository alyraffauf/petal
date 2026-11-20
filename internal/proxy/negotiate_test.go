package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
)

func TestAccountAdapter(t *testing.T) {
	name := os.Getenv("PETAL_NEGOTIATE_FIXTURE")
	if name == "" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var q struct {
			ID     json.RawMessage
			Method string
			Params map[string]any
		}
		if err := json.Unmarshal(scan.Bytes(), &q); err != nil {
			t.Fatal(err)
		}
		var r any = map[string]any{}
		switch q.Method {
		case "initialize":
			r = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": name == "b", "sessionCapabilities": map[string]any{"options": map[string]any{}, "list": map[string]any{}, "resume": map[string]any{}, "delete": map[string]any{}, "close": map[string]any{}}}, "authMethods": []map[string]any{
				{"id": "login", "name": "Sign in"},
				{"type": "terminal", "id": "interactive", "name": "Interactive", "args": []string{"auth"}},
			}}
		case "session/options":
			r = map[string]any{"configOptions": []map[string]string{{"id": "mode", "currentValue": "shared"}, {"id": "model", "currentValue": name}}}
		case "session/new":
			r = map[string]string{"sessionId": "same"}
		case "session/prompt":
			if q.Params["sessionId"] != "same" {
				t.Fatal(q.Params)
			}
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "read", "method": "fs/read_text_file", "params": map[string]string{"sessionId": "same", "path": "/work/PLAN.md"}}); err != nil {
				t.Fatal(err)
			}
			var rejected struct {
				ID    string
				Error acp.RequestError
			}
			if !scan.Scan() || json.Unmarshal(scan.Bytes(), &rejected) != nil || rejected.ID != "read" || rejected.Error.Code != -32601 {
				t.Fatalf("unadvertised file callback reply: %s", scan.Bytes())
			}
			for _, update := range []string{
				`{"sessionUpdate":"plan_update","plan":{"type":"items","planId":"p","entries":[]}}`,
				`{"sessionUpdate":"plan_update","plan":{"type":"markdown","planId":"p","content":"# Plan","path":"/work/PLAN.md"}}`,
				`{"sessionUpdate":"plan_removed","planId":"p"}`,
			} {
				if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "same", "update": json.RawMessage(update)}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "unknown/notice"}); err != nil {
				t.Fatal(err)
			}
			r = map[string]string{"stopReason": "end_turn"}
		case "unknown/notice":
			t.Fatal("unknown notification reached adapter")
		case "session/list":
			r = map[string]any{"sessions": []map[string]string{{"sessionId": "same", "cwd": "/work"}}}
		case "authenticate":
			if q.Params["methodId"] != "login" {
				t.Fatal(q.Params)
			}
			r = map[string]string{"account": name}
		case "session/resume", "session/delete", "session/close", "session/load":
			if q.Params["sessionId"] != "same" {
				t.Fatal(q.Params)
			}
			r = map[string]string{"account": name}
		}
		if q.ID != nil {
			if err := enc.Encode(map[string]any{"id": q.ID, "jsonrpc": "2.0", "result": r}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestNegotiatedAccountsAndPaging(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := routing.Config{Model: "m", Accounts: map[string]routing.Account{}}
	for _, name := range []string{"a", "b"} {
		cfg.Accounts[name] = routing.Account{Models: map[string][]string{"m": {exe, "-test.run=^TestAccountAdapter$"}}, Env: map[string]string{"PETAL_NEGOTIATE_FIXTURE": name}}
	}
	read, input := io.Pipe()
	out, write := io.Pipe()
	defer input.Close()
	defer write.Close()
	ConnectEmbedded(ctx, cfg, write, read)
	client := acp.NewConnection(nil, input, out)
	call := func(method string, params any) Object {
		t.Helper()
		r, err := acp.SendRequest[Object](client, ctx, method, params)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	hello := call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": Object{}})
	var caps acp.AgentCapabilities
	_ = json.Unmarshal(hello["agentCapabilities"], &caps)
	if caps.LoadSession || caps.SessionCapabilities.List == nil {
		t.Fatalf("capability intersection %s", hello["agentCapabilities"])
	}
	options := call("session/options", map[string]string{"cwd": "/work"})
	var choices []map[string]string
	if json.Unmarshal(options["configOptions"], &choices) != nil || len(choices) != 1 || choices[0]["id"] != "mode" {
		t.Fatalf("unrouted options include account-specific choices: %s", options)
	}
	var auth []map[string]string
	_ = json.Unmarshal(hello["authMethods"], &auth)
	if len(auth) != 2 || string(call("authenticate", map[string]string{"methodId": auth[1]["id"]})["account"]) != `"b"` {
		t.Fatal("account authentication")
	}
	_, err = acp.SendRequest[Object](client, ctx, "authenticate", map[string]string{"methodId": `["a","interactive"]`})
	var requestError *acp.RequestError
	if !errors.As(err, &requestError) || requestError.Code != -32602 {
		t.Fatalf("terminal authentication must not reach authenticate: %v", err)
	}
	var cursor json.RawMessage
	for _, name := range []string{"a", "b"} {
		params := Object{}
		if cursor != nil {
			params["cursor"] = cursor
		}
		page := call("session/list", params)
		cursor = page["nextCursor"]
		var rows []Object
		if json.Unmarshal(page["sessions"], &rows) != nil || len(rows) != 1 {
			t.Fatal(page)
		}
		for _, method := range []string{"session/resume", "session/close", "session/delete"} {
			r := call(method, Object{"sessionId": rows[0]["sessionId"]})
			if string(r["account"]) != string(encoded(name)) {
				t.Fatalf("%s routed to %s", method, r["account"])
			}
		}
	}
	if cursor != nil {
		t.Fatal("unexpected third page")
	}
}

func TestWirePreservesExtensionNotificationsAndElicitationScope(t *testing.T) {
	var transport bytes.Buffer
	w := &adapterWire{Writer: &transport}
	editorID := json.RawMessage(`"editor-prompt"`)
	frame := Object{"id": json.RawMessage("7"), "method": encoded(tagged("session/prompt", editorID))}
	line, _ := json.Marshal(frame)
	if _, err := w.Write(line); err != nil {
		t.Fatal(err)
	}
	var upstream Object
	if json.Unmarshal(transport.Bytes(), &upstream) != nil || string(upstream["method"]) != `"session/prompt"` {
		t.Fatalf("invalid upstream request %s", transport.Bytes())
	}
	wire := incoming(strings.NewReader("{\"method\":\"_auth/status_update\",\"params\":{}}\n{\"method\":\"elicitation/create\",\"id\":22,\"params\":{\"requestId\":7}}\n{\"id\":7,\"result\":{}}\n"), w)
	dec := json.NewDecoder(wire)
	var notification struct{ Method string }
	if dec.Decode(&notification) != nil || notification.Method != notificationPrefix+"_auth/status_update" {
		t.Fatal("extension notification lost its wire kind")
	}
	var elicitation struct{ Params map[string]json.RawMessage }
	if dec.Decode(&elicitation) != nil || string(elicitation.Params["requestId"]) != string(editorID) {
		t.Fatalf("wrong elicitation parent %+v", elicitation.Params)
	}
	var reply Object
	if dec.Decode(&reply) != nil {
		t.Fatal("missing final reply")
	}
	if _, ok := w.ids.Load("7"); ok {
		t.Fatal("completed parent request retained")
	}
}

// ACP-CLIENTCAP-006, ACP-PLAN-002/003/004. This adapter emits draft plans
// even when the editor did not announce support.
func TestPlanNotificationsRespectEditorCapability(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"omitted", "null", "advertised"} {
		t.Run(capability, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cfg := routing.Config{Model: "m", PinnedAccount: "a", Accounts: map[string]routing.Account{
				"a": {Models: map[string][]string{"m": {exe, "-test.run=^TestAccountAdapter$"}}, Env: map[string]string{"PETAL_NEGOTIATE_FIXTURE": "a"}},
			}}
			read, input := io.Pipe()
			out, write := io.Pipe()
			defer input.Close()
			defer write.Close()
			ConnectEmbedded(ctx, cfg, write, read)
			type notice struct {
				method string
				params json.RawMessage
			}
			updates := make(chan notice, 8)
			client := acp.NewConnection(func(_ context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
				updates <- notice{method, params}
				return nil, nil
			}, input, out)
			call := func(method string, params any) Object {
				t.Helper()
				r, err := acp.SendRequest[Object](client, ctx, method, params)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			caps := Object{}
			switch capability {
			case "null":
				caps["plan"] = json.RawMessage("null")
			case "advertised":
				caps["plan"] = json.RawMessage("{}")
			}
			call("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": caps})
			if err := client.SendNotification(ctx, "unknown/notice", Object{}); err != nil {
				t.Fatal(err)
			}
			session := call("session/new", Object{})
			call("session/prompt", Object{"sessionId": session["sessionId"]})
			if capability == "advertised" {
				for _, kind := range []string{"items", "markdown", "removed"} {
					select {
					case msg := <-updates:
						var got struct {
							SessionID json.RawMessage
							Update    struct {
								SessionUpdate, PlanID string
								Plan                  struct{ Type, PlanID, Path string }
							}
						}
						if err := json.Unmarshal(msg.params, &got); err != nil {
							t.Fatal(err)
						}
						if msg.method != "session/update" || string(got.SessionID) != string(session["sessionId"]) {
							t.Fatalf("unexpected notice %+v", msg)
						}
						if kind == "removed" {
							if got.Update.SessionUpdate != "plan_removed" || got.Update.PlanID != "p" {
								t.Fatalf("bad removal: %s", msg.params)
							}
						} else if got.Update.SessionUpdate != "plan_update" || got.Update.Plan.Type != kind ||
							got.Update.Plan.PlanID != "p" || (kind == "markdown" && got.Update.Plan.Path != "/work/PLAN.md") {
							t.Fatalf("bad plan: %s", msg.params)
						}
					default:
						t.Fatalf("missing %s update before prompt response", kind)
					}
				}
			}
			select {
			case msg := <-updates:
				t.Fatalf("unexpected notification: %s %s", msg.method, msg.params)
			default:
			}
		})
	}
}
