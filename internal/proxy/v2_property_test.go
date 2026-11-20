package proxy

import (
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"petal/internal/routing"
)

type wireOperation byte

const (
	openSession wireOperation = iota
	resumeSession
	closeSession
	deleteSession
	promptSession
	listSessions
	promptForeignSession
)

// The fixture exposes one durable native session per account. The model tracks
// activity per public route, independent of the proxy's scoped ID and cursor
// encodings. The generated operations always observe their reply, and resumes
// also observe the update sent before the reply.
func FuzzNativeProxyHistory(f *testing.F) {
	f.Add([]byte{0, 1, 2}, []byte{0, 4, 2, 4, 1, 4, 6, 5})
	f.Add([]byte{2, 2}, []byte{1, 3, 4, 2, 0, 4, 5, 5})
	f.Add([]byte{3, 1}, []byte{0, 5, 6})
	f.Add([]byte{0, 0}, []byte{0, 4, 5})
	f.Fuzz(func(t *testing.T, profiles, history []byte) {
		if len(profiles) == 0 || len(profiles) > 4 || len(history) > 24 {
			return
		}
		accounts := map[string]routing.Account{}
		var eligible []string
		hasSessions, hasDelete := true, true
		for i, profile := range profiles {
			name := "account " + strconv.Itoa(i)
			var hello string
			switch profile % 4 {
			case 0:
				hello = "v1"
			case 1:
				hello = `{"session":{}}`
				hasDelete = false
			case 2:
				hello = `{"session":{"delete":{},"prompt":{"image":{}}}}`
			case 3:
				hello = `{"auth":{}}`
				hasSessions, hasDelete = false, false
			}
			if hello != "v1" {
				eligible = append(eligible, name)
			}
			accounts[name] = nativeAccount(t, hello)
		}
		wire := connectWireTest(t, routing.Config{ACPv2: true, Model: "m", Accounts: accounts})
		wire.send(v2Initialize)
		init := wire.read().member("result")
		version := string(init["protocolVersion"])
		if len(eligible) == 0 {
			if version != "1" {
				t.Fatalf("all-legacy routes negotiated %q: profiles %v", version, profiles)
			}
			return
		}
		if version != "2" {
			t.Fatalf("native routes negotiated %q: profiles %v", version, profiles)
		}
		caps := init.member("capabilities").member("session")
		if (caps != nil) != hasSessions || (caps.member("delete") != nil) != hasDelete {
			t.Fatalf("capability mismatch: profiles %v result %s", profiles, jsonValue(caps))
		}

		active := map[string]bool{}
		currentCursor := json.RawMessage(nil)
		page := 0
		for index, value := range history {
			op := wireOperation(value % 7)
			target := eligible[int(value/7)%len(eligible)]
			method := [...]string{"session/new", "session/resume", "session/close", "session/delete", "session/prompt", "session/list", "session/prompt"}[op]
			if op == promptForeignSession {
				target = "unavailable account"
			}
			session := string(jsonValue([]string{target, "m", ""}))
			params := Object{"sessionId": encoded(session)}
			switch op {
			case openSession:
				params = Object{"cwd": encoded("/work")}
			case resumeSession:
				params["cwd"] = encoded("/work")
			case listSessions:
				params = Object{}
				if currentCursor != nil {
					params["cursor"] = currentCursor
				}
			case promptSession, promptForeignSession:
				params["prompt"] = json.RawMessage("[]")
			}
			id := strconv.Itoa(index + 2)
			wire.send(string(jsonValue(Object{"jsonrpc": encoded("2.0"), "id": json.RawMessage(id), "method": encoded(method), "params": jsonValue(params)})))
			if hasSessions && op == resumeSession {
				frame := wire.read()
				if stringField(frame, "method") != "session/update" || stringField(frame.member("params"), "sessionId") != session {
					t.Fatalf("resume event for %q: %s", session, jsonValue(frame))
				}
			}
			reply := wire.read()
			if string(reply["id"]) != id {
				t.Fatalf("reply correlation for %s: %s", id, jsonValue(reply))
			}
			wantCode := ""
			switch {
			case !hasSessions || (op == deleteSession && !hasDelete):
				wantCode = "-32601"
			case (op == promptSession || op == promptForeignSession) && !active[target]:
				wantCode = "-32002"
			}
			if failure := reply.member("error"); string(failure["code"]) != wantCode ||
				(wantCode == "-32002" && stringField(failure.member("data"), "sessionId") != session) {
				t.Fatalf("model divergence at %d (%s, %s) with profiles %v and history %v: want error %q, got %s", index, method, target, profiles, history, wantCode, jsonValue(reply))
			}
			if wantCode != "" {
				continue
			}
			result := reply.member("result")
			switch op {
			case openSession:
				var route []string
				if json.Unmarshal([]byte(stringField(result, "sessionId")), &route) != nil || len(route) != 3 ||
					!slices.Contains(eligible, route[0]) || route[1] != "m" || route[2] != "" {
					t.Fatalf("opened on an ineligible route: %s", jsonValue(result))
				}
				active[route[0]] = true
			case resumeSession:
				active[target] = true
			case closeSession, deleteSession:
				active[target] = false
			case listSessions:
				var rows []Object
				route := []string{eligible[page/2], "m", ""}
				expected := string(jsonValue(route))
				if json.Unmarshal(result["sessions"], &rows) != nil || len(rows) != 1 || stringField(rows[0], "sessionId") != expected {
					t.Fatalf("list page %d: want %q, got %s", page, expected, jsonValue(result))
				}
				currentCursor = result["nextCursor"]
				page++
				if page == 2*len(eligible) {
					if currentCursor != nil {
						t.Fatalf("last page has cursor %s", currentCursor)
					}
					page = 0
				} else if currentCursor == nil {
					t.Fatalf("page %d ended early", page)
				}
			}
		}
	})
}

