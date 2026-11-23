//go:build linux || darwin

package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const directoryEnv = "PETAL_DAEMON_DIRECTORY"
const lockFD = 3

// Command describes a dedicated daemon subprocess. Path must be absolute.
// When the command starts, it must call ServeStarted. Standard streams go to
// the null device and the working directory defaults to /. Configure logging inside
// the daemon if needed.
type Command struct {
	Path string
	Args []string
	Dir  string
}

// Dial connects to an existing daemon. The caller owns the returned connection.
func (endpoint Endpoint) Dial(ctx context.Context) (net.Conn, error) {
	if err := endpoint.checkDirectory(); err != nil {
		return nil, err
	}
	path := endpoint.SocketPath()
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("unsafe daemon socket: %q", path)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := checkPeer(conn.(*net.UnixConn)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// DialOrStart connects to the daemon or starts it once and waits until the
// socket accepts connections. Give ctx a deadline to bound startup. Canceling
// ctx stops the wait and leaves a started daemon running.
func (endpoint Endpoint) DialOrStart(ctx context.Context, command Command) (net.Conn, error) {
	var exited <-chan error
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := endpoint.Dial(ctx)
		if err == nil || !socketUnavailable(err) {
			return conn, err
		}
		if exited == nil {
			exited, err = endpoint.start(ctx, command)
			if err != nil {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-exited:
			if err == nil {
				err = errors.New("process exited")
			}
			return nil, fmt.Errorf("daemon exited while connecting: %w", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func socketUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ECONNREFUSED)
}

func (endpoint Endpoint) start(ctx context.Context, command Command) (<-chan error, error) {
	if err := endpoint.checkDirectory(); err != nil {
		return nil, err
	}
	lock, err := endpoint.lock()
	if errors.Is(err, unix.EWOULDBLOCK) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Close our copy without unlocking the open file description handed off
	// to the daemon. The lock survives cancellation and the launching process.
	defer lock.Close()

	if !filepath.IsAbs(command.Path) {
		return nil, fmt.Errorf("daemon command must be absolute: %q", command.Path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(command.Path, command.Args...)
	cmd.Dir = command.Dir
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, directoryEnv+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, directoryEnv+"="+endpoint.dir)
	cmd.ExtraFiles = []*os.File{lock}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
	}()
	return exited, nil
}
