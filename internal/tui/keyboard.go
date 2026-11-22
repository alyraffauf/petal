package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m *terminal) handleKey(t tea.KeyPressMsg) tea.Cmd {
	if m.urlKey(t) {
		return nil
	}
	var cmd tea.Cmd
	if m.forking && !key.Matches(t, quitPetal) {
		return nil
	}
	if m.reading {
		step := max(1, readerBodyHeight(m.Height)-1)
		switch {
		case key.Matches(t, closeReader):
			m.reading = false
		case key.Matches(t, quitPetal):
			cmd = tea.Quit
		case key.Matches(t, pageEarlier):
			m.readerBacklog.move(-step)
		case t.String() == "end" || key.Matches(t, jumpLatest):
			m.readerBacklog.browsing = false
		case key.Matches(t, selectPrevious):
			m.readerBacklog.move(-1)
		case key.Matches(t, pageLater):
			m.readerBacklog.move(step)
		case key.Matches(t, selectNext):
			m.readerBacklog.move(1)
		}
		return cmd
	}
	if m.connection == connectionFailed {
		switch t.String() {
		case "enter", "r":
			m.next = &sessionStart{cwd: m.Cwd, cfg: m.cfg, resume: m.resume, id: m.resumeID, editor: m.editor}
			return tea.Quit
		case "tab":
			return m.openPicker(true)
		case "ctrl+n", "f2", "ctrl+c", "f3", "pgup", "pgdown", "ctrl+end":
			// These shortcuts also work on the recovery screen.
		default:
			return nil
		}
	}
	if t.String() == "ctrl+v" && m.ask == nil {
		return m.readClipboardImage()
	}
	if matches := m.commandMatches(); len(matches) > 0 {
		index := min(m.commandIndex, len(matches)-1)
		switch {
		case key.Matches(t, selectPrevious):
			m.commandIndex = max(0, index-1)
			return nil
		case key.Matches(t, selectNext):
			m.commandIndex = min(len(matches)-1, index+1)
			return nil
		case key.Matches(t, runSelected):
			return m.runCommand(matches[index])
		case key.Matches(t, dismissMenu):
			m.editor.Reset()
			m.commandIndex = 0
			return nil
		}
	}
	m.commandIndex = 0
	switch {
	case key.Matches(t, resumeConversation):
		if m.h != nil && m.connection != connecting {
			return m.browseSessions()
		}
	case key.Matches(t, inspectAgents):
		if m.ask == nil && m.connection == connected {
			return m.showAgents()
		}
	case key.Matches(t, readHistory):
		m.readerBacklog.browsing = false
		m.reading = true
	case key.Matches(t, quitPetal):
		return tea.Quit
	case key.Matches(t, runSelected):
		cmd = m.submit()
	case key.Matches(t, cancelTurn):
		if m.ask != nil {
			m.recordNote("Permission declined.")
			m.ask.cancel()
			m.ask = nil
			m.restorePermissionDraft()
			cmd = m.deliver()
		} else if m.Busy && m.v2 != nil {
			cmd = m.cancelForegroundV2()
		} else if m.Busy {
			m.cancelling = true
			m.Status = "cancelling"
			cmd = func() tea.Msg {
				err := m.h.conn.SendNotification(m.ctx, "session/cancel", map[string]string{"sessionId": m.h.id})
				if err != nil {
					return cancelFailed{err}
				}
				return nil
			}
		}
	case key.Matches(t, pageEarlier):
		m.chatBacklog.move(-max(1, m.Height/2))
	case key.Matches(t, pageLater):
		m.chatBacklog.move(max(1, m.Height/2))
	case key.Matches(t, jumpLatest):
		m.chatBacklog.browsing = false
	case t.String() == "ctrl+u":
		m.editor.Reset()
	default:
		cmd = m.edit(t)
	}
	return cmd
}
