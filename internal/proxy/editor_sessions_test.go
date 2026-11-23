package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestEditorPicksProviderAndResumesOnSameAccount(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := routing.Config{Accounts: map[string]routing.Account{}}
	for model, account := range map[string]string{"claude/default": "a", "codex/default": "b"} {
		config.Accounts[account] = routing.Account{Models: map[string][]string{model: {exe, "-test.run=^TestAccountAdapter$"}}, Env: map[string]string{"PETAL_NEGOTIATE_FIXTURE": account}}
	}
	config.Accounts["offline"] = routing.Account{Models: map[string][]string{"aaa/default": {"/missing/petal-test-adapter"}}}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var client *acp.Connection
	launch := func() func(string, Object) Object {
		editor, server := net.Pipe()
		t.Cleanup(func() { editor.Close(); server.Close() })
		connect(ctx, config, server, server, false, nil)
		client = acp.NewConnection(nil, editor, editor)
		call := func(method string, params Object) Object {
			t.Helper()
			r, err := acp.SendRequest[Object](client, ctx, method, params)
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			return r
		}
		call("initialize", Object{"protocolVersion": json.RawMessage("1")})
		return call
	}
	call := launch()
	ids := map[string]json.RawMessage{}
	for _, model := range []string{"codex/default", "claude/default"} {
		session := call("session/new", Object{"cwd": encoded(cwd), "mcpServers": json.RawMessage("[]")})
		var opts []struct{ Options []struct{ Value string } }
		if err := json.Unmarshal(session["configOptions"], &opts); err != nil || len(opts) != 1 || len(opts[0].Options) != 3 || opts[0].Options[1].Value != "claude/default" || opts[0].Options[2].Value != "codex/default" {
			t.Fatalf("editor picker: %s", session["configOptions"])
		}
		if model == "codex/default" {
			call("session/set_config_option", Object{"sessionId": session["sessionId"], "configId": encoded("model"), "value": encoded("claude/default")})
		}
		ids[model] = session["sessionId"]
		call("session/set_config_option", Object{"configId": encoded("model"), "value": encoded(model), "sessionId": session["sessionId"]})
	}
	query := Object{"cwd": encoded(cwd)}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		page := call("session/list", query)
		var rows []Object
		if json.Unmarshal(page["sessions"], &rows) != nil {
			t.Fatal("invalid session page")
		}
		for _, row := range rows {
			parts := decodeRouteID(stringField(row, "sessionId"), 3)
			if parts == nil || seen[parts[0]] {
				t.Fatal("unexpected history row", row)
			}
			seen[parts[0]] = true
		}
		if page["nextCursor"] == nil {
			break
		}
		query["cursor"] = page["nextCursor"]
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("missing account in history: %v", seen)
	}
	moved := cwd + "-moved"
	if err := os.Rename(cwd, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(moved) })
	cwd = moved
	reopen := launch()
	other := map[string]string{"claude/default": "codex/default", "codex/default": "claude/default"}
	for model, account := range map[string]string{"claude/default": "a", "codex/default": "b"} {
		result := reopen("session/resume", Object{"cwd": encoded(cwd), "sessionId": ids[model], "mcpServers": json.RawMessage("[]")})
		if stringField(result, "account") != account {
			t.Fatalf("%s resumed on %s", model, result["account"])
		}
		var opts []struct {
			ID, CurrentValue string
			Options          []struct{ Value string }
		}
		if err := json.Unmarshal(result["configOptions"], &opts); err != nil || len(opts) != 1 || opts[0].ID != "model" || opts[0].CurrentValue != model || len(opts[0].Options) != 3 {
			t.Fatalf("resumed %s offered %s, want Petal's routes", model, result["configOptions"])
		}
		// Petal has saved nothing of this conversation, so a switch would lose it.
		if _, err := acp.SendRequest[Object](client, ctx, "session/set_config_option", Object{"sessionId": ids[model], "configId": encoded("model"), "value": encoded(other[model])}); err == nil {
			t.Fatalf("switched %s with no saved history", model)
		}
	}
	// The fixture gives every session the same native ID, so prompts start only
	// after each conversation has been checked without history.
	for _, model := range []string{"claude/default", "codex/default"} {
		var opts []struct{ CurrentValue string }
		reopen("session/prompt", Object{"sessionId": ids[model], "prompt": json.RawMessage(`[{"type":"text","text":"hello"}]`)})
		switched := reopen("session/set_config_option", Object{"sessionId": ids[model], "configId": encoded("model"), "value": encoded(other[model])})
		if err := json.Unmarshal(switched["configOptions"], &opts); err != nil || len(opts) == 0 || opts[0].CurrentValue != other[model] {
			t.Fatalf("switching %s to %s offered %s", model, other[model], switched["configOptions"])
		}
		// The pending handoff survives a close and resume or a restart.
		path, err := editorBindingPath(projectKey(cwd), stringField(Object{"sessionId": ids[model]}, "sessionId"))
		if err != nil {
			t.Fatal(err)
		}
		binding, err := os.ReadFile(path)
		if _, handoff, _ := strings.Cut(string(binding), "\n"); err != nil || decodeRouteID(handoff, 3) == nil {
			t.Fatalf("binding after the switch = %q, %v; want the pending handoff on its second line", binding, err)
		}
		reopen("session/prompt", Object{"sessionId": ids[model], "prompt": json.RawMessage(`[{"type":"text","text":"continue"}]`)})
		if binding, err = os.ReadFile(path); err != nil || strings.Contains(string(binding), "\n") {
			t.Fatalf("binding after the prompt = %q, %v; want the handoff consumed", binding, err)
		}
	}
}

