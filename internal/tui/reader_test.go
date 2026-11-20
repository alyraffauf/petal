package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestReaderPageKeepsOneLineOfContext(t *testing.T) {
	for _, height := range []int{10, 23, 36} {
		m := terminal{reading: true, screen: screen{Width: 92, Height: height}}
		for i := range 100 {
			m.Messages = append(m.Messages, entry{"assistant", fmt.Sprintf("message-%02d", i)})
		}
		body := func() []string {
			view := ansi.Strip(m.renderReader(m.screen))
			rows := strings.Split(view, "\n")
			start := 1
			if m.Height >= 12 {
				start++
			}
			return rows[start : len(rows)-navFooterHeight(m.Height)]
		}
		last := body()
		m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
		previous := body()
		if previous[len(previous)-1] != last[0] || slices.Equal(previous, last) {
			t.Fatalf("height %d: page up lost its boundary line", height)
		}
		m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
		if !slices.Equal(body(), last) {
			t.Fatalf("height %d: page down did not return to the same rows", height)
		}
	}
}
