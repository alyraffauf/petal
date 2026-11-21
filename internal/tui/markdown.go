package tui

import (
	"strings"

	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
)

// Rendering happens on the Bubble Tea event loop. Keep completed Markdown
// layouts across animation frames and transcript navigation.
type messageLayout struct {
	lines []string
	log   *plainLog
	text  string
	width int
	role  string
}
type markdownCache struct {
	layouts      map[int]messageLayout
	messageCount int
	renderer     goldmark.Markdown
	width        int
}

func (v screen) messageLines(index int, e entry, width int) []string {
	layout := v.layoutMessage(index, e, width)
	return v.layoutRows(layout, 0, layout.rowCount())
}

func (v screen) layoutMessage(index int, e entry, width int) messageLayout {
	width = max(1, width)
	c := v.markdown
	if c == nil {
		c = &markdownCache{}
	}
	layout := c.layouts[index]
	if layout.width == width && layout.text == e.Text && layout.role == e.Role {
		return layout
	}
	layout = messageLayout{text: e.Text, role: e.Role, width: width}
	preview, hasImage := v.images[e.Text]
	if e.Role == "tool" && !hasImage {
		layout.log = c.indexPlainLog(e.Text, width)
	}
	if layout.log == nil {
		source := e.Text
		if hasImage {
			source = preview.caption
		}
		layout.lines = c.renderLines(source, width)
		if hasImage && width >= preview.cols {
			layout.lines = append(layout.lines, preview.lines...)
		}
	}
	if c.layouts == nil {
		c.layouts = make(map[int]messageLayout)
	}
	c.layouts[index] = layout
	return layout
}

func (c *markdownCache) renderLines(source string, width int) []string {
	if c.renderer == nil || c.width != width {
		accent := roseHex
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
		c.renderer = newMarkdownRenderer(style, max(24, width))
		c.width = width
	}
	rendered := renderMarkdown(c.renderer, clean(source))
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
	background := "49"
	if bg != terminalBackground {
		background = "48;2;" + bg
	}
	base := "\x1b[" + weight + ";38;2;" + fg + ";" + background + "m"
	cells := ansi.StringWidth(s)
	if cells > width {
		s = ansi.Truncate(s, width, "")
		cells = ansi.StringWidth(s)
	}
	padding := strings.Repeat(" ", width-cells)
	if !strings.Contains(s, "\x1b") {
		return base + s + padding + "\x1b[0m"
	}
	s = strings.NewReplacer("\x1b[0m", "\x1b[0m"+base, "\x1b[m", "\x1b[m"+base, "\x1b[39m", "\x1b[38;2;"+fg+"m", "\x1b[49m", "\x1b["+background+"m").Replace(s)
	return base + s + base + padding + "\x1b[0m"
}
