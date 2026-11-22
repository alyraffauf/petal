package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/livesession"
	"petal/internal/proxy"
	"petal/internal/routing"
	"petal/internal/sessions"
)

type DaemonDialer func(context.Context) (*livesession.Client, error)

type daemonSession struct {
	client *livesession.Client
	dial   DaemonDialer
	handle string

	// Serialize operation allocation with submission. Cancellation targets a
	// specific accepted operation even if a later prompt starts.
	mu   sync.Mutex
	next uint64
	turn atomic.Uint64

	// Terminal releases are journaled. A tool update can also refer to a
	// terminal which still exists on the host.
	outputs sync.Map
}

func daemonError(err error, code livesession.Code) bool {
	var failure *livesession.Error
	return errors.As(err, &failure) && failure.Code == code
}

func sendRequest[T any](h *harness, ctx context.Context, method string, params any) (T, error) {
	if h.daemon == nil {
		return acp.SendRequest[T](h.conn, ctx, method, params)
	}
	var value T
	number, err := h.daemon.submit(ctx, method, params)
	if err != nil {
		return value, err
	}
	receipt, err := h.daemon.client.WaitResult(ctx, number)
	if err != nil {
		return value, err
	}
	if err := receipt.Err(); err != nil {
		return value, err
	}
	return value, json.Unmarshal(receipt.Result, &value)
}

func (d *daemonSession) submit(ctx context.Context, method string, params any) (uint64, error) {
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return 0, err
		}
	}
	if string(raw) == "null" {
		raw = nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	number, err := d.client.Submit(ctx, livesession.Call{Operation: d.next, Method: method, Params: raw})
	if err == nil {
		d.next = number + 1
		if method == "session/prompt" {
			d.turn.Store(number)
		}
	}
	return number, err
}

