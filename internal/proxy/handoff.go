package proxy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/sessions"
	"petal/internal/sessions/sessiondb"
)

// handoffPrompt prepends the earlier conversation to the first prompt after a
// model switch. The new model gets what the old one still had in context: the
// latest compaction summary, if any, and every message after it. A fork's
// lineage supplies the conversation before the fork, ahead of the previous
// route's own archive.
func handoffPrompt(cwd string, lineage []historySegment, previous string, prompt json.RawMessage) (json.RawMessage, error) {
	// The parent may have been deleted since the switch. Fail before the
	// prompt consumes the handoff, so it stays pending.
	if err := lineageAvailable(cwd, lineage); err != nil {
		return nil, err
	}
	var rows []sessiondb.ReadTranscriptRow
	for _, segment := range lineage {
		inherited, err := sessions.Transcript(cwd, segment.ID)
		if err != nil {
			return nil, fmt.Errorf("read the conversation before the fork: %w", err)
		}
		for _, row := range inherited {
			if row.Rowid <= segment.Head {
				rows = append(rows, row)
			}
		}
	}
	own, err := sessions.Transcript(cwd, previous)
	if err != nil {
		return nil, fmt.Errorf("read the conversation before the model switch: %w", err)
	}
	// A fork loaded with a full replay archives the inherited messages under
	// its own ID too. Keep each inherited message once, ahead of the fork's turns.
	type message struct{ role, body string }
	inherited := map[message]int{}
	for _, row := range rows {
		inherited[message{row.Role, row.Body}]++
	}
	for _, row := range own {
		if key := (message{row.Role, row.Body}); inherited[key] > 0 {
			inherited[key]--
			continue
		}
		rows = append(rows, row)
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

// lineageAvailable reports an error when an ancestor's history before the fork
// is gone, as after the parent conversation is deleted. A switch would then
// hand off the fork's own turns without what came before.
func lineageAvailable(cwd string, lineage []historySegment) error {
	for _, segment := range lineage {
		if segment.Head == 0 {
			continue
		}
		rows, err := sessions.Transcript(cwd, segment.ID)
		if err != nil {
			return err
		}
		if len(rows) == 0 || rows[0].Rowid > segment.Head {
			return acp.NewInvalidParams("the conversation this one was forked from was deleted, so switching models would lose it; start a new conversation to use another model")
		}
	}
	return nil
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
