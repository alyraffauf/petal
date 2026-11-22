//go:build linux

package proxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"petal/internal/routing"
)

const discoveryToken = "loopback-only-token"

type discoveryProvider struct {
	t                        *testing.T
	origin                   string
	outcome                  string
	public                   bool
	revoked                  bool
	mu                       sync.Mutex
	registeredURI, challenge string
	events                   []string
}

func (p *discoveryProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		reply(map[string]any{"resource": p.origin + "/mcp", "authorization_servers": []string{p.origin}})
	case "/.well-known/oauth-authorization-server":
		reply(map[string]any{
			"issuer": p.origin, "authorization_endpoint": p.origin + "/authorize",
			"token_endpoint": p.origin + "/token", "registration_endpoint": p.origin + "/register",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	case "/register":
		var metadata map[string]any
		if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
			p.t.Error(err)
			w.WriteHeader(400)
			return
		}
		p.events = append(p.events, "register")
		uris, _ := metadata["redirect_uris"].([]any)
		if len(uris) == 1 {
			p.registeredURI, _ = uris[0].(string)
		}
		metadata["client_id"] = "loopback-client"
		w.WriteHeader(201)
		reply(metadata)
	case "/authorize":
		query := r.URL.Query()
		callback, err := url.Parse(query.Get("redirect_uri"))
		if err != nil || callback.String() != p.registeredURI ||
			(callback.Hostname() != "localhost" && callback.Hostname() != "127.0.0.1") ||
			callback.Scheme != "http" || callback.Path != "/oauth/callback" ||
			query.Get("client_id") != "loopback-client" || query.Get("response_type") != "code" ||
			query.Get("state") == "" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
			p.t.Error("invalid authorization request")
			w.WriteHeader(400)
			return
		}
		p.events = append(p.events, "authorize:"+p.outcome)
		p.challenge = query.Get("code_challenge")
		parameters := url.Values{"state": {query.Get("state")}}
		if p.outcome == "deny" {
			parameters.Set("error", "access_denied")
			parameters.Set("error_description", "fixture declined consent")
		} else {
			parameters.Set("code", "loopback-code")
		}
		callback.RawQuery = parameters.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	case "/token":
		if err := r.ParseForm(); err != nil {
			p.t.Error(err)
		}
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "loopback-code" || r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("redirect_uri") != p.registeredURI ||
			base64.RawURLEncoding.EncodeToString(digest[:]) != p.challenge {
			p.t.Error("invalid authorization-code exchange")
			w.WriteHeader(400)
			return
		}
		p.events = append(p.events, "token:"+p.outcome)
		if p.outcome == "reject-token" {
			w.WriteHeader(400)
			reply(map[string]string{"error": "invalid_grant"})
			return
		}
		reply(map[string]any{"access_token": discoveryToken, "token_type": "Bearer", "expires_in": 3600})
	case "/mcp":
		if !p.public && (p.revoked || r.Header.Get("Authorization") != "Bearer "+discoveryToken) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, p.origin))
			w.WriteHeader(401)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var request struct {
			ID     json.RawMessage
			Method string
			Params struct{ ProtocolVersion string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			p.t.Error(err)
			w.WriteHeader(400)
			return
		}
		p.events = append(p.events, request.Method)
		if request.ID == nil {
			w.WriteHeader(202)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": request.Params.ProtocolVersion,
				"serverInfo":   map[string]string{"name": "loopback", "version": "1"},
				"capabilities": map[string]any{"tools": struct{}{}, "resources": struct{}{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "loopback-tool", "inputSchema": map[string]string{"type": "object"}}}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		default:
			result = struct{}{}
		}
		reply(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	default:
		w.WriteHeader(404)
	}
}

func (p *discoveryProvider) count(event string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, observed := range p.events {
		if observed == event {
			count++
		}
	}
	return count
}

func (p *discoveryProvider) revoke() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.public = false
	p.revoked = true
	p.outcome = "deny"
}

func testLogin(t *testing.T, command *exec.Cmd, provider *discoveryProvider, callbackStatus int, early, closeInput bool) (string, error) {
	t.Helper()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if !closeInput {
		input, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	scanner := bufio.NewScanner(stderr)
	callbacks := 0
	var authorizationURL string
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(&output, line)
		if strings.HasPrefix(line, provider.origin+"/authorize?") {
			authorizationURL = line
		}
		if authorizationURL == "" || (!early && !strings.Contains(line, "Waiting for authorization...")) {
			continue
		}
		callbacks++
		client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			host := req.URL.Hostname()
			if host == "localhost" || net.ParseIP(host).IsLoopback() {
				return nil
			}
			return fmt.Errorf("unexpected redirect outside loopback")
		}}
		response, err := client.Get(authorizationURL)
		authorizationURL = ""
		if err != nil {
			t.Error(err)
		} else {
			response.Body.Close()
			if response.StatusCode != callbackStatus {
				t.Errorf("callback HTTP status = %d, want %d", response.StatusCode, callbackStatus)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Error(err)
	}
	err = command.Wait()
	wantCallbacks := 0
	if callbackStatus != 0 {
		wantCallbacks = 1
	}
	if callbacks != wantCallbacks {
		t.Errorf("authorization callbacks = %d, want %d", callbacks, wantCallbacks)
	}
	return output.String(), err
}

