package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestControlCenterFollowsAgentLinksAndJumpsToATurn(t *testing.T) {
	cfg := routing.Config{Model: "test", Accounts: map[string]routing.Account{"local": {Models: map[string][]string{"test": {"adapter"}}}}}
	p := newPicker(cfg)
	parent := sessions.Record{ID: "root", Title: "Routing", Model: "test", Account: "local"}
	m := &terminal{h: &harness{record: parent}, launch: p, screen: screen{connection: connected, Width: 90, Height: 20, Messages: []entry{{"user", "First question"}, {"assistant", "Answer"}, {"user", "Second question"}}}}
	m.agents.children = []agentDetail{{id: "a", parent: "root", title: "Checks", state: "running"}, {id: "b", parent: "a", title: "Replay", state: "idle"}}
	m.agents.mail = []*agentMailbox{{sender: acp.SessionId("a"), recipient: acp.SessionId("b"), display: "Finished"}}
	p.history, p.embedded, p.width, p.height = true, true, 90, 20
	p.saved = []sessions.Record{parent, {ID: "fork", Parent: "root", Title: "Alternate"}}
	p.center = controlCenter{owner: m, selected: "a", folded: map[string]bool{"turns": true}}
	p.centerKey(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	if frame := ansi.Strip(p.View().Content); !strings.Contains(frame, "Agent graph") || !strings.Contains(frame, "Sent to") {
		t.Fatal(frame)
	}
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyDown})
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if p.center.graphID != "b" {
		t.Fatalf("graph followed %q", p.center.graphID)
	}
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if p.center.selected != "b" || p.center.graphID != "" {
		t.Fatal("tree did not retain graph focus")
	}
	p.search.SetValue("Second question")
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.launch != nil || m.chatBacklog.first != 2 || !m.chatBacklog.browsing {
		t.Fatal("turn did not open in the backlog")
	}
}

func TestControlCenterKeepsRunningThreadAndDraft(t *testing.T) {
	p := newPicker(routing.Config{})
	h := &harness{record: sessions.Record{ID: "current", Title: "Current"}}
	m := &terminal{h: h, launch: p, screen: screen{Busy: true, connection: connected, editor: testComposer("Draft", 5)}}
	p.saved = []sessions.Record{h.record, {ID: "other", Title: "Other"}}
	p.center = controlCenter{owner: m, selected: "other"}
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if p.chosen || m.launch == nil {
		t.Fatal("switched away from a running session")
	}
	p.center.selected = "current"
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.launch != nil || m.editor.Value() != "Draft" {
		t.Fatal("draft lost on return")
	}
}

func TestWheelOverThreadDetailsScrollsOnlyThatPane(t *testing.T) {
	parent := sessions.Record{ID: "root", Title: "Parent"}
	m := &terminal{h: &harness{record: parent}, screen: screen{connection: connected}}
	var paragraphs []string
	for i := range 50 {
		paragraphs = append(paragraphs, fmt.Sprintf("Detail %02d", i))
	}
	m.agents.children = []agentDetail{{id: "child", parent: "root", title: "Child", description: strings.Join(paragraphs, "\n\n")}}
	p := newPicker(routing.Config{})
	p.saved = []sessions.Record{parent}
	p.width, p.height, p.history = 92, 23, true
	p.center = controlCenter{owner: m, selected: "child", detailScroll: backlogViewport{browsing: true}}
	before := ansi.Strip(p.View().Content)
	p.Update(tea.MouseWheelMsg{X: 75, Y: 8, Button: tea.MouseWheelDown})
	after := ansi.Strip(p.View().Content)
	if before == after || !strings.Contains(before, "Detail 00") || strings.Contains(after, "Detail 00") || p.center.selected != "child" {
		t.Fatalf("detail wheel did not scroll the selected child's text:\n%s", after)
	}
}