func (h *harness) cancelAction() func(context.Context) error {
	d := h.daemon
	if d == nil {
		return func(ctx context.Context) error {
			return h.conn.SendNotification(ctx, "session/cancel", map[string]string{"sessionId": h.id})
		}
	}
	turn := d.turn.Load()
	written := h.promptWrite.Load()
	return func(ctx context.Context) error {
		number := turn
		if written != nil {
			select {
			case <-written.ready:
				number = written.operation
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if number == 0 {
			return nil
		}
		return d.client.Cancel(ctx, number)
	}
}

func (h *harness) openDaemon(ctx context.Context, c routing.Config, cwd string, resume bool) (string, error) {
	d := h.daemon
	h.cwd = cwd
	if resume && h.id == "" {
		record, err := sessions.Latest(cwd, c.Model)
		if err != nil {
			return "", err
		}
		h.id = record.ID
	}
	if d.handle == "" && h.id != "" {
		list, err := d.client.List(ctx)
		if err != nil {
			return "", err
		}
		for _, live := range list {
			if live.Cwd == cwd && (live.SessionID == h.id || live.ResumeID == h.id) {
				if c.PinnedAccount != "" && live.PinnedAccount != c.PinnedAccount {
					return "", errors.New("this conversation is live on another subscription. Close it before changing subscriptions")
				}
				d.handle = live.Handle
				break
			}
		}
	}

	snapshot, err := d.attach(ctx, cwd, c, h.id)
	if err != nil {
		if err == reopenAfterExit {
			h.id = snapshot.SessionID
		}
		return "", err
	}
	d.next = snapshot.NextOperation
	d.turn.Store(snapshot.Turn)
	established := len(snapshot.Setup) != 0
	if !established {
		if err := h.setupDaemon(ctx, c, snapshot); err != nil {
			h.discardFailedSession(ctx)
			return "", err
		}
		snapshot, err = d.client.Snapshot(ctx)
		if err != nil {
			return "", err
		}
	}
	if err := h.daemonMetadata(snapshot); err != nil {
		h.discardFailedSession(ctx)
		return "", err
	}
	h.id = snapshot.SessionID
	root := h.id
	h.activeID.Store(&root)
	h.record, err = sessions.Lookup(cwd, h.id)
	if err == nil && h.record.ID == "" {
		err = errors.New("live session is missing its archive record")
	}
	if err != nil {
		h.discardFailedSession(ctx)
		return "", err
	}
	if !established {
		if resume {
			err = h.reviewResumedSettings(ctx)
		} else {
			err = h.applyRequestedModel(ctx, false)
		}
		if err != nil {
			h.discardFailedSession(ctx)
			return "", err
		}
		snapshot, err = d.client.Snapshot(ctx)
		if err != nil {
			return "", err
		}
	}
	// The event reader owns the transition to connected. Updates at or below
	// Head reach the UI before the snapshot's foreground and pending questions.
	snapshot.After = snapshot.Floor
	go h.readDaemon(ctx, snapshot, !established)
	return snapshot.Account, nil
}

var reopenAfterExit = errors.New("reopen disconnected live session")

func (d *daemonSession) attach(ctx context.Context, cwd string, c routing.Config, id string) (livesession.Snapshot, error) {
	if d.handle == "" {
		options := livesession.OpenOptions{Handle: livesession.NewHandle(), Cwd: cwd, Model: c.Model, Account: c.PinnedAccount, ResumeID: id}
		deadline := time.Now().Add(2 * time.Second)
		for {
			info, err := d.client.Open(ctx, options)
			if err == nil {
				d.handle = info.Handle
				break
			}
			if !daemonError(err, livesession.Conflict) || time.Now().After(deadline) {
				return livesession.Snapshot{}, err
			}
			if err := waitForDaemonLease(ctx); err != nil {
				return livesession.Snapshot{}, err
			}
		}
	}
	snapshot, err := d.claimAttachment(ctx)
	if err != nil || !snapshot.AdapterExited {
		return snapshot, err
	}
	// Release the durable ID after the failed adapter has been reaped.
	if err := d.client.CloseSession(ctx); err != nil {
		return snapshot, err
	}
	return snapshot, reopenAfterExit
}

func (d *daemonSession) claimAttachment(ctx context.Context) (livesession.Snapshot, error) {
	// A process may exit before the server has observed EOF on its connection.
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		snapshot, err := d.client.Attach(ctx, livesession.AttachOptions{Handle: d.handle, Control: true})
		if !daemonError(err, livesession.Conflict) {
			return snapshot, err
		}
		if time.Now().After(deadline) {
			return snapshot, fmt.Errorf("this conversation is open in another terminal: %w", err)
		}
		if err := waitForDaemonLease(ctx); err != nil {
			return snapshot, err
		}
	}
}

func waitForDaemonLease(ctx context.Context) error {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *harness) discardFailedSession(ctx context.Context) {
	// A disconnected frontend must leave in-flight setup and questions alive.
	if ctx.Err() == nil {
		closeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_ = h.daemon.client.CloseSession(closeCtx)
	}
}

func (h *harness) daemonMetadata(snapshot livesession.Snapshot) error {
	h.v2.Store(snapshot.ACPVersion == 2)
	hello, err := daemonHello(snapshot.ACPVersion, snapshot.Hello)
	if err != nil {
		return err
	}
	h.setCapabilities(hello)
	var setup proxy.Object
	if err := json.Unmarshal(snapshot.Setup, &setup); err != nil {
		return err
	}
	h.initialSettings = settingsFrom(setup)
	_ = json.Unmarshal(setup["availableCommands"], &h.initialCommands)
	var steering struct{ Supported bool }
	_ = json.Unmarshal(setup["petalSteering"], &steering)
	h.supportsSteering = steering.Supported && !h.v2.Load()
	return nil
}

func daemonHello(version uint16, raw json.RawMessage) (acp.InitializeResponse, error) {
	var hello acp.InitializeResponse
	if version == 2 {
		var fields proxy.Object
		if err := json.Unmarshal(raw, &fields); err != nil {
			return hello, err
		}
		return decodeInitializeV2(fields)
	}
	return hello, json.Unmarshal(raw, &hello)
}

func (h *harness) setupDaemon(ctx context.Context, c routing.Config, snapshot livesession.Snapshot) error {
	d := h.daemon
	// A canceled Next may still be polling on the server. Keep its slot
	// on an observer connection and leave the controller free for live reads.
	observer, err := d.dial(ctx)
	if err != nil {
		return err
	}
	defer observer.Close()
	if observer.Instance() != d.client.Instance() {
		return errors.New("live session daemon restarted during setup")
	}
	if _, err := observer.Attach(ctx, livesession.AttachOptions{Handle: d.handle}); err != nil {
		return err
	}
	setupCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.readSetupQuestions(setupCtx, observer, snapshot)
	}()
	defer func() { stop(); <-done }()

	if snapshot.ActiveOperation != 0 {
		receipt, err := d.client.WaitResult(ctx, snapshot.ActiveOperation)
		if err != nil {
			return err
		}
		if receipt.Err() != nil && snapshot.ActiveMethod == "initialize" {
			return receipt.Err()
		}
		snapshot, err = d.client.Snapshot(ctx)
		if err != nil {
			return err
		}
	}
	if len(snapshot.Setup) != 0 {
		return nil
	}
	if len(snapshot.Hello) == 0 {
		if _, err := sendRequest[proxy.Object](h, ctx, "initialize", nil); err != nil {
			return err
		}
		var err error
		snapshot, err = d.client.Snapshot(ctx)
		if err != nil {
			return err
		}
	}
	h.v2.Store(snapshot.ACPVersion == 2)
	hello, err := daemonHello(snapshot.ACPVersion, snapshot.Hello)
	if err != nil {
		return err
	}
	_, err = h.openWithAuth(ctx, c, hello, "session/open", nil)
	return err
}

