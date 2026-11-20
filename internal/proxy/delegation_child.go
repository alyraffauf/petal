package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/delegation"
	"petal/internal/routing"
)

func (p *proxy) configureDelegation() {
	if !p.Config.Delegation {
		return
	}
	runtime := delegation.New(p.lifetime, p.openDelegatedChild, p.requestDelegatedPermission)
	p.delegationRuntime = runtime
	p.delegation = &delegationTools{
		delegate: func(ctx context.Context, parent, cwd, model, task string) (any, error) {
			return runtime.Delegate(ctx, parent, cwd, model, task)
		},
		read: func(ctx context.Context, parent, cwd, child string, after int64) (any, error) {
			return runtime.Read(ctx, parent, cwd, child, after)
		},
		wait: func(ctx context.Context, parent, cwd, child string, after int64, timeout time.Duration) (any, error) {
			return runtime.Wait(ctx, parent, cwd, child, after, timeout)
		},
	}
}

type delegatedConnection struct {
	host    *proxy
	account string
	backend *backend
	version uint16
}

func (p *proxy) openDelegatedChild(ctx context.Context, model string, receive delegation.Receive) (delegation.Connection, error) {
	config := p.Config
	config.Model = model
	config.Delegation = false
	config.MCPServers = nil
	config.Accounts = make(map[string]routing.Account)

	for name, account := range p.Accounts {
		if len(account.Models[model]) != 0 && (config.PinnedAccount == "" || config.PinnedAccount == name) {
			config.Accounts[name] = account
		}
	}
	if len(config.Accounts) == 0 {
		return nil, fmt.Errorf("no configured account offers model %q", model)
	}
	names := slices.Sorted(maps.Keys(config.Accounts))
	name := names[0]
	if len(names) > 1 {
		if chosen, err := config.Choose(ctx); err == nil {
			name = chosen
		}
	}
	host := &proxy{Config: config, lifetime: ctx, embedded: p.embedded}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if config.ACPv2 {
		b, err := host.startBackendWithReceiver(ctx, name, json.RawMessage(`{"protocolVersion":2,"info":{"name":"petal","version":"0-unstable"},"capabilities":{}}`), 2, receive)
		if err == nil {
			return &delegatedConnection{host, name, b, 2}, nil
		}
	}
	b, err := host.startBackendWithReceiver(ctx, name, json.RawMessage(`{"protocolVersion":1,"clientInfo":{"name":"petal","version":"0-unstable"},"clientCapabilities":{}}`), 1, receive)
	if err == nil {
		return &delegatedConnection{host, name, b, 1}, nil
	}

	return nil, fmt.Errorf("open adapter for child model %q: %w", model, err)
}

func (p *proxy) routeChildParams(account string, raw json.RawMessage) json.RawMessage {
	var params Object
	if json.Unmarshal(raw, &params) != nil || !hasString(params, "sessionId") {
		return raw
	}
	params["sessionId"] = encoded(p.publicID(account, stringField(params, "sessionId")))
	routed, _ := json.Marshal(params)
	return routed
}

func (c *delegatedConnection) AccountName() string   { return c.account }
func (c *delegatedConnection) Version() uint16       { return c.version }
func (c *delegatedConnection) Done() <-chan struct{} { return c.backend.conn.Done() }
func (c *delegatedConnection) Close()                { c.backend.close() }

func (c *delegatedConnection) nativeParams(params any) (Object, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var object Object
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object["sessionId"] != nil {
		parts := decodeRouteID(stringField(object, "sessionId"), 3)
		if parts == nil || parts[0] != c.account || parts[1] != c.host.Model {
			return nil, fmt.Errorf("invalid child route")
		}
		object["sessionId"] = encoded(parts[2])
	}
	return object, nil
}

func (c *delegatedConnection) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	native, err := c.nativeParams(params)
	if err != nil {
		return nil, err
	}
	result, err := acp.SendRequest[json.RawMessage](c.backend.conn, ctx, method, native)
	if err != nil || method != "session/new" {
		return result, err
	}
	var session Object
	if json.Unmarshal(result, &session) != nil || !hasString(session, "sessionId") || stringField(session, "sessionId") == "" {
		return nil, fmt.Errorf("adapter did not return a child session ID")
	}
	id := stringField(session, "sessionId")
	if c.host.Accounts[c.account].SelectModel[c.host.Model] {
		params := Object{"sessionId": encoded(id), "configId": encoded("model"), "value": encoded(c.host.Model)}
		if c.version == 2 {
			params["type"] = encoded("id")
		}
		if _, err := acp.SendRequest[json.RawMessage](c.backend.conn, ctx, "session/set_config_option", params); err != nil {
			return nil, fmt.Errorf("select child model %q: %w", c.host.Model, err)
		}
	}
	session["sessionId"] = encoded(c.host.publicID(c.account, id))
	return json.Marshal(session)
}

func (c *delegatedConnection) Notify(ctx context.Context, method string, params any) error {
	native, err := c.nativeParams(params)
	if err != nil {
		return err
	}
	return c.backend.conn.SendNotification(ctx, method, native)
}

func (p *proxy) requestDelegatedPermission(ctx context.Context, parent, _ string, version uint16, raw json.RawMessage) (json.RawMessage, error) {
	var params Object
	if json.Unmarshal(raw, &params) != nil || params == nil {
		return nil, acp.NewInvalidParams("invalid child permission request")
	}
	editorParent := parent
	if alias, ok := p.aliases.Load(parent); ok {
		editorParent = alias.(string)
	}
	params["sessionId"] = encoded(editorParent)

	if version == 2 && p.version == 1 {
		subject := params.member("subject")
		if stringField(subject, "type") != "tool_call" || subject.member("toolCall") == nil {
			return json.RawMessage(`{"outcome":{"outcome":"cancelled"}}`), nil
		}
		params["toolCall"] = subject["toolCall"]
		delete(params, "subject")
	} else if version == 1 && p.version == 2 {
		tool := params.member("toolCall")
		if tool == nil {
			return nil, acp.NewInvalidParams("missing child tool call")
		}
		title := stringField(tool, "title")
		if title == "" {
			title = stringField(tool, "toolCallId")
		}
		params["title"] = encoded(title)
		params["subject"], _ = json.Marshal(Object{"type": encoded("tool_call"), "toolCall": params["toolCall"]})
		delete(params, "toolCall")
	}
	if p.version == 2 {
		params["title"] = encoded("Delegated child: " + stringField(params, "title"))
	} else {
		rewriteObject(params, "toolCall", func(tool Object) {
			title := stringField(tool, "title")
			if title == "" {
				title = stringField(tool, "toolCallId")
			}
			tool["title"] = encoded("Delegated child: " + title)
		})
	}
	return acp.SendRequest[json.RawMessage](p.editor, ctx, "session/request_permission", params)
}
