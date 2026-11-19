package main

import (
	"strings"
	"testing"
)

func TestPickerKeepsMultilineModelOnOneRow(t *testing.T) {
	m := picker{height: 12, width: 70, models: []string{"model\nname"}}
	if got := strings.Count(m.View().Content, "\n") + 1; got != m.height {
		t.Fatalf("picker drew %d rows in a %d-row terminal", got, m.height)
	}
}
