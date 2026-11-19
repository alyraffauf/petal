package main

import (
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/anim"
	"github.com/charmbracelet/x/ansi"
)

func TestReaderSeparatesTruncatedRoleFromMessage(t *testing.T) {
	m := terminal{reader: anim.NewPresence()}
	now := time.Unix(10, 0)
	m.reader.ShowAt(now)
	m.reader.Advance(now.Add(time.Second))
	v := screen{Width: 38, Height: 16, Messages: []entry{{"assistant", "answer"}}}
	frame := ansi.Strip(bloom.Render(v.Width, v.Height, m.scene(v)))
	for _, row := range strings.Split(frame, "\n") {
		_, interior, found := strings.Cut(row, "│")
		if !found || !strings.Contains(interior, "answer") {
			continue
		}
		fields := strings.Fields(interior)
		if len(fields) >= 2 && strings.HasPrefix("ASSISTANT", fields[0]) && fields[1] == "answer" {
			return
		}
	}
	t.Fatalf("reader message should have a gap after its role:\n%s", frame)
}
