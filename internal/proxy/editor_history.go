package proxy

import (
	"context"
	"encoding/json"
	acp "github.com/coder/acp-go-sdk"
	"maps"
	"slices"
)

// An account's model aliases often launch the same adapter. List its native
// history once per distinct command and preserve native pagination.
func (p *proxy) editorHistory(ctx context.Context, params Object) (any, error) {
	type source struct{ account, model string }
	var sources []source
	for _, name := range slices.Sorted(maps.Keys(p.Accounts)) {
		if p.PinnedAccount != "" && p.PinnedAccount != name {
			continue
		}
		a := p.Accounts[name]
		seen := map[string]bool{}
		for _, model := range append([]string{p.Model, a.Provider + "/default"}, p.ModelNames()...) {
			args := a.Models[model]
			if len(args) == 0 {
				continue
			}
			key, _ := json.Marshal(args)
			if seen[string(key)] {
				continue
			}
			seen[string(key)] = true
			sources = append(sources, source{name, model})
		}
	}
	type cursor struct {
		Account, Model string
		Native         *string
	}
	var c cursor
	idx := 0
	if raw := params["cursor"]; raw != nil && string(raw) != "null" {
		if json.Unmarshal([]byte(stringField(params, "cursor")), &c) != nil {
			return nil, acp.NewInvalidParams("invalid history cursor")
		}
		idx = slices.Index(sources, source{c.Account, c.Model})
		if idx < 0 {
			return nil, acp.NewInvalidParams("unknown history source")
		}
	}
	if params == nil {
		params = Object{}
	}
	for ; idx < len(sources); idx++ {
		src := sources[idx]
		route, err := p.editorRoute(ctx, src.model)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || route.backends[src.account] == nil || (route.version == 1 && route.backends[src.account].hello.AgentCapabilities.SessionCapabilities.List == nil) {
			c.Native = nil
			continue
		}
		page, err := route.accountPage(ctx, params, []string{src.account}, 0, sessionCursor{account: src.account, native: c.Native})
		if err != nil {
			return nil, err
		}
		result := page.(Object)
		next, err := decodeSessionCursor(result["nextCursor"], route.version == 2)
		if err != nil {
			return nil, err
		}
		delete(result, "nextCursor")
		if next.account != "" || idx+1 < len(sources) {
			if next.account == "" {
				src = sources[idx+1]
			}
			data, _ := json.Marshal(cursor{src.account, src.model, next.native})
			result["nextCursor"] = encoded(string(data))
		}
		return result, nil
	}
	return Object{"sessions": json.RawMessage("[]")}, nil
}
