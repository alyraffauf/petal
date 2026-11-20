package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"petal/internal/routing"
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
				reply(json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"1"},"capabilities":{"session":{"mcp":{"stdio":{}}}}}`))
			} else {
				reply(json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":false},"sessionCapabilities":{"close":{}}}}`))
			}
		case "session/new", "session/load", "session/resume":
			if role == "child" && method == "session/new" {
				if string(params["mcpServers"]) != "[]" || !filepath.IsAbs(stringField(params, "cwd")) {
					t.Fatalf("child inherited MCP servers or lost its cwd: %s", jsonValue(params))
				}
				reply(Object{"sessionId": encoded("child")})
				continue
			}
			if method == "session/new" {
				nextID++
				reply(Object{"sessionId": encoded("parent-" + strconv.Itoa(nextID)), "fixtureMCP": params["mcpServers"]})
			} else {
				reply(Object{"fixtureMCP": params["mcpServers"]})
			}
		case "session/close":
			reply(Object{})
		case "session/prompt":
			if role != "child" || stringField(params, "sessionId") != "child" {
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
			second := request(editor, "session/new", "")
			parent := stringField(first, "sessionId")
			open := func(result Object) proxyWireTest {
				t.Helper()
				var servers []Object
				if err := json.Unmarshal(result["fixtureMCP"], &servers); err != nil || len(servers) != 1 {
					t.Fatalf("expected just the Petal MCP server: %s", result["fixtureMCP"])
				}
				if version == 1 && string(servers[0]["env"]) != "[]" {
					t.Fatalf("stdio MCP requires an env array: %s", servers[0]["env"])
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
					_ = RelayDelegationMCP(t.Context(), args[1], relay, relay)
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
			call(firstMCP, "petal_delegate", `{"model":"child","task":"echo","cwd":"/","parent_id":"forged"}`, true)
			child := call(firstMCP, "petal_delegate", `{"model":"child","task":"echo"}`, false)
			var handle struct{ ChildID string }
			if json.Unmarshal([]byte(child), &handle) != nil || handle.ChildID != `["child","child","child"]` {
				t.Fatalf("unexpected routed child: %s", child)
			}
			args := fmt.Sprintf(`{"child_id":%q}`, handle.ChildID)
			call(secondMCP, "petal_read", args, true)
			if page := call(firstMCP, "petal_wait", args, false); !strings.Contains(page, "receipt: echo") {
				t.Fatalf("child output missing: %s", page)
			}
			newEditor := connectWireTest(t, config)
			newEditor.send(init)
			newEditor.read()
			method := "session/load"
			if version == 2 {
				method = "session/resume"
				// A different spelling of the same routed ID must bind the same parent.
				parent = `[ "parent", "parent", "parent-1" ]`
			}
			reloaded := open(request(newEditor, method, parent))
			if page := call(reloaded, "petal_read", args, false); !strings.Contains(page, "receipt: echo") {
				t.Fatalf("resumed parent did not recover its child: %s", page)
			}
			config.Model = "child"
			childEditor := connectWireTest(t, config)
			childEditor.send(init)
			childEditor.read()
			if result := request(childEditor, method, handle.ChildID); string(result["fixtureMCP"]) != "[]" {
				t.Fatalf("resumed child received delegation tools: %s", result["fixtureMCP"])
			}
		})
	}
}
