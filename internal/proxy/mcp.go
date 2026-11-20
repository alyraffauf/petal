package proxy

import (
	"encoding/json"
	"fmt"

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
