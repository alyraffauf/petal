package tui

import (
	"testing"
	"time"

	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestRemoteActivityReordersSavedHistory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)

	cfg := routing.Config{Accounts: map[string]routing.Account{
		"work": {Models: map[string][]string{"model": {"adapter"}}},
	}}
	older := sessions.Record{
		ID: `["work","model","older"]`, Account: "work", Model: "model",
		Title: "Saved title", Opened: time.Unix(1, 0), Updated: time.Unix(2, 0),
		Selections: []sessions.Selection{{ID: "mode", Value: "plan"}},
	}
	newer := sessions.Record{
		ID: `["work","model","newer"]`, Account: "work", Model: "model",
		Opened: time.Unix(3, 0),
	}
	for _, saved := range []sessions.Record{older, newer} {
		if _, err := sessions.Create(cwd, saved); err != nil {
			t.Fatal(err)
		}
	}
	p := newPicker(cfg)
	p.showHistory()

	latest := time.Unix(4, 0)
	upstream := sessions.Record{ID: older.ID, Account: older.Account, Model: older.Model, Opened: latest}
	stale := upstream
	stale.Opened = time.Unix(1, 0)
	p.Update(remoteSessions{picker: p, records: []sessions.Record{upstream, stale}})

	if len(p.saved) != 2 || p.saved[0].ID != older.ID || p.saved[1].ID != newer.ID {
		t.Fatalf("new adapter activity should put the older saved session first: %+v", p.saved)
	}
	got := p.saved[0]
	if !got.LastActivity().Equal(latest) || !got.Opened.Equal(older.Opened) || got.Title != older.Title ||
		len(got.Selections) != 1 || got.Selections[0] != older.Selections[0] {
		t.Fatalf("merged history record: %+v", got)
	}
}
