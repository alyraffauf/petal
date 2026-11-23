package tui

import (
	"context"

	"fmt"
	"image"

	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"bloomfx.dev/bloom"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"

	"petal/internal/proxy"
	"petal/internal/routing"
	"petal/internal/sessions"
)

type balances []routing.Quota
type picker struct {
	motion                                  centerMotion
	monochrome                              bool
	transcripts                             *transcriptSearch
	pointerPane, pointerRows, pointerDetail image.Rectangle
	pointerFirst                            int
	center                                  controlCenter
	background                              string
	history, fromCenter                     bool
	otherProjects                           []sessions.ProjectThread
	saved                                   []sessions.Record
	historyError                            string
	id                                      string
	cfg                                     routing.Config
	models                                  []string
	search                                  textinput.Model
	matches                                 fuzzy.Matches
	routes                                  []string
	usage                                   map[string]string
	index, width, height                    int
	choosingRoute, chosen, resume           bool
	routeOnly, embedded                     bool
	setting                                 *settingMenu
}

func (m *picker) Init() tea.Cmd {
	if m.history {
		if !m.embedded && m.motion.started.IsZero() {
			m.motion.started = time.Now()
		}
		return tea.Batch(m.watchChildStatuses(), m.motionTick())
	}
	return nil
}
func newPicker(cwd string, c routing.Config) *picker {
	search := textinput.New()
	search.Prompt = "  Search  "
	search.SetVirtualCursor(true)
	styles := search.Styles()
	styles.Cursor.Blink = false
	styles.Cursor.Color = lipgloss.Color(roseHex)
	search.SetStyles(styles)
	search.KeyMap.Paste.SetEnabled(false) // Terminal pastes arrive as tea.PasteMsg.
	search.Focus()
	m := &picker{center: controlCenter{cwd: cwd}, cfg: c, background: canvasBackground(c.TransparentBackground), models: c.ModelNames(), search: search}
	m.filter()
	return m
}

func (m *picker) showHistory() {
	m.history = true
	m.historyError = ""
	m.center.cache = centerCache{}
	m.saved = nil
	cwd := m.center.cwd
	m.center.project = filepath.Base(cwd)
	ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	if branch, err := exec.CommandContext(ctx, "git", "-C", cwd, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); err == nil {
		m.center.project += " / " + strings.TrimSpace(string(branch))
	}
	saved, err := sessions.List(cwd, m.models)
	m.saved = saved
	if err != nil {
		m.historyError = err.Error()
	}
	other, otherErr := sessions.OtherProjectThreads(cwd)
	m.otherProjects = other
	if otherErr != nil {
		m.historyError = otherErr.Error()
	}
	// Delegated records can be saved before route metadata is available.
	for i, record := range m.saved {
		if record.DelegatedParent == "" || record.Account != "" {
			continue
		}
		if route := proxy.DecodeSessionRoute(record.ID); route != nil && route[1] == record.Model {
			m.saved[i].Account = route[0]
		}
	}
	if pin := m.cfg.PinnedAccount; pin != "" {
		m.saved = slices.DeleteFunc(m.saved, func(r sessions.Record) bool { return r.Account != pin })
	}
	m.search.Reset()
	m.center.folded = map[string]bool{"turns": true}
	m.center.detailScroll = backlogViewport{browsing: true}
}

func (m *picker) filter() {
	names := m.models

	if query := m.search.Value(); query != "" {
		keys := slices.Clone(names)
		if m.setting == nil {
			for i, id := range names {
				if label := m.cfg.ModelLabel(id); label != id {
					keys[i] = label + " " + id
				}
			}
		}
		m.matches = fuzzy.Find(query, keys)
		for i := range m.matches {
			m.matches[i].Str = names[m.matches[i].Index]
		}
	} else {
		m.matches = make(fuzzy.Matches, len(names))
		for i, name := range names {
			m.matches[i] = fuzzy.Match{Index: i, Str: name}
		}
	}
	m.index = 0
}

func (m *picker) count() int {
	if m.choosingRoute {
		return len(m.routes)
	}
	return len(m.matches)
}

func (m *picker) option(i int) string {
	if m.choosingRoute {
		return m.routes[i]
	}
	return m.matches[i].Str
}

func (m *picker) editSearch(msg tea.Msg) tea.Cmd {
	if m.choosingRoute {
		return nil
	}
	old := m.search.Value()
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	if m.search.Value() != old {
		m.filter()
	}
	return cmd
}
func (m *picker) routeMenu(model string) tea.Cmd {
	m.cfg.Model = model
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
		return routeBalances{model, c.Scores(ctx)}
	}
}

