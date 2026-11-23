package routing

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCodexDiscoversModelsWithoutCache(t *testing.T) {
	home := t.TempDir()
	cli := filepath.Join(home, "codex")
	if err := os.WriteFile(cli, []byte(`#!/bin/sh
[ "$1 $2" = "debug models" ] && [ -d "$CODEX_HOME" ] || exit 1
echo '{"models":[{"slug":"fresh","display_name":"Fresh model","visibility":"list"},{"slug":"hidden","visibility":"hide"}]}'
`), 0700); err != nil {
		t.Fatal(err)
	}
	a := discoverAccount("codex", cli, "codex-acp", Account{Env: map[string]string{"CODEX_HOME": home}}, func(probe func()) { probe() })
	if !a.SelectModel["fresh"] || a.ModelLabels["fresh"] != "Fresh model" || len(a.Models["hidden"]) > 0 {
		t.Fatal(a.Models)
	}
}

func TestColdCatalogsStayScopedToTheirAccount(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("CODEX_PATH", "")
	cli := []byte(`#!/bin/sh
printf '{"models":[{"slug":"%s","visibility":"list"},{"slug":"explicit","visibility":"list"}]}' "${CODEX_HOME##*/}"
`)
	for name, data := range map[string][]byte{"codex": cli, "codex-acp": []byte("#!/bin/sh\nexit 1")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := Config{Accounts: map[string]Account{}}
	for _, name := range []string{"personal", "work"} {
		c.Accounts[name] = Account{Provider: "codex", Env: map[string]string{"CODEX_HOME": filepath.Join(dir, name)}, Models: map[string][]string{"explicit": {"custom"}}}
	}
	c.discover()
	for name, a := range c.Accounts {
		if len(a.Models) != 3 || !a.SelectModel[name] || !slices.Equal(a.Models["explicit"], []string{"custom"}) || a.SelectModel["explicit"] {
			t.Fatalf("%s models %v, selectable %v", name, a.Models, a.SelectModel)
		}
	}
}

func TestDefaultLabelReflectsAllEligibleAccounts(t *testing.T) {
	const id = "codex/default"
	account := func(model string) Account {
		return Account{Models: map[string][]string{id: {"adapter"}}, ModelLabels: map[string]string{id: model}}
	}
	c := Config{Accounts: map[string]Account{"work": account("model-a"), "personal": account("model-a")}}
	if got := c.ModelLabel(id); got != "model-a" {
		t.Fatal(got)
	}
	for _, other := range []string{"model-b", ""} {
		c.Accounts["personal"] = account(other)
		if got := c.ModelLabel(id); got != "Codex default" {
			t.Fatal(got)
		}
	}
	c.PinnedAccount = "work"
	if got := c.ModelLabel(id); got != "model-a" {
		t.Fatal(got)
	}
}
