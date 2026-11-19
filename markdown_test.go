package main

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestBothSpeakersRenderMarkdownAtConversationWidth(t *testing.T) {
	v := screen{markdown: &markdownCache{}}
	source := "**Pinned** and `session/load`\n\n| Model | Account |\n|---|---|\n| Sonnet | Studio |\n\n```go\nreturn account\n```"
	for _, role := range []string{"user", "assistant", "tool", "system"} {
		for _, width := range []int{12, 40, 80} {
			rows := v.messageLines(entry{role, source}, width)
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
