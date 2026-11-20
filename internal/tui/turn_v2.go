package tui

import (
	"encoding/json"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/acpv2"
)

type promptV2Result struct{ err error }
type cancelV2Result struct{ err error }
type disconnectedV2 struct{}

// The prompt RPC and the foreground lifecycle can finish in either order.
type foregroundV2 struct {
	state         string
	submitting    bool
	awaitingStart bool
	wasActive     bool
}

func (f *foregroundV2) beginPrompt() {
	f.submitting = true
	f.awaitingStart = true
}

func (f *foregroundV2) settlePrompt(err error) {
	f.submitting = false
	if err != nil {
		f.awaitingStart = false
	}
}

func (f *foregroundV2) report(state string) (completed bool) {
	switch state {
	case "running", "requires_action":
		f.wasActive = true
		f.awaitingStart = false
	case "idle":
		completed = f.wasActive
		f.wasActive = false
	default:
		state = "unknown"
	}
	f.state = state
	return completed
}

func (f *foregroundV2) waiting() bool {
	return f.submitting || f.awaitingStart || (f.state != "" && f.state != "idle")
}

type conversationV2 struct {
	foreground    foregroundV2
	transcript    *acpv2.Transcript
	shown         int
	dirty         bool
	changed       bool
	cancelSending bool
	promptWritten <-chan struct{}
}

func (m *terminal) conversationV2() *conversationV2 {
	if m.v2 == nil {
		m.v2 = &conversationV2{transcript: acpv2.NewTranscript()}
	}
	return m.v2
}

func (m *terminal) deliverV2(params map[string]any) tea.Cmd {
	conversation := m.conversationV2()
	if m.connection != connected || conversation.foreground.waiting() || m.cancelling || conversation.cancelSending {
		return nil
	}
	conversation.foreground.beginPrompt()
	m.refreshForegroundV2()
	m.outbox = m.outbox[1:]
	m.chatBacklog.browsing = false
	h, ctx := m.h, m.ctx
	prompt := &promptWriteV2{ready: make(chan struct{})}
	h.promptWrite.Store(prompt)
	conversation.promptWritten = prompt.ready
	return func() tea.Msg {
		result, err := acp.SendRequest[struct{ MessageID *string }](h.conn, ctx, "session/prompt", params)
		h.completePromptWrite(prompt)
		if err == nil && result.MessageID == nil {
			err = fmt.Errorf("ACP v2 prompt response requires a messageId")
		}
		h.emitV2(ctx, promptV2Result{err})
		return nil
	}
}

func (m *terminal) cancelForegroundV2() tea.Cmd {
	conversation := m.conversationV2()
	if conversation.cancelSending || m.cancelling {
		return nil
	}
	conversation.cancelSending = true
	m.cancelling = true
	m.refreshForegroundV2()
	written, h, ctx := conversation.promptWritten, m.h, m.ctx
	return func() tea.Msg {
		if written != nil {
			select {
			case <-written:
			case <-ctx.Done():
				return nil
			}
		}
		return cancelV2Result{h.conn.SendNotification(ctx, "session/cancel", map[string]string{"sessionId": h.id})}
	}
}

func (m *terminal) refreshForegroundV2() {
	foreground := &m.conversationV2().foreground
	m.Busy = foreground.waiting()
	switch {
	case m.ask != nil:
		m.Status = "permission required"
	case m.cancelling:
		m.Status = "cancelling"
	case foreground.state == "unknown":
		m.Status = "agent activity unknown"
	case foreground.state == "requires_action":
		m.Status = "waiting for action"
	case foreground.state == "running":
		m.Status = "working"
	case foreground.submitting || foreground.awaitingStart:
		m.Status = "starting"
	default:
		m.Status = "ready"
	}
}

func (m *terminal) recordV2Update(update json.RawMessage) {
	conversation := m.conversationV2()
	changed, err := conversation.transcript.Apply(update)
	if err != nil {
		m.recordNote(err.Error())
		return
	}
	conversation.dirty = conversation.dirty || changed
	conversation.changed = conversation.changed || changed
}

// Updates can revise a previously displayed tool or message in place. Keep
// local UI notices after the projected transcript and replace the search rows
// from the same projection when the foreground is idle.
func (m *terminal) publishV2Transcript(checkpoint bool) {
	conversation := m.conversationV2()
	if m.connection != connected || (!conversation.dirty && !checkpoint) {
		return
	}
	busy := conversation.foreground.waiting()
	if busy && !m.cfg.StreamResponses && !checkpoint {
		return
	}
	if busy && !conversation.changed && !checkpoint {
		return
	}
	projected := conversation.transcript.Entries()
	conversation.changed = false
	messages := make([]entry, 0, len(projected)+len(m.Messages)-conversation.shown)
	for _, row := range projected {
		messages = append(messages, entry{row.Role, row.Text})
	}
	messages = append(messages, m.Messages[conversation.shown:]...)
	conversation.shown = len(projected)
	m.Messages = messages
	m.transcriptRevision++
	if !busy || checkpoint {
		conversation.dirty = false
		m.indexMessages(messages[:conversation.shown], true)
	}
}

func promptCancelledV2(err error) bool {
	var rpc *acp.RequestError
	return errors.As(err, &rpc) && rpc.Code == -32800
}
