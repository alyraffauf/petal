package tui

import (
	"bloomfx.dev/bloom"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
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
	for _, count := range []int{400, 4000} {
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

func BenchmarkOpenBacklog(b *testing.B) {
	for _, count := range []int{400, 4000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			v := screen{}
			for i := range count {
				v.Messages = append(v.Messages, entry{"assistant", fmt.Sprintf("### Item %d\n\nRead `file.go` and **continue**.", i)})
			}
			for b.Loop() {
				v.markdown = &markdownCache{}
				transcript(v, 80, 20)
			}
		})
	}
}

func BenchmarkOpenLargeToolResult(b *testing.B) {
	for _, count := range []int{1024, 16384} {
		b.Run(strconv.Itoa(count)+"_lines", func(b *testing.B) {
			body := logFence + strings.Repeat("build output line\n", count) + "```"
			e, _ := (&terminal{}).toolResult(toolEvent{id: "Build", status: "completed", body: &body})
			v := screen{Messages: []entry{{e.role, e.text}}}
			for b.Loop() {
				v.markdown = &markdownCache{}
				transcript(v, 80, 20)
			}
		})
	}
}

func TestMultilineCodeMatchesGlamour(t *testing.T) {
	style := styles.DarkStyleConfig
	wantRenderer, err := glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(60), glamour.WithTableWrap(true), glamour.WithEmoji())
	if err != nil {
		t.Fatal(err)
	}
	md := newMarkdownRenderer(style, 60)
	for _, source := range []string{
		"> ```go\n> var a = 1\n> var b = 2\n> ```\n\n*done*",
		"1. An indented block\n\n       a\n       b\n\nAfter\n\n    c\n    d",
		"~~~text\nfirst\nlast",
		"```\n\n\n```\n\n:smile: [link](https://example.com)",
	} {
		want, err := wantRenderer.Render(source)
		if err != nil {
			t.Fatal(err)
		}
		if got := renderMarkdown(md, source); got != want {
			t.Errorf("rendering %q differs from Glamour\ngot %q\nwant %q", source, got, want)
		}
	}
}

func BenchmarkSessionFrame(b *testing.B) {
	for _, count := range []int{400, 4000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			m := terminal{root: bloom.New(), screen: screen{
				connection: connected, Busy: true, Width: 100, Height: 30,
				markdown: &markdownCache{}, editor: newComposer(),
			}}
			for i := range count {
				m.Messages = append(m.Messages, entry{"assistant", fmt.Sprintf("### Item %d\n\nRead `file.go` and **continue**.", i)})
			}
			m.View()
			for b.Loop() {
				m.Frame++
				m.View()
			}
		})
	}
}

func TestPlainLogScrollMatchesFullLayout(t *testing.T) {
	var body strings.Builder
	for i := range 1050 {
		fmt.Fprintf(&body, "row %04d  \n", i)
	}
	for _, extra := range []string{"", strings.Repeat("word ", 24) + "\n", "wide 界\n", "\n", "```\n# heading\n```text\n"} {
		text := logFence + body.String() + extra + "```"
		e, _ := (&terminal{}).toolResult(toolEvent{id: "Build", status: "completed", body: &text})
		source := e.text
		v := screen{Messages: []entry{{"tool", source}}, backlog: &backlogViewport{}, markdown: &markdownCache{}}
		for _, width := range []int{40, 13, 10} {
			if extra == "" && width == 40 && v.layoutMessage(0, v.Messages[0], width).log == nil {
				t.Fatal("adapter log missed viewport layout")
			}
			want := (&markdownCache{}).renderLines(source, width)
			for _, top := range []int{len(want) - 11, len(want) - 14, 512, 0, 3, len(want) - 11} {
				transcript(v, width, 11)
				v.backlog.move(top - v.backlog.top)
				rows := transcript(v, width, 11)
				if len(rows) != 11 {
					t.Fatalf("got %d rows", len(rows))
				}
				for i, row := range rows {
					if row.Text != want[top+i] {
						t.Fatalf("width %d row %d: got %q want %q", width, top+i, row.Text, want[top+i])
					}
				}
			}
		}
	}
}
