package tui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"petal/internal/sessions"
)

func TestSessionUpdatesSeparateTimelineAndLiveState(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	h := &harness{cwd: cwd, record: sessions.Record{ID: "thread", Model: "model"}, events: make(chan tea.Msg, 1)}
	if _, err := sessions.Create(cwd, h.record); err != nil {
		t.Fatal(err)
	}
	settings := []sessions.Selection{{ID: "model", Value: "high"}}
	if _, err := sessions.Update(cwd, h.record.ID, func(record *sessions.Record) bool {
		record.Selections = settings
		return true
	}); err != nil {
		t.Fatal(err)
	}
	m := terminal{ctx: t.Context(), h: h, root: bloom.New(), screen: screen{Busy: true, connection: connected}}
	update := func(payload string) {
		t.Helper()
		_, err := h.callback(t.Context(), "session/update", json.RawMessage(`{"sessionId":"thread","update":`+payload+`}`))
		if err != nil {
			t.Fatal(err)
		}
		m.Update(<-h.events)
	}
	update(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"before"}}`)
	update(`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":{"type":"text","text":"partial"}}`)
	update(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"after"}}`)
	update(`{"sessionUpdate":"compaction_update","compactionId":"c","status":"completed","summary":[{"type":"text","text":"full"}],"_meta":{"kept":true}}`)
	update(`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":{"type":"text","text":" summary"}}`)
	update(`{"sessionUpdate":"notice","severity":"_custom","title":"Live notice","description":"details"}`)
	update(`{"sessionUpdate":"usage_update","used":10,"size":100,"cost":{"amount":1.25,"currency":"USD"}}`)
	update(`{"sessionUpdate":"session_info_update","title":"Thread title","updatedAt":"2024-01-12T08:00:00Z"}`)
	m.Update(finished{reason: "end_turn"})
	if len(m.Messages) != 3 || m.Messages[1] != (entry{compactionRole, "c"}) || m.Messages[0].Text != "before" || m.Messages[2].Text != "after" {
		t.Fatalf("timeline positions: %v", m.Messages)
	}
	if s := m.compactions["c"].display; !strings.Contains(s, "full summary") || strings.Contains(s, "partial") {
		t.Fatalf("summary replacement: %s", s)
	}
	update(`{"sessionUpdate":"compaction_update","compactionId":"c","status":"failed","summary":null,"error":"interrupted","_meta":null}`)
	if c := m.compactions["c"]; c.meta != nil || !strings.Contains(c.display, "interrupted") || strings.Contains(c.display, "summary") || len(m.Messages) != 3 {
		t.Fatalf("summary clear: %q (%d entries)", c.display, len(m.Messages))
	}
	if m.notice.Severity != "_custom" || !strings.Contains(usageDetails(m.usage), "1.25 USD") {
		t.Fatalf("live state: %+v %+v", m.notice, m.usage)
	}
	saved, err := sessions.Lookup(cwd, "thread")
	if err != nil || saved.Title != "Thread title" || saved.Updated.Year() != 2024 || !slices.Equal(saved.Selections, settings) {
		t.Fatalf("saved metadata: %+v, %v", saved, err)
	}
	update(`{"sessionUpdate":"session_info_update","title":null}`)
	if m.h.record.Title != "" || m.h.record.Updated.Year() != 2024 {
		t.Fatal("null title did not preserve timestamp")
	}
}
