package tui

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"strings"
)

func snippetText(s string) string {
	var b strings.Builder
	for _, part := range strings.SplitAfter(s, "\x02") {
		before, after, found := strings.Cut(strings.TrimSuffix(part, "\x02"), "\x01")
		b.WriteString(clean(before))
		if found {
			b.WriteString("\x1b[1;38;2;" + petalRose + "m" + clean(after) + "\x1b[0m")
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (p *picker) transcriptView() tea.View {
	s := p.transcripts
	w, h := max(1, p.width), max(1, p.height)
	rows := petalMasthead(w, mastheadHeight(w, h), 0, "Transcripts", p.center.project, false, p.background)
	bodyEnd := h - navFooterHeight(h)
	row := func(text, fg, bg string, bold bool) {
		if len(rows) < bodyEnd {
			rows = append(rows, richBand(text, w, fg, bg, bold))
		}
	}
	hint := "↑↓ Select  Enter Read  Esc Threads"
	if s.reader != nil {
		hit := s.hits[s.index]
		row(" "+petalLabel(hit.Title+" · "+hit.Model+" / "+hit.Account), petalRose, p.background, true)
		v := screen{Messages: s.reader, markdown: &s.markdown, backlog: &s.backlog, background: p.background}
		gutter := min(12, w/4)
		width := max(1, w-gutter)
		if s.anchor >= 0 {
			s.backlog = backlogViewport{first: s.anchor, width: width, browsing: true}
			_, match, _ := strings.Cut(hit.Snippet, "\x01")
			match, _, _ = strings.Cut(match, "\x02")
			for i, line := range v.messageLines(s.anchor, s.reader[s.anchor], width) {
				if strings.Contains(strings.ToLower(ansi.Strip(line)), strings.ToLower(match)) {
					s.backlog.top = i
					break
				}
			}
			s.anchor = -1
		}
		last := ""
		for _, r := range transcript(v, width, bodyEnd-len(rows)) {
			role := ""
			if r.Role != last {
				role = " " + sentenceLabel(r.Role)
			}
			last = r.Role
			rows = append(rows, richBand(role, gutter, petalRose, p.background, true)+richBand(r.Text, width, petalText, p.background, false))
		}
		hint = "↑↓ Scroll  Pg↑ Pg↓ Page  Esc Results"
	} else {
		s.input.SetWidth(max(1, w-12))
		row(s.input.View(), petalText, p.background, false)
		if s.status != "" {
			row("  "+petalLabel(s.status), petalMuted, p.background, false)
		}
		if len(s.hits) > 0 {
			status := fmt.Sprintf("  Matches %d–%d", s.offset+1, s.offset+len(s.hits))
			if s.more || s.offset > 0 {
				hint = "↑↓ Select  Enter Read  Pg↑ Pg↓ Results  Esc Threads"
			}
			row(status, petalMuted, p.background, false)
		}
		s.firstY = len(rows)
		size := max(1, (bodyEnd-len(rows))/2)
		s.first = max(0, s.index-size+1)
		for i := s.first; i < min(len(s.hits), s.first+size); i++ {
			hit := s.hits[i]
			title := hit.Title
			if title == "" {
				title = hit.Model
			}
			bg := p.background
			if i == s.index {
				bg = selectionBand
			}
			row("  "+petalLabel(title+" · "+sentenceLabel(hit.Role)+" · "+hit.Account), petalText, bg, true)
			row("    "+snippetText(hit.Snippet), petalMuted, bg, false)
		}
	}
	for len(rows) < bodyEnd {
		row("", petalText, p.background, false)
	}
	rows = appendNavFooter(rows, w, h, styledHint(hint, p.background), p.background)
	v := tea.NewView(strings.Join(rows, "\n"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
