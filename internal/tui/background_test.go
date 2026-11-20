package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"petal/internal/routing"
)

func TestConfiguredCanvasBackground(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		transparent  bool
	}{
		{"default", "", false},
		{"transparent", "transparent_background = true", true},
		{"opaque", "transparent_background = false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := routing.Load(path, "", "")
			if err != nil {
				t.Fatal(err)
			}
			m := newPicker(c)
			m.width, m.height = 80, 20
			chat := petal(screen{Width: 80, Height: 20, background: canvasBackground(c.TransparentBackground)})
			for name, frame := range map[string]string{"chat": chat, "picker": m.View().Content} {
				rows := strings.Split(frame, "\n")
				opaque := strings.Contains(rows[0], "48;2;"+petalInk+"m ")
				if opaque == tc.transparent {
					t.Fatalf("%s header has incorrect background: %q", name, rows[0])
				}
				opaque = strings.Contains(rows[12], "48;2;"+petalInk+"m")
				if opaque == tc.transparent {
					t.Fatalf("%s canvas has incorrect background: %q", name, rows[12])
				}
			}
		})
	}
}
