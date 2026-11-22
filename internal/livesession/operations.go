//go:build linux || darwin

package livesession

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"petal/internal/proxy"
	"petal/internal/sessions"
)

func bytesOf(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func fingerprint(value any) [32]byte { return sha256.Sum256(bytesOf(value)) }

func (s *session) submit(p *peer, call Call) (uint64, *Error) {
	hash := fingerprint(call)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireControl(p); err != nil {
		return 0, err
	}
	if call.Operation != 0 && call.Operation < s.nextOp {
		if op := s.findOperation(call.Operation); op != nil {
			if op.fingerprint != hash {
				return 0, failure(Conflict, "operation number is already bound to another call")
			}
			return op.number, nil
		}
		return 0, failure(Expired, "operation receipt is no longer retained")
	}
	if call.Operation != s.nextOp {
		return 0, failure(Invalid, "operation number does not match next operation")
	}
	if s.info.AdapterExited {
		return 0, failure(Closed, "session adapter disconnected")
	}
	if s.canceling || s.steering != nil || s.active != nil && call.Method != "_session/steering" {
		return 0, failure(Busy, "another request is active")
	}
	if call.Method == "_session/steering" && (s.info.ACPVersion != 1 || s.active == nil || s.active != s.turn) {
		return 0, failure(Conflict, "steering requires an active ACP v1 prompt")
	}
	if call.Method == "session/prompt" && s.running {
		return 0, failure(Busy, "a turn is running")
	}
	params, err := s.callParams(call)
	if err != nil {
		return 0, err
	}
	op := &operation{
		number: call.Operation, method: call.Method, fingerprint: hash,
		dispatched: make(chan struct{}), done: make(chan struct{}),
	}
	if call.Method == "session/open" {
		op.replay = &journal{}
	}
	s.nextOp++
	if call.Method == "_session/steering" {
		s.steering = op
	} else {
		s.active = op
	}
	if call.Method == "session/prompt" {
		s.turn = op
		if s.info.ACPVersion == 1 {
			s.running = true
		}
	}
	// ACP v1 adapters need not echo the user prompt. Preserve the accepted
	// input so a newly attached TUI can show the in-flight user message.
	started := Event{Kind: CallStarted, Method: call.Method, Operation: op.number}
	if call.Method == "session/prompt" && s.info.ACPVersion == 1 {
		started.Params = params
	}
	s.publish(started)
	go s.run(op, params)
	return op.number, nil
}

func (s *session) callParams(call Call) (json.RawMessage, *Error) {
	params := make(proxy.Object)
	if len(call.Params) > maxBody || (len(call.Params) != 0 &&
		(json.Unmarshal(call.Params, &params) != nil || params == nil)) {
		return nil, failure(Invalid, "expected bounded ACP parameters as an object")
	}
	delete(params, "cwd")
	delete(params, "sessionId")
	switch call.Method {
	case "initialize":
		if len(s.hello) != 0 {
			return nil, failure(Conflict, "session is already initialized")
		}
	case "session/open":
		if len(s.hello) == 0 || s.info.SessionID != "" {
			return nil, failure(Conflict, "initialize before opening a session")
		}
	case "authenticate", "logout", "auth/login", "auth/logout", "auth/status", "session/options", "session/list":
		if len(s.hello) == 0 {
			return nil, failure(Conflict, "initialize before making ACP calls")
		}
		if call.Method == "session/list" || call.Method == "session/options" {
			params["cwd"] = bytesOf(s.info.Cwd)
		}
	case "session/prompt", "_session/steering", "session/delete", "session/fork", "session/set_mode", "session/set_config_option", "session/status",
		"session/set_title", "session/archive", "session/unarchive", "tool_call/cancel", "terminal/cancel":
		if s.info.SessionID == "" {
			return nil, failure(Conflict, "open the adapter session first")
		}
		params["sessionId"] = bytesOf(s.info.SessionID)
		if call.Method == "session/fork" {
			params["cwd"] = bytesOf(s.info.Cwd)
		}
		if s.running && (call.Method == "session/delete" || call.Method == "session/fork") {
			return nil, failure(Busy, "wait for the current turn to finish")
		}
	default:
		return nil, failure(Invalid, "unsupported session method")
	}
	return bytesOf(params), nil
}

func (s *session) trimReceipts() {
	for len(s.receipts) > receiptCount || s.receiptSize > receiptBytes && len(s.receipts) > 1 {
		s.receiptSize -= s.receipts[0].size
		s.receipts[0] = nil
		s.receipts = s.receipts[1:]
	}
}

func (s *session) result(number uint64) (Receipt, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receiptLocked(number)
}

func (s *session) findOperation(number uint64) *operation {
	if s.active != nil && s.active.number == number {
		return s.active
	}
	if s.steering != nil && s.steering.number == number {
		return s.steering
	}
	for _, op := range s.receipts {
		if op.number == number {
			return op
		}
	}
	return nil
}

func (s *session) receiptLocked(number uint64) (Receipt, *Error) {
	if op := s.findOperation(number); op != nil {
		return op.receipt, nil
	}
	if number != 0 && number < s.nextOp {
		return Receipt{}, failure(Expired, "operation receipt is no longer retained")
	}
	return Receipt{}, failure(NotFound, "operation was not accepted")
}

func (s *session) waitResult(ctx context.Context, number uint64) (Receipt, *Error) {
	return poll(ctx, s, func() (Receipt, bool, *Error) {
		receipt, err := s.receiptLocked(number)
		return receipt, receipt.Done, err
	})
}

func (s *session) run(op *operation, params json.RawMessage) {
	defer close(op.done)
	var result any
	var err error
	select {
	case <-s.ready:
		err = s.initErr
	case <-s.ctx.Done():
		err = s.ctx.Err()
	}
	if err == nil {
		err = s.ctx.Err()
	}
	if err == nil {
		// Prompts and attached permissions can wait indefinitely while the
		// session is live. Other adapter requests have a bounded lifetime.
		ctx := s.ctx
		if op.method != "session/prompt" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
		}
		result, err = s.execute(ctx, op, params)
	}

	outcome := Outcome{}
	if err != nil {
		if !errors.As(err, &outcome.ACPError) && !errors.As(err, &outcome.Error) {
			outcome.Error = failure(Internal, err.Error())
		}
	} else {
		outcome.Result, err = json.Marshal(result)
		if err != nil {
			outcome.Error = failure(Internal, err.Error())
		}
	}
	size := len(bytesOf(outcome))
	if size > maxBody {
		outcome = Outcome{Error: failure(Limit, "adapter result exceeds daemon limit")}
		size = len(bytesOf(outcome))
	}
	s.mu.Lock()
	op.replay = nil
	op.receipt = Receipt{Done: true, Outcome: outcome}
	op.size = size
	s.receipts = append(s.receipts, op)
	s.receiptSize += size
	s.trimReceipts()
	if op == s.steering {
		s.steering = nil
	} else {
		s.active = nil
	}
	if op.method == "session/prompt" && s.info.ACPVersion == 1 {
		s.running = false
	}
	if !s.closed {
		finished := Event{Kind: CallFinished, Method: op.method, Operation: op.number}
		if op.method == "_session/steering" && proxy.SteeringInjected(outcome.Result) {
			finished.Params = params
		}
		s.publish(finished)
		if op.method == "session/delete" && outcome.Error == nil && outcome.ACPError == nil {
			s.closed = true
			s.publish(Event{Kind: SessionClosed})
			s.stop()
		}
	}
	s.mu.Unlock()
}

