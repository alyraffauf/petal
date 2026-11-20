package routing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Claude owns profile authentication. Keep stdin open until its control
// response arrives. Closing it after the request can end the CLI prematurely.
func (a Account) claudeQuota(ctx context.Context, cli, model string) (float64, error) {
	cmd := a.Command(ctx, []string{cli, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence"})
	in, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	if _, err := io.WriteString(in, `{"type":"control_request","request_id":"petal-usage","request":{"subtype":"get_usage","skip_behaviors":true}}`+"\n"); err != nil {
		return 0, err
	}
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 4<<20)
	for scan.Scan() {
		var frame struct {
			Type     string
			Response struct {
				RequestID string `json:"request_id"`
				Subtype   string
				Response  json.RawMessage
			}
		}
		if err := json.Unmarshal(scan.Bytes(), &frame); err != nil {
			return 0, err
		}
		if frame.Type != "control_response" || frame.Response.RequestID != "petal-usage" {
			continue
		}
		if frame.Response.Subtype != "success" {
			return 0, errors.New("Claude usage request rejected")
		}
		return claudeRemaining(frame.Response.Response, model)
	}
	if err := scan.Err(); err != nil {
		return 0, err
	}
	return 0, io.ErrUnexpectedEOF
}

func claudeRemaining(data json.RawMessage, model string) (float64, error) {
	var usage struct {
		Available *bool                      `json:"rate_limits_available"`
		Windows   map[string]json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(data, &usage); err != nil {
		return 0, err
	}
	if usage.Available == nil || !*usage.Available {
		return -1, nil
	}
	windows := []string{"five_hour", "seven_day"}
	for _, family := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(strings.ToLower(model), family) {
			windows = append(windows, "seven_day_"+family)
			break
		}
	}
	remaining := -1.0
	for _, key := range windows {
		raw := usage.Windows[key]
		if len(raw) == 0 {
			continue
		}
		var window struct{ Utilization *float64 }
		if err := json.Unmarshal(raw, &window); err != nil {
			return 0, err
		}
		if window.Utilization == nil {
			continue
		}
		used := *window.Utilization
		if used < 0 || used > 100 {
			return 0, errors.New("invalid Claude quota utilization")
		}
		if remaining < 0 || 100-used < remaining {
			remaining = 100 - used
		}
	}
	return remaining, nil
}
