package tui

import (
	"fmt"
	"image"

	"github.com/charmbracelet/x/ansi"
)

func appendCommandMenu(rows []string, v screen, hits *chatHitTargets, height int, heading bool) []string {
	inset := min(2, v.Width/12)
	first := max(0, v.commandIndex-height+1)
	visible := v.commands[first : first+height]
	// Size the name column to this page. Reserve the action column on
	// every row so descriptions stay aligned as the selection moves.
	width := min(88, v.Width-2*inset)
	nameWidth, descWidth := 0, 0
	for _, c := range visible {
		nameWidth = max(nameWidth, ansi.StringWidth(c.name)+1)
		descWidth = max(descWidth, ansi.StringWidth(petalLabel(c.description))+2)
	}
	nameWidth = min(nameWidth, 24, width/2)
	actionWidth := 0
	if v.Width >= 38 {
		actionWidth = 8
	}
	width = min(width, 2+nameWidth+descWidth+actionWidth)
	if heading {
		count := fmt.Sprintf("  %d–%d of %d", first+1, first+height, len(v.commands))
		label := richBand("commands", 8, petalRose, v.background, true)
		rows = append(rows, richBand(" ", inset+2, petalText, v.background, false)+label+richBand(count, v.Width-inset-10, petalMuted, v.background, false))
		if v.Height >= 22 {
			rows = append(rows, richBand("", v.Width, petalText, v.background, false))
		}
	}
	hits.commands = image.Rect(inset, len(rows), inset+width, len(rows)+height)
	hits.firstCommand = first
	hits.visibleCommands = visible
	for j, c := range hits.visibleCommands {
		selected := first+j == v.commandIndex
		bg, rail, nameColor := v.background, " ", petalText
		action := ""
		if selected {
			bg, rail, nameColor = selectionBand, "▌", petalRose
			hits.actionCommand = c
			if v.Width >= 38 {
				action = " ↵ Run  "
				hits.action = image.Rect(inset+width-ansi.StringWidth(action), len(rows), inset+width, len(rows)+1)
			}
		}

		available := max(0, width-2-actionWidth)
		names := min(nameWidth, available)
		if v.Width-2*inset < 32 {
			names = available
		}
		desc := available - names
		line := richBand("", inset, petalText, v.background, false) + richBand(rail+" ", min(2, width), petalRose, bg, false)
		line += richBand(ansi.Truncate(c.name, names, "…"), names, nameColor, bg, selected)
		line += richBand(ansi.Truncate(" "+petalLabel(c.description), desc, "…"), desc, petalMuted, bg, false)
		line += richBand(action, actionWidth, petalRose, bg, true)
		rows = append(rows, line+richBand("", v.Width-inset-width, petalText, v.background, false))
	}
	return rows
}
