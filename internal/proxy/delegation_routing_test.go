package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"petal/internal/delegation"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestDelegatedChildRetriesSetupOnEligibleAccounts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	broken := delegationRoutingAccount(exe, "echo 100", "")
	broken.Models["m"] = []string{"/nonexistent/petal-nope"}
	selection := delegationRoutingAccount(exe, "echo 80", "session/set_config_option")
	selection.SelectModel = map[string]bool{"m": true}
	routing.RecordRateLimit("cooled")
	p := &proxy{Config: routing.Config{Delegation: true, Model: "m", ACPv2: true, Accounts: map[string]routing.Account{
		"startup": broken, "initialize": delegationRoutingAccount(exe, "echo 95", "initialize"),
		"session": delegationRoutingAccount(exe, "echo 90", "session/new"), "select": selection,
		"cooled": delegationRoutingAccount(exe, "echo 100", ""), "exhausted": delegationRoutingAccount(exe, "echo 0", ""),
		"unknown": delegationRoutingAccount(exe, "exit 1", ""),
	}}, lifetime: t.Context()}
	p.configureDelegation()

	child, page := runDelegatedRoutingTest(t, p, `["startup","m","parent"]`)
	if child.ID != `["unknown","m","child"]` || page.Status != "completed" {
		t.Fatalf("child %s finished with %+v", child.ID, page)
	}
	for _, name := range []string{"initialize", "session", "select"} {
		if routing.RemainingBackoff(name) <= 0 {
			t.Errorf("setup rate limit for %q was not recorded", name)
		}
	}
}

func TestDelegatedChildRecordsPromptRateLimit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 2} {
		for _, failure := range []string{"session/prompt", "rate_limit"} {
			t.Run(strconv.Itoa(version)+"/"+failure, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				p := &proxy{Config: routing.Config{Delegation: true, Model: "m", ACPv2: version == 2, Accounts: map[string]routing.Account{
					"primary": delegationRoutingAccount(exe, "echo 100", failure), "standby": delegationRoutingAccount(exe, "echo 10", ""),
				}}, lifetime: t.Context()}
				p.configureDelegation()
				child, page := runDelegatedRoutingTest(t, p, `["primary","m","parent"]`)
				if child.ID != `["primary","m","child"]` || page.Status != "failed" {
					t.Fatalf("child %s finished with %+v", child.ID, page)
				}
				if routing.RemainingBackoff("primary") <= 0 {
					t.Fatal("prompt rate limit was not recorded")
				}
			})
		}
	}
}

func delegationRoutingAccount(exe, quota, failure string) routing.Account {
	return routing.Account{
		Models: map[string][]string{"m": {exe, "-test.run=^TestDelegationRoutingAdapterFixture$"}},
		Usage:  []string{"/bin/sh", "-c", quota},
		Env: map[string]string{
			"PETAL_ROUTING_FIXTURE":         "1",
			"PETAL_ROUTING_FIXTURE_FAILURE": failure,
		},
	}
}

func runDelegatedRoutingTest(t *testing.T, p *proxy, parentID string) (delegation.Child, delegation.Page) {
	t.Helper()
	cwd := t.TempDir()
	if _, err := sessions.Create(cwd, sessions.Record{ID: parentID, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child, err := p.delegationRuntime.Delegate(ctx, parentID, cwd, "m", "do it")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-child.Done:
	case <-ctx.Done():
		t.Fatal("child did not finish")
	}
	page, err := p.delegationRuntime.Read(ctx, parentID, cwd, child.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return child, page
}

func TestDelegationRoutingAdapterFixture(t *testing.T) {
	if os.Getenv("PETAL_ROUTING_FIXTURE") == "" {
		return
	}
	failure := os.Getenv("PETAL_ROUTING_FIXTURE_FAILURE")
	scan := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	write := func(frame Object) {
		t.Helper()
		if err := out.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	var version uint16
	for scan.Scan() {
		var request Object
		if err := json.Unmarshal(scan.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		method := stringField(request, "method")
		response := Object{"jsonrpc": encoded("2.0"), "id": request["id"]}
		if method == failure {
			response["error"] = json.RawMessage(`{"code":429,"message":"rate limited"}`)
			write(response)
			continue
		}
		switch method {
		case "initialize":
			if err := json.Unmarshal(request.member("params")["protocolVersion"], &version); err != nil {
				t.Fatal(err)
			}
			response["result"] = json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{}}`)
			if version == 2 {
				response["result"] = json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"1"},"capabilities":{}}`)
			}
		case "session/new":
			response["result"] = json.RawMessage(`{"sessionId":"child"}`)
		case "session/cancel":
			if os.Getenv("PETAL_DELEGATED_HELPER_ADDR") == "" {
				t.Fatalf("unexpected cancellation")
			}
			continue
		case "session/prompt":
			if addr := os.Getenv("PETAL_DELEGATED_HELPER_ADDR"); addr != "" {
				startDelegatedHelper(t, addr)
				continue
			}
			reason := "end_turn"
			if failure == "rate_limit" {
				reason = failure
			}
			if version == 2 {
				response["result"] = json.RawMessage(`{"messageId":"accepted"}`)
				write(response)
				write(Object{
					"jsonrpc": encoded("2.0"), "method": encoded("session/update"),
					"params": jsonValue(Object{"sessionId": encoded("child"), "update": jsonValue(Object{
						"sessionUpdate": encoded("state_update"), "state": encoded("idle"), "stopReason": encoded(reason),
					})}),
				})
				continue
			}
			response["result"] = jsonValue(Object{"stopReason": encoded(reason)})
		default:
			t.Fatalf("unexpected method %q", method)
		}
		write(response)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
}
