package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMCPLoginEnablesOnlyConnectedServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("PATH", dir)
	for name, script := range map[string]string{
		"mcp-remote":        "#!/bin/sh\nexit 0\n",
		"mcp-remote-client": "#!/bin/sh\ntest -d \"$MCP_REMOTE_CONFIG_DIR\" && test \"$1\" = https://example.org/mcp && exit \"$LOGIN_EXIT\"\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	server := MCPServer{Name: "work", URL: "https://example.org/mcp"}
	t.Setenv("LOGIN_EXIT", "1")
	if err := server.Login(t.Context()); err == nil {
		t.Fatal("failed connection accepted")
	}
	if _, ready, err := server.SessionServer(); ready || err != nil {
		t.Fatalf("failed login: ready=%v err=%v", ready, err)
	}
	t.Setenv("LOGIN_EXIT", "0")
	if err := server.Login(t.Context()); err != nil {
		t.Fatal(err)
	}
	bridge, ready, err := server.SessionServer()
	if err != nil || !ready || bridge.Command != filepath.Join(dir, "mcp-remote") || bridge.Args[0] != server.URL || bridge.Env[0].Name != "MCP_REMOTE_CONFIG_DIR" {
		t.Fatalf("after login: bridge=%+v ready=%v err=%v", bridge, ready, err)
	}
	server.URL += "/different"
	if _, ready, err := server.SessionServer(); ready || err != nil {
		t.Fatalf("changed endpoint: ready=%v err=%v", ready, err)
	}
}
