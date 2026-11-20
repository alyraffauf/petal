package proxy

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
)

func TestNativeV2Adapter(t *testing.T) {
	profile := os.Getenv("PETAL_V2_FIXTURE")
	if profile == "" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	send := func(method string, id json.RawMessage, params Object) {
		t.Helper()
		frame := Object{"jsonrpc": encoded("2.0"), "method": encoded(method), "params": jsonValue(params)}
		if id != nil {
			frame["id"] = id
		}
		if err := encoder.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	update := func(payload string) {
		send("session/update", nil, Object{"sessionId": encoded(""), "update": json.RawMessage(payload)})
	}
	reply := func(id json.RawMessage, result any) {
		t.Helper()
		if err := encoder.Encode(Object{"jsonrpc": encoded("2.0"), "id": id, "result": jsonValue(result)}); err != nil {
			t.Fatal(err)
		}
	}
	failure := func(id json.RawMessage, code int, data any) {
		t.Helper()
		if err := encoder.Encode(Object{"jsonrpc": encoded("2.0"), "id": id, "error": jsonValue(acp.NewRequestError(code, "fixture error", data))}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() Object {
		t.Helper()
		var frame Object
		if !scan.Scan() || json.Unmarshal(scan.Bytes(), &frame) != nil {
			t.Fatalf("read fixture frame: %s", scan.Bytes())
		}
		return frame
	}
	active := false
	for scan.Scan() {
		var frame Object
		if err := json.Unmarshal(scan.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		id, method, params := frame["id"], stringField(frame, "method"), frame.member("params")
		switch method {
		case "initialize":
			switch string(params["protocolVersion"]) {
			case "1":
				if profile != "v1" || params["info"] != nil || params["capabilities"] != nil {
					t.Fatal("invalid v1 handshake", params)
				}
				reply(id, Object{"protocolVersion": json.RawMessage("1"), "agentCapabilities": json.RawMessage("{}")})
			case "2":
				if profile == "v1" {
					failure(id, -32022, Object{"requested": json.RawMessage("2"), "supported": json.RawMessage("[1]")})
					continue
				}
				if !validImplementation(params["info"]) || params["clientInfo"] != nil || params["clientCapabilities"] != nil {
					t.Fatal("invalid v2 handshake", params)
				}
				capabilities := json.RawMessage(profile)
				if profile == "dynamic" {
					capabilities = params.member("_meta")["fixtureCapabilities"]
				}
				reply(id, Object{
					"protocolVersion": json.RawMessage("2"),
					"info":            json.RawMessage(`{"name":"fixture","version":"1"}`),
					"capabilities":    capabilities,
					"authMethods":     json.RawMessage(`[{"type":"agent","methodId":"","name":"Sign in"}]`),
				})
			default:
				t.Fatal("invalid version", params)
			}
		case "session/new":
			active = true
			reply(id, Object{"sessionId": encoded(""), "_meta": jsonValue(Object{"receivedMCP": params["mcpServers"]})})
		case "session/list":
			rows := json.RawMessage(`[{"sessionId":"","cwd":"/work"}]`)
			result := Object{"sessions": rows}
			if params["cursor"] == nil {
				result["nextCursor"] = encoded("")
			} else if !hasString(params, "cursor") || stringField(params, "cursor") != "" {
				t.Fatal("cursor changed", params)
			}
			reply(id, result)
		case "session/resume":
			active = true
			if params["replayFrom"] != nil {
				update(`{"sessionUpdate":"replay_complete"}`)
			}
			update(`{"sessionUpdate":"state_update","state":"running","background":[{"type":"subagent","sessionId":"child"}]}`)
			reply(id, Object{})
		case "session/cancel":
			update(`{"sessionUpdate":"state_update","state":"idle","stopReason":"cancelled"}`)
		case "session/close", "session/delete":
			active = false
			reply(id, Object{})
		case "session/prompt":
			if !hasString(params, "sessionId") || stringField(params, "sessionId") != "" {
				t.Fatal("native session ID changed", params)
			}
			if !active {
				failure(id, -32002, map[string]string{"sessionId": ""})
				continue
			}
			if stringField(params, "idempotencyKey") == "cancel" {
				send("session/request_permission", encoded("editor"), Object{
					"sessionId": encoded(""),
					"title":     encoded("Permission"),
					"subject":   json.RawMessage(`{"type":"tool_call","toolCall":{"toolCallId":"tool","content":[{"type":"session","sessionId":"child"}]}}`),
					"options":   json.RawMessage(`[{"optionId":"deny","name":"Deny","kind":"reject_once"}]`),
				})
				cancel := read()
				if stringField(cancel, "method") != "$/cancel_request" || string(cancel.member("params")["requestId"]) != string(id) {
					t.Fatalf("forward cancellation correlation: %v", cancel)
				}
				send("$/cancel_request", nil, Object{"requestId": encoded("editor")})
				response := read()
				if stringField(response, "id") != "editor" || stringField(response.member("result").member("outcome"), "outcome") != "cancelled" {
					t.Fatalf("permission cancellation outcome changed: %v", response)
				}
			}
			reply(id, Object{"messageId": encoded("accepted")})
		case "auth/login":
			if !hasString(params, "methodId") || stringField(params, "methodId") != "" {
				t.Fatal("native auth ID changed", params)
			}
			reply(id, Object{})
		case "auth/logout":
			reply(id, Object{})
		default:
			t.Fatal("unexpected backend method", method)
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
}

func jsonValue(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func nativeAccount(t testing.TB, profile string) routing.Account {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return routing.Account{Models: map[string][]string{"m": {exe, "-test.run=^TestNativeV2Adapter$"}}, Env: map[string]string{"PETAL_V2_FIXTURE": profile}}
}

type proxyWireTest struct {
	t       *testing.T
	scan    *bufio.Scanner
	encoder *json.Encoder
}

func connectWireTest(t *testing.T, config routing.Config) proxyWireTest {
	t.Helper()
	editor, proxy := net.Pipe()
	t.Cleanup(func() {
		_ = editor.Close()
		_ = proxy.Close()
	})
	if err := editor.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ConnectEmbedded(t.Context(), config, proxy, proxy, nil)
	return proxyWireTest{t, bufio.NewScanner(editor), json.NewEncoder(editor)}
}

func (w proxyWireTest) send(frame string) {
	w.t.Helper()
	if err := w.encoder.Encode(json.RawMessage(frame)); err != nil {
		w.t.Fatal(err)
	}
}

func (w proxyWireTest) read() Object {
	w.t.Helper()
	var frame Object
	if !w.scan.Scan() {
		w.t.Fatalf("read proxy frame: %v", w.scan.Err())
	}
	if err := json.Unmarshal(w.scan.Bytes(), &frame); err != nil {
		w.t.Fatal(err)
	}
	return frame
}

const v2Initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"editor","version":"1"}}}`

func TestNativeVersionSelectionAndFallback(t *testing.T) {
	config := routing.Config{ACPv2: true, Model: "m", Accounts: map[string]routing.Account{
		"a": nativeAccount(t, "v1"),
		"b": nativeAccount(t, `{"session":{"delete":{},"prompt":{"image":{}}}}`),
		"c": nativeAccount(t, `{"session":{"delete":null,"prompt":{"audio":{}}}}`),
	}}
	native := connectWireTest(t, config)
	native.send(v2Initialize)
	result := native.read().member("result")
	session := result.member("capabilities").member("session")
	if string(result["protocolVersion"]) != "2" || !validImplementation(result["info"]) || session == nil || session.member("delete") != nil || len(session.member("prompt")) != 0 {
		t.Fatalf("unexpected v2 result: %s", jsonValue(result))
	}

	config.PinnedAccount = "a"
	legacy := connectWireTest(t, config)
	legacy.send(v2Initialize)
	result = legacy.read().member("result")
	if string(result["protocolVersion"]) != "1" || result["info"] != nil || result["agentCapabilities"] == nil {
		t.Fatalf("unexpected fallback result: %s", jsonValue(result))
	}
	legacy.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1}}`)
	if result := legacy.read().member("result"); string(result["protocolVersion"]) != "1" {
		t.Fatalf("v1 retry failed: %s", jsonValue(result))
	}

	native.send(`{"jsonrpc":"2.0","id":2,"method":"session/list"}`)
	rows := native.read().member("result")
	var sessions []Object
	if json.Unmarshal(rows["sessions"], &sessions) != nil || len(sessions) != 1 || stringField(sessions[0], "sessionId") != `["b","m",""]` {
		t.Fatalf("v1 account leaked into v2 list: %s", rows["sessions"])
	}
	cursor := stringField(rows, "nextCursor")
	native.send(`{"jsonrpc":"2.0","id":3,"method":"session/list","params":{"cursor":` + string(encoded(cursor)) + `}}`)
	if cursor = stringField(native.read().member("result"), "nextCursor"); cursor != `["c",null]` {
		t.Fatalf("empty backend cursor did not round trip: %q", cursor)
	}
	native.send(`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":2}}`)
	if code := string(native.read().member("error")["code"]); code != "-32600" {
		t.Fatalf("late reinitialize error code = %s", code)
	}
}

func TestNativeV2SessionAndCancellation(t *testing.T) {
	wire := connectWireTest(t, routing.Config{
		ACPv2: true, Model: "m",
		Accounts:   map[string]routing.Account{"v2": nativeAccount(t, `{"session":{"mcp":{"stdio":{},"http":{}},"delete":{}}}`)},
		MCPServers: []routing.MCPServer{{Name: "local", Command: "/bin/tool"}, {Name: "remote", URL: "https://example.invalid/mcp"}},
	})
	wire.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":0}}`)
	if failure := wire.read().member("error"); string(failure["code"]) != "-32022" || string(failure.member("data")["supported"]) != "[1,2]" {
		t.Fatalf("unsupported version response: %s", jsonValue(failure))
	}
	wire.send(v2Initialize)
	hello := wire.read().member("result")
	if string(hello["protocolVersion"]) != "2" {
		t.Fatalf("initialize response: %s", jsonValue(hello))
	}
	var methods []Object
	if json.Unmarshal(hello["authMethods"], &methods) != nil || len(methods) != 1 || stringField(methods[0], "type") != "agent" {
		t.Fatalf("auth methods: %s", hello["authMethods"])
	}
	wire.send(`{"jsonrpc":"2.0","id":2,"method":"auth/login","params":{"methodId":` + string(methods[0]["methodId"]) + `}}`)
	if reply := wire.read(); reply["error"] != nil {
		t.Fatalf("login failed: %s", jsonValue(reply))
	}
	wire.send(`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/work"}}`)
	session := wire.read().member("result")
	public := string(session["sessionId"])
	var servers []Object
	if err := json.Unmarshal(session.member("_meta")["receivedMCP"], &servers); err != nil || len(servers) != 2 || stringField(servers[0], "type") != "stdio" || stringField(servers[1], "type") != "http" {
		t.Fatalf("v2 MCP definitions: %s", jsonValue(session))
	}

	wire.send(`{"jsonrpc":"2.0","id":"editor","method":"session/prompt","params":{"sessionId":` + public + `,"prompt":[{"type":"text","text":"test"}],"idempotencyKey":"cancel"}}`)
	permission := wire.read()
	if stringField(permission, "method") != "session/request_permission" || string(permission.member("params")["sessionId"]) != public {
		t.Fatalf("permission scope: %s", jsonValue(permission))
	}
	tool := permission.member("params").member("subject").member("toolCall")
	var content []Object
	if json.Unmarshal(tool["content"], &content) != nil || len(content) != 1 || stringField(content[0], "sessionId") != `["v2","m","child"]` {
		t.Fatalf("permission session reference: %s", jsonValue(tool))
	}
	wire.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"editor"}}`)
	cancel := wire.read()
	if stringField(cancel, "method") != "$/cancel_request" || string(cancel.member("params")["requestId"]) != string(permission["id"]) {
		t.Fatalf("reverse cancellation: %s", jsonValue(cancel))
	}
	wire.send(`{"jsonrpc":"2.0","id":` + string(permission["id"]) + `,"result":{"outcome":{"outcome":"cancelled"}}}`)
	prompt := wire.read()
	if stringField(prompt, "id") != "editor" || stringField(prompt.member("result"), "messageId") != "accepted" {
		t.Fatalf("prompt insertion acknowledgement lost on cancellation: %s", jsonValue(prompt))
	}

	wire.send(`{"jsonrpc":"2.0","id":4,"method":"session/close","params":{"sessionId":` + public + `}}`)
	if reply := wire.read(); reply["error"] != nil {
		t.Fatalf("close failed: %s", jsonValue(reply))
	}
	wire.send(`{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":` + public + `,"prompt":[]}}`)
	if failure := wire.read().member("error"); string(failure["code"]) != "-32002" || string(failure.member("data")["sessionId"]) != public {
		t.Fatalf("missing session response: %s", jsonValue(failure))
	}
	wire.send(`{"jsonrpc":"2.0","id":6,"method":"session/resume","params":{"sessionId":` + public + `,"cwd":"/work","replayFrom":{"type":"start"}}}`)
	if update := wire.read().member("params").member("update"); stringField(update, "sessionUpdate") != "replay_complete" {
		t.Fatalf("replay marker: %s", jsonValue(update))
	}
	state := wire.read().member("params").member("update")
	var background []Object
	if json.Unmarshal(state["background"], &background) != nil || len(background) != 1 || stringField(background[0], "sessionId") != `["v2","m","child"]` {
		t.Fatalf("state session reference: %s", jsonValue(state))
	}
	if reply := wire.read(); string(reply["id"]) != "6" || reply["error"] != nil {
		t.Fatalf("resume response ordering: %s", jsonValue(reply))
	}
}

func TestNativeV2ReinitializeReplacesCapabilities(t *testing.T) {
	wire := connectWireTest(t, routing.Config{ACPv2: true, Model: "m", Accounts: map[string]routing.Account{
		"native": nativeAccount(t, "dynamic"),
	}})
	wire.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"editor","version":"1"},"_meta":{"fixtureCapabilities":{"session":{}}}}}`)
	if caps := wire.read().member("result").member("capabilities"); caps.member("session") == nil {
		t.Fatalf("initial session capability missing: %s", jsonValue(caps))
	}
	wire.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":2}}`)
	if code := string(wire.read().member("error")["code"]); code != "-32602" {
		t.Fatalf("invalid repeat error code = %s", code)
	}
	wire.send(`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"editor","version":"1"},"_meta":{"fixtureCapabilities":{}}}}`)
	if caps := wire.read().member("result").member("capabilities"); caps == nil || caps.member("session") != nil {
		t.Fatalf("unexpected replacement capabilities: %s", jsonValue(caps))
	}
	wire.send(`{"jsonrpc":"2.0","id":4,"method":"session/list"}`)
	if failure := wire.read().member("error"); string(failure["code"]) != "-32601" {
		t.Fatalf("session/list should be unavailable: %s", jsonValue(failure))
	}
}