func runDiscoveryBridge(t *testing.T, server routing.MCPServer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, server.Command, server.Args...)
	cmd.Env = os.Environ()
	for _, env := range server.Env {
		cmd.Env = append(cmd.Env, env.Name+"="+env.Value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics strings.Builder
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(diagnostics.String())
		}
	}()
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	send := func(message string) {
		if err := encoder.Encode(json.RawMessage(message)); err != nil {
			t.Fatal(err)
		}
	}
	receive := func(id int) json.RawMessage {
		var reply struct {
			ID     int
			Result json.RawMessage
		}
		if err := decoder.Decode(&reply); err != nil {
			t.Fatal(err)
		}
		if reply.ID != id || reply.Result == nil {
			t.Fatalf("bridge reply = %+v", reply)
		}
		return reply.Result
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"discovery","version":"1"}}}`)
	receive(1)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if result := receive(2); !strings.Contains(string(result), `"loopback-tool"`) {
		t.Fatalf("bridge tools = %s", result)
	}
}

func TestMCPRemoteDiscovery(t *testing.T) {
	cli, helpers := os.Getenv("PETAL_OAUTH_CLI"), os.Getenv("PETAL_OAUTH_HELPERS")
	if cli == "" || helpers == "" {
		t.Skip("run scripts/check-mcp-oauth to install the real helpers and build Petal")
	}
	t.Setenv("PATH", helpers+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BROWSER", "/bin/false")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("MCP_REMOTE_CONFIG_DIR", filepath.Join(t.TempDir(), "ambient-helper-state"))

	t.Run("cli-helper-preflight", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "config.toml")
		const config = `
[[mcp_servers]]
name = "tools"
url = "http://127.0.0.1:1/mcp"

[[mcp_servers]]
name = "header-tools"
url = "http://127.0.0.1:1/mcp"

[[mcp_servers.headers]]
name = "Authorization"
value = "fixture"

