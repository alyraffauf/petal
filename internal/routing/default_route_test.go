package routing

import (
	"slices"
	"testing"
)

func claudeAccount() Account {
	argv := []string{"claude-agent-acp"}
	return Account{
		Provider: "claude", discoveredDefault: true,
		Models:      map[string][]string{"claude/default": argv, "opus": argv, "sonnet": argv},
		ModelLabels: map[string]string{"opus": "Opus 5.5", "sonnet": "Sonnet 5.5", "claude-opus-5-5": "Opus 5.5"},
		SelectModel: map[string]bool{"opus": true, "sonnet": true},
	}
}

func TestRouteForMapsReportedModelToListedRoute(t *testing.T) {
	for _, tc := range []struct{ reported, want string }{
		{"opus", "opus"},
		{"claude-opus-5-5", "opus"},
		{"claude-opus-4-8", ""},
		{"", ""},
		{"claude/default", ""},
	} {
		if got := claudeAccount().routeFor(tc.reported); got != tc.want {
			t.Errorf("routeFor(%q) = %q, want %q", tc.reported, got, tc.want)
		}
	}
}

func TestResolvedDefaultIsListedAsItsRealModel(t *testing.T) {
	resolved := claudeAccount()
	resolved.DefaultRoute = "opus"
	codex := Account{Provider: "codex", discoveredDefault: true, Models: map[string][]string{"codex/default": {"codex-acp"}, "gpt-6-sol": {"codex-acp"}}}
	c := Config{Accounts: map[string]Account{"claude": resolved, "codex": codex}}

	if got := c.PickerModels(); !slices.Equal(got, []string{"codex/default", "gpt-6-sol", "opus", "sonnet"}) {
		t.Errorf("PickerModels() = %v; want the resolved claude/default hidden and the unresolved codex/default kept", got)
	}
	if got := c.DefaultModel(); got != "claude/default" {
		t.Errorf("DefaultModel() = %q; new sessions must run on claude/default so each workspace's settings apply", got)
	}
	if got := c.PickerModel("claude/default"); got != "opus" {
		t.Errorf("PickerModel(claude/default) = %q, want opus", got)
	}
	if !slices.Contains(c.ModelNames(), "claude/default") {
		t.Error("claude/default is no longer routable, so saved conversations cannot resume")
	}

	unresolved := Config{Accounts: map[string]Account{"claude": claudeAccount()}}
	if got := unresolved.DefaultModel(); got != "claude/default" {
		t.Errorf("unresolved DefaultModel() = %q, want claude/default", got)
	}
	if !slices.Contains(unresolved.PickerModels(), "claude/default") {
		t.Error("unresolved claude/default was hidden, leaving no way to pick the provider default")
	}
}
