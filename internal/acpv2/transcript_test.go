package acpv2_test

import (
	"encoding/json"
	"slices"
	"testing"

	"petal/internal/acpv2"
)

func apply(t *testing.T, transcript *acpv2.Transcript, wire string, wantChange bool) {
	t.Helper()
	if changed, err := transcript.Apply(json.RawMessage(wire)); err != nil || changed != wantChange {
		t.Fatalf("Apply(%s) = %t, %v; want %t, nil", wire, changed, err, wantChange)
	}
}

func assertEntries(t *testing.T, transcript *acpv2.Transcript, want ...acpv2.Entry) {
	t.Helper()
	if got := transcript.Entries(); !slices.Equal(got, want) {
		t.Fatalf("Entries() = %#v; want %#v", got, want)
	}
}

func TestMessagePatchesKeepFirstSeenOrderAcrossRoleAndClear(t *testing.T) {
	log := acpv2.NewTranscript()
	apply(t, log, `{"sessionUpdate":"agent_message","messageId":"1"}`, false)
	apply(t, log, `{"sessionUpdate":"user_message","messageId":"1","content":[{"type":"text","text":"question"}]}`, true)
	apply(t, log, `{"sessionUpdate":"agent_thought_chunk","messageId":"1","content":{"type":"text","text":"thought"}}`, true)
	apply(t, log, `{"sessionUpdate":"agent_message_chunk","messageId":"1","content":{"type":"text","text":"draft"}}`, true)
	apply(t, log, `{"sessionUpdate":"agent_message","messageId":"1","content":[{"type":"text","text":"answer"}]}`, true)
	apply(t, log, `{"sessionUpdate":"agent_message","messageId":"1","_meta":{"finished":true}}`, false)
	snapshot := log.Entries()
	apply(t, log, `{"sessionUpdate":"agent_message","messageId":"1","content":null}`, true)
	apply(t, log, `{"sessionUpdate":"agent_message","messageId":"1","content":[]}`, false)
	apply(t, log, `{"sessionUpdate":"agent_message_chunk","messageId":"1","content":{"type":"text","text":"new"}}`, true)
	apply(t, log, `{"sessionUpdate":"agent_message_chunk","messageId":"1","content":{"type":"text","text":" text"}}`, true)
	snapshot[0].Text = "caller edit"
	assertEntries(t, log, acpv2.Entry{Role: "assistant", Text: "new text"}, acpv2.Entry{Role: "user", Text: "question"}, acpv2.Entry{Role: "system", Text: "thought"})
}

func TestToolPatchReplacesStreamedContentAndClearsFields(t *testing.T) {
	log := acpv2.NewTranscript()
	apply(t, log, `{"sessionUpdate":"tool_call_content_chunk","toolCallId":"run","content":{"type":"content","content":{"type":"text","text":"streamed"}}}`, true)
	apply(t, log, `{"sessionUpdate":"tool_call_update","toolCallId":"run","name":"exec","title":"Run test","status":"in_progress"}`, true)
	assertEntries(t, log, acpv2.Entry{Role: "tool", Text: "**Run test** · in_progress\n\nstreamed"})
	apply(t, log, `{"sessionUpdate":"tool_call_update","toolCallId":"run","title":null,"status":"completed","content":[{"type":"diff","changes":[{"operation":"move","oldPath":"/old","path":"/new"}]},null,{"type":"terminal","terminalId":"job"}]}`, true)
	assertEntries(t, log, acpv2.Entry{Role: "tool", Text: "**exec** · completed\n\nmove /old → /new\n\n[Terminal job]"})
	apply(t, log, `{"sessionUpdate":"tool_call_update","toolCallId":"run","name":null,"status":null,"content":null}`, true)
	assertEntries(t, log, acpv2.Entry{Role: "tool", Text: "**run** · pending"})
	apply(t, log, `{"sessionUpdate":"tool_call_update","toolCallId":"run","content":null}`, false)
}

func TestTranscriptReplayUsesFreshState(t *testing.T) {
	log := acpv2.NewTranscript()
	chunk := `{"sessionUpdate":"agent_message_chunk","messageId":"x","content":{"type":"text","text":"hi"}}`
	apply(t, log, chunk, true)
	apply(t, log, chunk, true)
	apply(t, log, `{"sessionUpdate":"replay_complete"}`, false)
	for _, invalid := range []string{
		`null`,
		`{"sessionUpdate":"agent_message","content":[]}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"x","content":{"type":"text","text":null}}`,
	} {
		if changed, err := log.Apply(json.RawMessage(invalid)); changed || err == nil {
			t.Fatalf("Apply(%s) = %t, %v; want false and an error", invalid, changed, err)
		}
	}
	apply(t, log, `{"sessionUpdate":"agent_message","messageId":"x","content":{}}`, false)
	assertEntries(t, log, acpv2.Entry{Role: "assistant", Text: "hihi"})
	log.Reset()
	assertEntries(t, log)
	apply(t, log, chunk, true)
	assertEntries(t, log, acpv2.Entry{Role: "assistant", Text: "hi"})
}
