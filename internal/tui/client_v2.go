package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/proxy"
)

func (h *harness) initialize(ctx context.Context, tryV2 bool, legacy acp.ClientCapabilities) (acp.InitializeResponse, error) {
	info := map[string]string{"name": "petal", "version": "0-unstable"}
	params := map[string]any{"protocolVersion": 1, "clientInfo": info, "clientCapabilities": legacy}
	if tryV2 {
		// Include the legacy fields so an agent that negotiates down can still
		// use those capabilities without a second initialize.
		params["protocolVersion"] = 2
		params["info"] = info
		params["capabilities"] = map[string]any{
			"auth":        map[string]any{"terminal": struct{}{}},
			"elicitation": legacy.Elicitation,
		}
	}

	raw, err := acp.SendRequest[proxy.Object](h.conn, ctx, "initialize", params)
	var rpc *acp.RequestError
	if tryV2 && errors.As(err, &rpc) && (rpc.Code == -32022 || rpc.Code == -32602) {
		return h.initialize(ctx, false, legacy)
	}
	if err != nil {
		return acp.InitializeResponse{}, err
	}
	var version int
	if err := json.Unmarshal(raw["protocolVersion"], &version); err != nil {
		return acp.InitializeResponse{}, fmt.Errorf("decode ACP protocol version: %w", err)
	}
	switch {
	case version == 1:
		body, err := json.Marshal(raw)
		if err != nil {
			return acp.InitializeResponse{}, err
		}
		var hello acp.InitializeResponse
		err = json.Unmarshal(body, &hello)
		return hello, err
	case tryV2 && version == 2:
		hello, err := decodeInitializeV2(raw)
		if err != nil {
			// A v2 connection without session support cannot host the TUI.
			// The draft permits another initialize before any other request.
			legacyHello, fallbackErr := h.initialize(ctx, false, legacy)
			if fallbackErr != nil {
				return acp.InitializeResponse{}, errors.Join(err, fallbackErr)
			}
			return legacyHello, nil
		}
		h.v2.Store(true)
		return hello, nil
	default:
		return acp.InitializeResponse{}, fmt.Errorf("unsupported ACP protocol version %d", version)
	}
}

