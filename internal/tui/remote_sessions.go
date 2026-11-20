package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"
	"petal/internal/sessions"
)

type remoteSessions struct {
	picker  *picker
	records []sessions.Record
	err     error
}

func (m *terminal) browseSessions() tea.Cmd {
	m.openPicker(false)
	picker := m.launch
	picker.showHistory()
	picker.center.owner = m
	if m.connection == connected {
		picker.center.selected = m.h.record.ID
	}
	watch := picker.Init()
	if m.h == nil || !m.h.canList {
		return watch
	}
	h, cwd, parent := m.h, m.Cwd, m.ctx
	return tea.Batch(watch, func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		var records []sessions.Record
		cursors := map[string]bool{}
		req := acp.ListSessionsRequest{Cwd: &cwd}
		for {
			result, err := acp.SendRequest[acp.ListSessionsResponse](h.conn, ctx, "session/list", req)
			if err != nil {
				return remoteSessions{picker, records, err}
			}
			for _, r := range result.Sessions {
				var route []string
				if r.Cwd != cwd || json.Unmarshal([]byte(r.SessionId), &route) != nil || len(route) != 3 {
					continue
				}
				s := sessions.Record{ID: string(r.SessionId), Model: route[1], Account: route[0]}
				if r.Title != nil {
					s.Title = *r.Title
				}
				if r.UpdatedAt != nil {
					s.Opened, _ = time.Parse(time.RFC3339Nano, *r.UpdatedAt)
				}
				records = append(records, s)
			}
			if result.NextCursor == nil {
				return remoteSessions{picker, records, nil}
			}
			if cursors[*result.NextCursor] {
				return remoteSessions{picker, records, fmt.Errorf("adapter repeated its session cursor")}
			}
			req.Cursor = result.NextCursor
			cursors[*req.Cursor] = true
		}
	})
}

type endedSession struct {
	err   error
	ended bool
}

func (h *harness) endSession(ctx context.Context, command, cwd, model string) tea.Msg {
	method, title := "session/delete", "Permanently delete this conversation?"
	params := map[string]string{"sessionId": h.id}
	if command == "/logout" {
		method, title = "logout", "Sign out of every connected subscription for "+model+"?"
		params = nil
		if h.v2.Load() {
			method = "auth/logout"
			params = map[string]string{}
		}
	}
	id, err := h.choose(ctx, title, []option{{"cancel", "Keep session"}, {"confirm", "Confirm"}})
	if err != nil || id != "confirm" {
		return endedSession{err: err}
	}
	if _, err := acp.SendRequest[json.RawMessage](h.conn, ctx, method, params); err != nil {
		return endedSession{err: err}
	}
	if method == "session/delete" {
		err = sessions.Forget(cwd, model, h.id)
	}
	return endedSession{err, true}
}
