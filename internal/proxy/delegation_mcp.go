package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
	"petal/internal/sessions"
)

const delegationTokenEnv = "PETAL_DELEGATION_TOKEN"

const delegationServerName = routing.DelegationMCPServerName

// The bridge encodes runtime results without coupling the wire format to their Go types.
type delegationTools struct {
	catalog  func(string) any
	delegate func(context.Context, string, string, string, string) (any, error)
	read     func(context.Context, string, string, string, int64) (any, error)
	wait     func(context.Context, string, string, string, int64, time.Duration) (any, error)
}

type delegationEndpoint struct {
	ctx        context.Context
	wakes      wakeQueue
	cancel     context.CancelFunc
	ready      chan struct{}
	parent     string // published by closing ready
	cwd        string
	forkParent string
	server     routing.MCPServer
}

func newDelegationEndpoint(lifetime context.Context, cwd string, tools delegationTools) (*delegationEndpoint, error) {
	command, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "petal-mcp-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(dir, "m")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.Remove(dir)
		return nil, err
	}
	ctx, cancel := context.WithCancel(lifetime)
	token := rand.Text()
	endpoint := &delegationEndpoint{
		ctx: ctx, cancel: cancel, ready: make(chan struct{}), cwd: cwd,
		server: routing.MCPServer{Name: delegationServerName, Command: command, Args: []string{"delegation-mcp", socket}, Env: []routing.MCPEnvVariable{{Name: delegationTokenEnv, Value: token}}},
	}
	context.AfterFunc(ctx, func() {
		_ = listener.Close()
		_ = os.Remove(dir)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				proof := make([]byte, len(token))
				if _, err := io.ReadFull(conn, proof); err != nil || subtle.ConstantTimeCompare(proof, []byte(token)) != 1 {
					return
				}
				_ = conn.SetReadDeadline(time.Time{})
				endpoint.serve(ctx, conn, tools)
			}()
		}
	}()
	return endpoint, nil
}

// RelayDelegationMCP is the stdio subprocess entry point. All authority stays
// with the proxy listening on the private socket.
func RelayDelegationMCP(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	return relayDelegationMCP(ctx, socket, os.Getenv(delegationTokenEnv), in, out)
}

func relayDelegationMCP(ctx context.Context, socket, token string, in io.Reader, out io.Writer) error {
	if token == "" {
		return errors.New("missing delegation credential")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, token); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(conn, in)
		_ = conn.(*net.UnixConn).CloseWrite()
	}()
	_, err = io.Copy(out, conn)
	return err
}

// parent is the canonical editor-facing route, within cwd. Adapter pins can change
// which backend serves that route without changing the stored session identity.
func (p *proxy) prepareDelegation(lifetime context.Context, params Object, method, parent string, supportsStdio bool) (*delegationEndpoint, []routing.MCPServer, error) {
	if p.delegation == nil || (method != "session/new" && method != "session/load" && method != "session/resume" && method != "session/fork") {
		return nil, p.MCPServers, nil
	}
	if !supportsStdio {
		// The adapter can still serve sessions on a mixed-capability route.
		return nil, p.MCPServers, nil
	}
	var servers []Object
	if raw := params["mcpServers"]; raw != nil {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, nil, acp.NewInvalidParams("mcpServers must be an array of server objects")
		}
	}
	for _, server := range servers {
		if stringField(server, "name") == delegationServerName {
			return nil, nil, acp.NewInvalidParams("the petal-delegation MCP server is reserved")
		}
	}
	cwd := stringField(params, "cwd")
	if !filepath.IsAbs(cwd) {
		return nil, nil, acp.NewInvalidParams("cwd must be an absolute path")
	}
	p.delegationMu.Lock()
	existingCwd := p.delegationCwds[parent]
	p.delegationMu.Unlock()
	if existingCwd != "" && existingCwd != cwd {
		return nil, nil, acp.NewInvalidParams("parent session belongs to a different working directory")
	}
	if method != "session/new" {
		records, err := sessions.List(cwd, []string{p.Model})
		if err != nil {
			return nil, nil, fmt.Errorf("check resumed delegation parent: %w", err)
		}
		for _, record := range records {
			if record.ID == parent && record.DelegatedParent != "" {
				return nil, p.MCPServers, nil
			}
		}
	}
	endpoint, err := newDelegationEndpoint(lifetime, cwd, *p.delegation)
	if err != nil {
		return nil, nil, err
	}
	if method == "session/fork" {
		endpoint.forkParent = parent
	}
	configured := append([]routing.MCPServer{endpoint.server}, p.MCPServers...)
	return endpoint, configured, nil
}

func (endpoint *delegationEndpoint) closeIfUnbound() {
	select {
	case <-endpoint.ready:
	default:
		endpoint.cancel()
	}
}

