package main

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const (
	petalInk     = "19;17;23"
	petalText    = "244;238;244"
	petalRose    = "255;174;211"
	petalMuted   = "177;163;181"
	petalSurface = "49;38;51"
)

// A continuous ribbon uses both half-cells to keep the fold smooth even in a
// two-row masthead. The text sits on the quiet ends of the same surface.
func petalMasthead(w, h, frame int, route, status string) []string {
	rows := make([]string, h)
	phase := float64(frame) * .07
	sample := func(x, y float64) string {
		center := .51 + .46*math.Sin(8*x+phase) + .18*math.Sin(17*x-phase*.55)
		edge := y - center
		envelope := math.Exp(-13 * (x - .55) * (x - .55))
		pearl := envelope * math.Exp(-edge*edge*11)
		blush := envelope * math.Exp(-math.Pow(edge+.48, 2)*3)
		violet := envelope * math.Exp(-math.Pow(edge-.45, 2)*4)
		return fmt.Sprintf("%d;%d;%d", min(255, int(29+161*pearl+73*blush+35*violet)), min(255, int(24+109*pearl+20*blush+19*violet)), min(255, int(35+131*pearl+37*blush+87*violet)))
	}
	for y := range h {
		var b strings.Builder
		for x := range w {
			t := float64(x) / float64(max(1, w-1))
			upper := sample(t, float64(y*2)/float64(max(1, h*2-1)))
			lower := sample(t, float64(y*2+1)/float64(max(1, h*2-1)))
			fmt.Fprintf(&b, "\x1b[38;2;%s;48;2;%sm▀", upper, lower)
		}
		band := b.String() + "\x1b[0m"
		label := "  ✿  Petal"
		if y == 1 {
			label = "     " + route
		}
		if h == 1 {
			label += "   " + route
		}
		right := ""
		if y == 0 {
			right = "  ● " + status + "  "
		}
		rw := min(w/2, ansi.StringWidth(right))
		lw := min(w-rw, ansi.StringWidth(label)+1)
		rows[y] = richBand(label, lw, petalText, sample(0, 0), y == 0) + ansi.Cut(band, lw, w-rw) + richBand(right, rw, petalRose, sample(1, 0), false)
	}
	return rows
}

func petalLabel(s string) string {
	return strings.ReplaceAll(clean(s), "\n", " ")
}

func petal(v screen) string {
	w, h := v.Width, v.Height
	if w < 1 || h < 1 {
		return ""
	}
	footer := 1
	if h >= 8 {
		footer = 2
	}
	header := min(1, h-footer)
	if h >= 27 && w >= 65 {
		header = 2
	}
	route := strings.Trim(petalLabel(v.Model)+" / "+petalLabel(v.Account), " /")
	rows := petalMasthead(w, header, v.Frame, route, petalLabel(v.Status))
	gutter := min(3, w/4)
	col := max(1, w-gutter-2)
	body := transcript(v, col, h-header-footer)
	previous := ""
	for _, line := range body {
		mark, fg, bg := "", petalText, petalInk
		switch line.Role {
		case "user":
			mark = "❯"
			bg = petalSurface
		case "assistant":
			mark = "✿"
		case "tool":
			mark = "↳"
			fg = petalMuted
		case "system":
			mark = "·"
			fg = petalRose
		}
		if line.Role == previous || line.Text == "" {
			mark = ""
		}
		previous = line.Role
		// Tint only the prompt text. Long replies and code blocks retain the
		// entire reading measure, with one shared left edge for both speakers.
		n := min(w-gutter, ansi.StringWidth(line.Text)+1)
		rows = append(rows, richBand(" "+mark, gutter, petalRose, petalInk, true)+richBand(line.Text, n, fg, bg, false)+richBand("", w-gutter-n, fg, petalInk, false))
	}
	for len(rows) < h-footer {
		rows = append(rows, richBand("", w, petalText, petalInk, false))
	}
	input := petalLabel(v.Input)
	fg := petalText
	if input == "" {
		input = "Ask " + petalLabel(v.Model) + "…"
		fg = petalMuted
	}
	action := " ↵ Send "
	aw := 0
	if w >= 38 {
		aw = ansi.StringWidth(action)
	}
	room := max(1, w-aw-4)
	if n := ansi.StringWidth(input); n > room {
		input = ansi.TruncateLeft(input, n-room+1, "…")
	}
	rows = append(rows, richBand("  › "+input, w-aw, fg, petalSurface, false)+richBand(action, aw, petalInk, petalRose, true))
	if footer == 2 {
		hint := " ^N new   Pg↑↓ history   F3 read   Esc cancel "
		if v.backlog != nil && v.backlog.browsing {
			hint = fmt.Sprintf(" ↓ %d lines   Ctrl+End latest ", v.backlog.limit-v.backlog.top)
		}
		hw := min(w, ansi.StringWidth(hint))
		rows = append(rows, richBand("  "+petalLabel(v.Cwd), w-hw, petalMuted, petalInk, false)+richBand(hint, hw, petalMuted, petalInk, false))
	}
	return strings.Join(rows, "\n")
}
