package tui

import (
	"fmt"
	"image"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A graph page centers one agent. Ownership and exchanged messages are
// separate links even when they connect the same pair of agents.
type graphLink struct {
	id, relation      string
	incoming, message bool
}

func (p *picker) graphName(id string) string {
	m := p.center.owner
	if m != nil && m.h != nil && id == m.h.record.ID {
		if m.h.record.Title != "" {
			return petalLabel(m.h.record.Title)
		}
		return "current session"
	}
	if m != nil {
		for i, c := range m.agents.children {
			if c.id == id {
				if c.title != "" {
					return petalLabel(c.title)
				}
				return fmt.Sprintf("agent %d", i+1)
			}
		}
	}
	p.graphOwnership()
	if i, ok := p.center.cache.savedIndex[id]; ok {
		if title := p.saved[i].Title; title != "" {
			return petalLabel(title)
		}
		return "untitled conversation"
	}
	return "related session"
}
func (p *picker) graphLinks(id string) []graphLink {
	cache := &p.center.cache
	if cache.linksValid && cache.focus == id {
		return cache.links
	}
	cache.focus, cache.links, cache.linksValid = id, p.buildGraphLinks(id), true
	return cache.links
}

func (p *picker) buildGraphLinks(id string) []graphLink {
	var links []graphLink
	seen := map[graphLink]bool{}
	add := func(edge graphLink) {
		if edge.id != "" && edge.id != id && !seen[edge] {
			seen[edge] = true
			links = append(links, edge)
		}
	}
	for _, edge := range p.graphOwnership() {
		if edge.child == id {
			add(graphLink{edge.parent, "Parent", true, false})
		}
		if edge.parent == id {
			add(graphLink{edge.child, "Child", false, false})
		}
	}
	m := p.center.owner
	if m == nil || m.connection != connected {
		return links
	}
	for _, mail := range m.agents.mail {
		if mail.display == "" {
			continue
		}
		if string(mail.sender) == id {
			add(graphLink{string(mail.recipient), "Sent to", false, true})
		}
		if string(mail.recipient) == id {
			add(graphLink{string(mail.sender), "Received from", true, true})
		}
	}
	return links
}

type graphOwnership struct {
	child, parent string
}

func (p *picker) graphOwnership() []graphOwnership {
	cache := &p.center.cache
	if cache.savedIndex != nil {
		return cache.ownership
	}
	cache.savedIndex = make(map[string]int, len(p.saved))
	for i, r := range p.saved {
		if _, exists := cache.savedIndex[r.ID]; !exists {
			cache.savedIndex[r.ID] = i
		}
	}
	var edges []graphOwnership
	bound := make(map[string]bool)
	for _, record := range p.saved {
		if _, ok := cache.savedIndex[record.DelegatedParent]; ok && record.DelegatedParent != "" {
			edges = append(edges, graphOwnership{record.ID, record.DelegatedParent})
			bound[record.ID] = true
		}
	}
	if m := p.center.owner; m != nil && m.connection == connected {
		for _, c := range m.agents.children {
			if !bound[c.id] {
				edges = append(edges, graphOwnership{c.id, c.parent})
			}
		}
	}
	cache.ownership = edges
	return edges
}

func (p *picker) hasGraph() bool {
	return len(p.graphOwnership()) > 0
}

func (p *picker) openGraph(id string) {
	edges := p.graphOwnership()
	if len(edges) == 0 {
		return
	}
	focus := edges[0].parent
	if m := p.center.owner; m != nil && m.connection == connected {
		focus = m.h.record.ID
	}
	for _, edge := range edges {
		if edge.child == id || edge.parent == id {
			focus = id
			break
		}
	}
	p.center.graphID = focus
	p.center.edge = 0
}
func (p *picker) graphKey(k tea.KeyPressMsg) tea.Cmd {
	links := p.graphLinks(p.center.graphID)
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "esc", "ctrl+g", "f2", "f4":
		p.center.graphID = ""
	case "r":
		p.revealGraphFocus()
	case "down", "up", "pgup", "pgdown":
		step := 1
		if strings.HasPrefix(k.String(), "pg") {
			step = max(1, p.height-5)
		}
		if k.String() == "up" || k.String() == "pgup" {
			step = -step
		}
		p.center.edge = max(0, min(len(links)-1, p.center.edge+step))
	case "enter", "right":
		if p.center.edge < len(links) {
			p.center.graphID = links[p.center.edge].id
			p.center.edge = 0
		}
	case "left":
		for _, e := range links {
			if e.relation == "Parent" {
				p.center.graphID = e.id
				p.center.edge = 0
				break
			}
		}
	}
	return nil
}

