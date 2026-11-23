package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
)

// Local is a model-bound proxy owned by a daemon session. Requests come from
// the session owner. The internal ACP connection serves adapter callbacks for
// the whole session lifetime, including when no frontend is attached.
// The owner archives ACP v2 updates alongside its journal.
type Local struct {
	proxy *proxy
}

// NewLocal starts the callback connection. update runs inline with notification
// writes, so updates preceding an ACP response are visible before Call returns.
// update must not call back into the proxy or wait on a frontend. reserveRoute
// runs before a newly returned route can record transcripts or bind delegation.
func NewLocal(ctx context.Context, config routing.Config, cwd string, callback acp.MethodHandler, update func(string, json.RawMessage), reserveRoute func(string) error) *Local {
	editor, host := net.Pipe()
	context.AfterFunc(ctx, func() {
		editor.Close()
		host.Close()
	})
	client := acp.NewConnection(callback, host, host)
	client.SetLogger(slog.New(slog.DiscardHandler))
	p := connectProxy(ctx, localOutput{editor, update}, editor, config, true, nil, &localRuntime{cwd: cwd, reserveRoute: reserveRoute})
	return &Local{proxy: p}
}

type localRuntime struct {
	reserveRoute func(string) error
	cwd          string
	mu           sync.Mutex
	stopping     bool
	active       int
	children     sync.WaitGroup
	next         atomic.Uint64
	dispatch     sync.Map
}

func (p *proxy) reserveLocalRoute(id string) error {
	if p.local == nil || p.local.reserveRoute == nil {
		return nil
	}
	return p.local.reserveRoute(id)
}

func (r *localRuntime) reserve() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return context.Canceled
	}
	const maxAdapters = 16
	if r.active >= maxAdapters {
		return errors.New("live session adapter limit reached")
	}
	r.active++
	r.children.Add(1)
	return nil
}

func (r *localRuntime) release() {
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	r.children.Done()
}

// Wait waits for adapter processes after the session context has been canceled.
func (l *Local) Wait() {
	p := l.proxy
	p.local.mu.Lock()
	p.local.stopping = true
	p.local.mu.Unlock()
	p.local.children.Wait()
	p.recorder.flush()
}

// Call dispatches an ACP request. If supplied, dispatched runs after the
// adapter prompt write, so a concurrent session/cancel cannot overtake it.
func (l *Local) Call(ctx context.Context, method string, params json.RawMessage, dispatched func()) (any, error) {
	p := l.proxy
	if dispatched != nil {
		id := strconv.FormatUint(p.local.next.Add(1), 10)
		p.local.dispatch.Store(id, dispatched)
		defer p.local.dispatch.Delete(id)
		ctx = context.WithValue(ctx, editorRequestID{}, json.RawMessage(id))
	}
	return p.handle(ctx, method, params)
}

func (l *Local) Cancel(ctx context.Context, params json.RawMessage) error {
	_, err := l.proxy.handle(ctx, notificationPrefix+"session/cancel", params)
	return err
}

// SessionDone reports when the adapter owning a routed session disconnects.
func (l *Local) SessionDone(id string) <-chan struct{} {
	p := l.proxy
	route := DecodeSessionRoute(id)
	if route == nil {
		return p.lifetime.Done()
	}
	account := route[0]
	if p.PinnedAccount != "" {
		account = p.PinnedAccount
	}
	p.mu.Lock()
	backend := p.backends[account]
	p.mu.Unlock()
	if backend == nil {
		return p.lifetime.Done()
	}
	return backend.conn.Done()
}

type localOutput struct {
	io.Writer
	update func(string, json.RawMessage)
}

func (w localOutput) Write(line []byte) (int, error) {
	var header struct {
		ID     json.RawMessage
		Method string
		Params json.RawMessage
	}
	if json.Unmarshal(line, &header) == nil && header.ID == nil && header.Method != "" && header.Method != "$/cancel_request" {
		w.update(header.Method, header.Params)
		return len(line), nil
	}
	return w.Writer.Write(line)
}
