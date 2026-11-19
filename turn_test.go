package main

import (
	"reflect"
	"testing"

	"bloomfx.dev/bloom"
)

func TestTurnPublishesWholeMessages(t *testing.T) {
	for _, reason := range []string{"end_turn", "cancelled"} {
		m := terminal{root: bloom.New(), screen: screen{Busy: true}}
		m.Update(event{role: "assistant", chunk: true, id: "first", text: "**one"})
		m.Update(event{tool: &toolEvent{id: "read", title: "Read file", status: "in_progress"}})
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