func (p *picker) revealGraphFocus() {
	parents := make(map[string]string)
	for _, record := range p.saved {
		parents[record.ID] = record.Parent
	}
	for _, edge := range p.graphOwnership() {
		parents[edge.child] = edge.parent
	}
	focus := p.center.graphID
	if _, ok := parents[focus]; !ok {
		return
	}
	ancestors := map[string]bool{focus: true}
	for id := parents[focus]; id != "" && !ancestors[id]; id = parents[id] {
		ancestors[id] = true
		p.center.cache.rowsValid = false
		delete(p.center.folded, id)
	}
	p.center.graphID = ""
	p.center.inspect = false
	p.center.finding = false
	p.search.Reset()
	p.center.filter = 0
	p.selectNode(controlNode{id: focus})
}

func (p *picker) graphView() tea.View {
	w, h := max(1, p.width), max(1, p.height)
	focus := p.graphName(p.center.graphID)
	links := p.graphLinks(p.center.graphID)
	rows := []string{petalMasthead(w, p.motion.frame, "agent graph", fmt.Sprintf("%d agents", len(p.graphOwnership())), p.workingTide(), p.background)}
	if h >= 6 {
		rows = append(rows, richBand("", w, petalText, p.background, false))
	}
	col := min(w/3, 34)
	if h >= 4 {
		rows = append(rows, richBand("  focus", col, petalRose, p.background, true)+richBand("   connections", w-col, petalRose, p.background, true))
	}
	if h >= 12 {
		rows = append(rows, richBand("", w, petalText, p.background, false))
	}
	space := max(0, h-len(rows)-navFooterHeight(h))
	idx := max(0, min(len(links)-1, p.center.edge))
	start := max(0, idx-space+1)
	visible := min(space, len(links)-start)
	p.pointerPane = image.Rect(col, len(rows), w, len(rows)+space)
	p.pointerDetail = image.Rectangle{}
	p.pointerRows = image.Rect(col, len(rows), w, len(rows)+visible)
	p.pointerFirst = start
	mid := max(0, visible-1) / 2
	for y := range space {
		name := ""
		if y == mid {
			name = ansi.Truncate(" "+focus+" ", max(0, col-1), "…")
			if len(links) > 0 {
				name += strings.Repeat("─", max(0, col-ansi.StringWidth(name)))
			}
		}
		row := richBand(ansi.Truncate(name, col, "…"), col, petalRose, p.background, true)
		i := start + y
		if i < len(links) {
			e := links[i]
			joint := "├"
			switch {
			case visible == 1:
				joint = "─"
			case y == mid && y == 0:
				joint = "┬"
			case y == mid:
				joint = "┼"
			case y == 0:
				joint = "┌"
			case y == visible-1:
				joint = "└"
			}
			line := "──▶ "
			if e.incoming {
				line = "◀── "
			}
			if e.message {
				line = "··▶ "
				if e.incoming {
					line = "◀·· "
				}
			}
			bg := p.background
			if i == idx {
				bg = selectionBand
			}
			row += richBand(joint+line+p.graphName(e.id)+"  · "+e.relation, w-col, petalText, bg, i == idx)
		} else {
			info := ""
			if y == 0 {
				info = "  No reported connections"
			}
			row += richBand(info, w-col, petalMuted, p.background, false)
		}
		rows = append(rows, row)
	}
	if len(rows) < h {
		hint := "Esc Back  r Reveal in threads  ↑↓ Select link  Enter Follow  ← Parent"
		rows = appendNavFooter(rows, w, h, styledHint(hint, p.background, w), p.background)
	}
	v := tea.NewView(strings.Join(rows, "\n"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
