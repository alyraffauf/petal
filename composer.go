package main

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"strings"
)

func newComposer() *textarea.Model {
	editor := textarea.New()
	editor.Prompt = ""
	editor.ShowLineNumbers = false
	editor.MaxHeight, editor.MaxWidth = 0, 0
	editor.SetHeight(1)
	styles := editor.Styles()
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
