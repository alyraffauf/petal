package tui

import (
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/canvas"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"petal/internal/routing"
	"petal/internal/sessions"
)

func testComposer(text string, column int) *composer {
	editor := newComposer()
	editor.SetValue(text)
	editor.SetCursorColumn(column)
	return editor
}

func TestPickerPasteStaysOutOfDraft(t *testing.T) {
	m := &terminal{launch: newPicker(routing.Config{}), screen: screen{editor: testComposer("draft", 5)}}
	m.Update(tea.PasteMsg{Content: "b"})
	if m.launch.search.Value() != "b" || m.editor.Value() != "draft" {
		t.Fatalf("query %q, draft %q", m.launch.search.Value(), m.editor.Value())
	}
	m.launch.choosingRoute = true
	m.Update(tea.PasteMsg{Content: "unused"})
	if m.launch.search.Value() != "b" || m.editor.Value() != "draft" {
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
		return &terminal{root: bloom.New(), screen: screen{connection: connectionFailed, editor: testComposer("draft", 3)},
			cfg:    routing.Config{Model: "m", PinnedAccount: "a", Accounts: map[string]routing.Account{"a": {Models: map[string][]string{"m": {"agent"}}}, "b": {Models: map[string][]string{"m": {"agent"}}}}},
			resume: true}
	}
	m := makeFailed()
	keys(m, tea.KeyEnter)
	if m.next == nil || !m.next.resume || m.next.cfg.PinnedAccount != "a" || m.next.editor.Value() != "draft" || m.next.editor.Column() != 3 {
		t.Fatal("retry lost the original resume request or draft")
	}
	m = makeFailed()
	keys(m, tea.KeyTab)
	keys(m, tea.KeyDown)
	keys(m, tea.KeyDown)
	keys(m, tea.KeyEnter)
	if m.next == nil || !m.next.resume || m.next.cfg.PinnedAccount != "b" || m.next.editor.Value() != "draft" {
		t.Fatal("changing subscriptions lost the resume request or draft")
	}
	m = makeFailed()
	m.Cwd = t.TempDir()
	m.h = &harness{}
	m.resumeID = `["a","m","failed"]`
	m.draftSessionID = m.resumeID
	keys(m, tea.KeyF2)
	other := sessions.Record{ID: `["a","m","other"]`, Model: "m"}
	m.launch.saved = []sessions.Record{other}
	m.launch.center.selected = other.ID
	keys(m, tea.KeyEnter)
	if m.next == nil || m.next.id != other.ID || m.next.editor != nil {
		t.Fatal("opening a different saved thread transferred the failed thread's draft")
	}
}

func TestSwitchingThreadsRestoresEachDraft(t *testing.T) {
	cfg := routing.Config{Model: "m", Accounts: map[string]routing.Account{"a": {Models: map[string][]string{"m": {"agent"}}}}}
	drafts := make(sessionDrafts)
	first := sessions.Record{ID: `["a","m","first"]`, Model: "m"}
	second := sessions.Record{ID: `["a","m","second"]`, Model: "m"}
	cwd := t.TempDir()
	open := func(start sessionStart, record sessions.Record) *terminal {
		m := &terminal{
			root:   bloom.New(),
			cfg:    start.cfg,
			h:      &harness{id: record.ID, record: record, cwd: cwd},
			screen: screen{connection: connecting, Cwd: cwd, editor: drafts.take(cwd, start)},
		}
		m.Update(opened{account: "a"})
		return m
	}
	switchTo := func(m *terminal, record sessions.Record) sessionStart {
		t.Helper()
		m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
		if m.launch == nil {
			t.Fatal("Threads picker did not open")
		}
		m.launch.saved = []sessions.Record{record}
		m.launch.center.selected = record.ID
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.next == nil || !m.next.resume || m.next.id != record.ID {
			t.Fatalf("choosing %q produced %#v", record.ID, m.next)
		}
		drafts.remember(m)
		return *m.next
	}

	m := open(sessionStart{cfg: cfg}, first)
	m.Update(tea.PasteMsg{Content: "first line\nsecond line"})
	m.editor.SetCursorColumn(3)
	m.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	if m.next == nil || m.next.resume {
		t.Fatal("Ctrl+N did not start a new thread")
	}
	drafts.remember(m)
	m = open(*m.next, second)
	if m.editor.Value() != "" {
		t.Fatalf("new thread inherited draft %q", m.editor.Value())
	}
	m.Update(tea.PasteMsg{Content: "draft for the second thread"})
	m = open(switchTo(m, first), first)
	if m.editor.Value() != "first line\nsecond line" || m.editor.Line() != 1 || m.editor.Column() != 3 {
		t.Fatalf("first draft = %q at %d:%d", m.editor.Value(), m.editor.Line(), m.editor.Column())
	}
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	m = open(switchTo(m, second), second)
	if m.editor.Value() != "draft for the second thread" {
		t.Fatalf("second draft = %q", m.editor.Value())
	}
	m = open(switchTo(m, first), first)
	if m.editor.Value() != "" {
		t.Fatalf("cleared draft reappeared as %q", m.editor.Value())
	}
}

func TestOversizedPastePreservesDraft(t *testing.T) {
	m := terminal{screen: screen{editor: testComposer("keep", 4)}}
	m.paste(strings.Repeat("x\n", 10000))
	if m.editor.Value() != "keep" || len(m.Messages) != 1 {
		t.Fatal("oversized paste changed draft or omitted feedback")
	}
}

func TestComposerKeepsColorsOnFirstKeystroke(t *testing.T) {
	m := terminal{screen: screen{Width: 60, Height: 12, connection: connected, editor: newComposer()}}
	frame := func() *canvas.Canvas {
		return canvas.Parse(petal(m.screen), ansi.GraphemeWidth)
	}
	placeholder := frame().CellAt(4, m.Height-2)
	m.edit(tea.KeyPressMsg{Code: 'x', Text: "x"})
	typed := frame().CellAt(4, m.Height-2)
	if placeholder.Content != "A" || typed.Content != "x" || !typed.Style.Equal(&placeholder.Style) {
		t.Fatalf("composer changed on typing: %#v → %#v", placeholder, typed)
	}
}
