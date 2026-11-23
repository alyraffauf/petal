package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
)

func validImplementation(raw json.RawMessage) bool {
	var info struct {
		Name, Version *string
	}
	return json.Unmarshal(raw, &info) == nil && info.Name != nil && info.Version != nil
}

func (p *proxy) initializeV2(ctx context.Context, raw json.RawMessage, names []string, requested uint16) (any, bool, error) {
	var params Object
	_ = json.Unmarshal(raw, &params)
	validInfo := validImplementation(params["info"])
	if !validInfo {
		// Probe before validating version-specific fields. A v1 fallback
		// ignores v2 info, and future versions may use a different shape.
		params["info"] = json.RawMessage(`{"name":"petal","version":"0-unstable"}`)
	}
	if params.member("capabilities") == nil {
		delete(params, "capabilities")
	}
	delete(params, "clientInfo")
	delete(params, "clientCapabilities")
	params["protocolVersion"] = json.RawMessage("2")
	init, _ := json.Marshal(params)
	results := queryAccounts(names, func(name string) (*backend, error) { return p.startBackend(ctx, name, init, 2) })
	backends := make(map[string]*backend)
	var available []string
	var capabilities, metadata map[string]any
	auth := []Object{}
	for i, result := range results {
		b := result.value
		if b == nil {
			continue
		}
		name := names[i]
		backends[name] = b
		available = append(available, name)
		caps := decodeV2Capabilities(b.raw["capabilities"])
		meta := decodeMap(b.raw["_meta"])
		if len(available) == 1 {
			capabilities, metadata = caps, meta
		} else {
			intersectV2(capabilities, caps, v2CapabilityShape)
			intersectV2(metadata, meta, nil)
		}
		for _, method := range authMethodsV2(b) {
			id := stringField(method, "methodId")
			if stringField(method, "type") == "terminal" {
				if params.member("capabilities").member("auth").member("terminal") == nil {
					continue
				}
				var args []string
				_ = json.Unmarshal(method["args"], &args)
				method["args"] = encodeTerminalAuthArgs(name, p.Model, args)
			}
			wire, _ := json.Marshal([]string{name, id})
			method["methodId"] = encoded(string(wire))
			method["name"] = encoded(name + " · " + stringField(method, "name"))
			auth = append(auth, method)
		}
	}
	if len(available) == 0 {
		return nil, false, ctx.Err()
	}
	err := ctx.Err()
	if err == nil && requested == 2 && !validInfo {
		err = acp.NewInvalidParams("info requires name and version")
	}
	if err != nil {
		for _, b := range backends {
			b.close()
		}
		return nil, false, err
	}
	// A connection-wide query has no unique answer across multiple accounts.
	if len(available) > 1 {
		if authCaps, ok := capabilities["auth"].(map[string]any); ok {
			delete(authCaps, "status")
		}
		if sessionCaps, ok := capabilities["session"].(map[string]any); ok {
			delete(sessionCaps, "options")
		}
	}
	caps, _ := json.Marshal(capabilities)
	var negotiated Object
	_ = json.Unmarshal(caps, &negotiated)
	p.v2Capabilities = negotiated
	p.hasV2Auth = len(auth) != 0
	p.installBackends(2, available, backends, true)
	result := map[string]any{
		"protocolVersion": 2,
		"info":            map[string]string{"name": "petal", "version": "0-unstable", "title": "Petal"},
		"capabilities":    capabilities,
		"authMethods":     auth,
	}
	if len(metadata) > 0 {
		result["_meta"] = metadata
	}
	return result, true, nil
}

func decodeMap(raw json.RawMessage) map[string]any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if decoder.Decode(&object) != nil || object == nil {
		return map[string]any{}
	}
	return object
}

// The presence of an object enables a v2 capability. Normalize known
// optional fields before intersecting so malformed values cannot advertise it.
var v2CapabilityShape = map[string]any{
	"auth": nil,
	"session": map[string]any{
		"prompt":                map[string]any{"image": nil, "audio": nil, "embeddedContext": nil},
		"mcp":                   map[string]any{"stdio": nil, "http": nil},
		"delete":                nil,
		"additionalDirectories": nil,
		"systemPrompt":          map[string]any{"mode": map[string]any{"override": nil}},
	},
}

func decodeV2Capabilities(raw json.RawMessage) map[string]any {
	capabilities := decodeMap(raw)
	// These draft methods require proxy handlers before clients can use them.
	delete(capabilities, "providers")
	if session, ok := capabilities["session"].(map[string]any); ok {
		delete(session, "status")
		if mcp, ok := session["mcp"].(map[string]any); ok {
			delete(mcp, "acp")
		}
	}
	normalizeV2Capabilities(capabilities, v2CapabilityShape)
	return capabilities
}

