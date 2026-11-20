package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	acp "github.com/coder/acp-go-sdk"
)

type agentCommandUpdate []acp.AvailableCommand

type chatCommand struct {
	name, description string
	agent, takesInput bool
}

func (m *terminal) commandMatches() []chatCommand {
	if m.ask != nil || m.editor == nil || m.connection == connectionFailed {
		return nil
	}
	query := m.editor.Value()
	if !strings.HasPrefix(query, showCommands.Keys()[0]) {
		return nil
	}
	var matches []chatCommand
	add := func(name, description string) {
		matches = append(matches, chatCommand{name: name, description: description})
	}
	if m.h != nil && m.connection == connected {
		add("/agents", "Find a subagent in the control center")
		add("/sessions", "Browse sessions, forks, agents, and turns")
		if m.h.promptCaps.Image {
			add("/paste-image", "Paste a clipboard image · Ctrl+V")
		}
		add("/attach", "Send a file from disk")
	}
	if m.h != nil && m.connection == connected && !m.Busy && !m.changingSetting && !m.forking {
		if slices.ContainsFunc(m.agentSettings.Config, setting.hasChoices) {
			add("/config", "Change an agent setting")
		}
		hasModeSetting := m.agentSettings.byCategory("mode").ID != ""
		if len(m.agentSettings.Modes) > 0 || hasModeSetting {
			add("/mode", "Change the agent mode for this conversation")
		}
		if hasModeSetting && (m.cfg.Accounts[m.Account].Provider == "codex" || strings.HasPrefix(m.cfg.Model, "codex/")) {
			add("/permissions", "Choose the agent approval and sandbox preset")
		}
		if m.agentSettings.byCategory("model").ID != "" {
			add("/model", "Change the model for this conversation")
		} else {
			add("/model", "Start a thread with another model")
		}
		if m.agentSettings.byCategory("thought_level").ID != "" {
			add("/effort", "Change reasoning effort for this conversation")
		}
		add("/resume", "Resume a saved conversation")
		add("/subscription", "Start a thread on a chosen subscription")
		if m.h.canFork {
			add("/fork", "Branch from this conversation")
		}
		if m.h.record.Parent != "" {
			add("/parent", "Return to the conversation this fork came from")
		}
		add("/new", "Start a new thread")
		if m.h.canDelete {
			add("/delete", "Delete this conversation")
		}
		if m.h.canLogout {
			add("/logout", "Sign out of connected subscriptions")
		}
	}
	if m.usage != nil {
		add("/usage", "Show context usage and session cost")
	}
	if m.notice != nil {
		add("/dismiss", "Dismiss the current agent notice")
	}
	add("/search", "Search project transcripts")
	add("/history", "Read the conversation")
	add("/quit", "Exit Petal")
	for _, c := range m.agentCommands {
		name := "/" + c.Name
		if c.Name == "" || clean(c.Name) != c.Name || strings.ContainsAny(c.Name, " \n\t") {
			continue
		}
		if slices.ContainsFunc(matches, func(local chatCommand) bool { return local.name == name }) {
			name = "/agent:" + c.Name
		}
		description := "Agent · " + clean(c.Description)
		if c.Input != nil && c.Input.Unstructured != nil {
			description += " · " + clean(c.Input.Unstructured.Hint)
		}
		matches = append(matches, chatCommand{name: name, description: description, agent: true, takesInput: c.Input != nil})
	}
	return slices.DeleteFunc(matches, func(c chatCommand) bool { return !strings.HasPrefix(c.name, query) })
}

func (m *terminal) openPicker(routeOnly bool) tea.Cmd {
	c := m.cfg
	c.PinnedAccount = ""
	m.launch = newPicker(c)
	m.launch.monochrome = m.monochrome
	m.launch.embedded = true
	m.launch.width, m.launch.height = m.Width, m.Height
	m.launch.routeOnly = routeOnly
	if routeOnly {
		m.launch.resume = m.connection == connectionFailed && m.resume
		m.launch.id = m.resumeID
		return m.launch.routeMenu(c.Model)
	}
	return nil
}

func (m *terminal) runCommand(command chatCommand) tea.Cmd {
	m.editor.Reset()
	m.commandIndex = 0
	if command.agent {
		if command.takesInput {
			m.editor.SetValue(command.name + " ")
			m.editor.CursorEnd()
			return nil
		}
		m.editor.SetValue(command.name)
		return m.submit()
	}
	switch command.name {
	case "/paste-image":
		return m.readClipboardImage()
	case "/attach":
		m.editor.SetValue("/attach ")
		return nil
	case "/model":
		if m.agentSettings.byCategory("model").ID != "" {
			m.openSettings("model")
			return nil
		}
		return m.openPicker(false)
	case "/agents":
		return m.showAgents()
	case "/parent":
		m.next = &sessionStart{cfg: m.cfg, id: m.h.record.Parent, resume: true}
		return tea.Quit
	case "/fork":
		return m.fork()
	case "/new":
		return m.openPicker(false)
	case "/delete", "/logout":
		h, ctx, cwd, model := m.h, m.ctx, m.Cwd, m.cfg.Model
		return func() tea.Msg { return h.endSession(ctx, command.name, cwd, model) }
	case "/config":
		m.openSettings("")
	case "/mode", "/permissions":
		m.openSettings("mode")
	case "/effort":
		m.openSettings("thought_level")
	case "/resume", "/sessions":
		return m.browseSessions()
	case "/subscription":
		return m.openPicker(true)
	case "/dismiss":
		m.notice = nil
	case "/usage":
		m.recordNote(usageDetails(m.usage))
	case "/search":
		cmd := m.browseSessions()
		m.launch.openTranscriptSearch()
		return cmd
	case "/history":
		m.readerBacklog.browsing = false
		m.reading = true
		return nil
	case "/quit":
		return tea.Quit
	}
	return nil
}
