package tui

import (
	"fmt"
	"image"
	"slices"
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

func (m *terminal) graphName(id string) string {
	if id == m.h.record.ID {
		if m.h.record.Title != "" {
			return petalLabel(m.h.record.Title)
		}
		return "Current session"
	}
	for i, c := range m.agents.children {
		if c.id == id {
			if c.title != "" {
				return petalLabel(c.title)
			}
			return fmt.Sprintf("Agent %d", i+1)
		}
	}
	return "Related session"
}
func (m *terminal) graphLinks(id string) []graphLink {
	var links []graphLink
	seen := map[graphLink]bool{}
	add := func(edge graphLink) {
		if edge.id != "" && edge.id != id && !seen[edge] {
			seen[edge] = true
			links = append(links, edge)
		}
	}
	for _, c := range m.agents.children {
		if c.id == id {
			add(graphLink{c.parent, "Parent", true, false})
		}
		if c.parent == id {
			add(graphLink{c.id, "Child", false, false})
		}
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
func (p *picker) hasGraph() bool {
	m := p.center.owner
	return m != nil && m.connection == connected && len(m.agents.children) > 0
}
func (p *picker) openGraph(id string) {
	if !p.hasGraph() {
		return
	}
	m := p.center.owner
	if !slices.ContainsFunc(m.agents.children, func(c agentDetail) bool { return c.id == id }) {
		id = m.h.record.ID
	}
	p.center.graphID = id
	p.center.edge = 0
}
func (p *picker) graphKey(k tea.KeyPressMsg) tea.Cmd {
	m := p.center.owner
	links := m.graphLinks(p.center.graphID)
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
	for _, child := range p.center.owner.agents.children {
		parents[child.id] = child.parent
	}
	focus := p.center.graphID
	if _, ok := parents[focus]; !ok {
		return
	}
	ancestors := map[string]bool{focus: true}
	for id := parents[focus]; id != "" && !ancestors[id]; id = parents[id] {
		ancestors[id] = true
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
	m := p.center.owner
	focus := m.graphName(p.center.graphID)
	links := m.graphLinks(p.center.graphID)
	rows := petalMasthead(w, mastheadHeight(w, h), 0, "Agent graph", fmt.Sprintf("%d agents", len(m.agents.children)), false, p.background)
	if h >= 6 {
		rows = append(rows, richBand("", w, petalText, p.background, false))
	}
	col := min(w/3, 34)
	if h >= 4 {
		rows = append(rows, richBand("  Focus", col, petalRose, p.background, true)+richBand("   Connections", w-col, petalRose, p.background, true))
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
			row += richBand(joint+line+m.graphName(e.id)+"  · "+e.relation, w-col, petalText, bg, i == idx)
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
		hint := "Esc Back  r Reveal in Threads  ↑↓ Select link  Enter Follow  ← Parent"
		rows = appendNavFooter(rows, w, h, styledHint(hint, p.background), p.background)
	}
	v := tea.NewView(strings.Join(rows, "\n"))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}
