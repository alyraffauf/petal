package main

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	acp "github.com/coder/acp-go-sdk"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type permission struct {
	options []struct{ OptionID, Name string }
	reply   chan int
}
type event struct {
	role, text, id string
	tool           *toolEvent
	ask            *permission
	chunk          bool
}
type harness struct {
	supportsSteering bool
	conn             *acp.Connection
	events           chan tea.Msg
	permissions      chan struct{}
	id               string
	close            func()
}

func (h *harness) callback(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
	emit := func(e event) {
		select {
		case h.events <- e:
		case <-ctx.Done():
		}
	}
	switch method {
	case "session/update":
		var p struct {
			Update struct {
				SessionUpdate, ToolCallID, Status, MessageID string
				Title                                        *string
				Content                                      json.RawMessage
			}
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		u := p.Update
		switch u.SessionUpdate {
		case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
			var content struct{ Type, Text string }
			if json.Unmarshal(u.Content, &content) == nil && content.Text != "" {
				role := map[string]string{"user_message_chunk": "user", "agent_message_chunk": "assistant", "agent_thought_chunk": "system"}[u.SessionUpdate]
				emit(event{role: role, text: content.Text, chunk: true, id: u.MessageID})
			}
		case "tool_call", "tool_call_update":
			tool := toolEvent{id: u.ToolCallID, title: u.Title, status: u.Status}
			if len(u.Content) > 0 {
				var parts []struct {
					Type, Path, TerminalID string
					Content                struct{ Type, Text string }
				}
				// Null leaves existing content intact. An empty array replaces it.
				if err := json.Unmarshal(u.Content, &parts); err == nil && parts != nil {
					var output []string
					for _, p := range parts {
						switch p.Type {
						case "content":
							if p.Content.Text != "" {
								output = append(output, p.Content.Text)
							}
						case "diff":
							output = append(output, "Changed file · "+p.Path)
						case "terminal":
							output = append(output, "Terminal · "+p.TerminalID)
						}
					}
					body := strings.Join(output, "\n\n")
					tool.body = &body
				}
			}
			emit(event{tool: &tool})
		}
		return nil, nil
	case "session/request_permission":
		select {
		case h.permissions <- struct{}{}:
			defer func() { <-h.permissions }()
		case <-ctx.Done():
			return nil, acp.NewRequestCancelled(nil)
		}
		var p struct {
			ToolCall struct{ Title string }
			Options  []struct{ OptionID, Name string }
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		q := &permission{p.Options, make(chan int, 1)}
		var b strings.Builder
		fmt.Fprintf(&b, "**Permission requested** · %s\n", p.ToolCall.Title)
		for i, o := range p.Options {
			fmt.Fprintf(&b, "\n%d. %s", i+1, o.Name)
		}
		emit(event{role: "system", text: b.String(), ask: q})
		select {
		case i := <-q.reply:
			if i >= 0 && i < len(q.options) {
				return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": q.options[i].OptionID}}, nil
			}
		case <-ctx.Done():
		}
		return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}
func startHarness(ctx context.Context, c config) *harness {
	toProxy, clientOut := io.Pipe()
	toClient, proxyOut := io.Pipe()
	c.quiet = true
	newProxy(ctx, c, proxyOut, toProxy)
	h := &harness{events: make(chan tea.Msg, 128), permissions: make(chan struct{}, 1), close: func() { clientOut.Close(); proxyOut.Close() }}
	h.conn = acp.NewConnection(h.callback, clientOut, toClient)
	h.conn.SetLogger(slog.New(slog.DiscardHandler))
	return h
}
func (h *harness) open(ctx context.Context, c config, cwd string, resume bool) (string, error) {
	_, err := acp.SendRequest[object](h.conn, ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": object{}})
	if err != nil {
		return "", err
	}
	path, err := xdg("STATE", fmt.Sprintf("sessions/%x", sha256.Sum256([]byte(cwd+"\x00"+c.model))))
	if err != nil {
		return "", err
	}
	p := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	method := "session/new"
	if resume {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		h.id = string(data)
		p["sessionId"] = h.id
		method = "session/load"
	}
	r, err := acp.SendRequest[object](h.conn, ctx, method, p)
	if err != nil {
		return "", err
	}
	var steering struct{ Supported bool }
	_ = json.Unmarshal(r["petalSteering"], &steering)
	h.supportsSteering = steering.Supported
	if !resume {
		h.id, err = sessionID(r)
		if err != nil {
			return "", err
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(filepath.Dir(path), ".session-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		_, err = f.WriteString(h.id)
		closeErr := f.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
		if err = os.Rename(f.Name(), path); err != nil {
			return "", err
		}
	}
	var parts []string
	if json.Unmarshal([]byte(h.id), &parts) != nil || len(parts) != 3 {
		return "", fmt.Errorf("invalid route from proxy")
	}
	if c.forced != "" {
		return c.forced, nil
	}
	return parts[0], nil
}
