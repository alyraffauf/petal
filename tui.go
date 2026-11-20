package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/anim"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
)

type entry struct{ Role, Text string }
type connectionState uint8

const (
	connecting connectionState = iota
	connected
	connectionFailed
)

type screen struct {
	connection                  connectionState
	busyAction                  string
	queued                      int
	backlog                     *backlogViewport
	markdown                    *markdownCache
	Width, Height, Frame        int
	editor                      *textarea.Model
	Model, Account, Cwd, Status string
	Messages                    []entry
	Busy                        bool
	commands                    []chatCommand
	commandIndex                int
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// While browsing, keep the top row fixed as new messages arrive.
type backlogViewport struct {
	top, limit int
	browsing   bool
}

func (b *backlogViewport) move(rows int) {
	b.top = max(0, min(b.limit, b.top+rows))
	b.browsing = b.top < b.limit
}

func transcript(v screen, width, height int) []entry {
	if width <= 0 || height <= 0 {
		return nil
	}
	if v.markdown == nil {
		v.markdown = &markdownCache{}
	}
	if n := len(v.Messages); n < len(v.markdown.layouts) {
		clear(v.markdown.layouts[n:])
		v.markdown.layouts = v.markdown.layouts[:n]
	}
	var lines []entry
	for index, e := range v.Messages {
		if len(lines) > 0 {
			lines = append(lines, entry{})
		}
		for _, s := range v.messageLines(index, e, width) {
			lines = append(lines, entry{e.Role, s})
		}
	}
	top := max(0, len(lines)-height)
	if b := v.backlog; b != nil {
		b.limit = top
		if b.browsing {
			top = min(top, b.top)
		}
		b.top = top
		b.browsing = top < b.limit
	}
	return lines[top:min(len(lines), top+height)]
}

type animationFrame struct{}

func nextFrame() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return animationFrame{} })
}

type terminal struct {
	outbox                []string
	steering, waitForTurn bool
	lateFinish            *finished
	permissionDraft       *textarea.Model
	launch                *picker
	next                  *sessionStart
	tools                 map[string]toolEvent
	reader                anim.Presence
	readerBacklog         backlogViewport
	readerMarkdown        markdownCache
	animating             bool
	replay                bool
	screen
	root               *bloom.Root
	h                  *harness
	ctx                context.Context
	cfg                config
	resume, cancelling bool
	ask                *permission
	chatBacklog        backlogViewport
	pending            turnLog
	turnStart          int
}

func demoScreen() screen {
	return screen{Model: "sonnet", Account: "studio", Cwd: "~/src/petal", Status: "Demo · ready", Messages: []entry{
		{"user", "Where does petal decide which account gets a new thread?"},
		{"tool", "Read  accounts.go  ·  config.choose"},
		{"assistant", "Placement happens in config.choose. It probes the accounts that serve your model and picks the one with the most remaining quota."},
		{"user", "What happens to **my thread** if the `work` account runs low?"},
		{"tool", "Read  proxy.go  ·  proxy.open"},
		{"assistant", "### Your thread stays pinned\nExisting turns stay on **the original adapter**. Only new sessions run the quota probes.\n\n| Action | Route |\n| :--- | :--- |\n| Continue | Original account |\n| Start fresh | Highest quota |\n\nResume the thread:\n\n```sh\npetal tui sonnet --resume\n```"},
	}}
}

type sessionStart struct {
	cfg    config
	resume bool
	editor *textarea.Model
}

func runTUI(parent context.Context, c config, resume, demo bool) error {
	start := sessionStart{cfg: c, resume: resume}
	for {
		next, err := runSession(parent, start, demo)
		if err != nil || next == nil {
			return err
		}
		start = *next
	}
}
func runSession(parent context.Context, start sessionStart, demo bool) (*sessionStart, error) {
	c := start.cfg
	ctx, stop := context.WithCancel(parent)
	defer stop()
	m := &terminal{reader: anim.NewPresence(anim.Duration(160 * time.Millisecond)), root: bloom.New(), ctx: ctx, cfg: c, resume: start.resume}
	if demo {
		m.screen = demoScreen()
		m.connection = connected
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		m.screen = screen{Cwd: cwd, Model: c.model, Account: c.forced, connection: connecting}
		m.h = startHarness(ctx, c)
		defer m.h.close()
	}
	m.editor = start.editor
	if m.editor == nil {
		m.editor = newComposer()
	}
	m.markdown = &markdownCache{}
	_, err := tea.NewProgram(m).Run()
	return m.next, err
}