type routeBalances struct {
	model  string
	scores balances
}

func (m *picker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.center.childStatuses != nil {
		defer func() {
			if !m.history {
				m.stopWatchingChildStatuses()
			}
		}()
	}
	if profile, ok := msg.(tea.ColorProfileMsg); ok {
		m.monochrome = profile.Profile <= colorprofile.ASCII
	}
	if m.transcripts != nil {
		switch msg.(type) {
		case tea.KeyPressMsg, tea.PasteMsg, tea.MouseWheelMsg, tea.MouseClickMsg, transcriptResults:
			return m, m.transcriptInput(msg)
		}
	}
	switch t := msg.(type) {
	case centerFrame:
		if t.owner != m {
			return m, nil
		}
		m.motion.pending = false
		m.motion.frame++
		return m, m.motionTick()
	case childStatusesChanged:
		cmd := m.refreshChildStatuses(t)
		return m, tea.Batch(cmd, m.motionTick())
	case childCancellationResult:
		if t.picker == m {
			m.childCancellationFinished(t)
		}
	case tea.MouseClickMsg:
		if t.Button != tea.MouseLeft {
			break
		}
		point := image.Pt(t.X, t.Y)
		if m.history && m.center.graphID == "" && point.In(m.center.cancelButton) {
			nodes := m.nodes()
			if len(nodes) > 0 {
				n := nodes[m.selectedNode(nodes)]
				if m.canCancelChild(n) && n.binding == m.center.cancelTarget {
					return m, m.cancelChild(n)
				}
			}
			break
		}
		if !point.In(m.pointerRows) {
			break
		}
		index := m.pointerFirst + t.Y - m.pointerRows.Min.Y
		switch {
		case m.history && m.center.graphID != "":
			m.center.edge = index
		case m.history:
			nodes := m.nodes()
			if index < len(nodes) {
				m.selectNode(nodes[index])
			}
		case index < m.count():
			m.index = index
		}
	case tea.MouseWheelMsg:
		delta := 1
		if t.Button == tea.MouseWheelUp {
			delta = -1
		} else if t.Button != tea.MouseWheelDown {
			break
		}
		point := image.Pt(t.X, t.Y)
		if m.history && point.In(m.pointerDetail) {
			m.center.detailScroll.move(3 * delta)
			break
		}
		if !point.In(m.pointerPane) {
			break
		}
		if m.history {
			code := tea.KeyDown
			if delta < 0 {
				code = tea.KeyUp
			}
			return m, m.centerKey(tea.KeyPressMsg{Code: code})
		}
		m.index = max(0, min(m.count()-1, m.index+delta))
	case tea.WindowSizeMsg:
		m.width = t.Width
		m.height = t.Height
	case remoteSessions:
		if t.picker != m || !m.history {
			return m, nil
		}
		m.center.cache = centerCache{}
		type threadKey struct{ project, id string }
		byProject := make(map[threadKey]int, len(m.otherProjects))
		directories := make(map[string]string, len(t.other))
		for _, r := range t.other {
			directories[r.Project] = r.Directory
		}
		for i, r := range m.otherProjects {
			if cwd := directories[r.Project]; cwd != "" {
				m.otherProjects[i].Directory = cwd
			}
			byProject[threadKey{r.Project, r.ID}] = i
		}
		for _, r := range t.other {
			key := threadKey{r.Project, r.ID}
			if i, ok := byProject[key]; ok {
				if m.otherProjects[i].Title == "" {
					m.otherProjects[i].Title = r.Title
				}
			} else {
				byProject[key] = len(m.otherProjects)
				m.otherProjects = append(m.otherProjects, r)
			}
		}
		slices.SortStableFunc(m.otherProjects, func(a, b sessions.ProjectThread) int { return strings.Compare(a.Project, b.Project) })
		known := make(map[string]int, len(m.saved))
		for index, r := range m.saved {
			known[r.ID] = index
		}
		for _, r := range t.records {
			if index, ok := known[r.ID]; ok {
				if activity := r.LastActivity(); activity.After(m.saved[index].LastActivity()) {
					m.saved[index].Updated = activity
				}
				continue
			}
			known[r.ID] = len(m.saved)
			m.saved = append(m.saved, r)
		}
		slices.SortStableFunc(m.saved, func(a, b sessions.Record) int { return b.LastActivity().Compare(a.LastActivity()) })
		if t.err != nil {
			m.historyError = t.err.Error()
		}
	case routeBalances:
		if t.model == m.cfg.Model {
			m.usage = map[string]string{}
			for _, s := range t.scores {
				switch {
				case s.Err != nil:
					if routing.RemainingBackoff(s.Name) > 0 {
						m.usage[s.Name] = s.Err.Error()
					} else {
						m.usage[s.Name] = "probe unavailable"
					}
				case s.Remaining < 0:
					m.usage[s.Name] = "unmetered"
				default:
					m.usage[s.Name] = fmt.Sprintf("%.0f%% remaining", s.Remaining)
				}
				if s.Tier != "" {
					m.usage[s.Name] += " · " + s.Tier
				}
				if s.Capacity > 0 && s.Capacity != 1 {
					m.usage[s.Name] += fmt.Sprintf(" · %g× capacity", s.Capacity)
				}
			}
		}
	case tea.PasteMsg:
		if m.history {
			if m.center.graphID == "" && !m.center.inspect {
				t.Content = petalLabel(t.Content)
				m.search, _ = m.search.Update(t)
			}
			return m, nil
		}
		t.Content = strings.Join(strings.Fields(clean(t.Content)), " ")
		return m, m.editSearch(t)
	case tea.KeyPressMsg:
		if key.Matches(t, newThread) {
			if model := m.cfg.DefaultModel(); model != "" {
				m.cfg.Model = model
				m.chosen = true
				m.resume = false
				m.id = ""
				return m, tea.Quit
			}
			return m, nil
		}
		if m.history {
			return m, m.centerKey(t)
		}
		count := m.count()
		switch {
		case key.Matches(t, quitPetal):
			return m, tea.Quit
		case key.Matches(t, resumeConversation) && !m.choosingRoute && m.setting == nil:
			m.showHistory()
			return m, m.watchChildStatuses()
		case key.Matches(t, dismissMenu):
			if m.choosingRoute {
				m.choosingRoute = false
				m.index = max(0, slices.IndexFunc(m.matches, func(match fuzzy.Match) bool { return match.Str == m.cfg.Model }))
			} else if m.fromCenter {
				m.showHistory()
				m.fromCenter = false
				return m, m.watchChildStatuses()
			} else {
				return m, tea.Quit
			}
		case key.Matches(t, selectNext):
			m.index = min(m.index+1, max(0, count-1))
		case key.Matches(t, selectPrevious) || t.String() == "ctrl+p":
			m.index = max(0, m.index-1)
		case key.Matches(t, pageLater):
			m.index = min(m.index+max(1, m.height-9), max(0, count-1))
		case key.Matches(t, pageEarlier):
			m.index = max(0, m.index-max(1, m.height-9))
		case key.Matches(t, chooseRoute):
			if m.setting == nil && !m.history && !m.choosingRoute && m.cfg.PinnedAccount == "" && count > 0 {
				return m, m.routeMenu(m.option(m.index))
			}
		case key.Matches(t, confirmChoice):
			if count > 0 {
				if m.setting == nil {
					switch {
					case m.choosingRoute:
						m.cfg.PinnedAccount = m.option(m.index)
					default:
						m.cfg.Model = m.option(m.index)
					}
				}
				m.chosen = true
				return m, tea.Quit
			}
		default:
			return m, m.editSearch(t)
		}
	}
	return m, nil
}
func (m *picker) View() tea.View {
	if m.transcripts != nil {
		return m.transcriptView()
	}
	if m.history {
		return m.centerView()
	}
	title := "choose a model"
	w, h := max(1, m.width), max(1, m.height)
	m.search.SetWidth(max(1, w-ansi.StringWidth(m.search.Prompt)-1))
	sub := m.search.View()
	if m.choosingRoute {
		title = "route " + m.cfg.ModelLabel(m.cfg.Model)
		sub = "  choose a subscription"
	}
	blank := func() string { return richBand("", w, petalText, m.background, false) }
	row := func(s, fg string, bold bool) string { return richBand(petalLabel(s), w, fg, m.background, bold) }
	var rows []string
	status := "new session"
	if m.setting != nil {
		title, status = m.setting.title, "settings"
	}
	if m.resume {
		status = "resume"
	}
	if h >= 5 {
		rows = []string{petalMasthead(w, 0, "control center", status, false, m.background)}
	}
	if h >= 8 {
		rows = append(rows, blank())
	}
	if h >= 4 {
		rows = append(rows, row("  "+title, petalRose, true))
	}
	if h >= 2 {
		rows = append(rows, richBand(sub, w, petalMuted, m.background, false))
	}
	if h >= 8 {
		rows = append(rows, blank())
	}
	count := m.count()
	hintRows := 0
	if !m.choosingRoute && !m.history && m.setting == nil {
		hintRows = len(m.cfg.Hints)
	}
	n := max(1, h-len(rows)-navFooterHeight(h)-hintRows)
	m.pointerPane = image.Rect(0, len(rows), w, h-navFooterHeight(h))
	m.pointerDetail = image.Rectangle{}
	m.pointerRows = image.Rect(0, len(rows), w, min(h, len(rows)+min(n, count)))
	m.pointerFirst = max(0, m.index-n+1)
	if count > 0 {
		rowWidth := w
		if count > n && w > 1 {
			rowWidth--
		}
		list := bloom.List(count, func(i int) bloom.Child {
			return bloom.Text(m.resultRow(i, rowWidth))
		}).ID("results").Keep(m.index)
		var content bloom.Child = list
		if rowWidth < w {
			bar := bloom.ScrollbarOf("results").Style(bloom.ScrollbarStyle{ThumbStyle: lipgloss.NewStyle().Foreground(lipgloss.Color(roseHex))})
			content = bloom.Row(bloom.Fill(list), bloom.Fixed(1, bar))
		}
		rows = append(rows, strings.Split(bloom.Render(w, min(n, count), content), "\n")...)
	}
	if count == 0 {
		message := "No matching models."
		if m.setting != nil {
			message = "No matching choices."
		}
		rows = append(rows, row("  "+message, petalMuted, false))
	}
	if hintRows > 0 {
		for _, hint := range m.cfg.Hints {
			rows = append(rows, row("  "+hint, petalMuted, false))
		}
	}
	for len(rows) < h-navFooterHeight(h) {
		rows = append(rows, blank())
	}
	hint := shortHelp(w, resumeConversation, confirmChoice, chooseRoute)
	if m.choosingRoute {
		hint = shortHelp(w, confirmChoice, dismissMenu)
	} else if m.history || m.setting != nil {
		hint = shortHelp(w, confirmChoice, dismissMenu)
	} else if m.cfg.PinnedAccount != "" {
		hint = shortHelp(w, resumeConversation, confirmChoice) + "   Account " + m.cfg.PinnedAccount
	}
	if m.embedded && !m.history && !m.choosingRoute && m.setting == nil {
		hint = shortHelp(w, confirmChoice, dismissMenu, chooseRoute, resumeConversation)
	}
	rows = appendNavFooter(rows, w, h, hint, m.background)
	rows = rows[:min(h, len(rows))]
	view := tea.NewView(strings.Join(rows, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}
func highlightMatch(label string, positions []int) string {
	var b strings.Builder
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(roseHex)).Underline(true)
	for offset, r := range label {
		if len(positions) > 0 && offset == positions[0] {
			b.WriteString(style.Render(string(r)))
			positions = positions[1:]
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m *picker) resultRow(i, w int) string {
	label := m.option(i)
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
				if m.usage != nil {
					detail = "probe unavailable"
				}
			}

			label += "   ·   " + detail
		}
	}
	raw := label
	if !m.choosingRoute && m.setting == nil {
		label = m.cfg.ModelLabel(raw)
	}
	label = petalLabel(label)
	if !m.choosingRoute && len(m.matches[i].MatchedIndexes) > 0 {
		label = highlightMatch(label, m.matches[i].MatchedIndexes)
	}
	if m.setting == nil && !m.history && !m.choosingRoute && w >= 48 {
		var names []string
		for name, a := range m.cfg.Accounts {
			if (m.cfg.PinnedAccount == "" || name == m.cfg.PinnedAccount) && len(a.Models[raw]) > 0 {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		nameWidth := min(w/2, 36)
		label = ansi.Truncate(label, nameWidth, "…") + strings.Repeat(" ", max(2, nameWidth-ansi.StringWidth(label)+2)) + petalLabel(strings.Join(names, ", "))
	}
	label = ansi.Truncate(label, max(1, w-5), "…")
	if i == m.index {
		return richBand("", 1, petalText, m.background, false) + richBand(" › "+label, w-1, petalText, selectionBand, true)
	}
	return richBand("    "+label, w, petalText, m.background, false)
}

func pick(cwd string, c routing.Config, resume bool) (sessionStart, bool, error) {
	m := newPicker(cwd, c)
	m.showHistory()
	if resume && len(m.saved) > 0 {
		m.center.selected = m.saved[0].ID
	}
	defer m.stopWatchingChildStatuses()
	_, err := tea.NewProgram(&frame{Model: m, background: m.background}).Run()
	return sessionStart{cwd: m.center.cwd, cfg: m.cfg, resume: m.resume, id: m.id}, m.chosen, err
}
