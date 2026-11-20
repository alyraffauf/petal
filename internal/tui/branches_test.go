package tui

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"petal/internal/routing"
)

func TestForkCanReopenParentOnFreshConnection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := routing.Config{Model: "model", Accounts: map[string]routing.Account{"work": {Models: map[string][]string{"model": {exe, "-test.run=^TestPropertyACPAdapter$"}}, Env: map[string]string{"PETAL_TEST_ADAPTER_PROFILE": "work"}}}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	open := func(id string) *terminal {
		h := startHarness(ctx, cfg)
		t.Cleanup(h.close)
		h.id = id
		if _, err := h.open(ctx, cfg, project, id != ""); err != nil {
			t.Fatal(err)
		}
		return &terminal{ctx: ctx, h: h, cfg: cfg, root: bloom.New(), screen: screen{connection: connected, Cwd: project, editor: newComposer()}}
	}
	parent := open("")
	if !parent.h.canFork {
		t.Fatal("fork not negotiated")
	}
	parent.Update(parent.runCommand(chatCommand{name: "/fork"})())
	if parent.next == nil || parent.next.id == parent.h.id {
		t.Fatal("fork did not select a new thread")
	}
	fork := open(parent.next.id)
	fork.runCommand(chatCommand{name: "/parent"})
	if fork.next == nil || fork.next.id != parent.h.id || !fork.next.resume {
		t.Fatal("parent route lost after restart")
	}
	open(fork.next.id)
}

func TestAgentReaderKeepsChildOutputOutOfParentTurn(t *testing.T) {
	root := "parent"
	h := &harness{events: make(chan tea.Msg, 1)}
	h.activeID.Store(&root)
	m := terminal{h: h, ctx: t.Context(), root: bloom.New(), screen: screen{connection: connected, Busy: true}}
	send := func(id, body string) {
		t.Helper()
		_, err := h.callback(t.Context(), "session/update", json.RawMessage(`{"sessionId":"`+id+`","update":`+body+`}`))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case msg := <-h.events:
			m.Update(msg)
		default:
			t.Fatal("missing update")
		}
	}
	send(root, `{"sessionUpdate":"subagent_update","sessionId":"child","title":"Tests","description":"Check suite","state":{"state":"running"}}`)
	send("child", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"suite passed"}}`)
	rows := m.agents.details(0)
	if len(rows) > 0 && strings.Contains(rows[len(rows)-1].Text, "suite passed") {
		t.Fatal("partial reply displayed")
	}
	send(root, `{"sessionUpdate":"subagent_update","sessionId":"child","state":{"state":"idle"}}`)
	m.Update(finished{reason: "end_turn"})
	rows = m.agents.details(0)
	if rows[len(rows)-1].Text != "suite passed" || len(m.Messages) != 0 {
		t.Fatalf("child %v, parent %v", rows, m.Messages)
	}
}
