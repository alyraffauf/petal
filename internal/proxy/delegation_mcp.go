package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/agentmcp"
	"petal/internal/routing"
	"petal/internal/sessions"
)

type delegationEndpoint struct {
	ctx    context.Context
	wakes  wakeQueue
	cancel context.CancelFunc
	ready  chan struct{}
	record sessions.Record

	cwd    string
	server routing.MCPServer
}

func newDelegationEndpoint(lifetime context.Context, cwd string, tools agentmcp.Tools) (*delegationEndpoint, error) {
	ctx, cancel := context.WithCancel(lifetime)
	e := &delegationEndpoint{ctx: ctx, cancel: cancel, cwd: cwd, ready: make(chan struct{})}
	bound := func() (sessions.Record, bool) {
		select {
		case <-e.ready:
			return e.record, true
		default:
			return sessions.Record{}, false
		}
	}
	config, err := agentmcp.Listen(ctx, cwd, tools, bound, e.wakes.forget)
	if err != nil {
		cancel()
		return nil, err
	}
	e.server = config
	return e, nil
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
		if stringField(server, "name") == routing.AgentMCPServerName {
			return nil, nil, acp.NewInvalidParams("the petal MCP server is reserved")
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

func (p *proxy) bindDelegation(endpoint *delegationEndpoint, account, parent string) error {
	p.delegationMu.Lock()
	defer p.delegationMu.Unlock()
	if cwd := p.delegationCwds[parent]; cwd != "" && cwd != endpoint.cwd {
		return acp.NewInvalidParams("parent session belongs to a different working directory")
	}
	endpoint.record = sessions.Record{ID: parent, Model: p.Model, Account: account, Opened: time.Now()}
	p.closeDelegationLocked(parent)
	if p.delegationEndpoints == nil {
		p.delegationEndpoints = make(map[string]*delegationEndpoint)
		p.delegationCwds = make(map[string]string)
	}
	p.delegationEndpoints[parent] = endpoint
	p.delegationCwds[parent] = endpoint.cwd

	close(endpoint.ready)
	go sessions.IndexHistory(endpoint.ctx, endpoint.cwd, parent, p.History.Embeddings)
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
