package proxy

import (
	"context"
	"encoding/json"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

func (p *proxy) relayV2ClientCall(ctx context.Context, name, method string, raw json.RawMessage, clientCaps Object, source context.Context) (any, *acp.RequestError) {
	method, id := untag(method)
	native, notification := strings.CutPrefix(method, notificationPrefix)
	switch native {
	case "session/update", "elicitation/complete":
		if !notification {
			return nil, acp.NewInvalidRequest("expected a notification")
		}
	case "session/request_permission", "elicitation/create":
		if notification {
			return nil, nil
		}
	default:
		if !strings.HasPrefix(native, "_") {
			return nil, acp.NewMethodNotFound(native)
		}
	}

	var params Object
	if len(raw) > 0 && json.Unmarshal(raw, &params) != nil {
		return nil, acp.NewInvalidParams("expected an object")
	}
	if !allowsClientCall(clientCaps, native, params) {
		return nil, acp.NewMethodNotFound(native)
	}
	if native == "elicitation/create" {
		if params["requestId"] != nil && string(params["requestId"]) == "null" {
			return nil, acp.NewInvalidParams("unknown elicitation request scope")
		}
	}
	rewriteID := func(fields Object, key string) {
		if hasString(fields, key) {
			fields[key] = encoded(p.publicID(name, stringField(fields, key)))
		}
	}
	nativeSession := stringField(params, "sessionId")
	rewriteID(params, "sessionId")
	switch native {
	case "elicitation/create", "elicitation/complete":
		rewriteID(params, "elicitationId")
	case "session/update":
		rewriteObject(params, "update", func(update Object) {
			switch stringField(update, "sessionUpdate") {
			case "subagent_update":
				rewriteID(update, "sessionId")
				rewriteObject(update, "state", func(state Object) { rewriteBackgroundIDs(state, rewriteID) })
			case "state_update":
				rewriteBackgroundIDs(update, rewriteID)
			case "session_message":
				rewriteID(update, "senderSessionId")
				rewriteID(update, "recipientSessionId")
			case "tool_call_update":
				rewriteToolContentIDs(update, rewriteID)
			case "tool_call_content_chunk":
				rewriteObject(update, "content", func(content Object) { rewriteSessionReference(content, rewriteID) })
			}
		})
	case "session/request_permission":
		rewriteObject(params, "subject", func(subject Object) {
			if stringField(subject, "type") == "tool_call" {
				rewriteObject(subject, "toolCall", func(tool Object) { rewriteToolContentIDs(tool, rewriteID) })
			}
		})
	}
	if notification {
		updateSlot := native == "session/update" && p.editorUpdates != nil
		if updateSlot {
			select {
			case p.editorUpdates <- struct{}{}:
			case <-source.Done():
				return nil, rpcError(source.Err())
			}
		}
		var wakeEndpoint *delegationEndpoint
		var wakeCtx context.Context
		var cancelWake context.CancelFunc
		if p.delegation != nil && native == "session/update" {
			update := params.member("update")
			if stringField(update, "sessionUpdate") == "state_update" {
				e := p.endpoint(stringField(params, "sessionId"))
				if e != nil {
					switch stringField(update, "state") {
					case "running":
						e.wakes.interrupt(false)
					case "idle":
						if stringField(update, "stopReason") == "end_turn" {
							wakeEndpoint = e
							wakeCtx, cancelWake = e.waitContext()
						}
					}
				}
			}
		}
		err := p.editor.SendNotification(ctx, native, params)
		if err != nil && updateSlot {
			<-p.editorUpdates
		}
		if wakeEndpoint != nil {
			if err != nil {
				cancelWake()
			} else {
				wakeEndpoint.idleV2(wakeCtx, cancelWake, p.backends[name], nativeSession)
			}
		}
		return nil, rpcError(err)
	}
	result, err := forwardV2(p.editor, p.editorWire, ctx, source, native, params, id)
	return result, rpcError(err)
}

func rewriteObject(fields Object, key string, rewrite func(Object)) {
	object := fields.member(key)
	if object == nil {
		return
	}
	rewrite(object)
	fields[key], _ = json.Marshal(object)
}

func rewriteBackgroundIDs(state Object, rewriteID func(Object, string)) {
	rewriteObjects(state, "background", func(work Object) {
		if stringField(work, "type") == "subagent" {
			rewriteID(work, "sessionId")
		}
	})
}

func rewriteToolContentIDs(tool Object, rewriteID func(Object, string)) {
	rewriteObjects(tool, "content", func(content Object) { rewriteSessionReference(content, rewriteID) })
}

func rewriteSessionReference(content Object, rewriteID func(Object, string)) {
	if stringField(content, "type") == "session" {
		rewriteID(content, "sessionId")
	}
}

func rewriteObjects(fields Object, key string, rewrite func(Object)) {
	var values []json.RawMessage
	if json.Unmarshal(fields[key], &values) != nil || values == nil {
		return
	}
	for i, value := range values {
		var object Object
		if json.Unmarshal(value, &object) == nil && object != nil {
			rewrite(object)
			values[i], _ = json.Marshal(object)
		}
	}
	fields[key], _ = json.Marshal(values)
}