type opened struct {
	account string
	err     error
}
type finished struct {
	err    error
	reason string
}
type cancelFailed struct{ err error }

func (m *terminal) listen() tea.Msg {
	select {
	case e := <-m.h.events:
		return e
	case <-m.ctx.Done():
		return nil
	}
}
func (m *terminal) Init() tea.Cmd {
	if m.h == nil {
		return m.root.Init()
	}
	return tea.Batch(m.root.Init(), m.listen, func() tea.Msg {
		a, e := m.h.open(m.ctx, m.cfg, m.Cwd, m.resume)
		select {
		case m.h.events <- opened{a, e}:
		case <-m.ctx.Done():
		}
		return nil
	})
}
func (m *terminal) View() tea.View {
	if m.launch != nil {
		return m.launch.View()
	}
	v := m.screen
	if m.Busy && m.connection == connected && m.h != nil {
		v.busyAction = "Follow up"
		if m.canSteer() {
			v.busyAction = "Steer"
		}
		if m.ask != nil {
			v.busyAction = "Choose"
		}
		v.queued = len(m.outbox)
	}
	v.commands = m.commandMatches()
	v.commandIndex = min(m.commandIndex, max(0, len(v.commands)-1))
	v.backlog = &m.chatBacklog
	v.Cwd = clean(v.Cwd)
	v.Account = clean(v.Account)
	v.Model = clean(v.Model)
	view := m.root.View(v.Width, v.Height, m.scene(v))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}
func (m *terminal) submit() tea.Cmd {
	text := strings.TrimSpace(m.editor.Value())
	if text == "" {
		return nil
	}
	if m.ask != nil {
		i, err := strconv.Atoi(text)
		if err != nil || i < 1 || i > len(m.ask.options) {
			m.Status = "Enter an option number"
			return nil
		}
		m.recordNote("Permission · " + m.ask.options[i-1].Name)
		m.ask.reply <- i - 1
		m.ask = nil
		m.Status = "Working"
		m.restorePermissionDraft()
		return m.deliver()
	}
	if m.connection != connected {
		return nil
	}
	m.editor.Reset()
	if m.h == nil {
		m.Messages = append(m.Messages, entry{"user", text}, entry{"system", "Demo session. Launch petal tui MODEL to send this prompt to your agent."})
		return nil
	}
	m.outbox = append(m.outbox, text)
	return m.deliver()
}

func (m *terminal) restorePermissionDraft() {
	m.editor, m.permissionDraft = m.permissionDraft, nil
}

