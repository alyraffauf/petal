package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
)

type Object map[string]json.RawMessage

func encoded(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
func SessionID(p Object) (string, error) {
	var id string
	if err := json.Unmarshal(p["sessionId"], &id); err != nil || id == "" {
		return "", acp.NewInvalidParams("missing sessionId")
	}
	return id, nil
}

type backend struct {
	close context.CancelFunc
	raw   Object
	conn  *acp.Connection
	hello acp.InitializeResponse
}
type proxy struct {
	embedded bool
	routing.Config
	lifetime     context.Context
	editor       *acp.Connection
	mu           sync.Mutex
	aliases      sync.Map // canonical backend ID to the editor ID for an explicit resume override
	initializing bool
	initialized  bool
	backends     map[string]*backend
	accounts     []string
	capabilities acp.AgentCapabilities
}

func unknownSession(id string) *acp.RequestError {
	return acp.NewRequestError(-32002, "Resource not found", map[string]string{"sessionId": id})
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

func relay(ctx context.Context, conn *acp.Connection, method string, p Object) (any, error) {
	if native, ok := strings.CutPrefix(method, notificationPrefix); ok {
		return nil, conn.SendNotification(ctx, native, p)
	}
	return forward[json.RawMessage](conn, ctx, method, p)
}

func (p *proxy) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == "initialize" {
		return p.initialize(ctx, raw)
	}
	p.mu.Lock()
	ready := p.initialized
	p.mu.Unlock()
	if !ready {
		return nil, acp.NewInvalidRequest("initialize first")
	}
	caps := p.capabilities
	switch method {
	case "session/load":
		if !caps.LoadSession {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/list":
		if caps.SessionCapabilities.List == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/resume":
		if caps.SessionCapabilities.Resume == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/delete":
		if caps.SessionCapabilities.Delete == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/close":
		if caps.SessionCapabilities.Close == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/options":
		if caps.SessionCapabilities.Options == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/archive", "session/unarchive":
		if caps.SessionCapabilities.Archive == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/fork":
		if caps.SessionCapabilities.Fork == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/status":
		if caps.SessionCapabilities.Status == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "session/set_title":
		if caps.SessionCapabilities.Title == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	case "logout":
		if caps.Auth.Logout == nil {
			return nil, acp.NewMethodNotFound(method)
		}
	}
	var params Object
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
	}
	switch method {
	case "session/list":
		return p.list(ctx, params)
	case "authenticate":
		var key string
		var parts []string
		if json.Unmarshal(params["methodId"], &key) != nil || json.Unmarshal([]byte(key), &parts) != nil || len(parts) != 2 || p.backends[parts[0]] == nil {
			return nil, acp.NewInvalidParams("unknown authentication method")
		}
		b := p.backends[parts[0]]
		if !slices.ContainsFunc(b.hello.AuthMethods, func(auth acp.AuthMethod) bool {
			return auth.Agent != nil && string(auth.Agent.Id) == parts[1]
		}) {
			return nil, acp.NewInvalidParams("unknown agent authentication method")
		}
		params["methodId"] = encoded(parts[1])
		return relay(ctx, b.conn, method, params)
	case "logout":
		for _, name := range p.accounts {
			if _, err := relay(ctx, p.backends[name].conn, method, params); err != nil {
				return nil, err
			}
		}
		return Object{}, nil
	}
	var name, native, public string
	if method == "session/new" {
		var err error
		name, err = p.Choose(ctx)
		if err != nil {
			return nil, err
		}
	} else if _, exists := params["sessionId"]; exists {
		id, err := SessionID(params)
		if err != nil {
			return nil, err
		}
		var parts []string
		if json.Unmarshal([]byte(id), &parts) != nil || len(parts) != 3 || parts[1] != p.Model || parts[2] == "" {
			if method == "session/delete" || method == "session/close" {
				return Object{}, nil
			}
			return nil, unknownSession(id)
		}
		public = id
		name, native = parts[0], parts[2]
		if p.PinnedAccount != "" && name != p.PinnedAccount {
			canonical := p.publicID(p.PinnedAccount, native)
			if method == "session/load" || method == "session/resume" {
				p.aliases.Store(canonical, id)
			} else if existing, ok := p.aliases.Load(canonical); !ok || existing != id {
				return nil, acp.NewInvalidParams("session belongs to another account")
			}
			name = p.PinnedAccount
		}
		params["sessionId"] = encoded(native)
	} else if method == "session/options" && len(p.accounts) > 0 {
		return p.sessionOptions(ctx, params)
	} else if len(p.accounts) == 1 {
		return relay(ctx, p.backends[p.accounts[0]].conn, method, params)
	} else {
		return nil, acp.NewMethodNotFound(method)
	}
	b := p.backends[name]
	if b == nil {
		if public != "" {
			if method == "session/delete" || method == "session/close" {
				return Object{}, nil
			}
			return nil, unknownSession(public)
		}
		return nil, acp.NewInvalidParams("session account is unavailable")
	}
	if strings.HasPrefix(method, notificationPrefix) {
		return relay(ctx, b.conn, method, params)
	}
	result, err := forward[json.RawMessage](b.conn, ctx, method, params)
	if err != nil {
		var re *acp.RequestError
		if public != "" && errors.As(err, &re) && re.Code == -32002 {
			return nil, unknownSession(public)
		}
		if method == "session/prompt" {
			routing.RememberRateLimit(name, err)
		}
		return nil, err
	}
	if method != "session/new" && method != "session/load" && method != "session/resume" && method != "session/fork" {
		return result, nil
	}
	var session Object
	if err := json.Unmarshal(result, &session); err != nil {
		return nil, err
	}
	if session == nil {
		session = Object{}
	}
	if method == "session/new" || session["sessionId"] != nil {
		native, err = SessionID(session)
		if err != nil {
			return nil, err
		}
		session["sessionId"] = encoded(p.publicID(name, native))
	}
	if method == "session/new" && p.Accounts[name].SelectModel[p.Model] {
		opts, err := forward[Object](b.conn, ctx, "session/set_config_option", map[string]string{"sessionId": native, "configId": "model", "value": p.Model})
		if err != nil {
			return nil, fmt.Errorf("select %s on %s: %w", p.Model, name, err)
		}
		if opts["configOptions"] != nil {
			session["configOptions"] = opts["configOptions"]
		}
	}
	if p.embedded && b.hello.Meta["steering"] != nil {
		session["petalSteering"] = b.hello.Meta["steering"]
	}
	return session, nil
}

func (p *proxy) publicID(name, native string) string {
	b, _ := json.Marshal([]string{name, p.Model, native})
	return string(b)
}

func (p *proxy) editorID(name, native string) string {
	id := p.publicID(name, native)
	if alias, ok := p.aliases.Load(id); ok {
		return alias.(string)
	}
	return id
}

func (p *proxy) startBackend(ctx context.Context, name string, init json.RawMessage) (*backend, error) {
	var helloRequest struct {
		ClientCapabilities Object
	}
	if err := json.Unmarshal(init, &helloRequest); err != nil {
		return nil, acp.NewInvalidParams(err.Error())
	}
	clientCaps := helloRequest.ClientCapabilities

	a := p.Accounts[name]
	lifetime, stop := context.WithCancel(p.lifetime)
	cmd := a.Command(lifetime, a.Models[p.Model])
	if !p.embedded {
		cmd.Stderr = os.Stderr
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		stop()
		return nil, err
	}
	// Own the read end so reaping the adapter cannot discard its final reply.
	out, childOutput, err := os.Pipe()
	if err != nil {
		stop()
		in.Close()
		return nil, err
	}
	cmd.Stdout = childOutput
	if err := cmd.Start(); err != nil {
		childOutput.Close()
		stop()
		in.Close()
		out.Close()
		return nil, err
	}
	childOutput.Close()
	context.AfterFunc(lifetime, func() { out.Close(); in.Close() })
	wire := &adapterWire{Writer: in}
	conn := acp.NewConnection(func(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
		native, notification := strings.CutPrefix(method, notificationPrefix)
		if notification {
			switch native {
			case "session/update", "elicitation/complete", "mcp/message", "auth/status_update":
			default:
				if !strings.HasPrefix(native, "_") {
					return nil, nil
				}
			}
		} else {
			switch native {
			case "session/update", "elicitation/complete", "mcp/message", "auth/status_update":
				return nil, acp.NewInvalidRequest("expected a notification")
			}
		}
		var args Object
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, acp.NewInvalidParams(err.Error())
			}
		}
		if !allowsClientCall(clientCaps, native, args) {
			if notification {
				return nil, nil
			}
			return nil, acp.NewMethodNotFound(native)
		}
		if _, ok := args["sessionId"]; ok {
			id, err := SessionID(args)
			if err != nil {
				return nil, rpcError(err)
			}
			args["sessionId"] = encoded(p.editorID(name, id))
		}
		field := ""
		switch native {
		case "session/update":
			field = "update"
		case "session/request_permission":
			field = "toolCall"
		}
		if field != "" {
			var item Object
			if json.Unmarshal(args[field], &item) == nil {
				changed := false
				if !allowsSessionUpdate(clientCaps, item) {
					return nil, nil
				}
				subagents := clientCaps.member("subagents") != nil
				switch string(item["sessionUpdate"]) {
				case `"subagent_update"`, `"session_message"`:
					for _, key := range []string{"sessionId", "senderSessionId", "recipientSessionId"} {
						var id string
						if json.Unmarshal(item[key], &id) == nil && id != "" {
							item[key] = encoded(p.editorID(name, id))
							changed = true
						}
					}
				}
				if field == "toolCall" || string(item["sessionUpdate"]) == `"tool_call"` || string(item["sessionUpdate"]) == `"tool_call_update"` {
					var parts []Object
					if json.Unmarshal(item["content"], &parts) == nil {
						visible := parts[:0]
						for _, part := range parts {
							if string(part["type"]) == `"session"` {
								changed = true
								if !subagents {
									continue
								}
								var id string
								if json.Unmarshal(part["sessionId"], &id) == nil {
									part["sessionId"] = encoded(p.editorID(name, id))
								}
							}
							visible = append(visible, part)
						}
						if changed {
							item["content"], _ = json.Marshal(visible)
						}
					}
				}
				if changed {
					args[field], _ = json.Marshal(item)
				}
			}
		}
		result, err := relay(ctx, p.editor, method, args)
		return result, rpcError(err)
	}, wire, incoming(out, wire))
	if p.embedded {
		conn.SetLogger(slog.New(slog.DiscardHandler))
	}
	go func() {
		_ = cmd.Wait()
		// A surviving helper may still hold stdout. Allow the reader to drain
		// buffered frames, then terminate any blocked read.
		_ = out.SetReadDeadline(time.Now().Add(time.Second))
	}()
	go func() { <-conn.Done(); stop() }()
	rawHello, err := forward[Object](conn, ctx, "initialize", init)
	if err != nil {
		stop()
		return nil, err
	}
	var hello acp.InitializeResponse
	data, err := json.Marshal(rawHello)
	if err == nil {
		err = json.Unmarshal(data, &hello)
	}
	if err != nil {
		stop()
		return nil, err
	}
	if hello.ProtocolVersion != 1 {
		stop()
		return nil, fmt.Errorf("unsupported adapter protocol version %d", hello.ProtocolVersion)
	}
	return &backend{raw: rawHello, conn: conn, hello: hello, close: stop}, nil
}

