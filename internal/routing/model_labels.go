package routing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// ResolveDefaultLabels asks the installed CLIs which model a new session would
// use. Keep the route ID so selecting a default still follows provider settings.
func (c Config) ResolveDefaultLabels(parent context.Context, cwd string) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	var probes []func()
	for name, a := range c.Accounts {
		if c.PinnedAccount != "" && c.PinnedAccount != name {
			continue
		}
		id := a.Provider + "/default"
		if !a.discoveredDefault {
			continue
		}
		cli := providerCLI(a.Provider, a.Env)
		if cli == "" {
			delete(a.ModelLabels, id)
			continue
		}
		probes = append(probes, func() {
			if model := a.defaultModelName(ctx, cli, cwd); model != "" {
				a.ModelLabels[id] = model
			} else {
				delete(a.ModelLabels, id)
			}
		})
	}
	runCatalogProbes(probes)
}

func (a Account) defaultModelName(ctx context.Context, cli, cwd string) string {
	switch a.Provider {
	case "opencode":
		return a.openCodeDefaultLabel(ctx, cli, cwd)
	case "claude":
		raw, err := a.claudeControl(ctx, cli, cwd, `{"subtype":"get_settings"}`)
		var settings struct{ Applied struct{ Model string } }
		if err == nil && json.Unmarshal(raw, &settings) == nil {
			if label := a.ModelLabels[settings.Applied.Model]; label != "" && settings.Applied.Model != "claude/default" {
				return label
			}
			return settings.Applied.Model
		}
	case "codex":
		raw, err := a.codexRead(ctx, cwd, "config/read")
		var settings struct{ Config struct{ Model string } }
		if err != nil || json.Unmarshal(raw, &settings) != nil {
			return ""
		}
		configured := settings.Config.Model
		if label := a.ModelLabels[configured]; label != "" && configured != "codex/default" {
			return label
		}
		raw, err = a.codexRead(ctx, cwd, "model/list")
		var models struct {
			Data []struct {
				ID, DisplayName string
				IsDefault       bool
			}
		}
		if err == nil && json.Unmarshal(raw, &models) == nil {
			for _, m := range models.Data {
				if m.ID == configured || (configured == "" && m.IsDefault) {
					if m.DisplayName != "" {
						return m.DisplayName
					}
					return m.ID
				}
			}
		}
		return configured
	}
	return ""
}
