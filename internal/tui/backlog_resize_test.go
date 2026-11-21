package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestResizeKeepsVisibleMessage(t *testing.T) {
	for _, body := range []string{
		"始まり\n\n" + strings.Repeat("界", 60),
		"| name | value |\n| --- | --- |\n| 界 | start |",
		"```go\nfirst()\nsecond()\n```",
	} {
		v := screen{markdown: &markdownCache{}, backlog: &backlogViewport{}, transcriptRevision: 1}
		v.Messages = []entry{{"user", strings.Repeat("older ", 100)}, {"assistant", body}, {"user", "tail"}}
		v.planRows = []entry{{"assistant", "stream"}}
		// From the tail, cross the streaming row, two separators and the tail message.
		transcript(v, 40, 1)
		v.backlog.move(-4)
		old := transcript(v, 40, 1)
		oldLines := v.messageLines(1, v.Messages[1], 40)
		row := len(oldLines) - 1
		if row < 0 || old[0].Text != oldLines[row] {
			t.Fatal("scroll did not reach the last row of the anchored message")
		}
		v.planRows[0].Text = "stream updated"
		v.transcriptRevision++
		got := transcript(v, 25, 1)
		lines := v.messageLines(1, v.Messages[1], 25)
		if got[0] != (entry{"assistant", lines[min(row, len(lines)-1)]}) {
			t.Fatalf("resize moved off the anchored row for %q", body)
		}
		v.backlog.move(1000)
		if tail := transcript(v, 25, 1); v.backlog.browsing || !strings.Contains(tail[0].Text, "stream updated") {
			t.Fatalf("failed to return to streaming tail: %v", tail)
		}
	}
}

func BenchmarkBacklogResize(b *testing.B) {
	for _, n := range []int{400, 4000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			v := screen{markdown: &markdownCache{}, backlog: &backlogViewport{}, transcriptRevision: 1}
			for i := range n {
				v.Messages = append(v.Messages, entry{"assistant", fmt.Sprintf("### item %d\n\nRead `file.go` and **continue**.", i)})
			}
			for b.Loop() {
				b.StopTimer()
				v.markdown = &markdownCache{}
				v.backlog = &backlogViewport{}
				transcript(v, 80, 20)
				v.backlog.move(-2)
				transcript(v, 80, 20)
				b.StartTimer()
				transcript(v, 81, 20)
			}
		})
	}
}
