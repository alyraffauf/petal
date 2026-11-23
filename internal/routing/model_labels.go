package routing

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

func (c Config) ModelLabel(id string) string {
	var label string
	resolved, missing := false, false
	for name, a := range c.Accounts {
		if c.PinnedAccount != "" && c.PinnedAccount != name || len(a.Models[id]) == 0 {
			continue
		}
		v := a.ModelLabels[id]
		if v == "" {
			missing = true
			continue
		}
		if resolved && label != v {
			return fallbackModelLabel(id)
		}
		label, resolved = v, true
	}
	if resolved && !(missing && strings.HasSuffix(id, "/default")) {
		return label
	}
	return fallbackModelLabel(id)
}

func fallbackModelLabel(id string) string {
	switch id {
	case "claude/default":
		return "Claude default"
	case "opencode/default":
		return "OpenCode default"
	case "codex/default":
		return "Codex default"
	}
	return id
}

func claudeLabels(env map[string]string) map[string]string {
	labels := map[string]string{"opus": "Opus", "haiku": "Haiku", "sonnet": "Sonnet", "fable": "Fable"}
	lookup := func(key string) string {
		if value, ok := env[key]; ok {
			return value
		}
		return os.Getenv(key)
	}
	dir := lookup("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "cache", "model-catalog", "*-cc.json"))
	var catalogModels []struct{ ID, Name string }
	var newest int64
	for _, file := range files {
		var cache struct {
			FetchedAt int64
			Catalog   struct {
				Config struct {
					Models  []struct{ ID, Name string }
					Targets map[string]struct{ Default string } `json:"provider_alias_targets"`
				}
			}
		}
		data, err := os.ReadFile(file)
		if err != nil || json.Unmarshal(data, &cache) != nil || cache.FetchedAt <= newest || len(cache.Catalog.Config.Targets) == 0 {
			continue
		}
		newest = cache.FetchedAt
		catalogModels = cache.Catalog.Config.Models
		for alias := range labels {
			labels[alias] = strings.ToUpper(alias[:1]) + alias[1:]
			target := cache.Catalog.Config.Targets[alias].Default
			for _, m := range cache.Catalog.Config.Models {
				if m.ID == target && m.Name != "" {
					labels[alias] = m.Name
					break
				}
			}
		}
	}
	for _, m := range catalogModels {
		if m.ID != "" && m.Name != "" {
			labels[m.ID] = m.Name
		}
	}
	for _, alias := range []string{"opus", "sonnet", "haiku", "fable"} {
		key := "ANTHROPIC_DEFAULT_" + strings.ToUpper(alias) + "_MODEL"
		if custom := lookup(key); custom != "" {
			labels[alias] = custom
			if v := lookup(key + "_NAME"); v != "" {
				labels[alias] = v
				labels[custom] = v
			}
		}
	}
	return labels
}

// ResolveDefaults asks the installed CLIs which model a new session would use.
// When the answer maps to one of an account's routes, pickers list that model
// in place of the account's "/default" route. Sessions still run on "/default",
// so the provider applies each workspace's own settings.
func (c Config) ResolveDefaults(parent context.Context, cwd string) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	type result struct{ account, model, label string }
	results := make([]result, 0, len(c.Accounts))
	var mu sync.Mutex
	var probes []func()
	for name, a := range c.Accounts {
		if c.PinnedAccount != "" && c.PinnedAccount != name || !a.discoveredDefault {
			continue
		}
		cli := providerCLI(a.Provider, a.Env)
		if cli == "" {
			delete(a.ModelLabels, a.Provider+"/default")
			continue
		}
		probes = append(probes, func() {
			r := result{account: name}
			if a.Provider == "opencode" {
				r.label = a.openCodeDefaultLabel(ctx, cli, cwd)
			} else {
				r.model = a.defaultModelID(ctx, cli, cwd)
			}
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		})
	}
	runCatalogProbes(probes)
	for _, r := range results {
		a := c.Accounts[r.account]
		id := a.Provider + "/default"
		if r.model != "" {
			a.DefaultRoute = a.routeFor(r.model)
			r.label = a.ModelLabels[r.model]
			if a.DefaultRoute != "" {
				r.label = a.ModelLabels[a.DefaultRoute]
			}
			if r.label == "" {
				r.label = r.model
			}
		}
		if r.label != "" {
			a.ModelLabels[id] = r.label
		} else {
			delete(a.ModelLabels, id)
		}
		c.Accounts[r.account] = a
	}
}

// routeFor returns the route that runs a model the CLI reported. An alias such
// as "opus" matches when its catalog name equals the model's. A model with no
// route leaves the default unresolved.
func (a Account) routeFor(model string) string {
	dflt := a.Provider + "/default"
	if model == "" || model == dflt {
		return ""
	}
	if len(a.Models[model]) > 0 {
		return model
	}
	if label := a.ModelLabels[model]; label != "" {
		for _, route := range slices.Sorted(maps.Keys(a.Models)) {
			if route != dflt && a.ModelLabels[route] == label {
				return route
			}
		}
	}
	return ""
}

func (a Account) defaultModelID(ctx context.Context, cli, cwd string) string {
	switch a.Provider {
	case "claude":
		raw, err := a.claudeControl(ctx, cli, cwd, `{"subtype":"get_settings"}`)
		var settings struct{ Applied struct{ Model string } }
		if err == nil && json.Unmarshal(raw, &settings) == nil {
			return settings.Applied.Model
		}
	case "codex":
		raw, err := a.codexRead(ctx, cwd, "config/read")
		var settings struct{ Config struct{ Model string } }
		if err != nil || json.Unmarshal(raw, &settings) != nil {
			return ""
		}
		if configured := settings.Config.Model; configured != "" {
			return configured
		}
		raw, err = a.codexRead(ctx, cwd, "model/list")
		var models struct {
			Data []struct {
				ID        string
				IsDefault bool
			}
		}
		if err == nil && json.Unmarshal(raw, &models) == nil {
			for _, m := range models.Data {
				if m.IsDefault {
					return m.ID
				}
			}
		}
	}
	return ""
}
