package proxy

import (
	"encoding/json"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/acphost"
)

type terminalReference struct {
	id   string
	view func() acp.TerminalOutputResponse
}

// Capture the view when the tool first references a terminal. The adapter can
// release its handle before the prompt finishes and the archive is written.
func (s *recordedSession) trackToolTerminals(host *acphost.Host, update Object) {
	var blocks []struct{ Type, TerminalID string }
	if json.Unmarshal(update["content"], &blocks) != nil || blocks == nil {
		return
	}
	tool := stringField(update, "toolCallId")
	old := s.toolTerminals[tool]
	delete(s.toolTerminals, tool)
	for _, block := range blocks {
		if block.Type != "terminal" {
			continue
		}
		view := host.OutputView(s.id, block.TerminalID)
		if view == nil {
			for _, ref := range old {
				if ref.id == block.TerminalID {
					view = ref.view
					break
				}
			}
		}
		if view != nil {
			if s.toolTerminals == nil {
				s.toolTerminals = make(map[string][]terminalReference)
			}
			s.toolTerminals[tool] = append(s.toolTerminals[tool], terminalReference{block.TerminalID, view})
		}
	}
}
