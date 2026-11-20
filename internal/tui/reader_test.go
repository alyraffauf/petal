package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestReaderSeparatesTruncatedRoleFromMessage(t *testing.T) {
	m := terminal{reading: true}
	v := screen{Width: 38, Height: 16, Messages: []entry{{"assistant", "answer"}}}
	frame := ansi.Strip(bloom.Render(v.Width, v.Height, m.scene(v)))
	for _, row := range strings.Split(frame, "\n") {
		if !strings.Contains(row, "answer") {
			continue
		}
		fields := strings.Fields(row)
		if len(fields) >= 2 && strings.HasPrefix("Assistant", fields[0]) && fields[1] == "answer" {
			return
		}
	}
	t.Fatalf("reader message should have a gap after its role:\n%s", frame)
}

func TestReaderPageKeepsOneLineOfContext(t *testing.T) {
	for _, height := range []int{10, 23, 36} {
		m := terminal{reading: true, screen: screen{Width: 92, Height: height}}
		for i := range 100 {
			m.Messages = append(m.Messages, entry{"assistant", fmt.Sprintf("message-%02d", i)})
		}
		body := func() []string {
			view := ansi.Strip(bloom.Render(m.Width, m.Height, m.scene(m.screen)))
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