func (h *harness) readSetupQuestions(ctx context.Context, observer *livesession.Client, snapshot livesession.Snapshot) {
	h.v2.Store(snapshot.ACPVersion == 2)
	questions := daemonQuestions{h: h, active: make(map[uint64]context.CancelFunc)}
	for _, question := range snapshot.Pending {
		questions.open(ctx, question)
	}
	cursor := snapshot.Head
	for {
		e, err := observer.Next(ctx, cursor)
		if err != nil {
			if daemonError(err, livesession.Gap) {
				current, err := observer.Snapshot(ctx)
				if err != nil {
					return
				}
				h.v2.Store(current.ACPVersion == 2)
				questions.reconcile(ctx, current.Pending)
				cursor = current.Head
				continue
			}
			return
		}
		cursor = e.Sequence
		if e.Kind == livesession.QuestionOpened {
			if current, err := observer.Snapshot(ctx); err == nil {
				h.v2.Store(current.ACPVersion == 2)
			}
		}
		questions.apply(ctx, e)
	}
}

type daemonQuestions struct {
	h      *harness
	active map[uint64]context.CancelFunc
	tail   <-chan struct{}
}

func (q *daemonQuestions) reconcile(ctx context.Context, pending []livesession.Question) {
	open := make(map[uint64]bool, len(pending))
	for _, question := range pending {
		open[question.ID] = true
	}
	for id, stop := range q.active {
		if !open[id] {
			stop()
			delete(q.active, id)
		}
	}
	for _, question := range pending {
		if q.active[question.ID] == nil {
			q.open(ctx, question)
		}
	}
}

func (q *daemonQuestions) open(parent context.Context, question livesession.Question) {
	ctx, stop := context.WithCancel(parent)
	q.active[question.ID] = stop
	var request struct{ Mode string }
	if question.Method == "elicitation/create" {
		_ = json.Unmarshal(question.Params, &request)
	}
	var previous <-chan struct{}
	var done chan struct{}
	if request.Mode != "url" {
		previous = q.tail
		done = make(chan struct{})
		q.tail = done
	}
	go func() {
		defer stop()
		if done != nil {
			defer close(done)
			if previous != nil {
				select {
				case <-previous:
				case <-parent.Done():
					return
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		h := q.h
		answer := livesession.Answer{QuestionID: question.ID}
		switch question.Method {
		case "host/approve":
			var request struct{ Title string }
			if json.Unmarshal(question.Params, &request) == nil {
				answer.OptionID, _ = h.choose(ctx, request.Title, []option{{"allow", "Allow once"}, {"deny", "Deny"}})
			}
		case "session/request_permission":
			if result, err := h.callback(ctx, question.Method, question.Params); err == nil {
				var response struct {
					Outcome struct{ OptionID string }
				}
				body, _ := json.Marshal(result)
				_ = json.Unmarshal(body, &response)
				answer.OptionID = response.Outcome.OptionID
			}
		case "elicitation/create":
			if result, err := h.elicit(ctx, question.Params); err == nil {
				answer.Result, _ = json.Marshal(result)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := h.daemon.client.Answer(ctx, answer); err != nil && ctx.Err() == nil && !daemonError(err, livesession.NotFound) {
			h.emitV2(ctx, daemonNotice{"Could not answer question: " + terminalError(err)})
		}
	}()
}

func (q *daemonQuestions) apply(ctx context.Context, e livesession.Event) {
	switch e.Kind {
	case livesession.QuestionOpened:
		if e.Question != nil {
			q.open(ctx, *e.Question)
		}
	case livesession.QuestionClosed:
		if stop := q.active[e.QuestionID]; stop != nil {
			stop()
			delete(q.active, e.QuestionID)
		}
	case livesession.Update:
		if e.Method == "elicitation/complete" {
			q.h.completeURL(ctx, e.Params)
		}
	}
}
