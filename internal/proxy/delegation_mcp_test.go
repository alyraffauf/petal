package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"petal/internal/delegation"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestDelegationAdapterFixture(t *testing.T) {
	role := os.Getenv("PETAL_DELEGATION_FIXTURE")
	if role == "" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	write := func(frame Object) {
		t.Helper()
		if err := out.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	update := func(payload string) {
		write(Object{"jsonrpc": encoded("2.0"), "method": encoded("session/update"), "params": jsonValue(Object{
			"sessionId": encoded("child"), "update": json.RawMessage(payload),
		})})
	}
	version, nextID := 0, 0
	rejectedWake := false
	rejectedTeardowns := make(map[string]bool)
	for scan.Scan() {
		var message Object
		if err := json.Unmarshal(scan.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		params := message.member("params")
		reply := func(result any) {
			write(Object{"jsonrpc": encoded("2.0"), "id": message["id"], "result": jsonValue(result)})
		}
		switch method := stringField(message, "method"); method {
		case "initialize":
			if err := json.Unmarshal(params["protocolVersion"], &version); err != nil {
				t.Fatal(err)
			}
			if version == 2 {
				reply(json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"1"},"capabilities":{"session":{"fork":{},"delete":{},"mcp":{"stdio":{}}}}}`))
			} else {
				reply(json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":false},"sessionCapabilities":{"close":{},"delete":{},"fork":{},"resume":{}}}}`))
			}
		case "session/new", "session/load", "session/resume", "session/fork":
			if role == "child" && method == "session/new" {
				if string(params["mcpServers"]) != "[]" || !filepath.IsAbs(stringField(params, "cwd")) {
					t.Fatalf("child inherited MCP servers or lost its cwd: %s", jsonValue(params))
				}
				reply(Object{"sessionId": encoded("child")})
				continue
			}
			if method == "session/new" || method == "session/fork" {
				nextID++
				reply(Object{"sessionId": encoded("parent-" + strconv.Itoa(nextID)), "fixtureMCP": params["mcpServers"]})
			} else {
				result := Object{"fixtureMCP": params["mcpServers"]}
				if version == 1 && method == "session/resume" {
					result["sessionId"] = params["sessionId"]
				}
				reply(result)
			}
		case "session/close", "session/delete":
			if role == "parent" && !rejectedTeardowns[method] {
				rejectedTeardowns[method] = true
				write(Object{"jsonrpc": encoded("2.0"), "id": message["id"], "error": json.RawMessage(`{"code":-32603,"message":"teardown rejected"}`)})
				continue
			}
			reply(Object{})
		case "session/prompt":
			if role == "parent" {
				native := stringField(params, "sessionId")
				var prompt []struct{ Text string }
				if err := json.Unmarshal(params["prompt"], &prompt); err != nil || native != "parent-1" || len(prompt) != 1 {
					t.Fatalf("invalid parent prompt %s", params["prompt"])
				}
				if prompt[0].Text != "finish independent work" {
					wantID, _ := json.Marshal(`["child","child","child"]`)
					if !strings.Contains(prompt[0].Text, string(wantID)) || !strings.Contains(prompt[0].Text, "petal_read") {
						t.Fatal("missing child completion context")
					}
					if version == 1 && !rejectedWake {
						rejectedWake = true
						write(Object{"jsonrpc": encoded("2.0"), "id": message["id"], "error": json.RawMessage(`{"code":-32603,"message":"wake rejected"}`)})
						continue
					}
					write(Object{"jsonrpc": encoded("2.0"), "method": encoded("session/update"), "params": jsonValue(Object{"sessionId": encoded(native), "update": json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"response","content":{"type":"text","text":"parent resumed"}}`)})})
				}
				if version == 2 {
					reply(json.RawMessage(`{"messageId":"inserted"}`))
					write(Object{"jsonrpc": encoded("2.0"), "method": encoded("session/update"), "params": jsonValue(Object{"sessionId": encoded(native), "update": json.RawMessage(`{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`)})})
				} else {
					reply(json.RawMessage(`{"stopReason":"end_turn"}`))
				}
				continue
			}
			if stringField(params, "sessionId") != "child" {
				t.Fatalf("wrong child prompt target: %s", jsonValue(params))
			}
			var prompt []Object
			if err := json.Unmarshal(params["prompt"], &prompt); err != nil || len(prompt) != 1 {
				t.Fatalf("invalid prompt: %s", params["prompt"])
			}
			if version == 2 {
				update(`{"sessionUpdate":"state_update","state":"running"}`)
			}
			update(fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","messageId":"reply","content":{"type":"text","text":%q}}`, "receipt: "+stringField(prompt[0], "text")))
			if version == 2 {
				update(`{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`)
				reply(json.RawMessage(`{"messageId":"accepted"}`))
			} else {
				reply(json.RawMessage(`{"stopReason":"end_turn"}`))
			}
		default:
			t.Fatalf("unexpected method %s", method)
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestDelegationBridgeThroughACP(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			config := routing.Config{ACPv2: version == 2, Model: "parent", Accounts: map[string]routing.Account{}}
			for _, model := range []string{"parent", "child"} {
				config.Accounts[model] = routing.Account{Models: map[string][]string{model: {exe, "-test.run=^TestDelegationAdapterFixture$"}},
					Env: map[string]string{"PETAL_DELEGATION_FIXTURE": model}}
			}
			init := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%d,"info":{"name":"editor","version":"1"}}}`, version)
			cwd := t.TempDir()
			request := func(w proxyWireTest, method, id string) Object {
				t.Helper()
				w.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":%q,"params":{"cwd":%q,"sessionId":%q,"mcpServers":[]}}`, method, cwd, id))
				frame := w.read()
				if result := frame.member("result"); result != nil {
					return result
				}
				t.Fatalf("session setup failed: %s", jsonValue(frame))
				return nil
			}
			disabled := connectWireTest(t, config)
			disabled.send(init)
			disabled.read()
			if result := request(disabled, "session/new", ""); string(result["fixtureMCP"]) != "[]" {
				t.Fatalf("disabled delegation injected a server: %s", result["fixtureMCP"])
			}

			config.Delegation = true
			editor := connectWireTest(t, config)
			editor.send(init)
			editor.read()
			first := request(editor, "session/new", "")
			parent := stringField(first, "sessionId")
			second := request(editor, "session/fork", parent)
			open := func(result Object) proxyWireTest {
				t.Helper()
				var servers []Object
				if err := json.Unmarshal(result["fixtureMCP"], &servers); err != nil || len(servers) != 1 {
					t.Fatalf("expected just the Petal MCP server: %s", result["fixtureMCP"])
				}
				var env []routing.MCPEnvVariable
				if json.Unmarshal(servers[0]["env"], &env) != nil || len(env) != 1 || env[0].Name != delegationTokenEnv || env[0].Value == "" {
					t.Fatal("missing bridge credential")
				}
				server := servers[0]
				if stringField(server, "name") != delegationServerName || !filepath.IsAbs(stringField(server, "command")) || (version == 2 && stringField(server, "type") != "stdio") {
					t.Fatalf("invalid stdio definition: %s", jsonValue(server))
				}
				var args []string
				if json.Unmarshal(server["args"], &args) != nil || len(args) != 2 || args[0] != "delegation-mcp" {
					t.Fatalf("invalid relay command: %s", server["args"])
				}
				client, relay := net.Pipe()
				t.Cleanup(func() { _ = client.Close(); _ = relay.Close() })
				if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				go func() {
					defer relay.Close()
					_ = relayDelegationMCP(t.Context(), args[1], env[0].Value, relay, relay)
				}()
				wire := proxyWireTest{t, bufio.NewScanner(client), json.NewEncoder(client)}
				wire.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
				if got := stringField(wire.read().member("result"), "protocolVersion"); got != "2024-11-05" {
					t.Fatalf("MCP version = %q", got)
				}
				wire.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
				return wire
			}
			call := func(w proxyWireTest, name, args string, wantError bool) string {
				t.Helper()
				w.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args))
				frame := w.read()
				var result mcpToolResult
				if json.Unmarshal(frame["result"], &result) != nil || len(result.Content) != 1 || result.IsError != wantError {
					t.Fatalf("%s: unexpected MCP response %s", name, jsonValue(frame))
				}
				return result.Content[0].Text
			}
			firstMCP := open(first)
			secondMCP := open(second)
			catalog := call(firstMCP, "petal_capabilities", `{"search":"child"}`, false)
			var found struct{ Models []delegationTarget }
			if json.Unmarshal([]byte(catalog), &found) != nil || len(found.Models) != 1 || found.Models[0].ID != "child" {
				t.Fatalf("unexpected targets %s", catalog)
			}
			call(firstMCP, "petal_delegate", `{"model":"child","task":"echo","cwd":"/","parent_id":"forged"}`, true)
			child := call(firstMCP, "petal_delegate", `{"model":"child","task":"echo"}`, false)
			var handle struct{ ChildID, Next string }
			if json.Unmarshal([]byte(child), &handle) != nil || !strings.Contains(handle.Next, "petal_wait") || handle.ChildID != `["child","child","child"]` {
				t.Fatalf("unexpected routed child: %s", child)
			}
			args := fmt.Sprintf(`{"child_id":%q}`, handle.ChildID)
			call(secondMCP, "petal_read", args, true)
			promptParent := func(id int) {
				t.Helper()
				editor.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"finish independent work"}]}}`, id, parent))
			}
			promptParent(3)
			if version == 1 {
				frame := editor.read()
				if string(frame["id"]) != "3" || stringField(frame.member("result"), "stopReason") != "end_turn" {
					t.Fatalf("rejected wake replaced successful parent reply: %s", jsonValue(frame))
				}
				promptParent(4)
			}
			// Neither parent calls read/wait before the continuation. Observe its
			// response through the same editor connection as the original turn.
			for {
				frame := editor.read()
				if frame["error"] != nil {
					t.Fatalf("parent turn failed: %s", frame["error"])
				}
				params := frame.member("params")
				if stringField(params.member("update").member("content"), "text") == "parent resumed" {
					if stringField(params, "sessionId") != parent {
						t.Fatal("woke wrong parent")
					}
					break
				}
			}
			if page := call(firstMCP, "petal_read", args, false); !strings.Contains(page, "receipt: echo") {
				t.Fatalf("child output missing: %s", page)
			}
			for _, teardown := range []struct {
				method, parent string
				mcp            proxyWireTest
			}{
				{"session/close", parent, firstMCP},
				{"session/delete", stringField(second, "sessionId"), secondMCP},
			} {
				waitReply := func(id string) Object {
					t.Helper()
					for {
						frame := editor.read()
						if string(frame["id"]) == id {
							return frame
						}
					}
				}
				editor.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":%q,"params":{"sessionId":%q}}`, teardown.method, teardown.parent))
				if frame := waitReply("5"); string(frame.member("error")["code"]) != "-32603" {
					t.Fatalf("%s did not relay backend rejection: %s", teardown.method, jsonValue(frame))
				}
				call(teardown.mcp, "petal_capabilities", `{}`, false)
				editor.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":6,"method":%q,"params":{"sessionId":%q}}`, teardown.method, teardown.parent))
				if frame := waitReply("6"); frame["result"] == nil {
					t.Fatalf("%s failed on retry: %s", teardown.method, jsonValue(frame))
				}
				if teardown.mcp.scan.Scan() || teardown.mcp.scan.Err() != nil {
					t.Fatalf("%s did not close the MCP connection: %q, %v", teardown.method, teardown.mcp.scan.Text(), teardown.mcp.scan.Err())
				}
			}
			if version == 1 {
				config.Accounts = maps.Clone(config.Accounts)
				config.Accounts["pinned-parent"] = config.Accounts["parent"]
				config.PinnedAccount = "pinned-parent"
			}
			newEditor := connectWireTest(t, config)
			newEditor.send(init)
			newEditor.read()
			// A different JSON spelling must bind the same saved parent.
			spelledParent := `[ "parent", "parent", "parent-1" ]`
			resumed := request(newEditor, "session/resume", spelledParent)
			if version == 1 && stringField(resumed, "sessionId") != spelledParent {
				t.Errorf("pinned resume changed the editor session ID: %s", resumed["sessionId"])
			}
			reloaded := open(resumed)
			if page := call(reloaded, "petal_read", args, false); !strings.Contains(page, "receipt: echo") {
				t.Errorf("resumed parent did not recover its child: %s", page)
			}
			if version == 1 {
				newEditor.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"finish independent work"}]}}`, parent))
				if frame := newEditor.read(); frame["error"] != nil {
					t.Errorf("pinned resume rejected equivalent routed spelling: %s", frame["error"])
				}
				newEditor.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"session/resume","params":{"cwd":%q,"sessionId":%q,"mcpServers":[]}}`, t.TempDir(), parent))
				if frame := newEditor.read(); frame["error"] == nil {
					t.Error("pinned resume allowed an already bound parent to change project directories")
				}
				config.Accounts = maps.Clone(config.Accounts)
				config.Accounts["pinned-child"] = config.Accounts["child"]
				config.PinnedAccount = "pinned-child"
			}
			config.Model = "child"
			childEditor := connectWireTest(t, config)
			childEditor.send(init)
			childEditor.read()
			if result := request(childEditor, "session/resume", handle.ChildID); string(result["fixtureMCP"]) != "[]" {
				t.Fatalf("resumed child received delegation tools: %s", result["fixtureMCP"])
			}
		})
	}
}

