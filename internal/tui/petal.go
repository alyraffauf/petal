package tui

import (
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"
)

const (
	terminalBackground = ""
	roseHex            = "#ffaed3"
	petalInk           = "19;17;23"
	petalText          = "244;238;244"
	petalRose          = "255;174;211"
	petalAmber         = "255;203;104"
	petalMuted         = "177;163;181"
	petalSurface       = "49;38;51"
)

// The ribbon and interactive accents use the same perceptual gradient.
var ribbonPalette = func() [256]string {
	var colors []colorful.Color
	for _, hex := range []string{"#131117", "#573e72", roseHex, "#fff0ef"} {
		c, err := colorful.Hex(hex)
		if err != nil {
			panic(err)
		}
		colors = append(colors, c)
	}
	var palette [256]string
	for i := range palette {
		t := float64(i) * float64(len(colors)-1) / float64(len(palette)-1)
		start := min(int(t), len(colors)-2)
		r, g, b := colors[start].BlendLab(colors[start+1], t-float64(start)).Clamped().RGB255()
		palette[i] = fmt.Sprintf("%d;%d;%d", r, g, b)
	}
	return palette
}()
var selectionBand = ribbonPalette[40]

func canvasBackground(transparent bool) string {
	if transparent {
		return terminalBackground
	}
	return petalInk
}

func mastheadHeight(w, h int) int {
	if h >= 32 && w >= 60 {
		return 2
	}
	return 1
}

func petalMasthead(w, h, frame int, title, status string, working bool, background string) []string {
	label := "  ✿ Petal   " + title
	lw := w * 31 / 100
	rw := w - lw - w*53/100
	if w < 80 {
		lw = w * 40 / 100
		rw = w / 4
		if ansi.StringWidth(label) >= lw {
			label = "  ✿ " + title
		}
	}
	left := ansi.Truncate(label, max(0, lw-1), "…")
	right := ansi.Truncate(status, max(0, rw-2), "…")
	right = strings.Repeat(" ", max(0, rw-ansi.StringWidth(right)-1)) + right
	rows := make([]string, h)
	phase := float64(frame) * .07
	statusColor := petalRose
	if status == "Needs input" {
		statusColor = petalAmber
	} else if working {
		statusColor = ribbonPalette[195+int(26*math.Sin(phase*2))]
	}
	sample := func(x, y float64) string {
		center := .51 + .46*math.Sin(8*x+phase) + .18*math.Sin(17*x-phase*.55)
		edge := y - center
		envelope := math.Exp(-13 * (x - .55) * (x - .55))
		pearl := envelope * math.Exp(-edge*edge*11)
		blush := envelope * math.Exp(-math.Pow(edge+.48, 2)*3)
		violet := envelope * math.Exp(-math.Pow(edge-.45, 2)*4)
		return ribbonPalette[min(255, int(190*pearl+53*blush+40*violet))]
	}
	for y := range h {
		var band strings.Builder
		for x := lw; x < w-rw; x++ {
			t := float64(x) / float64(max(1, w-1))
			if h == 1 {
				// The two shades share a terminal cell and its column boundaries.
				fmt.Fprintf(&band, "\x1b[38;2;%s;48;2;%sm▄", sample(t, .75), sample(t, .25))
				continue
			}
			color := sample(t, (float64(y)+0.5)/float64(h))
			fmt.Fprintf(&band, "\x1b[48;2;%sm ", color)
		}
		if y > 0 {
			left, right = "", ""
		}
		rows[y] = richBand(left, lw, petalText, background, y == 0) + band.String() + "\x1b[0m" + richBand(right, rw, statusColor, background, false)
	}
	return rows
}

func petalLabel(s string) string {
	return strings.ReplaceAll(clean(s), "\n", " ")
}

func petal(v screen) string {
	return renderChat(v, &chatHitTargets{})
}

