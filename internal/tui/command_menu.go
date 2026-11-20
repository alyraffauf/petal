package tui

import (
	"fmt"
	"image"

	"github.com/charmbracelet/x/ansi"
)

func appendCommandMenu(rows []string, v screen, hits *chatHitTargets, height int, heading bool) []string {
	inset := min(2, v.Width/12)
	width := min(76, v.Width-2*inset)
	nameWidth := min(18, width/2)
	if len(v.commands) == 1 {
		c := v.commands[0]
		nameWidth = min(nameWidth, ansi.StringWidth(c.name)+1)
		width = min(width, 2+nameWidth+ansi.StringWidth(petalLabel(c.description))+2+ansi.StringWidth(" ↵ Run "))
	}
	first := max(0, v.commandIndex-height+1)
	if heading {
		count := fmt.Sprintf("%d–%d of %d", first+1, first+height, len(v.commands))
		left := richBand("commands", width-ansi.StringWidth(count)-2, petalRose, v.background, true)
		rows = append(rows, richBand("", inset+2, petalText, v.background, false)+left+richBand(count, ansi.StringWidth(count), petalMuted, v.background, false)+richBand("", v.Width-inset-width, petalText, v.background, false))
	}
	hits.commands = image.Rect(inset, len(rows), inset+width, len(rows)+height)
	hits.firstCommand = first
	hits.visibleCommands = v.commands[first : first+height]
	for j, c := range hits.visibleCommands {
		selected := first+j == v.commandIndex
		bg, rail, nameColor := v.background, " ", petalText
		action := ""
		if selected {
			bg, rail, nameColor = selectionBand, "▌", petalRose
			hits.actionCommand = c
			if v.Width >= 38 {
				action = " ↵ Run "
				hits.action = image.Rect(inset+width-ansi.StringWidth(action), len(rows), inset+width, len(rows)+1)
			}
		}
		actionWidth := ansi.StringWidth(action)
		available := max(0, width-2-actionWidth)
		names := min(nameWidth, available)
		if v.Width-2*inset < 32 {
			names = available
		}
		desc := available - names
		line := richBand("", inset, petalText, v.background, false) + richBand(rail+" ", min(2, width), petalRose, bg, false)
		line += richBand(ansi.Truncate(c.name, names, "…"), names, nameColor, bg, selected)
		line += richBand(ansi.Truncate(" "+petalLabel(c.description), desc, "…"), desc, petalMuted, bg, false)
		line += richBand(action, actionWidth, petalRose, petalSurface, true)
		rows = append(rows, line+richBand("", v.Width-inset-width, petalText, v.background, false))
	}
	return rows
}
