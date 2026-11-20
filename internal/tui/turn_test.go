package tui

import (
	"encoding/json"
	"reflect"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
)

func TestTurnPublishesWholeMessages(t *testing.T) {
	for _, reason := range []string{"end_turn", "cancelled"} {
		m := terminal{root: bloom.New(), screen: screen{Busy: true}}
		m.Update(event{role: "assistant", chunk: true, id: "first", text: "**one"})
		title := "Read file"
		m.Update(event{tool: &toolEvent{id: "read", title: &title, status: "in_progress"}})
		if len(m.Messages) != 0 {
			t.Fatalf("uncompleted content became visible: %v", m.Messages)
		}
		body := "result"
		m.Update(event{tool: &toolEvent{id: "read", status: "completed", body: &body}})
		m.Update(event{role: "assistant", chunk: true, id: "first", text: "**"})
		m.Update(event{role: "assistant", chunk: true, id: "second", text: "two"})
		tool := entry{"tool", "**Read file** · completed\n\nresult"}
		if !reflect.DeepEqual(m.Messages, []entry{tool}) {
			t.Fatalf("before turn completion: %v", m.Messages)
		}
		m.Update(finished{reason: reason})
		want := []entry{{"assistant", "**one**"}, tool, {"assistant", "two"}}
		if reason == "cancelled" {
			want = []entry{tool, {"system", "Turn stopped · cancelled"}}
		}
		if !reflect.DeepEqual(m.Messages, want) {
			t.Fatalf("%s: got %v, want %v", reason, m.Messages, want)
		}
	}
}

func TestToolNullContentPreservesPriorOutput(t *testing.T) {
	m := terminal{root: bloom.New(), screen: screen{Busy: true}}
	h := harness{events: make(chan tea.Msg, 1)}
	for _, update := range []string{
		`{"sessionUpdate":"tool_call","toolCallId":"read","title":"Read","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"result"}}]}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"read","status":"completed","content":null}`,
	} {
		if _, err := h.callback(t.Context(), "session/update", json.RawMessage(`{"update":`+update+`}`)); err != nil {
			t.Fatal(err)
		}
		m.Update(<-h.events)
	}
	m.Update(finished{reason: "end_turn"})
	want := []entry{{"tool", "**Read** · completed\n\nresult"}}
	if !reflect.DeepEqual(m.Messages, want) {
		t.Fatalf("got %v, want %v", m.Messages, want)
	}
}

func TestToolEmptyTitleReplacesPriorTitle(t *testing.T) {
	m := terminal{root: bloom.New(), screen: screen{Busy: true}}
	h := harness{events: make(chan tea.Msg, 1)}
	for _, update := range []string{
		`{"sessionUpdate":"tool_call","toolCallId":"read","title":"Read","status":"in_progress"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"read","title":"","status":"completed"}`,
	} {
		if _, err := h.callback(t.Context(), "session/update", json.RawMessage(`{"update":`+update+`}`)); err != nil {
			t.Fatal(err)
		}
		m.Update(<-h.events)
	}
	m.Update(finished{reason: "end_turn"})
	want := []entry{{"tool", "**read** · completed"}}
	if !reflect.DeepEqual(m.Messages, want) {
		t.Fatalf("got %v, want %v", m.Messages, want)
	}
}

func TestTokenLimitPublishesStreamedReply(t *testing.T) {
	m := terminal{root: bloom.New(), screen: screen{Busy: true}}
	h := harness{events: make(chan tea.Msg, 1)}
	for _, chunk := range []string{"Partial ", "reply"} {
		applyTranscriptUpdate(t, &m, &h, map[string]any{
			"sessionUpdate": "agent_message_chunk", "messageId": "reply",
			"content": map[string]string{"type": "text", "text": chunk},
		})
	}
	m.Update(finished{reason: "max_tokens"})

	want := []entry{{"assistant", "Partial reply"}, {"system", "Turn stopped · max tokens"}}
	if m.Busy || !reflect.DeepEqual(m.Messages, want) {
		t.Fatalf("busy=%t, messages=%q, want busy=false, messages=%q", m.Busy, m.Messages, want)
	}
}