// Capability objects enable the feature even when empty.
func v2Object(raw json.RawMessage) proxy.Object {
	var object proxy.Object
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func decodeInitializeV2(raw proxy.Object) (acp.InitializeResponse, error) {
	capabilities := v2Object(raw["capabilities"])
	session := v2Object(capabilities["session"])
	if session == nil {
		return acp.InitializeResponse{}, fmt.Errorf("ACP v2 agent does not advertise sessions")
	}
	prompt := v2Object(session["prompt"])
	hello := acp.InitializeResponse{
		ProtocolVersion: 2,
		AgentCapabilities: acp.AgentCapabilities{
			PromptCapabilities: acp.PromptCapabilities{
				Image:           v2Object(prompt["image"]) != nil,
				Audio:           v2Object(prompt["audio"]) != nil,
				EmbeddedContext: v2Object(prompt["embeddedContext"]) != nil,
			},
			SessionCapabilities: acp.SessionCapabilities{
				List: &acp.SessionListCapabilities{}, Close: &acp.SessionCloseCapabilities{},
			},
		},
	}
	if v2Object(session["delete"]) != nil {
		hello.AgentCapabilities.SessionCapabilities.Delete = &acp.SessionDeleteCapabilities{}
	}
	var methods []json.RawMessage
	_ = json.Unmarshal(raw["authMethods"], &methods)
	for _, candidate := range methods {
		var method struct {
			Type, MethodID, Name string
			Args                 []string
			Env                  []struct{ Name, Value string }
		}
		if json.Unmarshal(candidate, &method) != nil || method.MethodID == "" {
			continue
		}
		switch method.Type {
		case "agent":
			hello.AuthMethods = append(hello.AuthMethods, acp.AuthMethod{Agent: &acp.AuthMethodAgent{
				Id: acp.AuthMethodId(method.MethodID), Name: method.Name,
			}})
		case "terminal":
			env := make(map[string]string, len(method.Env))
			for _, variable := range method.Env {
				env[variable.Name] = variable.Value
			}
			hello.AuthMethods = append(hello.AuthMethods, acp.AuthMethod{Terminal: &acp.AuthMethodTerminalInline{
				Id: acp.AuthMethodId(method.MethodID), Name: method.Name, Args: method.Args, Env: env,
			}})
		}
	}
	return hello, nil
}

// The SDK exposes prompt responses but no notification that an outbound
// request was written. Signal the write so Escape cannot overtake a prompt
// whose Bubble Tea command has yet to start.
type promptWriteV2 struct{ ready chan struct{} }

type promptWriterV2 struct {
	io.Writer
	harness *harness
}

func (w promptWriterV2) Write(p []byte) (int, error) {
	prompt := w.harness.promptWrite.Load()
	if prompt != nil {
		var header struct{ Method string }
		if json.Unmarshal(p, &header) != nil || header.Method != "session/prompt" {
			prompt = nil
		}
	}
	n, err := w.Writer.Write(p)
	if prompt != nil {
		w.harness.completePromptWrite(prompt)
	}
	return n, err
}

func (h *harness) completePromptWrite(prompt *promptWriteV2) {
	if h.promptWrite.CompareAndSwap(prompt, nil) {
		close(prompt.ready)
	}
}

type updateV2 struct {
	SessionUpdate string
	State         string
	StopReason    string
}

func (h *harness) updateV2(ctx context.Context, raw json.RawMessage) (bool, *acp.RequestError) {
	var params struct {
		SessionID string
		Update    json.RawMessage
	}
	var update updateV2
	if json.Unmarshal(raw, &params) != nil || params.SessionID == "" ||
		json.Unmarshal(params.Update, &update) != nil || update.SessionUpdate == "" {
		h.releaseV2Slot()
		return true, acp.NewInvalidParams("invalid ACP v2 session update")
	}
	if root := h.activeID.Load(); root != nil && params.SessionID != *root {
		return false, nil
	}
	switch update.SessionUpdate {
	case "replay_complete", "state_update":
		if update.SessionUpdate == "replay_complete" {
			h.v2ReplayComplete.Store(true)
		} else if h.v2ReplayComplete.Load() && update.State != "" {
			h.v2ResumeState.Store(true)
		}
		h.releaseV2Slot()
		h.emitV2(ctx, update)
	case "user_message", "user_message_chunk", "agent_message", "agent_message_chunk",
		"agent_thought", "agent_thought_chunk", "tool_call_update", "tool_call_content_chunk":
		h.releaseV2Slot()
		h.emitV2(ctx, event{v2Update: params.Update})
	default:
		return false, nil
	}
	return true, nil
}

func (h *harness) releaseV2Slot() {
	if h.updateSlots != nil {
		<-h.updateSlots
	}
}

func (h *harness) emitV2(ctx context.Context, msg tea.Msg) {
	select {
	case h.events <- msg:
	case <-ctx.Done():
	}
}

func (h *harness) permissionV2(ctx context.Context, raw json.RawMessage) (any, *acp.RequestError) {
	var params struct {
		SessionID, Description string
		Title                  *string
		Subject                struct {
			Type, Command, Cwd string
			ToolCall           proxy.Object
		}
		Options []struct {
			OptionID   *string
			Name, Kind string
		}
	}
	if json.Unmarshal(raw, &params) != nil || params.SessionID == "" || params.Title == nil || len(params.Options) == 0 {
		return nil, acp.NewInvalidParams("invalid ACP v2 permission request")
	}
	options := make([]option, 0, len(params.Options))
	for _, choice := range params.Options {
		if choice.OptionID == nil {
			return nil, acp.NewInvalidParams("ACP v2 permission option requires an optionId")
		}
		label := choice.Name
		if choice.Kind != "" {
			label += " · " + strings.ReplaceAll(choice.Kind, "_", " ")
		}
		options = append(options, option{*choice.OptionID, label})
	}
	description := "**Permission requested**"
	if *params.Title != "" {
		description += " · " + *params.Title
	}
	if params.Description != "" {
		description += "\n\n" + params.Description
	}
	question := &permission{sessionID: params.SessionID, title: *params.Title, options: options, reply: make(chan int, 1)}
	switch params.Subject.Type {
	case "command":
		description += "\n\n    " + strings.ReplaceAll(params.Subject.Command, "\n", "\n    ")
		if params.Subject.Cwd != "" {
			description += "\n\nWorking directory · " + params.Subject.Cwd
		}
	case "tool_call":
		if patch := params.Subject.ToolCall; patch != nil {
			patch["sessionUpdate"] = json.RawMessage(`"tool_call_update"`)
			if root := h.activeID.Load(); root == nil || *root == params.SessionID {
				question.v2Update, _ = json.Marshal(patch)
			}
			if input := string(patch["rawInput"]); input != "" && input != "null" {
				description += "\n\n    " + strings.ReplaceAll(input, "\n", "\n    ")
			}
		}
	}
	answer, err := h.question(ctx, description, question)
	id, selected := answer.(string)
	outcome := map[string]string{"outcome": "cancelled"}
	if err == nil && ctx.Err() == nil && selected {
		outcome = map[string]string{"outcome": "selected", "optionId": id}
	}
	return map[string]any{"outcome": outcome}, nil
}
