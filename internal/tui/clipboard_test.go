package tui

import (
	"encoding/base64"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestClipboardStagesOnlyCompleteMatchingRead(t *testing.T) {
	m := terminal{screen: screen{editor: newComposer()}}
	m.requestClipboard("image/png", "", "")
	id := m.clipboard.id
	packet := func(id, status, payload string) {
		t.Helper()
		m.clipboardEvent(uv.UnknownOscEvent("\x1b]5522;type=read:id=" + id + ":status=" + status + ":mime=aW1hZ2UvcG5n;" + payload + "\x1b\\"))
	}
	// The receiver checks the MIME signature. Full decoding is done by the preview.
	header := []byte("\x89PNG\r\n\x1a\n")
	packet("stale", "DATA", "eA==")
	for _, chunk := range [][]byte{header[:3], header[3:]} {
		packet(id, "DATA", base64.StdEncoding.EncodeToString(chunk))
	}
	packet("stale", "DONE", "")
	if m.editor.Value() != "" {
		t.Fatal("incomplete transfer changed draft")
	}
	packet(id, "DONE", "")
	if m.editor.Value() != imageMarker || m.editor.image["data"] != base64.StdEncoding.EncodeToString(header) {
		t.Fatalf("staged %v", m.editor.image)
	}
	m.editor.Reset()
	if m.editor.image != nil {
		t.Fatal("cleared draft retained image")
	}
}
