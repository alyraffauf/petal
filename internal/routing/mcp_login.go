package routing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"petal/internal/paths"
)

func (s MCPServer) loginDirectory() (string, error) {
	key := sha256.Sum256([]byte(s.Name + "\x00" + s.URL))
	return paths.XDG("STATE", fmt.Sprintf("mcp/%x", key))
}

func (s MCPServer) Login(ctx context.Context) error {
	if s.URL == "" || len(s.Headers) != 0 {
		return fmt.Errorf("%q uses stdio or configured headers; browser login applies to HTTP servers without headers", s.Name)
	}
	endpoint, err := url.Parse(s.URL)
	if err != nil {
		return err
	}
	host := endpoint.Hostname()
	if endpoint.Scheme == "http" && !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("MCP login for %q requires HTTPS or a loopback HTTP endpoint", s.Name)
	}
	dir, err := s.loginDirectory()
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("mcp-remote"); err != nil {
		return fmt.Errorf("install mcp-remote to log in: %w", err)
	}
	client, err := exec.LookPath("mcp-remote-client")
	if err != nil {
		return fmt.Errorf("install mcp-remote to log in: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	marker := filepath.Join(dir, "connected")
	// Invalidate before connecting so a failed attempt cannot erase a concurrent success.
	if err := syscall.Unlink(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear saved MCP login: %w", err)
	}
	cmd := exec.CommandContext(ctx, client, s.URL, "--transport", "http-only", "--auth-timeout", "300")
	cmd.Env = append(os.Environ(), "MCP_REMOTE_CONFIG_DIR="+dir)
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	if err := runMCPLogin(cmd); err != nil {
		return fmt.Errorf("connect to %q: %w", s.Name, err)
	}
	return os.WriteFile(marker, nil, 0600)
}

// mcp-remote 0.16.7 can receive a denial before it subscribes to the
// callback. In that order it logs the denial and then waits for a second callback.
func runMCPLogin(cmd *exec.Cmd) error {
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	reader := bufio.NewReader(io.TeeReader(stderr, os.Stderr))
	prefix := []byte(fmt.Sprintf("[%d] ", cmd.Process.Pid))
	denied := false
	startOfLine := true
	for {
		line, readErr := reader.ReadSlice('\n')
		if startOfLine && bytes.HasPrefix(line, prefix) {
			message := line[len(prefix):]
			if bytes.HasPrefix(message, []byte("Authorization failed: access_denied - ")) {
				denied = true
			}
			if denied && bytes.Equal(bytes.TrimSpace(message), []byte("Authentication required. Waiting for authorization...")) {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return errors.New("OAuth authorization denied")
			}
		}
		startOfLine = readErr != bufio.ErrBufferFull
		if readErr != nil && startOfLine {
			break
		}
	}
	return cmd.Wait()
}

// SessionServer selects the transport after editor overrides have been applied.
func (s MCPServer) SessionServer() (MCPServer, bool, error) {
	if s.URL == "" || len(s.Headers) != 0 {
		return s, true, nil
	}
	dir, err := s.loginDirectory()
	if err != nil {
		return s, false, err
	}
	marker, err := os.Lstat(filepath.Join(dir, "connected"))
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if !marker.Mode().IsRegular() {
		return s, false, nil
	}
	command, err := exec.LookPath("mcp-remote")
	if errors.Is(err, exec.ErrNotFound) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	return MCPServer{Name: s.Name, Command: command, Args: []string{s.URL, "--transport", "http-only"}, Env: []MCPEnvVariable{{Name: "MCP_REMOTE_CONFIG_DIR", Value: dir}}}, true, nil
}
