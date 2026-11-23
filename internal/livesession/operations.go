//go:build linux || darwin

package livesession

import (
	"bytes"
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
	if call.Operation != s.nextOp || call.Operation >= independentTurnBase {
		return 0, failure(Invalid, "operation number does not match next operation")
	}
	if s.info.AdapterExited {
		return 0, failure(Closed, "session adapter disconnected")
	}
	// After setup, listing can overlap a prompt without taking its operation slot.
	backgroundList := call.Method == "session/list" && len(s.setup) != 0
	if backgroundList {
		if s.listing != nil {
			return 0, failure(Busy, "another session list is active")
		}
	} else if s.canceling || s.steering != nil || s.active != nil && call.Method != "_session/steering" {
		return 0, failure(Busy, "another request is active")
	}
	if call.Method == "_session/steering" && (s.info.ACPVersion != 1 || s.active == nil || s.active != s.turn) {
		return 0, failure(Conflict, "steering requires an active ACP v1 prompt")
	}
	if call.Method == "session/prompt" && s.settingsPending() {
		return 0, failure(Conflict, "confirm resumed settings before prompting")
	}
	if call.Method == "session/prompt" && (s.running || s.pendingPrompt != nil) {
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
		op.replay = &setupReplay{}
	}
	s.nextOp++
	switch {
	case call.Method == "_session/steering":
		s.steering = op
	case backgroundList:
		s.listing = op
	default:
		s.active = op
	}
	if call.Method == "session/prompt" {
		if s.info.ACPVersion == 1 {
			s.turn = op
			s.running = true
		} else {
			s.pendingPrompt = op
		}
	}
	// ACP v1 adapters need not echo the user prompt. Preserve the accepted
	// input so a newly attached TUI can show the in-flight user message.
	started := Event{Kind: CallStarted, Method: call.Method, Operation: op.number}
	if call.Method == "session/prompt" && s.info.ACPVersion == 1 {
		started.Params = promptEventParams(params)
	}
	s.publish(started)
	if !s.preparing {
		s.preparing = true
		go s.prepare()
	}
	go s.run(op, params)
	return op.number, nil
}

