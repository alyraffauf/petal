package routing

import (
	"context"
	"encoding/json"
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

func TestBundledDiscoveryUsesConfiguredProviderExecutables(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CODEX_PATH", "")
	t.Setenv("CLAUDE_CODE_EXECUTABLE", "")
	dir := t.TempDir()
	executableFile := func(name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldCodex, oldClaude := bundledCodex, bundledClaude
	t.Cleanup(func() { bundledCodex, bundledClaude = oldCodex, oldClaude })
	bundledCodex, bundledClaude = executableFile("codex-acp"), executableFile("claude-agent-acp")
	codex, claude := executableFile("codex"), executableFile("claude")
	c := Config{Accounts: map[string]Account{
		"work":       {Provider: "codex", Env: map[string]string{"CODEX_PATH": codex, "CODEX_HOME": dir}},
		"personal":   {Provider: "claude", Env: map[string]string{"CLAUDE_CODE_EXECUTABLE": claude}},
		"unresolved": {Provider: "codex"},
	}}
	c.discover()
	if got := c.Accounts["work"].Models["codex/default"]; !slices.Equal(got, []string{bundledCodex}) {
		t.Fatalf("Codex route points to %v", got)
	}
	if got := c.Accounts["personal"].Models["claude/default"]; !slices.Equal(got, []string{bundledClaude}) {
		t.Fatalf("Claude route points to %v", got)
	}
	if got := c.Accounts["unresolved"].Models; len(got) != 0 {
		t.Fatalf("another account unexpectedly inherited a private CLI: %v", got)
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
	const profiles = `[[accounts]]
name = "work"
provider = "claude"
home = "~/.claude-work"
[[accounts]]
name = "personal"
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

func TestConfigPreservesProviderKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for ext, data := range map[string]string{
		".toml": `[[accounts]]
name="Work"
[[accounts.env]]
name="MODE_SWITCH"
value="enabled"
[[accounts.models]]
id="Vendor/Model-V2"
command=["agent"]`,
		".json": `{"accounts":[{"name":"Work","env":[{"name":"MODE_SWITCH","value":"enabled"}],"models":[{"id":"Vendor/Model-V2","command":["agent"]}]}]}`,
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

func TestCodexDiscoveryHonorsEmptyAccountHomeOverride(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	ambient := filepath.Join(dir, "ambient")
	for cache, model := range map[string]string{filepath.Join(home, ".codex"): "home-model", ambient: "ambient-model"} {
		if err := os.MkdirAll(cache, 0700); err != nil {
			t.Fatal(err)
		}
		data := []byte(`{"models":[{"slug":"` + model + `","visibility":"list"}]}`)
		if err := os.WriteFile(filepath.Join(cache, "models_cache.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", ambient)
	account := discoverAccount("codex", "", "codex-acp", Account{Env: map[string]string{"CODEX_HOME": ""}})
	if len(account.Models["home-model"]) == 0 || len(account.Models["ambient-model"]) != 0 {
		t.Fatalf("loaded models from the wrong Codex home: %v", account.Models)
	}
}

func TestRankChildAccountsByWeightedQuotaAndAffinity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	account := func(model, probe string, capacity float64) Account {
		return Account{
			Models:   map[string][]string{model: {"agent"}},
			Usage:    []string{"/bin/sh", "-c", probe},
			Capacity: &capacity,
		}
	}
	config := Config{Model: "m", Accounts: map[string]Account{
		"a":         account("m", "echo 90", 1),
		"parent":    account("m", "echo 30", 3),
		"larger":    account("m", "echo 20", 5),
		"unknown":   account("m", "exit 1", 1),
		"exhausted": account("m", "echo 0", 10),
		"cooled":    account("m", "echo 100", 10),
		"other":     account("another", "echo 100", 10),
	}}
	RecordRateLimit("cooled")
	rank := func(want ...string) {
		t.Helper()
		got, err := config.Rank(t.Context(), "parent")
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("got %v, %v; want %v", got, err, want)
		}
	}
	rank("larger", "parent", "a", "unknown")
	config.PinnedAccount = "cooled"
	rank("cooled")
	config.PinnedAccount = "other"
	if names, err := config.Rank(t.Context(), "parent"); err == nil {
		t.Fatalf("rank accepted a pin that does not offer the model: %v", names)
	}
	config.PinnedAccount = ""
	config.Accounts = map[string]Account{"cooled": config.Accounts["cooled"], "exhausted": config.Accounts["exhausted"]}
	if names, err := config.Rank(t.Context(), "parent"); err == nil {
		t.Fatalf("rank accepted exhausted or cooled accounts: %v", names)
	}
}