func intersectV2(shared, other, shape map[string]any) {
	for name, value := range shared {
		if nested, known := shape[name]; known {
			if right, ok := other[name].(map[string]any); ok {
				children, _ := nested.(map[string]any)
				intersectV2(value.(map[string]any), right, children)
				continue
			}
		} else if name == "_meta" {
			left, leftOK := value.(map[string]any)
			right, rightOK := other[name].(map[string]any)
			if leftOK && rightOK {
				// Extension capability values are opaque at the key boundary.
				for key, extension := range left {
					if candidate, ok := right[key]; !ok || !reflect.DeepEqual(extension, candidate) {
						delete(left, key)
					}
				}
				continue
			}
		} else if candidate, ok := other[name]; ok && reflect.DeepEqual(value, candidate) {
			continue
		}
		delete(shared, name)
	}
}

func normalizeV2Capabilities(values, children map[string]any) {
	for name, shape := range children {
		object, ok := values[name].(map[string]any)
		if !ok {
			delete(values, name)
			continue
		}
		if nested, ok := shape.(map[string]any); ok {
			normalizeV2Capabilities(object, nested)
		}
	}
}

func authMethodsV2(b *backend) []Object {
	var methods []json.RawMessage
	_ = json.Unmarshal(b.raw["authMethods"], &methods)
	var valid []Object
	for _, raw := range methods {
		var method Object
		if json.Unmarshal(raw, &method) == nil && hasString(method, "methodId") && hasString(method, "name") && hasString(method, "type") {
			valid = append(valid, method)
		}
	}
	return valid
}

func hasString(object Object, key string) bool {
	var value *string
	return json.Unmarshal(object[key], &value) == nil && value != nil
}

func stringField(object Object, key string) string {
	var value string
	_ = json.Unmarshal(object[key], &value)
	return value
}

