package proxy

import (
	"encoding/json"
	"petal/internal/sessions"
	"testing"
)

func TestNativeChildAndMailboxArchivedWithoutTerminal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	p := &proxy{recorder: &transcriptRecorder{}}
	id := p.publicID("work", "root")
	p.recorder.open(id, id, cwd, "work", "model")
	send := func(native, data string) {
		t.Helper()
		var u Object
		if err := json.Unmarshal([]byte(data), &u); err != nil {
			t.Fatal(err)
		}
		if !p.observeNativeTranscript("work", native, u) {
			t.Fatal("update missed", data)
		}
	}
	send("root", `{"sessionUpdate":"subagent_update","sessionId":"child","title":"Tests"}`)
	send("child", `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"passed"}}`)
	send("root", `{"sessionUpdate":"subagent_update","sessionId":"child","state":{"state":"idle"}}`)
	send("root", `{"sessionUpdate":"session_message","messageId":"m","content":[{"type":"text","text":"test"}]}`)
	send("root", `{"sessionUpdate":"session_message_chunk","messageId":"m","content":{"type":"text","text":" results"}}`)
	p.recorder.flush()
	p.recorder.flush()
	rows, err := sessions.Transcript(cwd, id)
	if err != nil || len(rows) != 3 || rows[0].Role != "subagent" || rows[0].Body != "**Tests** · assistant\n\npassed" || rows[2].Role != "message" || rows[2].Body != "test results" {
		t.Fatalf("archive = %+v, %v", rows, err)
	}
}
