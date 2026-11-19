package main

import (
	"strings"
	"testing"

	"bloomfx.dev/bloom"
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

func TestFailedResumeRecoveryKeepsSessionAndDraft(t *testing.T) {
	keys := func(m *terminal, code rune) { m.Update(tea.KeyPressMsg{Code: code}) }
	makeFailed := func() *terminal {
		return &terminal{root: bloom.New(), screen: screen{Status: "Connection failed"},
			cfg:    config{model: "m", forced: "a", Accounts: map[string]account{"a": {Models: map[string][]string{"m": {"agent"}}}, "b": {Models: map[string][]string{"m": {"agent"}}}}},
			resume: true, draft: []rune("draft"), cursor: 3}
	}
	m := makeFailed()
	keys(m, tea.KeyEnter)
	if m.next == nil || !m.next.resume || m.next.cfg.forced != "a" || string(m.next.draft) != "draft" || m.next.cursor != 3 {
		t.Fatal("retry lost the original resume request or draft")
	}
	m = makeFailed()
	keys(m, tea.KeyTab)
	keys(m, tea.KeyDown)
	keys(m, tea.KeyDown)
	keys(m, tea.KeyEnter)
	if m.next == nil || !m.next.resume || m.next.cfg.forced != "b" || string(m.next.draft) != "draft" {
		t.Fatal("changing subscriptions lost the resume request or draft")
	}
}