func (p *proxy) handleV2(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	nativeMethod, notification := strings.CutPrefix(method, notificationPrefix)
	if (nativeMethod == "session/cancel") != notification && !strings.HasPrefix(nativeMethod, "_") {
		if notification {
			return nil, nil
		}
		return nil, acp.NewInvalidRequest("expected a notification")
	}
	method = nativeMethod
	params := Object{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, acp.NewInvalidParams(err.Error())
		}
	}
	sessionCaps := p.v2Capabilities.member("session")
	switch method {
	case "auth/status":
		if len(p.accounts) != 1 || p.v2Capabilities.member("auth").member("status") == nil {
			return nil, acp.NewMethodNotFound(method)
		}
		return p.relayV2Backend(ctx, p.backends[p.accounts[0]], method, params, false)
	case "auth/login", "auth/logout":
		if !p.hasV2Auth {
			return nil, acp.NewMethodNotFound(method)
		}
		if method == "auth/login" {
			return p.loginV2(ctx, params)
		}
		return p.logoutV2(ctx, params)
	case "session/options", "session/delete", "session/fork", "session/archive", "session/unarchive", "session/set_title":
		capability := strings.TrimPrefix(method, "session/")
		switch method {
		case "session/unarchive":
			capability = "archive"
		case "session/set_title":
			capability = "title"
		}
		if sessionCaps.member(capability) == nil {
			return nil, acp.NewMethodNotFound(method)
		}
		if method == "session/options" && params["sessionId"] == nil {
			return p.relayV2Backend(ctx, p.backends[p.accounts[0]], method, params, false)
		}
	case "session/new", "session/list", "session/resume", "session/close", "session/prompt",
		"session/set_config_option", "session/cancel", "tool_call/cancel", "terminal/cancel":
	default:
		if !strings.HasPrefix(method, "_") {
			return nil, acp.NewMethodNotFound(method)
		}
		if params["sessionId"] == nil {
			if len(p.accounts) == 1 {
				return p.relayV2Backend(ctx, p.backends[p.accounts[0]], method, params, notification)
			}
			return nil, acp.NewMethodNotFound(method)
		}
	}
	if !strings.HasPrefix(method, "_") && sessionCaps == nil {
		return nil, acp.NewMethodNotFound(method)
	}
	if method == "session/list" {
		return p.list(ctx, params)
	}
	if method == "session/new" || method == "session/resume" || method == "session/fork" {
		if err := validateV2SessionSetup(params, sessionCaps); err != nil {
			return nil, err
		}
	}

	var name, public, canonical string
	if method == "session/new" {
		var err error
		name, err = p.newSessionAccount(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		if !hasString(params, "sessionId") {
			return nil, acp.NewInvalidParams("sessionId must be a string")
		}
		public = stringField(params, "sessionId")
		parts := decodeRouteID(public, 3)
		if parts == nil || parts[1] != p.Model || p.backends[parts[0]] == nil {
			return missingSessionV2(method, public)
		}
		name = parts[0]
		canonical = p.publicID(name, parts[2])
		params["sessionId"] = encoded(parts[2])
	}

	if method == "session/prompt" {
		if err := validateV2Prompt(params["prompt"], sessionCaps.member("prompt")); err != nil {
			return nil, err
		}
	}

	b := p.backends[name]
	var endpoint *delegationEndpoint
	if method == "session/new" || method == "session/resume" || method == "session/fork" {
		backendMCP := b.raw.member("capabilities").member("session").member("mcp")
		var configured []routing.MCPServer
		var err error
		endpoint, configured, err = p.prepareDelegation(b.wire.lifetime, params, method, canonical, backendMCP.member("stdio") != nil)
		if err != nil {
			return nil, err
		}
		if endpoint != nil {
			defer endpoint.closeIfUnbound()
		}
		params, err = mergeMCPServersV2(params, configured, sessionCaps.member("mcp"), backendMCP)
		if err != nil {
			return nil, err
		}
	}
	if method == "session/prompt" && p.delegation != nil {
		if e := p.endpoint(canonical); e != nil {
			e.wakes.interrupt(false)
		}
	}
	if method == "session/cancel" {
		p.cancelDelegation(canonical)
	}
	closingEndpoint := p.delegationEndpointToClose(method, canonical)
	finishTranscript, err := p.beforeTranscript(method, name, stringField(params, "sessionId"), canonical, params)
	if err != nil {
		return nil, err
	}
	b.trackSessionStartup(method, params)
	doneTrace := traceAdapter(ctx, b.wire, method, params)
	result, err := p.relayV2Backend(ctx, b, method, params, notification)
	doneTrace(result, err)
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	t.event("transcript_finish_started")
	err = finishTranscript(err)
	t.event("transcript_finish_returned", "ok", err == nil)
	if err != nil {
		var rpc *acp.RequestError
		if public != "" && errors.As(err, &rpc) && rpc.Code == -32002 {
			if method == "session/cancel" {
				return nil, nil
			}
			if method == "session/close" || method == "session/delete" {
				return Object{}, nil
			}
			var data Object
			if json.Unmarshal(rpc.Data, &data) != nil || data == nil {
				data = Object{}
			}
			data["sessionId"] = encoded(public)
			return nil, acp.NewRequestError(rpc.Code, rpc.Message, data)
		}
		if method == "session/prompt" {
			routing.RememberRateLimit(name, err)
		}
		return nil, err
	}
	b.forgetClosedSession(method, params)
	p.finishDelegationClose(canonical, closingEndpoint)
	if method == "session/fork" {
		var fork Object
		if json.Unmarshal(result, &fork) != nil || !hasString(fork, "sessionId") {
			return nil, fmt.Errorf("adapter did not return forked sessionId")
		}
		id := stringField(fork, "sessionId")
		if id == "" {
			return nil, fmt.Errorf("empty forked sessionId")
		}
		routed := p.publicID(name, id)
		if err := p.reserveLocalRoute(routed); err != nil {
			return nil, err
		}
		if endpoint != nil {
			if err := p.bindDelegation(endpoint, name, routed); err != nil {
				return nil, err
			}
		}
		fork["sessionId"] = encoded(routed)
		if p.recorder != nil {
			p.recorder.open(routed, routed, stringField(params, "cwd"), name, p.Model)
		}
		return fork, nil
	}
	if method != "session/new" {
		if endpoint != nil {
			if err := p.bindDelegation(endpoint, name, canonical); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
	session, err := p.newSessionV2(ctx, b, name, result)
	if err == nil && endpoint != nil {
		if err := p.bindDelegation(endpoint, name, stringField(session, "sessionId")); err != nil {
			return nil, err
		}
	}
	if err == nil && p.recorder != nil {
		id := stringField(session, "sessionId")
		p.recorder.open(id, id, stringField(params, "cwd"), name, p.Model)
	}
	return session, err
}

func decodeRouteID(id string, size int) []string {
	var fields []*string
	if json.Unmarshal([]byte(id), &fields) != nil || len(fields) != size {
		return nil
	}
	parts := make([]string, len(fields))
	for i, field := range fields {
		if field == nil {
			return nil
		}
		parts[i] = *field
	}
	return parts
}

func missingSessionV2(method, id string) (any, error) {
	if method == "session/close" || method == "session/delete" || method == "session/cancel" {
		return Object{}, nil
	}
	return nil, unknownSession(id)
}

func validateV2SessionSetup(params, caps Object) error {
	if !filepath.IsAbs(stringField(params, "cwd")) {
		return acp.NewInvalidParams("cwd must be an absolute path")
	}
	if caps.member("additionalDirectories") == nil {
		delete(params, "additionalDirectories")
	} else if data := params["additionalDirectories"]; data != nil {
		var dirs []string
		if json.Unmarshal(data, &dirs) != nil {
			return acp.NewInvalidParams("additionalDirectories must be an array")
		}
		for _, dir := range dirs {
			if !filepath.IsAbs(dir) {
				return acp.NewInvalidParams("additionalDirectories must contain absolute paths")
			}
		}
	}
	if data := params["systemPrompt"]; data != nil && !bytes.Equal(data, []byte("null")) {
		promptCaps := caps.member("systemPrompt")
		mode := stringField(params.member("systemPrompt"), "mode")
		if promptCaps == nil || (mode != "append" && (mode != "override" || promptCaps.member("mode").member("override") == nil)) {
			return acp.NewInvalidParams("unsupported systemPrompt mode")
		}
	}
	return nil
}

func validateV2Prompt(raw json.RawMessage, caps Object) error {
	var blocks []Object
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return acp.NewInvalidParams("prompt must be an array")
	}
	for _, block := range blocks {
		if !hasString(block, "type") {
			return acp.NewInvalidParams("prompt blocks require a type")
		}
		kind := stringField(block, "type")
		switch kind {
		case "image", "audio":
			if caps.member(kind) == nil {
				return acp.NewInvalidParams("unsupported prompt content type " + kind)
			}
		case "resource":
			if caps.member("embeddedContext") == nil {
				return acp.NewInvalidParams("unsupported prompt content type " + kind)
			}
		}
	}
	return nil
}

func (p *proxy) newSessionV2(ctx context.Context, b *backend, name string, result json.RawMessage) (Object, error) {
	var session Object
	if json.Unmarshal(result, &session) != nil || !hasString(session, "sessionId") {
		return nil, fmt.Errorf("adapter did not return sessionId")
	}
	native := stringField(session, "sessionId")
	id := p.publicID(name, native)
	if err := p.reserveLocalRoute(id); err != nil {
		return nil, err
	}
	session["sessionId"] = encoded(id)
	if p.Accounts[name].SelectModel[p.Model] {
		opts, err := forward[Object](b.conn, ctx, "session/set_config_option", map[string]string{
			"sessionId": native, "configId": "model", "type": "id", "value": p.Model,
		})
		if err != nil {
			return nil, fmt.Errorf("select %s on %s: %w", p.Model, name, err)
		}
		if opts["configOptions"] != nil {
			session["configOptions"] = opts["configOptions"]
		}
	}
	return session, nil
}

func (p *proxy) loginV2(ctx context.Context, params Object) (any, error) {
	method := decodeRouteID(stringField(params, "methodId"), 2)
	if method == nil {
		return nil, acp.NewInvalidParams("unknown authentication method")
	}
	if b := p.backends[method[0]]; b != nil {
		for _, candidate := range authMethodsV2(b) {
			kind := stringField(candidate, "type")
			if kind != "terminal" && stringField(candidate, "methodId") == method[1] {
				params["methodId"] = encoded(method[1])
				return p.relayV2Backend(ctx, b, "auth/login", params, false)
			}
		}
	}
	return nil, acp.NewInvalidParams("unknown agent authentication method")
}

func (p *proxy) logoutV2(ctx context.Context, params Object) (any, error) {
	called := false
	for _, name := range p.accounts {
		b := p.backends[name]
		if len(authMethodsV2(b)) == 0 {
			continue
		}
		called = true
		if _, err := p.relayV2Backend(ctx, b, "auth/logout", params, false); err != nil {
			return nil, err
		}
	}
	if !called {
		return nil, acp.NewMethodNotFound("auth/logout")
	}
	return Object{}, nil
}

func (p *proxy) relayV2Backend(ctx context.Context, b *backend, method string, params Object, notification bool) (json.RawMessage, error) {
	if notification {
		return nil, b.conn.SendNotification(ctx, method, params)
	}
	id, _ := ctx.Value(editorRequestID{}).(json.RawMessage)
	return forwardV2(b.conn, b.wire, ctx, p.lifetime, method, params, id)
}
