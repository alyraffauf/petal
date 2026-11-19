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
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
)

type entry struct{ Role, Text string }
type screen struct {
	backlog                            *backlogViewport
	markdown                           *markdownCache
	Width, Height, Frame, cursorColumn int
	Model, Account, Cwd, Status, Input string
	Messages                           []entry
	Busy                               bool
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
	var lines []entry
	for _, e := range v.Messages {
		if len(lines) > 0 {
			lines = append(lines, entry{})
		}
		for _, s := range v.messageLines(e, width) {
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
	launch        *picker
	next          *config
	tools         map[string]toolEvent
	reader        anim.Presence
	readerBacklog backlogViewport
	animating     bool
	replay        bool
	screen
	root          *bloom.Root
	h             *harness
	ctx           context.Context
	cfg           config
	resume, ready bool
	draft         []rune
	cursor        int
	ask           *permission
	chatBacklog   backlogViewport
	pending       turnLog
	turnStart     int
}

func demoScreen() screen {
	return screen{Model: "sonnet", Account: "studio", Cwd: "~/src/petal", Status: "Demo · ready", Messages: []entry{
		{"user", "Where does petal decide which account gets a new thread?"},
		{"tool", "Read  accounts.go  ·  config.choose"},
		{"assistant", "Placement happens in config.choose. It probes the accounts that serve your model and picks the one with the most remaining quota."},
		{"user", "What happens to **my thread** if the `work` account runs low?"},
		{"tool", "Read  main.go  ·  proxy.open"},
		{"assistant", "### Your thread stays pinned\nExisting turns stay on **the original adapter**. Only new sessions run the quota probes.\n\n| Action | Route |\n| :--- | :--- |\n| Continue | Original account |\n| Start fresh | Highest quota |\n\nResume the thread:\n\n```sh\npetal tui sonnet --resume\n```"},
	}}
}
func runTUI(parent context.Context, c config, resume, demo bool) error {
	for {
		next, err := runSession(parent, c, resume, demo)
		if err != nil || next == nil {
			return err
		}
		c = *next
		resume = false
	}
}
func runSession(parent context.Context, c config, resume, demo bool) (*config, error) {
	ctx, stop := context.WithCancel(parent)
	defer stop()
	m := &terminal{reader: anim.NewPresence(anim.Duration(160 * time.Millisecond)), root: bloom.New(), ctx: ctx, cfg: c, resume: resume}
	if demo {
		m.screen = demoScreen()
		m.ready = true
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		m.screen = screen{Cwd: cwd, Model: c.model, Account: c.forced, Status: "Connecting"}
		m.h = startHarness(ctx, c)
		defer m.h.close()
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
	v.backlog = &m.chatBacklog
	if len(m.draft) > 0 {
		prefix := strings.ReplaceAll(string(m.draft[:m.cursor]), "\n", " ↵ ")
		v.cursorColumn = ansi.StringWidth(prefix)
		v.Input = prefix + "▏" + strings.ReplaceAll(string(m.draft[m.cursor:]), "\n", " ↵ ")
	}
	v.Cwd = clean(v.Cwd)
	v.Account = clean(v.Account)
	v.Model = clean(v.Model)
	view := m.root.View(v.Width, v.Height, m.scene(v))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}
func (m *terminal) insert(s string) {
	r := []rune(clean(s))
	tail := append([]rune(nil), m.draft[m.cursor:]...)
	m.draft = append(append(m.draft[:m.cursor], r...), tail...)
	m.cursor += len(r)
}
func (m *terminal) submit() tea.Cmd {
	text := strings.TrimSpace(string(m.draft))
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
	} else {
		if m.Busy || !m.ready {
			return nil
		}
		m.Messages = append(m.Messages, entry{"user", text})
		m.turnStart = len(m.Messages)
		m.chatBacklog.browsing = false
		if m.h == nil {
			m.Messages = append(m.Messages, entry{"system", "Demo session. Launch petal tui MODEL to send this prompt to your agent."})
		} else {
			m.Busy = true
			m.Status = "Working"
			m.draft = nil
			m.cursor = 0
			return func() tea.Msg {
				result, err := acp.SendRequest[acp.PromptResponse](m.h.conn, m.ctx, "session/prompt", map[string]any{"sessionId": m.h.id, "prompt": []any{map[string]string{"type": "text", "text": text}}})
				select {
				case m.h.events <- finished{err, string(result.StopReason)}:
				case <-m.ctx.Done():
				}
				return nil
			}
		}
	}
	m.draft = nil
	m.cursor = 0
	return nil
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
			if t.String() == "esc" && !m.launch.choosingRoute {
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
				m.next = &m.launch.cfg
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
			m.Status = "Connection failed"
			m.Messages = append(m.Messages, entry{"system", terminalError(t.err)})
		} else {
			m.Messages = m.pending.commit(m.Messages[:m.turnStart])
			m.ready = true
			m.Account = t.account
			m.Status = "Ready"
		}
	case finished:
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
		}
		if t.err != nil {
			m.Messages = append(m.Messages, entry{"system", terminalError(t.err)})
		}
	case cancelFailed:
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
		if !m.reader.Shown() {
			m.insert(t.Content)
		}
	case tea.KeyPressMsg:
		if m.reader.Shown() {
			step := max(1, m.Height-8)
			switch t.String() {
			case "esc", "f3":
				cmd = m.reader.Hide()
			case "ctrl+c":
				cmd = tea.Quit
			case "pgup":
				m.readerBacklog.move(-step)
			case "end", "ctrl+end":
				m.readerBacklog.browsing = false
			case "up":
				m.readerBacklog.move(-1)
			case "pgdown":
				m.readerBacklog.move(step)
			case "down":
				m.readerBacklog.move(1)
			}
			break
		}
		switch t.String() {
		case "ctrl+n":
			if m.h != nil && !m.Busy && m.Status != "Connecting" {
				c := m.cfg
				c.forced = ""
				m.launch = &picker{cfg: c, models: c.modelNames(), width: m.Width, height: m.Height}
			}
		case "f3":
			m.readerBacklog.browsing = false
			cmd = m.reader.Show()
		case "ctrl+r":
			if m.h == nil {
				sample := demoScreen()
				m.Messages = sample.Messages
				last := m.Messages[len(m.Messages)-1]
				m.Messages = m.Messages[:len(m.Messages)-1]
				m.pending = turnLog{}
				m.pending.push(event{role: last.Role, text: last.Text, chunk: true})
				m.replay = true
				m.chatBacklog.browsing = false
				m.Frame = 0
				m.Status = "Demo · working"
				m.Busy = true
			}
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			cmd = m.submit()
		case "esc":
			if m.ask != nil {
				m.recordNote("Permission declined.")
				m.ask.reply <- -1
				m.ask = nil
				m.Status = "Working"
			} else if m.Busy && m.h == nil {
				m.replay = false
				m.pending = turnLog{}
				m.Busy = false
				m.Status = "Demo · ready"
			} else if m.Busy {
				m.Status = "Cancelling"
				cmd = func() tea.Msg {
					err := m.h.conn.SendNotification(m.ctx, "session/cancel", map[string]string{"sessionId": m.h.id})
					if err != nil {
						return cancelFailed{err}
					}
					return nil
				}
			}
		case "pgup":
			m.chatBacklog.move(-max(1, m.Height/2))
		case "pgdown":
			m.chatBacklog.move(max(1, m.Height/2))
		case "ctrl+end":
			m.chatBacklog.browsing = false
		case "left":
			m.cursor = max(0, m.cursor-1)
		case "right":
			m.cursor = min(len(m.draft), m.cursor+1)
		case "home", "ctrl+a":
			m.cursor = 0
		case "end", "ctrl+e":
			m.cursor = len(m.draft)
		case "ctrl+u":
			m.draft = nil
			m.cursor = 0
		case "backspace":
			if m.cursor > 0 {
				m.draft = append(m.draft[:m.cursor-1], m.draft[m.cursor:]...)
				m.cursor--
			}
		case "delete":
			if m.cursor < len(m.draft) {
				m.draft = append(m.draft[:m.cursor], m.draft[m.cursor+1:]...)
			}
		default:
			if t.Text != "" {
				m.insert(t.Text)
			}
		}
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
