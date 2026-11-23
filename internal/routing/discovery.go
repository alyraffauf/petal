package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
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

func providerCLI(provider string, env map[string]string) string {
	var variable string
	switch provider {
	case "codex":
		variable = "CODEX_PATH"
	case "claude":
		variable = "CLAUDE_CODE_EXECUTABLE"
	}
	override, configured := env[variable]
	if !configured {
		override = os.Getenv(variable)
	}
	return executable(override, provider)
}

func (c *Config) discover() {
	if c.Accounts == nil {
		c.Accounts = map[string]Account{}
	}
	for _, provider := range []string{"claude", "codex"} {
		var adapter, bundled string
		switch provider {
		case "codex":
			adapter = executable("codex-acp")
			bundled = bundledCodex
		case "claude":
			adapter = executable("claude-agent-acp", "claude-code-acp")
			bundled = bundledClaude
		}
		if adapter == "" {
			bundled = executable(bundled)
		}
		var probes []func()
		missingBridge := false
		discover := func(a Account) Account {
			cli := providerCLI(provider, a.Env)
			bridge := adapter
			if bridge == "" && cli != "" {
				bridge = bundled
				missingBridge = missingBridge || bridge == ""
			}
			if bridge == "" {
				return a
			}
			return discoverAccount(provider, cli, bridge, a, func(probe func()) { probes = append(probes, probe) })
		}
		named := false
		for name, a := range c.Accounts {
			if a.Provider == provider {
				c.Accounts[name] = discover(a)
				named = true
			}
		}
		if _, exists := c.Accounts[provider]; !named && !exists {
			a := discover(Account{Provider: provider})
			if len(a.Models) > 0 {
				c.Accounts[provider] = a
			}
		}
		runCatalogProbes(probes)
		if missingBridge {
			c.Hints = append(c.Hints, provider+" installed; ACP bridge missing")
		}
	}
}

func runCatalogProbes(probes []func()) {
	if len(probes) == 1 {
		probes[0]()
		return
	}
	if len(probes) == 0 {
		return
	}
	slots := make(chan struct{}, 4)
	var group sync.WaitGroup
	for _, probe := range probes {
		slots <- struct{}{}
		group.Go(func() {
			defer func() { <-slots }()
			probe()
		})
	}
	group.Wait()
}

func discoverAccount(provider, cli, adapter string, a Account, schedule func(func())) Account {
	if a.Models == nil {
		a.Models = map[string][]string{}
	}
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	a.ModelLabels = map[string]string{}
	a.SelectModel = map[string]bool{}
	argv := []string{adapter}
	if _, exists := a.Models[provider+"/default"]; !exists {
		a.Models[provider+"/default"] = argv
		a.discoveredDefault = true
	}
	addModel := func(id string) {
		if _, exists := a.Models[id]; id != "" && !exists {
			a.Models[id] = argv
			a.SelectModel[id] = true
		}
	}
	switch provider {
	case "claude":
		if cli != "" && a.Env["CLAUDE_CODE_EXECUTABLE"] == "" {
			a.Env["CLAUDE_CODE_EXECUTABLE"] = cli
		}
		a.ModelLabels = claudeLabels(a.Env)
		for _, id := range []string{"sonnet", "opus", "haiku", "fable"} {
			addModel(id)
		}
	case "codex":
		if cli != "" && a.Env["CODEX_PATH"] == "" {
			a.Env["CODEX_PATH"] = cli
		}
		dir, configured := a.Env["CODEX_HOME"]
		if !configured {
			dir = os.Getenv("CODEX_HOME")
		}
		if dir == "" {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, ".codex")
		}
		var cache struct {
			Models []struct {
				Slug, Visibility string
				DisplayName      string `json:"display_name"`
			}
		}
		data, err := os.ReadFile(filepath.Join(dir, "models_cache.json"))
		if err == nil {
			err = json.Unmarshal(data, &cache)
		}
		apply := func() {
			for _, m := range cache.Models {
				if m.Visibility == "list" {
					addModel(m.Slug)
					a.ModelLabels[m.Slug] = m.DisplayName
				}
			}
		}
		if err == nil && len(cache.Models) > 0 {
			apply()
		} else if cli != "" {
			// Deferred probes write only to their own account's model map.
			a.Models = maps.Clone(a.Models)
			schedule(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				cmd := a.Command(ctx, []string{cli, "debug", "models"})
				cmd.WaitDelay = time.Second
				data, err := cmd.Output()
				if err == nil && json.Unmarshal(data, &cache) == nil {
					apply()
				}
			})
		}
	}
	return a
}

func (c Config) ModelNames() []string {
	seen := map[string]bool{}
	var result []string
	for name, a := range c.Accounts {
		if c.PinnedAccount != "" && name != c.PinnedAccount {
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

// DefaultModel prefers the provider's configured model when serving an editor.
func (c Config) DefaultModel() string {
	models := c.ModelNames()
	for _, model := range models {
		if strings.HasSuffix(model, "/default") {
			return model
		}
	}
	if len(models) == 0 {
		return ""
	}
	return models[0]
}

func (c Config) validate() error {
	if !slices.Contains(c.ModelNames(), c.Model) {
		return fmt.Errorf("no account can run model %q with account selection %q", c.Model, c.PinnedAccount)
	}
	return nil
}
