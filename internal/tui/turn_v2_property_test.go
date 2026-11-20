package tui

import (
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"
)

func FuzzV2ForegroundHistory(f *testing.F) {
	// Fast local completion before acknowledgment, early acknowledgment,
	// rejected pre-insertion cancellation, and independently initiated work.
	f.Add([]byte{0, 2, 4, 0, 6, 2, 4, 6})
	f.Add([]byte{0, 6, 4, 0, 3, 8, 4, 9})
	f.Add([]byte{0, 10, 0, 4, 7, 11, 2, 4, 6})
	f.Add([]byte{2, 0, 5, 4, 2, 3, 4, 6})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 512 {
			operations = operations[:512]
		}
		h := &harness{events: make(chan tea.Msg, 1)}
		h.v2.Store(true)
		m := &terminal{root: bloom.New(), h: h, ctx: t.Context()}
		m.Update(opened{})
		const (
			post = iota
			reportIdle
			reportRunning
			reportAction
			reportDone
			reportUnknown
			accept
			reject
			ask
			dismiss
			cancel
			cancelSent
		)
		var trace []int
		var submitted, queued int
		var question *permission
		var permissionVisible, cancellationWriting bool
		var cancelAt int
		for index, raw := range operations {
			op := int(raw % 12)
			step := index + 1
			activity, unsettled, awaiting, _ := summarizeForegroundHistory(trace, submitted)
			switch op {
			case post:
				queued++
				m.outbox = append(m.outbox, promptMessage{text: "queued"})
				m.deliver()
			case reportIdle, reportRunning, reportAction, reportDone, reportUnknown:
				// reportIdle is an unsolicited repeat. Generate it only
				// when no known foreground work has been reported.
				if op == reportIdle && activity != "idle" {
					op = -1
					break
				}
				// reportDone marks the observed end, including recovery from
				// an unobservable state. Duplicate idle reports are allowed.
				state := map[int]string{
					reportIdle: "idle", reportRunning: "running", reportAction: "requires_action",
					reportDone: "idle", reportUnknown: "unknown",
				}[op]
				m.Update(updateV2{SessionUpdate: "state_update", State: state})
			case accept, reject:
				if !unsettled {
					op = -1
					break
				}
				var err error
				if op == reject {
					err = acp.NewRequestError(-32800, "Request cancelled", nil)
				}
				m.Update(promptV2Result{err})
			case ask:
				if permissionVisible {
					break
				}
				permissionVisible = true
				question = &permission{reply: make(chan int, 1)}
				m.Update(event{ask: question})
			case dismiss:
				permissionVisible = false
				m.Update(dismissedQuestion{question})
			case cancel:
				if (activity != "idle" || unsettled || awaiting) && !permissionVisible && !cancellationWriting && cancelAt == 0 {
					if m.cancelForegroundV2() == nil {
						t.Fatalf("step %d: cancellation refused for %v", index, trace)
					}
					cancelAt, cancellationWriting = step, true
				}
			case cancelSent:
				if cancellationWriting {
					cancellationWriting = false
					m.Update(cancelV2Result{})
				}
			}
			trace = append(trace, op)
			activity, unsettled, awaiting, foregroundFinished := summarizeForegroundHistory(trace, submitted)
			if cancelAt != 0 && ((!awaiting && foregroundFinished == step && foregroundFinished >= cancelAt) ||
				(op == reject && activity == "idle")) {
				cancelAt = 0
			}
			busy := activity != "idle" || unsettled || awaiting
			if !busy && !cancellationWriting && cancelAt == 0 && !permissionVisible && queued > 0 {
				submitted = step
				queued--
				busy = true
			}
			if len(m.outbox) != queued || m.Busy != busy || (m.ask != nil) != permissionVisible || m.cancelling != (cancelAt != 0) {
				t.Fatalf("step %d trace %v: queue=%d busy=%t permission=%t cancelling=%t; want %d %t %t %t",
					index, trace, len(m.outbox), m.Busy, m.ask != nil, m.cancelling,
					queued, busy, permissionVisible, cancelAt != 0)
			}
			if len(m.Messages) != 0 {
				t.Fatalf("step %d: lifecycle alone produced transcript entries: %v", index, m.Messages)
			}
		}
	})
}

// Derive the oracle from positions in the history. A submission occupies a
// boundary after the event that dispatched it.
func summarizeForegroundHistory(trace []int, submitted int) (activity string, unsettled, awaiting bool, lastIdle int) {
	activity = "idle"
	var lastActive, lastResponse int
	var rejected bool
	for i, op := range trace {
		step := i + 1
		switch op {
		case 1, 4: // idle or completion
			activity, lastIdle = "idle", step
		case 2: // running
			activity, lastActive = "running", step
		case 3: // requires_action
			activity, lastActive = "requires_action", step
		case 5: // unknown
			activity = "unknown"
		case 6, 7:
			lastResponse, rejected = step, op == 7
		}
	}
	unsettled = submitted > 0 && lastResponse <= submitted
	awaiting = submitted > 0 && lastActive <= submitted && (lastResponse <= submitted || !rejected)
	return
}
