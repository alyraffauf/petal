package acphost

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	acp "github.com/coder/acp-go-sdk"
)

// CleanText removes terminal escape sequences and non-displayable controls.
func CleanText(s string) string {
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

// FormatTerminalOutput renders the captured terminal tail for a tool transcript.
func FormatTerminalOutput(out acp.TerminalOutputResponse) string {
	text := ""
	if out.Truncated {
		text = "\n\nEarlier output was truncated."
	}
	if out.Output != "" {
		text += "\n\n    " + strings.ReplaceAll(strings.TrimSuffix(CleanText(out.Output), "\n"), "\n", "\n    ")
	}
	return text
}
