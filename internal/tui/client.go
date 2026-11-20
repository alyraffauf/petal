package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/acphost"
	"petal/internal/proxy"
	"petal/internal/routing"
	"petal/internal/sessions"
)

type planUpdate struct {
	id, text string
	remove   bool
	legacy   bool
}

type event struct {
	v2Update       json.RawMessage
	picture        *previewImage
	role, text, id string
	tool           *toolEvent
	ask            *permission
	chunk          bool
}
type harness struct {
	updateSlots          chan struct{}
	resumedHistory       []entry
	graphics             bool
	replayed             bool
	v2                   atomic.Bool
	v2ReplayComplete     atomic.Bool
	v2ResumeState        atomic.Bool
	promptWrite          atomic.Pointer[promptWriteV2]
	initialCommands      agentCommandUpdate
	activeID             atomic.Pointer[string]
	promptCaps           acp.PromptCapabilities
	canDelete, canLogout bool
	canFork, canList     bool
	canClose             bool
	lifetime             context.Context
	host                 *acphost.Host
	initialSettings      settingsUpdate
	record               sessions.Record
	cwd                  string
	supportsSteering     bool
	conn                 *acp.Connection
	events               chan tea.Msg
	permissions          chan struct{}
	id                   string
	close                func()
}

func (h *harness) callback(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
	emit := func(e event) {
		select {
		case h.events <- e:
		case <-ctx.Done():
		}
	}
	if h.v2.Load() {
		switch method {
		case "session/request_permission":
			return h.permissionV2(ctx, raw)
		case "session/update":
			handled, err := h.updateV2(ctx, raw)
			if handled || err != nil {
				return nil, err
			}
		}
	}
	if strings.HasPrefix(method, "fs/") || strings.HasPrefix(method, "terminal/") {
		if h.host == nil {
			return nil, acp.NewMethodNotFound(method)
		}
		result, err := h.host.Call(ctx, method, raw)
		if err != nil {
			var re *acp.RequestError
			if errors.As(err, &re) {
				return nil, re
			}
			return nil, acp.NewInternalError(err.Error())
		}
		return result, nil
	}
	switch method {
	case "session/update":
		if h.updateSlots != nil {
			defer func() { <-h.updateSlots }()
		}
		var p struct {
			SessionID string
			Update    struct {
				SessionUpdate, ToolCallID, Status, MessageID string
				Title                                        *string
				Content                                      json.RawMessage
				Entries                                      []acp.PlanEntry
				Plan                                         json.RawMessage
				PlanID                                       *string
				CurrentModeID, Description, Severity         string
				AvailableCommands                            []acp.AvailableCommand
				ConfigOptions                                []setting
				Used, Size                                   int64
			}
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		u := p.Update
		if u.SessionUpdate == "subagent_update" {
			var n struct{ Update acp.SubagentUpdate }
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, acp.NewInvalidParams(err.Error())
			}
			if p.SessionID != "" {
				h.activeID.CompareAndSwap(nil, &p.SessionID)
			}
			select {
			case h.events <- childAnnouncement{p.SessionID, n.Update}:
			case <-ctx.Done():
			}
			return nil, nil
		}
		if root := h.activeID.Load(); root != nil && p.SessionID != "" && p.SessionID != *root {
			// Child transcript traffic must never enter the parent composer or timeline.
			switch u.SessionUpdate {
			case "user_message_chunk", "agent_message_chunk":
				role := "assistant"
				if u.SessionUpdate == "user_message_chunk" {
					role = "user"
				}
				select {
				case h.events <- childOutput{p.SessionID, event{role: role, text: contentText(u.Content), id: u.MessageID, chunk: true}}:
				case <-ctx.Done():
				}
			}
			return nil, nil
		}
		switch u.SessionUpdate {
		case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
			role := map[string]string{"user_message_chunk": "user", "agent_message_chunk": "assistant", "agent_thought_chunk": "system"}[u.SessionUpdate]
			e := messageEvent(role, u.MessageID, u.Content, h.graphics && role != "system")
			if e.text != "" {
				emit(e)
			}
		case "plan", "plan_update", "plan_removed":
			update := planUpdate{}
			switch u.SessionUpdate {
			case "plan":
				update.legacy = true
				update.text = planEntries(u.Entries)
			case "plan_removed":
				update.remove = true
				if u.PlanID == nil {
					return nil, acp.NewInvalidParams("missing planId")
				}
				update.id = *u.PlanID
			case "plan_update":
				var plan acp.PlanUpdateContent
				if err := json.Unmarshal(u.Plan, &plan); err != nil {
					return nil, acp.NewInvalidParams(err.Error())
				}
				if plan.Items != nil {
					update.id = string(plan.Items.PlanId)
					update.text = planEntries(plan.Items.Entries)
				} else {
					update.id = string(plan.Markdown.PlanId)
					update.text = plan.Markdown.Content
				}
			}
			select {
			case h.events <- update:
			case <-ctx.Done():
			}
		case "session_message":
			return forwardUpdate[acp.SessionMessage](ctx, h, raw)
		case "session_message_chunk":
			return forwardUpdate[acp.SessionMessageChunk](ctx, h, raw)
		case "available_commands_update":
			select {
			case h.events <- agentCommandUpdate(u.AvailableCommands):
			case <-ctx.Done():
			}
		case "current_mode_update":
			return forwardUpdate[acp.SessionCurrentModeUpdate](ctx, h, raw)
		case "usage_update":
			return forwardUpdate[acp.SessionUsageUpdate](ctx, h, raw)
		case "session_info_update":
			return forwardUpdate[acp.SessionSessionInfoUpdate](ctx, h, raw)
		case "notice":
			return forwardUpdate[acp.SessionUpdateNotice](ctx, h, raw)
		case "compaction_update":
			return forwardUpdate[acp.SessionCompactionUpdate](ctx, h, raw)
		case "compaction_summary_chunk":
			return forwardUpdate[acp.SessionUpdateCompactionSummaryChunk](ctx, h, raw)
		case "config_option_update":
			select {
			case h.events <- settingsUpdate{Config: u.ConfigOptions}:
			case <-ctx.Done():
			}
		case "tool_call", "tool_call_update":
			tool := toolEvent{id: u.ToolCallID, title: u.Title, status: u.Status}
			if len(u.Content) > 0 {
				var parts []struct {
					Type, Path, TerminalID, SessionID string
					Content                           json.RawMessage
					OldText, NewText                  string
				}
				// Null leaves existing content intact. An empty array replaces it.
				if err := json.Unmarshal(u.Content, &parts); err == nil && parts != nil {
					var output []string
					sessionID := p.SessionID
					for _, p := range parts {
						switch p.Type {
						case "content":
							if text := contentText(p.Content); text != "" {
								output = append(output, text)
							}
						case "diff":
							output = append(output, "Changed file · "+p.Path)
							if p.OldText != "" || p.NewText != "" {
								output = append(output, "Removed:\n```\n"+p.OldText+"\n```\nAdded:\n```\n"+p.NewText+"\n```")
							}
						case "session":
							if p.SessionID != "" {
								output = append(output, "Subagent · see /agents")
							}
						case "terminal":
							output = append(output, "Terminal · "+p.TerminalID)
							if h.host != nil {
								if view := h.host.OutputView(sessionID, p.TerminalID); view != nil {
									tool.terminals = append(tool.terminals, view)
								}
							}
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
		var p struct {
			SessionID string
			ToolCall  struct {
				Title, ToolCallID, Kind string
				RawInput                json.RawMessage
				Locations               []struct {
					Path string
					Line *int
				}
			}
			Options []struct{ OptionID, Name, Kind string }
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		title := p.ToolCall.Title
		if title == "" {
			title = p.ToolCall.ToolCallID
		}
		var description strings.Builder
		if root := h.activeID.Load(); root != nil && p.SessionID != "" && p.SessionID != *root {
			description.WriteString("**Subagent permission requested** · ")
		} else {
			description.WriteString("**Permission requested** · ")
		}
		description.WriteString(title)
		if p.ToolCall.Kind != "" {
			fmt.Fprintf(&description, " (%s)", p.ToolCall.Kind)
		}
		for _, loc := range p.ToolCall.Locations {
			fmt.Fprintf(&description, "\n\n    %s", loc.Path)
			if loc.Line != nil {
				fmt.Fprintf(&description, ":%d", *loc.Line)
			}
		}
		if input := string(p.ToolCall.RawInput); input != "" && input != "null" {
			description.WriteString("\n\n    " + strings.ReplaceAll(input, "\n", "\n    "))
		}
		options := make([]option, 0, len(p.Options))
		for _, opt := range p.Options {
			label := opt.Name
			if opt.Kind != "" {
				label += " · " + strings.ReplaceAll(opt.Kind, "_", " ")
			}
			options = append(options, option{opt.OptionID, label})
		}
		answer, err := h.question(ctx, description.String(), &permission{sessionID: p.SessionID, title: title, options: options, reply: make(chan int, 1)})
		id, _ := answer.(string)
		if err != nil || id == "" {
			return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
		}
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": id}}, nil
	case "elicitation/create":
		return h.elicit(ctx, raw)
	case "elicitation/complete":
		h.completeURL(ctx, raw)
		return nil, nil
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

func planEntries(entries []acp.PlanEntry) string {
	lines := make([]string, 0, len(entries))
	for _, step := range entries {
		lines = append(lines, "- "+step.Content+" · "+string(step.Status))
	}
	return strings.Join(lines, "\n")
}

type terminalSignIn struct {
	cmd  *exec.Cmd
	done chan error
}

var signedIn = errors.New("terminal sign-in finished")

func startHarness(ctx context.Context, c routing.Config) *harness {
	toProxy, clientOut := io.Pipe()
	toClient, proxyOut := io.Pipe()
	updateSlots := make(chan struct{}, 128)
	proxy.ConnectEmbedded(ctx, c, proxyOut, toProxy, updateSlots)
	h := &harness{updateSlots: updateSlots, graphics: directGraphicsTerminal(), lifetime: ctx, events: make(chan tea.Msg, 128), permissions: make(chan struct{}, 1), close: func() { clientOut.Close(); proxyOut.Close() }}
	h.conn = acp.NewConnection(h.callback, promptWriterV2{clientOut, h}, toClient)
	h.conn.SetLogger(slog.New(slog.DiscardHandler))
	return h
}
func (h *harness) open(ctx context.Context, c routing.Config, cwd string, resume bool) (string, error) {
	host, err := acphost.Open(ctx, cwd, func(ctx context.Context, title string) error {
		id, err := h.choose(ctx, title, []option{{"allow", "Allow once"}, {"deny", "Deny"}})
		if err != nil {
			return err
		}
		if id != "allow" {
			return fmt.Errorf("user declined %s", title)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	h.host = host
	context.AfterFunc(ctx, func() { host.Close() })
	caps := acp.ClientCapabilities{
		Auth:        acp.AuthCapabilities{Terminal: true},
		Fs:          acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
		Elicitation: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}, Form: &acp.ElicitationFormCapabilities{}},
		Subagents:   &acp.SubagentCapabilities{}, Terminal: true, Plan: &acp.PlanCapabilities{},
		Session: &acp.ClientSessionCapabilities{
			Notices: &acp.NoticeCapabilities{}, Compaction: &acp.CompactionCapabilities{},
			ConfigOptions: &acp.SessionConfigOptionsCapabilities{Boolean: &acp.BooleanConfigOptionCapabilities{}},
		},
	}
	hello, err := h.initialize(ctx, c.ACPv2, caps)
	if err != nil {
		return "", err
	}
	h.promptCaps = hello.AgentCapabilities.PromptCapabilities
	h.canDelete = hello.AgentCapabilities.SessionCapabilities.Delete != nil
	h.canLogout = hello.AgentCapabilities.Auth.Logout != nil
	h.canFork = hello.AgentCapabilities.SessionCapabilities.Fork != nil && (hello.AgentCapabilities.LoadSession || hello.AgentCapabilities.SessionCapabilities.Resume != nil)
	h.canList = hello.AgentCapabilities.SessionCapabilities.List != nil
	h.canClose = hello.AgentCapabilities.SessionCapabilities.Close != nil
	authenticate := "authenticate"
	if h.v2.Load() {
		authenticate = "auth/login"
		h.canLogout = len(hello.AuthMethods) != 0
	}
	// The embedded proxy resolves configured MCP credentials and transport capabilities.
	p := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	method := "session/new"
	if resume {
		if h.id == "" {
			record, err := sessions.Latest(cwd, c.Model)
			if err != nil {
				return "", err
			}
			h.id = record.ID
		}
		root := h.id
		h.activeID.Store(&root)
		p["sessionId"] = h.id
		method = "session/load"
		if h.v2.Load() {
			method = "session/resume"
			p["replayFrom"] = map[string]string{"type": "start"}
		} else if !hello.AgentCapabilities.LoadSession && hello.AgentCapabilities.SessionCapabilities.Resume != nil {
			method = "session/resume"
		}
	}
	h.replayed = method == "session/load" || (resume && h.v2.Load())
	r, err := acp.SendRequest[proxy.Object](h.conn, ctx, method, p)
	var authErr *acp.RequestError
	if errors.As(err, &authErr) && authErr.Code == -32000 {
		var options []option
		for _, m := range hello.AuthMethods {
			if m.Terminal != nil {
				options = append(options, option{string(m.Terminal.Id), m.Terminal.Name})
			}
			if m.Agent != nil {
				options = append(options, option{string(m.Agent.Id), m.Agent.Name})
			}
		}
		if len(options) > 0 {
			id, askErr := h.choose(ctx, "Choose a sign-in method", options)
			if askErr != nil {
				return "", askErr
			}
			if id != "" {
				for _, m := range hello.AuthMethods {
					if m.Terminal == nil || string(m.Terminal.Id) != id {
						continue
					}
					login := m.Terminal
					if len(login.Args) != 1 || !strings.HasPrefix(login.Args[0], "--terminal-auth=") {
						return "", fmt.Errorf("invalid terminal sign-in invocation")
					}
					cmd, err := proxy.LoginCommand(ctx, c, strings.TrimPrefix(login.Args[0], "--terminal-auth="))
					if err != nil {
						return "", err
					}
					for k, v := range login.Env {
						cmd.Env = append(cmd.Env, k+"="+v)
					}
					q := terminalSignIn{cmd, make(chan error, 1)}
					select {
					case h.events <- q:
					case <-ctx.Done():
						return "", ctx.Err()
					}
					select {
					case err := <-q.done:
						if err != nil {
							return "", err
						}
						return "", signedIn
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}
				if _, err = acp.SendRequest[proxy.Object](h.conn, ctx, authenticate, map[string]string{"methodId": id}); err == nil {
					r, err = acp.SendRequest[proxy.Object](h.conn, ctx, method, p)
				}
			}
		}
	}
	if err != nil {
		return "", err
	}
	if h.v2.Load() {
		if resume && (!h.v2ReplayComplete.Load() || !h.v2ResumeState.Load()) {
			return "", fmt.Errorf("ACP v2 resume ended without replay completion and current state")
		}
		_ = json.Unmarshal(r["availableCommands"], &h.initialCommands)
	} else if method == "session/resume" {
		// This ACP method reconnects without replaying the transcript.
		rows, err := sessions.RecentTranscript(ctx, cwd, h.id, -1)
		if err == nil {
			for _, row := range rows {
				h.resumedHistory = append(h.resumedHistory, entry{row.Role, row.Body})
			}
		}
	}
	h.initialSettings = settingsFrom(r)
	var steering struct{ Supported bool }
	_ = json.Unmarshal(r["petalSteering"], &steering)
	h.supportsSteering = steering.Supported && !h.v2.Load()
	if !resume {
		h.id, err = proxy.SessionID(r)
		if err != nil {
			return "", err
		}
	}
	root := h.id
	h.activeID.Store(&root)
	var parts []string
	if json.Unmarshal([]byte(h.id), &parts) != nil || len(parts) != 3 {
		return "", fmt.Errorf("invalid route from proxy")
	}
	h.cwd = cwd
	initialRecord := sessions.Record{ID: h.id, Model: c.Model, Account: parts[0], Opened: time.Now()}
	if !resume {
		initialRecord.Placement, err = placementFrom(r)
		if err != nil {
			return "", err
		}
	}
	if err := h.prepareSessionRecord(ctx, initialRecord, resume); err != nil {
		return "", err
	}
	if c.PinnedAccount != "" {
		return c.PinnedAccount, nil
	}
	return parts[0], nil
}

func placementFrom(r proxy.Object) (*routing.Placement, error) {
	var placement *routing.Placement
	if raw := r["petalPlacement"]; raw != nil {
		if err := json.Unmarshal(raw, &placement); err != nil {
			return nil, fmt.Errorf("read account placement: %w", err)
		}
	}
	return placement, nil
}

func (h *harness) prepareSessionRecord(ctx context.Context, initial sessions.Record, resume bool) error {
	var err error
	if resume {
		h.record, err = sessions.Lookup(h.cwd, initial.ID)
		if err != nil {
			return err
		}
	}
	if h.record.ID == "" {
		h.record, err = sessions.Create(h.cwd, initial)
		if err != nil {
			return err
		}
	}
	if resume {
		return h.reviewResumedSettings(ctx)
	}
	return nil
}

func (h *harness) updateRecord(edit func(*sessions.Record) bool) error {
	updated, err := sessions.Update(h.cwd, h.record.ID, edit)
	if updated.ID != "" {
		h.record = updated
	}
	return err
}
