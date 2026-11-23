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
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/agentmcp"
	"petal/internal/delegation"
	"petal/internal/processgroup"
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
	promptedSessions sync.Map // native session IDs that have received a prompt on this connection
	close            context.CancelFunc
	exited           <-chan struct{}
	ready            atomic.Bool
	raw              Object
	conn             *acp.Connection
	wire             *adapterWire
	hello            acp.InitializeResponse
}
type proxy struct {
	recorder       *transcriptRecorder
	editorSessions *editorSessions

	embedded      bool
	editorUpdates chan struct{}
	local         *localRuntime
	routing.Config
	lifetime            context.Context
	editor              *acp.Connection
	editorWire          *adapterWire
	mu                  sync.Mutex
	aliases             sync.Map // canonical backend ID to the editor ID for an explicit resume override
	initializing        bool
	initialized         bool
	repeatInitialize    bool
	used                bool
	version             uint16
	v2Capabilities      Object
	hasV2Auth           bool
	backends            map[string]*backend
	accounts            []string
	capabilities        acp.AgentCapabilities
	delegation          *agentmcp.Tools
	delegationRuntime   *delegation.Runtime
	delegationMu        sync.Mutex
	delegationEndpoints map[string]*delegationEndpoint
	delegationCwds      map[string]string
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
	if errors.Is(err, context.Canceled) {
		return acp.NewRequestCancelled(nil)
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
	notification := strings.HasPrefix(method, notificationPrefix)
	ready, version := p.initialized && !p.initializing, p.version
	if ready && !notification {
		p.used = true
	}
	// Notifications can arrive before the first request, while the selected
	// backends are still replaceable by a repeated initialize.
	if ready && notification && !p.used {
		defer p.mu.Unlock()
	} else {
		p.mu.Unlock()
	}
	if !ready {
		if notification {
			return nil, nil
		}
		return nil, acp.NewInvalidRequest("initialize first")
	}
	if version == 2 {
		return p.handleV2(ctx, method, raw)
	}
	if native, notification := strings.CutPrefix(method, notificationPrefix); notification {
		if native != "session/cancel" && native != "mcp/message" && !strings.HasPrefix(native, "_") {
			return nil, nil
		}
	} else if method == "session/cancel" || method == "mcp/message" {
		return nil, acp.NewInvalidRequest("expected a notification")
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
	var name, native, public, delegationParent string
	if method == "session/new" {
		var err error
		name, err = p.newSessionAccount(ctx)
		if err != nil {
			return nil, err
		}
	} else if _, exists := params["sessionId"]; exists {
		id, err := SessionID(params)
		if err != nil {
			return nil, err
		}
		parts := DecodeSessionRoute(id)
		if parts == nil || parts[1] != p.Model || parts[2] == "" {
			if method == "session/delete" || method == "session/close" {
				return Object{}, nil
			}
			return nil, unknownSession(id)
		}
		public = id
		name, native = parts[0], parts[2]
		// The pin chooses the adapter. Delegation follows the editor's saved session.
		delegationParent = p.publicID(name, native)
		if p.PinnedAccount != "" && name != p.PinnedAccount {
			canonical := p.publicID(p.PinnedAccount, native)
			if method == "session/load" || method == "session/resume" {
				if err := p.reserveLocalRoute(canonical); err != nil {
					return nil, err
				}
				p.aliases.Store(canonical, id)
			} else if existing, ok := p.aliases.Load(canonical); !ok || !slices.Equal(decodeRouteID(existing.(string), 3), parts) {
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
	var endpoint *delegationEndpoint
	switch method {
	case "session/new", "session/load", "session/resume", "session/fork":
		var configured []routing.MCPServer
		var err error
		endpoint, configured, err = p.prepareDelegation(b.wire.lifetime, params, method, delegationParent, true)
		if err != nil {
			return nil, err
		}
		if endpoint != nil {
			defer endpoint.closeIfUnbound()
		}
		params, err = mergeMCPServers(params, configured, b.hello.AgentCapabilities.McpCapabilities.Http)
		if err != nil {
			return nil, err
		}
	}
	if strings.HasPrefix(method, notificationPrefix) {
		if method == notificationPrefix+"session/cancel" {
			p.cancelDelegation(delegationParent)
		}
		return relay(ctx, b.conn, method, params)
	}
	closingEndpoint := p.delegationEndpointToClose(method, delegationParent)
	var promptEndpoint *delegationEndpoint
	var wakeCtx context.Context
	if p.delegation != nil && method == "session/prompt" {
		promptEndpoint = p.endpoint(delegationParent)
		if promptEndpoint != nil {
			var cancel context.CancelFunc
			wakeCtx, cancel = promptEndpoint.waitContext(ctx)
			defer cancel()
			stop := context.AfterFunc(promptEndpoint.ctx, cancel)
			defer stop()
		}
	}
	finishTranscript, err := p.beforeTranscript(method, name, native, delegationParent, params)
	if err != nil {
		return nil, err
	}
	b.trackSessionStartup(method, params)
	doneTrace := traceAdapter(ctx, b.wire, method, params)
	result, err := forward[json.RawMessage](b.conn, ctx, method, params)
	doneTrace(result, err)
	if p.version == 1 && method == "_session/steering" && err == nil && p.recorder != nil && SteeringInjected(result) {
		p.recorder.recordSteering(p.publicID(name, native), params)
	}
	if promptEndpoint != nil && err == nil {
		result = promptEndpoint.continueV1(wakeCtx, b, native, result)
		t, _ := ctx.Value(traceKey{}).(*requestTrace)
		t.event("child_wait_returned")
	}
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	t.event("transcript_finish_started")
	err = finishTranscript(err)
	t.event("transcript_finish_returned", "ok", err == nil)
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
	b.forgetClosedSession(method, params)
	p.finishDelegationClose(delegationParent, closingEndpoint)
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
	if method == "session/new" || method == "session/fork" || session["sessionId"] != nil {
		returnedNative, err := SessionID(session)
		if err != nil {
			return nil, err
		}
		if (method == "session/load" || method == "session/resume") && returnedNative == native {
			session["sessionId"] = encoded(public)
		} else {
			delegationParent = p.publicID(name, returnedNative)
			if method != "session/fork" {
				if err := p.reserveLocalRoute(delegationParent); err != nil {
					return nil, err
				}
			}
			session["sessionId"] = encoded(delegationParent)
		}
		native = returnedNative
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
	if endpoint != nil && (method != "session/fork" || p.local == nil) {
		if err := p.bindDelegation(endpoint, name, delegationParent); err != nil {
			return nil, err
		}
	}
	if p.recorder != nil && (method == "session/new" || method == "session/fork" && p.local == nil) {
		id := stringField(session, "sessionId")
		p.recorder.open(id, id, stringField(params, "cwd"), name, p.Model)
	}
	return session, nil
}

func (p *proxy) newSessionAccount(ctx context.Context) (string, error) {
	if len(p.accounts) == 0 {
		return "", fmt.Errorf("no subscription offers %q", p.Model)
	}
	if len(p.accounts) > 1 {
		if chosen, err := p.choose(ctx); err == nil {
			return chosen, nil
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	// Opening a session consumes no model quota. The provider enforces its limit on prompt.
	return p.accounts[0], nil
}

func (p *proxy) choose(ctx context.Context) (string, error) {
	config := p.Config
	config.Accounts = make(map[string]routing.Account, len(p.accounts))
	for _, name := range p.accounts {
		config.Accounts[name] = p.Accounts[name]
	}
	return config.Choose(ctx)
}

func (p *proxy) publicID(name, native string) string {
	b, _ := json.Marshal([]string{name, p.Model, native})
	return string(b)
}

// DecodeSessionRoute returns the account, model, and native session ID in wire order.
// Legacy routes use encoding/json's string decoding, which accepts null as "".
func DecodeSessionRoute(id string) []string {
	var parts []string
	if json.Unmarshal([]byte(id), &parts) != nil || len(parts) != 3 {
		return nil
	}
	return parts
}

func (p *proxy) editorID(name, native string) string {
	id := p.publicID(name, native)
	if alias, ok := p.aliases.Load(id); ok {
		return alias.(string)
	}
	return id
}

func (p *proxy) startBackend(ctx context.Context, name string, init json.RawMessage, version uint16) (*backend, error) {
	return p.startBackendWithReceiver(ctx, name, init, version, nil)
}

func (p *proxy) startBackendWithReceiver(ctx context.Context, name string, init json.RawMessage, version uint16, receive delegation.Receive) (*backend, error) {
	var request Object
	if err := json.Unmarshal(init, &request); err != nil {
		return nil, acp.NewInvalidParams(err.Error())
	}
	capabilityField := "clientCapabilities"
	if version == 2 {
		capabilityField = "capabilities"
	}
	clientCaps := request.member(capabilityField)

	// The recorder consumes v1 compaction events even when the editor does
	// not. Filter forwarded updates using the editor's original capabilities.
	if version == 1 && p.recorder != nil && clientCaps.member("session").member("compaction") == nil {
		caps := request.member(capabilityField)
		if caps == nil {
			caps = Object{}
		}
		session := caps.member("session")
		if session == nil {
			session = Object{}
		}
		session["compaction"] = json.RawMessage("{}")
		caps["session"], _ = json.Marshal(session)
		request[capabilityField], _ = json.Marshal(caps)
		init, _ = json.Marshal(request)
	}

	a := p.Accounts[name]
	lifetime, stop := context.WithCancel(p.lifetime)
	cmd := a.Command(lifetime, a.Models[p.Model])
	if p.local != nil {
		cmd.Dir = p.local.cwd
		cmd.Env = append(cmd.Env, "PWD="+p.local.cwd)
	}
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
	if p.local != nil {
		if err := p.local.reserve(); err != nil {
			stop()
			in.Close()
			out.Close()
			childOutput.Close()
			return nil, err
		}
	}
	group, err := processgroup.Start(cmd)
	if err != nil {
		if p.local != nil {
			p.local.release()
		}
		childOutput.Close()
		stop()
		in.Close()
		out.Close()
		return nil, err
	}
	childOutput.Close()
	context.AfterFunc(lifetime, func() { out.Close(); in.Close() })
	exited := make(chan struct{})
	b := &backend{close: stop, exited: exited}
	wire := &adapterWire{Writer: in, lifetime: lifetime, updates: make(chan struct{}, 128), nativeV2: version == 2}
	if p.local != nil {
		wire.dispatched = func(id json.RawMessage) {
			if signal, ok := p.local.dispatch.LoadAndDelete(string(id)); ok {
				signal.(func())()
			}
		}
	}
	reader := incoming(out, wire)
	if p.editorWire != nil && p.editorWire.traceLog != nil {
		reader.traceUpdates = p.editorWire.traceLog
	}
	if receive == nil && p.recorder != nil && (p.local == nil || version != 2) {
		reader.observeUpdate = func(raw json.RawMessage) {
			var args Object
			if json.Unmarshal(raw, &args) == nil {
				p.recorder.observe(p.publicID(name, stringField(args, "sessionId")), version, args.member("update"))
			}
		}
	}
	if receive == nil && p.local != nil && version == 2 {
		// Apply daemon updates before the reader exposes a subsequent ACP
		// response. Resume validation and archive checkpoints use this order.
		reader.sessionUpdate = func(raw json.RawMessage) {
			if b.ready.Load() {
				_, _ = p.relayV2ClientCall(lifetime, name, notificationPrefix+"session/update", raw, clientCaps, lifetime)
			}
		}
	}
	if receive != nil {
		reader.sessionUpdate = func(raw json.RawMessage) {
			_, _ = receive(lifetime, "session/update", p.routeChildParams(name, raw), true)
		}
	}
	conn := acp.NewConnection(func(ctx context.Context, method string, raw json.RawMessage) (any, *acp.RequestError) {
		if receive != nil {
			method, _ = untag(method)
			native, notification := strings.CutPrefix(method, notificationPrefix)
			result, err := receive(ctx, native, p.routeChildParams(name, raw), notification)
			return result, rpcError(err)
		}
		native, notification := strings.CutPrefix(method, notificationPrefix)
		if notification && native == "session/update" {
			defer func() { <-wire.updates }()
		}
		if version == 2 && !b.ready.Load() {
			return nil, acp.NewInvalidRequest("initialize first")
		}
		callback, _ := untag(method)
		if callback == "elicitation/create" && clientCaps.member("elicitation").member("url") != nil && b.startupURL(raw) {
			if reader.traceUpdates != nil {
				reader.traceUpdates.Info("startup_url_elicitation_cancelled", "account", name)
			}
			return map[string]string{"action": "cancel"}, nil
		}
		if version == 2 {
			return p.relayV2ClientCall(ctx, name, method, raw, clientCaps, lifetime)
		}
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
		if native == "elicitation/create" || native == "elicitation/complete" {
			var id string
			if json.Unmarshal(args["elicitationId"], &id) == nil && id != "" {
				args["elicitationId"] = encoded(p.publicID(name, id))
			}
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
				var updateKind string
				_ = json.Unmarshal(item["sessionUpdate"], &updateKind)
				switch updateKind {
				case "config_option_update":
					if strings.HasPrefix(stringField(args, "sessionId"), "petal-") && item["configOptions"] != nil {
						item["configOptions"] = p.editorOptions(item["configOptions"], false)["configOptions"]
						changed = true
					}
				case "subagent_update", "session_message":
					for _, key := range []string{"sessionId", "senderSessionId", "recipientSessionId"} {
						var id string
						if json.Unmarshal(item[key], &id) == nil && id != "" {
							item[key] = encoded(p.editorID(name, id))
							changed = true
						}
					}
				}
				if field == "toolCall" || updateKind == "tool_call" || updateKind == "tool_call_update" {
					var parts []Object
					if json.Unmarshal(item["content"], &parts) == nil {
						visible := parts[:0]
						for _, part := range parts {
							var contentType string
							_ = json.Unmarshal(part["type"], &contentType)
							if contentType == "session" {
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
		if notification && native == "session/update" && p.editorUpdates != nil {
			// The embedded client releases this slot after consuming the update.
			select {
			case p.editorUpdates <- struct{}{}:
			case <-lifetime.Done():
				return nil, rpcError(lifetime.Err())
			}
			err := p.editor.SendNotification(ctx, native, args)
			if err != nil {
				<-p.editorUpdates
			}
			return nil, rpcError(err)
		}
		result, err := relay(ctx, p.editor, method, args)
		return result, rpcError(err)
	}, wire, reader)
	wire.conn = conn
	if p.embedded {
		conn.SetLogger(slog.New(slog.DiscardHandler))
	}
	go func() {
		if p.local != nil {
			defer p.local.release()
		}
		_ = group.Wait()
		close(exited)
		// A surviving helper may still hold stdout. Allow the reader to drain
		// buffered frames, then terminate any blocked read.
		_ = out.SetReadDeadline(time.Now().Add(time.Second))
		if p.local != nil {
			<-conn.Done()
		}
	}()
	go func() { <-conn.Done(); stop() }()
	rawHello, err := forward[Object](conn, ctx, "initialize", init)
	if err != nil {
		stop()
		return nil, err
	}
	b.raw, b.conn, b.wire = rawHello, conn, wire
	if version == 2 {
		var selected uint16
		if json.Unmarshal(rawHello["protocolVersion"], &selected) != nil || selected != 2 {
			stop()
			return nil, fmt.Errorf("adapter did not negotiate ACP v2")
		}
		if !validImplementation(rawHello["info"]) {
			stop()
			return nil, fmt.Errorf("adapter returned invalid ACP v2 implementation info")
		}
		return b, nil
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
	b.hello = hello
	return b, nil
}

func Serve(c routing.Config) {
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	conn := connect(lifetime, c, os.Stdout, os.Stdin, false, nil)
	<-conn.Done()
}

// ConnectEmbedded serves Petal's terminal client over its in-process pipes.
func ConnectEmbedded(ctx context.Context, c routing.Config, out io.Writer, in io.Reader, updates chan struct{}) {
	connect(ctx, c, out, in, true, updates)
}

func connect(lifetime context.Context, c routing.Config, out io.Writer, in io.Reader, embedded bool, updates chan struct{}) *acp.Connection {
	return connectProxy(lifetime, out, in, c, embedded, updates, nil).editor
}

func connectProxy(lifetime context.Context, out io.Writer, in io.Reader, c routing.Config, embedded bool, updates chan struct{}, local *localRuntime) *proxy {
	lifetime, stop := context.WithCancel(lifetime)
	editorWire := &adapterWire{Writer: out, lifetime: lifetime}
	enableTrace(lifetime, editorWire)
	p := proxy{local: local, editorWire: editorWire, editorUpdates: updates, embedded: embedded, Config: c, lifetime: lifetime, backends: make(map[string]*backend)}
	if !embedded || local != nil {
		p.recorder = &transcriptRecorder{}
	}
	if !embedded && c.Model == "" {
		p.Model = c.DefaultModel()
		if len(c.ModelNames()) > 1 {
			p.editorSessions = &editorSessions{routes: make(map[string]*proxy)}
			// The catalog can route successive sessions to different adapters.
			// Stock Claude and Codex share v1; a model-bound entry can negotiate v2.
			var claude, codex bool
			for name, account := range c.Accounts {
				if c.PinnedAccount != "" && c.PinnedAccount != name {
					continue
				}
				claude = claude || account.Provider == "claude"
				codex = codex || account.Provider == "codex"
			}
			p.editorSessions.commonV1 = claude && codex
		}
	}
	p.configureDelegation()
	ready := make(chan struct{})
	reader := incoming(in, nil)
	reader.editor = editorWire
	p.editor = acp.NewConnection(func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		<-ready
		method, id := untag(method)
		ctx = editorWire.traceRequest(ctx, method, id, params)
		if id != nil {
			ctx = context.WithValue(ctx, editorRequestID{}, id)
		}
		var result any
		var err error
		if p.editorSessions != nil {
			result, err = p.handleEditor(ctx, method, params)
		} else {
			result, err = p.handle(ctx, method, params)
		}
		return result, rpcError(err)
	}, editorWire, reader)
	editorWire.conn = p.editor
	go func() { <-p.editor.Done(); stop() }()
	if embedded {
		p.editor.SetLogger(slog.New(slog.DiscardHandler))
	}
	close(ready)
	return &p
}
