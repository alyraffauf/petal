// Package processgroup owns the lifetime of a subprocess and its process group.
package processgroup

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

type Group struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	closed bool
}

// Start runs a command created with exec.CommandContext in its own process group.
func Start(cmd *exec.Cmd) (*Group, error) {
	group := &Group{cmd: cmd}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = group.Stop
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return group, nil
}

func (g *Group) Stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stop()
}

func (g *Group) stop() error {
	if g.closed {
		return nil
	}
	err := syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	g.closed = true
	return nil
}

func (g *Group) Wait() error {
	observeExit(g.cmd.Process.Pid)
	// The unreaped leader still reserves the numeric group ID. Disallow
	// further signals before Wait reaps it, even if the signal failed.
	g.mu.Lock()
	stopErr := g.stop()
	g.closed = true
	g.mu.Unlock()
	return errors.Join(g.cmd.Wait(), stopErr)
}
