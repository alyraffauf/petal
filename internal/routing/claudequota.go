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
func (a Account) claudeQuota(ctx context.Context, cli, model string) Quota {
	cmd := a.Command(ctx, []string{cli, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence"})
	in, out, cleanup, err := startQuotaProcess(ctx, cmd)
	if err != nil {
		return Quota{Err: err}
	}
	defer cleanup()
	if _, err := io.WriteString(in, `{"type":"control_request","request_id":"petal-usage","request":{"subtype":"get_usage","skip_behaviors":true}}`+"\n"); err != nil {
		return Quota{Err: err}
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
			return Quota{Err: err}
		}
		if frame.Type != "control_response" || frame.Response.RequestID != "petal-usage" {
			continue
		}
		if frame.Response.Subtype != "success" {
			return Quota{Err: errors.New("Claude usage request rejected")}
		}
		return claudeQuotaWindow(frame.Response.Response, model)
	}
	if err := scan.Err(); err != nil {
		return Quota{Err: err}
	}
	return Quota{Err: io.ErrUnexpectedEOF}
}

func claudeRemaining(data json.RawMessage, model string) (float64, error) {
	q := claudeQuotaWindow(data, model)
	return q.Remaining, q.Err
}

func claudeQuotaWindow(data json.RawMessage, model string) Quota {
	var usage struct {
		Available *bool                      `json:"rate_limits_available"`
		Windows   map[string]json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(data, &usage); err != nil {
		return Quota{Err: err}
	}
	if usage.Available == nil || !*usage.Available {
		return Quota{Remaining: -1}
	}
	windows := []string{"five_hour", "seven_day"}
	for _, family := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(strings.ToLower(model), family) {
			windows = append(windows, "seven_day_"+family)
			break
		}
	}
	q := Quota{Remaining: -1}
	for _, key := range windows {
		raw := usage.Windows[key]
		if len(raw) == 0 {
			continue
		}
		var window struct{ Utilization *float64 }
		if err := json.Unmarshal(raw, &window); err != nil {
			return Quota{Err: err}
		}
		if window.Utilization == nil {
			continue
		}
		used := *window.Utilization
		if used < 0 || used > 100 {
			return Quota{Err: errors.New("invalid Claude quota utilization")}
		}
		if q.Remaining < 0 || 100-used < q.Remaining {
			q.Remaining, q.Window = 100-used, key
		}
	}
	return q
}
