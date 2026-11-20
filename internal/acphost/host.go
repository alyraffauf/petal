// Package acphost implements the workspace services offered by Petal's ACP client.
package acphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"
)

type Host struct {
	root      *os.Root
	ctx       context.Context
	cancel    context.CancelFunc
	approve   func(context.Context, string) error
	mu        sync.Mutex
	next      int
	terminals map[string]*terminal
}

func Open(ctx context.Context, cwd string, approve func(context.Context, string) error) (*Host, error) {
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Host{root: root, ctx: ctx, cancel: cancel, approve: approve, terminals: map[string]*terminal{}}, nil
}
func (h *Host) Close() {
	h.cancel()
	h.root.Close()
}
func (h *Host) path(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", acp.NewInvalidParams("expected an absolute workspace path")
	}
	rel, err := filepath.Rel(h.root.Name(), path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", acp.NewInvalidParams("path outside the workspace")
	}
	return rel, nil
}

func (h *Host) Call(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "fs/read_text_file":
		var p acp.ReadTextFileRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		path, err := h.path(p.Path)
		if err != nil {
			return nil, err
		}
		return h.readText(ctx, path, p)
	case "fs/write_text_file":
		var p acp.WriteTextFileRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		path, err := h.path(p.Path)
		if err != nil {
			return nil, err
		}
		if err = h.approve(ctx, "Write "+p.Path); err != nil {
			return nil, err
		}
		if err = h.root.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		return struct{}{}, h.root.WriteFile(path, []byte(p.Content), 0644)
	case "terminal/create":
		var p acp.CreateTerminalRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		if p.Command == "" {
			return nil, acp.NewInvalidParams("missing terminal command")
		}
		dir := h.root.Name()
		if p.Cwd != nil {
			if _, err := h.path(*p.Cwd); err != nil {
				return nil, err
			}
			dir = *p.Cwd
		}
		limit := int64(1 << 20)
		if p.OutputByteLimit != nil {
			limit = *p.OutputByteLimit
			if limit < 0 {
				return nil, acp.NewInvalidParams("invalid output limit")
			}
		}
		argv, _ := json.Marshal(append([]string{p.Command}, p.Args...))
		action := fmt.Sprintf("Run %s in %s", argv, dir)
		if len(p.Env) > 0 {
			overrides, _ := json.Marshal(p.Env)
			action += "\n\nEnvironment overrides: " + string(overrides)
		}
		if err := h.approve(ctx, action); err != nil {
			return nil, err
		}
		if p.Cwd != nil {
			rel, err := h.path(*p.Cwd)
			if err != nil {
				return nil, err
			}
			root, err := h.root.OpenRoot(rel)
			if err != nil {
				return nil, err
			}
			root.Close()
		}
		cmd := exec.CommandContext(h.ctx, p.Command, p.Args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		for _, e := range p.Env {
			cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		t := &terminal{cmd: cmd, session: string(p.SessionId), limit: limit, done: make(chan struct{})}
		cmd.Stdout = t
		cmd.Stderr = t
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.next++
		id := strconv.Itoa(h.next)
		h.terminals[id] = t
		h.mu.Unlock()
		go t.wait()
		return acp.CreateTerminalResponse{TerminalId: acp.TerminalId(id)}, nil
	case "terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		var p struct{ TerminalID, SessionID string }
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		h.mu.Lock()
		t := h.terminals[p.TerminalID]
		if t == nil || t.session != p.SessionID {
			h.mu.Unlock()
			return nil, acp.NewRequestError(-32002, "Terminal not found", nil)
		}
		if method == "terminal/release" {
			delete(h.terminals, p.TerminalID)
		}
		h.mu.Unlock()
		switch method {
		case "terminal/output":
			return t.snapshot(), nil
		case "terminal/wait_for_exit":
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-t.done:
				return t.exit, nil
			}
		default:
			select {
			case <-t.done:
				return struct{}{}, nil
			default:
			}
			err := t.cmd.Cancel()
			if errors.Is(err, syscall.ESRCH) {
				err = nil
			}
			return struct{}{}, err
		}
	}
	return nil, acp.NewMethodNotFound(method)
}

// OutputView retains the display of a terminal embedded in a tool call.
// Releasing its ACP handle prevents further protocol calls while the UI
// can still read the final output.
func (h *Host) OutputView(session, id string) func() acp.TerminalOutputResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.terminals[id]
	if t == nil || t.session != session {
		return nil
	}
	return t.snapshot
}

func (t *terminal) snapshot() acp.TerminalOutputResponse {
	t.mu.Lock()
	defer t.mu.Unlock()
	return acp.TerminalOutputResponse{Output: t.output(), Truncated: t.truncated, ExitStatus: t.exit}
}

type terminal struct {
	cmd       *exec.Cmd
	session   string
	limit     int64
	done      chan struct{}
	mu        sync.Mutex
	buf       []byte
	start     int
	truncated bool
	exit      *acp.TerminalExitStatus
}

func (t *terminal) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(data)
	if int64(n) <= t.limit-int64(len(t.buf)) {
		t.buf = append(t.buf, data...)
		return n, nil
	}

	t.truncated = true
	if int64(n) >= t.limit {
		t.buf = append(t.buf[:0], data[n-int(t.limit):]...)
		t.start = 0
		return n, nil
	}

	// Fill the remaining capacity, then overwrite the oldest bytes in place.
	free := int(t.limit - int64(len(t.buf)))
	t.buf = append(t.buf, data[:free]...)
	data = data[free:]
	copied := copy(t.buf[t.start:], data)
	copy(t.buf, data[copied:])
	t.start = (t.start + len(data)) % len(t.buf)
	return n, nil
}

// output runs under t.mu. Drop a partial leading UTF-8 sequence at the read
// boundary, including when its bytes straddle the end of the ring.
func (t *terminal) output() string {
	s := string(t.buf[t.start:]) + string(t.buf[:t.start])
	if t.truncated {
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return strings.ToValidUTF8(s, "�")
}
func (t *terminal) wait() {
	_ = t.cmd.Wait()
	status := &acp.TerminalExitStatus{}
	if s, ok := t.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && s.Signaled() {
		signal := s.Signal().String()
		status.Signal = &signal
	} else {
		code := int64(t.cmd.ProcessState.ExitCode())
		status.ExitCode = &code
	}
	t.mu.Lock()
	t.exit = status
	t.mu.Unlock()
	close(t.done)
}
