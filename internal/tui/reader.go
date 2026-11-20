package tui

import (
	"strings"
)

func readerBodyHeight(h int) int {
	rows := h - 1 - navFooterHeight(h)
	if h >= 12 {
		rows--
	}
	return max(0, rows)
}

func (m *terminal) renderReader(v screen) string {
	w, h := max(1, v.Width), max(1, v.Height)
	v.backlog = &m.readerBacklog
	v.markdown = &m.readerMarkdown
	rows := []string{petalMasthead(w, 0, "conversation", "reader", false, v.background)}
	if h >= 12 {
		rows = append(rows, richBand("", w, petalText, v.background, false))
	}
	gutter := roleGutter(w)
	last := ""
	for _, r := range transcript(v, max(1, w-gutter), readerBodyHeight(h)) {
		rows = append(rows, transcriptRow(r, r.Role != last, w, gutter, v.background))
		last = r.Role
	}
	for len(rows) < h-navFooterHeight(h) {
		rows = append(rows, richBand("", w, petalText, v.background, false))
	}
	if len(rows) < h {
		rows = appendNavFooter(rows, w, h, shortHelp(w, closeReader, pageEarlier, pageLater, jumpLatest), v.background)
	}
	return strings.Join(rows, "\n")
}
