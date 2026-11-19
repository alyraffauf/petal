package main

import "strings"

// A turn owns all incoming text until the agent acknowledges the prompt (or
// session/load finishes replaying history). Keep boundaries between speakers,
// message IDs, and tool calls even if their notifications interleave.
type turnBlock struct {
	role, id string
	text     strings.Builder
}
type turnLog struct {
	blocks []*turnBlock
	last   *turnBlock
	ids    map[[2]string]*turnBlock
}

func (t *turnLog) push(e event) {
	var b *turnBlock
	if e.chunk {
		if e.id != "" {
			b = t.ids[[2]string{e.role, e.id}]
		} else if t.last != nil && t.last.id == "" && t.last.role == e.role {
			b = t.last
		}
	}
	if b == nil {
		b = &turnBlock{role: e.role, id: e.id}
		t.blocks = append(t.blocks, b)
		if e.chunk && e.id != "" {
			if t.ids == nil {
				t.ids = make(map[[2]string]*turnBlock)
			}
			t.ids[[2]string{e.role, e.id}] = b
		}
	}
	b.text.WriteString(e.text)
	t.last = nil
	if e.chunk {
		t.last = b
	}
}
func (t *turnLog) commit(dst []entry) []entry {
	for _, b := range t.blocks {
		dst = append(dst, entry{b.role, b.text.String()})
	}
	*t = turnLog{}
	return dst
}

type toolEvent struct {
	id, title, status string
	body              *string
}

func (m *terminal) toolResult(update toolEvent) (event, bool) {
	m.pending.last = nil
	if m.tools == nil {
		m.tools = make(map[string]toolEvent)
	}
	call := m.tools[update.id]
	if update.title != "" {
		call.title = update.title
	}
	if update.body != nil {
		call.body = update.body
	}
	if update.status != "completed" && update.status != "failed" {
		m.tools[update.id] = call
		return event{}, false
	}
	delete(m.tools, update.id)
	if call.title == "" {
		call.title = update.id
	}
	result := "**" + call.title + "** · " + update.status
	if call.body != nil && *call.body != "" {
		result += "\n\n" + *call.body
	}
	return event{role: "tool", text: result}, true
}
