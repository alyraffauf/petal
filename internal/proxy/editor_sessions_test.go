package proxy

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"petal/internal/routing"
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
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	launch := func() func(string, Object) Object {
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
		return call
	}
	call := launch()
	ids := map[string]json.RawMessage{}
	for _, model := range []string{"codex/default", "claude/default"} {
		session := call("session/new", Object{"cwd": encoded(cwd), "mcpServers": json.RawMessage("[]")})
		var opts []struct{ Options []struct{ Value string } }
		if err := json.Unmarshal(session["configOptions"], &opts); err != nil || len(opts) != 1 || len(opts[0].Options) != 2 || opts[0].Options[0].Value != "claude/default" || opts[0].Options[1].Value != "codex/default" {
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
	reopen := launch()
	for model, account := range map[string]string{"claude/default": "a", "codex/default": "b"} {
		result := reopen("session/resume", Object{"cwd": encoded(cwd), "sessionId": ids[model], "mcpServers": json.RawMessage("[]")})
		if stringField(result, "account") != account {
			t.Fatalf("%s resumed on %s", model, result["account"])
		}
	}
}
