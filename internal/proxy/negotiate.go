package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// Only advertise options that every account eligible for a new thread supports.
func intersect(dst, src map[string]any) {
	for k, v := range dst {
		x, ok := src[k]
		if !ok {
			delete(dst, k)
			continue
		}
		a, aok := v.(map[string]any)
		b, bok := x.(map[string]any)
		if aok && bok {
			intersect(a, b)
		} else if !reflect.DeepEqual(v, x) {
			delete(dst, k)
		}
	}
}

type accountResult[T any] struct {
	value T
	err   error
}

// Start independent account requests together but merge in stable account order.
func queryAccounts[T any](names []string, call func(string) (T, error)) []accountResult[T] {
	results := make([]accountResult[T], len(names))
	var group sync.WaitGroup
	for i, name := range names {
		group.Go(func() { results[i].value, results[i].err = call(name) })
	}
	group.Wait()
	return results
}

func (p *proxy) initialize(ctx context.Context, raw json.RawMessage) (any, error) {
	p.mu.Lock()
	if p.initializing || p.used || (p.initialized && !p.repeatInitialize) {
		p.mu.Unlock()
		return nil, acp.NewInvalidRequest("already initialized")
	}
	p.initializing = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.initializing = false; p.mu.Unlock() }()

	var request struct {
		Version *uint16 `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || request.Version == nil {
		return nil, acp.NewInvalidParams("protocolVersion must be a uint16")
	}
	if *request.Version == 0 {
		supported := []uint16{1}
		if p.ACPv2 {
			supported = append(supported, 2)
		}
		return nil, acp.NewRequestError(-32022, "Unsupported protocol version", map[string]any{
			"requested": *request.Version, "supported": supported,
		})
	}

	var names []string
	for _, name := range slices.Sorted(maps.Keys(p.Accounts)) {
		if len(p.Accounts[name].Models[p.Model]) > 0 && (p.PinnedAccount == "" || name == p.PinnedAccount) {
			names = append(names, name)
		}
	}
	if p.ACPv2 && *request.Version >= 2 && (p.editorSessions == nil || !p.editorSessions.commonV1) {
		result, selected, err := p.initializeV2(ctx, raw, names, *request.Version)
		if selected || err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := p.initializeV1(ctx, raw, names, *request.Version > 1)
	var unavailable *unavailableAdapters
	if p.editorSessions == nil || !errors.As(err, &unavailable) {
		return result, err
	}
	original := p.Model
	for _, model := range p.ModelNames() {
		if model == original {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		var alternatives []string
		for _, name := range slices.Sorted(maps.Keys(p.Accounts)) {
			if (p.PinnedAccount == "" || p.PinnedAccount == name) && len(p.Accounts[name].Models[model]) > 0 && !slices.Equal(p.Accounts[name].Models[model], p.Accounts[name].Models[original]) {
				alternatives = append(alternatives, name)
			}
		}
		if len(alternatives) == 0 {
			continue
		}
		p.Model = model
		result, trialErr := p.initializeV1(ctx, raw, alternatives, *request.Version > 1)
		if trialErr == nil {
			return result, nil
		}
	}
	p.Model = original
	return nil, err
}

func (p *proxy) installBackends(version uint16, names []string, backends map[string]*backend, repeat bool) {
	p.mu.Lock()
	previous := p.backends
	p.editorWire.selectedV2.Store(version == 2)
	p.version, p.accounts, p.backends = version, names, backends
	p.repeatInitialize, p.initialized = repeat, true
	for _, b := range backends {
		b.ready.Store(true)
	}
	for _, b := range previous {
		b.ready.Store(false)
		b.close()
	}
	p.mu.Unlock()
}

type unavailableAdapters struct{ error }

func (e *unavailableAdapters) Unwrap() error { return e.error }

func (p *proxy) initializeV1(ctx context.Context, raw json.RawMessage, names []string, fallback bool) (any, error) {
	var params Object
	_ = json.Unmarshal(raw, &params)
	if fallback {
		// The offered version determines which initialization fields are sent.
		delete(params, "info")
		delete(params, "capabilities")
	}
	params["protocolVersion"] = json.RawMessage("1")
	init, _ := json.Marshal(params)
	var req acp.InitializeRequest
	if err := json.Unmarshal(init, &req); err != nil {
		if !fallback {
			return nil, acp.NewInvalidParams(err.Error())
		}
		delete(params, "clientInfo")
		delete(params, "clientCapabilities")
		init, _ = json.Marshal(params)
		req = acp.InitializeRequest{}
	}
	var caps, meta map[string]any
	auth := []Object{}
	results := queryAccounts(names, func(name string) (*backend, error) { return p.startBackend(ctx, name, init, 1) })
	backends := make(map[string]*backend)
	var available []string
	committed := false
	defer func() {
		if !committed {
			for _, r := range results {
				if r.value != nil {
					r.value.close()
				}
			}
		}
	}()
	var failures []error
	for i, result := range results {
		name, b := names[i], result.value
		if result.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, result.err))
			continue
		}
		backends[name] = b
		available = append(available, name)
		c := map[string]any{}
		if data := b.raw["agentCapabilities"]; data != nil {
			if err := json.Unmarshal(data, &c); err != nil {
				return nil, err
			}
		}
		bmeta := map[string]any{}
		_ = json.Unmarshal(b.raw["_meta"], &bmeta)
		if len(available) == 1 {
			caps, meta = c, bmeta
		} else {
			intersect(caps, c)
			intersect(meta, bmeta)
		}
		for _, method := range b.hello.AuthMethods {
			data, _ := json.Marshal(method)
			var m Object
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, err
			}
			var id, label string
			if json.Unmarshal(m["id"], &id) != nil {
				continue
			}
			if method.Terminal != nil {
				if !req.ClientCapabilities.Auth.Terminal {
					continue
				}
				m["args"] = encodeTerminalAuthArgs(name, p.Model, method.Terminal.Args)
			}
			_ = json.Unmarshal(m["name"], &label)
			wire, _ := json.Marshal([]string{name, id})
			m["id"] = encoded(string(wire))
			m["name"] = encoded(name + " · " + label)
			auth = append(auth, m)
		}
	}
	if len(available) == 0 && len(failures) > 0 {
		return nil, &unavailableAdapters{errors.Join(failures...)}
	}
	if caps == nil {
		caps = map[string]any{}
	}
	data, err := json.Marshal(caps)
	if err != nil {
		return nil, err
	}
	var negotiated acp.AgentCapabilities
	if err := json.Unmarshal(data, &negotiated); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.capabilities = negotiated
	p.installBackends(1, available, backends, fallback || p.ACPv2)
	committed = true
	result := map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "petal", "version": "0-unstable", "title": "Petal"}, "agentCapabilities": caps, "authMethods": auth}
	if len(meta) > 0 {
		result["_meta"] = meta
	}
	if p.editorSessions != nil {
		sessionCaps, ok := caps["sessionCapabilities"].(map[string]any)
		if !ok {
			sessionCaps = map[string]any{}
			caps["sessionCapabilities"] = sessionCaps
		}
		sessionCaps["list"] = map[string]any{}
	}
	if len(p.accounts) > 1 {
		delete(caps, "providers")
	}
	return result, nil
}

// Each page belongs to one account. Its native cursor stays opaque.
type sessionCursor struct {
	account string
	native  *string
}

func decodeSessionCursor(raw json.RawMessage, nativeV2 bool) (sessionCursor, error) {
	var cursor sessionCursor
	if raw == nil || string(raw) == "null" {
		return cursor, nil
	}
	var key string
	var fields []json.RawMessage
	if json.Unmarshal(raw, &key) != nil || json.Unmarshal([]byte(key), &fields) != nil ||
		len(fields) != 2 || json.Unmarshal(fields[0], &cursor.account) != nil || cursor.account == "" ||
		json.Unmarshal(fields[1], &cursor.native) != nil || (!nativeV2 && cursor.native == nil) {
		return sessionCursor{}, acp.NewInvalidParams("invalid session cursor")
	}
	if !nativeV2 && *cursor.native == "" {
		cursor.native = nil
	}
	return cursor, nil
}

// Each page belongs to one account. Its native cursor stays opaque.
func (p *proxy) list(ctx context.Context, params Object) (any, error) {
	if params == nil {
		params = Object{}
	}
	nativeV2 := p.version == 2
	cursor, err := decodeSessionCursor(params["cursor"], nativeV2)
	if err != nil || (cursor.account != "" && p.backends[cursor.account] == nil) {
		return nil, acp.NewInvalidParams("invalid session cursor")
	}
	index := sort.SearchStrings(p.accounts, cursor.account)
	if index >= len(p.accounts) {
		return Object{"sessions": json.RawMessage("[]")}, nil
	}
	return p.accountPage(ctx, params, p.accounts, index, cursor)
}

func (p *proxy) accountPage(ctx context.Context, params Object, accounts []string, index int, cursor sessionCursor) (any, error) {
	nativeV2 := p.version == 2
	name := accounts[index]
	var err error
	delete(params, "cursor")
	if cursor.native != nil {
		params["cursor"] = encoded(*cursor.native)
	}
	var result Object
	if nativeV2 {
		b := p.backends[name]
		raw, err := p.relayV2Backend(ctx, b, "session/list", params, false)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
	} else {
		result, err = forward[Object](p.backends[name].conn, ctx, "session/list", params)
		if err != nil {
			return nil, err
		}
	}
	var rows []Object
	if err := json.Unmarshal(result["sessions"], &rows); err != nil {
		return nil, err
	}
	if nativeV2 && rows == nil {
		return nil, fmt.Errorf("adapter did not return a sessions array")
	}
	for _, row := range rows {
		if !hasString(row, "sessionId") || (!nativeV2 && stringField(row, "sessionId") == "") {
			return nil, fmt.Errorf("adapter returned a session without sessionId")
		}
		row["sessionId"] = encoded(p.publicID(name, stringField(row, "sessionId")))
	}
	result["sessions"], _ = json.Marshal(rows)
	var next *string
	if raw := result["nextCursor"]; raw != nil {
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, err
		}
	}
	if !nativeV2 && next != nil && *next == "" {
		next = nil
	}
	delete(result, "nextCursor")
	if next != nil || index+1 < len(accounts) {
		if next == nil {
			name = accounts[index+1]
			if !nativeV2 {
				empty := ""
				next = &empty
			}
		}
		raw, _ := json.Marshal([2]any{name, next})
		result["nextCursor"] = encoded(string(raw))
	}
	return result, nil
}

func (o Object) member(key string) Object {
	var value Object
	_ = json.Unmarshal(o[key], &value)
	return value
}

func allowsClientCall(caps Object, method string, params Object) bool {
	switch method {
	case "fs/read_text_file":
		return string(caps.member("fs")["readTextFile"]) == "true"
	case "fs/write_text_file":
		return string(caps.member("fs")["writeTextFile"]) == "true"
	case "elicitation/create":
		var mode string
		_ = json.Unmarshal(params["mode"], &mode)
		return (mode == "form" || mode == "url") && caps.member("elicitation").member(mode) != nil
	case "elicitation/complete":
		return caps.member("elicitation").member("url") != nil
	default:
		return !strings.HasPrefix(method, "terminal/") || string(caps["terminal"]) == "true"
	}
}

func allowsSessionUpdate(caps Object, update Object) bool {
	var kind string
	_ = json.Unmarshal(update["sessionUpdate"], &kind)
	session := caps.member("session")
	switch kind {
	case "plan_update", "plan_removed":
		return caps.member("plan") != nil
	case "notice":
		return session.member("notices") != nil
	case "compaction_update", "compaction_summary_chunk":
		return session.member("compaction") != nil
	case "subagent_update", "session_message", "session_message_chunk":
		return caps.member("subagents") != nil
	case "prompt_suggestions_update":
		return session.member("promptSuggestions") != nil
	case "cursor_update":
		return session.member("cursors") != nil
	default:
		return true
	}
}

// A new session has no account until routing. An option is safe to offer in
// advance only if its ID, choices and default agree across eligible accounts.
func (p *proxy) sessionOptions(ctx context.Context, params Object) (any, error) {
	if len(p.accounts) == 1 {
		return relay(ctx, p.backends[p.accounts[0]].conn, "session/options", params)
	}
	results := queryAccounts(p.accounts, func(name string) (map[string]any, error) {
		return forward[map[string]any](p.backends[name].conn, ctx, "session/options", params)
	})
	var shared map[string]any
	for i, result := range results {
		if result.err != nil {
			return nil, result.err
		}
		if i == 0 {
			shared = maps.Clone(result.value)
			continue
		}
		for k, v := range shared {
			if k != "configOptions" {
				if !reflect.DeepEqual(v, result.value[k]) {
					delete(shared, k)
				}
				continue
			}
			options, _ := v.([]any)
			other, _ := result.value[k].([]any)
			shared[k] = slices.DeleteFunc(options, func(opt any) bool {
				return !slices.ContainsFunc(other, func(candidate any) bool { return reflect.DeepEqual(opt, candidate) })
			})
		}
	}
	return shared, nil
}
