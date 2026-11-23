package tui

import (
	"slices"
	"testing"

	"petal/internal/routing"
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
