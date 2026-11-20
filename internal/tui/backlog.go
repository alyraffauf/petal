package tui

// top and limit count rows from first, the earliest message needed by the
// viewport. Following the tail never requires formatting the unseen prefix.
type backlogViewport struct {
	first, width int
	revision     uint64
	count, rows  int
	top, limit   int
	browsing     bool
}

func (b *backlogViewport) move(rows int) {
	b.top = min(b.limit, b.top+rows)
	if b.first == 0 {
		b.top = max(0, b.top)
	}
	b.browsing = b.top < b.limit
}

func transcript(v screen, width, height int) []entry {
	if width <= 0 || height <= 0 {
		return nil
	}
	if v.markdown == nil {
		v.markdown = &markdownCache{}
	}
	n := len(v.Messages) + len(v.planRows)
	messageAt := func(i int) entry {
		if i < len(v.Messages) {
			e := v.Messages[i]
			if e.Role == compactionRole {
				return entry{"system", v.compactions[e.Text].display}
			}
			return e
		}
		return v.planRows[i-len(v.Messages)]
	}
	if n < v.markdown.messageCount {
		for i := range v.markdown.layouts {
			if i >= n {
				delete(v.markdown.layouts, i)
			}
		}
	}
	v.markdown.messageCount = n
	b := v.backlog
	if b == nil {
		b = &backlogViewport{}
	}
	// Each message except the last owns the blank row after it.
	rowsAt := func(i, width int) int {
		count := len(v.messageLines(i, messageAt(i), width))
		if i+1 < n {
			count++
		}
		return count
	}
	b.first = min(b.first, n)
	total := 0
	if b.browsing {
		// A resize preserves the same absolute document row while browsing.
		if b.width != width {
			for i := range b.first {
				b.top += rowsAt(i, b.width)
			}
			b.first = 0
		}
		if b.width == width && v.transcriptRevision != 0 && b.revision == v.transcriptRevision && b.count == n {
			total = b.rows
		} else {
			for i := b.first; i < n; i++ {
				total += rowsAt(i, width)
			}
		}
	} else {
		b.first = n
	}
	for b.first > 0 && (total < height || b.browsing && b.top < 0) {
		b.first--
		rows := rowsAt(b.first, width)
		total += rows
		if b.browsing {
			b.top += rows
		}
	}
	b.width = width
	b.limit = max(0, total-height)
	if b.browsing {
		b.top = max(0, min(b.top, b.limit))
	} else {
		b.top = b.limit
	}
	b.browsing = b.top < b.limit

	visible := make([]entry, 0, min(total, height))
	skip := b.top
	for i := b.first; i < n && len(visible) < height; i++ {
		lines := v.messageLines(i, messageAt(i), width)
		if skip >= len(lines) {
			skip -= len(lines)
		} else {
			for _, line := range lines[skip:min(len(lines), skip+height-len(visible))] {
				visible = append(visible, entry{messageAt(i).Role, line})
			}
			skip = 0
		}
		if i+1 < n && len(visible) < height {
			if skip > 0 {
				skip--
			} else {
				visible = append(visible, entry{})
			}
		}
	}
	// Drop fully skipped messages from the measured suffix. Scrolling back
	// towards the tail should start the next frame close to its first row.
	for b.browsing && b.top > 0 && b.first < n {
		rows := rowsAt(b.first, width)
		if rows > b.top {
			break
		}
		b.first++
		b.top -= rows
		b.limit -= rows
		total -= rows
	}
	b.revision, b.count, b.rows = v.transcriptRevision, n, total
	return visible
}
