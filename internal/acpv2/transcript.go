// Package acpv2 projects ACP v2 session updates into a text transcript.
package acpv2

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Entry is one visible message or tool call in the transcript.
type Entry struct {
	Role string
	Text string
}

// Transcript materializes one session's user messages, assistant messages,
// thoughts, and tool calls. The zero value is ready to use. Serialize calls
// to a Transcript and keep one instance per session.
type Transcript struct {
	positions map[transcriptKey]int
	rows      []transcriptRow
}

type transcriptKey struct {
	role string
	id   string
}

type transcriptRow struct {
	role, id, name, title, status string
	content                       []string
	displaySet, lastText          bool
	rawText                       string
}

func NewTranscript() *Transcript { return &Transcript{} }

// Reset discards the transcript and all first-seen positions.
func (t *Transcript) Reset() { *t = Transcript{} }

// Entries returns an independent snapshot in first-seen order. A message
// with no displayable content keeps its position but has no visible entry.
func (t *Transcript) Entries() []Entry {
	var entries []Entry
	t.Visit(func(_ string, e Entry) { entries = append(entries, e) })
	return entries
}

// HasCompaction reports whether the transcript holds an entry for a compaction.
func (t *Transcript) HasCompaction(id string) bool {
	_, ok := t.positions[transcriptKey{role: "compaction", id: id}]
	return ok
}

// FinishedCompaction returns the latest summary after its terminal status arrives.
func (t *Transcript) FinishedCompaction(id string) (Entry, bool) {
	const role = "compaction"
	index, ok := t.positions[transcriptKey{role: role, id: id}]
	if !ok {
		return Entry{}, false
	}
	row := &t.rows[index]
	switch row.status {
	case "completed", "failed", "cancelled":
		return Entry{Role: role, Text: row.text()}, true
	}
	return Entry{}, false
}

// Visit yields visible entries with their adapter IDs in first-seen order.
func (t *Transcript) Visit(yield func(string, Entry)) {
	for _, row := range t.rows {
		if text := row.text(); text != "" {
			yield(row.id, Entry{Role: row.role, Text: text})
		}
	}
}

func (row transcriptRow) text() string {
	body := row.body()
	header := row.header()
	if header == "" {
		return body
	}
	if body == "" {
		return header
	}
	return header + "\n\n" + body
}

func (row transcriptRow) body() string {
	if row.role == "tool" && !row.displaySet {
		return row.rawText
	}
	return strings.Join(row.content, "")
}

func (row transcriptRow) header() string {
	if row.role == "compaction" {
		return strings.TrimSpace("Context compaction " + row.status + " " + row.title)
	}
	if row.role != "tool" {
		return ""
	}
	label := row.title
	if label == "" {
		label = row.name
	}
	if label == "" {
		label = row.id
	}
	if label == "" {
		label = "Tool call"
	}
	status := row.status
	if status == "" {
		status = "pending"
	}
	return "**" + label + "** · " + status
}

// Apply takes the update object from session/update params. It returns true
// when Entries changes. Unsupported update kinds, including replay_complete,
// leave the transcript untouched. A malformed required field on a supported
// update returns an error before any changes. Invalid optional patch fields
// are ignored and invalid items in replacement arrays are skipped.
//
// Apply updates in receive order. Upserts patch the same entry and chunks
// append once per delivery. For full replay, build a fresh Transcript and
// install it after replay succeeds. The caller owns replay boundaries and
// cursor deduplication.
func (t *Transcript) Apply(update json.RawMessage) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(update, &fields); err != nil {
		return false, fmt.Errorf("decode ACP v2 session update: %w", err)
	}
	return t.ApplyFields(fields, 2)
}

// ApplyFields projects a decoded update, including tool updates from v1 adapters.
func (t *Transcript) ApplyFields(fields map[string]json.RawMessage, version uint16) (bool, error) {
	kind, ok := readString(fields["sessionUpdate"])
	if version == 1 && kind == "tool_call" {
		kind = "tool_call_update"
	}
	if !ok {
		return false, fmt.Errorf("ACP v2 session update requires a string sessionUpdate")
	}

	var role string
	idField := "messageId"
	switch kind {
	case "compaction_update", "compaction_summary_chunk":
		role = "compaction"
		idField = "compactionId"
	case "user_message", "user_message_chunk":
		role = "user"
	case "agent_message", "agent_message_chunk":
		role = "assistant"
	case "agent_thought", "agent_thought_chunk":
		role = "system"
	case "tool_call_update", "tool_call_content_chunk":
		role = "tool"
		idField = "toolCallId"
	default:
		return false, nil
	}
	id, ok := readString(fields[idField])
	if !ok {
		return false, fmt.Errorf("ACP v2 %s requires a string %s", kind, idField)
	}

	chunk := strings.HasSuffix(kind, "_chunk")
	var chunkText string
	var plain bool
	if chunk {
		var valid bool
		if role == "tool" {
			chunkText, plain, valid = toolBlock(fields["content"], version)
		} else {
			chunkText, valid = projectContent(fields["content"], false)
		}
		if !valid {
			return false, fmt.Errorf("ACP v2 %s requires a content block", kind)
		}
	}

	key := transcriptKey{role: role, id: id}
	position, found := t.positions[key]
	if !found {
		if t.positions == nil {
			t.positions = make(map[transcriptKey]int)
		}
		position = len(t.rows)
		t.positions[key] = position
		t.rows = append(t.rows, transcriptRow{role: role, id: id})
	}
	row := &t.rows[position]
	if chunk {
		if role == "tool" {
			hadFallback := !row.displaySet && row.rawText != ""
			row.appendTool(chunkText, plain)
			return !found || chunkText != "" || hadFallback, nil
		}
		if chunkText != "" {
			row.content = append(row.content, chunkText)
		}
		return chunkText != "", nil
	}

	before := *row
	content := fields["content"]
	if role == "compaction" {
		patchString(&row.status, fields["status"])
		patchString(&row.title, fields["error"])
		content = fields["summary"]
	}
	if role == "tool" {
		patchString(&row.name, fields["name"])
		patchString(&row.title, fields["title"])
		patchString(&row.status, fields["status"])
		var blocks []json.RawMessage
		if content != nil && json.Unmarshal(content, &blocks) == nil {
			row.content = nil
			row.lastText = false
			row.displaySet = true
			row.rawText = ""
			for _, block := range blocks {
				text, plain, valid := toolBlock(block, version)
				if valid {
					row.appendTool(text, plain)
				}
			}
		}
		if raw := fields["rawOutput"]; !row.displaySet && raw != nil {
			row.rawText = rawToolText(raw)
		}
		return !found || before.header() != row.header() || before.body() != row.body(), nil
	}

	var blocks []json.RawMessage
	if content != nil && json.Unmarshal(content, &blocks) == nil {
		row.content = nil
		for _, block := range blocks {
			if projected, valid := projectContent(block, false); valid && projected != "" {
				row.content = append(row.content, projected)
			}
		}
		return before.text() != row.text(), nil
	}
	return role == "compaction" && (!found || before.header() != row.header()), nil
}

func readString(raw json.RawMessage) (string, bool) {
	var value *string
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return "", false
	}
	return *value, true
}

func patchString(dst *string, raw json.RawMessage) {
	if string(raw) == "null" {
		*dst = ""
	} else if value, ok := readString(raw); ok {
		*dst = value
	}
}
