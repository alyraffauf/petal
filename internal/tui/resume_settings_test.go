package tui

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"petal/internal/proxy"
	"petal/internal/sessions"
)

func TestResumeMergesSettingsWithAnInterveningSessionUpdate(t *testing.T) {
	for _, concurrentMode := range []string{"read", "other"} {
		t.Run(concurrentMode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			cwd := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			mode := sessions.Selection{LegacyMode: true, Value: "read"}
			record := sessions.Record{ID: "session", Title: "Old title", Selections: []sessions.Selection{mode}}
			if _, err := sessions.Create(cwd, record); err != nil {
				t.Fatal(err)
			}
			h := &harness{cwd: cwd, id: record.ID, lifetime: ctx, events: make(chan tea.Msg, 1), permissions: make(chan struct{}, 1)}
			h.initialSettings = settingsFrom(proxy.Object{"modes": json.RawMessage(`{
				"currentModeId":"write",
				"availableModes":[{"id":"write","name":"Write"},{"id":"read","name":"Read only"}]
			}`)})
			done := make(chan error, 1)
			go func() { done <- h.prepareSessionRecord(ctx, sessions.Record{ID: record.ID}, true) }()
			var question *permission
			select {
			case msg := <-h.events:
				question = msg.(event).ask
			case err := <-done:
				t.Fatalf("resume returned before reviewing the changed mode: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			extra := sessions.Selection{ID: "effort", Value: "high"}
			updated := time.Unix(123, 0)
			if _, err := sessions.Update(cwd, record.ID, func(latest *sessions.Record) bool {
				latest.Title, latest.Updated = "New title", updated
				latest.Selections[0].Value = concurrentMode
				latest.Selections = append(latest.Selections, extra)
				return true
			}); err != nil {
				t.Fatal(err)
			}
			for i, option := range question.options {
				if option.OptionID == "keep" {
					question.reply <- i
					break
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			saved, err := sessions.Lookup(cwd, record.ID)
			mode.Value = concurrentMode
			if concurrentMode == "read" {
				mode.Value = "write"
			}
			if err != nil || saved.Title != "New title" || !saved.Updated.Equal(updated) || !slices.Equal(saved.Selections, []sessions.Selection{mode, extra}) {
				t.Fatalf("saved session = %+v, %v", saved, err)
			}
		})
	}
}
