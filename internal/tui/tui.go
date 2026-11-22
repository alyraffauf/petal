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
	"unicode/utf8"

	"bloomfx.dev/bloom"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
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
	transcriptRevision          uint64
	images                      map[string]inlinePreview
	question                    *permission
	background                  string
	hasAgents                   bool
	compactions                 map[string]*compaction
	usage                       *acp.SessionUsageUpdate
	notice                      *acp.SessionUpdateNotice
	planRows                    []entry
	connection                  connectionState
	busyAction                  chatAction
	activeModel                 string
	queued                      int
	backlog                     *backlogViewport
	markdown                    *markdownCache
	Width, Height, Frame        int
	editor                      *composer
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
	urls                  []*urlRequest
	showURL               bool
	urlOffset             int
	openingPreview        []entry
	streamSource          *turnBlock
	streamPreview         string
	inputTTY              *os.File
	clipboard             clipboardRead
	limitedColor          bool
	imageBase             int
	monochrome            bool
	agents                agentBrowser
	forking               bool
	pendingInfo           acp.SessionSessionInfoUpdate
	agentCommands         agentCommandUpdate
	agentSettings         settingsUpdate
	changingSetting       bool
	outbox                []promptMessage
	steering, waitForTurn bool
	lateFinish            *finished
	permissionDraft       *composer
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
	draftSessionID     string
	resume, cancelling bool
	ask                *permission
	chatBacklog        backlogViewport
	chatHits           chatHitTargets
	pending            turnLog
	v2                 *conversationV2
	turnStart          int
}

type sessionStart struct {
	id     string
	cfg    routing.Config
	resume bool
	editor *composer
}