func TestExternalV2TurnIsSearchableAtIdle(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Setenv("PETAL_COMPACT_REPLAY", "1")
	launch := func() proxyWireTest {
		editor, server := net.Pipe()
		t.Cleanup(func() { editor.Close(); server.Close() })
		if err := editor.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		cfg := routing.Config{ACPv2: true, Model: "m", Accounts: map[string]routing.Account{"work": nativeAccount(t, `{"session":{}}`)}}
		connect(t.Context(), cfg, server, server, false, nil)
		wire := proxyWireTest{t, bufio.NewScanner(editor), json.NewEncoder(editor)}
		wire.send(v2Initialize)
		wire.read()
		return wire
	}
	wire := launch()
	wire.send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":` + string(encoded(cwd)) + `,"mcpServers":[]}}`)
	id := wire.read().member("result")["sessionId"]
	wire.send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":` + string(id) + `,"idempotencyKey":"transcript","prompt":[{"type":"text","text":"lookup"}]}}`)
	for {
		frame := wire.read()
		if stringField(frame.member("params").member("update"), "state") == "idle" {
			break
		}
	}
	hits, err := sessions.Search(cwd, "work", "needle", 0, 5)
	if err != nil || len(hits) != 1 || hits[0].Role != "assistant" {
		t.Fatalf("persisted hits %v: %v", hits, err)
	}
	rows, err := sessions.Transcript(cwd, hits[0].Session)
	if err != nil || len(rows) != 2 || rows[0].Body != "lookup" || rows[1].Body != "needle" {
		t.Fatalf("persisted transcript %v: %v", rows, err)
	}
	reopened := launch()
	for i := 0; i < 2; i++ {
		reopened.send(`{"jsonrpc":"2.0","id":4,"method":"session/resume","params":{"sessionId":` + string(id) + `,"cwd":` + string(encoded(cwd)) + `,"mcpServers":[],"replayFrom":{"type":"start"}}}`)
		for {
			frame := reopened.read()
			if string(frame["id"]) == "4" {
				if frame["error"] != nil {
					t.Fatal(string(frame["error"]))
				}
				break
			}
		}
	}
	archive, err := sessions.Transcript(cwd, hits[0].Session)
	if err != nil || len(archive) != 4 || archive[0] != rows[0] || archive[1] != rows[1] || archive[2].Role != "compaction" || archive[3].Role != "compaction" {
		t.Fatalf("archive after compacted replay: %+v, %v", archive, err)
	}

}

