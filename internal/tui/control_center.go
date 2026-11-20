package tui

import (
	"fmt"
	"image"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var centerFilters = [...]string{"all", "needs input", "working", "ready", "saved"}

func nodeStatus(n controlNode) string {
	switch strings.ToLower(n.state) {
	case "permission required", "requires action", "needs input":
		return "needs input"
	case "working", "running", "cancelling", "forking":
		return "working"
	case "idle", "ready":
		return "ready"
	case "saved":
		return "saved"
	}
	return ""
}

type controlCenter struct {
	finding      bool
	filter       int
	graphID      string
	edge         int
	project      string
	owner        *terminal
	selected     string
	folded       map[string]bool
	inspect      bool
	detailScroll backlogViewport
	markdown     markdownCache
}

type controlNode struct {
	id, parent, title, kind, state, route string
	index, depth                          int
	branch                                bool
}

func (p *picker) nodes() []controlNode {
	var all []controlNode
	newSession := controlNode{id: "new", title: "new session", kind: "New"}
	if len(p.models) > 0 && p.center.owner == nil {
		all = append(all, newSession)
	}
	m := p.center.owner
	current := ""
	if m != nil && m.connection == connected {
		current = m.h.record.ID
	}
	if m == nil && len(p.models) > 0 {
		quick := []string{p.cfg.DefaultModel()}
		for _, model := range p.models {
			if strings.HasSuffix(model, "/default") && model != quick[0] {
				quick = append(quick, model)
			}
		}
		for _, model := range quick {
			all = append(all, controlNode{id: "model/" + model, parent: "new", title: petalLabel(p.cfg.ModelLabel(model)), kind: "Model", state: "start"})
		}
	}
	for i, r := range p.saved {
		kind, state := "Session", "saved"
		if r.Parent != "" {
			kind = "Fork"
		}
		if r.ID == current {
			state = m.Status
			if m.ask != nil {
				state = "needs input"
				if m.ask.sessionID != "" && m.ask.sessionID != current && m.Busy {
					state = "working"
				}
			}
		}
		title := r.Title
		if title == "" {
			title = "untitled conversation"
		}
		all = append(all, controlNode{id: r.ID, parent: r.Parent, title: petalLabel(title), kind: kind, state: petalLabel(state), route: petalLabel(r.Model + " / " + r.Account), index: i})
	}
	if current != "" {
		for i, c := range m.agents.children {
			title := c.title
			if title == "" {
				title = fmt.Sprintf("agent %d", i+1)
			}
			state := petalLabel(c.state)
			if m.ask != nil && m.ask.sessionID == c.id {
				state = "needs input"
			}
			all = append(all, controlNode{id: c.id, parent: c.parent, title: petalLabel(title), kind: "Subagent", state: state, index: i})
		}
		group := "turns"
		added := false
		for i, msg := range m.Messages {
			if msg.Role != "user" {
				continue
			}
			if !added {
				all = append(all, controlNode{id: group, parent: current, title: "conversation turns", kind: "Turns"})
				added = true
			}
			if p.center.folded[group] && p.search.Value() == "" {
				break
			}
			text := msg.Text
			if preview, ok := m.images[text]; ok {
				text = preview.caption
			}
			line, _, _ := strings.Cut(text, "\n")
			all = append(all, controlNode{id: "turn/" + strconv.Itoa(i), parent: group, title: petalLabel(ansi.Truncate(line, 120, "…")), kind: "Turn", index: i})
		}
	}
	if len(p.models) > 0 && m != nil {
		all = append(all, newSession)
	}
	byID := make(map[string]int, len(all))
	for i, n := range all {
		byID[n.id] = i
	}
	kids := make(map[string][]int)
	for i, n := range all {
		parent := n.parent
		if _, ok := byID[parent]; !ok {
			parent = ""
		}
		kids[parent] = append(kids[parent], i)
	}
	var result []controlNode
	seen := make(map[string]bool)
	query := strings.ToLower(strings.TrimSpace(p.search.Value()))
	var visit func(int, int)
	visit = func(i, depth int) {
		n := all[i]
		if seen[n.id] {
			return
		}
		seen[n.id] = true
		n.depth, n.branch = depth, len(kids[n.id]) > 0 || n.kind == "Turns"
		if (p.center.filter == 0 || nodeStatus(n) == centerFilters[p.center.filter]) && (query == "" || (n.kind == "Model" && strings.Contains(strings.ToLower(strings.TrimPrefix(n.id, "model/")), query)) || strings.Contains(strings.ToLower(n.title+" "+n.kind+" "+n.state+" "+n.route), query)) {
			result = append(result, n)
		}
		if p.center.filter == 0 && query == "" && p.center.folded[n.id] {
			return
		}
		for _, child := range kids[n.id] {
			visit(child, depth+1)
		}
	}
	for _, i := range kids[""] {
		visit(i, 0)
	}
	return result
}
func (p *picker) selectedNode(rows []controlNode) int {
	i := slices.IndexFunc(rows, func(n controlNode) bool { return n.id == p.center.selected })
	return max(0, i)
}
func (p *picker) selectNode(n controlNode) {
	p.center.selected = n.id
	p.center.detailScroll = backlogViewport{browsing: true}
	p.center.markdown = markdownCache{}
}

func (p *picker) centerKey(k tea.KeyPressMsg) tea.Cmd {
	if k.String() == "ctrl+f" {
		p.openTranscriptSearch()
		return nil
	}
	if p.center.graphID != "" {
		return p.graphKey(k)
	}
	rows := p.nodes()
	i := p.selectedNode(rows)
	var n controlNode
	if len(rows) > 0 {
		n = rows[i]
	}
	m := p.center.owner
	if !p.center.inspect && (p.center.finding || p.search.Value() != "") {
		switch k.String() {
		case "left", "right", "home", "end":
			var cmd tea.Cmd
			p.search, cmd = p.search.Update(k)
			return cmd
		}
	}
	switch k.String() {
	case "/":
		if !p.center.finding && p.search.Value() == "" {
			p.center.finding = true
			return nil
		}
		p.search, _ = p.search.Update(k)
		return nil
	case "shift+tab":
		p.center.filter = (p.center.filter + 1) % len(centerFilters)
		p.center.inspect = false
		p.center.detailScroll = backlogViewport{browsing: true}
		return nil
	case "ctrl+g":
		p.openGraph(n.id)
		return nil
	case "ctrl+c":
		return tea.Quit
	case "esc":
		if p.center.inspect {
			p.center.inspect = false
			return nil
		}
		if p.center.finding || p.search.Value() != "" {
			p.center.finding = false
			p.search.Reset()
			return nil
		}
		if p.embedded && m != nil {
			m.launch = nil
			return nil
		}
		return tea.Quit
	case "f2", "f4":
		if m != nil {
			m.launch = nil
		} else {
			p.history = false
			p.search.Reset()
			p.filter()
		}
		return nil
	case "up", "down", "pgup", "pgdown":
		delta := 1
		if strings.HasPrefix(k.String(), "pg") {
			delta = max(1, p.height/2)
		}
		if k.String() == "up" || k.String() == "pgup" {
			delta = -delta
		}
		if p.center.inspect {
			p.center.detailScroll.move(delta)
		} else if len(rows) > 0 {
			p.selectNode(rows[max(0, min(len(rows)-1, i+delta))])
		}
	case "left":
		if p.center.inspect {
			p.center.inspect = false
		} else if n.branch && !p.center.folded[n.id] {
			p.center.folded[n.id] = true
		} else if n.parent != "" {
			if j := slices.IndexFunc(rows, func(node controlNode) bool { return node.id == n.parent }); j >= 0 {
				p.selectNode(rows[j])
			}
		}
	case "right":
		if n.branch {
			if p.center.folded[n.id] {
				delete(p.center.folded, n.id)
			} else if i+1 < len(rows) && rows[i+1].parent == n.id {
				p.selectNode(rows[i+1])
			}
		} else if n.id != "" {
			p.center.inspect = true
		}
	case "tab":
		if n.id != "" {
			p.center.inspect = !p.center.inspect
		}
	case "enter":
		if n.id == "" {
			return nil
		}
		switch n.kind {
		case "Model":
			p.cfg.Model = strings.TrimPrefix(n.id, "model/")
			p.id = ""
			p.resume = false
			p.chosen = true
			return tea.Quit
		case "New":
			if m != nil && (m.ask != nil || m.Busy || m.forking || m.changingSetting) {
				return nil
			}
			p.history = false
			p.fromCenter = true
			p.resume = false
			p.search.Reset()
			p.filter()
		case "Turns":
			p.center.folded[n.id] = !p.center.folded[n.id]
		case "Subagent":
			if m.ask != nil && m.ask.sessionID == n.id {
				m.launch = nil
				return nil
			}
			p.center.inspect = !p.center.inspect
		case "Turn":
			m.chatBacklog = backlogViewport{first: n.index, width: max(1, m.Width-roleGutter(m.Width)-2), browsing: true}
			m.launch = nil
		default:
			if m != nil && m.connection == connected && n.id == m.h.record.ID {
				m.launch = nil
				return nil
			}
			if m != nil && (m.ask != nil || m.Busy || m.changingSetting || m.forking) {
				return nil
			}
			r := p.saved[n.index]
			p.id, p.cfg.Model, p.resume, p.chosen = r.ID, r.Model, true, true
			return tea.Quit
		}
	default:
		if !p.center.inspect {
			var cmd tea.Cmd
			p.search, cmd = p.search.Update(k)
			p.center.detailScroll = backlogViewport{browsing: true}
			return cmd
		}
	}
	return nil
}

func (p *picker) centerDetails(n controlNode) []entry {
	m := p.center.owner
	switch n.kind {
	case "Model":
		return []entry{{"system", "Enter · Start session"}}
	case "New":
		if m != nil && m.Busy {
			return []entry{{"system", "Finish the active turn to start another session."}}
		}
		return []entry{{"system", "Enter · Choose a model"}, {"system", "Ctrl+N · Start with `" + p.cfg.ModelLabel(p.cfg.DefaultModel()) + "`"}}
	case "Subagent":
		rows := []entry{{"system", "Subagent · " + m.Model + " / " + m.Account}}
		c := m.agents.children[n.index]
		if c.description != "" {
			rows = append(rows, entry{"system", c.description})
		}
		if m.ask != nil && m.ask.sessionID == n.id {
			rows = append(rows, entry{"system", "**Permission requested**\n\n" + m.ask.title}, entry{"system", "**Enter** · Review in parent"})
		}
		rows = append(rows, entry{"system", "**Parent** · " + m.graphName(c.parent)})
		for _, r := range m.agents.details(n.index) {
			rows = append(rows, entry{r.Role, "**" + sentenceLabel(r.Role) + "** · " + r.Text})
		}
		return rows
	case "Turns":
		return []entry{{"system", "Select a prompt to jump to that turn."}}
	case "Turn":
		msg := m.Messages[n.index]
		if preview, ok := m.images[msg.Text]; ok {
			msg.Text = preview.caption
		}
		return []entry{msg}
	default:
		r := p.saved[n.index]
		rows := []entry{{"system", n.kind + " · " + n.route}}
		if stamp := r.LastActivity(); !stamp.IsZero() {
			rows = append(rows, entry{"system", stamp.Local().Format("Jan 2 · 15:04")})
		}
		if r.Parent != "" {
			for _, parent := range p.saved {
				if parent.ID == r.Parent {
					rows = append(rows, entry{"system", "Fork of " + parent.Title})
					break
				}
			}
		}
		if m != nil && m.Busy && n.id != m.h.record.ID {
			rows = append(rows, entry{"system", "Available to resume after the active turn."})
		}
		return rows
	}
}
func (p *picker) centerView() tea.View {
	if p.center.graphID != "" {
		return p.graphView()
	}
	w, h := max(1, p.width), max(1, p.height)
	project := p.center.project
	rows := []string{petalMasthead(w, 0, "control center", petalLabel(project), false, p.background)}
	filtering := !p.center.inspect && (p.center.finding || p.search.Value() != "")
	nodes := p.nodes()
	index := p.selectedNode(nodes)
	left := w
	if p.center.inspect {
		left = 0
	} else if w >= 72 && (!filtering || len(nodes) > 0) {
		left = w * 60 / 100
	}
	if h >= 6 || filtering && h >= 3 {
		title := "threads"
		if p.center.filter != 0 {
			title += " · " + centerFilters[p.center.filter]
		}
		name := "details"
		if len(nodes) > 0 {
			name = nodes[index].title
		}
		if h >= 12 {
			rows = append(rows, richBand("", w, petalText, p.background, false))
		}
		if left == 0 {
			title = name
		}
		line := richBand("  "+title, left, petalRose, p.background, true)
		if filtering {
			prefix := "  threads  / "
			if left < 24 {
				prefix = "  / "
			}
			labelWidth := ansi.StringWidth(prefix)
			p.search.SetWidth(max(1, left-labelWidth-1))
			input := p.search
			input.Prompt = ""
			input.Placeholder = "filter threads"
			line = richBand(prefix, ansi.StringWidth(prefix), petalRose, p.background, true) + richBand(input.View(), max(0, left-ansi.StringWidth(prefix)), petalText, p.background, false)
			line = ansi.Truncate(line, left, "")
		}
		if left < w {
			line += richBand(" │ "+name, w-left, petalRose, p.background, true)
		}
		rows = append(rows, line)
	}
	footerHeight := navFooterHeight(h)
	height := max(0, h-len(rows)-footerHeight)
	var detail []entry
	if left < w && len(nodes) > 0 && height > 0 {
		n := nodes[index]
		width := max(1, w-left-3)
		m := p.center.owner
		if !p.center.inspect && n.kind == "Subagent" && m.ask != nil && m.ask.sessionID == n.id {
			detail = p.permissionPreview(n, width)
			scroll := &p.center.detailScroll
			scroll.first = 0
			scroll.limit = max(0, len(detail)-height)
			scroll.top = max(0, min(scroll.top, scroll.limit))
			detail = detail[scroll.top:]
		} else {
			v := screen{Messages: p.centerDetails(n), markdown: &p.center.markdown, backlog: &p.center.detailScroll}
			detail = transcript(v, width, height)
		}
	}
	headerGap := 0
	if h >= 12 {
		headerGap = 1
	}
	first := max(0, index-max(1, height-headerGap)+1)
	p.pointerPane = image.Rect(0, len(rows), left, len(rows)+height)
	p.pointerRows = image.Rect(0, len(rows)+headerGap, left, len(rows)+headerGap+min(len(nodes)-first, max(0, height-headerGap)))
	p.pointerFirst = first
	p.pointerDetail = image.Rect(left, len(rows), w, len(rows)+height)
	const orchidWidth = 24
	detailWidth := max(0, w-left-3)
	treeRows := headerGap + max(1, len(nodes)-first)
	spaceBelow := height - treeRows
	showOrchid := !p.monochrome && left < w && left >= orchidWidth+4 && spaceBelow >= len(orchidRows)+2 && !filtering
	orchidTop := treeRows + (spaceBelow-len(orchidRows))/2
	for y := range height {
		row := richBand("", left, petalText, p.background, false)
		i := first + y - headerGap
		if y >= headerGap && i < len(nodes) {
			if left > 0 {
				row = p.nodeLine(nodes[i], left, i == index)
			}
		} else if left > 0 && y == headerGap && len(nodes) == 0 {
			line := "  no matching threads"
			if len(p.models) == 0 {
				line = " Install a Claude or Codex ACP adapter to start."
			}
			row = richBand(line, left, petalMuted, p.background, false)
		} else if showOrchid && y >= orchidTop && y < orchidTop+len(orchidRows) {
			art := strings.Repeat(" ", (left-orchidWidth)/2) + orchidRows[y-orchidTop]
			row = richBand(art, left, petalText, p.background, false)
		}
		if left < w {
			text := ""
			if y < len(detail) {
				text = detail[y].Text
			}
			row += richBand(" │ ", min(3, w-left), petalMuted, p.background, false) + richBand(text, detailWidth, petalText, p.background, false)
		}
		rows = append(rows, row)
	}
	hint := "↑↓ Select  ←→ Fold  ^F Search  / Find  Enter Open  ^N New  ⇧Tab Filter  Esc Back"
	if p.hasGraph() {
		hint = "↑↓ Select  ←→ Fold  ^F Search  / Find  Enter Open  Esc Back  ^G Graph  Tab Details"
	}
	if p.center.inspect {
		hint = " ↑↓ Scroll  Pg↑ Pg↓ Page  Tab Tree  Esc Back"
	}
	escape := "Esc Back"
	if !p.center.inspect {
		if p.center.finding || p.search.Value() != "" {
			hint = "←→ Cursor  ↑↓ Select  Enter Open  Esc Back"
			if len(nodes) == 0 {
				hint = "←→ Cursor  Esc Back"
			}
			escape = "Esc Clear"
		} else if !p.embedded {
			escape = "Esc Quit"
		}
	}
	hint = strings.Replace(hint, "Esc Back", escape, 1)
	if p.historyError != "" {
		hint = petalLabel(p.historyError) + " · " + escape
	} else {
		hint = styledHint(hint, p.background, w)
	}
	rows = appendNavFooter(rows, w, h, hint, p.background)
	view := tea.NewView(strings.Join(rows, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func (p *picker) nodeLine(n controlNode, width int, selected bool) string {
	// Indent the highlight with its branch, leaving a cell inside the band
	// before the glyph. Keep room for the title on narrow panes.
	indent := min(width/3, 1+min(n.depth, 8)*3)
	mark, iconColor := "▶", petalRose
	switch {
	case n.kind == "New":
		mark = "+"
	case n.parent != "":
		mark, iconColor = "↳", petalText
	case n.branch && !p.center.folded[n.id]:
		mark = "▼"
	}
	name := n.title
	if n.kind == "Fork" {
		name = "Fork · " + name
	}
	state := n.state
	fg := petalMuted
	dot := "○"
	switch nodeStatus(n) {
	case "working":
		fg = petalRose
		dot = "●"
	case "ready":
		if n.kind != "Subagent" {
			fg = petalRose
			dot = "●"
		}
	case "needs input":
		fg = petalAmber
		dot = "●"
		state = "needs input"
	}
	labelWidth := width - indent
	stateWidth := 0
	if state != "" && width >= 28 {
		stateWidth = min(14, width/3)
		labelWidth -= stateWidth
	}
	iconWidth := min(labelWidth, ansi.StringWidth(" "+mark+" "))
	titleWidth := labelWidth - iconWidth
	bg := p.background
	span := func(text string, cols, offset int, fg string, bold bool) string {
		if !selected {
			return richBand(text, cols, fg, bg, bold)
		}
		return selectedTreeSpan(text, cols, offset, width-indent, fg, bold)
	}
	row := richBand("", indent, petalText, bg, false)
	row += span(" "+mark+" ", iconWidth, 0, iconColor, false)
	titleLimit := titleWidth
	if stateWidth > 0 {
		titleLimit = max(0, titleWidth-1)
	}
	row += span(ansi.Truncate(name, titleLimit, "…"), titleWidth, iconWidth, petalText, false)
	return row + span(dot+" "+state, stateWidth, labelWidth, fg, false)
}

// The split inspector leads with the decision; Tab opens the full message history.
func (p *picker) permissionPreview(n controlNode, width int) []entry {
	m := p.center.owner
	c := m.agents.children[n.index]
	var rows []entry
	field := func(label, value string) {
		line := richBand(label, ansi.StringWidth(label), petalRose, p.background, true) + clean(value)
		for _, part := range strings.Split(ansi.Hardwrap(line, width, true), "\n") {
			rows = append(rows, entry{Text: part})
		}
	}
	field("", "Subagent · "+m.Model+" / "+m.Account)
	if c.description != "" {
		rows = append(rows, entry{})
		field("", c.description)
	}
	rows = append(rows, entry{})
	field("Permission requested", "")
	field("", m.ask.title)
	field("Enter", "  Review in parent")
	rows = append(rows, entry{})
	field("Parent", "  "+m.graphName(c.parent))
	return rows
}

// Color terminal cells by their displayed column, including wide graphemes.
func selectedTreeSpan(text string, width, offset, total int, fg string, bold bool) string {
	if width <= 0 {
		return ""
	}
	text = ansi.Truncate(text, width, "")
	text += strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
	var b strings.Builder
	weight := "22"
	if bold {
		weight = "1"
	}
	fmt.Fprintf(&b, "\x1b[%s;38;2;%sm", weight, fg)
	last := -1
	for len(text) > 0 {
		cluster, cells := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
		step := min(12, offset*13/max(1, total))
		if step != last {
			fmt.Fprintf(&b, "\x1b[48;2;%d;%d;%dm", 42+step, 31+step/2, 47+step/2)
			last = step
		}
		b.WriteString(cluster)
		offset += cells
		text = text[len(cluster):]
	}
	b.WriteString("\x1b[0m")
	return b.String()
}
