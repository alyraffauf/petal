package routing

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"petal/internal/paths"
)

type Account struct {
	Provider    string
	Home        string
	Capacity    *float64
	ModelLabels map[string]string `toml:"-" json:"-"`
	SelectModel map[string]bool   `toml:"-" json:"-"`
	Env         map[string]string
	Models      map[string][]string
	Usage       []string
}

func (a *Account) configureHome(configDir string) error {
	var key string
	switch a.Provider {
	case "claude":
		key = "CLAUDE_CONFIG_DIR"
	case "codex":
		key = "CODEX_HOME"
	case "":
		if a.Home == "" {
			return nil // Explicit ACP command without a managed provider.
		}
		return fmt.Errorf("home requires provider claude or codex")
	default:
		return fmt.Errorf("unsupported provider %q (expected claude or codex)", a.Provider)
	}
	if a.Home == "" {
		return nil
	}
	dir := a.Home
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, dir[1:])
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(configDir, dir)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if a.Env == nil {
		a.Env = make(map[string]string)
	}
	a.Env[key] = dir
	return nil
}

func (a Account) capacity() float64 {
	if a.Capacity == nil {
		return 1
	}
	return *a.Capacity
}

func (a Account) Command(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range a.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
}

const DelegationMCPServerName = "petal-delegation"

type Config struct {
	Delegation            bool   `toml:"delegation" json:"delegation"`
	ACPv2                 bool   `json:"acp_v2"`
	StreamResponses       bool   `json:"stream_responses"`
	TransparentBackground bool   `toml:"transparent_background" json:"transparent_background"`
	BusyInput             string `toml:"busy_input" json:"busy_input"`
	Accounts              map[string]Account
	Tiers                 map[string]map[string]float64
	MCPServers            []MCPServer `toml:"mcp_servers" json:"mcp_servers"`
	Model, PinnedAccount  string      `toml:"-" json:"-"`
	Hints                 []string    `toml:"-" json:"-"`
}

func Load(path, model, accountName string) (Config, error) {
	c := Config{Model: model, PinnedAccount: accountName}
	optional := path == ""
	if optional {
		defaultPath, err := paths.XDG("CONFIG", "config.toml")
		if err != nil {
			return c, err
		}
		path = defaultPath
	}
	err := c.readSettings(path)
	if err != nil && !(optional && errors.Is(err, os.ErrNotExist)) {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.BusyInput != "" && c.BusyInput != "steer" && c.BusyInput != "follow_up" {
		return c, fmt.Errorf("busy_input must be steer or follow_up")
	}
	for provider, tiers := range c.Tiers {
		for tier, size := range tiers {
			if size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
				return c, fmt.Errorf("tiers.%s.%s must be a positive finite number", provider, tier)
			}
		}
	}
	for name, a := range c.Accounts {
		if err := a.configureHome(filepath.Dir(path)); err != nil {
			return c, fmt.Errorf("accounts.%s: %w", name, err)
		}
		c.Accounts[name] = a
		if size := a.capacity(); size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
			return c, fmt.Errorf("accounts.%s.capacity must be a positive finite number", name)
		}
	}
	c.discover()
	if c.Model == "" {
		return c, nil
	}
	return c, c.validate()
}

func (c Config) Choose(ctx context.Context) (string, error) {
	if c.PinnedAccount != "" {
		return c.PinnedAccount, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	scores := c.Scores(ctx)
	best := weightedQuota(-1, 1)
	var choices []string
	for _, s := range scores {
		if s.Err != nil || s.Remaining == 0 {
			continue
		}
		score := weightedQuota(s.Remaining, s.Capacity)
		order := score.Cmp(best)
		if order > 0 {
			best.Set(score)
			choices = nil
		}
		if order >= 0 {
			choices = append(choices, s.Name)
		}
	}
	if len(choices) == 0 {
		return "", fmt.Errorf("no matching subscription has available quota; check the account probes or choose a subscription manually")
	}
	return choices[rand.IntN(len(choices))], nil
}

// Rank returns eligible accounts in descending quota order, with unknown quota
// after measured quota. Affinity breaks ties in favor of the parent account.
func (c Config) Rank(ctx context.Context, affinity string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.PinnedAccount != "" {
		if len(c.Accounts[c.PinnedAccount].Models[c.Model]) == 0 {
			return nil, fmt.Errorf("pinned account %q does not offer model %q", c.PinnedAccount, c.Model)
		}
		return []string{c.PinnedAccount}, nil
	}

	scores := make(map[string]Quota, len(c.Accounts))
	for _, quota := range c.Scores(ctx) {
		scores[quota.Name] = quota
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type candidate struct {
		name  string
		score *big.Float
	}
	var candidates []candidate
	for name, account := range c.Accounts {
		quota, measured := scores[name]
		if len(account.Models[c.Model]) == 0 || quota.coolingDown || RemainingBackoff(name) > 0 {
			continue
		}
		if measured && quota.Err == nil && quota.Remaining == 0 {
			continue
		}
		score := weightedQuota(-1, 1)
		if measured && quota.Err == nil {
			score = weightedQuota(quota.Remaining, quota.Capacity)
		}
		candidates = append(candidates, candidate{name, score})
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		if order := b.score.Cmp(a.score); order != 0 {
			return order
		}
		switch {
		case a.name == b.name:
			return 0
		case a.name == affinity:
			return -1
		case b.name == affinity:
			return 1
		default:
			return cmp.Compare(a.name, b.name)
		}
	})
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no eligible subscription for model %q has available or unknown quota", c.Model)
	}
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		names = append(names, candidate.name)
	}
	return names, nil
}

