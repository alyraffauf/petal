package tui

import (
	"encoding/base64"
	"os"
	"strings"
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
	data, err := os.ReadFile("orchid.png")
	if err != nil {
		t.Fatal(err)
	}
	packet("stale", "DATA", "eA==")
	for _, chunk := range [][]byte{data[:3], data[3:]} {
		packet(id, "DATA", base64.StdEncoding.EncodeToString(chunk))
	}
	packet("stale", "DONE", "")
	if m.editor.Value() != "" {
		t.Fatal("incomplete transfer changed draft")
	}
	packet(id, "DONE", "")
	if m.editor.Value() != imageMarker || m.editor.image["data"] != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("staged %v", m.editor.image)
	}
	m.editor.Reset()
	if m.editor.image != nil {
		t.Fatal("cleared draft retained image")
	}
}

func TestClipboardInputSeparatesKeyboardAndPacketAtEverySplit(t *testing.T) {
	for _, end := range []string{"\a", "\x1b\\"} {
		packet := clipboardPrefix + "type=read:id=x:status=DATA;YWJjZA==" + end
		wire := "draft" + packet + "caption\r"
		for i := 0; i <= len(wire); i++ {
			c := new(clipboardInput)
			out := c.frame(nil, []byte(wire[:i]))
			out = c.frame(out, []byte(wire[i:]))
			if string(out) != "draft"+clipboardPacketMarker+"caption\r" || c.restorePacket(nil, uv.UnknownOscEvent(clipboardPacketMarker)) != uv.UnknownOscEvent(packet) {
				t.Fatalf("split %d leaked or lost input", i)
			}
		}
		c := new(clipboardInput)
		paste := "\x1b[200~" + packet + "\x1b[201~"
		if got := string(c.frame(nil, []byte(paste))); got != paste || len(c.packets) != 0 {
			t.Fatal("literal paste interpreted as a clipboard reply")
		}
	}
}

func TestClipboardRejectsDamagedPNG(t *testing.T) {
	data, err := os.ReadFile("orchid.png")
	if err != nil {
		t.Fatal(err)
	}
	m := terminal{screen: screen{editor: newComposer()}}
	for _, damaged := range [][]byte{data[:len(data)/2], append(append([]byte{}, data[:100]...), data[150:]...)} {
		m.clipboard = clipboardRead{id: "x", editor: m.editor, mime: "image/png", data: damaged}
		m.clipboardEvent(uv.UnknownOscEvent(clipboardPrefix + "type=read:id=x:status=DONE\a"))
		if m.editor.Value() != "" || !strings.Contains(m.Messages[len(m.Messages)-1].Text, "damaged") {
			t.Fatal("damaged image entered the draft")
		}
	}
}
