package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadMCPServers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, "config.toml")
	const settings = `
[[mcp_servers]]
name = "ProjectTools"
command = "/path with spaces/tool"
args = ["--root", "A B", ""]
[[mcp_servers.env]]
name = "PROJECT_MODE"
value = "Local=On"

[[mcp_servers]]
name = "OtherTools"
command = "other-tool"
`
	if err := os.WriteFile(path, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(config.MCPServers)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[
  {
    "name": "ProjectTools",
    "command": "/path with spaces/tool",
    "args": ["--root", "A B", ""],
    "env": [{"name": "PROJECT_MODE", "value": "Local=On"}]
  },
  {"name": "OtherTools", "command": "other-tool", "args": [], "env": []}
]`
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("MCP server JSON = %s, want %s", got, want)
	}
}

func TestLoadRejectsDuplicateMCPServerNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	const settings = `
[[mcp_servers]]
name = "tools"
command = "one"

[[mcp_servers]]
name = "tools"
command = "two"
`
	if err := os.WriteFile(path, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, "", ""); err == nil || !strings.Contains(err.Error(), `"tools"`) {
		t.Fatalf("expected an error identifying the duplicate server, got %v", err)
	}
}

func TestHTTPHeadersResolveWhenOpeningSession(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	path := filepath.Join(dir, "config.toml")
	settings := `[[mcp_servers]]
name = "work"
url = "https://example.org/mcp"
[[mcp_servers.headers]]
name = "Authorization"
from_file = "` + file + `"
[[mcp_servers.headers]]
name = "X-User"
from_env = "PETAL_TEST_USER"
`
	if err := os.WriteFile(path, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PETAL_TEST_USER", "Work")
	server := config.MCPServers[0]
	for _, token := range []string{"first", "rotated"} {
		if err := os.WriteFile(file, []byte(token+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		wire, err := server.WireDefinition()
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		var server struct {
			Name, Type, URL string
			Headers         []MCPEnvVariable
		}
		if err := json.Unmarshal(got, &server); err != nil {
			t.Fatal(err)
		}
		if server.Name != "work" || server.Type != "http" || server.URL != "https://example.org/mcp" ||
			!reflect.DeepEqual(server.Headers, []MCPEnvVariable{{"Authorization", token}, {"X-User", "Work"}}) {
			t.Fatalf("unexpected HTTP definition %s", got)
		}
	}
}

func TestDelegationConfigurationReservesBridgeNameOnlyWhenEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	write := func(config string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const server = "\n[[mcp_servers]]\nname = \"petal-delegation\"\ncommand = \"tool\"\n"
	write("delegation = false\n" + server)
	if config, err := Load(path, "", ""); err != nil || config.Delegation {
		t.Fatalf("default delegation = %v, error = %v", config.Delegation, err)
	}
	write(server)
	if _, err := Load(path, "", ""); err == nil || !strings.Contains(err.Error(), DelegationMCPServerName) {
		t.Fatalf("expected reserved name error, got %v", err)
	}
	write("")
	if config, err := Load(path, "", ""); err != nil || !config.Delegation {
		t.Fatalf("configured delegation = %v, error = %v", config.Delegation, err)
	}
}
