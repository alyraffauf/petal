//go:build linux || darwin

// Package localdaemon starts and serves Petal's per-user Unix socket.
//
// Clients use Discover and Endpoint.DialOrStart with an absolute executable
// path and arguments for a dedicated daemon mode. That mode calls ServeStarted
// with a process shutdown context and a connection handler. The handler owns
// request framing and any application state associated with the connection.
package localdaemon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

const socketName = "daemon.sock"

// Endpoint identifies a private runtime directory and the socket inside it.
type Endpoint struct {
	dir string
}

// Discover creates or opens the endpoint. An explicit runtimeRoot must be an
// existing absolute directory. With an empty root, Discover uses
// XDG_RUNTIME_DIR if set and falls back to /tmp if it is unset or its socket
// path would exceed the platform limit.
func Discover(runtimeRoot string) (Endpoint, error) {
	return discover(runtimeRoot, "")
}

// DiscoverProfile isolates a daemon by its caller-defined profile identity.
func DiscoverProfile(runtimeRoot, identity string) (Endpoint, error) {
	if identity == "" {
		return Discover(runtimeRoot)
	}
	hash := sha256.Sum256([]byte(identity))
	return discover(runtimeRoot, fmt.Sprintf("-%x", hash[:8]))
}

func discover(runtimeRoot, suffix string) (Endpoint, error) {
	if runtimeRoot != "" {
		return openRoot(runtimeRoot, suffix)
	}
	if root := os.Getenv("XDG_RUNTIME_DIR"); root != "" {
		endpoint, err := openRoot(root, suffix)
		if !errors.Is(err, errSocketPathTooLong) {
			return endpoint, err
		}
	}
	return openRoot("/tmp", suffix)
}

var errSocketPathTooLong = errors.New("Unix socket path too long")

func openRoot(root, suffix string) (Endpoint, error) {
	if !filepath.IsAbs(root) {
		return Endpoint{}, fmt.Errorf("runtime root must be absolute: %q", root)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return Endpoint{}, err
	}
	endpoint := Endpoint{dir: filepath.Join(root, fmt.Sprintf("petal-%d", os.Geteuid())+suffix)}
	limit := len(unix.RawSockaddrUnix{}.Path) - 1
	if runtime.GOOS == "darwin" {
		// Darwin's bind implementation also counts the sockaddr header.
		limit = 103
	}
	if len(endpoint.SocketPath()) > limit {
		return Endpoint{}, fmt.Errorf("%w: %q", errSocketPathTooLong, endpoint.SocketPath())
	}
	if err := checkAncestors(root); err != nil {
		return Endpoint{}, err
	}
	if err := os.Mkdir(endpoint.dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return Endpoint{}, err
	}
	if err := endpoint.checkDirectory(); err != nil {
		return Endpoint{}, err
	}
	return endpoint, nil
}

func checkAncestors(path string) error {
	// Walk top down. A sticky shared directory protects entries owned by
	// root or the current user. Refuse paths through any other shared parent.
	var ancestors []string
	for {
		ancestors = append(ancestors, path)
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		if err := checkAncestor(ancestors[i]); err != nil {
			return err
		}
	}
	return nil
}

func checkAncestor(path string) error {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		(stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) ||
		(stat.Mode&0022 != 0 && stat.Mode&unix.S_ISVTX == 0) {
		return fmt.Errorf("unsafe runtime ancestor: %q", path)
	}
	return nil
}

func validDirectoryName(name string) bool {
	base := fmt.Sprintf("petal-%d", os.Geteuid())
	if name == base {
		return true
	}
	suffix, ok := strings.CutPrefix(name, base+"-")
	return ok && len(suffix) == 16 && strings.Trim(suffix, "0123456789abcdef") == ""
}

func (endpoint Endpoint) checkDirectory() error {
	if !filepath.IsAbs(endpoint.dir) || filepath.Clean(endpoint.dir) != endpoint.dir ||
		strings.IndexByte(endpoint.dir, 0) >= 0 ||
		!validDirectoryName(filepath.Base(endpoint.dir)) {
		return fmt.Errorf("invalid daemon directory: %q", endpoint.dir)
	}
	var stat unix.Stat_t
	if err := unix.Lstat(endpoint.dir, &stat); err != nil {
		return &os.PathError{Op: "lstat", Path: endpoint.dir, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) ||
		stat.Mode&07777 != 0700 {
		return fmt.Errorf("daemon directory must be owned by the current user with mode 0700: %q", endpoint.dir)
	}
	return nil
}

// SocketPath returns the filesystem address of the Unix socket.
func (endpoint Endpoint) SocketPath() string {
	return filepath.Join(endpoint.dir, socketName)
}

func (endpoint Endpoint) lock() (*os.File, error) {
	path := filepath.Join(endpoint.dir, "daemon.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if err := checkLock(file); err != nil {
		file.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func checkLock(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) ||
		stat.Mode&07777 != 0600 || stat.Nlink != 1 {
		return fmt.Errorf("daemon lock must be a private regular file with one link: %q", file.Name())
	}
	return nil
}
