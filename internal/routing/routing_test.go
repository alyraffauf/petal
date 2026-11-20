package routing

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestPlacementUsesOnlyEligibleSubscriptions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	script := filepath.Join(t.TempDir(), "quota")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$REMAINING\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	accountFor := func(model, value string) Account {
		return Account{Models: map[string][]string{model: {"agent"}}, Usage: []string{script}, Env: map[string]string{"REMAINING": value}}
	}
	c := Config{Model: "m", Accounts: map[string]Account{"a": accountFor("m", "76"), "b": accountFor("m", "90"), "other": accountFor("another", "100"), "bad": accountFor("m", "nan"), "exhausted": accountFor("m", "0"), "unmetered": {Models: map[string][]string{"m": {"agent"}}}}}
	choose := func(want string) {
		t.Helper()
		got, err := c.Choose(context.Background())
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	choose("b")
	large := c.Accounts["a"]
	size := 5.0
	large.Capacity = &size
	large.Env["REMAINING"] = "30"
	c.Accounts["a"] = large
	choose("a")
	large.Env["REMAINING"] = "10"
	choose("b")
	delete(c.Accounts, "b")
	choose("a")
	delete(c.Accounts, "a")
	choose("unmetered")
	delete(c.Accounts, "unmetered")
	if _, err := c.Choose(context.Background()); err == nil {
		t.Fatal("all eligible probes failed or reported exhaustion")
	}
	c.PinnedAccount = "exhausted"
	choose("exhausted")
}
func TestProfileDiscoveryPreservesExplicitCommands(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"codex", "codex-acp", "claude", "claude-agent-acp", "opencode"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf 'local/chat:latest\\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-test","visibility":"list"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Accounts: map[string]Account{"work": {Provider: "codex", Env: map[string]string{"CODEX_HOME": dir}, Models: map[string][]string{"gpt-test": {"custom"}}}}}
	c.discover()
	if _, exists := c.Accounts["opencode"]; exists {
		t.Fatal("discovered an unsupported provider")
	}
	if _, dup := c.Accounts["codex"]; dup {
		t.Fatal("default credentials duplicated a configured provider")
	}
	if a := c.Accounts["work"]; a.SelectModel["gpt-test"] || !slices.Equal(a.Models["gpt-test"], []string{"custom"}) {
		t.Fatal("discovery replaced explicit model command")
	}
	for _, want := range []string{"sonnet", "gpt-test"} {
		if !slices.Contains(c.ModelNames(), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestRateLimitMovesOnlyFutureAutoRoutes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := Account{Models: map[string][]string{"model": {"agent"}}}
	c := Config{Accounts: map[string]Account{"work": a, "other": a}, Model: "model"}
	RememberRateLimit("work", &acp.RequestError{Code: -32603, Data: json.RawMessage(`{"errorKind":"rate_limit"}`)})
	n, err := c.Choose(context.Background())
	if err != nil || n != "other" {
		t.Fatalf("cooled route won: %s %v", n, err)
	}
	c.PinnedAccount = "work"
	n, err = c.Choose(context.Background())
	if err != nil || n != "work" {
		t.Fatalf("manual override failed: %s %v", n, err)
	}
}

func TestConfiguredHomesReachProviderCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	const profiles = `[accounts.work]
provider = "claude"
home = "~/.claude-work"
[accounts.personal]
provider = "codex"
home = "profiles/codex"
`
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(profiles), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ key, dir string }{
		"work":     {"CLAUDE_CONFIG_DIR", ".claude-work"},
		"personal": {"CODEX_HOME", "profiles/codex"},
	} {
		t.Setenv(tc.key, "/unrelated")
		out, err := c.Accounts[name].Command(context.Background(), []string{"/bin/sh", "-c", "printf %s \"$" + tc.key + "\""}).Output()
		if err != nil || string(out) != filepath.Join(dir, tc.dir) {
			t.Errorf("%s profile directory = %q, err = %v", name, out, err)
		}
	}
}

func TestAutoRoutingPreservesTinyQuotaOnLargeSubscription(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	large, small := math.MaxFloat64, math.SmallestNonzeroFloat64
	models := map[string][]string{"m": {"agent"}}
	c := Config{Model: "m", Accounts: map[string]Account{
		"large": {Capacity: &large, Models: models, Usage: []string{"/bin/sh", "-c", "printf 5e-324"}},
		"small": {Capacity: &small, Models: models, Usage: []string{"/bin/sh", "-c", "printf 100"}},
	}}
	// The large subscription has about 9e-18 units left. The small
	// subscription has about 5e-324 units even at full quota.
	got, err := c.Choose(context.Background())
	if err != nil || got != "large" {
		t.Fatalf("expected large subscription, got %q (%v)", got, err)
	}
}

func TestConfigPreservesProviderKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for ext, data := range map[string]string{
		".toml": `[accounts.Work.env]
MODE_SWITCH="enabled"
[accounts.Work.models]
"Vendor/Model-V2"=["agent"]`,
		".json": `{"accounts":{"Work":{"env":{"MODE_SWITCH":"enabled"},"models":{"Vendor/Model-V2":["agent"]}}}}`,
	} {
		path := filepath.Join(dir, "profile"+ext)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path, "Vendor/Model-V2", "Work")
		if err != nil || c.Accounts["Work"].Env["MODE_SWITCH"] != "enabled" {
			t.Fatalf("%s profile did not preserve provider keys: %v", ext, err)
		}
	}
}