func weightedQuota(remaining, capacity float64) *big.Float {
	// A product of two float64 significands fits in 106 bits. The common
	// divisor of 100 cannot change the order.
	score := new(big.Float).SetPrec(106).SetFloat64(remaining)
	if remaining > 0 {
		score.Mul(score, big.NewFloat(capacity))
	}
	return score
}

type Quota struct {
	Tier        string
	Capacity    float64
	Name        string
	Remaining   float64
	Err         error
	coolingDown bool
}

func (c Config) Scores(ctx context.Context) []Quota {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pending := make(chan Quota, len(c.Accounts))
	count := 0
	for name, a := range c.Accounts {
		if len(a.Models[c.Model]) == 0 {
			continue
		}
		count++
		go func() {
			if d := RemainingBackoff(name); d > 0 {
				pending <- Quota{Name: name, coolingDown: true, Err: fmt.Errorf("rate limited; retry in %s", d.Round(time.Second))}
				return
			}
			q := a.inspectQuota(ctx, c.Model)
			q.Name = name
			q.Capacity = a.capacity()
			if a.Capacity == nil {
				if size := c.Tiers[a.Provider][q.Tier]; q.Tier != "" && size > 0 {
					q.Capacity = size
				} else if a.Provider == "claude" {
					switch q.Tier {
					case "max_5x":
						q.Capacity = 5
					case "max_20x":
						q.Capacity = 20
					}
				}
			}
			pending <- q
		}()
	}
	result := make([]Quota, 0, count)
	for range count {
		select {
		case q := <-pending:
			result = append(result, q)
		case <-ctx.Done():
			for {
				select {
				case q := <-pending:
					result = append(result, q)
				default:
					return result
				}
			}
		}
	}
	return result
}
func (a Account) inspectQuota(ctx context.Context, model string) Quota {
	if a.Provider == "codex" && len(a.Usage) == 0 && providerCLI("codex", a.Env) != "" {
		data, err := a.codexQuota(ctx)
		if err != nil {
			return Quota{Err: err}
		}
		remaining, err := codexRemaining(data)
		return Quota{Remaining: remaining, Err: err, Tier: codexTier(data)}
	}
	tier := make(chan string, 1)
	go func() {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		tier <- a.detectTier(probe)
	}()
	q := Quota{Remaining: -1}
	if len(a.Usage) != 0 {
		q.Remaining, q.Err = a.probeRemaining(ctx)
	} else if a.Provider == "claude" {
		if cli := providerCLI("claude", a.Env); cli != "" {
			q.Remaining, q.Err = a.claudeQuota(ctx, cli, model)
		}
	}
	q.Tier = <-tier
	return q
}

func codexTier(data json.RawMessage) string {
	var status struct{ RateLimits struct{ PlanType string } }
	if json.Unmarshal(data, &status) != nil {
		return ""
	}
	return status.RateLimits.PlanType
}

func (a Account) detectTier(ctx context.Context) string {
	switch a.Provider {
	case "codex":
		if providerCLI("codex", a.Env) != "" {
			data, _ := a.codexQuota(ctx)
			return codexTier(data)
		}
	case "claude":
		if cli := providerCLI("claude", a.Env); cli != "" {
			cmd := a.Command(ctx, []string{cli, "auth", "status", "--json"})
			cmd.WaitDelay = 200 * time.Millisecond
			data, err := cmd.Output()
			var status struct {
				LoggedIn                               bool
				AuthMethod, PlanTier, SubscriptionType string
			}
			if err == nil && json.Unmarshal(data, &status) == nil && status.LoggedIn && status.AuthMethod == "claude.ai" {
				if status.PlanTier != "" && status.PlanTier != "other" && status.PlanTier != "unknown" {
					return status.PlanTier
				}
				return status.SubscriptionType
			}
		}
	}
	return ""
}

func (a Account) probeRemaining(ctx context.Context) (float64, error) {
	cmd := a.Command(ctx, a.Usage)
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || math.IsNaN(n) || n < 0 || n > 100 {
		return 0, fmt.Errorf("expected a percentage from 0 to 100")
	}
	return n, nil
}
