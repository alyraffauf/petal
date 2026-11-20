package tui

// Keep the same text column throughout a turn, including image placeholders
// and blank lines inside Markdown. Narrow terminals keep just the glyph.
func roleGutter(width int) int {
	if width >= 36 {
		return 10
	}
	return min(4, max(0, width-1))
}

func transcriptRow(line entry, first bool, width, gutter int, background string) string {
	label, glyph, accent := line.Role, "·", petalMuted
	textColor := petalText
	switch line.Role {
	case "user":
		label, glyph, accent = "you", "●", "210;199;237"
	case "assistant":
		label, glyph, accent = "agent", "✦", petalRose
	case "tool":
		label, glyph, accent = "tool", "└", "173;203;200"
		textColor = petalMuted
	case "system":
		label = "note"
	case "subagent":
		label, glyph, accent = "child", "◇", petalRose
	}
	mark := ""
	if first && line.Role != "" {
		mark = " " + glyph
		if gutter >= 10 {
			mark += " " + label
		}
	}
	return richBand(mark, gutter, accent, background, true) + richBand(line.Text, width-gutter, textColor, background, false)
}
