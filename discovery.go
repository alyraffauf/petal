package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

// Nix supplies bridges for installed CLIs. Explicit PATH entries take precedence.
var bundledCodex, bundledClaude string

func executable(names ...string) string {
	for _, n := range names {
		if n != "" {
			if p, err := exec.LookPath(n); err == nil {
				return p
			}
		}
	}
	return ""
}

func (c *config) discover() {
	if c.Accounts == nil {
		c.Accounts = map[string]account{}
	}
	for _, provider := range []string{"claude", "codex"} {
		cli := executable(provider)
		var adapter string
		switch provider {
		case "codex":
			adapter = executable("codex-acp")
			if adapter == "" && cli != "" {
				adapter = executable(bundledCodex)
			}
		case "claude":
			adapter = executable("claude-agent-acp", "claude-code-acp")
			if adapter == "" && cli != "" {
				adapter = executable(bundledClaude)
			}
		}
		if adapter == "" {
			if cli != "" {
				c.hints = append(c.hints, provider+" installed; ACP bridge missing")
			}
			continue
		}
		named := false
		for name, a := range c.Accounts {
			if a.Provider == provider {
				c.Accounts[name] = discoverAccount(provider, cli, adapter, a)
				named = true
			}
		}
		if _, exists := c.Accounts[provider]; !named && !exists {
			c.Accounts[provider] = discoverAccount(provider, cli, adapter, account{Provider: provider})
		}
	}
}

func discoverAccount(provider, cli, adapter string, a account) account {
	if a.Models == nil {
		a.Models = map[string][]string{}
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.selectModel = map[string]bool{}
	argv := []string{adapter}
	var ids []string
	switch provider {
	case "claude":
		if cli != "" && a.Env["CLAUDE_CODE_EXECUTABLE"] == "" {
			a.Env["CLAUDE_CODE_EXECUTABLE"] = cli
		}
		ids = []string{"sonnet", "opus", "haiku", "fable"}
	case "codex":
		if cli != "" && a.Env["CODEX_PATH"] == "" {
			a.Env["CODEX_PATH"] = cli
		}
		dir := a.Env["CODEX_HOME"]
		if dir == "" {
			dir = os.Getenv("CODEX_HOME")
		}
		if dir == "" {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, ".codex")
		}
		var cache struct {
			Models []struct{ Slug, Visibility string }
		}
		if data, err := os.ReadFile(filepath.Join(dir, "models_cache.json")); err == nil && json.Unmarshal(data, &cache) == nil {
			for _, m := range cache.Models {
				if m.Visibility == "list" {
					ids = append(ids, m.Slug)
				}
			}
		}

	}
	if _, exists := a.Models[provider+"/default"]; !exists {
		a.Models[provider+"/default"] = argv
	}
	for _, id := range ids {
		if id != "" {
			if _, exists := a.Models[id]; !exists {
				a.Models[id] = argv
				a.selectModel[id] = true
			}
		}
	}
	return a
}

func (c config) modelNames() []string {
	seen := map[string]bool{}
	var result []string
	for name, a := range c.Accounts {
		if c.forced != "" && name != c.forced {
			continue
		}
		for model, args := range a.Models {
			if len(args) > 0 && !seen[model] {
				seen[model] = true
				result = append(result, model)
			}
		}
	}
	slices.Sort(result)
	return result
}

func (c config) validate() error {
	if !slices.Contains(c.modelNames(), c.model) {
		return fmt.Errorf("no account can run model %q with account selection %q", c.model, c.forced)
	}
	return nil
}
