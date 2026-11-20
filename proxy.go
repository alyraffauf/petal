package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

type object map[string]json.RawMessage

func encoded(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
func sessionID(p object) (string, error) {
	var id string
	if err := json.Unmarshal(p["sessionId"], &id); err != nil || id == "" {
		return "", acp.NewInvalidParams("missing sessionId")
	}
	return id, nil
}

type session struct {
	account string
	conn    *acp.Connection
	native  string
	stop    context.CancelFunc
}
type proxy struct {
	config
	lifetime context.Context
	editor   *acp.Connection
	mu       sync.Mutex
	init     json.RawMessage
	sessions map[string]*session
}

func rpcError(err error) *acp.RequestError {
	if err == nil {
		return nil
	}
	var re *acp.RequestError
	if errors.As(err, &re) {
		return re
	}
	return acp.NewInternalError(err.Error())
}

func relay(ctx context.Context, conn *acp.Connection, method string, p object) (any, error) {
	switch method {
	case "session/update", "session/cancel":
		return nil, conn.SendNotification(ctx, method, p)
	}
	return acp.SendRequest[json.RawMessage](conn, ctx, method, p)
}

func (p *proxy) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == "initialize" {
		p.mu.Lock()
		p.init = raw
		p.mu.Unlock()
		return acp.InitializeResponse{ProtocolVersion: 1, AgentCapabilities: acp.AgentCapabilities{LoadSession: true}}, nil
	}
	var params object
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, acp.NewInvalidParams(err.Error())
	}
	p.mu.Lock()
	init := p.init
	p.mu.Unlock()
	if init == nil {
		return nil, acp.NewInvalidRequest("initialize first")
	}
	switch method {
	case "session/new", "session/load":
		return p.open(ctx, init, method, params)
	}
	if _, ok := params["sessionId"]; !ok {
		return nil, acp.NewMethodNotFound(method)
	}
	id, err := sessionID(params)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	s := p.sessions[id]
	p.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("unknown session")
	}
	params["sessionId"] = encoded(s.native)
	result, err := relay(ctx, s.conn, method, params)
	if err != nil && method == "session/prompt" {
		rememberRateLimit(s.account, err)
	}
	return result, err
}

func (p *proxy) open(ctx context.Context, init json.RawMessage, method string, params object) (any, error) {
	var name, public, native string
	var err error
	if method == "session/load" {
		public, err = sessionID(params)
		if err != nil {
			return nil, err
		}
		var parts []string
		if json.Unmarshal([]byte(public), &parts) != nil || len(parts) != 3 || parts[1] != p.model || parts[2] == "" {
			return nil, acp.NewInvalidParams("invalid session for selected model")
		}
		name, native = parts[0], parts[2]
		if p.forced != "" {
			name = p.forced
		}
		params["sessionId"] = encoded(native)
	} else {
		name, err = p.choose(ctx)
		if err != nil {
			return nil, err
		}
	}
	a := p.Accounts[name]
	argv := a.Models[p.model]
	if len(argv) == 0 {
		return nil, fmt.Errorf("account %q cannot serve model %q", name, p.model)
	}
	wrap := func(id string) string { b, _ := json.Marshal([]string{name, p.model, id}); return string(b) }
	loadedID := public
	callback := func(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
		var args object
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
		if _, ok := args["sessionId"]; ok {
			id, err := sessionID(args)
			if err != nil {
				return nil, rpcError(err)
			}
			external := loadedID
			if external == "" {
				external = wrap(id)
			}
			args["sessionId"] = encoded(external)
		}
		result, err := relay(ctx, p.editor, method, args)
		return result, rpcError(err)
	}
	lifetime, stop := context.WithCancel(p.lifetime)
	keep := false
	defer func() {
		if !keep {
			stop()
		}
	}()
	cmd := a.command(lifetime, argv)
	if !p.quiet {
		cmd.Stderr = os.Stderr
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	conn := acp.NewConnection(callback, in, out)
	if p.quiet {
		conn.SetLogger(slog.New(slog.DiscardHandler))
	}
	go func() { <-conn.Done(); stop(); cmd.Wait() }()
	hello, err := acp.SendRequest[acp.InitializeResponse](conn, ctx, "initialize", init)
	if err != nil {
		return nil, err
	}
	if hello.ProtocolVersion != 1 {
		return nil, fmt.Errorf("unsupported ACP protocol version")
	}
	if method == "session/load" && !hello.AgentCapabilities.LoadSession {
		return nil, fmt.Errorf("adapter cannot load sessions")
	}
	result, err := acp.SendRequest[object](conn, ctx, method, params)
	if err != nil {
		return nil, err
	}
	if method == "session/new" {
		native, err = sessionID(result)
		if err != nil {
			return nil, err
		}
		if a.selectModel[p.model] {
			updated, err := acp.SendRequest[object](conn, ctx, "session/set_config_option", map[string]string{"sessionId": native, "configId": "model", "value": p.model})
			if err != nil {
				return nil, fmt.Errorf("select %s on %s: %w", p.model, name, err)
			}
			if opts, ok := updated["configOptions"]; ok {
				result["configOptions"] = opts
			}
		}
		public = wrap(native)
		result["sessionId"] = encoded(public)
	}
	// The embedded client learns the capabilities of the selected backend
	// only after the proxy has routed this session.
	if p.quiet && hello.Meta["steering"] != nil {
		if result == nil {
			result = object{}
		}
		result["petalSteering"] = hello.Meta["steering"]
	}
	s := &session{name, conn, native, stop}
	p.mu.Lock()
	old := p.sessions[public]
	p.sessions[public] = s
	p.mu.Unlock()
	if old != nil {
		old.stop()
	}
	keep = true
	go func() {
		<-conn.Done()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.sessions[public] == s {
			delete(p.sessions, public)
		}
	}()
	return result, nil
}

func serve(c config) {
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	conn := newProxy(lifetime, c, os.Stdout, os.Stdin)
	<-conn.Done()
}

func newProxy(lifetime context.Context, c config, out io.Writer, in io.Reader) *acp.Connection {
	p := proxy{config: c, lifetime: lifetime, sessions: make(map[string]*session)}
	ready := make(chan struct{})
	p.editor = acp.NewConnection(func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		<-ready
		result, err := p.handle(ctx, method, params)
		return result, rpcError(err)
	}, out, in)
	if c.quiet {
		p.editor.SetLogger(slog.New(slog.DiscardHandler))
	}
	close(ready)
	return p.editor
}
