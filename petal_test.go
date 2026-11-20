package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPetalKeepsMultilineChromeOnItsOwnRows(t *testing.T) {
	v := screen{
		Width: 80, Height: 27, Model: "model\nvariant", Account: "account\nname",
		Status: "status\nready", Cwd: "/tmp/line\nbreak", editor: testComposer("draft\ncontinued", 9),
	}
	rows := strings.Split(ansi.Strip(petal(v)), "\n")
	if len(rows) != v.Height {
		t.Fatalf("got %d rows, want %d", len(rows), v.Height)
	}
	for _, want := range []struct {
		row  int
		text string
	}{
		{0, "status ready"},
		{1, "model variant / account name"},
		{25, "continued"},
		{26, "/tmp/line break"},
	} {
		if !strings.Contains(rows[want.row], want.text) {
			t.Errorf("row %d = %q, want %q", want.row, rows[want.row], want.text)
		}
	}
}
