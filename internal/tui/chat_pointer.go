package tui

import (
	"image"

	tea "charm.land/bubbletea/v2"
)

type chatHitTargets struct {
	commands        image.Rectangle
	firstCommand    int
	visibleCommands []chatCommand
	action          image.Rectangle
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
	if point.In(hits.commands) {
		row := point.Y - hits.commands.Min.Y
		index := hits.firstCommand + row
		matches := m.commandMatches()
		if index < len(matches) && matches[index] == hits.visibleCommands[row] {
			m.commandIndex = index
		}
		return nil
	}
	if point.In(hits.action) {
		return m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	return nil
}
