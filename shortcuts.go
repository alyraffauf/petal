package main

import (
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
)

func shortcut(keys []string, label, description string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(label, description))
}

var (
	chooseRoute    = shortcut([]string{"tab"}, "Tab", "subscription")
	confirmChoice  = shortcut([]string{"enter"}, "Enter", "confirm")
	closeReader    = shortcut([]string{"esc", "f3"}, "Esc", "close")
	showCommands   = shortcut([]string{"/"}, "/", "commands")
	newThread      = shortcut([]string{"ctrl+n"}, "^N", "new")
	readHistory    = shortcut([]string{"f3"}, "F3", "read")
	pageEarlier    = shortcut([]string{"pgup"}, "Pg↑", "older")
	pageLater      = shortcut([]string{"pgdown"}, "Pg↓", "newer")
	jumpLatest     = shortcut([]string{"ctrl+end"}, "^End", "latest")
	quitPetal      = shortcut([]string{"ctrl+c"}, "^C", "quit")
	cancelTurn     = shortcut([]string{"esc"}, "Esc", "cancel")
	selectPrevious = shortcut([]string{"up"}, "↑", "previous")
	selectNext     = shortcut([]string{"down"}, "↓", "next")
	runSelected    = shortcut([]string{"enter"}, "Enter", "run")
	dismissMenu    = shortcut([]string{"esc"}, "Esc", "dismiss")
)

func shortHelp(width int, bindings ...key.Binding) string {
	h := help.New()
	h.Styles = help.Styles{}
	h.ShortSeparator = "  "
	h.SetWidth(max(1, width-2))
	return h.ShortHelpView(bindings)
}

func chatHelp(v screen) string {
	switch {
	case v.backlog != nil && v.backlog.browsing:
		return shortHelp(v.Width, jumpLatest, pageEarlier, pageLater)
	case v.Status == "Connection failed":
		return shortHelp(v.Width, newThread, quitPetal)
	case len(v.commands) > 0:
		return shortHelp(v.Width, selectPrevious, selectNext, runSelected, dismissMenu)
	case v.Busy:
		return shortHelp(v.Width, cancelTurn, readHistory, pageEarlier, pageLater)
	default:
		return shortHelp(v.Width, showCommands, newThread, readHistory, pageEarlier, pageLater)
	}
}
