package tui

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type urlRequest struct {
	id, message, url string
	answer           chan string
}
type urlCompleted string
type urlCancelled struct{ request *urlRequest }

func (h *harness) requestURL(ctx context.Context, q *urlRequest) string {
	select {
	case h.events <- q:
	case <-ctx.Done():
		return "cancel"
	}
	select {
	case action := <-q.answer:
		return action
	case <-ctx.Done():
		select {
		case h.events <- urlCancelled{q}:
		case <-h.lifetime.Done():
		}
		return "cancel"
	}
}

func (m *terminal) removeURL(i int, action string) {
	q := m.urls[i]
	if action != "" {
		q.answer <- action
	}
	if i == 0 {
		m.showURL = false
		m.urlOffset = 0
	}
	m.urls = slices.Delete(m.urls, i, i+1)
}

func (m *terminal) urlKey(k tea.KeyPressMsg) bool {
	if len(m.urls) == 0 || m.ask != nil {
		return false
	}
	if k.String() == "f6" {
		m.showURL = !m.showURL
		m.urlOffset = 0
		return true
	}
	if !m.showURL {
		return false
	}
	switch k.String() {
	case "esc":
		m.showURL = false
	case "enter":
		m.removeURL(0, "accept")
	case "d":
		m.removeURL(0, "decline")
	case "pgdown":
		m.urlOffset += max(1, m.Height-3)
	case "pgup":
		m.urlOffset = max(0, m.urlOffset-max(1, m.Height-3))
	case "down":
		m.urlOffset++
	case "up":
		m.urlOffset = max(0, m.urlOffset-1)
	case "ctrl+c":
		return false
	}
	return true
}

func (m *terminal) urlView() tea.View {
	w, h := max(1, m.Width), max(1, m.Height)
	q := m.urls[0]
	rows := []string{petalMasthead(w, 0, "external sign-in", "", false, m.background)}
	wrap := func(text string) []string {
		return strings.Split(ansi.Hardwrap(text, max(1, w-4), true), "\n")
	}
	lines := append(wrap(clean(q.message)), "")
	linkStart := len(lines)
	lines = append(lines, wrap(q.url)...)
	height := max(0, h-2)
	m.urlOffset = min(m.urlOffset, max(0, len(lines)-height))
	for i := range height {
		line := ""
		if i+m.urlOffset < len(lines) {
			line = lines[i+m.urlOffset]
			if i+m.urlOffset >= linkStart {
				line = ansi.SetHyperlink(q.url) + line + ansi.ResetHyperlink()
			}
		}
		rows = append(rows, richBand("  "+line, w, petalText, m.background, false))
	}
	if h > 1 {
		rows = append(rows, richBand("  Enter Continue after visiting   d Decline   Esc Back   ↑↓ Scroll", w, petalRose, m.background, false))
	}
	v := tea.NewView(strings.Join(rows, "\n"))
	v.AltScreen = true
	return v
}

func (h *harness) completeURL(ctx context.Context, raw json.RawMessage) {
	var p struct{ ElicitationID string }
	if json.Unmarshal(raw, &p) == nil && p.ElicitationID != "" {
		select {
		case h.events <- urlCompleted(p.ElicitationID):
		case <-ctx.Done():
		}
	}
}
