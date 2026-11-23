package tui

import (
	"slices"
	"testing"

	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestQuickStartOffersEachProviderDefault(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := routing.Config{Accounts: map[string]routing.Account{
		"claude": {Provider: "claude", DefaultRoute: "opus", Models: map[string][]string{"claude/default": {"claude-agent-acp"}, "opus": {"claude-agent-acp"}}},
		"codex":  {Provider: "codex", DefaultRoute: "gpt-6-sol", Models: map[string][]string{"codex/default": {"codex-acp"}, "gpt-6-sol": {"codex-acp"}}},
	}}
	var quick []string
	for _, n := range newPicker(t.TempDir(), cfg).buildNodes() {
		if n.kind == "Model" {
			quick = append(quick, n.id)
		}
	}
	if !slices.Equal(quick, []string{"model/claude/default", "model/codex/default"}) {
		t.Fatalf("quick start offers %v, want each provider's default", quick)
	}
}

func TestPickerHidesResolvedDefaultOnlyFromModelChoices(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := sessions.Create(cwd, sessions.Record{ID: `["claude","claude/default","saved"]`, Model: "claude/default", Account: "claude", Title: "started from quick start"}); err != nil {
		t.Fatal(err)
	}
	cfg := routing.Config{Accounts: map[string]routing.Account{
		"claude": {Provider: "claude", DefaultRoute: "opus", Models: map[string][]string{"claude/default": {"claude-agent-acp"}, "opus": {"claude-agent-acp"}}},
	}}
	p := newPicker(cwd, cfg)
	p.showHistory()
	if len(p.saved) != 1 {
		t.Errorf("history shows %d conversations, want the one saved on claude/default", len(p.saved))
	}
	p.filter()
	var choices []string
	for _, m := range p.matches {
		choices = append(choices, m.Str)
	}
	if !slices.Equal(choices, []string{"opus"}) {
		t.Errorf("model choices = %v, want only opus", choices)
	}
}
