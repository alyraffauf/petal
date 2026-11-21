package tui

import "strings"

const logFence = "```text\n"

type plainLog struct {
	offsets []int // Includes the start of the closing fence as a sentinel.
	first   int
	window  []string
}

// Index only lines guaranteed to occupy one rendered row. Other blocks use
// the full Markdown layout, including its wrapping and blank-line trimming.
func indexPlainLog(source string, width int) *plainLog {
	if !strings.HasPrefix(source, logFence) || !strings.HasSuffix(source, "\n```") {
		return nil
	}
	count := strings.Count(source, "\n") - 1
	if count < 1024 {
		return nil
	}
	offsets := make([]int, 0, count+1)
	pos := len(logFence)
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
	return &plainLog{offsets: append(offsets, pos)}
}

func (l messageLayout) rowCount() int {
	if l.log != nil {
		return len(l.log.offsets) - 1
	}
	return len(l.lines)
}

func (v screen) layoutRows(l messageLayout, start, end int) []string {
	log := l.log
	if log == nil {
		return l.lines[start:end]
	}
	if start < log.first || end > log.first+len(log.window) {
		first := max(0, start-8)
		last := min(l.rowCount(), end+8)
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
	return log.window[start-log.first : end-log.first]
}
