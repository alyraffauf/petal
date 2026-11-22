package tui

import (
	"bloomfx.dev/bloom"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom/anim"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestControlCenterFollowsAgentLinksAndJumpsToATurn(t *testing.T) {
	cfg := routing.Config{Model: "test", Accounts: map[string]routing.Account{"local": {Models: map[string][]string{"test": {"adapter"}}}}}
	p := newPicker(".", cfg)
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
	p := newPicker(".", routing.Config{})
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
	p := newPicker(".", routing.Config{})
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
	p := newPicker(".", routing.Config{})
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
	p.refreshChildStatuses(childStatusesChanged{statuses: map[sessions.ChildBinding]sessions.ChildStatus{nodes[1].binding: {State: "completed"}}})
	p.selectNode(p.nodes()[1])
	if view := ansi.Strip(p.View().Content); strings.Contains(view, "Unread result") {
		t.Fatalf("center still shows the consumed result as unread:\n%s", view)
	}
}

func TestControlCenterCancelsOnlySelectedDelegation(t *testing.T) {
	for _, input := range []string{"keyboard", "mouse"} {
		t.Run(input, func(t *testing.T) {
			p := runningChildrenPicker(t)
			p.center.filter = 2
			p.View()
			var cmd tea.Cmd
			if input == "keyboard" {
				_, cmd = p.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
			} else {
				point := p.center.cancelButton.Min
				_, cmd = p.Update(tea.MouseClickMsg{X: point.X, Y: point.Y, Button: tea.MouseLeft})
			}
			if cmd == nil {
				t.Fatal("no cancellation requested")
			}
			binding := sessions.ChildBinding{ParentID: "parent", ChildID: "child"}
			if len(p.center.cancelling) != 1 || !p.center.cancelling[binding.ChildID] {
				t.Fatalf("wrong cancellation target: %v", p.center.cancelling)
			}
			if len(p.nodes()) != 2 {
				t.Fatal("pending cancellation disappeared from the working filter")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			owner := make(chan error, 1)
			go func() {
				err := sessions.WaitForChildCancellation(ctx, p.center.cwd, binding)
				if err == nil {
					err = sessions.FinishChild(ctx, p.center.cwd, binding, sessions.ChildResult{Status: "cancelled"})
				}
				owner <- err
			}()
			p.Update(cmd())
			if err := <-owner; err != nil {
				t.Fatal(err)
			}
			sibling, err := sessions.ReadChildActivity(ctx, p.center.cwd, sessions.ChildBinding{ParentID: "parent", ChildID: "sibling"}, 0, 10)
			if err != nil || sibling.Status != "running" || sibling.Result != nil {
				t.Fatalf("sibling journal changed: %+v, %v", sibling, err)
			}
			rows := p.nodes()
			if len(rows) != 1 || rows[0].id != "sibling" || rows[0].state != "running" {
				t.Fatalf("expected only the running sibling, got %+v", rows)
			}
			if view := ansi.Strip(p.View().Content); !strings.Contains(view, "Selected · cancelled") {
				t.Fatal(view)
			}
			if _, cmd := p.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}); cmd != nil {
				t.Fatal("repeated cancellation targeted the sibling after the selected child left the filter")
			}
			p.center.selected = "child"
			p.center.filter = 0
			if view := ansi.Strip(p.View().Content); !strings.Contains(view, "cancelled") || strings.Contains(view, cancelChildLabel) {
				t.Fatal(view)
			}
		})
	}
}

func TestControlCenterRefreshesAfterIndependentChildCompletion(t *testing.T) {
	previous := anim.Motion()
	anim.SetMotion(false)
	t.Cleanup(func() { anim.SetMotion(previous) })
	p := runningChildrenPicker(t)
	p.center.filter = 2
	initial := p.Init()
	if initial == nil {
		t.Fatal("control center did not subscribe to child state")
	}
	_, next := p.Update(initial())
	if next == nil {
		t.Fatal("control center stopped watching after its initial snapshot")
	}
	if view := ansi.Strip(p.View().Content); !strings.Contains(view, cancelChildLabel) {
		t.Fatal(view)
	}
	oldButton := p.center.cancelButton.Min
	binding := sessions.ChildBinding{ParentID: "parent", ChildID: "child"}
	if err := sessions.FinishChild(t.Context(), p.center.cwd, binding, sessions.ChildResult{Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	updated := make(chan tea.Msg, 1)
	go func() { updated <- next() }()
	select {
	case msg := <-updated:
		p.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("control center did not refresh after the database commit")
	}

	if nodes := p.nodes(); len(nodes) != 1 || nodes[0].id != "sibling" {
		t.Fatalf("working filter after completion: %+v", nodes)
	}
	if _, cmd := p.Update(tea.MouseClickMsg{X: oldButton.X, Y: oldButton.Y, Button: tea.MouseLeft}); cmd != nil {
		t.Fatal("the previous button targeted a running sibling")
	}
	if _, cmd := p.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}); cmd != nil {
		t.Fatal("keyboard cancellation targeted a running sibling")
	}
	p.center.filter = 0
	if view := ansi.Strip(p.View().Content); !strings.Contains(view, "completed") || strings.Contains(view, cancelChildLabel) {
		t.Fatal(view)
	}
}

func runningChildrenPicker(t *testing.T) *picker {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	parent := sessions.Record{ID: "parent", Title: "Parent", Model: "test"}
	if _, err := sessions.Create(cwd, parent); err != nil {
		t.Fatal(err)
	}
	for _, child := range []sessions.Record{
		{ID: "child", Title: "Selected", Model: "test"},
		{ID: "sibling", Title: "Sibling", Model: "test"},
	} {
		binding := sessions.ChildBinding{ParentID: parent.ID, ChildID: child.ID}
		release, err := sessions.StartChild(t.Context(), cwd, binding, child, "test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	p := newPicker(".", routing.Config{})
	p.history, p.width, p.height = true, 92, 23
	p.center.cwd, p.center.selected = cwd, "child"
	records, err := sessions.List(cwd, []string{"test"})
	if err != nil {
		t.Fatal(err)
	}
	p.saved = records
	t.Cleanup(p.stopWatchingChildStatuses)
	return p
}

func TestControlCenterUpdatesAfterChildPermissionDismissal(t *testing.T) {
	p := newPicker(".", routing.Config{})
	p.history = true
	p.saved = []sessions.Record{{ID: "root"}}
	q := &permission{sessionID: "child"}
	m := &terminal{root: bloom.New(), launch: p, h: &harness{record: p.saved[0]}, ask: q, screen: screen{connection: connected}}
	p.center.owner = m
	m.agents.children = []agentDetail{{id: "child", parent: "root", state: "running"}}
	if rows := p.nodes(); rows[1].state != "needs input" {
		t.Fatalf("pending permission: %+v", rows)
	}
	m.Update(dismissedQuestion{q: q})
	if rows := p.nodes(); rows[1].state != "running" {
		t.Fatalf("dismissed permission: %+v", rows)
	}
}
