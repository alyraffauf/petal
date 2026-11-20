package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"petal/internal/sessions"
)

func TestReadSearchHitInsideLongReply(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := sessions.Create(cwd, sessions.Record{ID: "thread"}); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("Earlier paragraph.\n\n", 70) + "The marigold circuit failed.\n\n" + strings.Repeat("Later paragraph.\n\n", 70)
	if err := sessions.SaveTranscript(cwd, "thread", []sessions.Message{{Role: "assistant", Body: text}}, false); err != nil {
		t.Fatal(err)
	}
	hits, err := sessions.Search(cwd, "", "marigold", 0, 1)
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	s := &transcriptSearch{cwd: cwd, hits: hits}
	s.openHit()
	p := picker{transcripts: s, width: 78, height: 12, background: terminalBackground}
	frame := ansi.Strip(p.View().Content)
	if !strings.Contains(frame, "The marigold circuit failed.") {
		t.Fatal(frame)
	}
}
