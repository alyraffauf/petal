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
		{Role: "compaction", Body: "first summary", SourceID: "c1"},
		{Role: "user", Body: "superseded question"},
		{Role: "compaction", Body: "latest summary", SourceID: "c2"},
		{Role: "user", Body: "which file?"},
		{Role: "tool", Body: "**rg config** · completed"},
		{Role: "assistant", Body: "main.go"},
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
	for _, stale := range []string{"forgotten question", "first summary", "superseded question"} {
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
