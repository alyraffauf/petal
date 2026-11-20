package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"

	acp "github.com/coder/acp-go-sdk"
	"petal/internal/delegation"
)

// Each live parent owns one queue. Revoking the endpoint also revokes its
// pending wakeups. The durable journal remains readable on a later connection.
type wakeQueue struct {
	mu       sync.Mutex
	children map[string]bool
	changed  chan struct{}
	stop     context.CancelFunc
}

func (q *wakeQueue) signal() {
	if q.changed != nil {
		close(q.changed)
	}
	q.changed = make(chan struct{})
}

func (e *delegationEndpoint) track(child delegation.Child) {
	q := &e.wakes
	q.mu.Lock()
	if q.children == nil {
		q.children = make(map[string]bool)
	}
	q.children[child.ID] = false
	q.signal()
	q.mu.Unlock()
	go func() {
		select {
		case <-e.ctx.Done():
			return
		case <-child.Done:
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if _, exists := q.children[child.ID]; exists {
			q.children[child.ID] = true
			q.signal()
		}
	}()
}

func (q *wakeQueue) forget(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.children, id)
	q.signal()
}

func (q *wakeQueue) interrupt(clear bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stop != nil {
		q.stop()
		q.stop = nil
	}
	if clear {
		q.children = nil
		q.signal()
	}
}

func (e *delegationEndpoint) waitContext() (context.Context, context.CancelFunc) {
	q := &e.wakes
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stop != nil {
		q.stop()
	}
	ctx, cancel := context.WithCancel(e.ctx)
	q.stop = cancel
	return ctx, cancel
}

func (q *wakeQueue) next(ctx context.Context, consume bool) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	for ctx.Err() == nil && len(q.children) > 0 {
		var ready []string
		for id, done := range q.children {
			if done {
				ready = append(ready, id)
				if consume {
					delete(q.children, id)
				}
			}
		}
		if len(ready) > 0 {
			slices.Sort(ready)
			return ready
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		q.mu.Lock()
	}
	return nil
}

func wakePrompt(native string, ids []string) Object {
	data, _ := json.Marshal(ids)
	text := fmt.Sprintf("Petal delegation update: these child runs have ended: %s. Call petal_read with each child_id to retrieve its result and continue your original task.", data)
	prompt, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	return Object{"sessionId": encoded(native), "prompt": prompt}
}

func (p *proxy) endpoint(parent string) *delegationEndpoint {
	p.delegationMu.Lock()
	defer p.delegationMu.Unlock()
	return p.delegationEndpoints[parent]
}

// V1 keeps the editor's prompt open across continuation turns so that its
// reply and every session/update still belong to the original user turn.
func (e *delegationEndpoint) continueV1(wait context.Context, b *backend, native string, result json.RawMessage) (json.RawMessage, error) {
	for {
		var end struct{ StopReason string }
		if json.Unmarshal(result, &end) != nil || end.StopReason != "end_turn" {
			return result, nil
		}
		ids := e.wakes.next(wait, true)
		if len(ids) == 0 {
			return result, nil
		}
		var err error
		result, err = acp.SendRequest[json.RawMessage](b.conn, wait, "session/prompt", wakePrompt(native, ids))
		if err != nil {
			return nil, err
		}
	}
}

// V2 acknowledges insertion immediately. Only schedule the follow-up once
// the adapter has reported idle; subsequent user input supersedes the wait.
func (e *delegationEndpoint) idleV2(wait context.Context, cancel context.CancelFunc, b *backend, native string) {
	go func() {
		defer cancel()
		ids := e.wakes.next(wait, false)
		if len(ids) == 0 {
			return
		}
		token := strconv.FormatUint(b.wire.nextRequest.Add(1), 10)
		b.wire.wakes.Store(token, &wakeSubmission{&e.wakes, wait, native, ids})
		defer b.wire.wakes.Delete(token)
		if _, err := acp.SendRequest[json.RawMessage](b.conn, wait, wakePrefix+token, nil); err != nil && wait.Err() == nil {
			slog.Warn("Could not wake delegation parent", "error", err)
		}
	}()
}

// The SDK writes before checking the request context. Check ownership at the
// wire, holding the queue lock only through submission, never through the reply.
type wakeSubmission struct {
	queue  *wakeQueue
	ctx    context.Context
	native string
	ids    []string
}

func (s *wakeSubmission) write(out io.Writer, frame Object, originalLen int) (int, error) {
	q := s.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	var ready []string
	for _, id := range s.ids {
		if q.children[id] {
			ready = append(ready, id)
		}
	}
	if len(ready) == 0 {
		return 0, context.Canceled
	}
	frame["method"] = encoded("session/prompt")
	frame["params"], _ = json.Marshal(wakePrompt(s.native, ready))
	data, err := json.Marshal(frame)
	if err != nil {
		return 0, err
	}
	if _, err = out.Write(append(data, '\n')); err != nil {
		return 0, err
	}
	for _, id := range ready {
		delete(q.children, id)
	}
	q.stop = nil // The following running update belongs to this accepted prompt.
	return originalLen, nil
}