[[mcp_servers]]
name = "local-tools"
command = "/bin/false"
`
		if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		partialPath := t.TempDir()
		t.Setenv("PATH", partialPath)
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		checkError := func(name, want string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, cli, "--config", configPath, "mcp", "login", name).CombinedOutput()
			if err == nil || !strings.Contains(string(output), want) {
				t.Fatalf("login %q error = %v, output = %s, want %q", name, err, output, want)
			}
		}
		checkError("missing", "unknown MCP server")
		checkError("header-tools", "browser login applies to HTTP servers without headers")
		checkError("local-tools", "browser login applies to HTTP servers without headers")
		checkError("tools", "install mcp-remote to log in")
		bridge := filepath.Join(partialPath, "mcp-remote")
		if err := os.Symlink(filepath.Join(helpers, "mcp-remote"), bridge); err != nil {
			t.Fatal(err)
		}
		checkError("tools", `"mcp-remote-client": executable file not found`)
	})

	for _, scenario := range []struct {
		name, outcome                             string
		public, early, closeInput, denyAfterLogin bool
		callbackStatus                            int
		tokenRequests                             int
		wantReady                                 bool
	}{
		{name: "approve-and-reuse", outcome: "approve", callbackStatus: 200, tokenRequests: 1, wantReady: true},
		{name: "deny-cold", outcome: "deny", callbackStatus: 400},
		{name: "deny-before-waiter", outcome: "deny", callbackStatus: 400, early: true},
		{name: "approve-with-eof", outcome: "approve", callbackStatus: 200, tokenRequests: 1, wantReady: true, closeInput: true},
		{name: "reject-token", outcome: "reject-token", callbackStatus: 200, tokenRequests: 2},
		{name: "public-then-deny", public: true, wantReady: true, denyAfterLogin: true},
		{name: "approve-then-revoke", outcome: "approve", callbackStatus: 200, tokenRequests: 1, wantReady: true, denyAfterLogin: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			provider := &discoveryProvider{t: t, outcome: scenario.outcome, public: scenario.public}
			server := httptest.NewServer(provider)
			defer server.Close()
			provider.mu.Lock()
			provider.origin = server.URL
			provider.mu.Unlock()
			configPath := filepath.Join(t.TempDir(), "config.toml")
			config := fmt.Sprintf("[[mcp_servers]]\nname = \"tools\"\nurl = %q\n", server.URL+"/mcp")
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			settings, err := routing.Load(configPath, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, ready, err := settings.MCPServers[0].SessionServer(); err != nil || ready {
				t.Fatalf("fresh server ready = %v, err = %v", ready, err)
			}
			login := func(status int) (string, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				output, err := testLogin(t, exec.CommandContext(ctx, cli, "--config", configPath, "mcp", "login", "tools"), provider, status, scenario.early, scenario.closeInput)
				if scenario.early {
					denied := strings.Index(output, "Authorization failed: access_denied")
					waiting := strings.Index(output, "Waiting for authorization...")
					if ctx.Err() != nil || err == nil || denied < 0 || waiting <= denied || !strings.Contains(output, "OAuth authorization denied") {
						t.Fatalf("early denial did not exit promptly: %v\n%s", err, output)
					}
				} else if ctx.Err() != nil {
					t.Fatalf("login did not terminate within harness deadline:\n%s", output)
				}
				return output, err
			}
			output, err := login(scenario.callbackStatus)
			if (err == nil) != scenario.wantReady {
				t.Fatalf("login error = %v:\n%s", err, output)
			}
			if got := provider.count("token:" + scenario.outcome); got != scenario.tokenRequests {
				t.Fatalf("token requests = %d, want %d", got, scenario.tokenRequests)
			}
			if got := provider.count("initialize"); (got > 0) != scenario.wantReady {
				t.Fatalf("successful MCP initializes = %d", got)
			}
			v1, err := mergeMCPServers(Object{}, settings.MCPServers, false)
			if err != nil {
				t.Fatal(err)
			}
			v2, err := mergeMCPServersV2(Object{}, settings.MCPServers, nil, Object{"stdio": json.RawMessage("{}")})
			if err != nil {
				t.Fatal(err)
			}
			toolsBeforeBridges := provider.count("tools/list")
			for _, params := range []Object{v1, v2} {
				if (params["mcpServers"] != nil) != scenario.wantReady {
					t.Fatalf("session servers = %s", params["mcpServers"])
				}
				if !scenario.wantReady {
					continue
				}
				var bridges []routing.MCPServer
				if err := json.Unmarshal(params["mcpServers"], &bridges); err != nil || len(bridges) != 1 {
					t.Fatalf("decode session bridge: %v, %s", err, params["mcpServers"])
				}
				bridge := bridges[0]
				if bridge.Name != "tools" || bridge.Command != filepath.Join(helpers, "mcp-remote") ||
					!slices.Equal(bridge.Args, []string{server.URL + "/mcp", "--transport", "http-only"}) ||
					len(bridge.Env) != 1 || bridge.Env[0].Name != "MCP_REMOTE_CONFIG_DIR" ||
					!strings.HasPrefix(bridge.Env[0].Value, filepath.Join(os.Getenv("XDG_STATE_HOME"), "petal", "mcp")+string(os.PathSeparator)) {
					t.Fatalf("unexpected bridge definition: %+v", bridge)
				}
				runDiscoveryBridge(t, bridge)
			}
			if scenario.outcome == "approve" {
				if got := provider.count("authorize:approve"); got != 1 {
					t.Errorf("authorization requests after two bridges = %d", got)
				}
				if got := provider.count("token:approve"); got != 1 {
					t.Errorf("token exchanges after two bridges = %d", got)
				}
				if got := provider.count("tools/list") - toolsBeforeBridges; got != 2 {
					t.Errorf("tools/list requests from bridges = %d", got)
				}
				wantDiagnostics := 1
				if scenario.closeInput {
					wantDiagnostics = 0
				}
				if toolsBeforeBridges != wantDiagnostics || provider.count("resources/list") != wantDiagnostics {
					t.Fatalf("login diagnostics: tools=%d resources=%d, want %d each\n%s",
						toolsBeforeBridges, provider.count("resources/list"), wantDiagnostics, output)
				}
				if output, err := login(0); err != nil {
					t.Fatalf("repeat login: %v\n%s", err, output)
				}
			}
			if scenario.wantReady {
				configured := settings.MCPServers[0]
				for _, other := range []routing.MCPServer{
					{Name: configured.Name + "-other", URL: configured.URL},
					{Name: configured.Name, URL: configured.URL + "?other=1"},
				} {
					if _, ready, err := other.SessionServer(); err != nil || ready {
						t.Errorf("changed identity ready = %v, err = %v", ready, err)
					}
				}
			}
			if scenario.denyAfterLogin {
				provider.revoke()
				output, err := login(400)
				if err == nil || !strings.Contains(output, "access_denied") {
					t.Fatalf("expected denied re-login: %v\n%s", err, output)
				}
				if _, ready, err := settings.MCPServers[0].SessionServer(); err != nil || ready {
					t.Fatalf("expected server disabled after denied re-login, got ready=%v err=%v", ready, err)
				}
			}
		})
	}
}
