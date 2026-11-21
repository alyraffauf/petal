package acpv2_test

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"petal/internal/acpv2"
)

type historyEvent struct {
	identity int
	action   byte
	value    string
}

var wireKinds = [...]string{"agent_message", "user_message", "agent_thought", "tool_call_update"}
var displayRoles = [...]string{"assistant", "user", "system", "tool"}

func (event historyEvent) wire() json.RawMessage {
	group := event.identity / 3
	id := [...]string{"", "1", "2"}[event.identity%3]
	fields := map[string]any{"sessionUpdate": wireKinds[group], "messageId": id}
	if group == 3 {
		delete(fields, "messageId")
		fields["toolCallId"] = id
	}
	block := map[string]any{"type": "text", "text": event.value}
	if group == 3 {
		block = map[string]any{"type": "content", "content": block}
	}
	switch event.action {
	case 0:
		fields["content"] = block
		fields["sessionUpdate"] = wireKinds[group] + "_chunk"
		if group == 3 {
			fields["sessionUpdate"] = "tool_call_content_chunk"
		}
	case 1:
		fields["content"] = []any{block}
	case 2:
		fields["content"] = nil
	case 3:
		fields["content"] = []any{}
	case 4:
		fields["content"] = 5
	case 5:
		fields["title"] = event.value
	case 6:
		fields["title"] = nil
	case 7:
		fields["name"] = event.value
	case 8:
		fields["name"] = nil
	case 9:
		fields["status"] = "completed"
	case 10:
		fields["status"] = nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return raw
}

// Recompute from the raw operation history. Each identity's latest replacement
// bounds its surviving content, while labels are the most recent assignment to
// each field. No state from a previous check is carried into this oracle.
func historyEntries(history []historyEvent) []acpv2.Entry {
	var entries []acpv2.Entry
	var seen [12]bool
	for _, first := range history {
		identity := first.identity
		if seen[identity] {
			continue
		}
		seen[identity] = true
		group := identity / 3
		var content []string
		var title, name, status *string
		hasReplacement := false
		for _, event := range slices.Backward(history) {
			if event.identity != identity {
				continue
			}
			switch event.action {
			case 0, 1, 2, 3:
				if !hasReplacement && event.action <= 1 && event.value != "" {
					content = append(content, event.value)
				}
				hasReplacement = hasReplacement || event.action != 0
			case 5, 6:
				if title == nil {
					title = &event.value
				}
			case 7, 8:
				if name == nil {
					name = &event.value
				}
			case 9, 10:
				if status == nil {
					status = &event.value
				}
			}
		}
		slices.Reverse(content)
		text := strings.Join(content, "")
		if group == 3 {
			label := [...]string{"Tool call", "1", "2"}[identity%3]
			if name != nil && *name != "" {
				label = *name
			}
			if title != nil && *title != "" {
				label = *title
			}
			state := "pending"
			if status != nil && *status != "" {
				state = *status
			}
			header := "**" + label + "** · " + state
			if text != "" {
				header += "\n\n" + text
			}
			text = header
		}
		if text != "" {
			entries = append(entries, acpv2.Entry{Role: displayRoles[group], Text: text})
		}
	}
	return entries
}

func FuzzTranscriptHistory(f *testing.F) {
	f.Add([]byte{0, 1, 0, 2, 0, 3, 0, 4, 0})
	f.Add([]byte{9, 5, 2, 9, 0, 3, 9, 6, 0, 9, 2, 0})
	f.Fuzz(checkTranscriptHistory)
}

func TestTranscriptHistoryLongTraces(t *testing.T) {
	random := rand.NewPCG(17, 29)
	for trace := 0; trace < 100; trace++ {
		var data [384]byte
		for step := 0; step < len(data); step += 3 {
			data[step] = byte(random.Uint64() % 12)
			data[step+1] = byte(random.Uint64() % 16)
			if trace%2 == 0 && data[step+1] == 11 {
				data[step+1] = 0
			}
			data[step+2] = byte(random.Uint64() % 6)
		}
		t.Run(strconv.Itoa(trace), func(t *testing.T) { checkTranscriptHistory(t, data[:]) })
	}
}

func checkTranscriptHistory(t *testing.T, data []byte) {
	log := acpv2.NewTranscript()
	var history []historyEvent
	for step := 0; step+2 < len(data) && step < 384; step += 3 {
		value := [...]string{"", "α", "one", " two", "\n", "x\ny"}[data[step+2]%6]
		event := historyEvent{int(data[step] % 12), data[step+1] % 16, value}
		before := historyEntries(history)
		switch event.action {
		case 11:
			log.Reset()
			history = nil
		case 12, 13, 14:
			wire := [...]string{
				`{"sessionUpdate":"replay_complete"}`,
				`{"sessionUpdate":"_future","messageId":"1"}`,
				`{"sessionUpdate":"tool_call_content_chunk","toolCallId":"1","content":null}`,
			}[event.action-12]
			changed, err := log.Apply(json.RawMessage(wire))
			if changed || (err != nil) != (event.action == 14) {
				t.Fatalf("step %d: Apply(%s) = %t, %v", step/3, wire, changed, err)
			}
		case 15:
			// Replay the current generation into an isolated projection.
			replay := acpv2.NewTranscript()
			for _, earlier := range history {
				if _, err := replay.Apply(earlier.wire()); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(replay.Entries(), before) {
				t.Fatalf("step %d: replay = %#v; want %#v", step/3, replay.Entries(), before)
			}
		default:
			if event.action == 6 || event.action == 8 || event.action == 10 {
				event.value = ""
			}
			if event.action == 9 {
				event.value = "completed"
			}
			history = append(history, event)
			changed, err := log.Apply(event.wire())
			wantChange := !reflect.DeepEqual(before, historyEntries(history))
			if err != nil || changed != wantChange {
				t.Fatalf("step %d: Apply(%s) = %t, %v; want %t, nil. History: %#v", step/3, event.wire(), changed, err, wantChange, history)
			}
		}
		if got, want := log.Entries(), historyEntries(history); !reflect.DeepEqual(got, want) {
			t.Fatalf("step %d: Entries() = %#v; want %#v. History: %#v", step/3, got, want, history)
		}
	}
}
