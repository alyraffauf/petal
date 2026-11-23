//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"petal/internal/livesession"
	"petal/internal/localdaemon"
	"petal/internal/routing"
	"petal/internal/sessions"
)

func TestDaemonProfilesShareDurableLeaseThroughShutdownFlush(t *testing.T) {
	workspace := t.TempDir()
	base := ptyEnvironment(t, workspace, "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(workspace, "petal", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`acp_v2=true
delegation=false
[[accounts]]
name="fixture"
[[accounts.env]]
name="PETAL_PTY_ROLE"
value="live-fixture"
[[accounts.env]]
name="PETAL_PTY_ACP_VERSION"
value="2"
[[accounts.models]]
id="fixture"
command=[%q, "-test.run=^TestPTYChild$"]
`, exe)
	if err := os.WriteFile(config, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	first, endpoint, pidPath := dialProfileFixture(t, ctx, base, "", "first")
	explicit, _, _ := dialProfileFixture(t, ctx, base, config, "explicit")
	if explicit.Instance() != first.Instance() {
		t.Fatal("implicit and explicit default configuration launched separate daemons")
	}

	// Another PATH can discover a different adapter while still using the same
	// config and archive. The two daemon processes must contend for the route.
	otherPath := t.TempDir()
	bridge := fmt.Sprintf("#!/bin/sh\nPETAL_PTY_ROLE=live-fixture PETAL_PTY_ACP_VERSION=2 exec %q -test.run=^TestPTYChild$\n", exe)
	if err := os.WriteFile(filepath.Join(otherPath, "claude-agent-acp"), []byte(bridge), 0700); err != nil {
		t.Fatal(err)
	}
	otherEnv := append(append([]string(nil), base...), "PATH="+otherPath)
	other, _, _ := dialProfileFixture(t, ctx, otherEnv, "", "other-path")
	if other.Instance() == first.Instance() {
		t.Fatal("different adapter discovery PATH reused the original daemon")
	}

	discovered, _, _ := dialProfileFixture(t, ctx, otherEnv, "", "other-path")
	discovery := livesession.OpenOptions{Handle: livesession.NewHandle(), Cwd: workspace, Model: "claude/default"}
	if _, err := discovered.Open(ctx, discovery); err != nil {
		t.Fatal(err)
	}
	if _, err := discovered.Attach(ctx, livesession.AttachOptions{Handle: discovery.Handle, Control: true}); err != nil {
		t.Fatal(err)
	}
	if err := profileCall(t, ctx, discovered, 1, "initialize", ""); err != nil {
		t.Fatalf("daemon could not start adapter discovered in its client shell: %v", err)
	}
	if err := discovered.CloseSession(ctx); err != nil {
		t.Fatal(err)
	}

	const route = `["fixture","fixture","parent-session"]`
	resume := livesession.OpenOptions{Handle: livesession.NewHandle(), Cwd: workspace, Model: "fixture", ResumeID: route}
	if _, err := other.Open(ctx, resume); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Attach(ctx, livesession.AttachOptions{Handle: resume.Handle, Control: true}); err != nil {
		t.Fatal(err)
	}
	open := livesession.OpenOptions{Handle: livesession.NewHandle(), Cwd: workspace, Model: "fixture"}
	if _, err := first.Open(ctx, open); err != nil {
		t.Fatal(err)
	}
	snapshot, err := first.Attach(ctx, livesession.AttachOptions{Handle: open.Handle, Control: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := profileCall(t, ctx, first, 1, "initialize", ""); err != nil {
		t.Fatal(err)
	}
	requireDaemonCode(t, profileCall(t, ctx, first, 2, "session/open", ""), livesession.Conflict)
	if err := other.CloseSession(ctx); err != nil {
		t.Fatal(err)
	}
	retryProfileLease(t, ctx, func() error { return profileCall(t, ctx, first, 3, "session/open", "") })
	resume.Handle = livesession.NewHandle()
	_, err = explicit.Open(ctx, resume)
	requireDaemonCode(t, err, livesession.Conflict)
	_, err = other.Open(ctx, resume)
	requireDaemonCode(t, err, livesession.Conflict)

	isolatedEnv := append(append([]string(nil), base...), "XDG_STATE_HOME="+t.TempDir())
	isolated, _, _ := dialProfileFixture(t, ctx, isolatedEnv, "", "other-state")
	if isolated.Instance() == first.Instance() {
		t.Fatal("different XDG_STATE_HOME reused the original daemon")
	}
	if _, err := isolated.Open(ctx, resume); err != nil {
		t.Fatalf("separate archive could not reserve the same route: %v", err)
	}

	if err := profileCall(t, ctx, first, 4, "session/prompt", `{"prompt":[{"type":"text","text":"flush on shutdown"}]}`); err != nil {
		t.Fatal(err)
	}
	for cursor := snapshot.Head; ; {
		event, err := first.Next(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		cursor = event.Sequence
		if strings.Contains(string(event.Params), "pending archive tail") {
			break
		}
	}

	// Hold the archive directory's migration lock so shutdown reaches the
	// flush but cannot finish it. The listener disappearing is the barrier.
	archives, err := filepath.Glob(filepath.Join(workspace, "petal", "conversations", "*", "index.sqlite"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("workspace archives = %v, %v", archives, err)
	}
	lock, err := os.Open(filepath.Dir(archives[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil || pid <= 1 {
		t.Fatalf("daemon PID = %q, %v", pidBytes, err)
	}
	if err := unix.Kill(pid, unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for {
		conn, err := endpoint.Dial(ctx)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err == nil {
			conn.Close()
		} else if !errors.Is(err, unix.ECONNREFUSED) {
			t.Fatal(err)
		}
		waitProfileRetry(t, ctx)
	}
	if err := endpoint.Serve(ctx, func(context.Context, net.Conn) {}); !errors.Is(err, localdaemon.ErrRunning) {
		t.Fatalf("replacement endpoint acquired lock before shutdown flush: %v", err)
	}
	observer, _, _ := dialProfileFixture(t, ctx, otherEnv, "", "other-path")
	_, err = observer.Open(ctx, resume)
	requireDaemonCode(t, err, livesession.Conflict)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	retryProfileLease(t, ctx, func() error {
		_, err := observer.Open(ctx, resume)
		return err
	})

	t.Setenv("XDG_STATE_HOME", workspace)
	rows, err := sessions.Transcript(workspace, route)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Role == "assistant" && row.Body == "pending archive tail" {
			return
		}
	}
	t.Fatalf("replacement acquired lease before transcript was flushed: %+v", rows)
}

func dialProfileFixture(t *testing.T, ctx context.Context, env []string, configPath, label string) (*livesession.Client, localdaemon.Endpoint, string) {
	t.Helper()
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "PATH", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "TMPDIR", "PETAL_PTY_ROLE":
			t.Setenv(key, value)
		}
	}
	pidPath := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), label+".pid")
	t.Setenv("PETAL_PTY_PID", pidPath)
	config, err := routing.Load(configPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	dial, err := daemonDialer(configPath, config)
	if err != nil {
		t.Fatal(err)
	}
	// Register cleanup before dialing in case the subprocess starts but fails
	// to answer the startup handshake.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := fixtureEndpoint(os.Environ(), configPath, cwd)
	if err != nil {
		t.Fatal(err)
	}
	cleanupPTYDaemon(t, pidPath)
	client, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client, endpoint, pidPath
}

func profileCall(t *testing.T, ctx context.Context, client *livesession.Client, operation uint64, method, params string) error {
	t.Helper()
	if _, err := client.Submit(ctx, livesession.Call{Operation: operation, Method: method, Params: []byte(params)}); err != nil {
		t.Fatal(err)
	}
	receipt, err := client.WaitResult(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	return receipt.Err()
}

func requireDaemonCode(t *testing.T, err error, code livesession.Code) {
	t.Helper()
	var protocolErr *livesession.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func retryProfileLease(t *testing.T, ctx context.Context, reserve func() error) {
	t.Helper()
	for {
		err := reserve()
		if err == nil {
			return
		}
		requireDaemonCode(t, err, livesession.Conflict)
		waitProfileRetry(t, ctx)
	}
}

func waitProfileRetry(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(10 * time.Millisecond):
	}
}

func TestMCPHeaderEnvironmentSelectsDaemon(t *testing.T) {
	const key = "PETAL_TEST_HTTP_HEADER"
	servers := []routing.MCPServer{{Headers: []routing.MCPHeader{{FromEnv: key}}}}
	root, err := os.MkdirTemp("/tmp", "petal-profile-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	socket := func() string {
		t.Helper()
		var profile daemonProfile
		profile.includeMCPEnvironment(servers)
		endpoint, err := profile.endpoint(root)
		if err != nil {
			t.Fatal(err)
		}
		return endpoint.SocketPath()
	}
	t.Setenv(key, "first-test-credential")
	first := socket()
	t.Setenv(key, "second-test-credential")
	second := socket()
	if first == second || strings.Contains(second, "second-test-credential") {
		t.Fatal("credential rotation did not select a distinct opaque socket path")
	}
	t.Setenv(key, "")
	empty := socket()
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	if socket() == empty {
		t.Fatal("absent and empty credentials shared a socket")
	}
}
