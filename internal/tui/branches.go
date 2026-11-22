package tui

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/proxy"
	"petal/internal/sessions"
)

type forkResult struct {
	id  string
	err error
}

func (m *terminal) fork() tea.Cmd {
	m.forking = true
	m.Status = "forking"
	h, parent, cwd := m.h, m.ctx, m.Cwd
	record := h.record
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		r, err := acp.SendRequest[proxy.Object](h.conn, ctx, "session/fork", map[string]any{"sessionId": record.ID, "cwd": cwd, "mcpServers": []any{}})
		if err != nil {
			return forkResult{err: err}
		}
		id, err := proxy.SessionID(r)
		if err != nil {
			return forkResult{err: err}
		}
		child := sessions.Record{ID: id, Parent: record.ID, Title: "Fork of " + record.Title, Model: record.Model, Account: record.Account, Opened: time.Now()}
		if record.Title == "" {
			child.Title = "Forked conversation"
		}
		_, err = sessions.Create(cwd, child)
		return forkResult{id, err}
	}
}

type childAnnouncement struct {
	parent string
	update acp.SubagentUpdate
}

type childOutput struct {
	id    string
	event event
}

type agentDetail struct {
	indexed                               int
	pending                               turnLog
	transcript                            []entry
	id, parent, title, description, state string
}

type agentMailbox struct {
	indexed           string
	id                acp.MessageId
	sender, recipient acp.SessionId
	body              strings.Builder
	display           string
}

type agentBrowser struct {
	children []agentDetail
	mail     []*agentMailbox
}

func (b *agentBrowser) child(id string) *agentDetail {
	for i := range b.children {
		if b.children[i].id == id {
			return &b.children[i]
		}
	}
	b.children = append(b.children, agentDetail{id: id})
	return &b.children[len(b.children)-1]
}

func (b *agentBrowser) update(parent string, u acp.SubagentUpdate) {
	c := b.child(string(u.SessionId))
	c.parent = parent
	if !u.Title.IsUnset() {
		c.title, _ = u.Title.Value()
	}
	if !u.Description.IsUnset() {
		c.description, _ = u.Description.Value()
	}
	if !u.State.IsUnset() {
		c.state = ""
		if state, ok := u.State.Value(); ok {
			raw, _ := json.Marshal(state)
			var v struct{ State string }
			_ = json.Unmarshal(raw, &v)
			c.state = strings.ReplaceAll(v.State, "_", " ")
			if v.State == "idle" {
				c.transcript = c.pending.commit(c.transcript)
			}
		}
	}
}
func (b *agentBrowser) message(id acp.MessageId) *agentMailbox {
	for _, m := range b.mail {
		if m.id == id {
			return m
		}
	}
	m := &agentMailbox{id: id}
	b.mail = append(b.mail, m)
	return m
}
func (m *agentMailbox) append(block acp.ContentBlock) {
	m.body.WriteString(contentBlockText(block))
}
func (b *agentBrowser) publish() {
	for _, m := range b.mail {
		m.display = m.body.String()
	}
}
func (b *agentBrowser) details(index int) []entry {
	c := b.children[index]
	var rows []entry
	for _, m := range b.mail {
		if m.display == "" {
			continue
		}
		switch c.id {
		case string(m.sender):
			rows = append(rows, entry{"sent", m.display})
		case string(m.recipient):
			rows = append(rows, entry{"received", m.display})
		}
	}
	return append(rows, c.transcript...)
}

func (m *terminal) showAgents() tea.Cmd {
	cmd := m.browseSessions()
	if len(m.agents.children) > 0 {
		m.launch.center.selected = m.agents.children[0].id
	}
	return cmd
}

func (b *agentBrowser) updateMessage(u acp.SessionMessage) {
	m := b.message(u.MessageId)
	if !u.SenderSessionId.IsUnset() {
		m.sender, _ = u.SenderSessionId.Value()
	}
	if !u.RecipientSessionId.IsUnset() {
		m.recipient, _ = u.RecipientSessionId.Value()
	}
	if !u.Content.IsUnset() {
		m.body.Reset()
		blocks, _ := u.Content.Value()
		for _, block := range blocks {
			m.append(block)
		}
	}
}