func TestSessionOpenedWithModelSelectionCanSwitch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := routing.Config{Accounts: map[string]routing.Account{}}
	for model, account := range map[string]string{"claude/default": "a", "codex/default": "b"} {
		config.Accounts[account] = routing.Account{Models: map[string][]string{model: {exe, "-test.run=^TestAccountAdapter$"}}, Env: map[string]string{"PETAL_NEGOTIATE_FIXTURE": account}}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	editor, server := net.Pipe()
	t.Cleanup(func() { editor.Close(); server.Close() })
	connect(ctx, config, server, server, false, nil)
	client := acp.NewConnection(nil, editor, editor)
	call := func(method string, params Object) Object {
		t.Helper()
		r, err := acp.SendRequest[Object](client, ctx, method, params)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return r
	}
	call("initialize", Object{"protocolVersion": json.RawMessage("1")})
	opened := call("session/new", Object{"cwd": encoded(t.TempDir()), "mcpServers": json.RawMessage("[]"), "configOptions": json.RawMessage(`[{"configId":"model","value":"codex/default"}]`)})
	switched := call("session/set_config_option", Object{"sessionId": opened["sessionId"], "configId": encoded("model"), "value": encoded("claude/default")})
	var opts []struct{ ID, CurrentValue string }
	if err := json.Unmarshal(switched["configOptions"], &opts); err != nil || len(opts) == 0 || opts[0].ID != "model" || opts[0].CurrentValue != "claude/default" {
		t.Fatalf("switch after selecting a model at open returned %v", switched)
	}
}

func TestForkWaitsForPendingHandoff(t *testing.T) {
	p := &proxy{editorSessions: &editorSessions{}}
	p.initialized = true
	s := &editorSession{route: p, routedID: `["a","m","same"]`, handoff: `["b","n","old"]`}
	p.editorSessions.sessions.Store("petal-x", s)
	_, err := p.handleEditor(t.Context(), "session/fork", json.RawMessage(`{"sessionId":"petal-x","cwd":"/tmp","mcpServers":[]}`))
	var re *acp.RequestError
	if !errors.As(err, &re) || !strings.Contains(fmt.Sprint(re.Data), "before forking") {
		t.Fatalf("fork returned %v; want it refused until the handoff is sent", err)
	}
}

func TestForkWithoutLineageRefusesModelSwitch(t *testing.T) {
	p := &proxy{editorSessions: &editorSessions{}, Config: routing.Config{Accounts: map[string]routing.Account{
		"a": {Models: map[string][]string{"m": {"adapter"}, "n": {"adapter"}}},
	}}}
	p.initialized = true
	p.Model = "m"
	p.editorSessions.sessions.Store(`["a","m","fork"]`, &editorSession{route: p, routedID: `["a","m","fork"]`, legacyFork: true})
	_, err := p.handleEditor(t.Context(), "session/set_config_option", json.RawMessage(`{"sessionId":"[\"a\",\"m\",\"fork\"]","configId":"model","value":"n"}`))
	var re *acp.RequestError
	if !errors.As(err, &re) || !strings.Contains(fmt.Sprint(re.Data), "forked from") {
		t.Fatalf("switching a fork with unknown lineage returned %v; want it refused", err)
	}
}

func TestForkLineageSurvivesResume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	p := &proxy{editorSessions: &editorSessions{}, Config: routing.Config{Accounts: map[string]routing.Account{"a": {Models: map[string][]string{"m": {"adapter"}}}}}}
	p.initialized = true
	p.Model = "m"
	lineage := []historySegment{{ID: `["a","m","parent"]`, Head: 7}}
	for id, saved := range map[string]*editorSession{
		`["a","m","fork"]`:   {cwd: projectKey(cwd), routedID: `["a","m","fork"]`, lineage: lineage},
		`["a","m","legacy"]`: {cwd: projectKey(cwd), routedID: `["a","m","legacy"]`, legacyFork: true},
	} {
		saved.saveBinding(id)
		params, _ := json.Marshal(Object{"sessionId": encoded(id), "cwd": encoded(cwd), "mcpServers": json.RawMessage("[]")})
		_, _ = p.handleEditor(t.Context(), "session/resume", params) // No adapter runs here; the binding is read first.
		value, ok := p.editorSessions.sessions.Load(id)
		if !ok {
			t.Fatalf("%s was not resumed", id)
		}
		got := value.(*editorSession)
		if !slices.Equal(got.lineage, saved.lineage) || got.legacyFork != saved.legacyFork {
			t.Errorf("%s resumed with lineage %v legacy %v, want %v %v", id, got.lineage, got.legacyFork, saved.lineage, saved.legacyFork)
		}
	}
}

