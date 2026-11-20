package tui

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

func TestImageInMultipartReply(t *testing.T) {
	const caption = "[image · image/png]"
	block, _ := json.Marshal(map[string]string{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(orchidPNG)})
	e := messageEvent("assistant", "reply", block, true)
	m := terminal{h: &harness{graphics: true}}
	key, upload := m.registerPreview(e.text, e.picture)
	if upload == nil {
		t.Fatal("no image transfer")
	}
	var pending turnLog
	pending.push(event{role: e.role, id: e.id, text: "before", chunk: true})
	e.text = key
	pending.push(e)
	pending.push(event{role: e.role, id: e.id, text: "after", chunk: true})
	entries := pending.commit(nil)
	if len(entries) != 3 || entries[0].Text != "before" || entries[2].Text != "after" {
		t.Fatal(entries)
	}
	lines := m.messageLines(1, entries[1], 60)
	if !strings.Contains(ansi.Strip(lines[0]), caption) || len(lines) < 2 || !strings.ContainsRune(lines[1], kitty.Placeholder) {
		t.Fatalf("bad preview: %q", lines)
	}
	if fallback := messageEvent(e.role, e.id, block, false); fallback.picture != nil || fallback.text != caption {
		t.Fatal("invalid text fallback")
	}
	if decodePreview("not base64") != nil {
		t.Fatal("invalid payload accepted")
	}
}
