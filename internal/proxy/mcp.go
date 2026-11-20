package proxy

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	acp "github.com/coder/acp-go-sdk"

	"petal/internal/routing"
)

func mergeMCPServers(params Object, configured []routing.MCPServer, supportsHTTP bool) (Object, error) {
	if len(configured) == 0 {
		return params, nil
	}

	// Keep the editor's wire representations so future transport types and
	// extension fields pass through to the agent.
	var servers []json.RawMessage
	if raw, exists := params["mcpServers"]; exists {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, acp.NewInvalidParams("mcpServers must be an array")
		}
	}
	editorNames := make(map[string]bool, len(servers))
	for _, server := range servers {
		var fields Object
		var name *string
		if json.Unmarshal(server, &fields) == nil && json.Unmarshal(fields["name"], &name) == nil && name != nil {
			editorNames[*name] = true
		}
	}
	editorCount := len(servers)
	for _, server := range configured {
		if editorNames[server.Name] {
			continue
		}
		if server.URL != "" && !supportsHTTP {
			return nil, fmt.Errorf("adapter does not support HTTP MCP server %q", server.Name)
		}
		wire, err := server.WireDefinition()
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(wire)
		if err != nil {
			return nil, err
		}
		servers = append(servers, raw)
	}
	if len(servers) == editorCount {
		return params, nil
	}
	raw, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = Object{}
	}
	params["mcpServers"] = raw
	return params, nil
}

func mergeMCPServersV2(params Object, configured []routing.MCPServer, sharedCaps, backendCaps Object) (Object, error) {
	var servers []json.RawMessage
	if raw := params["mcpServers"]; raw != nil {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, acp.NewInvalidParams("mcpServers must be an array")
		}
	}
	names := make(map[string]bool, len(servers))
	for _, raw := range servers {
		var server Object
		if json.Unmarshal(raw, &server) != nil || !hasString(server, "name") || !hasString(server, "type") {
			return nil, acp.NewInvalidParams("MCP servers require name and type")
		}
		if kind := stringField(server, "type"); (kind == "http" || kind == "stdio" || kind == "acp") && sharedCaps.member(kind) == nil {
			return nil, acp.NewInvalidParams(fmt.Sprintf("unsupported MCP transport %q", kind))
		}
		names[stringField(server, "name")] = true
	}
	if len(configured) == 0 {
		return params, nil
	}
	for _, server := range configured {
		if names[server.Name] {
			continue
		}
		kind := "stdio"
		if server.URL != "" {
			kind = "http"
		}
		if backendCaps.member(kind) == nil {
			return nil, fmt.Errorf("adapter does not support %s MCP server %q", kind, server.Name)
		}
		wire, err := server.WireDefinition()
		if err != nil {
			return nil, err
		}
		var raw []byte
		if kind == "stdio" {
			if !filepath.IsAbs(server.Command) {
				return nil, fmt.Errorf("MCP server %q needs an absolute command path in ACP v2", server.Name)
			}
			raw, err = json.Marshal(struct {
				Type    string                   `json:"type"`
				Name    string                   `json:"name"`
				Command string                   `json:"command"`
				Args    []string                 `json:"args,omitempty"`
				Env     []routing.MCPEnvVariable `json:"env,omitempty"`
			}{kind, server.Name, server.Command, server.Args, server.Env})
		} else {
			raw, err = json.Marshal(wire)
		}
		if err != nil {
			return nil, err
		}
		servers = append(servers, raw)
	}
	if len(servers) > 0 {
		params["mcpServers"], _ = json.Marshal(servers)
	}
	return params, nil
}