func Serve(c routing.Config) {
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	conn := connect(lifetime, c, os.Stdout, os.Stdin, false)
	<-conn.Done()
}

// ConnectEmbedded serves Petal's terminal client over its in-process pipes.
func ConnectEmbedded(ctx context.Context, c routing.Config, out io.Writer, in io.Reader) {
	connect(ctx, c, out, in, true)
}

func connect(lifetime context.Context, c routing.Config, out io.Writer, in io.Reader, embedded bool) *acp.Connection {
	p := proxy{embedded: embedded, Config: c, lifetime: lifetime, backends: make(map[string]*backend)}
	ready := make(chan struct{})
	p.editor = acp.NewConnection(func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		<-ready
		method, id := untag(method)
		if native, notification := strings.CutPrefix(method, notificationPrefix); notification {
			if native != "session/cancel" && native != "mcp/message" && !strings.HasPrefix(native, "_") {
				return nil, nil
			}
		} else if id != nil {
			if method == "session/cancel" || method == "mcp/message" {
				return nil, acp.NewInvalidRequest("expected a notification")
			}
			ctx = context.WithValue(ctx, editorRequestID{}, id)
		}
		result, err := p.handle(ctx, method, params)
		return result, rpcError(err)
	}, out, incoming(in, nil))
	if embedded {
		p.editor.SetLogger(slog.New(slog.DiscardHandler))
	}
	close(ready)
	return p.editor
}