func TestFailedHandoffPromptKeepsForkLineage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	p := &proxy{editorSessions: &editorSessions{}, Config: routing.Config{Accounts: map[string]routing.Account{"a": {Models: map[string][]string{"m": {"adapter"}}}}}}
	p.initialized = true
	p.Model = "m"
	lineage := []historySegment{{ID: `["a","m","parent"]`, Head: 3}}
	s := &editorSession{route: p, routedID: `["a","m","new"]`, cwd: projectKey(cwd), handoff: `["a","m","fork"]`, lineage: lineage, request: Object{"cwd": encoded(cwd)}}
	p.editorSessions.sessions.Store("petal-x", s)
	// No adapter runs here, so the prompt fails after taking the handoff.
	if _, err := p.handleEditor(t.Context(), "session/prompt", json.RawMessage(`{"sessionId":"petal-x","prompt":[{"type":"text","text":"hi"}]}`)); err == nil {
		t.Fatal("prompt unexpectedly succeeded without an adapter")
	}
	if s.handoff != `["a","m","fork"]` || !slices.Equal(s.lineage, lineage) {
		t.Fatalf("after a failed prompt, handoff %q lineage %v; want both restored", s.handoff, s.lineage)
	}
}

func TestForkOfUnarchivedConversationRefusesSwitch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := routing.Config{Accounts: map[string]routing.Account{}}
	for model, account := range map[string]string{"claude/default": "a", "codex/default": "b"} {
		config.Accounts[account] = routing.Account{Models: map[string][]string{model: {exe, "-test.run=^TestAccountAdapter$"}}, Env: map[string]string{"PETAL_NEGOTIATE_FIXTURE": account}}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	editor, server := net.Pipe()
	t.Cleanup(func() { editor.Close(); server.Close() })
	connect(ctx, config, server, server, false, nil)
	client := acp.NewConnection(nil, editor, editor)
	call := func(method string, params Object) (Object, error) {
		return acp.SendRequest[Object](client, ctx, method, params)
	}
	if _, err := call("initialize", Object{"protocolVersion": json.RawMessage("1")}); err != nil {
		t.Fatal(err)
	}
	open := Object{"cwd": encoded(cwd), "mcpServers": json.RawMessage("[]")}
	forkAndSwitch := func(parent json.RawMessage) error {
		t.Helper()
		forked, err := call("session/fork", Object{"sessionId": parent, "cwd": encoded(cwd), "mcpServers": json.RawMessage("[]")})
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		_, err = call("session/set_config_option", Object{"sessionId": forked["sessionId"], "configId": encoded("model"), "value": encoded("codex/default")})
		return err
	}

	// An imported conversation, resumed without replay, has history Petal never archived.
	imported := encoded(`["a","claude/default","same"]`)
	if _, err := call("session/resume", Object{"sessionId": imported, "cwd": encoded(cwd), "mcpServers": json.RawMessage("[]")}); err != nil {
		t.Fatal(err)
	}
	if err := forkAndSwitch(imported); err == nil {
		t.Fatal("a fork of a conversation Petal never archived switched models and dropped its history")
	}

	opened, err := call("session/new", open)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call("session/set_config_option", Object{"sessionId": opened["sessionId"], "configId": encoded("model"), "value": encoded("claude/default")}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("session/prompt", Object{"sessionId": opened["sessionId"], "prompt": json.RawMessage(`[{"type":"text","text":"before the fork"}]`)}); err != nil {
		t.Fatal(err)
	}
	if err := forkAndSwitch(opened["sessionId"]); err != nil {
		t.Fatalf("a fork of an archived conversation could not switch: %v", err)
	}
}
