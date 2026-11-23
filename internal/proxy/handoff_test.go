package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"petal/internal/sessions"
)

func TestHandoffCarriesContextSinceLastCompaction(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	id := `["claude","opus","native"]`
	if _, err := sessions.Create(cwd, sessions.Record{ID: id, Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SaveTranscript(cwd, id, []sessions.Message{
		{Role: "user", Body: "forgotten question"},
		{Role: "compaction", Body: "Context compaction completed\n\nfirst summary", SourceID: "c1"},
		{Role: "user", Body: "superseded question"},
		{Role: "compaction", Body: "Context compaction completed\n\nlatest summary", SourceID: "c2"},
		{Role: "user", Body: "which file?"},
		{Role: "tool", Body: "**rg config** · completed"},
		{Role: "assistant", Body: "main.go"},
		{Role: "compaction", Body: "Context compaction failed interrupted", SourceID: "c3"},
		{Role: "compaction", Body: "Context compaction cancelled", SourceID: "c4"},
	}, false); err != nil {
		t.Fatal(err)
	}
	prompt, err := handoffPrompt(cwd, id, json.RawMessage(`[{"type":"text","text":"now fix it"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var blocks []struct{ Type, Text string }
	if err := json.Unmarshal(prompt, &blocks); err != nil || len(blocks) != 2 || blocks[1].Text != "now fix it" {
		t.Fatalf("prompt = %s", prompt)
	}
	carried := blocks[0].Text
	for _, want := range []string{"latest summary", "which file?", "rg config", "main.go"} {
		if !strings.Contains(carried, want) {
			t.Errorf("handoff lacks %q:\n%s", want, carried)
		}
	}
	for _, stale := range []string{"forgotten question", "first summary", "superseded question", "failed", "cancelled"} {
		if strings.Contains(carried, stale) {
			t.Errorf("handoff includes %q from before the last compaction", stale)
		}
	}
	if !(strings.Index(carried, "latest summary") < strings.Index(carried, "which file?") && strings.Index(carried, "which file?") < strings.Index(carried, "main.go")) {
		t.Errorf("handoff is out of order:\n%s", carried)
	}
}

func TestHandoffWithoutHistoryLeavesPromptAlone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	prompt := json.RawMessage(`[{"type":"text","text":"hello"}]`)
	got, err := handoffPrompt(t.TempDir(), `["claude","opus","none"]`, prompt)
	if err != nil || string(got) != string(prompt) {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestClosingSessionKeepsUnsavedV2Turn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	p := &proxy{recorder: &transcriptRecorder{}, version: 2}
	p.Model = "opus"
	id := p.publicID("claude", "native")
	p.recorder.open(id, id, cwd, "claude", "opus")
	// No idle update arrives before the switch closes the session.
	p.recorder.observe(id, 2, Object{"sessionUpdate": encoded("agent_message"), "messageId": encoded("m1"), "content": json.RawMessage(`[{"type":"text","text":"the codename is BLUE-HERON"}]`)})
	finish, err := p.beforeTranscript("session/close", "claude", "native", id, Object{})
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	prompt, err := handoffPrompt(cwd, id, json.RawMessage(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "BLUE-HERON") {
		t.Fatalf("handoff after close lost the unsaved turn: %s", prompt)
	}
}

func TestHandoffKeepsLeadingSkillInvocable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	id := `["claude","opus","native"]`
	if _, err := sessions.Create(cwd, sessions.Record{ID: id, Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SaveTranscript(cwd, id, []sessions.Message{{Role: "assistant", Body: "the codename is BLUE-HERON"}}, false); err != nil {
		t.Fatal(err)
	}
	prompt, err := handoffPrompt(cwd, id, json.RawMessage(`[{"type":"text","text":"$bro"}]`))
	if err != nil {
		t.Fatal(err)
	}
	forwarded, ok := invokeSkill(prompt, map[string]bool{"bro": true})
	var blocks []struct{ Text string }
	if err := json.Unmarshal(forwarded, &blocks); err != nil || !ok || len(blocks) != 2 || blocks[0].Text != "/bro" || !strings.Contains(blocks[1].Text, "BLUE-HERON") {
		t.Fatalf("forwarded %s, want /bro followed by the handoff", forwarded)
	}
}

func TestRecorderReportsUnsavedV2Turn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	r := &transcriptRecorder{}
	id := `["claude","opus","native"]`
	r.open(id, id, t.TempDir(), "claude", "opus")
	if r.unsaved(id) {
		t.Fatal("a new session reports unsaved history")
	}
	r.observe(id, 2, Object{"sessionUpdate": encoded("agent_message"), "messageId": encoded("m1"), "content": json.RawMessage(`[{"type":"text","text":"first turn"}]`)})
	if !r.unsaved(id) {
		t.Fatal("a v2 turn before idle is not reported, so a switch would be refused")
	}
}
