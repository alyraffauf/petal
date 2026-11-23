//go:build linux || darwin

package livesession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Client multiplexes control requests with a single event reader on an
// endpoint connection. Its Close releases the attachment and controller lease.
type Client struct {
	instance string
	conn     net.Conn
	writes   chan struct{}
	slots    chan struct{}
	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan response
	done     chan struct{}
	readErr  error
}

// Connect negotiates the local protocol on a connection from localdaemon.
// It takes ownership of conn, including on handshake failure.
func Connect(ctx context.Context, conn net.Conn) (*Client, error) {
	client := &Client{
		conn: conn, writes: make(chan struct{}, 1), slots: make(chan struct{}, 16),
		pending: make(map[uint64]chan response), done: make(chan struct{}),
	}
	go client.read()
	ctx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()
	var hello handshake
	if err := client.call(ctx, request{Method: "hello", Version: ProtocolVersion}, &hello); err != nil {
		client.Close()
		return nil, err
	}
	if hello.Version != ProtocolVersion || hello.Instance == "" {
		client.Close()
		return nil, failure(Version, "daemon returned an unexpected protocol version")
	}
	client.instance = hello.Instance
	return client, nil
}

// Instance identifies the daemon process. Retry an ambiguous open or call
// only against the same instance. After replacement, use the durable ID to
// reopen the archive instead.
func (c *Client) Instance() string { return c.instance }

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) read() {
	reader := readFrames(c.conn)
	for {
		var res response
		if err := reader.read(&res); err != nil {
			c.readErr = err
			close(c.done)
			c.conn.Close()
			return
		}
		c.mu.Lock()
		wait := c.pending[res.ID]
		delete(c.pending, res.ID)
		c.mu.Unlock()
		if wait != nil {
			wait <- res
		}
	}
}

func (c *Client) write(ctx context.Context, req request, reply chan response) (uint64, error) {
	select {
	case c.writes <- struct{}{}:
		defer func() { <-c.writes }()
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.done:
		return 0, c.readErr
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// IDs are assigned in wire order, including when callers run concurrently.
	c.mu.Lock()
	c.nextID++
	req.ID = c.nextID
	c.pending[req.ID] = reply
	c.mu.Unlock()
	deadline := time.Now().Add(attachTimeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	if err := writeFrame(c.conn, req, deadline); err != nil {
		// A partially written frame cannot be retried on this connection.
		c.conn.Close()
		return req.ID, err
	}
	return req.ID, nil
}

func (c *Client) call(ctx context.Context, req request, out any) error {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.readErr
	}
	wait := make(chan response, 1)
	id, err := c.write(ctx, req, wait)
	if id != 0 {
		defer func() {
			c.mu.Lock()
			delete(c.pending, id)
			c.mu.Unlock()
		}()
	}
	if err != nil {
		return err
	}
	var res response
	select {
	case res = <-wait:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		// A final response can arrive just before EOF.
		select {
		case res = <-wait:
		default:
			return c.readErr
		}
	}
	if res.Error != nil {
		return res.Error
	}
	if out != nil {
		if err := json.Unmarshal(res.Result, out); err != nil {
			return fmt.Errorf("decode %s reply: %w", req.Method, err)
		}
	}
	return nil
}

func rpc[T any](ctx context.Context, c *Client, method string, params any) (T, error) {
	var result T
	raw, err := json.Marshal(params)
	if err != nil {
		return result, err
	}
	limit := maxBody
	if method == "call" {
		limit = maxPromptBody
	}
	if len(raw) > limit+4096 {
		return result, failure(Limit, "request exceeds daemon limit")
	}
	err = c.call(ctx, request{Method: method, Params: raw}, &result)
	return result, err
}

func (c *Client) Open(ctx context.Context, opts OpenOptions) (Info, error) {
	return rpc[Info](ctx, c, "open", opts)
}

func (c *Client) List(ctx context.Context) ([]Info, error) {
	return rpc[[]Info](ctx, c, "list", nil)
}

func (c *Client) Attach(ctx context.Context, opts AttachOptions) (Snapshot, error) {
	return rpc[Snapshot](ctx, c, "attach", opts)
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	return rpc[Snapshot](ctx, c, "snapshot", nil)
}

func (c *Client) Claim(ctx context.Context) (Snapshot, error) {
	return rpc[Snapshot](ctx, c, "claim", nil)
}

// Next waits for the event immediately after after. Advance the cursor only
// once the frontend has applied the event. One goroutine should call Next.
func (c *Client) Next(ctx context.Context, after uint64) (Event, error) {
	for {
		event, err := rpc[Event](ctx, c, "next", after)
		if err == nil && event.Sequence == 0 {
			continue
		}
		var protocolErr *Error
		if !errors.As(err, &protocolErr) || protocolErr.Code != Busy {
			return event, err
		}
		// A prior Next may have lost its caller before its server-side
		// heartbeat expired. Retrying with the same cursor is safe.
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Event{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) Submit(ctx context.Context, call Call) (uint64, error) {
	return rpc[uint64](ctx, c, "call", call)
}

func (c *Client) Result(ctx context.Context, operation uint64) (Receipt, error) {
	return rpc[Receipt](ctx, c, "result", operation)
}

// WaitResult waits for an operation receipt independently of Next. Canceling
// the wait leaves the accepted operation running.
func (c *Client) WaitResult(ctx context.Context, operation uint64) (Receipt, error) {
	for {
		receipt, err := rpc[Receipt](ctx, c, "wait_result", operation)
		if err != nil || receipt.Done {
			return receipt, err
		}
	}
}

func (c *Client) Answer(ctx context.Context, answer Answer) error {
	_, err := rpc[bool](ctx, c, "answer", answer)
	return err
}

// ConfirmSettings releases the resumed-session prompt gate for the reviewed snapshot.
func (c *Client) ConfirmSettings(ctx context.Context, revision uint64) error {
	_, err := rpc[bool](ctx, c, "confirm_settings", revision)
	return err
}

func (c *Client) Cancel(ctx context.Context, turn uint64) error {
	_, err := rpc[bool](ctx, c, "cancel", turn)
	return err
}

// TerminalOutput returns the current output for an adapter-owned terminal.
// On release the daemon publishes its last output as an Update instead.
func (c *Client) TerminalOutput(ctx context.Context, ref TerminalRef) (acp.TerminalOutputResponse, error) {
	return rpc[acp.TerminalOutputResponse](ctx, c, "terminal/output", ref)
}

func (c *Client) CloseSession(ctx context.Context) error {
	_, err := rpc[any](ctx, c, "close", nil)
	return err
}
