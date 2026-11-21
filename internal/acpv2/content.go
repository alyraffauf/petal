package acpv2

import (
	"encoding/json"
	"strings"
)

// projectContent returns false for an invalid block. A future block type is
// valid and may have no text representation.
func projectContent(raw json.RawMessage, tool bool) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return "", false
	}
	kind, ok := readString(fields["type"])
	if !ok {
		return "", false
	}
	if tool {
		switch kind {
		case "content":
			return projectContent(fields["content"], false)
		case "diff":
			return projectDiff(fields)
		case "terminal":
			id, valid := readString(fields["terminalId"])
			return "[Terminal " + id + "]", valid
		case "session":
			id, valid := readString(fields["sessionId"])
			return "[Session " + id + "]", valid
		}
		return "", true
	}

	switch kind {
	case "text":
		return readString(fields["text"])
	case "image", "audio":
		_, hasData := readString(fields["data"])
		_, hasMediaType := readString(fields["mimeType"])
		return "[" + kind + "]", hasData && hasMediaType
	case "resource_link":
		name, hasName := readString(fields["name"])
		uri, hasURI := readString(fields["uri"])
		title, _ := readString(fields["title"])
		if title != "" {
			name = title
		}
		return name + " (" + uri + ")", hasName && hasURI
	case "resource":
		var resource struct {
			Text *string `json:"text"`
			Blob *string `json:"blob"`
			URI  *string `json:"uri"`
		}
		if json.Unmarshal(fields["resource"], &resource) != nil || resource.URI == nil {
			return "", false
		}
		if resource.Text != nil {
			return *resource.Text, true
		}
		return "[Resource " + *resource.URI + "]", resource.Blob != nil
	default:
		return "", true
	}
}

func projectDiff(fields map[string]json.RawMessage) (string, bool) {
	var changes []json.RawMessage
	if json.Unmarshal(fields["changes"], &changes) != nil || changes == nil {
		return "", false
	}
	var patch struct {
		Format string `json:"format"`
		Text   string `json:"text"`
	}
	if json.Unmarshal(fields["patch"], &patch) == nil && patch.Format == "git_patch" && patch.Text != "" {
		return patch.Text, true
	}
	var paths []string
	for _, raw := range changes {
		var change struct {
			Operation string `json:"operation"`
			Path      string `json:"path"`
			OldPath   string `json:"oldPath"`
		}
		if json.Unmarshal(raw, &change) != nil || change.Path == "" {
			continue
		}
		switch change.Operation {
		case "move", "copy":
			if change.OldPath != "" {
				paths = append(paths, change.Operation+" "+change.OldPath+" → "+change.Path)
			}
		case "add", "delete", "modify":
			paths = append(paths, change.Operation+" "+change.Path)
		}
	}
	return strings.Join(paths, "\n"), true
}

func (r *transcriptRow) appendTool(text string, plain bool) {
	r.displaySet = true
	r.rawText = ""
	if text == "" {
		return
	}
	if len(r.content) > 0 && !(r.lastText && plain) {
		r.content = append(r.content, "\n\n")
	}
	r.content = append(r.content, text)
	r.lastText = plain
}

func toolBlock(raw json.RawMessage, version uint16) (text string, plain, valid bool) {
	var block struct {
		Type, Path, OldText, NewText string
		Content                      struct {
			Type string
			Text *string
		}
	}
	if json.Unmarshal(raw, &block) != nil {
		return "", false, false
	}
	if version == 1 && block.Type == "diff" {
		return block.Path + "\nBefore:\n" + block.OldText + "\nAfter:\n" + block.NewText, false, true
	}
	if block.Type == "content" && block.Content.Type == "text" && block.Content.Text != nil {
		return *block.Content.Text, version == 2, true
	}
	text, valid = projectContent(raw, true)
	return text, false, valid
}

// Stock adapters may expose MCP text only in rawOutput.result.content or rawOutput.
func rawToolText(raw json.RawMessage) string {
	type item struct{ Type, Text string }
	var blocks []item
	if len(raw) > 0 && raw[0] == '[' {
		if json.Unmarshal(raw, &blocks) != nil {
			return ""
		}
	} else {
		var output struct{ Result struct{ Content []item } }
		if json.Unmarshal(raw, &output) != nil {
			return ""
		}
		blocks = output.Result.Content
	}
	var text []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			text = append(text, b.Text)
		}
	}
	return strings.Join(text, "\n\n")
}