func FuzzNativeIDAndCursor(f *testing.F) {
	f.Add("account", "m", "", uint8(0))
	f.Add("account 🐾", `"quoted"`, "cursor\n", uint8(1))
	f.Add("a", "b", "c", uint8(2))
	f.Add("a", "b", "c", uint8(3))
	f.Fuzz(func(t *testing.T, name, model, native string, variation uint8) {
		if len(name)+len(model)+len(native) > 1024 {
			return
		}
		// JSON defines string values as Unicode, so normalize arbitrary fuzz
		// bytes by round-tripping through Go's encoder first.
		for _, value := range []*string{&name, &model, &native} {
			_ = json.Unmarshal(encoded(*value), value)
		}
		name = "a" + name
		p := proxy{Config: routing.Config{Model: model}}
		field := []any{name, model, native}
		tuple := []any{name, native}
		switch variation % 4 {
		case 1:
			field[2], tuple[1] = nil, nil
		case 2:
			field, tuple = append(field, "trailing"), append(tuple, "trailing")
		case 3:
			field, tuple = field[:2], tuple[:1]
		}
		routeID := p.publicID(name, native)
		if variation%4 != 0 {
			routeID = string(jsonValue(field))
		}
		got := decodeRouteID(routeID, 3)
		if variation%4 == 0 {
			if !reflect.DeepEqual(got, []string{name, model, native}) {
				t.Fatalf("namespace round trip = %q", got)
			}
		} else if got != nil {
			t.Fatalf("invalid route accepted: %s", routeID)
		}
		wireCursor := jsonValue(string(jsonValue(tuple)))
		cursor, err := decodeSessionCursor(wireCursor, true)
		switch variation % 4 {
		case 0:
			if err != nil || cursor.account != name || cursor.native == nil || *cursor.native != native {
				t.Fatalf("cursor round trip = %+v, %v", cursor, err)
			}
		case 1:
			if err != nil || cursor.account != name || cursor.native != nil {
				t.Fatalf("account boundary = %+v, %v", cursor, err)
			}
		default:
			if err == nil {
				t.Fatalf("invalid cursor accepted: %s", wireCursor)
			}
		}
	})
}
