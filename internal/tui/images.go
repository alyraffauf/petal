package tui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math/rand/v2"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi/kitty"
)

// Multiplexers require an independently configured graphics passthrough.
func directGraphicsTerminal() bool {
	if os.Getenv("TMUX") != "" || os.Getenv("STY") != "" {
		return false
	}
	switch os.Getenv("TERM") {
	case "xterm-kitty", "xterm-ghostty":
		return true
	}
	return false
}

type previewImage struct {
	pixels *image.NRGBA
	digest [32]byte
}

func decodePreview(encoded string) *previewImage {
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(7<<20) {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > (16<<20)/config.Height {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	scale := min(1.0, 384.0/float64(config.Width), 240.0/float64(config.Height))
	w, h := max(1, int(float64(config.Width)*scale)), max(1, int(float64(config.Height)*scale))
	pixels := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			pixels.Set(x, y, img.At(x*config.Width/w, y*config.Height/h))
		}
	}
	return &previewImage{pixels, sha256.Sum256(data)}
}

func messageEvent(role, id string, raw json.RawMessage, graphics bool) event {
	e := event{role: role, id: id, chunk: true, text: contentText(raw)}
	if graphics && strings.HasPrefix(e.text, "[image · ") {
		var block struct{ Type, Data string }
		if json.Unmarshal(raw, &block) == nil && block.Type == "image" {
			e.picture = decodePreview(block.Data)
		}
	}
	return e
}

type inlinePreview struct {
	caption string
	cols    int
	lines   []string
}

func (m *terminal) registerPreview(caption string, pic *previewImage) (string, tea.Cmd) {
	if pic == nil || m.limitedColor || m.h == nil || !m.h.graphics {
		return caption, nil
	}
	label := fmt.Sprintf("%s · %x", caption, pic.digest)
	if _, ok := m.images[label]; ok {
		return label, nil
	}
	// Bound both terminal-side graphics memory and retained placeholder layouts.
	if len(m.images) >= 128 {
		return caption, nil
	}
	bounds := pic.pixels.Bounds()
	ratio := float64(bounds.Dx()) / float64(bounds.Dy())
	cols := max(1, min(32, int(20*ratio+0.5)))
	rows := max(1, min(10, int(float64(cols)/(2*ratio)+0.5)))
	if m.imageBase == 0 {
		m.imageBase = 1 + rand.IntN(0xffffff-128)
	}
	id := m.imageBase + len(m.images)
	if m.images == nil {
		m.images = make(map[string]inlinePreview)
	}
	preview := inlinePreview{cols: cols, caption: caption}
	for y := range rows {
		var line strings.Builder
		fmt.Fprintf(&line, "\x1b[38;2;%d;%d;%dm", id>>16, (id>>8)&255, id&255)
		for x := range cols {
			line.WriteRune(kitty.Placeholder)
			line.WriteRune(kitty.Diacritic(y))
			line.WriteRune(kitty.Diacritic(x))
		}
		line.WriteString("\x1b[39m")
		preview.lines = append(preview.lines, line.String())
	}
	var wire bytes.Buffer
	opts := kitty.Options{ID: id, Action: kitty.TransmitAndPut, Quiet: 2, Format: kitty.PNG, Transmission: kitty.Direct, Chunk: true, VirtualPlacement: true, Columns: cols, Rows: rows, DoNotMoveCursor: true}
	if err := kitty.EncodeGraphics(&wire, pic.pixels, &opts); err != nil {
		return caption, nil
	}
	m.images[label] = preview
	return label, tea.Raw(wire.String())
}
