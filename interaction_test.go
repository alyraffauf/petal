package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestPickerPasteStaysOutOfDraft(t *testing.T) {
	m := &terminal{launch: &picker{models: []string{"a", "b"}}, draft: []rune("draft"), cursor: 5}
	m.Update(tea.PasteMsg{Content: "b"})
	if m.launch.query != "b" || string(m.draft) != "draft" {
		t.Fatalf("query %q, draft %q", m.launch.query, m.draft)
	}
	m.launch.choosingRoute = true
	m.Update(tea.PasteMsg{Content: "unused"})
	if m.launch.query != "b" || string(m.draft) != "draft" {
		t.Fatal("paste escaped subscription picker")
	}
}

func TestLongDraftKeepsCaretVisible(t *testing.T) {
	for _, pos := range []int{0, 24, 64} {
		prefix := strings.Repeat("界", pos)
		v := screen{Width: 45, Height: 10, Input: prefix + "▏" + strings.Repeat("x", 64-pos), cursorColumn: ansi.StringWidth(prefix)}
		row := strings.Split(ansi.Strip(petal(v)), "\n")[8]
		if !strings.Contains(row, "▏") {
			t.Fatalf("position %d lost caret: %q", pos, row)
		}
	}
}
