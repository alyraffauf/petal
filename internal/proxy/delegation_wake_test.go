package proxy

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestSupersededWakeDoesNotReachAdapter(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			for _, clear := range []bool{false, true} {
				e := &delegationEndpoint{ctx: t.Context()}
				e.wakes.children = map[string]bool{"child": true}
				wait, cancel := e.waitContext(e.ctx)
				ids := e.wakes.next(wait)
				var adapter bytes.Buffer
				wire := &adapterWire{Writer: &adapter}
				wire.wakes.Store("1", &wakeSubmission{
					queue: &e.wakes, ctx: wait, endpoint: e.ctx, native: "parent",
					ids: ids, consumeOnWrite: version == "v2",
				})
				e.wakes.interrupt(clear)
				_, err := wire.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"\u0000petal/wake/1"}` + "\n"))
				cancel()
				if !errors.Is(err, context.Canceled) || adapter.Len() != 0 {
					t.Fatalf("superseded wake wrote %d bytes, error %v", adapter.Len(), err)
				}
				if e.wakes.children["child"] == clear {
					t.Fatalf("pending child after interrupt(clear=%t): %v", clear, e.wakes.children)
				}
			}
		})
	}
}
