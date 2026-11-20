package tui

import (
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
)

func clickChatLabel(t *testing.T, m *terminal, label string) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(chatLabelClick(t, m, label))
	return cmd
}

func chatLabelClick(t *testing.T, m *terminal, label string) tea.MouseClickMsg {
	t.Helper()
	for y, row := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		if before, _, found := strings.Cut(row, label); found {
			return tea.MouseClickMsg{X: ansi.StringWidth(before), Y: y, Button: tea.MouseLeft}
		}
	}
	t.Fatalf("label %q is missing from the chat", label)
	return tea.MouseClickMsg{}
}

func TestClickSelectsCommandInScrolledMenu(t *testing.T) {
	m := &terminal{root: bloom.New(), h: &harness{}, screen: screen{
		Width: 80, Height: 12, editor: testComposer("/", 1), connection: connected,
	}}
	m.agentCommands = agentCommandUpdate{{Name: "review"}, {Name: "stats"}}
	for range len(m.commandMatches()) {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	clickChatLabel(t, m, "/history")
	if m.reading || !strings.Contains(ansi.Strip(m.View().Content), "› /history") {
		t.Fatal("click did not select the visible command")
	}
	clickChatLabel(t, m, "↵ Run")
	if !m.reading || m.editor.Value() != "" {
		t.Fatal("Run did not open the history reader")
	}
}

func TestClickConfirmValidatesPermissionChoice(t *testing.T) {
	reply := make(chan int, 1)
	m := &terminal{root: bloom.New(), screen: screen{
		Width: 60, Height: 7, editor: testComposer("3", 1), connection: connected,
	}}
	m.permissionDraft = testComposer("saved draft", 11)
	m.ask = &permission{options: []option{{"allow", "Allow"}, {"deny", "Deny"}}, reply: reply}
	clickChatLabel(t, m, "↵ Confirm")
	if m.ask == nil || m.editor.Value() != "3" || len(reply) != 0 || m.Status == "" {
		t.Fatal("Confirm accepted an invalid choice or omitted validation feedback")
	}
	m.editor.SetValue("2")
	clickChatLabel(t, m, "↵ Confirm")
	select {
	case choice := <-reply:
		if choice != 1 {
			t.Fatalf("Confirm sent choice %d, want 1", choice)
		}
	default:
		t.Fatal("Confirm did not answer the permission request")
	}
	if m.ask != nil || m.editor.Value() != "saved draft" {
		t.Fatal("Confirm did not restore the original draft")
	}
}

func TestClickSendSubmitsDraft(t *testing.T) {
	m := &terminal{root: bloom.New(), h: &harness{}, screen: screen{
		Width: 60, Height: 12, editor: testComposer("hello", 5), connection: connected,
		notice: &acp.SessionUpdateNotice{Title: "A notice"},
	}}
	if cmd := clickChatLabel(t, m, "↵ Send"); cmd == nil ||
		len(m.Messages) != 1 || m.Messages[0] != (entry{"user", "hello"}) || m.editor.Value() != "" {
		t.Fatal("Send did not submit the draft")
	}
}

func TestHiddenSendButtonDoesNotSubmitDraft(t *testing.T) {
	m := &terminal{root: bloom.New(), h: &harness{}, screen: screen{
		Width: 38, Height: 1, editor: testComposer("hello", 5), connection: connected,
	}}
	if !strings.Contains(ansi.Strip(m.View().Content), "↵ Send") {
		t.Fatal("Send missing before resize")
	}
	m.Update(tea.WindowSizeMsg{Width: 37, Height: 1})
	if strings.Contains(ansi.Strip(m.View().Content), "↵ Send") {
		t.Fatal("Send still visible after resize")
	}
	m.Update(tea.MouseClickMsg{X: 35, Y: 0, Button: tea.MouseLeft})
	if m.editor.Value() != "hello" || len(m.Messages) != 0 {
		t.Fatal("the hidden Send button submitted the draft")
	}
}

func TestStaleFooterClickDoesNotDispatchNewAction(t *testing.T) {
	for _, test := range []struct {
		name, input, label string
		change             func(*terminal)
	}{
		{
			name: "Send became Run", input: "hello", label: "↵ Send",
			change: func(m *terminal) { m.editor.SetValue("/quit") },
		},
		{
			name: "Run became Send", input: "/history", label: "↵ Run",
			change: func(m *terminal) { m.editor.SetValue("hello") },
		},
		{
			name: "Run selection changed", input: "/", label: "↵ Run",
			change: func(m *terminal) { m.handleKey(tea.KeyPressMsg{Code: tea.KeyDown}) },
		},
		{
			name: "Send draft changed", input: "hello", label: "↵ Send",
			change: func(m *terminal) { m.editor.SetValue("different prompt") },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := &terminal{root: bloom.New(), h: &harness{}, screen: screen{
				Width: 80, Height: 12, editor: testComposer(test.input, len(test.input)), connection: connected,
			}}
			click := chatLabelClick(t, m, test.label)
			test.change(m)
			current := m.editor.Value()
			if cmd := m.handleChatClick(click); cmd != nil || m.editor.Value() != current || len(m.Messages) != 0 {
				t.Fatal("stale footer click dispatched the action from the new state")
			}
		})
	}
}
