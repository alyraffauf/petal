//go:build linux || darwin

package livesession

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"petal/internal/agentmcp"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestDelegationFollowsUpWithNoController(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx, dial := startSocket(t, version, true)
			cwd := t.TempDir()
			client := dial()
			setup := openSession(t, ctx, client, OpenOptions{Handle: NewHandle(), Cwd: cwd, Model: "fixture"}, version)
			observer := dial()
			if _, err := observer.Attach(ctx, AttachOptions{Handle: setup.Handle}); err != nil {
				t.Fatal(err)
			}
			submit(t, ctx, client, Call{Operation: 3, Method: "session/prompt", Params: json.RawMessage(`{"prompt":[{"type":"text","text":"delegate"}]}`)})
			cursor := setup.Head
			advance(t, ctx, observer, &cursor, func(event Event) bool {
				return event.Kind == Update && strings.Contains(string(event.Params), "before ")
			})
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			childID := delegateThroughMCP(t, ctx, setup.Setup)
			if err := os.WriteFile(filepath.Join(cwd, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			wake := advance(t, ctx, observer, &cursor, func(event Event) bool {
				return event.Kind == Update && strings.Contains(string(event.Params), "delegation-wake")
			})
			if !strings.Contains(string(wake.Params), "thread") {
				t.Fatalf("child result reached wrong session: %+v", wake)
			}
			if version == 2 {
				reconnected := dial()
				active := takeControl(t, ctx, reconnected, AttachOptions{Handle: setup.Handle, Control: true})
				if !active.Running || active.Turn == 0 || active.Turn == 3 || active.ActiveOperation != 0 {
					t.Fatalf("delegation wake lacks independent turn identity: %+v", active)
				}
				if err := reconnected.Cancel(ctx, 3); !hasCode(err, NotFound) {
					t.Fatalf("cancel by old prompt operation: %v", err)
				}
				if err := reconnected.Cancel(ctx, active.Turn); err != nil {
					t.Fatal(err)
				}
				advance(t, ctx, observer, &cursor, func(event Event) bool {
					return strings.Contains(string(event.Params), "wake-cancelled")
				})
			}
			if rows, err := sessions.Transcript(cwd, childID); err != nil || len(rows) == 0 || rows[len(rows)-1].Body != "child-work" {
				t.Fatalf("child archive = %+v, %v", rows, err)
			}
		})
	}
}

func delegateThroughMCP(t *testing.T, ctx context.Context, setup json.RawMessage) string {
	t.Helper()
	var definition struct {
		FixtureMCP []struct {
			Args []string
			Env  []routing.MCPEnvVariable
		}
	}
	if err := json.Unmarshal(setup, &definition); err != nil || len(definition.FixtureMCP) != 1 {
		t.Fatalf("setup did not install delegation: %v", err)
	}
	config := definition.FixtureMCP[0]
	if len(config.Args) != 2 || len(config.Env) != 1 || config.Env[0].Name != agentmcp.TokenEnv {
		t.Fatal("invalid delegation relay definition")
	}
	client, relay := net.Pipe()
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	go func() {
		defer relay.Close()
		_ = agentmcp.Relay(ctx, config.Args[1], config.Env[0].Value, relay, relay)
	}()
	out, in := json.NewEncoder(client), bufio.NewScanner(client)
	send := func(raw string) {
		t.Helper()
		if err := out.Encode(json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	read := func() json.RawMessage {
		t.Helper()
		var reply struct{ Result, Error json.RawMessage }
		if !in.Scan() || json.Unmarshal(in.Bytes(), &reply) != nil || reply.Error != nil || reply.Result == nil {
			t.Fatalf("delegation relay response failed: %s, %v", in.Bytes(), in.Err())
		}
		return reply.Result
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	read()
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delegate","arguments":{"model":"child","task":"child-work"}}}`)
	var result struct {
		Content []struct{ Text string }
		IsError bool
	}
	if err := json.Unmarshal(read(), &result); err != nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("delegate result = %+v, %v", result, err)
	}
	var child struct{ ChildID string }
	if err := json.Unmarshal([]byte(result.Content[0].Text), &child); err != nil || child.ChildID != `["child","child","child"]` {
		t.Fatalf("delegate ID = %q, %v", child.ChildID, err)
	}
	return child.ChildID
}
