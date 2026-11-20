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
	parent := sessions.Record{ID: "root", Parent: "previous", Title: "Routing", Model: "test", Account: "local"}
	m := &terminal{h: &harness{record: parent}, launch: p, screen: screen{connection: connected, Width: 90, Height: 20, Messages: []entry{{"user", "First question"}, {"assistant", "Answer"}, {"user", "Second question"}}}}
	m.agents.children = []agentDetail{
		{id: "a", parent: "root", title: "Checks", state: "running"},
		{id: "b", parent: "a", title: "Replay", state: "idle"},
		{id: "unrelated", parent: "root"},
		{id: "unrelated-child", parent: "unrelated"},
	}
	m.agents.mail = []*agentMailbox{{sender: acp.SessionId("a"), recipient: acp.SessionId("b"), display: "Finished"}}
	p.history, p.embedded, p.width, p.height = true, true, 90, 20
	p.saved = []sessions.Record{parent, {ID: "previous", Title: "Previous"}}
	p.center = controlCenter{owner: m, selected: "a", folded: map[string]bool{
		"previous": true, "root": true, "a": true, "unrelated": true, "turns": true,
	}, finding: true, inspect: true, filter: 2}
	p.search.SetValue("Checks")
	before := p.View().Content

	p.centerKey(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	if frame := ansi.Strip(p.View().Content); !strings.Contains(frame, "agent graph") || !strings.Contains(frame, "Sent to") || !strings.Contains(frame, "r Reveal in threads") {
		t.Fatal(frame)
	}
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyDown})
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if p.center.graphID != "b" {
		t.Fatalf("graph followed %q", p.center.graphID)
	}
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if p.center.graphID != "" || p.center.selected != "a" || !p.center.finding || p.View().Content != before {
		t.Fatal("Escape did not restore the previous Threads view")
	}
	for _, id := range []string{"previous", "root", "a", "unrelated", "turns"} {
		if !p.center.folded[id] {
			t.Fatalf("Escape unfolded %q", id)
		}
	}

	p.centerKey(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyDown})
	p.centerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	p.centerKey(tea.KeyPressMsg{Code: 'r'})
	rows := p.nodes()
	if p.center.graphID != "" || len(rows) == 0 || rows[p.selectedNode(rows)].id != "b" || p.center.inspect || p.center.finding {
		t.Fatal("Reveal in threads did not show the focused graph node")
	}
	if !p.center.folded["unrelated"] || !p.center.folded["turns"] {
		t.Fatal("Reveal in threads unfolded an unrelated branch")
	}

	p.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	p.Update(tea.PasteMsg{Content: "Unrelated search"})
	p.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if p.search.Value() != "" {
		t.Fatal("pasting in the graph changed the Threads search")
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

func TestControlCenterFollowsSavedDelegationWithoutLiveSession(t *testing.T) {
	p := newPicker(routing.Config{})
	p.saved = []sessions.Record{
		{ID: "child", Title: "Check ordering", DelegatedParent: "parent", DelegationStatus: "completed", UnreadChildResult: true},
		{ID: "parent", Title: "Review changes"},
	}
	p.center = controlCenter{selected: "parent", folded: map[string]bool{"parent": true}}
	p.openGraph("parent")
	p.graphKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	p.graphKey(tea.KeyPressMsg{Code: 'r'})
	nodes := p.nodes()
	if len(nodes) != 2 || p.center.selected != "child" || nodes[1].id != "child" || nodes[1].depth != 1 || nodes[1].state != "completed" {
		t.Fatalf("saved delegation was not revealed under its parent: %+v", nodes)
	}
	p.width, p.height, p.history = 92, 23, true
	view := ansi.Strip(p.View().Content)
	if !strings.Contains(view, "Unread result") || !strings.Contains(view, "completed") {
		t.Fatalf("reopened center did not show the unread result and terminal status:\n%s", view)
	}
	p.saved[0].UnreadChildResult = false
	p.selectNode(p.nodes()[1])
	if view := ansi.Strip(p.View().Content); strings.Contains(view, "Unread result") {
		t.Fatalf("center still shows the consumed result as unread:\n%s", view)
	}
}
