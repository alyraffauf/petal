package main

import (
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"strconv"
	"strings"
	"testing"
)

func TestBothSpeakersRenderMarkdownAtConversationWidth(t *testing.T) {
	v := screen{markdown: &markdownCache{}}
	source := "**Pinned** and `session/load`\n\n| Model | Account |\n|---|---|\n| Sonnet | Studio |\n\n```go\nreturn account\n```"
	for _, role := range []string{"user", "assistant", "tool", "system"} {
		for _, width := range []int{12, 40, 80} {
			rows := v.messageLines(0, entry{role, source}, width)
			result := strings.Join(rows, "\n")
			plain := ansi.Strip(result)
			if plain == result || strings.Contains(plain, "**Pinned**") || strings.Contains(plain, "```go") {
				t.Fatalf("%s contains unrendered Markdown: %q", role, plain)
			}
			for _, token := range []string{"Pinned", "Studio", "return"} {
				if !strings.Contains(strings.Join(strings.Fields(plain), ""), token) {
					t.Errorf("%s lost %s at width %d", role, token, width)
				}
			}
			for _, row := range rows {
				if ansi.StringWidth(row) > width {
					t.Fatalf("%s exceeds %d columns: %q", role, width, row)
				}
			}
		}
	}
}

func TestMarkdownPaddingDoesNotAddRowsToNarrowTranscript(t *testing.T) {
	rows := transcript(screen{Messages: []entry{
		{"user", "**first**"},
		{"assistant", "second"},
	}}, 20, 3)
	var visible []string
	for _, row := range rows {
		visible = append(visible, strings.TrimSpace(ansi.Strip(row.Text)))
	}
	if strings.Join(visible, "|") != "first||second" {
		t.Fatalf("visible rows = %q, want first, separator, second", visible)
	}
}

func BenchmarkWarmBacklog(b *testing.B) {
	for _, count := range []int{192, 193, 400} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			v := screen{markdown: &markdownCache{}}
			for i := range count {
				v.Messages = append(v.Messages, entry{"assistant", fmt.Sprintf("### Item %d\n\nRead `file.go` and **continue**.", i)})
			}
			transcript(v, 80, 20)
			for b.Loop() {
				transcript(v, 80, 20)
			}
		})
	}
}
