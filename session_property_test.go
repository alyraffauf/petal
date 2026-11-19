package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// The child speaks stdio ACP. Its native session ID is deliberately the same
// for both profiles: only Petal can tell the accounts apart on resume.
func TestPropertyACPAdapter(t *testing.T) {
	account := os.Getenv("PETAL_TEST_ADAPTER_PROFILE")
	if account == "" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	output := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage
			Method string
			Params struct{ SessionID string }
		}
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			t.Fatal(err)
		}
		if len(req.ID) == 0 {
			continue
		}
		result := map[string]any{}
		reply := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		switch req.Method {
		case "initialize":
			result["protocolVersion"] = 1
			result["agentCapabilities"] = map[string]bool{"loadSession": true}
		case "session/new":
			result["sessionId"] = "shared-native-id"
		case "session/load", "test/whichProfile":
			if req.Params.SessionID != "shared-native-id" {
				t.Fatalf("invalid native session ID %q", req.Params.SessionID)
			}
			result["profile"] = account
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
		if err := output.Encode(reply); err != nil {
			t.Fatal(err)
		}
	}
}

func FuzzSessionResumePlacement(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 0, 3})
	f.Add([]byte{2, 5, 0, 4, 2, 5})
	f.Add([]byte{0, 7, 2, 8, 11})
	f.Fuzz(func(t *testing.T, trace []byte) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		projects := [2]string{t.TempDir(), t.TempDir()}
		names := [2]string{"personal", "work"}
		models := [2]string{"shared-model", "other-model"}
		c := config{Accounts: map[string]account{}}
		for _, name := range names {
			c.Accounts[name] = account{
				Models: map[string][]string{
					models[0]: {exe, "-test.run=^TestPropertyACPAdapter$"},
					models[1]: {exe, "-test.run=^TestPropertyACPAdapter$"},
				},
				Env: map[string]string{"PETAL_TEST_ADAPTER_PROFILE": name},
			}
		}
		// Each operation opens a fresh client in one of two projects.
		// The oracle remembers only the most recently created account per (project, model).
		var saved [2][2]string
		for step, op := range trace[:min(len(trace), 8)] {
			project := int(op / 3 % 2)
			model := int(op / 6 % 2)
			c.model = models[model]
			action := int(op % 3)
			resume := action == 2
			c.forced = ""
			want := saved[project][model]
			if !resume {
				want = names[action]
				c.forced = want
			}
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				h := startHarness(ctx, c)
				defer h.close()
				account, err := h.open(ctx, c, projects[project], resume)
				if want == "" {
					if !os.IsNotExist(err) {
						t.Fatalf("step %d: missing history, got %v", step, err)
					}
					return
				}
				if err != nil || account != want {
					t.Fatalf("step %d: open = %q, %v; want %q", step, account, err, want)
				}
				got, err := acp.SendRequest[struct{ Profile string }](h.conn, ctx, "test/whichProfile", map[string]string{"sessionId": h.id})
				if err != nil || got.Profile != want {
					t.Fatalf("step %d: routed to %q, %v; want %q", step, got.Profile, err, want)
				}
				saved[project][model] = want
			}()
		}
	})
}
