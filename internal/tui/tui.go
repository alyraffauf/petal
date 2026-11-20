package tui

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"bloomfx.dev/bloom"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"

	"petal/internal/proxy"
	"petal/internal/routing"
	"petal/internal/sessions"
)

type entry struct{ Role, Text string }
type connectionState uint8

const (
	connecting connectionState = iota
	connected
	connectionFailed
)

type screen struct {
	question                    *permission
	background                  string
	hasAgents                   bool
	compactions                 map[string]*compaction
	usage                       *acp.SessionUsageUpdate
	notice                      *acp.SessionUpdateNotice
	planRows                    []entry
	connection                  connectionState
	busyAction, activeModel     string
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

type animationFrame struct{}

func nextFrame() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return animationFrame{} })
}

type terminal struct {
	agents                agentBrowser
	forking               bool
	pendingInfo           acp.SessionSessionInfoUpdate
	agentCommands         agentCommandUpdate
	agentSettings         settingsUpdate
	changingSetting       bool
	outbox                []promptMessage
	steering, waitForTurn bool
	lateFinish            *finished
	permissionDraft       *textarea.Model
	launch                *picker
	next                  *sessionStart
	tools                 map[string]toolEvent
	plans                 map[string]string
	legacyPlan            *entry
	reading               bool
	readerBacklog         backlogViewport
	readerMarkdown        markdownCache
	animating             bool
	screen
	root               *bloom.Root
	h                  *harness
	ctx                context.Context
	cfg                routing.Config
	resumeID           string
	resume, cancelling bool
	ask                *permission
	chatBacklog        backlogViewport
	pending            turnLog
	turnStart          int
}

type sessionStart struct {
	id     string
	cfg    routing.Config
	resume bool
	editor *textarea.Model
}

func Run(parent context.Context, c routing.Config, resume bool) error {
	start, ok, err := pick(c, resume)
	if err != nil || !ok {
		return err
	}
	for {
		next, err := runSession(parent, start)
		if err != nil || next == nil {
			return err
		}
		start = *next
	}
}
func runSession(parent context.Context, start sessionStart) (*sessionStart, error) {
	c := start.cfg
	ctx, stop := context.WithCancel(parent)
	defer stop()
	m := &terminal{root: bloom.New(), ctx: ctx, cfg: c, resume: start.resume, resumeID: start.id}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	m.screen = screen{background: canvasBackground(c.TransparentBackground), Cwd: cwd, Model: c.Model, Account: c.PinnedAccount, connection: connecting}
	m.h = startHarness(ctx, c)
	m.h.id = start.id
	defer m.h.close()
	m.editor = start.editor
	if m.editor == nil {
		m.editor = newComposer()
	}
	m.markdown = &markdownCache{}
	_, err = tea.NewProgram(m).Run()
	if m.h != nil && m.connection == connected && m.h.canClose {
		closeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = acp.SendRequest[proxy.Object](m.h.conn, closeCtx, "session/close", map[string]string{"sessionId": m.h.id})
		cancel()
	}
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
	v.question = m.ask
	v.hasAgents = len(m.agents.children) > 0 && m.ask == nil
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
	v.activeModel = m.agentSettings.byCategory("model").current()
	if v.Status == "Ready" {
		v.Status = settingLabel(v.Status, m.agentSettings.byCategory("mode").currentLabel())
		if m.h != nil && m.h.record.Parent != "" {
			v.Status += " · Fork"
		}
	}
	view := m.root.View(v.Width, v.Height, m.scene(v))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}
func (m *terminal) submit() tea.Cmd {
	text := strings.TrimSpace(m.editor.Value())
	if m.ask != nil {
		if m.ask.form != nil {
			var fields map[string]any
			if err := json.Unmarshal([]byte(text), &fields); err != nil || fields == nil {
				m.Status = "Enter a JSON object"
				return nil
			}
			if err := m.ask.form.Validate(fields); err != nil {
				m.Status = terminalError(err)
				return nil
			}
			m.ask.answer <- fields
		} else {
			i, err := strconv.Atoi(text)
			if err != nil || i < 1 || i > len(m.ask.options) {
				m.Status = "Enter an option number"
				return nil
			}
			m.recordNote("Selected · " + m.ask.options[i-1].Name)
			m.ask.reply <- i - 1
		}
		m.ask = nil
		m.restorePermissionDraft()
		return m.deliver()
	}
	if text == "" || m.connection != connected || m.forking {
		return nil
	}
	input := text
	if escaped, ok := strings.CutPrefix(text, "/agent:"); ok {
		name, _, _ := strings.Cut(escaped, " ")
		for _, command := range m.agentCommands {
			if command.Name == name {
				input = "/" + escaped
				break
			}
		}
	}
	message := promptMessage{text: input, content: []map[string]any{{"type": "text", "text": input}}}
	if path, ok := strings.CutPrefix(text, "/attach "); ok && m.h != nil {
		loaded, err := attachment(m.Cwd, strings.TrimSpace(path), m.h.promptCaps)
		if err != nil {
			m.recordNote(terminalError(err))
			return nil
		}
		message = loaded
	}
	m.editor.Reset()
	if m.h.record.ID != "" && m.h.record.Title == "" {
		title := []rune(strings.Join(strings.Fields(clean(message.text)), " "))
		m.h.record.Title = string(title[:min(len(title), 80)])
		if err := sessions.Save(m.h.cwd, m.h.record); err != nil {
			m.recordNote("Could not save conversation title: " + err.Error())
		}
	}
	m.outbox = append(m.outbox, message)
	return m.deliver()
}

