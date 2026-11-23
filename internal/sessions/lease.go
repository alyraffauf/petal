//go:build linux || darwin

package sessions

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrRouteLeased means a live daemon session already owns this archive route.
var ErrRouteLeased = errors.New("durable session already has a live owner")

// LeaseRoute reserves route in the same workspace archive used for transcripts.
// Close releases the lease. Keep the lock file in place so a new owner always
// locks the same inode, including after a daemon crash.
func LeaseRoute(cwd, route string) (*os.File, error) {
	dir, err := storeDir(cwd)
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "leases")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, key(route)+".lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &os.PathError{Op: "lock", Path: path, Err: unix.EINVAL}
	}
	if err == nil {
		for {
			err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
	}
	if err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrRouteLeased
		}
		return nil, err
	}
	return file, nil
}
