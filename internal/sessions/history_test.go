package sessions

import (
	"strings"
	"testing"
)

func TestReadHistoryAfterCompaction(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	for _, id := range []string{"parent", "other"} {
		if _, err := Create(project, Record{ID: id, Model: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	body := "x\x00" + strings.Repeat("🌺", 1024) + "tail"
	for _, message := range []Message{{Role: "tool", Body: body}, {Role: "compaction", Body: "summary one"}, {Role: "compaction", Body: "summary two"}} {
		if err := SaveTranscript(project, "parent", []Message{message}, true); err != nil {
			t.Fatal(err)
		}
	}
	read := func(ref int64, offset int) HistoryPassage {
		t.Helper()
		value, err := ReadHistory(t.Context(), project, "parent", ref, offset)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	last := read(0, 0)
	first := read(read(last.Previous, 0).Previous, 0)
	tail := read(first.Reference, first.NextOffset)
	if last.Text != "summary two" || first.NextOffset != 4094 || first.Text+tail.Text != body || tail.NextOffset != 0 {
		t.Fatal("incomplete archive traversal")
	}
	if _, err := ReadHistory(t.Context(), project, "other", first.Reference, 0); err == nil {
		t.Fatal("unrelated session read the message")
	}
}

func TestSearchHistoryContinuesAcrossNewTurns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := Create(cwd, Record{ID: "p", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	messages := make([]Message, 11)
	for i := range messages {
		messages[i] = Message{Role: "tool", Body: strings.Repeat("x", 600) + " needle"}
	}
	if err := SaveTranscript(cwd, "p", messages, false); err != nil {
		t.Fatal(err)
	}
	search := func(before int64) HistoryMatches {
		t.Helper()
		page, err := SearchHistory(t.Context(), cwd, "p", "needle", before)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := search(0)
	if len(first.Hits) != 10 || first.NextBefore == 0 || !strings.Contains(first.Hits[0].Snippet, "needle") {
		t.Fatal(first)
	}
	if err := SaveTranscript(cwd, "p", messages[:1], false); err != nil {
		t.Fatal(err)
	}
	last := search(first.NextBefore)
	if len(last.Hits) != 1 || last.NextBefore != 0 || last.Hits[0].Reference >= first.NextBefore {
		t.Fatal(last)
	}
}
