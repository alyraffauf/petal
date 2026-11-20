package tui

import (
	"strings"

	"bloomfx.dev/bloom"
	"github.com/charmbracelet/x/ansi"
)

func readerBodyHeight(h int) int {
	rows := h - 1 - navFooterHeight(h)
	if h >= 12 {
		rows--
	}
	return max(0, rows)
}

func (m *terminal) scene(v screen) bloom.Child {
	if !m.reading {
		return bloom.Text(renderChat(v, &m.chatHits))
	}
	w, h := max(1, v.Width), max(1, v.Height)
	v.backlog = &m.readerBacklog
	v.markdown = &m.readerMarkdown
	rows := []string{petalMasthead(w, 0, "conversation", "reader", false, v.background)}
	if h >= 12 {
		rows = append(rows, richBand("", w, petalText, v.background, false))
	}
	gutter := min(w-1, min(12, max(2, w/4)))
	last := ""
	for _, r := range transcript(v, max(1, w-gutter), readerBodyHeight(h)) {
		label := ""
		if r.Role != last {
			label = " " + sentenceLabel(r.Role)
		}
		last = r.Role
		rows = append(rows, richBand(ansi.Truncate(label, max(0, gutter-1), ""), gutter, petalRose, v.background, true)+richBand(r.Text, w-gutter, petalText, v.background, false))
	}
	for len(rows) < h-navFooterHeight(h) {
		rows = append(rows, richBand("", w, petalText, v.background, false))
	}
	if len(rows) < h {
		rows = appendNavFooter(rows, w, h, shortHelp(w, closeReader, pageEarlier, pageLater, jumpLatest), v.background)
	}
	return bloom.Text(strings.Join(rows, "\n"))
}
