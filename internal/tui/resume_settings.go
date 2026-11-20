package tui

import (
	"context"
	"fmt"
	"slices"

	"petal/internal/sessions"
)

func selectionIndex(selections []sessions.Selection, selection sessions.Selection) int {
	return slices.IndexFunc(selections, func(candidate sessions.Selection) bool {
		return candidate.ID == selection.ID && candidate.LegacyMode == selection.LegacyMode
	})
}

func (h *harness) rememberSetting(selection sessions.Selection) error {
	return h.updateRecord(func(record *sessions.Record) bool {
		index := selectionIndex(record.Selections, selection)
		if index < 0 {
			record.Selections = append(record.Selections, selection)
			return true
		}
		if record.Selections[index] == selection {
			return false
		}
		record.Selections[index] = selection
		return true
	})
}

func (u settingsUpdate) selected(s sessions.Selection) (target setting, current string, available bool) {
	if s.LegacyMode {
		for _, mode := range u.Modes {
			available = available || mode.OptionID == s.Value
		}
		return setting{Name: "Mode", Type: "mode"}, u.CurrentModeID, available
	}
	for _, target := range u.Config {
		if target.ID == s.ID {
			for _, choice := range target.choices() {
				available = available || choice.Value == s.Value
			}
			return target, target.current(), available
		}
	}
	return setting{Name: s.ID}, "", false
}

// Consult the user before resuming with changed model or permission settings.
// An adapter may return fresh defaults even when it restores the transcript.
func (h *harness) reviewResumedSettings(ctx context.Context) error {
	previous := h.record.Selections
	var accepted []sessions.Selection
	for _, saved := range previous {
		target, current, available := h.initialSettings.selected(saved)
		if current == saved.Value {
			accepted = append(accepted, saved)
			continue
		}
		label := current
		if label == "" {
			label = "unavailable"
		}
		title := fmt.Sprintf("Resuming changed %s from %q to %q.", target.Name, saved.Value, label)
		opts := []option{{"keep", "Use the adapter's current setting"}, {"cancel", "Cancel resume"}}
		if available {
			opts = append([]option{{"restore", "Restore " + saved.Value}}, opts...)
		}
		id, err := h.choose(ctx, title, opts)
		if err != nil {
			return err
		}
		switch id {
		case "keep":
			if current != "" {
				saved.Value = current
				accepted = append(accepted, saved)
			}
		case "restore":
			result := h.changeSetting(ctx, target, saved.Value)
			if result.err != nil {
				return result.err
			}
			if result.options.Config != nil {
				h.initialSettings.Config = result.options.Config
			}
			if result.options.CurrentModeID != "" {
				h.initialSettings.CurrentModeID = result.options.CurrentModeID
			}
			if _, got, _ := h.initialSettings.selected(saved); got != saved.Value {
				return fmt.Errorf("adapter did not confirm %s = %q", target.Name, saved.Value)
			}
			accepted = append(accepted, saved)
		default:
			return fmt.Errorf("resume cancelled")
		}
	}
	for _, saved := range accepted {
		target, current, _ := h.initialSettings.selected(saved)
		if saved.Value != current {
			return fmt.Errorf("another setting changed %s to %q during resume", target.Name, current)
		}
	}
	return h.saveReviewedSelections(previous, accepted)
}

func (h *harness) saveReviewedSelections(previous, accepted []sessions.Selection) error {
	if slices.Equal(previous, accepted) {
		return nil
	}
	return h.updateRecord(func(record *sessions.Record) bool {
		changed := false
		for _, reviewed := range previous {
			current := selectionIndex(record.Selections, reviewed)
			if current < 0 || record.Selections[current] != reviewed {
				continue
			}
			next := selectionIndex(accepted, reviewed)
			if next < 0 {
				record.Selections = slices.Delete(record.Selections, current, current+1)
				changed = true
			} else if accepted[next] != reviewed {
				record.Selections[current] = accepted[next]
				changed = true
			}
		}
		return changed
	})
}
