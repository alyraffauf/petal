//go:build linux || darwin

package livesession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/proxy"
	"petal/internal/sessions"
)

func (s *session) initialize(ctx context.Context) (any, error) {
	info := map[string]string{"name": "petal", "version": "0-unstable"}
	params := map[string]any{
		"protocolVersion": 1, "clientInfo": info,
		"clientCapabilities": map[string]any{
			"auth":     map[string]any{"terminal": true},
			"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
			"terminal": true, "elicitation": elicitationCapabilities(),
			"subagents": struct{}{}, "plan": struct{}{},
			"session": map[string]any{
				"notices": struct{}{}, "compaction": struct{}{},
				"configOptions": map[string]any{"boolean": struct{}{}},
			},
		},
	}
	if s.config.ACPv2 {
		params["protocolVersion"] = 2
		params["info"] = info
		params["capabilities"] = map[string]any{
			"auth": map[string]any{"terminal": struct{}{}}, "elicitation": elicitationCapabilities(),
		}
	}
	result, err := s.backend.Call(ctx, "initialize", bytesOf(params), nil)
	body := bytesOf(result)
	var hello struct {
		ProtocolVersion uint16
		Capabilities    struct{ Session json.RawMessage }
	}
	_ = json.Unmarshal(body, &hello)
	var rpc *acp.RequestError
	if s.config.ACPv2 && (errors.As(err, &rpc) && (rpc.Code == -32022 || rpc.Code == -32602) ||
		err == nil && hello.ProtocolVersion == 2 && len(hello.Capabilities.Session) == 0) {
		params["protocolVersion"] = 1
		result, err = s.backend.Call(ctx, "initialize", bytesOf(params), nil)
		body = bytesOf(result)
		_ = json.Unmarshal(body, &hello)
	}
	if err != nil {
		return nil, err
	}
	if hello.ProtocolVersion != 1 && hello.ProtocolVersion != 2 {
		return nil, fmt.Errorf("unsupported ACP protocol version %d", hello.ProtocolVersion)
	}
	if len(body) > maxMetadata {
		return nil, failure(Limit, "adapter capabilities exceed daemon limit")
	}
	s.mu.Lock()
	s.info.ACPVersion = hello.ProtocolVersion
	s.hello = body
	s.mu.Unlock()
	return result, nil
}

func elicitationCapabilities() map[string]any {
	return map[string]any{"form": struct{}{}, "url": struct{}{}}
}

func (s *session) setupAdapter(ctx context.Context, op *operation, raw json.RawMessage) (any, error) {
	var params proxy.Object
	_ = json.Unmarshal(raw, &params)
	params["cwd"] = bytesOf(s.options.Cwd)
	if params["mcpServers"] == nil {
		params["mcpServers"] = json.RawMessage("[]")
	}
	method := "session/new"
	id := s.options.ResumeID
	if id != "" {
		params["sessionId"] = bytesOf(id)
		if s.info.ACPVersion == 2 {
			method = "session/resume"
			params["replayFrom"] = json.RawMessage(`{"type":"start"}`)
		} else {
			method = "session/load"
			var capabilities acp.InitializeResponse
			_ = json.Unmarshal(s.hello, &capabilities)
			if !capabilities.AgentCapabilities.LoadSession && capabilities.AgentCapabilities.SessionCapabilities.Resume != nil {
				method = "session/resume"
			}
		}
	}
	var archiveHead int64
	if method == "session/resume" && s.info.ACPVersion == 1 {
		var err error
		archiveHead, err = sessions.TranscriptHead(ctx, s.options.Cwd, id)
		if err != nil {
			return nil, fmt.Errorf("read archived transcript: %w", err)
		}
	}
	result, err := s.backend.Call(ctx, method, bytesOf(params), nil)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	validReplay := op.replay.foreground != ""
	s.mu.Unlock()
	if method == "session/resume" && s.info.ACPVersion == 2 && !validReplay {
		return nil, errors.New("ACP v2 resume ended without replay completion and current state")
	}
	body := bytesOf(result)
	var returned proxy.Object
	_ = json.Unmarshal(body, &returned)
	if returned == nil {
		returned = make(proxy.Object)
	}
	if id == "" {
		id, _ = proxy.SessionID(returned)
	}
	returned["sessionId"] = bytesOf(id)
	body = bytesOf(returned)
	route := proxy.DecodeSessionRoute(id)
	if route == nil || route[2] == "" {
		return nil, errors.New("adapter did not return a durable session ID")
	}
	record := sessions.Record{ID: id, Account: route[0], Model: route[1], Opened: time.Now()}
	if len(body) > maxMetadata {
		return nil, failure(Limit, "adapter session metadata exceeds daemon limit")
	}
	if _, err := sessions.Create(s.options.Cwd, record); err != nil {
		return nil, fmt.Errorf("save session %s: %w", id, err)
	}
	s.server.register(s, id)
	s.mu.Lock()
	s.info.SessionID = id
	s.info.Account = route[0]
	if s.config.PinnedAccount != "" {
		s.info.Account = s.config.PinnedAccount
	}
	s.resumeMethod = method
	s.archiveHead = archiveHead
	if !s.closed {
		s.commitSetup(op.replay)
	}
	op.replay = nil
	s.setup = body
	s.settingsRevision++
	s.mu.Unlock()
	go s.watchAdapter(s.backend.SessionDone(id))
	return returned, nil
}

