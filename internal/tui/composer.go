package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type sessionDraftKey struct {
	cwd, id string // The public session ID includes the account and model.
}

type sessionDrafts map[sessionDraftKey]*textarea.Model

func (drafts sessionDrafts) take(cwd string, start sessionStart) *textarea.Model {
	if start.resume && start.id != "" {
		key := sessionDraftKey{cwd, start.id}
		editor := drafts[key]
		delete(drafts, key)
		if start.editor == nil && editor != nil {
			return editor
		}
	}
	if start.editor != nil {
		return start.editor
	}
	return newComposer()
}

func (drafts sessionDrafts) remember(m *terminal) {
	if m.draftSessionID == "" {
		return
	}
	editor := m.editor
	if m.permissionDraft != nil {
		editor = m.permissionDraft
	}
	if editor.Value() != "" {
		drafts[sessionDraftKey{m.Cwd, m.draftSessionID}] = editor
	}
}

func newComposer() *textarea.Model {
	editor := textarea.New()
	editor.Prompt = ""
	editor.ShowLineNumbers = false
	editor.MaxHeight, editor.MaxWidth = 0, 0
	editor.SetHeight(1)
	styles := editor.Styles()
	// The surrounding composer band supplies the line colors, including while typing.
	state := textarea.StyleState{Selection: lipgloss.NewStyle().Reverse(true)}
	styles.Focused, styles.Blurred = state, state
	styles.Cursor.Color = lipgloss.Color(roseHex)
	styles.Cursor.Blink = false
	editor.SetStyles(styles)
	editor.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter"))
	editor.KeyMap.Paste.SetEnabled(false) // Terminal bracketed pastes arrive as tea.PasteMsg.
	editor.Focus()
	return &editor
}

func (m *terminal) edit(msg tea.Msg) tea.Cmd {
	updated, cmd := m.editor.Update(msg)
	*m.editor = updated
	return cmd
}

// Bubbles v2.2 caps its backing buffer at 10,000 logical lines. Reject an
// oversized paste atomically so the submitted prompt cannot lose its tail.
func (m *terminal) paste(text string) tea.Cmd {
	text = clean(text)
	lines := m.editor.LineCount() - strings.Count(m.editor.SelectedText(), "\n") + strings.Count(text, "\n")
	if lines > 10000 {
		message := "Paste exceeds the composer limit of 10,000 lines."
		if m.Busy {
			m.recordNote(message)
		} else {
			m.Messages = append(m.Messages, entry{"system", message})
		}
		return nil
	}
	return m.edit(tea.PasteMsg{Content: text})
}
