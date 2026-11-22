package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPetalKeepsMultilineChromeOnItsOwnRows(t *testing.T) {
	v := screen{connection: connected,
		Width: 80, Height: 27, Model: "model\nvariant", Account: "account\nname",
		Status: "status\nready", Cwd: "/tmp/line\nbreak", editor: testComposer("draft\ncontinued", 9),
	}
	rows := strings.Split(ansi.Strip(petal(v)), "\n")
	if len(rows) != v.Height {
		t.Fatalf("got %d rows, want %d", len(rows), v.Height)
	}
	for _, want := range []string{"status ready", "model variant / account name", "continued", "/tmp/line break"} {
		i := slices.IndexFunc(rows, func(row string) bool { return strings.Contains(row, want) })
		if i < 0 {
			t.Fatalf("missing row containing %q", want)
		}
		rows = rows[i+1:]
	}
}
