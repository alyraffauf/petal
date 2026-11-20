package tui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"petal/internal/sessions"
)

const cancelChildLabel = "Ctrl+X  Cancel child"

type childCancellationResult struct {
	picker  *picker
	binding sessions.ChildBinding
	result  *sessions.ChildResult
	err     error
}

func (p *picker) canCancelChild(n controlNode) bool {
	if p.center.selected != n.id && (p.center.cancelling[p.center.selected] || p.center.cancelOutcome != "") {
		return false
	}
	return n.cancellable
}

func (p *picker) cancelChild(n controlNode) tea.Cmd {
	binding := n.binding
	if binding.ChildID == "" || p.center.cwd == "" || p.center.cancelling[binding.ChildID] {
		return nil
	}
	if p.center.cancelling == nil {
		p.center.cancelling = make(map[string]bool)
	}
	p.center.selected = n.id
	p.center.cancelOutcome = ""
	p.center.cancelling[binding.ChildID] = true
	delete(p.center.cancelErrors, binding.ChildID)
	cwd := p.center.cwd
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		result, err := sessions.CancelChild(ctx, cwd, binding)
		return childCancellationResult{p, binding, result, err}
	}
}

func (p *picker) childCancellationFinished(msg childCancellationResult) {
	delete(p.center.cancelling, msg.binding.ChildID)
	if msg.result != nil {
		for i, record := range p.saved {
			if record.ID == msg.binding.ChildID && record.DelegatedParent == msg.binding.ParentID {
				p.saved[i].DelegationStatus = msg.result.Status
				title := record.Title
				if title == "" {
					title = "Child"
				}
				if p.center.selected == record.ID {
					p.center.cancelOutcome = petalLabel(title + " · " + msg.result.Status)
				}
				break
			}
		}
	}
	if msg.err == nil {
		return
	}
	if p.center.cancelErrors == nil {
		p.center.cancelErrors = make(map[string]string)
	}
	message := "Could not cancel child: " + msg.err.Error()
	if errors.Is(msg.err, context.DeadlineExceeded) {
		message = "Cancellation unconfirmed."
	}
	p.center.cancelErrors[msg.binding.ChildID] = message
}
