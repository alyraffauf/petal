package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"petal/internal/sessions"
)

func TestManualCompactionsAreDurableBeforeAnotherTurn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	var recorder transcriptRecorder
	if _, err := recorder.open("key", "thread", cwd, "work", "model"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second", "second"} {
		recorder.observe("key", 1, Object{"sessionUpdate": encoded("compaction_update"), "compactionId": encoded(id), "status": encoded("completed")})
	}
	recorder.observe("key", 1, Object{"sessionUpdate": encoded("compaction_summary_chunk"), "compactionId": encoded("second"), "content": json.RawMessage(`{"type":"text","text":"late receipt"}`)})
	rows, err := sessions.Transcript(cwd, "thread")
	if err != nil || len(rows) != 3 || !strings.Contains(rows[2].Body, "late receipt") || rows[0].Role != "compaction" || rows[1].Role != "compaction" {
		t.Fatalf("archived compactions: %+v, %v", rows, err)
	}
}
