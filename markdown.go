package main

import (
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

// Rendering happens on the Bubble Tea event loop. Keep completed Markdown
// layouts across animation frames and transcript navigation.
type markdownKey struct {
	text  string
	width int
}
type markdownCache struct {
	lines    map[markdownKey][]string
	renderer *glamour.TermRenderer
	width    int
}

func (v screen) messageLines(e entry, width int) []string {
	width = max(1, width)
	c := v.markdown
	if c == nil {
		c = &markdownCache{}
	}
	accent := "#ffafd5"
	key := markdownKey{e.Text, width}
	if result, ok := c.lines[key]; ok {
		return result
	}
	if c.renderer == nil || c.width != width {
		style := styles.DarkStyleConfig
		zero := uint(0)
		style.H2.Prefix = ""
		style.H3.Prefix = ""
		style.H4.Prefix = ""
		style.H5.Prefix = ""
		style.H6.Prefix = ""
		style.Heading.Color = &accent
		style.Code.Color = &accent
		style.Code.BackgroundColor = nil
		style.LinkText.Color = &accent
		style.H1.BackgroundColor = nil
		style.H1.Color = &accent
		style.Document.Margin = &zero
		style.Document.Color = nil
		style.Document.BlockPrefix = ""
		style.Document.BlockSuffix = ""
		c.renderer, _ = glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(max(24, width)), glamour.WithTableWrap(true), glamour.WithEmoji())
		c.width = width
	}
	source := clean(e.Text)
	rendered := source
	if c.renderer != nil {
		if formatted, err := c.renderer.Render(source); err == nil {
			rendered = formatted
		}
	}
	lines := strings.Split(strings.Trim(rendered, "\n"), "\n")
	for len(lines) > 1 && strings.TrimSpace(ansi.Strip(lines[0])) == "" {
		lines = lines[1:]
	}
	for len(lines) > 1 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	var wrapped []string
	for _, line := range lines {
		parts := strings.Split(ansi.Hardwrap(line, width, true), "\n")
		// Glamour pads to its wrap width, which can exceed the transcript width.
		// Preserve original blank lines and discard rows created from padding.
		for len(parts) > 1 && strings.TrimSpace(ansi.Strip(parts[len(parts)-1])) == "" {
			parts = parts[:len(parts)-1]
		}
		wrapped = append(wrapped, parts...)
	}
	if c.lines == nil || len(c.lines) >= 192 {
		c.lines = make(map[markdownKey][]string)
	}
	c.lines[key] = wrapped
	return wrapped
}

// richBand receives trusted renderer output. Reapply the surrounding panel's
// colors after an inline span resets its own style.
func richBand(s string, width int, fg, bg string, bold bool) string {
	if width <= 0 {
		return ""
	}
	weight := "22"
	if bold {
		weight = "1"
	}
	base := "\x1b[" + weight + ";38;2;" + fg + ";48;2;" + bg + "m"
	s = ansi.Truncate(s, width, "")
	s = strings.NewReplacer("\x1b[0m", "\x1b[0m"+base, "\x1b[m", "\x1b[m"+base, "\x1b[39m", "\x1b[38;2;"+fg+"m", "\x1b[49m", "\x1b[48;2;"+bg+"m").Replace(s)
	return base + s + base + strings.Repeat(" ", max(0, width-ansi.StringWidth(s))) + "\x1b[0m"
}
