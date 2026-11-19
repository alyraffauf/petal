package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
)

func (a account) codexQuota(ctx context.Context) (float64, error) {
	cmd := a.command(ctx, []string{executable(a.Env["CODEX_PATH"], "codex"), "app-server", "--listen", "stdio://"})
	in, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err = cmd.Start(); err != nil {
		return 0, err
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
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
		return 0, err
	}
	if _, err = read(1); err != nil {
		return 0, err
	}
	if _, err = io.WriteString(in, `{"method":"initialized"}
{"id":2,"method":"account/rateLimits/read","params":{}}
`); err != nil {
		return 0, err
	}
	data, err := read(2)
	if err != nil {
		return 0, err
	}
	return codexRemaining(data)
}

func codexRemaining(data json.RawMessage) (float64, error) {
	type window struct{ UsedPercent *float64 }
	var r struct {
		OrdinaryUsageAllowed *bool
		RateLimits           struct{ Primary, Secondary *window }
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return 0, err
	}
	if r.OrdinaryUsageAllowed != nil && !*r.OrdinaryUsageAllowed {
		return 0, nil
	}
	available := 101.0
	for _, w := range []*window{r.RateLimits.Primary, r.RateLimits.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		if *w.UsedPercent < 0 || *w.UsedPercent > 100 {
			return 0, errors.New("invalid Codex used percentage")
		}
		available = min(available, 100-*w.UsedPercent)
	}
	if available == 101 {
		return -1, nil
	}
	return available, nil
}
