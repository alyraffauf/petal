// Package delegation runs routed child sessions and exposes their journal to their parent.
package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"petal/internal/sessions"
)

// The connection is initialized and routed to the requested model. It delivers
// updates serially and drains preceding updates before delivering a response or closing Done().
type Connection interface {
	Version() uint16
	Request(context.Context, string, any) (json.RawMessage, error)
	Notify(context.Context, string, any) error
	Done() <-chan struct{}
	Close()
}

type Receive func(ctx context.Context, method string, params json.RawMessage, notification bool) (any, error)
type OpenChild func(ctx context.Context, model string, receive Receive) (Connection, error)
type Permission func(ctx context.Context, parentID, childID string, version uint16, params json.RawMessage) (json.RawMessage, error)

type Child struct {
	Done  <-chan struct{} `json:"-"`
	ID    string          `json:"childId"`
	Model string          `json:"model"`
}

type Page = sessions.ChildActivityPage

type Runtime struct {
	lifetime   context.Context
	open       OpenChild
	permission Permission

	mu sync.Mutex

	runs map[*run]struct{}
}

func New(lifetime context.Context, open OpenChild, permission Permission) *Runtime {
	return &Runtime{
		lifetime: lifetime, open: open, permission: permission,
		runs: make(map[*run]struct{}),
	}
}

func (r *Runtime) Delegate(ctx context.Context, parentID, cwd, model, task string) (Child, error) {
	if parentID == "" || cwd == "" || strings.TrimSpace(model) == "" || strings.TrimSpace(task) == "" {
		return Child{}, errors.New("parent, working directory, model, and task are required")
	}
	if r.open == nil {
		return Child{}, errors.New("child routing is unavailable")
	}

	work, cancel := context.WithCancel(r.lifetime)
	child := &run{runtime: r, cwd: cwd, parentID: parentID, ctx: work, cancel: cancel, ready: make(chan struct{})}
	r.mu.Lock()
	r.runs[child] = struct{}{}
	r.mu.Unlock()

	transport, closeTransport := context.WithCancel(context.WithoutCancel(work))
	stopWork := context.AfterFunc(work, closeTransport)
	stopRequest := context.AfterFunc(ctx, closeTransport)
	defer func() {
		stopWork()
		stopRequest()
	}()
	started := false
	defer func() {
		if !started {
			closeTransport()
			r.remove(child)
		}
	}()

	parent, err := sessions.Lookup(cwd, parentID)
	if err != nil {
		return Child{}, err
	}
	if parent.ID == "" {
		return Child{}, sessions.ErrChildNotFound
	}
	if err := work.Err(); err != nil {
		return Child{}, err
	}
	conn, err := r.open(transport, model, child.receive)
	if err != nil {
		return Child{}, err
	}
	defer func() {
		if !started {
			conn.Close()
		}
	}()
	version := conn.Version()
	if version != 1 && version != 2 {
		return Child{}, fmt.Errorf("unsupported child ACP version %d", version)
	}
	params := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	raw, err := conn.Request(transport, "session/new", params)
	if err != nil {
		return Child{}, err
	}
	var created struct {
		ID *string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &created) != nil || created.ID == nil || *created.ID == "" || *created.ID == parentID {
		return Child{}, errors.New("adapter returned an invalid child session ID")
	}
	if err := transport.Err(); err != nil {
		return Child{}, err
	}
	if err := ctx.Err(); err != nil {
		return Child{}, err
	}
	// From this point a parent cancellation ends the run, while a cancelled
	// tool request cannot tear down the connection after its child is bound.
	if !stopRequest() || !stopWork() {
		return Child{}, context.Canceled
	}
	binding := sessions.ChildBinding{ParentID: parentID, ChildID: *created.ID}
	record := sessions.Record{ID: binding.ChildID, Model: model, Opened: time.Now()}
	if routed, ok := conn.(interface{ AccountName() string }); ok {
		record.Account = routed.AccountName()
	}
	release, err := sessions.StartChild(work, cwd, binding, record, task)
	if err != nil {
		return Child{}, err
	}
	child.binding, child.version = binding, version
	close(child.ready)
	started = true
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer r.remove(child)
		defer release()
		defer closeTransport()
		defer conn.Close()
		child.execute(conn, task, closeTransport)
	}()
	return Child{ID: binding.ChildID, Model: model, Done: done}, nil
}

func (r *Runtime) remove(child *run) {
	child.cancel()
	r.mu.Lock()
	delete(r.runs, child)
	r.mu.Unlock()
}

func (r *Runtime) CancelParent(parentID, cwd string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for child := range r.runs {
		if child.parentID == parentID && child.cwd == cwd {
			child.cancel()
		}
	}
}

func (r *Runtime) Read(ctx context.Context, parentID, cwd, childID string, after int64) (Page, error) {
	if cwd == "" || childID == "" {
		return Page{}, sessions.ErrChildNotFound
	}
	if after < 0 {
		return Page{}, errors.New("child cursor must not be negative")
	}
	return sessions.ReadChildActivity(ctx, cwd, sessions.ChildBinding{ParentID: parentID, ChildID: childID}, after, 100)
}

func (r *Runtime) Wait(ctx context.Context, parentID, cwd, childID string, after int64, timeout time.Duration) (Page, error) {
	if timeout <= 0 {
		return r.Read(ctx, parentID, cwd, childID, after)
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Check durable state even if a different connection owns the running child.
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	for {
		page, err := r.Read(bounded, parentID, cwd, childID, after)
		if err != nil || len(page.Activities) > 0 || page.Result != nil {
			return page, err
		}
		select {
		case <-ctx.Done():
			return Page{}, ctx.Err()
		case <-r.lifetime.Done():
			return Page{}, r.lifetime.Err()
		case <-bounded.Done():
			return page, ctx.Err()
		case <-poll.C:
		}
	}
}

func (r *run) finish(result sessions.ChildResult) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Second)
	defer cancel()
	if err := sessions.FinishChild(ctx, r.cwd, r.binding, result); err != nil {
		slog.Error("Could not record child result", "childId", r.binding.ChildID, "error", err)
	}
}
