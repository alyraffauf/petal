package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestSupersededWakeDoesNotReachAdapter(t *testing.T) {
	for _, clear := range []bool{false, true} {
		e := &delegationEndpoint{ctx: t.Context()}
		e.wakes.children = map[string]bool{"child": true}
		wait, cancel := e.waitContext()
		ids := e.wakes.next(wait, false)
		submission := wakeSubmission{&e.wakes, wait, "parent", ids}
		e.wakes.interrupt(clear)
		var adapter bytes.Buffer
		_, err := submission.write(&adapter, Object{}, 1)
		cancel()
		if !errors.Is(err, context.Canceled) || adapter.Len() != 0 {
			t.Fatalf("superseded wake wrote %d bytes, error %v", adapter.Len(), err)
		}
		if e.wakes.children["child"] == clear {
			t.Fatalf("pending child after interrupt(clear=%t): %v", clear, e.wakes.children)
		}
	}
}
