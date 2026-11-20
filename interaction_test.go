package main

import (
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func testComposer(text string, column int) *textarea.Model {
	editor := newComposer()
	editor.SetValue(text)
	editor.SetCursorColumn(column)
	return editor
}

func TestPickerPasteStaysOutOfDraft(t *testing.T) {
	m := &terminal{launch: &picker{models: []string{"a", "b"}}, screen: screen{editor: testComposer("draft", 5)}}
	m.Update(tea.PasteMsg{Content: "b"})
	if m.launch.query != "b" || m.editor.Value() != "draft" {
		t.Fatalf("query %q, draft %q", m.launch.query, m.editor.Value())
	}
	m.launch.choosingRoute = true
	m.Update(tea.PasteMsg{Content: "unused"})
	if m.launch.query != "b" || m.editor.Value() != "draft" {
		t.Fatal("paste escaped subscription picker")
	}
}

func TestLongDraftKeepsCaretVisible(t *testing.T) {
	for _, pos := range []int{0, 24, 64} {
		prefix := strings.Repeat("界", pos)
		v := screen{Width: 45, Height: 10, editor: testComposer(prefix+"Z"+strings.Repeat("x", 64-pos), pos)}
		row := strings.Split(ansi.Strip(petal(v)), "\n")[8]
		if !strings.Contains(row, "Z") {
			t.Fatalf("position %d lost caret: %q", pos, row)
		}
	}
}

func TestFailedResumeRecoveryKeepsSessionAndDraft(t *testing.T) {
	keys := func(m *terminal, code rune) { m.Update(tea.KeyPressMsg{Code: code}) }
	makeFailed := func() *terminal {
		return &terminal{root: bloom.New(), screen: screen{Status: "Connection failed", editor: testComposer("draft", 3)},
			cfg:    config{model: "m", forced: "a", Accounts: map[string]account{"a": {Models: map[string][]string{"m": {"agent"}}}, "b": {Models: map[string][]string{"m": {"agent"}}}}},
			resume: true}
	}
	m := makeFailed()
	keys(m, tea.KeyEnter)
	if m.next == nil || !m.next.resume || m.next.cfg.forced != "a" || m.next.editor.Value() != "draft" || m.next.editor.Column() != 3 {
		t.Fatal("retry lost the original resume request or draft")
	}
	m = makeFailed()
	keys(m, tea.KeyTab)
	keys(m, tea.KeyDown)
	keys(m, tea.KeyDown)
	keys(m, tea.KeyEnter)
	if m.next == nil || !m.next.resume || m.next.cfg.forced != "b" || m.next.editor.Value() != "draft" {
		t.Fatal("changing subscriptions lost the resume request or draft")
	}
}

func TestOversizedPastePreservesDraft(t *testing.T) {
	m := terminal{screen: screen{editor: testComposer("keep", 4)}}
	m.paste(strings.Repeat("x\n", 10000))
	if m.editor.Value() != "keep" || len(m.Messages) != 1 {
		t.Fatal("oversized paste changed draft or omitted feedback")
	}
}
