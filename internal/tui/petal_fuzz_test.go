package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func FuzzPetalFrame(f *testing.F) {
	f.Add(byte(65), byte(27), "# Heading", "route", "account", "idle", "/tmp", "draft")
	f.Add(byte(1), byte(8), "界", "界", "界", "界", "界", "界")
	f.Add(byte(40), byte(9), "", "route\nextra", "", "", "", "")
	f.Fuzz(func(t *testing.T, columns, rows byte, message, model, account, status, cwd, input string) {
		if len(message)+len(model)+len(account)+len(status)+len(cwd)+len(input) > 512 {
			return
		}
		v := screen{connection: connected,
			Width: int(columns % 101), Height: int(rows % 41), Model: model,
			Account: account, Status: status, Cwd: cwd, editor: testComposer(clean(input), 0),
			Messages: []entry{{"assistant", message}},
		}
		if v.Width == 0 || v.Height == 0 {
			if got := petal(v); got != "" {
				t.Fatal("nonempty frame for an empty terminal")
			}
			return
		}
		checkFrame(t, petal(v), v.Width, v.Height)
	})
}

func checkFrame(t *testing.T, frame string, width, height int) {
	t.Helper()
	rows := strings.Split(frame, "\n")
	if len(rows) != height {
		t.Fatalf("got %d rows, want %d", len(rows), height)
	}
	for i, row := range rows {
		if got := ansi.StringWidth(row); got != width {
			t.Fatalf("row %d has %d columns, want %d: %q", i, got, width, ansi.Strip(row))
		}
	}
}

func FuzzPetalBacklog(f *testing.F) {
	f.Add([]byte{0, 0, 2, 0, 1, 6, 4, 0, 2, 3})
	f.Add([]byte{0, 0, 0, 2, 0, 0, 3, 3, 5, 0, 6})
	f.Add([]byte{0, 1, 8, 8, 7, 2, 8, 16, 0, 4})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 64 {
			return
		}
		var viewport backlogViewport
		v := screen{transcriptRevision: 1, connection: connected, backlog: &viewport, markdown: &markdownCache{}}
		var messages []entry
		width, height, end, pause := 24, 3, 0, false
		for i, op := range ops {
			if op%9 == 0 || op%9 == 1 || op%9 == 5 || op%9 == 8 {
				v.transcriptRevision++
			}
			rowCount := len(plainTranscript(messages, width))
			switch op % 9 {
			case 0, 1:
				role := []string{"user", "assistant"}[op%2]
				token := fmt.Sprintf("m%02d", i)
				v.Messages = append(v.Messages, entry{role, token})
				messages = append(messages, entry{role, token})
			case 2, 3:
				delta := 1 + int(op/9)%8
				if op%9 == 2 {
					delta = -delta
				}
				viewport.move(delta)
				// Model scrolling by moving the lower edge of a full window.
				end = min(rowCount, max(min(height, rowCount), end+delta))
				pause = end < rowCount
			case 4:
				viewport.browsing = false
				pause = false
			case 5:
				v.Messages, messages = nil, nil
			case 6:
				newHeight := 1 + int(op/9)%10
				end += newHeight - height
				height = newHeight
			case 7:
				width = 3 + int(op/9)%22
			case 8:
				last := len(messages) - 1
				if last >= 0 && len(messages[last].Text) < 15 {
					suffix := fmt.Sprintf("x%02d", i)
					v.Messages[last].Text += suffix
					messages[last].Text += suffix
				}
			}
			rows := plainTranscript(messages, width)
			if !pause || end >= len(rows) {
				end, pause = len(rows), false
			}
			start := max(0, end-height)
			got := transcript(v, width, height)
			want := rows[start:end]
			if len(got) != len(want) || viewport.browsing != pause {
				t.Fatalf("op %d: got %d rows and paused=%v, want %d rows and paused=%v", i, len(got), viewport.browsing, len(want), pause)
			}
			for j, row := range got {
				visible := entry{row.Role, strings.TrimSpace(ansi.Strip(row.Text))}
				if visible != want[j] {
					t.Fatalf("op %d row %d: got %#v, want %#v", i, j, visible, want[j])
				}
			}
		}
	})
}

// The generated messages are ASCII words, so each byte occupies one cell.
func plainTranscript(messages []entry, width int) []entry {
	var rows []entry
	for i, message := range messages {
		if i > 0 {
			rows = append(rows, entry{})
		}
		for start := 0; start < len(message.Text); start += width {
			end := min(len(message.Text), start+width)
			rows = append(rows, entry{message.Role, message.Text[start:end]})
		}
	}
	return rows
}

