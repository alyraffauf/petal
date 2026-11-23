package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"petal/internal/proxy"
	"petal/internal/routing"
)

func TestSettingPickerShowsCurrentGroupedValue(t *testing.T) {
	settings := settingsFrom(proxy.Object{
		"configOptions": json.RawMessage(`[{
			"configId":"effort","name":"Reasoning effort","category":"thought_level","type":"select",
			"currentValue":"high","options":[{"group":"depth","name":"Depth","options":[
				{"value":"low","name":"Low"},{"value":"high","name":"High","description":"More reasoning"}
			]}]
		}]`),
	})
	model := &terminal{agentSettings: settings}
	model.Width, model.Height = 65, 6
	model.openSettings("thought_level")
	if model.launch == nil || model.launch.setting.target.ID != "effort" {
		t.Fatal("missing ACP v2 effort option ID")
	}

	view := ansi.Strip(model.launch.View().Content)
	if !strings.Contains(view, "› Depth · High (current) · More reasoning") {
		t.Fatalf("current grouped value should be visible and selected on a short terminal:\n%s", view)
	}
}

func TestSettingsPickerShowsCurrentLegacyMode(t *testing.T) {
	model := &terminal{agentSettings: settingsFrom(proxy.Object{
		"modes": json.RawMessage(`{
			"currentModeId":"read",
			"availableModes":[{"id":"write","name":"Write"},{"id":"read","name":"Read only"}]
		}`),
	})}
	model.Width, model.Height = 65, 12
	model.openSettings("mode")

	view := ansi.Strip(model.launch.View().Content)
	if !strings.Contains(view, "› Read only (current)") {
		t.Fatalf("legacy mode should be visible and selected:\n%s", view)
	}
}

func TestComposerUsesActiveAccountsResolvedDefault(t *testing.T) {
	m := terminal{screen: screen{Account: "work", Model: "claude/default", Width: 96, Height: 20, editor: testComposer("", 0)}}
	m.cfg.Accounts = map[string]routing.Account{
		"work":     {Provider: "claude", ModelLabels: map[string]string{"claude/default": "work-model"}},
		"personal": {Provider: "claude", ModelLabels: map[string]string{"claude/default": "personal-model"}},
	}
	for value, want := range map[string]string{"default": "work-model", "claude/default": "work-model", "custom": "custom"} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		m.agentSettings.Config = []setting{{Category: "model", Type: "select", CurrentValue: raw, Options: []valueChoice{{Value: value}}}}
		if view := ansi.Strip(m.View().Content); !strings.Contains(view, "Ask "+want+"…") {
			t.Fatalf("model %q: %s", value, view)
		}
	}
}
