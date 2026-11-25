package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type herdrStatus struct{ state, id, cwd string }

func (m *terminal) herdrStatus() herdrStatus {
	state := "idle"
	switch {
	case m.ask != nil || len(m.urls) > 0 || (m.v2 != nil && m.v2.foreground.state == "requires_action"):
		state = "blocked"
	case m.connection != connected || (m.v2 != nil && m.v2.foreground.state == "unknown"):
		state = "unknown"
	case m.Busy:
		state = "working"
	}
	return herdrStatus{state: state, id: m.draftSessionID, cwd: m.Cwd}
}

type herdrReporter struct {
	pending chan herdrStatus
	done    chan struct{}
	last    herdrStatus
}

func newHerdrReporter(config string) *herdrReporter {
	pane := os.Getenv("HERDR_PANE_ID")
	if pane == "" {
		return nil
	}
	binary, err := exec.LookPath("herdr")
	if err != nil {
		return nil
	}
	if config != "" {
		config, err = filepath.Abs(config)
		if err != nil {
			return nil
		}
	}
	r := &herdrReporter{pending: make(chan herdrStatus, 1), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		run := func(args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return exec.CommandContext(ctx, binary, args...).Run()
		}
		for status := range r.pending {
			args := []string{"pane", "report-agent", pane, "--source", "custom:petal", "--agent", "petal", "--state", status.state}
			stateOnly := args
			if status.id != "" {
				args = append(args, "--agent-session-id", status.id, "--", "petal", "--directory", status.cwd, "--session", status.id)
				if config != "" {
					args = append(args, "--config", config)
				}
			}
			if err := run(args); err != nil && status.id != "" {
				// Herdr rejects the whole report when it cannot accept the resume command.
				run(stateOnly)
			}
		}
		run([]string{"pane", "release-agent", pane, "--source", "custom:petal", "--agent", "petal"})
	}()
	return r
}

// The UI is the sole publisher. Keep only the latest unsent update.
func (r *herdrReporter) publish(status herdrStatus) {
	if r.last == status {
		return
	}
	r.last = status
	select {
	case <-r.pending:
	default:
	}
	r.pending <- status
}

func (r *herdrReporter) close() { close(r.pending); <-r.done }
