package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"petal/internal/routing"
)

func TestPickerKeepsMultilineModelOnOneRow(t *testing.T) {
	m := newPicker(".", routing.Config{Accounts: map[string]routing.Account{"work": {Models: map[string][]string{"model\nname": {"agent"}}}}})
	m.height, m.width = 12, 70
	if got := strings.Count(m.View().Content, "\n") + 1; got != m.height {
		t.Fatalf("picker drew %d rows in a %d-row terminal", got, m.height)
	}
}

func TestPickerEditsFuzzyQueryAndRoutesSelectedModel(t *testing.T) {
	models := map[string][]string{"gpt-6.2-sol": {"agent"}}
	for i := range 25 {
		models[fmt.Sprintf("model-%02d", i)] = []string{"agent"}
	}
	m := newPicker(".", routing.Config{Accounts: map[string]routing.Account{"work": {Models: models, ModelLabels: map[string]string{"gpt-6.2-sol": "Solar"}}}})
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 7})
	for range len(models) {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	frame := ansi.Strip(m.View().Content)
	if m.option(m.index) != "model-24" || !strings.Contains(frame, "model-24") {
		t.Fatalf("selection did not scroll into view:\n%s", frame)
	}
	m.Update(tea.PasteMsg{Content: "Solar"})
	if m.count() != 1 || m.option(0) != "gpt-6.2-sol" || !strings.Contains(ansi.Strip(m.View().Content), "Solar") {
		t.Fatal("display-name search lost its route")
	}
	m.search.Reset()
	m.filter()
	m.Update(tea.PasteMsg{Content: "g6s"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	if m.search.Value() != "g62s" || m.count() != 1 || m.option(0) != "gpt-6.2-sol" {
		t.Fatalf("query %q returned %v", m.search.Value(), m.matches)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.chosen || m.cfg.Model != "gpt-6.2-sol" || m.cfg.PinnedAccount != "work" {
		t.Fatalf("unexpected route %q / %q", m.cfg.Model, m.cfg.PinnedAccount)
	}
}
