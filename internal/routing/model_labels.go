package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func (c Config) ModelLabel(id string) string {
	var label string
	for name, a := range c.Accounts {
		if c.PinnedAccount != "" && c.PinnedAccount != name || len(a.Models[id]) == 0 {
			continue
		}
		if v := a.ModelLabels[id]; v != "" {
			if label != "" && label != v {
				return id
			}
			label = v
		}
	}
	if label != "" {
		return label
	}
	switch id {
	case "claude/default":
		return "Claude default"
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
	for alias := range labels {
		key := "ANTHROPIC_DEFAULT_" + strings.ToUpper(alias) + "_MODEL"
		if custom := lookup(key); custom != "" {
			labels[alias] = custom
			if v := lookup(key + "_NAME"); v != "" {
				labels[alias] = v
			}
		}
	}
	return labels
}