func renderChat(v screen, hits *chatHitTargets) string {
	*hits = chatHitTargets{}
	w, h := v.Width, v.Height
	if w < 1 || h < 1 {
		return ""
	}
	footer := 1
	if h >= 8 {
		footer = 2
	}
	header := min(mastheadHeight(w, h), h-footer)
	route := strings.Trim(petalLabel(v.Model)+" / "+petalLabel(v.Account), " /")
	status := petalLabel(v.Status)
	switch v.connection {
	case connecting:
		status = "Connecting"
	case connectionFailed:
		status = "Connection failed"
	}
	if v.usage != nil && v.usage.Size > 0 && w >= 78 {
		status += fmt.Sprintf(" · %.0f%% ctx", float64(v.usage.Used)*100/float64(v.usage.Size))
	}
	primary, extra, _ := strings.Cut(status, " · ")
	state := primary
	if v.question != nil {
		state = "Needs input"
	}
	rows := petalMasthead(w, header, v.Frame, "Session", state, v.Busy, v.background)
	if h >= 8 {
		context := route
		if extra != "" {
			context += " · " + extra
		}
		if state != primary || ansi.StringWidth(state)+2 > w*16/100 {
			context = strings.Trim(context+" · "+primary, " ·")
		}
		rows = append(rows, richBand("  "+context, w, petalMuted, v.background, false))
		if h >= 12 {
			rows = append(rows, richBand("", w, petalText, v.background, false))
		}
		header = len(rows)
	}
	if v.notice != nil && h-header-footer >= 3 {
		message := string(v.notice.Severity) + " · " + v.notice.Title
		if v.notice.Description != nil {
			message += " · " + *v.notice.Description
		}
		for _, line := range strings.Split(ansi.Hardwrap(clean(message), max(1, w-2), true), "\n") {
			if len(rows)-header >= min(4, h-header-footer-1) {
				break
			}
			rows = append(rows, richBand(" "+line, w, petalRose, petalSurface, true))
		}
		header = len(rows)
	}
	gutter := min(3, w/4)
	col := max(1, w-gutter-2)
	menuHeight := min(5, len(v.commands), max(0, h-header-footer-1))
	available := h - header - footer - menuHeight
	gap := min(2, max(0, available-1), h/8)
	body := transcript(v, col, available-gap)
	previous := ""
	for _, line := range body {
		mark, fg := "", petalText
		switch line.Role {
		case "user":
			mark = "❯"
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
		rows = append(rows, richBand(" "+mark, gutter, petalRose, v.background, true)+richBand(line.Text, w-gutter, fg, v.background, false))
	}
	for len(rows) < h-footer-menuHeight {
		rows = append(rows, richBand("", w, petalText, v.background, false))
	}
	menuStart := max(0, v.commandIndex-menuHeight+1)
	if menuHeight > 0 {
		hits.commands = image.Rect(1, len(rows), w, len(rows)+menuHeight)
		hits.firstCommand = menuStart
		hits.visibleCommands = v.commands[menuStart : menuStart+menuHeight]
	}
	for i := menuStart; i < menuStart+menuHeight; i++ {
		c := v.commands[i]
		bg, mark := v.background, "   "
		if i == v.commandIndex {
			bg, mark = selectionBand, " › "
		}
		rows = append(rows, richBand("", 1, petalText, v.background, false)+richBand(mark+c.name+"  "+c.description, w-1, petalText, bg, false))
	}
	model := v.Model
	if v.activeModel != "" {
		model = v.activeModel
	}
	input := "Ask " + petalLabel(model) + "…"
	action := chatSend
	switch {
	case v.question != nil:
		input = fmt.Sprintf("Type an option number (1–%d)", len(v.question.options))
		if v.question.form != nil {
			input = "Enter a JSON object"
		}
		action = chatConfirm
	case v.connection == connectionFailed:
		input, action = "Enter retry   Tab subscription", ""
	case v.busyAction != "" && len(v.commands) == 0:
		action = v.busyAction
	case v.connection == connecting || v.Busy:
		action = ""
	case len(v.commands) > 0:
		action = chatRun
	}
	aw := 0
	actionLabel := ""
	if w >= 38 && action != "" {
		actionLabel = " ↵ " + string(action) + " "
		aw = ansi.StringWidth(actionLabel)
	}
	hits.action = image.Rect(w-aw, len(rows), w, len(rows)+1)
	hits.actionKind = action
	if action == chatRun {
		hits.actionCommand = v.commands[v.commandIndex]
	}
	hits.question = v.question
	room := max(1, w-aw-4)
	if v.editor != nil && v.connection != connectionFailed {
		if v.editor.Width() != room {
			v.editor.SetWidth(room)
			*v.editor, _ = v.editor.Update(nil) // Reflow content before scrolling to the cursor.
		}
		hits.actionInput = v.editor.Value()
		if hits.actionInput != "" {
			input = v.editor.View()
		}
	}
	rows = append(rows, richBand("  › "+input, w-aw, petalText, petalSurface, false)+richBand(actionLabel, aw, petalInk, petalRose, true))
	if footer == 2 {
		hint := " " + chatHelp(v) + " "
		if v.queued > 0 {
			hint = fmt.Sprintf(" %d pending ", v.queued) + hint
		}
		if v.backlog != nil && v.backlog.browsing {
			hint = fmt.Sprintf(" ↓ %d lines ", v.backlog.limit-v.backlog.top) + hint
		}
		hw := min(w, ansi.StringWidth(hint))
		rows = append(rows, richBand("  "+petalLabel(v.Cwd), w-hw, petalMuted, v.background, false)+richBand(hint, hw, petalText, v.background, false))
	}
	return strings.Join(rows, "\n")
}
