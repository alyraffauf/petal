package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"petal/internal/sessions"
)

func TestReadSearchHitInsideLongReply(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := sessions.Create(cwd, sessions.Record{ID: "thread"}); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("Earlier paragraph.\n\n", 70) + "The marigold circuit failed.\n\n" + strings.Repeat("Later paragraph.\n\n", 70)
	if err := sessions.SaveTranscript(cwd, "thread", []sessions.Message{{Role: "assistant", Body: text}}, false); err != nil {
		t.Fatal(err)
	}
	hits, err := sessions.Search(cwd, "", "marigold", 0, 1)
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	s := &transcriptSearch{cwd: cwd, hits: hits}
	s.openHit()
	p := picker{transcripts: s, width: 78, height: 12, background: terminalBackground}
	frame := ansi.Strip(p.View().Content)
	if !strings.Contains(frame, "The marigold circuit failed.") {
		t.Fatal(frame)
	}
}

func TestProjectMatchesKeepSemanticHitsOnFirstPage(t *testing.T) {
	s := &transcriptSearch{}
	s.input.SetValue("needle")
	p := picker{transcripts: s}
	hits := make([]sessions.Hit, searchPageSize+1)
	for i := range hits {
		hits[i].Row = int64(i + 1)
	}
	if cmd := p.transcriptInput(transcriptResults{page: s, hits: hits}); cmd == nil || !s.more {
		t.Fatal("expected semantic lookup and another keyword page")
	}
	semantic := sessions.Hit{Row: 100, Semantic: true}
	p.transcriptInput(transcriptResults{
		page: s, semantic: true, search: &sessions.ProjectSearch{}, final: true, hits: []sessions.Hit{semantic},
	})
	if len(s.hits) != searchPageSize+1 || s.hits[searchPageSize] != semantic {
		t.Fatalf("first page did not append the semantic match: %+v", s.hits)
	}
	if cmd := p.transcriptInput(tea.KeyPressMsg{Code: tea.KeyPgDown}); cmd == nil || s.offset != searchPageSize {
		t.Fatalf("next page offset = %d, want %d", s.offset, searchPageSize)
	}
	next := hits[searchPageSize:]
	if cmd := p.transcriptInput(transcriptResults{page: s, revision: s.revision, hits: next}); cmd != nil || s.more || !slices.Equal(s.hits, next) {
		t.Fatalf("second page did not contain just the remaining keyword hit: %+v", s.hits)
	}
}
func TestSemanticResultsKeepUpdatingWhileReading(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	if _, err := sessions.Create(cwd, sessions.Record{ID: "thread"}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SaveTranscript(cwd, "thread", []sessions.Message{
		{Role: "assistant", Body: "marigold"},
		{Role: "assistant", Body: "tulip"},
	}, false); err != nil {
		t.Fatal(err)
	}
	hits, err := sessions.Search(cwd, "", "marigold", 0, 1)
	if err != nil || len(hits) != 1 {
		t.Fatal(hits, err)
	}
	hits[0].Title = "Marigold thread"
	hits[0].Semantic = true
	replacement, err := sessions.Search(cwd, "", "tulip", 0, 1)
	if err != nil || len(replacement) != 1 {
		t.Fatal(replacement, err)
	}
	replacement[0].Title = "Tulip thread"
	replacement[0].Semantic = true

	s := &transcriptSearch{cwd: cwd, hits: hits}
	p := picker{transcripts: s, width: 78, height: 12, background: terminalBackground}
	p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	_, next := p.Update(transcriptResults{
		page: s, semantic: true, search: &sessions.ProjectSearch{}, hits: replacement,
		coverage: sessions.HistoryCoverage{Indexed: 1, Window: 2},
	})
	if next == nil {
		t.Error("reading interrupted semantic backfill")
	}
	frame := ansi.Strip(p.View().Content)
	if !strings.Contains(frame, "Marigold thread") || !strings.Contains(frame, "marigold") {
		t.Fatalf("reader lost the hit it opened:\n%s", frame)
	}
	p.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	frame = ansi.Strip(p.View().Content)
	if !strings.Contains(frame, "Tulip thread") {
		t.Fatalf("returning to results showed the old ranking:\n%s", frame)
	}
}

func TestClickBelowVisibleSearchHitsKeepsSelection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &transcriptSearch{cwd: t.TempDir(), hits: []sessions.Hit{
		{Title: "First hit"}, {Title: "Second hit"}, {Title: "Hidden hit"},
	}, lexical: 3}
	p := picker{transcripts: s, width: 78, height: 9, background: terminalBackground}
	rows := strings.Split(ansi.Strip(p.View().Content), "\n")
	blankRow := 7
	if strings.TrimSpace(rows[blankRow]) != "" || strings.Contains(strings.Join(rows, "\n"), "Hidden hit") {
		t.Fatalf("expected two visible hits and a blank body row, got %q", rows)
	}
	p.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: blankRow})
	if s.index != 0 || s.reader != nil {
		t.Fatalf("click on a blank row changed the selection to hit %d", s.index)
	}
}