func (s *session) callParams(call Call) (json.RawMessage, *Error) {
	params := make(proxy.Object)
	limit := maxBody
	if call.Method == "session/prompt" || call.Method == "_session/steering" {
		limit = maxPromptBody
	}
	if len(call.Params) > limit || (len(call.Params) != 0 &&
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
		if (s.running || s.pendingPrompt != nil) && (call.Method == "session/delete" || call.Method == "session/fork") {
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
	if s.listing != nil && s.listing.number == number {
		return s.listing
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
	if op.method == "session/prompt" && s.info.ACPVersion == 2 {
		var reply struct{ MessageID *string }
		if outcome.Err() == nil && (json.Unmarshal(outcome.Result, &reply) != nil || reply.MessageID == nil) {
			outcome = Outcome{Error: failure(Internal, "ACP v2 prompt response requires a messageId")}
			size = len(bytesOf(outcome))
		}
	}
	op.receipt = Receipt{Done: true, Outcome: outcome}
	op.size = size
	s.receipts = append(s.receipts, op)
	s.receiptSize += size
	s.trimReceipts()
	if err == nil && outcome.Err() == nil {
		s.updateSetupSettings(op.method, params, outcome.Result)
	}
	switch op {
	case s.steering:
		s.steering = nil
	case s.listing:
		s.listing = nil
	default:
		s.active = nil
		s.reservationIdleSince = time.Now()
	}
	if op.method == "session/prompt" && s.info.ACPVersion == 1 {
		s.running = false
	}
	if op == s.pendingPrompt && outcome.Err() != nil {
		s.pendingPrompt = nil
	}
	if !s.closed {
		finished := Event{Kind: CallFinished, Method: op.method, Operation: op.number, Outcome: eventOutcome(op.method, outcome)}
		if op.method == "_session/steering" && proxy.SteeringInjected(outcome.Result) {
			finished.Params = promptEventParams(params)
		}
		s.publish(finished)
		if (op.method == "initialize" && outcome.Err() != nil) || (op.method == "session/delete" && outcome.Err() == nil) {
			s.closed = true
			s.archiveV2.deleted = true
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

type adapterUpdate struct {
	SessionID string
	Update    struct{ SessionUpdate, State string }
}

func (s *session) update(method string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	event := Event{Kind: Update, Method: method, Params: raw}
	if method == "session/update" {
		var params adapterUpdate
		_ = json.Unmarshal(raw, &params)
		if s.active != nil && s.active.replay != nil {
			replay := s.active.replay
			if s.info.ACPVersion == 2 && s.options.ResumeID != "" && routeKey(params.SessionID) == routeKey(s.options.ResumeID) {
				replay.archive.observe(raw, params.Update.SessionUpdate, params.Update.State, replay.head+1)
				if replay.archive.cursor != 0 && params.Update.SessionUpdate == "state_update" && params.Update.State != "" {
					replay.foreground = params.Update.State
					replay.stateCursor = replay.head + 1
				}
			}
			replay.append(event)
			return
		}
		if s.info.SessionID != "" && routeKey(params.SessionID) == routeKey(s.info.SessionID) {
			s.updateSetupFromNotification(raw)
			if s.info.ACPVersion == 2 {
				s.observeV2(raw, params.Update.SessionUpdate, params.Update.State, s.events.head+1)
			}
			if !s.info.AdapterExited && params.Update.SessionUpdate == "state_update" {
				s.setRunning(params.Update.State)
				event.Turn = s.currentV2Turn()
			}
		}
	}
	s.publish(event)
}

func (s *session) currentV2Turn() uint64 {
	if s.turnV2 == 0 && s.pendingPrompt != nil {
		return s.pendingPrompt.number
	}
	return s.turnV2
}

func (s *session) setRunning(state string) {
	s.foreground = state
	switch state {
	case "running", "requires_action":
		if !s.running && s.info.ACPVersion == 2 {
			if s.pendingPrompt != nil {
				s.turnV2 = s.pendingPrompt.number
				s.pendingPrompt = nil
			} else {
				s.nextIndependentTurn++
				s.turnV2 = independentTurnBase | s.nextIndependentTurn
			}
		}
		s.running = true
	case "idle":
		s.running = false
		s.turnV2 = 0
	}
}

// commitSetup publishes retained adapter updates only after the setup request
// succeeds. An authentication failure can then be retried without replaying a
// partially loaded conversation twice.
func (s *session) commitSetup(replay *setupReplay) {
	base, floor := s.events.head, replay.floor()
	resumedV2 := s.info.ACPVersion == 2 && s.options.ResumeID != ""
	if resumedV2 {
		s.commitReplayV2(&replay.archive, base)
		s.setRunning(replay.foreground)
	}
	if floor > 0 {
		s.events.skip(floor)
	}
	for cursor := floor; cursor < replay.head; cursor++ {
		event, _ := replay.after(cursor)
		if resumedV2 && cursor+1 == replay.stateCursor {
			event.Turn = s.currentV2Turn()
		}
		if s.info.ACPVersion == 2 && !resumedV2 {
			var params adapterUpdate
			if json.Unmarshal(event.Params, &params) == nil && routeKey(params.SessionID) == routeKey(s.info.SessionID) {
				s.observeV2(event.Params, params.Update.SessionUpdate, params.Update.State, base+cursor+1)
				if params.Update.SessionUpdate == "state_update" {
					s.setRunning(params.Update.State)
					event.Turn = s.currentV2Turn()
				}
			}
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
	var prompt *operation
	switch {
	case number != 0 && number == s.cancelled:
		current := s.currentV2Turn()
		if s.info.ACPVersion == 1 && s.running {
			current = s.turn.number
		}
		if current != 0 && current != number {
			return failure(NotFound, "turn does not match the current turn")
		}
		return nil
	case s.info.ACPVersion == 2 && number != 0 && number == s.turnV2:
		// The adapter has already reported the turn running.
	case s.info.ACPVersion == 2 && s.pendingPrompt != nil && s.pendingPrompt.number == number:
		prompt = s.pendingPrompt
	case s.info.ACPVersion == 1 && s.turn != nil && s.turn.number == number:
		if s.active != s.turn {
			return nil
		}
		prompt = s.turn
	default:
		return failure(NotFound, "turn does not match the current turn")
	}
	if s.canceling {
		return failure(Busy, "another cancellation is in progress")
	}
	s.cancelled = number
	s.canceling = true
	go s.cancelPrompt(number, prompt, s.info.SessionID)
	return nil
}

func (s *session) cancelPrompt(number uint64, prompt *operation, id string) {
	var err error
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	dispatched := prompt == nil
	if prompt != nil {
		select {
		case <-prompt.done:
		case <-prompt.dispatched:
		case <-ctx.Done():
		}
		select {
		case <-prompt.dispatched:
			dispatched = true
		default:
			err = ctx.Err()
		}
	}
	s.mu.Lock()
	current := s.info.ACPVersion == 1 || s.turnV2 == number ||
		s.turnV2 == 0 && prompt != nil && s.pendingPrompt == prompt
	s.mu.Unlock()
	if dispatched && current {
		params := bytesOf(map[string]string{"sessionId": id})
		err = s.backend.Cancel(ctx, params)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.canceling = false
	if !s.closed {
		if err == nil && (s.info.ACPVersion == 1 || s.turnV2 == 0 || s.turnV2 == number) {
			s.cancelQuestions()
		}
		var outcome Outcome
		if err != nil {
			outcome.Error = failure(Internal, err.Error())
		}
		s.publish(Event{Kind: CancelFinished, Operation: number, Turn: number, Outcome: &outcome})
	}
}

// Keep the small lifecycle receipts with the event that they complete. The
// separately bounded RPC receipt cache may expire before a terminal attaches.
func eventOutcome(method string, outcome Outcome) *Outcome {
	if method != "session/prompt" {
		return nil
	}
	if len(bytesOf(outcome)) > 16<<10 {
		return nil
	}
	return &outcome
}

func (s *session) updateSetupSettings(method string, params, result json.RawMessage) {
	if method != "session/set_mode" && method != "session/set_config_option" {
		return
	}
	var setup, changes proxy.Object
	if json.Unmarshal(s.setup, &setup) != nil || setup == nil {
		return
	}
	if method == "session/set_mode" {
		var input struct{ ModeID string }
		_ = json.Unmarshal(params, &input)
		var modes proxy.Object
		_ = json.Unmarshal(setup["modes"], &modes)
		if modes == nil {
			modes = make(proxy.Object)
		}
		modes["currentModeId"] = bytesOf(input.ModeID)
		setup["modes"] = bytesOf(modes)
	} else if json.Unmarshal(result, &changes) == nil && changes["configOptions"] != nil {
		setup["configOptions"] = changes["configOptions"]
	}
	if raw := bytesOf(setup); len(raw) <= maxMetadata && !bytes.Equal(raw, s.setup) {
		s.setup = raw
		s.settingsRevision++
	}
}

func promptEventParams(params json.RawMessage) json.RawMessage {
	if len(params) <= maxBody/2 {
		return params
	}
	var fields proxy.Object
	var prompt []proxy.Object
	_ = json.Unmarshal(params, &fields)
	if json.Unmarshal(fields["prompt"], &prompt) != nil {
		return nil
	}
	for _, block := range prompt {
		kind := string(block["type"])
		switch kind {
		case `"image"`, `"audio"`:
			block["data"] = bytesOf("")
		case `"resource"`:
			var resource proxy.Object
			if json.Unmarshal(block["resource"], &resource) == nil && resource != nil {
				if resource["blob"] != nil {
					resource["blob"] = bytesOf("")
				}
				if len(resource["text"]) > maxBody/4 {
					resource["text"] = bytesOf("[Embedded text omitted from live replay]")
				}
				block["resource"] = bytesOf(resource)
			}
		case `"text"`:
			if len(block["text"]) > maxBody/4 {
				block["text"] = bytesOf("[Long prompt omitted from live replay]")
			}
		}
	}
	if replay := bytesOf(map[string]any{"prompt": prompt}); len(replay) <= maxBody/2 {
		return replay
	}
	return json.RawMessage(`{"prompt":[{"type":"text","text":"[Large prompt omitted from live replay]"}]}`)
}

func (s *session) updateSetupFromNotification(raw json.RawMessage) {
	var envelope struct{ Update proxy.Object }
	if len(s.setup) == 0 || json.Unmarshal(raw, &envelope) != nil {
		return
	}
	update := envelope.Update
	switch string(update["sessionUpdate"]) {
	case `"config_option_update"`:
		s.updateSetupSettings("session/set_config_option", nil, bytesOf(update))
	case `"current_mode_update"`:
		s.updateSetupSettings("session/set_mode", bytesOf(proxy.Object{"modeId": update["currentModeId"]}), nil)
	}
}
