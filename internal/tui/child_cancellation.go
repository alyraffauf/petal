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
	if p.center.selected != "" && p.center.selected != n.id {
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

type childStatusesChanged struct {
	watch    *sessions.ChildStatusWatch
	statuses map[sessions.ChildBinding]string
	err      error
}

func childIsRunning(status string) bool {
	return status == "running" || status == "cancelling"
}

func (p *picker) hasRunningChild() bool {
	for _, record := range p.saved {
		if childIsRunning(record.DelegationStatus) {
			return true
		}
	}
	return false
}

func (p *picker) watchChildStatuses() tea.Cmd {
	if p.center.cwd == "" || !p.hasRunningChild() {
		return nil
	}
	watch, err := sessions.WatchChildStatuses(p.center.cwd)
	if err != nil {
		p.historyError = err.Error()
		return nil
	}
	p.center.childStatuses = watch
	return nextChildStatuses(watch)
}

func nextChildStatuses(watch *sessions.ChildStatusWatch) tea.Cmd {
	return func() tea.Msg {
		statuses, err := watch.Next()
		return childStatusesChanged{watch, statuses, err}
	}
}

func (p *picker) stopWatchingChildStatuses() {
	if watch := p.center.childStatuses; watch != nil {
		watch.Close()
		p.center.childStatuses = nil
	}
}

func (p *picker) refreshChildStatuses(msg childStatusesChanged) tea.Cmd {
	if p.center.childStatuses != msg.watch {
		return nil
	}
	if msg.err != nil {
		p.stopWatchingChildStatuses()
		p.historyError = msg.err.Error()
		return nil
	}
	for i := range p.saved {
		record := &p.saved[i]
		// A cancellation response may already have applied a newer result.
		if childIsRunning(record.DelegationStatus) {
			binding := sessions.ChildBinding{ParentID: record.DelegatedParent, ChildID: record.ID}
			record.DelegationStatus = msg.statuses[binding]
		}
	}
	if !p.hasRunningChild() {
		p.stopWatchingChildStatuses()
		return nil
	}
	return nextChildStatuses(msg.watch)
}
