package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
)

type balances []quota
type picker struct {
	cfg                           config
	models                        []string
	query                         string
	routes                        []string
	usage                         map[string]string
	index, width, height          int
	choosingRoute, chosen, resume bool
}

func (m *picker) Init() tea.Cmd { return nil }
func (m *picker) options() []string {
	if m.choosingRoute {
		return m.routes
	}
	var matches []string
	for _, s := range m.models {
		if strings.Contains(strings.ToLower(s), strings.ToLower(m.query)) {
			matches = append(matches, s)
		}
	}
	return matches
}
func (m *picker) routeMenu(model string) tea.Cmd {
	m.cfg.model = model
	m.choosingRoute = true
	m.index = 0
	m.usage = nil
	m.routes = []string{""}
	for n, a := range m.cfg.Accounts {
		if len(a.Models[model]) > 0 {
			m.routes = append(m.routes, n)
		}
	}
	slices.Sort(m.routes[1:])
	c := m.cfg
	return func() tea.Msg {
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return routeBalances{model, c.scores(ctx)}
	}
}

type routeBalances struct {
	model  string
	scores balances
}

func (m *picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = t.Width
		m.height = t.Height
	case routeBalances:
		if t.model == m.cfg.model {
			m.usage = map[string]string{}
			for _, s := range t.scores {
				switch {
				case s.err != nil:
					if remainingBackoff(s.name) > 0 {
						m.usage[s.name] = s.err.Error()
					} else {
						m.usage[s.name] = "probe unavailable"
					}
				case s.remaining < 0:
					m.usage[s.name] = "unmetered"
				default:
					m.usage[s.name] = fmt.Sprintf("%.0f%% remaining", s.remaining)
				}
				if s.tier != "" {
					m.usage[s.name] += " · " + s.tier
				}
				if s.capacity > 0 && s.capacity != 1 {
					m.usage[s.name] += fmt.Sprintf(" · %g× capacity", s.capacity)
				}
			}
		}
	case tea.PasteMsg:
		if !m.choosingRoute {
			m.query += strings.Join(strings.Fields(clean(t.Content)), " ")
			m.index = 0
		}
	case tea.KeyPressMsg:
		opts := m.options()
		switch t.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.choosingRoute {
				m.choosingRoute = false
				m.index = max(0, slices.Index(m.options(), m.cfg.model))
			} else {
				return m, tea.Quit
			}
		case "down", "ctrl+n":
			m.index = min(m.index+1, max(0, len(opts)-1))
		case "up", "ctrl+p":
			m.index = max(0, m.index-1)
		case "pgdown":
			m.index = min(m.index+max(1, m.height-9), max(0, len(opts)-1))
		case "pgup":
			m.index = max(0, m.index-max(1, m.height-9))
		case "tab":
			if !m.choosingRoute && m.cfg.forced == "" && len(opts) > 0 {
				return m, m.routeMenu(opts[m.index])
			}
		case "enter":
			if len(opts) > 0 {
				if m.choosingRoute {
					m.cfg.forced = opts[m.index]
				} else {
					m.cfg.model = opts[m.index]
				}
				m.chosen = true
				return m, tea.Quit
			}
		case "backspace":
			if !m.choosingRoute && len(m.query) > 0 {
				_, size := utf8.DecodeLastRuneInString(m.query)
				m.query = m.query[:len(m.query)-size]
				m.index = 0
			}
		default:
			if !m.choosingRoute && t.Text != "" {
				m.query += t.Text
				m.index = 0
			}
		}
	}
	return m, nil
}
func (m *picker) View() tea.View {
	title := "Choose a model"
	sub := "Search  " + m.query + "▏"
	if m.choosingRoute {
		title = "Route " + m.cfg.model
		sub = "Choose a subscription"
	}
	w, h := max(1, m.width), max(1, m.height)
	blank := func() string { return richBand("", w, petalText, petalInk, false) }
	row := func(s, fg string, bold bool) string { return richBand(petalLabel(s), w, fg, petalInk, bold) }
	var rows []string
	status := "New session"
	if m.resume {
		status = "Resume session"
	}
	rows = petalMasthead(w, 1, 0, "", status)
	rows = append(rows, blank(), row("  "+title, petalText, true), row("  "+sub, petalMuted, false), blank())
	opts := m.options()
	n := max(1, h-7-len(m.cfg.hints))
	start := max(0, m.index-n+1)
	for i := start; i < min(start+n, len(opts)); i++ {
		label := opts[i]
		if m.choosingRoute {
			if label == "" {
				label = "Auto   ·   most remaining capacity"
				if m.resume {
					label = "Auto   ·   resume on original subscription"
				}
			} else {
				detail := m.usage[label]
				if detail == "" {
					detail = "checking quota…"
				}

				label += "   ·   " + detail
			}
		}
		if !m.choosingRoute && w >= 48 {
			var names []string
			for name, a := range m.cfg.Accounts {
				if (m.cfg.forced == "" || name == m.cfg.forced) && len(a.Models[label]) > 0 {
					names = append(names, name)
				}
			}
			slices.Sort(names)
			nameWidth := min(w/2, 36)
			label = ansi.Truncate(label, nameWidth, "…") + strings.Repeat(" ", max(2, nameWidth-ansi.StringWidth(label)+2)) + strings.Join(names, ", ")
		}
		label = ansi.Truncate(petalLabel(label), max(1, w-5), "…")
		if i == m.index {
			rows = append(rows, richBand(" › "+label, w, petalRose, petalSurface, true))
		} else {
			rows = append(rows, row("   "+label, petalText, false))
		}
	}
	if len(opts) == 0 {
		rows = append(rows, row("  No matching models.", petalMuted, false))
	}
	if !m.choosingRoute {
		for _, hint := range m.cfg.hints {
			rows = append(rows, row("  "+hint, petalMuted, false))
		}
	}
	for len(rows) < h-1 {
		rows = append(rows, blank())
	}
	hint := "  Enter start   Tab override subscription"
	if m.resume {
		hint = "  Enter resume   Tab override subscription"
	}
	if m.choosingRoute {
		hint = "  Enter confirm   Esc back"
	} else if m.cfg.forced != "" {
		hint = "  Enter confirm   Account " + m.cfg.forced
	}
	rows = append(rows, row(hint, petalMuted, false))
	rows = rows[:min(h, len(rows))]
	view := tea.NewView(strings.Join(rows, "\n"))
	view.AltScreen = true
	return view
}
func pick(c config, resume bool) (config, bool, error) {
	models := c.modelNames()
	if len(models) == 0 {
		return c, false, fmt.Errorf("no usable models found; install an ACP adapter or configure an account in petal/config.toml. %s", strings.Join(c.hints, "; "))
	}
	m := &picker{cfg: c, models: models, resume: resume}
	_, err := tea.NewProgram(m).Run()
	return m.cfg, m.chosen, err
}
