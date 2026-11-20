package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

type chatCommand struct{ name, description string }

func (m *terminal) commandMatches() []chatCommand {
	if m.ask != nil || m.editor == nil || m.Status == "Connection failed" {
		return nil
	}
	query := m.editor.Value()
	if !strings.HasPrefix(query, showCommands.Keys()[0]) {
		return nil
	}
	var matches []chatCommand
	add := func(name, description string) {
		if strings.HasPrefix(name, query) {
			matches = append(matches, chatCommand{name, description})
		}
	}
	if m.h != nil && m.ready && !m.Busy {
		add("/model", "Start a thread with another model")
		add("/subscription", "Start a thread on a chosen subscription")
		add("/new", "Start a new thread")
	}
	add("/history", "Read the conversation")
	add("/quit", "Exit Petal")
	return matches
}

func (m *terminal) openPicker(routeOnly bool) tea.Cmd {
	c := m.cfg
	c.forced = ""
	m.launch = &picker{cfg: c, models: c.modelNames(), width: m.Width, height: m.Height, routeOnly: routeOnly}
	if routeOnly {
		m.launch.resume = m.Status == "Connection failed" && m.resume
		return m.launch.routeMenu(c.model)
	}
	return nil
}

func (m *terminal) runCommand(command chatCommand) tea.Cmd {
	m.editor.Reset()
	m.commandIndex = 0
	switch command.name {
	case "/new", "/model":
		return m.openPicker(false)
	case "/subscription":
		return m.openPicker(true)
	case "/history":
		m.readerBacklog.browsing = false
		return m.reader.Show()
	case "/quit":
		return tea.Quit
	}
	return nil
}
