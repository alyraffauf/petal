package tui

import (
	"strings"
	"testing"

	upstream "charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

func TestComposerWrapMatchesBubbles(t *testing.T) {
	for _, width := range []int{2, 7, 80} {
		for _, value := range []string{strings.Repeat("a", 161), "go  界 e\u0301\t👩‍💻 ascii", "abc\n def 🇯🇵 xyz"} {
			editor := newComposer()
			original := upstream.New()
			original.Prompt = ""
			original.ShowLineNumbers = false
			original.SetVirtualCursor(false)
			original.Focus()
			original.MaxWidth = 0
			original.SetWidth(width)
			editor.SetWidth(width)
			editor.SetValue(value)
			original.SetValue(value)
			for range len([]rune(value)) + 1 {
				if upstream.LineInfo(editor.LineInfo()) != original.LineInfo() || editor.Column() != original.Column() || editor.Line() != original.Line() {
					t.Fatalf("width %d, value %q: composer row %d col %d %+v, bubbles row %d col %d %+v", width, value, editor.Line(), editor.Column(), editor.LineInfo(), original.Line(), original.Column(), original.LineInfo())
				}
				left := tea.KeyPressMsg{Code: tea.KeyLeft}
				editor.Model, _ = editor.Update(left)
				original, _ = original.Update(left)
			}
		}
	}
}
