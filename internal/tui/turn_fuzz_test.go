package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
)

type transcriptFragment struct {
	role, id, text string
	chunk          bool
}

// Partition the complete input by logical message. Anonymous chunks belong
// to runs of adjacent notifications, while explicit IDs can span runs.
func materializeTranscript(fragments []transcriptFragment, publishChunks bool) []entry {
	positions := make(map[string]int)
	var transcript []entry
	for index, fragment := range fragments {
		if !fragment.chunk {
			if fragment.role != "" {
				transcript = append(transcript, entry{fragment.role, fragment.text})
			}
			continue
		}
		if !publishChunks {
			continue
		}
		key := fragment.role + "/" + fragment.id
		if fragment.id == "" {
			start := index
			for start > 0 {
				previous := fragments[start-1]
				if !previous.chunk || previous.role != fragment.role || previous.id != "" {
					break
				}
				start--
			}
			key = fmt.Sprintf("%s/anonymous/%d", fragment.role, start)
		}
		position, exists := positions[key]
		if !exists {
			position = len(transcript)
			positions[key] = position
			transcript = append(transcript, entry{Role: fragment.role})
		}
		transcript[position].Text += fragment.text
	}
	return transcript
}

type transcriptToolPatch struct {
	id, status string
	title      *string
	hasTitle   bool
	content    any
	hasContent bool
}

func completedToolText(patches []transcriptToolPatch) string {
	last := patches[len(patches)-1]
	title := last.id
	for index := len(patches) - 1; index >= 0; index-- {
		patch := patches[index]
		if patch.id != last.id {
			continue
		}
		if index != len(patches)-1 && (patch.status == "completed" || patch.status == "failed") {
			break
		}
		if patch.title != nil {
			if *patch.title != "" {
				title = *patch.title
			}
			break
		}
	}
	lines := []string{"**" + title + "** · " + last.status}
	for index := len(patches) - 1; index >= 0; index-- {
		patch := patches[index]
		if patch.id != last.id {
			continue
		}
		if index != len(patches)-1 && (patch.status == "completed" || patch.status == "failed") {
			break
		}
		if patch.content == nil {
			continue
		}
		if output, ok := patch.content.(string); ok {
			lines = append(lines, output)
		}
		break
	}
	return strings.Join(lines, "\n\n")
}

func (patch transcriptToolPatch) notification() map[string]any {
	update := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": patch.id, "status": patch.status}
	if patch.status == "pending" {
		update["sessionUpdate"] = "tool_call"
	}
	if patch.hasTitle {
		update["title"] = patch.title
	}
	if patch.hasContent {
		switch output := patch.content.(type) {
		case string:
			update["content"] = []any{map[string]any{"type": "content", "content": map[string]string{"type": "text", "text": output}}}
		default:
			update["content"] = output
		}
	}
	return update
}

func applyTranscriptUpdate(t *testing.T, m *terminal, h *harness, update map[string]any) {
	t.Helper()
	notification, err := json.Marshal(map[string]any{"sessionId": "session", "update": update})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.callback(t.Context(), "session/update", notification); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-h.events:
		m.Update(message)
	default:
		t.Fatalf("no event for %s", notification)
	}
}

func FuzzACPTranscriptLifecycle(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{173, 64, 10}) // Explicit null content after recorded output.
	f.Add([]byte{5, 92, 10})   // Explicit empty title after a named call.
	f.Add([]byte{0, 9, 4, 8, 6, 5, 9, 7, 10, 0, 6, 11, 0, 5, 12, 8, 4, 13})
	f.Add([]byte{27, 215, 10, 132, 59, 10, 4, 60, 5, 6, 11, 7, 8, 9, 13})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 128 {
			operations = operations[:128]
		}
		h := harness{events: make(chan tea.Msg, 1)}
		prefix := []entry{{"user", "earlier prompt"}, {"assistant", "earlier reply"}}
		m := terminal{root: bloom.New(), screen: screen{Busy: true, Messages: slices.Clone(prefix)}, turnStart: len(prefix)}
		var fragments []transcriptFragment
		var toolPatches []transcriptToolPatch
		check := func(step int, publishChunks bool, note string) {
			t.Helper()
			want := append(slices.Clone(prefix), materializeTranscript(fragments, publishChunks)...)
			if note != "" {
				want = append(want, entry{"system", note})
			}
			if !slices.Equal(m.Messages, want) {
				t.Fatalf("step %d, history %v: got %q, want %q", step, operations, m.Messages, want)
			}
		}
		for index, operation := range operations {
			value := int(operation) / 14
			payload := fmt.Sprintf("é%d ", index)
			switch operation % 14 {
			case 0, 1, 2, 3, 4:
				roles := []string{"assistant", "user", "system"}
				kinds := []string{"agent_message_chunk", "user_message_chunk", "agent_thought_chunk"}
				roleIndex := int(operation%14) % len(roles)
				id := []string{"", "a", "b"}[value%3]
				applyTranscriptUpdate(t, &m, &h, map[string]any{
					"sessionUpdate": kinds[roleIndex], "messageId": id,
					"content": map[string]string{"type": "text", "text": payload},
				})
				fragments = append(fragments, transcriptFragment{roles[roleIndex], id, payload, true})
			case 5, 6, 7, 8, 9:
				status := []string{"pending", "in_progress", "", "completed", "failed"}[operation%14-5]
				patch := transcriptToolPatch{id: []string{"read", "write"}[value%2], status: status}
				switch {
				case status == "pending" || value%8 == 2 || value%8 == 3:
					patch.hasTitle, patch.title = true, &payload
				case value%8 == 6 || value%8 == 7:
					patch.hasTitle, patch.title = true, new("")
				case value%8 == 4 || value%8 == 5:
					patch.hasTitle = true // Explicit null.
				}
				switch value / 4 % 4 {
				case 1:
					patch.hasContent = true // Explicit null.
				case 2:
					patch.hasContent, patch.content = true, []any{}
				case 3:
					patch.hasContent, patch.content = true, payload
				}
				applyTranscriptUpdate(t, &m, &h, patch.notification())
				toolPatches = append(toolPatches, patch)
				fragment := transcriptFragment{}
				if status == "completed" || status == "failed" {
					fragment = transcriptFragment{role: "tool", text: completedToolText(toolPatches)}
				}
				fragments = append(fragments, fragment)
			default:
				var note string
				switch operation % 14 {
				case 10:
					m.Update(finished{reason: "end_turn"})
				case 11:
					if value%2 == 0 {
						m.Update(finished{reason: "cancelled"})
						note = "Turn stopped · cancelled"
					} else {
						m.Update(finished{err: errors.New("prompt failed")})
						note = "prompt failed"
					}
				case 12:
					m.Update(opened{account: "account"})
				case 13:
					m.Update(opened{err: errors.New("load failed")})
					note = "load failed"
				}
				check(index, note == "", note)
				if operation%14 <= 11 && m.Busy {
					t.Fatalf("step %d: finished turn still busy", index)
				}
				if operation%14 == 12 && (m.connection != connected || m.Account != "account") {
					t.Fatalf("step %d: successful replay left session unavailable", index)
				}
				// Start a new lifecycle with the same terminal and ID space.
				prefix = slices.Clone(m.Messages)
				m.turnStart = len(prefix)
				m.Busy = true
				fragments, toolPatches = nil, nil
			}
			check(index, false, "")
		}
		m.Update(finished{})
		check(len(operations), true, "")
	})
}
