package main

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

func (m *terminal) handleKey(t tea.KeyPressMsg) tea.Cmd {
	var cmd tea.Cmd
	if m.reader.Shown() {
		step := max(1, m.Height-8)
		switch {
		case key.Matches(t, closeReader):
			cmd = m.reader.Hide()
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
	if m.Status == "Connection failed" {
		switch t.String() {
		case "enter", "r":
			m.next = &sessionStart{cfg: m.cfg, resume: m.resume, editor: m.editor}
			return tea.Quit
		case "tab":
			return m.openPicker(true)
		case "ctrl+n", "ctrl+c", "f3", "pgup", "pgdown", "ctrl+end":
			// These shortcuts also work on the recovery screen.
		default:
			return nil
		}
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
	case key.Matches(t, newThread):
		if m.h != nil && !m.Busy && m.Status != "Connecting" {
			cmd = m.openPicker(false)
		}
	case key.Matches(t, readHistory):
		m.readerBacklog.browsing = false
		cmd = m.reader.Show()
	case t.String() == "ctrl+r":
		if m.h == nil {
			sample := demoScreen()
			m.Messages = sample.Messages
			last := m.Messages[len(m.Messages)-1]
			m.Messages = m.Messages[:len(m.Messages)-1]
			m.pending = turnLog{}
			m.pending.push(event{role: last.Role, text: last.Text, chunk: true})
			m.replay = true
			m.chatBacklog.browsing = false
			m.Frame = 0
			m.Status = "Demo · working"
			m.Busy = true
		}
	case key.Matches(t, quitPetal):
		return tea.Quit
	case key.Matches(t, runSelected):
		cmd = m.submit()
	case key.Matches(t, cancelTurn):
		if m.ask != nil {
			m.recordNote("Permission declined.")
			m.ask.reply <- -1
			m.ask = nil
			m.restorePermissionDraft()
			m.Status = "Working"
			cmd = m.deliver()
		} else if m.Busy && m.h == nil {
			m.replay = false
			m.pending = turnLog{}
			m.Busy = false
			m.Status = "Demo · ready"
		} else if m.Busy {
			m.Status = "Cancelling"
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
