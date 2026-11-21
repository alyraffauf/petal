package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/acpv2"
	"petal/internal/sessions"
)

type run struct {
	runtime  *Runtime
	cwd      string
	parentID string
	ctx      context.Context
	cancel   context.CancelFunc
	ready    chan struct{}
	binding  sessions.ChildBinding
	version  uint16

	mu             sync.Mutex
	active         bool
	permissionCtx  context.Context
	endPermissions context.CancelFunc
	reply          strings.Builder
	replyID        string
	replyCount     int
	transcript     acpv2.Transcript
	permissions    int
	busy           bool
	idle           chan json.RawMessage
	err            error
}

func (r *run) execute(conn Connection, task string, closeTransport context.CancelFunc) sessions.ChildResult {
	aborted := make(chan struct{})
	stopAbort := context.AfterFunc(r.ctx, func() {
		defer close(aborted)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), time.Second)
		defer cancel()
		// Closing the transport at the deadline also releases a blocked write.
		stopClose := context.AfterFunc(ctx, closeTransport)
		defer stopClose()
		defer closeTransport()
		_ = conn.Notify(ctx, "session/cancel", map[string]string{"sessionId": r.binding.ChildID})
	})
	r.mu.Lock()
	r.permissionCtx, r.endPermissions = context.WithCancel(r.ctx)
	r.active = r.ctx.Err() == nil
	r.idle = make(chan json.RawMessage, 1)
	r.mu.Unlock()

	data, err := r.prompt(conn, task)
	r.mu.Lock()
	r.active = false
	r.endPermissions()
	journalErr := r.err
	r.mu.Unlock()

	var stop struct {
		Reason string `json:"stopReason"`
	}
	_ = json.Unmarshal(data, &stop)
	if stop.Reason == "rate_limit" {
		if reporter, ok := conn.(interface{ RecordRateLimit() }); ok {
			reporter.RecordRateLimit()
		}
	}

	cancelled := !stopAbort()
	if cancelled {
		<-aborted
	}
	r.cancel()

	result := sessions.ChildResult{Status: "completed", Data: data}
	switch {
	case journalErr != nil:
		result = failed(journalErr)
	case cancelled && (err != nil || stop.Reason == ""):
		result = sessions.ChildResult{Status: "cancelled"}
	case err != nil:
		result = failed(err)
	default:
		switch stop.Reason {
		case "cancelled":
			result.Status = "cancelled"
		case "rate_limit", "error", "refusal", "max_tokens", "max_turn_requests":
			result.Status = "failed"
		}
	}

	messages := []sessions.Message{{Role: "user", Body: task}}
	r.finishReply()
	messages = append(messages, sessions.ProjectTranscript(&r.transcript, r.version == 2)...)
	answer := ""
	for _, message := range messages[1:] {
		if message.Role == "assistant" {
			answer = message.Body
			payload, _ := json.Marshal(map[string]string{"text": answer})
			if _, err := sessions.AppendChildActivity(context.WithoutCancel(r.ctx), r.cwd, r.binding, sessions.ChildActivity{Kind: "agent_message", Data: payload}); err != nil {
				result = failed(err)
				break
			}
		}
	}
	if err := sessions.SaveTranscript(r.cwd, r.binding.ChildID, messages, true); err != nil {
		result = failed(err)
	}
	if result.Status == "completed" {
		var terminal map[string]any
		if json.Unmarshal(result.Data, &terminal) == nil && terminal != nil {
			terminal["answer"] = answer
			result.Data, _ = json.Marshal(terminal)
		}
	}
	return result
}

func failed(err error) sessions.ChildResult {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	return sessions.ChildResult{Status: "failed", Data: data}
}

func (r *run) prompt(conn Connection, task string) (json.RawMessage, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	params := map[string]any{
		"sessionId": r.binding.ChildID,
		"prompt":    []map[string]string{{"type": "text", "text": task}},
	}
	reply, err := conn.Request(r.ctx, "session/prompt", params)
	if err != nil {
		if r.version == 2 && r.ctx.Err() != nil {
			return r.terminalResultOr(err)
		}
		return nil, err
	}
	if r.version == 1 {
		var terminal struct {
			StopReason string `json:"stopReason"`
		}
		if json.Unmarshal(reply, &terminal) != nil || terminal.StopReason == "" {
			return nil, errors.New("invalid ACP v1 prompt result")
		}
		return reply, nil
	}

	var inserted struct {
		MessageID *string `json:"messageId"`
	}
	if json.Unmarshal(reply, &inserted) != nil || inserted.MessageID == nil {
		return nil, errors.New("invalid ACP v2 prompt acknowledgement")
	}
	select {
	case result := <-r.idle:
		return result, nil
	case <-r.ctx.Done():
		return r.terminalResultOr(r.ctx.Err())
	case <-conn.Done():
		return r.terminalResultOr(errors.New("child disconnected before ACP v2 prompt completed"))
	}
}

// A terminal update already accepted before cancellation or EOF wins.
func (r *run) terminalResultOr(err error) (json.RawMessage, error) {
	select {
	case result := <-r.idle:
		return result, nil
	default:
		return nil, err
	}
}