func Run(parent context.Context, c routing.Config, resume bool) error {
	c.ResolveDefaultLabels(parent)
	start, ok, err := pick(c, resume)
	if err != nil || !ok {
		return err
	}
	drafts := make(sessionDrafts)
	for {
		next, err := runSession(parent, start, drafts)
		if err != nil || next == nil {
			return err
		}
		start = *next
	}
}
func runSession(parent context.Context, start sessionStart, drafts sessionDrafts) (*sessionStart, error) {
	c := start.cfg
	ctx, stop := context.WithCancel(parent)
	defer stop()
	m := &terminal{root: bloom.New(), ctx: ctx, cfg: c, resume: start.resume, resumeID: start.id}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	m.screen = screen{transcriptRevision: 1, background: canvasBackground(c.TransparentBackground), Cwd: cwd, Model: c.Model, Account: c.PinnedAccount, connection: connecting}
	m.h = startHarness(ctx, c)
	m.h.id = start.id
	defer m.h.close()
	m.draftSessionID = start.id
	m.editor = drafts.take(cwd, start)
	m.markdown = &markdownCache{}
	if directGraphicsTerminal() {
		defer os.Stdout.WriteString("\x1b[?5522l")
	}
	err = runChat(m)
	if m.v2 != nil && m.connection == connected {
		m.publishV2Transcript(true)
	}
	if m.next != nil {
		drafts.remember(m)
	}
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

type outputBatch struct {
	events []event
	next   tea.Msg
}

func (e event) batchable() bool { return e.ask == nil && e.picture == nil }

func (m *terminal) listen() tea.Msg {
	var disconnected <-chan struct{}
	if m.h.conn != nil {
		disconnected = m.h.conn.Done()
	}
	var e tea.Msg
	select {
	case e = <-m.h.events:
	default:
		select {
		case e = <-m.h.events:
		case <-disconnected:
			if m.h.v2.Load() {
				return disconnectedV2{}
			}
			return nil
		case <-m.ctx.Done():
			return nil
		}
	}
	return m.batchOutput(e)
}

func (m *terminal) batchOutput(e tea.Msg) tea.Msg {
	first, ok := e.(event)
	if !ok || !first.batchable() {
		return e
	}
	// Drain only already queued output. A permission or lifecycle notification
	// after it must be processed before reading any more notifications.
	batch := outputBatch{events: []event{first}}
	for len(batch.events) < 128 {
		select {
		case next := <-m.h.events:
			if chunk, ok := next.(event); ok && chunk.batchable() {
				batch.events = append(batch.events, chunk)
			} else {
				batch.next = next
				return batch
			}
		default:
			return batch
		}
	}
	return batch
}

type cachedPreview []entry

func (m *terminal) openingTranscript() tea.Cmd {
	if !m.resume || m.resumeID == "" {
		return nil
	}
	ctx, cwd, id := m.ctx, m.Cwd, m.resumeID
	return func() tea.Msg {
		rows, err := sessions.RecentTranscript(ctx, cwd, id, 64)
		if err != nil {
			// The provider replay is authoritative if the local index is unavailable.
			return nil
		}
		entries := make(cachedPreview, len(rows))
		for i, row := range rows {
			entries[i] = entry{row.Role, row.Body}
		}
		return entries
	}
}

func (m *terminal) Init() tea.Cmd {
	var pasteMode tea.Cmd
	if directGraphicsTerminal() {
		pasteMode = tea.Raw("\x1b[?5522h")
	}
	return tea.Batch(m.openingTranscript(), pasteMode, m.root.Init(), m.listen, func() tea.Msg {
		a, e := m.h.open(m.ctx, m.cfg, m.Cwd, m.resume)
		select {
		case m.h.events <- opened{a, e}:
		case <-m.ctx.Done():
		}
		return nil
	})
}
func (m *terminal) View() tea.View {
	m.chatHits = chatHitTargets{}
	if m.launch != nil {
		return m.launch.View()
	}
	if m.showURL && len(m.urls) > 0 && m.ask == nil {
		return m.urlView()
	}
	v := m.screen
	if len(m.urls) > 0 && m.ask == nil {
		v.notice = &acp.SessionUpdateNotice{Severity: "info", Title: "External sign-in requested · F6 Review"}
	}
	if m.connection == connecting && m.ask == nil && len(m.openingPreview) > 0 {
		v.Messages = m.openingPreview
	}
	if m.Busy && m.streamPreview != "" {
		v.planRows = append(slices.Clip(v.planRows), entry{"assistant", m.streamPreview})
	}
	v.question = m.ask
	v.hasAgents = len(m.agents.children) > 0 && m.ask == nil
	if m.Busy && m.connection == connected && m.h != nil {
		v.busyAction = m.currentSubmitAction()
		v.queued = len(m.outbox)
	}
	v.commands = m.commandMatches()
	v.commandIndex = min(m.commandIndex, max(0, len(v.commands)-1))
	v.backlog = &m.chatBacklog
	v.Cwd = clean(v.Cwd)
	v.Account = clean(v.Account)
	v.Model = clean(v.Model)
	v.activeModel = m.agentSettings.byCategory("model").current()
	if v.Status == "ready" {
		v.Status = settingLabel(v.Status, m.agentSettings.byCategory("mode").currentLabel())
		if m.h != nil && m.h.record.Parent != "" {
			v.Status += " · fork"
		}
	}
	var view tea.View
	if m.reading {
		view = tea.NewView(m.renderReader(v))
	} else {
		// Chat already produces the complete styled terminal frame.
		view = tea.NewView(renderChat(v, &m.chatHits))
		view.Cursor = m.chatHits.cursor
	}
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
				m.Status = "enter a JSON object"
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
				m.Status = "enter an option number"
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
	if m.clipboard.id != "" && m.clipboard.editor == m.editor {
		m.recordNote("Waiting for clipboard data. Press Enter again after the paste finishes.")
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
	if m.editor.image != nil && strings.Contains(input, imageMarker) {
		message = m.editor.imageMessage(input)
	} else if path, ok := strings.CutPrefix(text, "/attach "); ok && m.h != nil {
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
		if err := m.h.updateRecord(func(record *sessions.Record) bool {
			if record.Title != "" || len(title) == 0 {
				return false
			}
			record.Title = string(title[:min(len(title), 80)])
			return true
		}); err != nil {
			m.recordNote("Could not save conversation title: " + err.Error())
		}
	}
	var upload tea.Cmd
	if m.h.graphics && !m.monochrome {
		for _, block := range message.content {
			if block["type"] != "image" {
				continue
			}
			data, _ := block["data"].(string)
			message.text, upload = m.registerPreview(message.text, decodePreview(data))
			break
		}
	}
	m.outbox = append(m.outbox, message)
	return tea.Batch(upload, m.deliver())
}

func (m *terminal) restorePermissionDraft() {
	if m.v2 != nil && m.connection == connected {
		m.refreshForegroundV2()
	} else if m.connection == connected {
		m.Status = "ready"
		if m.Busy {
			m.Status = "working"
		}
	}
	m.editor, m.permissionDraft = m.permissionDraft, nil
}

func (m *terminal) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if launch := m.launch; launch != nil && launch.center.childStatuses != nil {
		defer func() {
			if launch != m.launch {
				launch.stopWatchingChildStatuses()
			}
		}()
	}
	if batch, ok := msg.(outputBatch); ok {
		if m.launch != nil {
			m.launch.center.cache = centerCache{}
		}
		visibleBefore := len(m.Messages)
		for _, e := range batch.events {
			m.recordOutput(e)
		}
		if m.v2 != nil {
			m.publishV2Transcript(false)
		}
		if len(m.Messages) != visibleBefore {
			m.transcriptRevision++
		}
		if batch.next == nil {
			return m, m.listen
		}
		msg = batch.next
	}
	// Between navigation inputs both the transcript and its wrapped row counts
	// are unchanged. Other updates can replace plans, compactions or past turns.
	switch key := msg.(type) {
	case tea.MouseWheelMsg, tea.KeyReleaseMsg, animationFrame:
	case tea.KeyPressMsg:
		switch key.String() {
		case "pgup", "pgdown", "ctrl+end", "up", "down":
		default:
			m.transcriptRevision++
		}
	default:
		m.transcriptRevision++
	}
	if m.showURL && m.ask == nil {
		switch msg.(type) {
		case tea.PasteMsg, tea.MouseClickMsg, tea.MouseWheelMsg:
			return m, nil
		}
	}
	if handled, cmd := m.clipboardEvent(msg); handled {
		return m, cmd
	}
	if profile, ok := msg.(tea.ColorProfileMsg); ok {
		m.monochrome = profile.Profile <= colorprofile.ASCII
		m.limitedColor = profile.Profile < colorprofile.TrueColor
		if m.launch != nil {
			m.launch.monochrome = m.monochrome
		}
	}
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
			if t.String() == "esc" && m.launch.transcripts == nil && !m.launch.fromCenter && (!m.launch.history || (!m.launch.center.finding && m.launch.center.graphID == "" && !m.launch.center.inspect && m.launch.search.Value() == "")) && (!m.launch.choosingRoute || m.launch.routeOnly) {
				m.launch = nil
				return m, nil
			}
		}
		switch msg.(type) {
		case tea.MouseClickMsg, tea.MouseWheelMsg, tea.WindowSizeMsg, tea.KeyPressMsg, tea.PasteMsg, routeBalances, remoteSessions, transcriptResults, childCancellationResult, childStatusesChanged, centerFrame:
			_, cmd := m.launch.Update(msg)
			if m.launch == nil {
				return m, cmd
			}
			if m.launch.chosen {
				if m.launch.setting != nil {
					return m, m.selectSetting(m.launch)
				}
				m.next = &sessionStart{cfg: m.launch.cfg, resume: m.launch.resume, id: m.launch.id}
				if m.connection == connectionFailed && m.launch.routeOnly && m.next.resume == m.resume && m.next.id == m.resumeID {
					m.next.editor = m.editor
				}
				return m, tea.Quit
			}
			return m, cmd
		}
		// Session events can change permissions, ownership and conversation turns.
		// Picker navigation and animation reuse the current tree.
		switch msg.(type) {
		case animationFrame, tea.KeyReleaseMsg:
		default:
			m.launch.center.cache = centerCache{}
		}
	}
	ui := m.root.Update(msg)
	var cmd tea.Cmd
	switch t := msg.(type) {
	case animationFrame:
		m.animating = false
		m.Frame++
		if b := m.streamSource; b != nil && m.Busy {
			text := b.text.String()
			// Bound Markdown layout work per frame even for very long replies.
			if len(text) > 4096 {
				text = text[len(text)-4096:]
				for len(text) > 0 && !utf8.RuneStart(text[0]) {
					text = text[1:]
				}
				text = "…" + text
			}
			m.streamPreview = text
			m.streamSource = nil
			m.transcriptRevision++
		}
	case tea.WindowSizeMsg:
		m.Width = t.Width
		m.Height = t.Height
		m.chatHits = chatHitTargets{}
	case terminalSignIn:
		t.cmd.Stdin = m.inputTTY // The child needs the terminal fd for interactive sign-in.
		cmd = tea.Batch(m.listen, tea.ExecProcess(t.cmd, func(err error) tea.Msg { t.done <- err; return nil }))
	case *urlRequest:
		m.urls = append(m.urls, t)
		cmd = m.listen
	case urlCompleted:
		if i := slices.IndexFunc(m.urls, func(q *urlRequest) bool { return q.id == string(t) }); i >= 0 {
			m.removeURL(i, "accept")
		}
		cmd = m.listen
	case urlCancelled:
		if i := slices.Index(m.urls, t.request); i >= 0 {
			m.removeURL(i, "")
		}
		cmd = m.listen
	case dismissedQuestion:
		if m.ask == t.q {
			m.ask = nil
			m.restorePermissionDraft()
		}
		cmd = m.listen
		if m.v2 != nil {
			cmd = tea.Batch(cmd, m.deliver())
		}
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
		m.Status = "ready"
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
		if m.connection == connected && !m.Busy {
			m.indexMessages(m.agents.completedEntries(nil), false)
		}
		cmd = m.listen
	case acp.SessionMessage:
		m.agents.updateMessage(t)
		if !m.Busy && m.connection == connected {
			m.agents.publish()
			m.indexMessages(m.agents.completedEntries(nil), false)
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
		m.Status = "ready"
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
			if err := m.h.rememberSetting(t.selection); err != nil {
				m.recordNote("Could not save session settings: " + err.Error())
			}
		}
		cmd = m.deliver()
	case cachedPreview:
		if m.connection == connecting {
			m.openingPreview = t
		}
	case opened:
		m.openingPreview = nil
		m.agents.publish()
		if m.h != nil && (m.resume || t.err == nil) {
			m.draftSessionID = m.h.id
			if m.resume {
				m.resumeID = m.h.id
			}
		}
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
			m.connection = connected
			if m.h != nil && m.h.v2.Load() {
				m.refreshForegroundV2()
				m.publishV2Transcript(m.h.replayed)
				if m.agentCommands == nil {
					m.agentCommands = m.h.initialCommands
				}
				m.agentSettings = m.h.initialSettings
				m.saveSessionInfo()
				m.Account = t.account
				break
			}
			m.Messages = m.pending.commit(m.Messages[:m.turnStart])
			if m.h != nil {
				replay := m.agents.completedEntries(slices.Clip(m.Messages[m.turnStart:]))
				m.indexMessages(replay, m.h.replayed && len(m.Messages) > m.turnStart)
				if len(m.h.resumedHistory) > 0 {
					m.Messages = append(m.h.resumedHistory, m.Messages...)
					m.h.resumedHistory = nil
				}
				m.agentSettings = m.h.initialSettings
				m.saveSessionInfo()
			}
			m.Account = t.account
			m.Status = "ready"
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
	case updateV2:
		cmd = m.listen
		conversation := m.conversationV2()
		if t.SessionUpdate == "state_update" {
			completed := conversation.foreground.report(t.State)
			if t.State == "idle" && !conversation.foreground.awaitingStart {
				m.cancelling = false
			}
			if completed && m.connection == connected && t.StopReason != "" && t.StopReason != "end_turn" {
				m.recordNote("Turn stopped · " + strings.ReplaceAll(t.StopReason, "_", " "))
			}
			m.refreshForegroundV2()
			m.publishV2Transcript(false)
			if m.connection == connected {
				cmd = tea.Batch(cmd, m.deliver())
			}
		}
	case promptV2Result:
		foreground := &m.conversationV2().foreground
		m.v2.promptWritten = nil
		foreground.settlePrompt(t.err)
		if t.err != nil {
			if foreground.state == "" || foreground.state == "idle" {
				m.cancelling = false
			}
			if !promptCancelledV2(t.err) {
				m.recordNote(terminalError(t.err))
			}
		}
		m.refreshForegroundV2()
		m.publishV2Transcript(false)
		cmd = tea.Batch(m.listen, m.deliver())
	case cancelV2Result:
		if m.connection != connected {
			break
		}
		m.v2.cancelSending = false
		if t.err != nil {
			m.cancelling = false
			m.recordNote(terminalError(t.err))
		}
		m.refreshForegroundV2()
		cmd = m.deliver()
	case disconnectedV2:
		m.publishV2Transcript(true)
		m.connection = connectionFailed
		m.Busy, m.cancelling = false, false
		m.Status = "disconnected"
		if m.ask != nil {
			m.ask = nil
			m.restorePermissionDraft()
		}
		m.recordNote("Agent connection closed.")
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
		m.streamSource = nil
		m.streamPreview = ""
		m.Status = "ready"
		if t.err == nil && (t.reason == "end_turn" || t.reason == "" || t.reason == "max_tokens") {
			m.Messages = m.pending.commit(m.Messages[:m.turnStart])
		} else {
			m.pending = turnLog{}
		}
		if t.err == nil && t.reason != "" && t.reason != "end_turn" {
			m.Messages = append(m.Messages, entry{"system", "Turn stopped · " + strings.ReplaceAll(t.reason, "_", " ")})
		}
		if m.ask != nil {
			m.ask.cancel()
			m.ask = nil
			m.restorePermissionDraft()
		}
		if t.err != nil {
			m.Messages = append(m.Messages, entry{"system", terminalError(t.err)})
		}
		m.indexMessages(m.agents.completedEntries(slices.Clip(m.Messages[m.turnStart:])), false)
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
		id := string(t.CompactionId)
		m.compaction(id).update(t)
		m.indexIdleCompaction(id)
		cmd = m.listen
	case acp.SessionUpdateCompactionSummaryChunk:
		id := string(t.CompactionId)
		m.compaction(id).append(t.Content)
		m.indexIdleCompaction(id)
		cmd = m.listen
	case agentCommandUpdate:
		m.agentCommands = t
		m.commandIndex = 0
		cmd = m.listen
	case cancelFailed:
		m.cancelling = false
		m.Status = "cancellation failed"
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
		if t.ask != nil {
			if t.ask.v2Update != nil {
				m.recordV2Update(t.ask.v2Update)
				m.publishV2Transcript(false)
			}
			m.launch = nil
			m.chatBacklog.browsing = false
			m.reading = false
			m.permissionDraft, m.editor = m.editor, newComposer()
			m.ask = t.ask
			m.Status = "permission required"
		}
		var upload tea.Cmd
		if t.picture != nil {
			t.text, upload = m.registerPreview(t.text, t.picture)
		}
		m.recordOutput(t)
		cmd = tea.Batch(upload, m.listen)
	case tea.MouseClickMsg:
		cmd = m.handleChatClick(t)
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
	if (m.h == nil || !m.h.v2.Load()) && (m.connection != connected || m.Busy) {
		m.pending.push(event{role: "system", text: text})
	}
	m.Messages = append(m.Messages, entry{"system", text})
}

func (m *terminal) recordOutput(e event) {
	if e.v2Update != nil {
		m.recordV2Update(e.v2Update)
		return
	}
	if m.h != nil && m.h.v2.Load() {
		if e.text != "" {
			m.Messages = append(m.Messages, entry{e.role, e.text})
		}
		return
	}
	if e.tool != nil {
		var done bool
		e, done = m.toolResult(*e.tool)
		if !done {
			return
		}
	}
	m.pending.push(e)
	if m.cfg.StreamResponses && m.Busy && m.connection == connected && e.chunk && e.role == "assistant" {
		m.streamSource = m.pending.last
	}
	if !e.chunk {
		m.Messages = append(m.Messages, entry{e.role, e.text})
	}
}
