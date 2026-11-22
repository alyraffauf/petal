//go:build linux || darwin

package localdaemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// ErrRunning means another process holds the endpoint or is listening on it.
var ErrRunning = errors.New("daemon already running or starting")

// Handler handles a single connection. Serve calls handlers concurrently and
// closes each connection when its handler returns. On shutdown Serve cancels
// ctx and closes the connection. A handler must return when canceled.
type Handler func(ctx context.Context, conn net.Conn)

// ServeStarted serves the endpoint passed by DialOrStart. Call it once in the
// dedicated subprocess. The caller supplies the shutdown context, typically
// from signal.NotifyContext for SIGTERM and SIGINT.
func ServeStarted(ctx context.Context, handler Handler) error {
	dir, ok := os.LookupEnv(directoryEnv)
	if !ok {
		return errors.New("missing daemon launch environment")
	}
	os.Unsetenv(directoryEnv)
	lock := os.NewFile(lockFD, filepath.Join(dir, "daemon.lock"))
	defer lock.Close()
	unix.CloseOnExec(lockFD)
	endpoint := Endpoint{dir: dir}
	if err := checkAncestors(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := endpoint.checkDirectory(); err != nil {
		return err
	}
	if err := checkLock(lock); err != nil {
		return err
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := os.Lstat(lock.Name())
	if err != nil {
		return err
	}
	if !os.SameFile(lockInfo, pathInfo) {
		return errors.New("inherited daemon lock does not match endpoint")
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	return endpoint.serve(ctx, handler)
}

// Serve runs a daemon in the current process until ctx is canceled or the
// listener fails. Only the process holding the endpoint lock may bind it.
func (endpoint Endpoint) Serve(ctx context.Context, handler Handler) error {
	if err := endpoint.checkDirectory(); err != nil {
		return err
	}
	lock, err := endpoint.lock()
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrRunning
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	return endpoint.serve(ctx, handler)
}

func (endpoint Endpoint) serve(ctx context.Context, handler Handler) error {
	if handler == nil {
		return errors.New("missing daemon connection handler")
	}
	listener, err := endpoint.listen(ctx)
	if err != nil {
		return err
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(ctx)
	stopAccept := context.AfterFunc(ctx, func() { listener.Close() })
	defer stopAccept()

	var handlers sync.WaitGroup
	defer func() {
		cancel()
		handlers.Wait()
	}()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := checkPeer(conn); err != nil {
			conn.Close()
			continue
		}
		handlers.Go(func() {
			defer conn.Close()
			stopRead := context.AfterFunc(ctx, func() { conn.Close() })
			defer stopRead()
			handler(ctx, conn)
		})
	}
}

// listen runs with the endpoint lock held. Only a refused connection to a
// socket owned by this user permits unlinking a stale address.
func (endpoint Endpoint) listen(ctx context.Context) (*net.UnixListener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := endpoint.Dial(ctx)
	switch {
	case err == nil:
		conn.Close()
		return nil, ErrRunning
	case errors.Is(err, unix.ECONNREFUSED):
		if err := os.Remove(endpoint.SocketPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	path := endpoint.SocketPath()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
