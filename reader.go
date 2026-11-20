package main

import (
	"strings"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/fx"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func (m *terminal) scene(v screen) bloom.Child {
	base := bloom.Text(petal(v))
	if !m.reader.Visible() {
		return base
	}
	w := max(4, min(94, v.Width-4))
	h := max(5, v.Height-4)
	bg := lipgloss.Color("#191725")
	accent := lipgloss.Color("#e3b1ff")
	chrome := lipgloss.NewStyle().Background(bg).Foreground(accent).Bold(true)
	panel := lipgloss.NewStyle().Background(bg).Foreground(lipgloss.Color("#f6efff")).Border(lipgloss.RoundedBorder()).BorderForeground(accent)
	v.backlog = &m.readerBacklog
	v.markdown = &m.readerMarkdown
	gutter := min(11, max(2, w/5))
	rows := transcript(v, max(1, w-gutter-2), max(1, h-4))
	body := make([]string, 0, len(rows))
	last := ""
	for _, r := range rows {
		role := ""
		if r.Role != last {
			role = strings.ToUpper(r.Role)
		}
		last = r.Role
		label := ansi.Truncate(" "+role, gutter-1, "")
		body = append(body, richBand(label, gutter, "208;170;231", "25;23;37", true)+richBand(r.Text, max(1, w-gutter-2), "246;239;255", "25;23;37", false))
	}
	content := bloom.Column(bloom.Fixed(1, bloom.Text("  Conversation").Style(chrome)), bloom.Fill(bloom.Text(strings.Join(body, "\n"))), bloom.Fixed(1, bloom.Text(" "+shortHelp(w-2, closeReader, selectPrevious, selectNext, pageEarlier, pageLater)).Style(chrome)))
	layer := bloom.Layer(bloom.Box(panel, content)).Size(w, h).Modal().Backdrop(fx.Dim{Amount: .65}).Shadow(1, 1, fx.Dim{Amount: .4}).Presence(m.reader, fx.Unfold{})
	return bloom.Overlay(base, layer)
}