func (s *session) execute(ctx context.Context, op *operation, raw json.RawMessage) (any, error) {
	switch op.method {
	case "initialize":
		return s.initialize(ctx)
	case "session/open":
		return s.setupAdapter(ctx, op, raw)
	case "session/prompt":
		return s.backend.Call(ctx, op.method, raw, func() { close(op.dispatched) })
	case "session/fork":
		return s.fork(ctx, raw)
	case "session/delete":
		result, err := s.backend.Call(ctx, op.method, raw, nil)
		if err == nil {
			err = sessions.Forget(s.options.Cwd, "", s.info.SessionID)
		}
		return result, err
	default:
		return s.backend.Call(ctx, op.method, raw, nil)
	}
}

func (s *session) update(method string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	event := Event{Kind: Update, Method: method, Params: raw}
	if method == "session/update" {
		var params struct {
			SessionID string
			Update    struct{ SessionUpdate, State string }
		}
		_ = json.Unmarshal(raw, &params)
		if s.active != nil && s.active.replay != nil {
			op := s.active
			if s.options.ResumeID != "" && routeKey(params.SessionID) == routeKey(s.options.ResumeID) {
				switch params.Update.SessionUpdate {
				case "replay_complete":
					op.replayComplete = true
				case "state_update":
					if op.replayComplete && params.Update.State != "" {
						op.resumeState = true
					}
				}
			}
			op.replay.append(event)
			return
		}
		if !s.info.AdapterExited && params.Update.SessionUpdate == "state_update" &&
			routeKey(params.SessionID) == routeKey(s.info.SessionID) {
			s.setRunning(params.Update.State)
		}
	}
	s.publish(event)
}

func (s *session) setRunning(state string) {
	switch state {
	case "running":
		s.running = true
	case "idle":
		s.running = false
	}
}

// commitSetup publishes retained adapter updates only after the setup request
// succeeds. An authentication failure can then be retried without replaying a
// partially loaded conversation twice.
func (s *session) commitSetup(replay *journal) {
	floor := replay.floor()
	if floor > 0 {
		s.events.skip(floor)
	}
	for cursor := floor; cursor < replay.head; cursor++ {
		event, _ := replay.after(cursor)
		var params struct {
			SessionID string
			Update    struct{ SessionUpdate, State string }
		}
		if json.Unmarshal(event.Params, &params) == nil && params.Update.SessionUpdate == "state_update" &&
			routeKey(params.SessionID) == routeKey(s.info.SessionID) {
			s.setRunning(params.Update.State)
		}
		s.publish(event)
	}
}

func (s *session) cancelTurn(p *peer, number uint64) *Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireControl(p); err != nil {
		return err
	}
	if number != 0 && number == s.cancelled {
		return nil
	}
	if s.turn == nil || s.turn.number != number {
		return failure(NotFound, "turn does not match the current prompt")
	}
	if s.active != s.turn && !s.running {
		return nil
	}
	op, id := s.turn, s.info.SessionID
	s.cancelled = number
	s.canceling = true
	go s.cancelPrompt(op, id)
	return nil
}

func (s *session) cancelPrompt(op *operation, id string) {
	var err error
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	select {
	case <-op.done:
	case <-op.dispatched:
	case <-ctx.Done():
	}
	select {
	case <-op.dispatched:
		params := bytesOf(map[string]string{"sessionId": id})
		err = s.backend.Cancel(ctx, params)
	default:
		err = ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceling = false
	if !s.closed {
		if err == nil {
			s.cancelQuestions()
		}
		var outcome Outcome
		if err != nil {
			outcome.Error = failure(Internal, err.Error())
		}
		s.publish(Event{Kind: CancelFinished, Operation: op.number, Outcome: &outcome})
	}
}
