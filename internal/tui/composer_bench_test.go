package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func BenchmarkComposerEdit(b *testing.B) {
	for _, n := range []int{64, 1024, 8192} {
		b.Run(fmt.Sprint(n), func(b *testing.B) { benchmarkComposerEdit(b, strings.Repeat("a", n)) })
	}
	b.Run("code", func(b *testing.B) {
		benchmarkComposerEdit(b, strings.Repeat("\treturn fmt.Sprintf(\"%s\", value)\n", 64)+"a")
	})
	b.Run("unicode", func(b *testing.B) { benchmarkComposerEdit(b, strings.Repeat("界e\u0301👩‍💻", 256)+"a") })
}

func benchmarkComposerEdit(b *testing.B, value string) {
	m := terminal{screen: screen{editor: newComposer()}}
	m.editor.CharLimit = 0
	m.editor.SetWidth(80)
	m.editor.SetValue(value)
	for b.Loop() {
		m.edit(tea.KeyPressMsg{Code: tea.KeyBackspace})
		m.edit(tea.KeyPressMsg{Text: "a", Code: 'a'})
	}
}