func (s *session) watchAdapter(done <-chan struct{}) {
	select {
	case <-s.ctx.Done():
		return
	case <-done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.info.AdapterExited = true
		s.running = false
		s.turnV2, s.pendingPrompt = 0, nil
		s.cancelQuestions()
		s.publish(Event{Kind: AdapterExited})
	}
}

// An idle adapter gets a chance to finish its own session cleanup. Force
// teardown at the deadline even if the adapter fails to answer ACP close.
func (s *session) closeAdapter(id string, done chan<- struct{}) {
	defer close(done)
	defer s.stop()
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, s.stop)
	defer stop()
	_, _ = s.backend.Call(ctx, "session/close", bytesOf(map[string]string{"sessionId": id}), nil)
}

func (s *session) fork(ctx context.Context, raw json.RawMessage) (any, error) {
	result, err := s.backend.Call(ctx, "session/fork", raw, nil)
	if err != nil {
		return nil, err
	}
	var response struct{ SessionID string }
	_ = json.Unmarshal(bytesOf(result), &response)
	route := proxy.DecodeSessionRoute(response.SessionID)
	if route == nil || route[2] == "" {
		return nil, errors.New("adapter did not return a durable fork ID")
	}
	parent, err := sessions.Lookup(s.options.Cwd, s.info.SessionID)
	if err != nil {
		return nil, err
	}
	title := "Forked conversation"
	if parent.Title != "" {
		title = "Fork of " + parent.Title
	}
	_, err = sessions.Create(s.options.Cwd, sessions.Record{
		ID: response.SessionID, Parent: s.info.SessionID, Title: title,
		Account: route[0], Model: route[1], Opened: time.Now(),
	})
	return result, err
}

func (s *session) callback(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
	select {
	case s.callbacks <- struct{}{}:
		defer func() { <-s.callbacks }()
	default:
		return nil, acp.NewInternalError("session callback limit reached")
	}
	switch {
	case strings.HasPrefix(method, "fs/"), strings.HasPrefix(method, "terminal/"):
		var view func() acp.TerminalOutputResponse
		var ref TerminalRef
		if method == "terminal/release" && json.Unmarshal(raw, &ref) == nil {
			view = s.host.OutputView(ref.SessionID, ref.TerminalID)
		}
		result, err := s.host.Call(ctx, method, raw)
		if err == nil && view != nil {
			s.update("terminal/output", bytesOf(struct {
				TerminalRef
				acp.TerminalOutputResponse
			}{ref, terminalPreview(view())}))
		}
		if err == nil {
			return result, nil
		}
		var rpc *acp.RequestError
		if errors.As(err, &rpc) {
			return nil, rpc
		}
		return nil, acp.NewInternalError(err.Error())
	case method == "session/request_permission", method == "elicitation/create":
		return s.askAdapter(ctx, method, raw)
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

func (s *session) terminalOutput(ref TerminalRef) (acp.TerminalOutputResponse, *Error) {
	select {
	case <-s.ready:
		if s.host != nil {
			if view := s.host.OutputView(ref.SessionID, ref.TerminalID); view != nil {
				return terminalPreview(view()), nil
			}
		}
	default:
	}
	return acp.TerminalOutputResponse{}, failure(NotFound, "terminal is unavailable")
}

func terminalPreview(output acp.TerminalOutputResponse) acp.TerminalOutputResponse {
	const previewBytes = 64 << 10
	if len(output.Output) > previewBytes {
		start := len(output.Output) - previewBytes
		for !utf8.RuneStart(output.Output[start]) {
			start++
		}
		output.Output = output.Output[start:]
		output.Truncated = true
	}
	return output
}
