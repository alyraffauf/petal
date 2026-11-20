package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"petal/internal/proxy"
)

func TestSettingPickerShowsCurrentGroupedValue(t *testing.T) {
	settings := settingsFrom(proxy.Object{
		"configOptions": json.RawMessage(`[{
			"id":"effort","name":"Reasoning effort","category":"thought_level","type":"select",
			"currentValue":"high","options":[{"group":"depth","name":"Depth","options":[
				{"value":"low","name":"Low"},{"value":"high","name":"High","description":"More reasoning"}
			]}]
		}]`),
	})
	model := &terminal{agentSettings: settings}
	model.Width, model.Height = 65, 6
	model.openSettings("thought_level")

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