func (p *proxy) bindDelegation(endpoint *delegationEndpoint, account, parent string, placement *routing.Placement) error {
	p.delegationMu.Lock()
	defer p.delegationMu.Unlock()
	if cwd := p.delegationCwds[parent]; cwd != "" && cwd != endpoint.cwd {
		return acp.NewInvalidParams("parent session belongs to a different working directory")
	}
	record := sessions.Record{
		ID: parent, Model: p.Model, Account: account, Opened: time.Now(), Placement: placement,
	}
	if endpoint.forkParent != "" {
		source, err := sessions.Lookup(endpoint.cwd, endpoint.forkParent)
		if err != nil {
			return fmt.Errorf("read fork parent: %w", err)
		}
		record.Parent = endpoint.forkParent
		record.Title = source.ForkTitle()
	}
	if _, err := sessions.Create(endpoint.cwd, record); err != nil {
		return fmt.Errorf("record delegation parent session: %w", err)
	}
	p.closeDelegationLocked(parent)
	if p.delegationEndpoints == nil {
		p.delegationEndpoints = make(map[string]*delegationEndpoint)
		p.delegationCwds = make(map[string]string)
	}
	p.delegationEndpoints[parent] = endpoint
	p.delegationCwds[parent] = endpoint.cwd
	endpoint.parent = parent
	close(endpoint.ready)
	return nil
}

func (p *proxy) closeDelegationLocked(parent string) {
	if endpoint := p.delegationEndpoints[parent]; endpoint != nil {
		endpoint.wakes.interrupt(true)
		endpoint.cancel()
		delete(p.delegationEndpoints, parent)
	}
}

func (p *proxy) delegationEndpointToClose(method, parent string) *delegationEndpoint {
	if method == "session/close" || method == "session/delete" {
		return p.endpoint(parent)
	}
	return nil
}

func (p *proxy) finishDelegationClose(parent string, endpoint *delegationEndpoint) {
	if endpoint == nil {
		return
	}
	p.delegationMu.Lock()
	defer p.delegationMu.Unlock()
	if p.delegationEndpoints[parent] == endpoint {
		p.closeDelegationLocked(parent)
		p.delegationRuntime.CancelParent(parent, endpoint.cwd)
	}
}

func (p *proxy) cancelDelegation(parent string) {
	if parent == "" || p.delegationRuntime == nil {
		return
	}
	p.delegationMu.Lock()
	defer p.delegationMu.Unlock()
	if endpoint := p.delegationEndpoints[parent]; endpoint != nil {
		endpoint.wakes.interrupt(true)
	}
	p.delegationRuntime.CancelParent(parent, p.delegationCwds[parent])
}

type mcpRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (endpoint *delegationEndpoint) serve(lifetime context.Context, conn net.Conn, tools delegationTools) {
	ctx, cancel := context.WithCancel(lifetime)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(conn)
	var mu sync.Mutex
	active := make(map[string]context.CancelFunc)
	write := func(response mcpResponse) {
		mu.Lock()
		defer mu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := encoder.Encode(response); err != nil {
			cancel()
		}
	}

	initialized := false
	for scanner.Scan() {
		var req mcpRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			write(mcpResponse{"2.0", json.RawMessage("null"), nil, &mcpError{-32700, "Parse error"}})
			continue
		}
		if req.Version != "2.0" || req.Method == "" {
			write(mcpResponse{"2.0", json.RawMessage("null"), nil, &mcpError{-32600, "Invalid request"}})
			continue
		}
		if req.ID == nil {
			if req.Method == "notifications/cancelled" {
				var params struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(req.Params, &params) == nil {
					mu.Lock()
					if stop := active[string(params.RequestID)]; stop != nil {
						stop()
					}
					mu.Unlock()
				}
			}
			continue
		}
		response := mcpResponse{Version: "2.0", ID: req.ID}
		switch {
		case req.Method == "initialize" && !initialized:
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion == "" {
				response.Error = &mcpError{-32602, "protocolVersion is required"}
				break
			}
			initialized = true
			switch params.ProtocolVersion {
			case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
			default:
				params.ProtocolVersion = "2025-11-25"
			}
			response.Result = map[string]any{
				"protocolVersion": params.ProtocolVersion,
				"capabilities":    map[string]any{"tools": struct{}{}},
				"instructions":    "Call petal_capabilities before choosing a child model. Use its exact model ID. Give the child a self-contained task with the context, file scope and output you need. The child shares your working directory and starts with fresh conversation history. Keep childId. Finish your turn when you have no independent work. Petal wakes the connected parent for unread child results. Follow next on each response.",
				"serverInfo":      map[string]string{"name": delegationServerName, "version": "1.0.0"},
			}
		case req.Method == "ping":
			response.Result = struct{}{}
		case !initialized:
			response.Error = &mcpError{-32002, "Server not initialized"}
		case req.Method == "tools/list":
			response.Result = json.RawMessage(delegationToolList)
		case req.Method == "tools/call":
			key := string(req.ID)
			mu.Lock()
			if len(active) >= 16 || active[key] != nil {
				mu.Unlock()
				response.Error = &mcpError{-32600, "Duplicate request ID or too many active requests"}
				break
			}
			callCtx, stop := context.WithCancel(ctx)
			active[key] = stop
			mu.Unlock()
			go func() {
				defer stop()
				result := endpoint.call(callCtx, tools, req.Params)
				mu.Lock()
				delete(active, key)
				mu.Unlock()
				if callCtx.Err() == nil {
					response.Result = result
					write(response)
				}
			}()
			continue
		default:
			response.Error = &mcpError{-32601, "Method not found"}
		}
		write(response)
	}
}

