//go:build linux || darwin

package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func runtimeRoot(t *testing.T) string {
	t.Helper()
	// Nix can map only the build user into its Linux sandbox. In that
	// namespace / and /tmp have an unmapped owner, so discovery must fail.
	if runtime.GOOS == "linux" && os.Getenv("NIX_BUILD_TOP") != "" {
		var rootStat unix.Stat_t
		if err := unix.Stat("/", &rootStat); err != nil {
			t.Fatal(err)
		}
		if rootStat.Uid != 0 && rootStat.Uid != uint32(os.Geteuid()) {
			if _, err := Discover("/tmp"); err == nil {
				t.Fatal("accepted a runtime path beneath an untrusted root")
			}
			t.Skip("daemon startup requires a trusted runtime path; CI also runs this suite outside the Nix sandbox")
		}
	}
	root, err := os.MkdirTemp("/tmp", "petal-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}

func TestColdStartAcrossProcessesAndRecoverAfterKill(t *testing.T) {
	root := runtimeRoot(t)
	endpoint, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	record := filepath.Join(root, "launches")

	const clients = 8
	results := make(chan result, clients)
	for range clients {
		go func() {
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestDaemonHelper$", "--", "client", root)
			output, err := cmd.CombinedOutput()
			results <- result{string(output), err}
		}()
	}
	var firstPID int
	for range clients {
		got := <-results
		if got.err != nil {
			t.Errorf("launcher: %v\n%s", got.err, got.output)
			continue
		}
		pid, sid := readIdentity(t, got.output)
		if pid != sid {
			t.Errorf("daemon PID %d has session ID %d", pid, sid)
		}
		if firstPID == 0 {
			firstPID = pid
		}
		if pid != firstPID {
			t.Errorf("client reached daemon %d, want %d", pid, firstPID)
		}
	}
	if t.Failed() {
		return
	}
	assertModes(t, endpoint)
	assertLaunches(t, record, firstPID)

	// Every launcher has exited. The session leader still accepts requests.
	conn, err := endpoint.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := readIdentity(t, exchange(t, conn))
	if pid != firstPID {
		t.Fatalf("daemon changed after launchers exited: %d", pid)
	}
	if err := unix.Kill(firstPID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, func() bool {
		conn, err := endpoint.Dial(ctx)
		if err == nil {
			conn.Close()
		}
		return errors.Is(err, unix.ECONNREFUSED)
	})

	conn, err = endpoint.DialOrStart(ctx, daemonCommand(executable, root))
	if err != nil {
		t.Fatal(err)
	}
	secondPID, _ := readIdentity(t, exchange(t, conn))
	if secondPID == firstPID {
		t.Fatal("stale endpoint still reports the first daemon")
	}
	assertLaunches(t, record, firstPID, secondPID)

	if err := unix.Kill(secondPID, unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, func() bool {
		_, err := os.Lstat(endpoint.SocketPath())
		return errors.Is(err, os.ErrNotExist)
	})
}

type result struct {
	output string
	err    error
}

func daemonCommand(executable, root string) Command {
	return Command{Path: executable, Args: []string{"-test.run=^TestDaemonHelper$", "--", "daemon", root}}
}

func TestDaemonHelper(t *testing.T) {
	if len(os.Args) < 5 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	role, root := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	switch role {
	case "client":
		endpoint, err := Discover(root)
		if err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		conn, err := endpoint.DialOrStart(ctx, daemonCommand(executable, root))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Print(exchange(t, conn))
	case "daemon":
		record, err := os.OpenFile(filepath.Join(root, "launches"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintln(record, os.Getpid())
		record.Close()
		if err != nil {
			t.Fatal(err)
		}
		sid, err := unix.Getsid(0)
		if err != nil {
			t.Fatal(err)
		}
		lifetime, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		ctx, stop := signal.NotifyContext(lifetime, unix.SIGTERM, unix.SIGINT)
		defer stop()
		err = ServeStarted(ctx, func(_ context.Context, conn net.Conn) {
			var request [1]byte
			if _, err := conn.Read(request[:]); err == nil {
				fmt.Fprintf(conn, "identity %d %d\n", os.Getpid(), sid)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown helper role %q", role)
	}
}

func exchange(t *testing.T, conn net.Conn) string {
	t.Helper()
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if _, err := io.Copy(&output, conn); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func readIdentity(t *testing.T, output string) (pid, sid int) {
	t.Helper()
	if _, err := fmt.Sscanf(output, "identity %d %d", &pid, &sid); err != nil || pid <= 0 {
		t.Fatalf("invalid daemon response %q", output)
	}
	return pid, sid
}

func assertLaunches(t *testing.T, record string, pids ...int) {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, pid := range pids {
		fmt.Fprintln(&want, pid)
	}
	if string(data) != want.String() {
		t.Fatalf("launches %q, want %q", data, want.String())
	}
}

func assertModes(t *testing.T, endpoint Endpoint) {
	t.Helper()
	for path, want := range map[string]os.FileMode{
		filepath.Dir(endpoint.SocketPath()):                               0700,
		endpoint.SocketPath():                                             0600,
		filepath.Join(filepath.Dir(endpoint.SocketPath()), "daemon.lock"): 0600,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func waitFor(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestServeCancelsActiveConnections(t *testing.T) {
	endpoint, err := Discover(runtimeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serving := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- endpoint.Serve(ctx, func(_ context.Context, conn net.Conn) {
			close(serving)
			var input [1]byte
			conn.Read(input[:])
		})
	}()
	deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	var conn net.Conn
	waitFor(t, deadline, func() bool {
		conn, err = endpoint.Dial(deadline)
		return err == nil
	})
	defer conn.Close()
	select {
	case <-serving:
	case <-deadline.Done():
		t.Fatal(deadline.Err())
	}
	if err := endpoint.Serve(deadline, func(context.Context, net.Conn) {}); !errors.Is(err, ErrRunning) {
		t.Fatalf("second server: %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline.Done():
		t.Fatal(deadline.Err())
	}
	if _, err := os.Lstat(endpoint.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after shutdown: %v", err)
	}
}

func TestServePreservesLiveSocketWithoutLock(t *testing.T) {
	endpoint, err := Discover(runtimeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", endpoint.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := endpoint.Serve(ctx, func(context.Context, net.Conn) {}); !errors.Is(err, ErrRunning) {
		t.Fatalf("serve over a live socket: %v", err)
	}
	conn, err := endpoint.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestUnsafeEndpointEntriesArePreserved(t *testing.T) {
	for _, entry := range []string{"directory symlink", "shared directory", "socket symlink", "socket regular file", "lock symlink", "lock hardlink"} {
		t.Run(entry, func(t *testing.T) {
			root := runtimeRoot(t)
			endpoint, err := Discover(root)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(endpoint.SocketPath())
			target := filepath.Join(root, "sentinel")
			if err := os.WriteFile(target, []byte("sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			var path string
			switch entry {
			case "directory symlink":
				path = dir
				if err = os.Remove(dir); err == nil {
					err = os.Symlink(root, dir)
				}
			case "shared directory":
				path = dir
				err = os.Chmod(dir, 0755)
			case "socket symlink":
				path = endpoint.SocketPath()
				err = os.Symlink(target, path)
			case "socket regular file":
				path = endpoint.SocketPath()
				err = os.WriteFile(path, []byte("occupied"), 0600)
			case "lock symlink":
				path = filepath.Join(dir, "daemon.lock")
				err = os.Symlink(target, path)
			case "lock hardlink":
				path = filepath.Join(dir, "daemon.lock")
				err = os.Link(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := endpoint.Serve(ctx, func(context.Context, net.Conn) {}); err == nil {
				t.Fatal("server accepted an unsafe endpoint entry")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("endpoint entry changed: %v", err)
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "sentinel" {
				t.Fatalf("sentinel changed: %q, %v", data, err)
			}
		})
	}
}

func TestDiscoverUsesRuntimeRootAndRejectsUnsafeAncestor(t *testing.T) {
	root := runtimeRoot(t)
	t.Setenv("XDG_RUNTIME_DIR", root)
	endpoint, err := Discover("")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := Discover(root)
	if err != nil || explicit != endpoint {
		t.Fatalf("explicit endpoint = %v, %v, want %v", explicit, err, endpoint)
	}
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); err == nil {
		t.Fatal("discovery accepted a shared non-sticky ancestor")
	}
}

func TestSocketPathLength(t *testing.T) {
	root := runtimeRoot(t)
	limit := 107
	if runtime.GOOS == "darwin" {
		limit = 103
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("/petal-%d/daemon.sock", os.Geteuid())
	length := limit - len(canonical) - len(suffix) - 1
	for _, size := range []int{length, length + 1} {
		dir := filepath.Join(root, strings.Repeat("x", size))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		endpoint, err := Discover(dir)
		if size > length {
			if !errors.Is(err, errSocketPathTooLong) {
				t.Fatalf("long socket path: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", endpoint.SocketPath())
		if err != nil {
			t.Fatal(err)
		}
		listener.Close()
	}
}
