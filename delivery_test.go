package main

import (
	"testing"

	"bloomfx.dev/bloom"
)

func TestBusyInputAtTurnBoundary(t *testing.T) {
	for _, outcome := range []string{"injected", "promptRequired"} {
		for _, finishFirst := range []bool{false, true} {
			m := terminal{root: bloom.New(), h: &harness{supportsSteering: true}, screen: screen{connection: connected, Busy: true, editor: testComposer("adjust", 6)}}
			if cmd := m.submit(); cmd == nil || !m.steering {
				t.Fatal("default input did not steer")
			}
			if finishFirst {
				m.Update(finished{})
			}
			m.Update(steerResult{outcome: outcome})
			if !finishFirst {
				m.Update(finished{})
			}
			if len(m.outbox) != 0 || len(m.Messages) != 1 || m.Messages[0] != (entry{"user", "adjust"}) || m.Busy != (outcome == "promptRequired") {
				t.Fatalf("finishFirst=%t outcome=%s transcript=%v queued=%v busy=%t", finishFirst, outcome, m.Messages, m.outbox, m.Busy)
			}
		}
	}
}

func TestBusyFollowUpsStayInOrder(t *testing.T) {
	for _, cfg := range []config{{BusyInput: "follow_up"}, {}} {
		m := terminal{root: bloom.New(), cfg: cfg, h: &harness{supportsSteering: cfg.BusyInput == "follow_up"}, screen: screen{connection: connected, Busy: true}}
		for _, text := range []string{"one", "two"} {
			m.editor = testComposer(text, len(text))
			if cmd := m.submit(); cmd != nil {
				t.Fatal("sent before turn finished")
			}
		}
		for i, want := range []string{"one", "two"} {
			m.Update(finished{})
			if !m.Busy || len(m.Messages) != i+1 || m.Messages[i].Text != want {
				t.Fatal(m.Messages)
			}
		}
	}
}
