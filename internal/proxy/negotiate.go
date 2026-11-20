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
	var req acp.InitializeRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.ProtocolVersion == 0 {
		return nil, acp.NewInvalidParams("protocolVersion is required")
	}
	p.mu.Lock()
	if p.initialized || p.initializing {
		p.mu.Unlock()
		return nil, acp.NewInvalidRequest("already initialized")
	}
	p.initializing = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.initializing = false; p.mu.Unlock() }()
	var caps, meta map[string]any
	auth := []Object{}
	accounts := maps.Clone(p.Accounts)
	var names []string
	for _, name := range slices.Sorted(maps.Keys(accounts)) {
		if len(accounts[name].Models[p.Model]) == 0 || (p.PinnedAccount != "" && name != p.PinnedAccount) {
			delete(accounts, name)
			continue
		}
		names = append(names, name)
	}
	if req.ProtocolVersion > 1 {
		// The version offered to an adapter is the version Petal will speak.
		// Preserve v1 capabilities and extension fields from dual-stack editors.
		var hello Object
		if err := json.Unmarshal(raw, &hello); err != nil {
			return nil, err
		}
		hello["protocolVersion"] = json.RawMessage("1")
		delete(hello, "info")
		delete(hello, "capabilities")
		raw, _ = json.Marshal(hello)
	}
	results := queryAccounts(names, func(name string) (*backend, error) { return p.startBackend(ctx, name, raw) })
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
			delete(accounts, name)
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
				token, _ := json.Marshal(terminalLogin{name, p.Model, method.Terminal.Args})
				m["args"], _ = json.Marshal([]string{"--terminal-auth=" + string(token)})
			}
			_ = json.Unmarshal(m["name"], &label)
			wire, _ := json.Marshal([]string{name, id})
			m["id"] = encoded(string(wire))
			m["name"] = encoded(name + " · " + label)
			auth = append(auth, m)
		}
	}
	if len(available) == 0 && len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	if caps == nil {
		caps = map[string]any{}
	}
	data, err := json.Marshal(caps)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &p.capabilities); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.Config.Accounts = accounts
	p.accounts, p.backends = available, backends
	committed = true
	p.initialized = true
	p.mu.Unlock()
	result := map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "petal", "version": "0-unstable", "title": "Petal"}, "agentCapabilities": caps, "authMethods": auth}
	if len(meta) > 0 {
		result["_meta"] = meta
	}
	if len(p.accounts) > 1 {
		delete(caps, "providers")
	}
	return result, nil
}

// Each page belongs to one account. Its native cursor stays opaque.
func (p *proxy) list(ctx context.Context, params Object) (any, error) {
	var cursor [2]string
	if params == nil {
		params = Object{}
	}
	if raw := params["cursor"]; raw != nil && string(raw) != "null" {
		var key string
		if json.Unmarshal(raw, &key) != nil || json.Unmarshal([]byte(key), &cursor) != nil || p.backends[cursor[0]] == nil {
			return nil, acp.NewInvalidParams("invalid session cursor")
		}
	}
	index := 0
	if cursor[0] != "" {
		index = sort.SearchStrings(p.accounts, cursor[0])
	}
	if index >= len(p.accounts) {
		return Object{"sessions": json.RawMessage("[]")}, nil
	}
	name := p.accounts[index]
	delete(params, "cursor")
	if cursor[1] != "" {
		params["cursor"] = encoded(cursor[1])
	}
	result, err := forward[Object](p.backends[name].conn, ctx, "session/list", params)
	if err != nil {
		return nil, err
	}
	var rows []Object
	if err := json.Unmarshal(result["sessions"], &rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		id, err := SessionID(r)
		if err != nil {
			return nil, err
		}
		r["sessionId"] = encoded(p.publicID(name, id))
	}
	result["sessions"], _ = json.Marshal(rows)
	var next string
	if raw := result["nextCursor"]; raw != nil {
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, err
		}
	}
	delete(result, "nextCursor")
	if next != "" || index+1 < len(p.accounts) {
		if next == "" {
			name = p.accounts[index+1]
		}
		raw, _ := json.Marshal([2]string{name, next})
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
