package tui

import "strings"

const logFence = "```text\n"

type plainLog struct {
	header  []string
	offsets []int // Includes the start of the closing fence as a sentinel.
	first   int
	window  []string
}

// Index only lines guaranteed to occupy one rendered row. Other blocks use
// the full Markdown layout, including its wrapping and blank-line trimming.
func (c *markdownCache) indexPlainLog(source string, width int) *plainLog {
	prefix := 0
	if strings.HasPrefix(source, "**") {
		header, _, ok := strings.Cut(source, "\n\n")
		if !ok || strings.Contains(header, "\n") || !strings.Contains(header, "** · ") {
			return nil
		}
		prefix = len(header) + 2
	}
	if !strings.HasPrefix(source[prefix:], logFence) || !strings.HasSuffix(source, "\n```") {
		return nil
	}
	count := strings.Count(source, "\n") - 1
	if count < 1024 {
		return nil
	}
	offsets := make([]int, 0, count+1)
	pos := prefix + len(logFence)
	for pos < len(source)-3 {
		offsets = append(offsets, pos)
		end := pos + strings.IndexByte(source[pos:], '\n')
		line := source[pos:end]
		if len(line) > width-4 || strings.Trim(line, " ") == "" || strings.Contains(line, "```") {
			return nil
		}
		for i := range len(line) {
			if line[i] < 32 || line[i] > 126 {
				return nil
			}
		}
		pos = end + 1
	}
	log := &plainLog{offsets: append(offsets, pos)}
	if prefix > 0 {
		// Include the first two body rows to retain the renderer's exact spacing
		// and ANSI transitions between the heading and the fence.
		rows := c.renderLines(source[:offsets[2]]+"```", width)
		if len(rows) < 2 {
			return nil
		}
		log.header = rows[:len(rows)-2]
	}
	return log
}

func (l messageLayout) rowCount() int {
	if l.log != nil {
		return len(l.log.header) + len(l.log.offsets) - 1
	}
	return len(l.lines)
}

func (v screen) layoutRows(l messageLayout, start, end int) []string {
	log := l.log
	if log == nil {
		return l.lines[start:end]
	}
	var heading []string
	if start < len(log.header) {
		heading = log.header[start:min(end, len(log.header))]
		if end <= len(log.header) {
			return heading
		}
	}
	start = max(0, start-len(log.header))
	end -= len(log.header)
	if start < log.first || end > log.first+len(log.window) {
		first := max(0, start-8)
		last := min(len(log.offsets)-1, end+8)
		// Rendering the preceding line preserves Glamour's style reset at the
		// start of interior lines.
		context := max(0, first-1)
		source := logFence + l.text[log.offsets[context]:log.offsets[last]] + "```"
		cache := v.markdown
		if cache == nil {
			cache = &markdownCache{}
		}
		log.window = cache.renderLines(source, l.width)[first-context:]
		log.first = first
	}
	body := log.window[start-log.first : end-log.first]
	if len(heading) > 0 {
		return append(heading[:len(heading):len(heading)], body...)
	}
	return body
}