func (m *terminal) restorePermissionDraft() {
	if m.connection == connected {
		m.Status = "Ready"
		if m.Busy {
			m.Status = "Working"
		}
	}
	m.editor, m.permissionDraft = m.permissionDraft, nil
}

func (m *terminal) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "ctrl+n" {
		if m.ask == nil && !m.Busy && !m.forking && !m.changingSetting && m.connection != connecting {
			c := m.cfg
			c.Model = c.DefaultModel()
			if c.Model != "" {
				m.next = &sessionStart{cfg: c}
				return m, tea.Quit
			}
		}
		return m, nil
	}
	if m.launch != nil {
		switch t := msg.(type) {
		case tea.WindowSizeMsg:
			m.Width = t.Width
			m.Height = t.Height
		case tea.KeyPressMsg:
			if t.String() == "ctrl+c" {
				return m, tea.Quit
			}
			if t.String() == "esc" && !m.launch.fromCenter && (!m.launch.history || (!m.launch.center.finding && m.launch.center.graphID == "" && !m.launch.center.inspect && m.launch.search.Value() == "")) && (!m.launch.choosingRoute || m.launch.routeOnly) {
				m.launch = nil
				return m, nil
			}
		}
		switch msg.(type) {
		case tea.MouseClickMsg, tea.MouseWheelMsg, tea.WindowSizeMsg, tea.KeyPressMsg, tea.PasteMsg, routeBalances, remoteSessions:
			_, cmd := m.launch.Update(msg)
			if m.launch == nil {
				return m, cmd
			}
			if m.launch.chosen {
				if m.launch.setting != nil {
					return m, m.selectSetting(m.launch)
				}
				m.next = &sessionStart{cfg: m.launch.cfg, resume: m.launch.resume, id: m.launch.id}
				if m.connection == connectionFailed {
					m.next.editor = m.editor
				}
				return m, tea.Quit
			}
			return m, cmd
		}
	}
	ui := m.root.Update(msg)
	var cmd tea.Cmd
	switch t := msg.(type) {
	case animationFrame:
		m.animating = false
		m.Frame++
	case tea.WindowSizeMsg:
		m.Width = t.Width
		m.Height = t.Height
	case terminalSignIn:
		cmd = tea.Batch(m.listen, tea.ExecProcess(t.cmd, func(err error) tea.Msg { t.done <- err; return nil }))
	case dismissedQuestion:
		if m.ask == t.q {
			m.ask = nil
			m.restorePermissionDraft()
		}
		cmd = m.listen
	case settingsUpdate:
		if t.CurrentModeID != "" {
			m.agentSettings.CurrentModeID = t.CurrentModeID
		}
		if t.Config != nil {
			m.agentSettings.Config = t.Config
		}
		cmd = m.listen
	case forkResult:
		m.forking = false
		m.Status = "Ready"
		if t.err != nil {
			m.recordNote(terminalError(t.err))
			break
		}
		m.next = &sessionStart{cfg: m.cfg, id: t.id, resume: true}
		return m, tea.Quit
	case childOutput:
		for i := range m.agents.children {
			c := &m.agents.children[i]
			if c.id == t.id {
				c.pending.push(t.event)
				break
			}
		}
		cmd = m.listen
	case childAnnouncement:
		m.agents.update(t.parent, t.update)
		cmd = m.listen
	case acp.SessionMessage:
		m.agents.updateMessage(t)
		if !m.Busy && m.connection == connected {
			m.agents.publish()
		}
		cmd = m.listen
	case acp.SessionMessageChunk:
		m.agents.message(t.MessageId).append(t.Content)
		cmd = m.listen
	case endedSession:
		if t.err != nil {
			m.recordNote(terminalError(t.err))
		}
		if t.ended {
			m.h.canClose = false
			return m, tea.Quit
		}
	case settingResult:
		m.changingSetting = false
		m.Status = "Ready"
		if t.options.CurrentModeID != "" {
			m.agentSettings.CurrentModeID = t.options.CurrentModeID
		}
		if t.err != nil {
			m.recordNote(terminalError(t.err))
		}
		if t.options.Config != nil {
			m.agentSettings.Config = t.options.Config
		}
		if t.err == nil && m.h.record.ID != "" {
			m.h.rememberSetting(t.selection)
			if err := sessions.Save(m.h.cwd, m.h.record); err != nil {
				m.recordNote("Could not save session settings: " + err.Error())
			}
		}
		cmd = m.deliver()
	case opened:
		m.agents.publish()
		if t.err == signedIn {
			m.next = &sessionStart{cfg: m.cfg, resume: m.resume, id: m.resumeID, editor: m.editor}
			return m, tea.Quit
		}
		clear(m.tools)
		cmd = m.listen
		if t.err != nil {
			m.pending = turnLog{}
			m.connection = connectionFailed
			m.Messages = append(m.Messages, entry{"system", terminalError(t.err)})
		} else {
			m.Messages = m.pending.commit(m.Messages[:m.turnStart])
			m.connection = connected
			if m.h != nil {
				m.agentSettings = m.h.initialSettings
				m.saveSessionInfo()
			}
			m.Account = t.account
			m.Status = "Ready"
		}
	case steerResult:
		m.steering = false
		text := m.outbox[0].text
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
		m.agents.publish()
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
			m.ask.cancel()
			m.ask = nil
			m.restorePermissionDraft()
		}
		if t.err != nil {
			m.Messages = append(m.Messages, entry{"system", terminalError(t.err)})
		}
		cmd = tea.Batch(cmd, m.deliver())
	case acp.SessionUsageUpdate:
		m.usage = &t
		cmd = m.listen
	case acp.SessionCurrentModeUpdate:
		m.agentSettings.CurrentModeID = string(t.CurrentModeId)
		cmd = m.listen
	case acp.SessionSessionInfoUpdate:
		if !t.Title.IsUnset() {
			m.pendingInfo.Title = t.Title
		}
		if !t.UpdatedAt.IsUnset() {
			m.pendingInfo.UpdatedAt = t.UpdatedAt
		}
		if m.connection == connected {
			m.saveSessionInfo()
		}
		cmd = m.listen
	case acp.SessionUpdateNotice:
		m.notice = &t
		cmd = m.listen
	case acp.SessionCompactionUpdate:
		m.compaction(string(t.CompactionId)).update(t)
		cmd = m.listen
	case acp.SessionUpdateCompactionSummaryChunk:
		c := m.compaction(string(t.CompactionId))
		c.append(t.Content)
		cmd = m.listen
	case agentCommandUpdate:
		m.agentCommands = t
		m.commandIndex = 0
		cmd = m.listen
	case cancelFailed:
		m.cancelling = false
		m.Status = "Cancellation failed"
		m.recordNote(terminalError(t.err))
	case planUpdate:
		switch {
		case t.legacy:
			m.legacyPlan = &entry{"system", "**Plan**\n" + t.text}
		case t.remove:
			delete(m.plans, t.id)
		default:
			if m.plans == nil {
				m.plans = make(map[string]string)
			}
			m.plans[t.id] = t.text
		}
		clear(m.planRows)
		m.planRows = m.planRows[:0]
		if m.legacyPlan != nil {
			m.planRows = append(m.planRows, *m.legacyPlan)
		}
		for _, id := range slices.Sorted(maps.Keys(m.plans)) {
			m.planRows = append(m.planRows, entry{"system", "**Plan**\n" + m.plans[id]})
		}
		cmd = m.listen
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
			m.launch = nil
			m.chatBacklog.browsing = false
			m.reading = false
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
		if m.reading {
			target = &m.readerBacklog
		}
		switch t.Button {
		case tea.MouseWheelUp:
			target.move(-3)
		case tea.MouseWheelDown:
			target.move(3)
		}
	case tea.PasteMsg:
		if !m.reading && m.connection != connectionFailed {
			cmd = m.paste(t.Content)
		}
	case tea.KeyPressMsg:
		cmd = m.handleKey(t)
	}
	if !m.animating && m.Busy {
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
	if m.connection != connected || m.Busy {
		m.pending.push(event{role: "system", text: text})
	}
	m.Messages = append(m.Messages, entry{"system", text})
}