const delegationToolList = `{"tools":[
{"name":"petal_capabilities","description":"Discover configured child models before delegating. Search by model ID, label or provider if a target is missing. IDs respect the current account pin. Quota is checked at routing time when needed.","inputSchema":{"type":"object","properties":{"search":{"type":"string"}},"additionalProperties":false}},
{"name":"petal_delegate","description":"Use a model ID from petal_capabilities. Start an independent child in your working directory. Its only prompt is task, so include context, edit scope and expected output. Keep childId and follow next.","inputSchema":{"type":"object","properties":{"model":{"type":"string","minLength":1},"task":{"type":"string","minLength":1}},"required":["model","task"],"additionalProperties":false}},
{"name":"petal_read","description":"Read available child activity immediately. Start at 0 and pass nextAfter as after on subsequent calls. Follow next.","inputSchema":{"type":"object","properties":{"child_id":{"type":"string","minLength":1},"after":{"type":"integer","minimum":0,"default":0}},"required":["child_id"],"additionalProperties":false}},
{"name":"petal_wait","description":"Wait for activity, a permission request, completion or timeout (default 30 seconds). Follow next. A running child may return an empty page.","inputSchema":{"type":"object","properties":{"child_id":{"type":"string","minLength":1},"after":{"type":"integer","minimum":0,"default":0},"timeout":{"type":"integer","minimum":1,"maximum":60,"default":30}},"required":["child_id"],"additionalProperties":false}}
]}`

type mcpText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpText `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

func (endpoint *delegationEndpoint) call(ctx context.Context, tools delegationTools, raw json.RawMessage) mcpToolResult {
	var call struct {
		Name      string
		Arguments json.RawMessage
	}
	var value any
	err := json.Unmarshal(raw, &call)
	if err == nil {
		select {
		case <-endpoint.ready:
			if err = ctx.Err(); err == nil {
				value, err = endpoint.invoke(ctx, tools, call.Name, call.Arguments)
			}
		default:
			err = errors.New("Petal has not confirmed the parent session yet")
		}
	}
	if err == nil {
		var encoded []byte
		encoded, err = json.Marshal(value)
		if err == nil {
			return mcpToolResult{Content: []mcpText{{Type: "text", Text: string(encoded)}}}
		}
	}
	return mcpToolResult{Content: []mcpText{{Type: "text", Text: err.Error()}}, IsError: true}
}

func (endpoint *delegationEndpoint) invoke(ctx context.Context, tools delegationTools, name string, arguments json.RawMessage) (any, error) {
	decode := func(dest any) error {
		decoder := json.NewDecoder(bytes.NewReader(arguments))
		decoder.DisallowUnknownFields()
		return decoder.Decode(dest)
	}
	switch name {
	case "petal_capabilities":
		var args struct{ Search string }
		if len(arguments) > 0 {
			if err := decode(&args); err != nil {
				return nil, err
			}
		}
		return tools.catalog(args.Search), nil
	case "petal_delegate":
		var args struct{ Model, Task string }
		if err := decode(&args); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Model) == "" || strings.TrimSpace(args.Task) == "" {
			return nil, errors.New("model and task are required")
		}
		return tools.delegate(ctx, endpoint.parent, endpoint.cwd, args.Model, args.Task)
	case "petal_read":
		var args struct {
			ChildID string `json:"child_id"`
			After   int64
		}
		if err := decode(&args); err != nil {
			return nil, err
		}
		if args.ChildID == "" || args.After < 0 {
			return nil, errors.New("child_id is required and after must be nonnegative")
		}
		return tools.read(ctx, endpoint.parent, endpoint.cwd, args.ChildID, args.After)
	case "petal_wait":
		args := struct {
			ChildID string `json:"child_id"`
			After   int64
			Timeout int64
		}{Timeout: 30}
		if err := decode(&args); err != nil {
			return nil, err
		}
		if args.ChildID == "" || args.After < 0 || args.Timeout < 1 || args.Timeout > 60 {
			return nil, errors.New("child_id is required, after must be nonnegative, and timeout must be 1 to 60 seconds")
		}
		return tools.wait(ctx, endpoint.parent, endpoint.cwd, args.ChildID, args.After, time.Duration(args.Timeout)*time.Second)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}