func FuzzPetalMarkdownCache(f *testing.F) {
	f.Add("# A\n\n**bold**", "~~strike~~", []byte{0, 1, 4, 5, 0, 4})
	f.Add("| a | b |\n| - | - |\n| 界 | 🙂 |", "`something`", []byte{0, 255, 0, 3, 0})
	f.Add("old", "new", append(append([]byte{0}, []byte(strings.Repeat("\x03", 193))...), 0))
	f.Fuzz(func(t *testing.T, first, second string, ops []byte) {
		if len(first)+len(second) > 384 || len(ops) > 200 {
			return
		}
		v := screen{connection: connected, markdown: &markdownCache{}}
		for i, op := range ops {
			source := first
			if op&1 != 0 {
				source = second
			}
			if op&2 != 0 {
				source += fmt.Sprintf("\n\nRevision %d", i)
			}
			width := 1 + int(op>>2)
			e := entry{"assistant", source}
			got := v.messageLines(0, e, width)
			want := (screen{}).messageLines(0, e, width)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("op %d at width %d: cached render differs from a fresh renderer", i, width)
			}
		}
	})
}

func FuzzPetalReader(f *testing.F) {
	f.Add(byte(80), byte(27), byte(12), byte(2))
	f.Add(byte(11), byte(10), byte(3), byte(0))
	f.Add(byte(2), byte(2), byte(1), byte(0))
	f.Fuzz(func(t *testing.T, columns, rows, count, scroll byte) {
		width, height := 1+int(columns%100), 1+int(rows%42)
		m := terminal{reading: true}
		v := screen{connection: connected, Width: width, Height: height, markdown: &markdownCache{}}
		for i := range int(count % 20) {
			v.Messages = append(v.Messages, entry{"assistant", fmt.Sprintf("Q%02dZ", i)})
		}
		m.renderReader(v)
		m.readerBacklog.move(-int(scroll % 30))
		frame := m.renderReader(v)
		checkFrame(t, frame, width, height)
		if len(v.Messages) == 0 || width < 25 || height < 12 {
			return
		}
		// A short message occupies one row, with one separator between messages.
		// At these heights the header has a spacer and the footer has a divider.
		page := height - 4
		if height >= 24 {
			page-- // Roomy terminals leave a row beneath the shortcuts.
		}
		total := 2*len(v.Messages) - 1
		top := max(0, total-page-int(scroll%30))
		want := make([]string, 0)
		for i := top; i < min(total, top+page); i++ {
			if i%2 == 0 {
				want = append(want, fmt.Sprintf("Q%02dZ", i/2))
			}
		}
		var got []string
		for _, line := range strings.Split(ansi.Strip(frame), "\n") {
			for _, token := range strings.Fields(line) {
				if len(token) == 4 && strings.HasPrefix(token, "Q") && strings.HasSuffix(token, "Z") {
					got = append(got, token)
				}
			}
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("visible reader messages = %v, want %v\n%s", got, want, ansi.Strip(frame))
		}
	})
}

func FuzzPetalMarkdownContent(f *testing.F) {
	f.Add(byte(2), []byte{0, 1, 2, 3, 4})
	f.Add(byte(38), []byte{5, 3, 3, 0, 2})
	f.Fuzz(func(t *testing.T, columns byte, fragments []byte) {
		if len(fragments) > 24 {
			return
		}
		forms := [...]struct{ markdown, visible string }{
			{"**Bold**", "Bold"},
			{"*Italic*", "Italic"},
			{"`literal`", "literal"},
			{"界", "界"},
			{"👩‍💻", "👩‍💻"},
			{"[Label](https://example.org/)", "Labelhttps://example.org/"},
		}
		var source, want strings.Builder
		for _, fragment := range fragments {
			form := forms[int(fragment)%len(forms)]
			source.WriteString(form.markdown + " ")
			want.WriteString(form.visible)
		}
		width := 2 + int(columns%79)
		rows := (screen{}).messageLines(0, entry{"assistant", source.String()}, width)
		for _, row := range rows {
			if actual := ansi.StringWidth(row); actual > width {
				t.Fatalf("rendered %d columns at width %d: %q", actual, width, ansi.Strip(row))
			}
		}
		plain := ansi.Strip(strings.Join(rows, "\n"))
		if got := strings.Join(strings.Fields(plain), ""); got != want.String() {
			t.Fatalf("rendered %q, want %q", got, want.String())
		}
	})
}
