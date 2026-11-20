package routing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
)

func (a Account) codexQuota(ctx context.Context) (json.RawMessage, error) {
	cmd := a.Command(ctx, []string{providerCLI("codex", a.Env), "app-server", "--listen", "stdio://"})
	in, out, cleanup, err := startQuotaProcess(ctx, cmd)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 4<<20)
	read := func(id int) (json.RawMessage, error) {
		for scan.Scan() {
			var r struct {
				ID     int
				Result json.RawMessage
				Error  json.RawMessage
			}
			if json.Unmarshal(scan.Bytes(), &r) == nil && r.ID == id {
				if len(r.Error) > 0 {
					return nil, errors.New("Codex quota request rejected")
				}
				return r.Result, nil
			}
		}
		if err := scan.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	if _, err = io.WriteString(in, `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"petal","version":"1"}}}`+"\n"); err != nil {
		return nil, err
	}
	if _, err = read(1); err != nil {
		return nil, err
	}
	if _, err = io.WriteString(in, `{"method":"initialized"}
{"id":2,"method":"account/rateLimits/read","params":{}}
`); err != nil {
		return nil, err
	}
	return read(2)
}

func codexRemaining(data json.RawMessage) (float64, error) {
	q := codexQuotaWindow(data)
	return q.Remaining, q.Err
}

func codexQuotaWindow(data json.RawMessage) Quota {
	type window struct{ UsedPercent *float64 }
	var r struct {
		OrdinaryUsageAllowed *bool
		RateLimits           struct{ Primary, Secondary *window }
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Quota{Err: err}
	}
	if r.OrdinaryUsageAllowed != nil && !*r.OrdinaryUsageAllowed {
		return Quota{}
	}
	q := Quota{Remaining: -1}
	for i, w := range []*window{r.RateLimits.Primary, r.RateLimits.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		if *w.UsedPercent < 0 || *w.UsedPercent > 100 {
			return Quota{Err: errors.New("invalid Codex used percentage")}
		}
		if remaining := 100 - *w.UsedPercent; q.Remaining < 0 || remaining < q.Remaining {
			q.Remaining, q.Window = remaining, []string{"Primary", "Secondary"}[i]
		}
	}
	return q
}
