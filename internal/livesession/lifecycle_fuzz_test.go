//go:build linux || darwin

package livesession

import (
	"fmt"
	"strings"
	"testing"
)

func FuzzSessionAttachmentLifecycle(f *testing.F) {
	f.Add([]byte{0, 64, 129, 2, 3, 65, 128, 1, 64, 192})
	f.Add([]byte(strings.Repeat("\x80", 4100) + "\x00\xa0\xa1\x40\xc0\x80"))
	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) > 5000 {
			script = script[:5000]
		}
		live := newSession(NewServer(t.Context(), nil), OpenOptions{Handle: "fixture"})
		defer live.stop()
		clients := [2]*peer{{}, {}}
		attached := [2]bool{}
		cursors := [2]uint64{}
		controller := -1
		closed := false
		var updates []string
		for step, action := range script {
			client := int(action & 1)
			p := clients[client]
			switch action >> 5 {
			case 0, 1:
				if attached[client] {
					continue
				}
				control := action>>5 == 0
				var after *uint64
				if action&2 != 0 {
					cursor := cursors[client]
					after = &cursor
				}
				head := uint64(len(updates))
				floor := head - min(head, 4096)
				want := Code("")
				switch {
				case closed:
					want = Closed
				case after != nil && *after < floor:
					want = Gap
				case control && controller >= 0:
					want = Conflict
				}
				snapshot, err := live.attach(p, AttachOptions{After: after, Control: control})
				assertCode(t, step, err, want)
				if want == "" {
					attached[client] = true
					if control {
						controller = client
					}
					if after == nil {
						cursors[client] = floor
					}
					if snapshot.Head != head || snapshot.Floor != floor || snapshot.After != cursors[client] || snapshot.Control != control {
						t.Fatalf("step %d: attachment snapshot = %+v", step, snapshot)
					}
				}
			case 2:
				if attached[client] {
					live.detach(p)
					attached[client] = false
					if controller == client {
						controller = -1
					}
				}
			case 3:
				if !attached[client] {
					continue
				}
				want := Code("")
				if closed {
					want = Closed
				} else if controller >= 0 && controller != client {
					want = Conflict
				}
				snapshot, err := live.claim(p)
				assertCode(t, step, err, want)
				if want == "" {
					controller = client
					if !snapshot.Control {
						t.Fatalf("step %d: claim omitted control", step)
					}
				}
			case 4:
				payload := fmt.Sprintf("%d", step)
				live.update("fixture", bytesOf(payload))
				if !closed {
					updates = append(updates, payload)
				}
			case 5:
				if !attached[client] {
					continue
				}
				cursor := cursors[client]
				head := uint64(len(updates))
				if cursor == head && !closed {
					continue
				}
				event, err := live.next(t.Context(), cursor)
				switch {
				case head-cursor > 4096:
					assertCode(t, step, err, Gap)
					if err.Floor != head-4096 {
						t.Fatalf("step %d: gap boundary = %d", step, err.Floor)
					}
				case cursor == head:
					assertCode(t, step, err, Closed)
				default:
					assertCode(t, step, err, "")
					if event.Sequence != cursor+1 {
						t.Fatalf("step %d: sequence = %d, want %d", step, event.Sequence, cursor+1)
					}
					if updates[cursor] == "closed" {
						if event.Kind != SessionClosed {
							t.Fatalf("step %d: terminal event = %+v", step, event)
						}
					} else if string(event.Params) != fmt.Sprintf("%q", updates[cursor]) {
						t.Fatalf("step %d: payload = %s, want %q", step, event.Params, updates[cursor])
					}
					cursors[client]++
				}
			case 6, 7:
				// Closing is rarer than the other actions so longer histories
				// exercise handover, cursor reuse, and retention.
				if action&30 != 0 || !attached[client] {
					continue
				}
				want := Code("")
				if controller != client {
					want = ReadOnly
					if closed {
						want = Closed
					}
				}
				assertCode(t, step, live.closeBy(p), want)
				if want == "" && !closed {
					closed = true
					updates = append(updates, "closed")
				}
			}
		}
	})
}

func assertCode(t *testing.T, step int, err *Error, want Code) {
	t.Helper()
	if want == "" && err == nil || err != nil && err.Code == want {
		return
	}
	t.Fatalf("step %d: error = %+v, want code %q", step, err, want)
}
