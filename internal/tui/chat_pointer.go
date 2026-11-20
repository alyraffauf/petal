package tui

import (
	"image"

	tea "charm.land/bubbletea/v2"
)

type chatAction string

const (
	chatSend     chatAction = "Send"
	chatConfirm  chatAction = "Confirm"
	chatRun      chatAction = "Run"
	chatSteer    chatAction = "Steer"
	chatFollowUp chatAction = "Follow up"
)

type chatHitTargets struct {
	cursor          *tea.Cursor
	commands        image.Rectangle
	firstCommand    int
	visibleCommands []chatCommand
	action          image.Rectangle
	actionKind      chatAction
	actionInput     string
	actionCommand   chatCommand
	question        *permission
}

func (m *terminal) handleChatClick(click tea.MouseClickMsg) tea.Cmd {
	if click.Button != tea.MouseLeft || m.reading || m.forking || m.connection == connectionFailed {
		return nil
	}
	point := image.Pt(click.X, click.Y)
	hits := m.chatHits
	if hits.question != m.ask {
		return nil
	}
	if point.In(hits.commands) && !point.In(hits.action) {
		row := point.Y - hits.commands.Min.Y
		index := hits.firstCommand + row
		matches := m.commandMatches()
		if index < len(matches) && matches[index] == hits.visibleCommands[row] {
			m.commandIndex = index
		}
		return nil
	}
	if !point.In(hits.action) || m.editor == nil || m.editor.Value() != hits.actionInput {
		return nil
	}
	matches := m.commandMatches()
	switch hits.actionKind {
	case chatConfirm:
		if m.ask == nil {
			return nil
		}
	case chatRun:
		if len(matches) == 0 ||
			matches[min(m.commandIndex, len(matches)-1)] != hits.actionCommand {
			return nil
		}
	case chatSend, chatFollowUp, chatSteer:
		if m.connection != connected || len(matches) != 0 || m.currentSubmitAction() != hits.actionKind {
			return nil
		}
	default:
		return nil
	}
	return m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func (m *terminal) currentSubmitAction() chatAction {
	if m.ask != nil {
		return chatConfirm
	}
	if !m.Busy {
		return chatSend
	}
	if m.canSteer() {
		return chatSteer
	}
	return chatFollowUp
}