func (r *run) receive(ctx context.Context, method string, params json.RawMessage, notification bool) (any, error) {
	if notification && method != "session/update" {
		return nil, nil
	}
	if !notification && method != "session/request_permission" {
		return nil, acp.NewMethodNotFound(method)
	}

	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &envelope) != nil {
		return nil, acp.NewInvalidParams("invalid child callback")
	}
	select {
	case <-r.ready:
	default:
		if notification {
			return nil, nil
		}
		return cancelledPermission(), nil
	}
	if envelope.SessionID != r.binding.ChildID {
		if notification {
			return nil, nil
		}
		return cancelledPermission(), nil
	}
	if notification {
		return nil, r.update(envelope.Update)
	}

	r.mu.Lock()
	permissionCtx := r.permissionCtx
	active := r.active && r.runtime.permission != nil && r.ctx.Err() == nil
	if active {
		r.permissions++
		if r.permissions == 1 {
			r.record("needs_input", nil)
		}
	}
	r.mu.Unlock()
	if !active || r.runtime.permission == nil || r.ctx.Err() != nil {
		return cancelledPermission(), nil
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(permissionCtx, cancel)
	defer stop()
	defer cancel()
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.permissions--
		if r.permissions == 0 && r.active && r.ctx.Err() == nil {
			r.record("running", nil)
		}
	}()
	response, err := r.runtime.permission(ctx, r.parentID, r.binding.ChildID, r.version, params)
	if ctx.Err() != nil || permissionCtx.Err() != nil {
		return cancelledPermission(), nil
	}
	return response, err
}

func cancelledPermission() json.RawMessage {
	return json.RawMessage(`{"outcome":{"outcome":"cancelled"}}`)
}

func (r *run) finishReply() {
	if r.reply.Len() != 0 {
		r.replyCount++
		update, _ := json.Marshal(map[string]any{"sessionUpdate": "agent_message_chunk", "messageId": strconv.Itoa(r.replyCount), "content": map[string]string{"type": "text", "text": r.reply.String()}})
		_, _ = r.transcript.Apply(update)
		r.reply.Reset()
	}
	r.replyID = ""
}

func (r *run) update(raw json.RawMessage) error {
	var update struct {
		Kind       string `json:"sessionUpdate"`
		State      string `json:"state"`
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(raw, &update) != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || r.ctx.Err() != nil {
		return nil
	}

	if r.version == 1 {
		switch update.Kind {
		case "tool_call", "tool_call_update", "tool_call_content_chunk", "agent_thought_chunk", "compaction_update":
			r.finishReply()
		}
	}
	switch update.Kind {
	case "compaction_update", "compaction_summary_chunk":
		_, err := r.transcript.Apply(raw)
		return err
	case "agent_message", "agent_message_chunk", "tool_call", "tool_call_update", "tool_call_content_chunk":
		if update.Kind == "agent_message" || update.Kind == "agent_message_chunk" {
			if r.version == 2 {
				_, _ = r.transcript.Apply(raw)
			} else {
				var chunk struct {
					MessageID string
					Content   struct{ Type, Text string }
				}
				if json.Unmarshal(raw, &chunk) == nil && chunk.Content.Type == "text" {
					if chunk.MessageID != "" && chunk.MessageID != r.replyID {
						r.finishReply()
					}
					if chunk.MessageID != "" {
						r.replyID = chunk.MessageID
					}
					r.reply.WriteString(chunk.Content.Text)
				}
			}
			return nil
		}
		r.projectTool(update.Kind, raw)
		return r.record(update.Kind, raw)
	case "state_update":
		if r.version != 2 {
			break
		}
		switch update.State {
		case "running", "requires_action":
			r.busy = true
		case "idle":
			if r.busy || update.StopReason != "" {
				// Stop accepting activity once the single delegated turn ends.
				r.active = false
				r.endPermissions()
				r.idle <- raw
			}
		}
	}
	return nil
}

// record is called with r.mu held while the turn is active.
func (r *run) record(kind string, data json.RawMessage) error {
	_, err := sessions.AppendChildActivity(r.ctx, r.cwd, r.binding, sessions.ChildActivity{Kind: kind, Data: data})
	if err != nil && r.ctx.Err() == nil {
		r.err = err
		r.cancel()
	}
	return err
}

// Some adapters expose MCP result blocks only in rawOutput.result.content.
// Project those blocks without copying call inputs or adapter metadata.
func (r *run) projectTool(kind string, raw json.RawMessage) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	if kind == "tool_call_content_chunk" {
		_, _ = r.transcript.Apply(raw)
		return
	}
	if kind == "tool_call" {
		fields["sessionUpdate"] = json.RawMessage(`"tool_call_update"`)
	}
	var content []json.RawMessage
	_ = json.Unmarshal(fields["content"], &content)
	if len(content) == 0 {
		var output struct {
			Result struct{ Content []json.RawMessage }
		}
		var blocks []json.RawMessage
		if json.Unmarshal(fields["rawOutput"], &output) == nil {
			blocks = output.Result.Content
		} else {
			_ = json.Unmarshal(fields["rawOutput"], &blocks)
		}
		for _, block := range blocks {
			var item struct {
				Type string
				Text *string
			}
			if json.Unmarshal(block, &item) == nil && item.Type == "text" && item.Text != nil {
				wrapper, _ := json.Marshal(map[string]any{"type": "content", "content": block})
				content = append(content, wrapper)
			}
		}
		if len(content) > 0 {
			fields["content"], _ = json.Marshal(content)
		}
	}
	if r.version == 1 {
		acpv2.NormalizeV1Tool(fields)
	}
	normalized, _ := json.Marshal(fields)
	_, _ = r.transcript.Apply(normalized)
}
