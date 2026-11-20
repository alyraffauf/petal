package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func sentenceLabel(s string) string {
	if s == "" {
		return ""
	}
	first, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(first)) + s[n:]
}

func shortcut(keys []string, label, description string) key.Binding {
	return key.NewBinding(key.WithKeys(keys...), key.WithHelp(label, sentenceLabel(description)))
}

var (
	inspectAgents      = shortcut([]string{"f4"}, "F4", "agents")
	resumeConversation = shortcut([]string{"f2"}, "F2", "Threads")
	chooseRoute        = shortcut([]string{"tab"}, "Tab", "subscription")
	confirmChoice      = shortcut([]string{"enter"}, "Enter", "confirm")
	closeReader        = shortcut([]string{"esc", "f3"}, "Esc", "close")
	showCommands       = shortcut([]string{"/"}, "/", "commands")
	newThread          = shortcut([]string{"ctrl+n"}, "^N", "new")
	readHistory        = shortcut([]string{"f3"}, "F3", "read")
	pageEarlier        = shortcut([]string{"pgup"}, "Pg↑", "older")
	pageLater          = shortcut([]string{"pgdown"}, "Pg↓", "newer")
	jumpLatest         = shortcut([]string{"ctrl+end"}, "^End", "latest")
	quitPetal          = shortcut([]string{"ctrl+c"}, "^C", "quit")
	declineRequest     = shortcut([]string{"esc"}, "Esc", "decline")
	cancelTurn         = shortcut([]string{"esc"}, "Esc", "cancel")
	selectPrevious     = shortcut([]string{"up"}, "↑", "previous")
	selectNext         = shortcut([]string{"down"}, "↓", "next")
	runSelected        = shortcut([]string{"enter"}, "Enter", "run")
	dismissMenu        = shortcut([]string{"esc"}, "Esc", "dismiss")
)

func shortHelp(width int, bindings ...key.Binding) string {
	h := help.New()
	h.Styles = help.Styles{ShortKey: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(roseHex))}
	h.ShortSeparator = "  "
	h.SetWidth(max(1, width-2))
	return h.ShortHelpView(bindings)
}

func chatHelp(v screen) string {
	switch {
	case v.question != nil:
		return shortHelp(v.Width, confirmChoice, declineRequest)
	case v.backlog != nil && v.backlog.browsing && len(v.commands) == 0:
		if v.Busy {
			return shortHelp(v.Width, cancelTurn, jumpLatest, pageEarlier, pageLater)
		}
		return shortHelp(v.Width, jumpLatest, pageEarlier, pageLater)
	case v.connection == connectionFailed:
		return shortHelp(v.Width, newThread, quitPetal)
	case len(v.commands) > 0:
		return shortHelp(v.Width, selectPrevious, selectNext, runSelected, dismissMenu)
	case v.Busy && v.hasAgents:
		return shortHelp(v.Width, cancelTurn, inspectAgents, readHistory, pageEarlier, pageLater)
	case v.hasAgents:
		return shortHelp(v.Width, inspectAgents, readHistory, pageEarlier, pageLater)
	case v.Busy:
		return shortHelp(v.Width, cancelTurn, readHistory, pageEarlier, pageLater)
	default:
		return shortHelp(v.Width, showCommands, resumeConversation, newThread, readHistory, pageEarlier, pageLater)
	}
}

func styledHint(hint, background string) string {
	var parts []string
	for _, item := range strings.Split(strings.TrimSpace(hint), "  ") {
		key, label, _ := strings.Cut(item, " ")
		parts = append(parts, richBand(key, ansi.StringWidth(key), petalRose, background, true)+" "+label)
	}
	return strings.Join(parts, "  ")
}

func navFooterHeight(h int) int {
	if h >= 12 {
		return 2
	}
	return 1
}

func appendNavFooter(rows []string, w, h int, hint, background string) []string {
	if len(rows) >= h {
		return rows[:h]
	}
	if navFooterHeight(h) == 2 && len(rows) < h-1 {
		rows = append(rows, richBand(strings.Repeat("─", w), w, selectionBand, background, false))
	}
	return append(rows, richBand(" "+hint, w, petalText, background, false))
}
