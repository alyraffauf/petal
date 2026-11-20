package tui

import (
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// OSC 5522 reads the clipboard from the terminal displaying the session,
// including when Petal itself runs on the other side of an SSH connection.
type clipboardRead struct {
	id, mime, loc, requested string
	data                     []byte
	editor                   *composer
	picker                   *picker
}
type clipboardTimeout string

var clipboardImages = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

const clipboardFallback = "Copy the image to the SSH host and use /attach <path>."

func (m *terminal) readClipboardImage() tea.Cmd {
	if m.h == nil || !m.h.promptCaps.Image {
		m.recordNote("This agent does not accept images.")
		return nil
	}
	return m.requestClipboard(strings.Join(clipboardImages, " "), "", "")
}

func (m *terminal) requestClipboard(types, password, location string) tea.Cmd {
	id := fmt.Sprintf("petal-%x", rand.Uint64())
	m.clipboard = clipboardRead{id: id, editor: m.editor, picker: m.launch, loc: location, requested: types}
	meta := "type=read:id=" + id
	if password != "" {
		meta += ":pw=" + password + ":name=UGFzdGUgZXZlbnQ="
	}
	if location == "primary" {
		meta += ":loc=primary"
	}
	return tea.Batch(tea.Raw("\x1b]5522;"+meta+";"+base64.StdEncoding.EncodeToString([]byte(types))+"\x1b\\"), tea.Tick(20*time.Second, func(time.Time) tea.Msg { return clipboardTimeout(id) }))
}

func (m *terminal) clipboardError(text string) tea.Cmd {
	m.clipboard = clipboardRead{}
	m.recordNote(text)
	return nil
}

func (m *terminal) clipboardEvent(msg tea.Msg) (bool, tea.Cmd) {
	if timeout, ok := msg.(clipboardTimeout); ok {
		if string(timeout) == m.clipboard.id {
			m.clipboardError("Terminal clipboard timed out. " + clipboardFallback)
		}
		return true, nil
	}
	event, ok := msg.(uv.UnknownOscEvent)
	if !ok {
		return false, nil
	}
	seq, ok := strings.CutPrefix(string(event), "\x1b]5522;")
	if !ok {
		return false, nil
	}
	meta, payload, _ := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(seq, "\x1b\\"), "\a"), ";")
	fields := map[string]string{}
	for field := range strings.SplitSeq(meta, ":") {
		k, v, _ := strings.Cut(field, "=")
		fields[k] = v
	}
	if fields["type"] != "read" {
		return true, nil
	}
	mimeBytes, _ := base64.StdEncoding.DecodeString(fields["mime"])
	mime := string(mimeBytes)
	r := &m.clipboard
	if fields["id"] == "" {
		return true, m.clipboardOffer(fields, mime, payload)
	}
	if fields["id"] != r.id {
		return true, nil
	}
	if r.editor != m.editor || r.picker != m.launch || m.reading {
		m.clipboard = clipboardRead{}
		return true, nil
	}
	switch fields["status"] {
	case "OK":
		return true, nil
	case "DATA":
		if !slices.Contains(strings.Fields(r.requested), mime) || (r.mime != "" && mime != r.mime) {
			return true, nil
		} // Only one representation of an image is needed.
		if len(payload) > base64.StdEncoding.EncodedLen(4096) {
			return true, m.clipboardError("Terminal sent an oversized clipboard chunk.")
		}
		chunk, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return true, m.clipboardError("Terminal sent invalid clipboard data.")
		}
		if len(r.data)+len(chunk) > 7<<20 {
			return true, m.clipboardError("Clipboard exceeds the 7 MiB attachment limit.")
		}
		r.mime = mime
		r.data = append(r.data, chunk...)
	case "DONE":
		data, typ := r.data, r.mime
		m.clipboard = clipboardRead{}
		if strings.HasPrefix(typ, "text/") {
			return true, func() tea.Msg { return tea.PasteMsg{Content: string(data)} }
		}
		if len(data) == 0 {
			return true, m.clipboardError("Clipboard has no supported image. " + clipboardFallback)
		}
		if !slices.Contains(clipboardImages, typ) || http.DetectContentType(data) != typ {
			return true, m.clipboardError("Clipboard image format does not match its contents.")
		}
		return true, m.stageImage(map[string]any{"type": "image", "mimeType": typ, "data": base64.StdEncoding.EncodeToString(data)})
	default:
		return true, m.clipboardError("Terminal clipboard unavailable (" + petalLabel(fields["status"]) + "). " + clipboardFallback)
	}
	return true, nil
}

func (m *terminal) stageImage(block map[string]any) tea.Cmd {
	if m.editor.image != nil {
		m.recordNote("Send the staged image or remove its marker before pasting another.")
		return nil
	}
	cmd := m.edit(tea.PasteMsg{Content: imageMarker})
	if strings.Contains(m.editor.Value(), imageMarker) {
		m.editor.image = block
	}
	return cmd
}

func (c *composer) imageMessage(input string) promptMessage {
	before, after, _ := strings.Cut(input, imageMarker)
	var blocks []map[string]any
	if before != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": before})
	}
	blocks = append(blocks, c.image)
	if after != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": after})
	}
	return promptMessage{text: input, content: blocks}
}

func (m *terminal) clipboardOffer(fields map[string]string, mime, payload string) tea.Cmd {
	// A terminal paste gesture offers its MIME list with a one-use password.
	if fields["status"] == "OK" {
		m.clipboard.loc = fields["loc"]
	}
	if fields["status"] != "DATA" || mime != "." || len(payload) > 32768 || m.reading {
		return nil
	}
	formats, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	pw := fields["pw"]
	if _, err := base64.StdEncoding.DecodeString(pw); err != nil || strings.ContainsAny(pw, "\r\n") {
		return nil
	}
	options := strings.Fields(string(formats))
	if m.launch == nil && m.ask == nil {
		for _, typ := range clipboardImages {
			if slices.Contains(options, typ) {
				if m.h == nil || !m.h.promptCaps.Image {
					return m.clipboardError("This agent does not accept images.")
				}
				return m.requestClipboard(typ, pw, m.clipboard.loc)
			}
		}
	}
	for _, typ := range []string{"text/plain", "text/plain;charset=utf-8", "text/uri-list"} {
		if slices.Contains(options, typ) {
			return m.requestClipboard(typ, pw, m.clipboard.loc)
		}
	}
	return nil
}
