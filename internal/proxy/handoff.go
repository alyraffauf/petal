package proxy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"petal/internal/sessions"
	"petal/internal/sessions/sessiondb"
)

// handoffPrompt prepends the earlier conversation to the first prompt after a
// model switch. The new model gets what the old one still had in context: the
// latest compaction summary, if any, and every message after it.
func handoffPrompt(cwd, previous string, prompt json.RawMessage) (json.RawMessage, error) {
	rows, err := sessions.Transcript(cwd, previous)
	if err != nil {
		return nil, fmt.Errorf("read the conversation before the model switch: %w", err)
	}
	context := handoffText(rows)
	if context == "" {
		return prompt, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(prompt, &blocks); err != nil {
		return nil, fmt.Errorf("prompt must be an array: %w", err)
	}
	preamble, _ := json.Marshal(map[string]string{"type": "text", "text": context})
	// A prompt that starts with a command, such as "$skill" or "/compact", must
	// keep it first, so the context follows the command's block.
	at := 0
	if len(blocks) > 0 {
		var first struct{ Type, Text string }
		if json.Unmarshal(blocks[0], &first) == nil && first.Type == "text" {
			if text := strings.TrimSpace(first.Text); strings.HasPrefix(text, "$") || strings.HasPrefix(text, "/") {
				at = 1
			}
		}
	}
	return json.Marshal(slices.Insert(blocks, at, json.RawMessage(preamble)))
}

func handoffText(rows []sessiondb.ReadTranscriptRow) string {
	start := 0
	for i, row := range rows {
		if row.Role == "compaction" && completedCompaction(row.Body) {
			start = i
		}
	}
	var b strings.Builder
	for _, row := range rows[start:] {
		body := strings.TrimSpace(row.Body)
		if body == "" || row.Role == "compaction" && !completedCompaction(row.Body) {
			continue
		}
		label := map[string]string{
			"user":       "User",
			"assistant":  "Assistant",
			"tool":       "Tool call",
			"system":     "Notice",
			"compaction": "Summary of the earlier conversation",
		}[row.Role]
		if label == "" {
			label = row.Role
		}
		fmt.Fprintf(&b, "%s:\n%s\n\n", label, body)
	}
	if b.Len() == 0 {
		return ""
	}
	return "<conversation-so-far>\nThis conversation began with another model. It continues with you from here. Below is the conversation so far, oldest first. Answer the user's next message in that context.\n\n" +
		b.String() + "</conversation-so-far>"
}

// completedCompaction reports whether an archived compaction row holds a
// summary. The archive also keeps failed and cancelled attempts, which replace
// nothing, as a status line without a summary.
func completedCompaction(body string) bool {
	status, summary, _ := strings.Cut(strings.TrimSpace(body), "\n\n")
	return strings.Contains(status, "completed") && strings.TrimSpace(summary) != ""
}