func (m *terminal) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.launch != nil {
		switch t := msg.(type) {
		case tea.WindowSizeMsg:
			m.Width = t.Width
			m.Height = t.Height
		case tea.KeyPressMsg:
			if t.String() == "ctrl+c" {
				return m, tea.Quit
			}
			if t.String() == "esc" && (!m.launch.choosingRoute || m.launch.routeOnly) {
				m.launch = nil
				return m, nil
			}
		}
		switch msg.(type) {
		case tea.MouseWheelMsg:
			return m, nil
		case tea.WindowSizeMsg, tea.KeyPressMsg, tea.PasteMsg, routeBalances:
			_, cmd := m.launch.Update(msg)
			if m.launch.chosen {
				m.next = &sessionStart{cfg: m.launch.cfg, resume: m.launch.resume}
				if m.connection == connectionFailed {
					m.next.editor = m.editor
				}
				return m, tea.Quit
			}
			return m, cmd
		}
	}
	ui := tea.Batch(m.root.Update(msg), anim.Update(msg, &m.reader))
	var cmd tea.Cmd
	switch t := msg.(type) {
	case animationFrame:
		m.animating = false
		m.Frame++
		if m.replay && m.Frame >= 12 {
			m.Messages = m.pending.commit(m.Messages)
			m.replay = false
			m.Busy = false
			m.Status = "Demo · ready"
		}
	case tea.WindowSizeMsg:
		m.Width = t.Width
		m.Height = t.Height
	case opened:
		clear(m.tools)
		cmd = m.listen
		if t.err != nil {
			m.pending = turnLog{}
			m.connection = connectionFailed
			m.Messages = append(m.Messages, entry{"system", terminalError(t.err)})
		} else {
			m.Messages = m.pending.commit(m.Messages[:m.turnStart])
			m.connection = connected
			m.Account = t.account
			m.Status = "Ready"
		}
	case steerResult:
		m.steering = false
		text := m.outbox[0]
		switch {
		case t.err == nil && t.outcome == "promptRequired":
			m.waitForTurn = true
		case t.err == nil && t.outcome == "injected":
			m.outbox = m.outbox[1:]
			e := event{role: "user", text: text}
			m.pending.push(e)
			m.Messages = append(m.Messages, entry{e.role, e.text})
		default:
			m.outbox = m.outbox[1:]
			note := "Steer delivery unconfirmed"
			if t.err != nil {
				note += " · " + terminalError(t.err)
			} else {
				note += " · " + t.outcome
			}
			m.recordNote(note + "\n\n" + text)
		}
		if m.lateFinish != nil {
			done := *m.lateFinish
			m.lateFinish = nil
			return m.Update(done)
		}
		cmd = m.deliver()
	case finished:
		if m.steering {
			m.lateFinish = &t
			return m, ui
		}
		m.cancelling = false
		m.waitForTurn = false
		clear(m.tools)
		cmd = m.listen
		m.Busy = false
		m.Status = "Ready"
		if t.err == nil && (t.reason == "end_turn" || t.reason == "") {
			m.Messages = m.pending.commit(m.Messages[:m.turnStart])
		} else {
			m.pending = turnLog{}
			if t.err == nil {
				m.Messages = append(m.Messages, entry{"system", "Turn stopped · " + strings.ReplaceAll(t.reason, "_", " ")})
			}
		}
		if m.ask != nil {
			m.ask.reply <- -1
			m.ask = nil
			m.restorePermissionDraft()
		}
		if t.err != nil {
			m.Messages = append(m.Messages, entry{"system", terminalError(t.err)})
		}
		cmd = tea.Batch(cmd, m.deliver())
	case cancelFailed:
		m.cancelling = false
		m.Status = "Cancellation failed"
		m.recordNote(terminalError(t.err))
	case event:
		cmd = m.listen
		if t.tool != nil {
			var complete bool
			t, complete = m.toolResult(*t.tool)
			if !complete {
				break
			}
		}
		if t.ask != nil {
			m.chatBacklog.browsing = false
			ui = tea.Batch(ui, m.reader.Hide())
			m.permissionDraft, m.editor = m.editor, newComposer()
			m.ask = t.ask
			m.Status = "Permission required"
		}
		m.pending.push(t)
		if !t.chunk {
			m.Messages = append(m.Messages, entry{t.role, t.text})
		}
		cmd = m.listen
	case tea.MouseWheelMsg:
		target := &m.chatBacklog
		if m.reader.Shown() {
			target = &m.readerBacklog
		}
		switch t.Button {
		case tea.MouseWheelUp:
			target.move(-3)
		case tea.MouseWheelDown:
			target.move(3)
		}
	case tea.PasteMsg:
		if !m.reader.Shown() && m.connection != connectionFailed {
			cmd = m.paste(t.Content)
		}
	case tea.KeyPressMsg:
		cmd = m.handleKey(t)
	}
	if !m.animating && (m.replay || m.Busy) {
		m.animating = true
		cmd = tea.Batch(cmd, nextFrame())
	}
	return m, tea.Batch(ui, cmd)
}

func terminalError(err error) string {
	var rpc *acp.RequestError
	if !errors.As(err, &rpc) || rpc.Message == "" {
		return err.Error()
	}
	var data struct{ Details string }
	if json.Unmarshal(rpc.Data, &data) == nil && data.Details != "" && rpc.Message == "Internal error" {
		return rpc.Message + ": " + data.Details
	}
	return rpc.Message
}

func (m *terminal) recordNote(text string) {
	m.pending.push(event{role: "system", text: text})
	m.Messages = append(m.Messages, entry{"system", text})
}