func TestDelegationSocketRejectsMissingCredential(t *testing.T) {
	endpoint, err := newDelegationEndpoint(t.Context(), t.TempDir(), delegationTools{})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.cancel()
	conn, err := net.Dial("unix", endpoint.server.Args[1])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, strings.Repeat("x", len(endpoint.server.Env[0].Value))); err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err != io.EOF {
		t.Fatalf("unauthorized socket read: %v", err)
	}
}

func TestDelegationResultRemainsUnreadWhenRelayOutputFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := t.Context()
	cwd := t.TempDir()
	binding := sessions.ChildBinding{ParentID: "parent", ChildID: "child"}
	for _, id := range []string{binding.ParentID, binding.ChildID} {
		if _, err := sessions.Create(cwd, sessions.Record{ID: id, Model: "model"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sessions.BindChild(ctx, cwd, binding); err != nil {
		t.Fatal(err)
	}
	runtime := delegation.New(ctx, nil, nil)
	endpoint, err := newDelegationEndpoint(ctx, cwd, delegationTools{read: runtime.Read, wait: runtime.Wait})
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.cancel()
	endpoint.parent = binding.ParentID
	close(endpoint.ready)

	start := func(failDelivery bool) (proxyWireTest, <-chan error) {
		t.Helper()
		client, relay := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			defer relay.Close()
			output := terminalResultWriter{relay, failDelivery}
			done <- relayDelegationMCP(ctx, endpoint.server.Args[1], endpoint.server.Env[0].Value, relay, output)
		}()
		wire := proxyWireTest{t, bufio.NewScanner(client), json.NewEncoder(client)}
		wire.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
		if wire.read()["result"] == nil {
			t.Fatal("bridge did not initialize")
		}
		return wire, done
	}
	read := func(wire proxyWireTest, tool string) {
		t.Helper()
		wire.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":{"child_id":"child"}}}`, tool))
	}
	unread := func() bool {
		t.Helper()
		records, err := sessions.List(cwd, []string{"model"})
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range records {
			if record.ID == binding.ChildID {
				return record.UnreadChildResult
			}
		}
		t.Fatal("child missing from project history")
		return false
	}

	interrupted, failed := start(true)
	read(interrupted, "petal_read")
	if response := interrupted.read(); !strings.Contains(string(response["result"]), "running") {
		t.Fatalf("expected running child, got %s", jsonValue(response))
	}
	if err := sessions.FinishChild(ctx, cwd, binding, sessions.ChildResult{Status: "completed", Data: json.RawMessage(`{"answer":"test receipt"}`)}); err != nil {
		t.Fatal(err)
	}
	read(interrupted, "petal_wait")
	select {
	case err := <-failed:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("expected stdout failure, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not report the interrupted write")
	}
	if !unread() {
		t.Fatal("interrupted relay write consumed the result")
	}

	endpoint.wakes.mu.Lock()
	endpoint.wakes.children = map[string]bool{binding.ChildID: true}
	endpoint.wakes.signal()
	changed := endpoint.wakes.changed
	endpoint.wakes.mu.Unlock()
	// Drive the relay's private socket so cancellation and acknowledgment arrive
	// in a deterministic order after the terminal response.
	conn, err := net.Dial("unix", endpoint.server.Args[1])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, endpoint.server.Env[0].Value); err != nil {
		t.Fatal(err)
	}
	wire := proxyWireTest{t, bufio.NewScanner(conn), json.NewEncoder(conn)}
	wire.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	wire.read()
	read(wire, "petal_wait")
	frame := wire.read()
	if !strings.Contains(string(frame["response"]), "test receipt") || frame["delivery"] == nil {
		t.Fatalf("missing terminal delivery: %s", jsonValue(frame))
	}
	databases, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "petal", "conversations", "*", "index.sqlite"))
	if err != nil || len(databases) != 1 {
		t.Fatalf("locating project database: %v, %v", databases, err)
	}
	lock, err := os.Open(filepath.Dir(databases[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	wire.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	wire.send(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s}`, deliveryAckMethod, frame["delivery"]))
	wire.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if result := wire.read(); !strings.Contains(string(result["response"]), `"id":3`) {
		t.Fatalf("missing ping response while the store is locked: %s", jsonValue(result))
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("successfully delivered result did not clear the pending wake")
	}
	if unread() {
		t.Fatal("successfully delivered result stayed unread")
	}
}

type terminalResultWriter struct {
	io.Writer
	failDelivery bool
}

func (w terminalResultWriter) Write(data []byte) (int, error) {
	if w.failDelivery && strings.Contains(string(data), "test receipt") {
		return 0, io.ErrClosedPipe
	}
	return w.Writer.Write(data)
}
