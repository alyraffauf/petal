package tui

import (
	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"
)

type steerResult struct {
	outcome string
	err     error
}

func (m *terminal) canSteer() bool {
	return m.h != nil && m.h.supportsSteering && m.cfg.BusyInput != "follow_up" && !m.waitForTurn && !m.cancelling
}

// One prompt owns the turn and at most one steering request can be outstanding.
// Keep the head queued until the adapter says whether it consumed the message.
func (m *terminal) deliver() tea.Cmd {
	if len(m.outbox) == 0 || m.steering || m.ask != nil || m.changingSetting {
		return nil
	}
	message := m.outbox[0]
	params := map[string]any{"sessionId": m.h.id, "prompt": message.content}
	if m.h.v2.Load() {
		return m.deliverV2(params)
	}
	if m.Busy {
		if !m.canSteer() {
			return nil
		}
		m.steering = true
		params["_meta"] = map[string]any{"steering": map[string]string{"idleBehavior": "promptRequired"}}
		return func() tea.Msg {
			result, err := sendRequest[struct{ Outcome string }](m.h, m.ctx, "_session/steering", params)
			return steerResult{result.Outcome, err}
		}
	}
	m.outbox = m.outbox[1:]
	m.Messages = append(m.Messages, entry{"user", message.text})
	m.indexMessages(m.Messages[len(m.Messages)-1:], false)
	m.turnStart = len(m.Messages)
	m.chatBacklog.browsing = false
	m.Busy = true
	m.Status = "working"
	var written *promptWriteV2
	if m.h.daemon != nil {
		written = &promptWriteV2{ready: make(chan struct{})}
		m.h.promptWrite.Store(written)
	}
	return func() tea.Msg {
		if written != nil {
			number, err := m.h.daemon.submit(m.ctx, "session/prompt", params)
			written.operation = number
			m.h.completePromptWrite(written)
			if err != nil {
				m.h.emitV2(m.ctx, finished{err: err})
			}
			return nil
		}
		result, err := sendRequest[acp.PromptResponse](m.h, m.ctx, "session/prompt", params)
		select {
		case m.h.events <- finished{err, string(result.StopReason)}:
		case <-m.ctx.Done():
		}
		return nil
	}
}
