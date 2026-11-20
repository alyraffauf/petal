package routing

import (
	"testing"
)

func TestClaudeUsageSelectsApplicableWindows(t *testing.T) {
	data := []byte(`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":61.5},"seven_day":{"utilization":40},"seven_day_opus":{"utilization":100},"extra_usage":{"utilization":100}}}`)
	for model, want := range map[string]float64{"sonnet": 38.5, "opus": 0, "claude/default": 38.5} {
		if got, err := claudeRemaining(data, model); err != nil || got != want {
			t.Fatalf("%s: got %v, %v; want %v", model, got, err, want)
		}
	}
}
