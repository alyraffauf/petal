package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type account struct {
	Provider    string
	Home        string
	Capacity    *float64
	selectModel map[string]bool
	Env         map[string]string
	Models      map[string][]string
	Usage       []string
}

func (a *account) configureHome(configDir string) error {
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

func (a account) capacity() float64 {
	if a.Capacity == nil {
		return 1
	}
	return *a.Capacity
}

func (a account) command(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range a.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
}

type config struct {
	quiet         bool
	Accounts      map[string]account
	Tiers         map[string]map[string]float64
	model, forced string
	hints         []string
}

func readConfig(args []string) (config, error) {
	var c config
	if len(args) < 2 || len(args) > 3 {
		return c, fmt.Errorf("usage: petal CONFIG.toml MODEL [ACCOUNT]")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return c, err
	}
	if filepath.Ext(args[0]) == ".json" {
		err = json.Unmarshal(data, &c)
	} else {
		err = toml.Unmarshal(data, &c)
	}
	if err != nil {
		return c, err
	}
	for provider, tiers := range c.Tiers {
		for tier, size := range tiers {
			if size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
				return c, fmt.Errorf("tiers.%s.%s must be a positive finite number", provider, tier)
			}
		}
	}
	for name, a := range c.Accounts {
		if err := a.configureHome(filepath.Dir(args[0])); err != nil {
			return c, fmt.Errorf("accounts.%s: %w", name, err)
		}
		c.Accounts[name] = a
		if size := a.capacity(); size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
			return c, fmt.Errorf("accounts.%s.capacity must be a positive finite number", name)
		}
	}
	c.model = args[1]
	if len(args) == 3 {
		c.forced = args[2]
	}
	c.discover()
	if c.model == "" {
		return c, nil
	}
	return c, c.validate()
}

func (c config) choose(ctx context.Context) (string, error) {
	if c.forced != "" {
		return c.forced, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	scores := c.scores(ctx)
	// A product of two float64 significands fits in 106 bits.
	const scorePrecision = 106
	var best big.Float
	best.SetPrec(scorePrecision).SetInt64(-1)
	var choices []string
	for _, s := range scores {
		if s.err != nil || s.remaining == 0 {
			continue
		}
		var score big.Float
		score.SetPrec(scorePrecision).SetFloat64(s.remaining)
		if s.remaining > 0 {
			// Compare exact products of two float64 values. The common
			// divisor of 100 cannot change their order.
			score.Mul(&score, big.NewFloat(s.capacity))
		}
		order := score.Cmp(&best)
		if order > 0 {
			best.Set(&score)
			choices = nil
		}
		if order >= 0 {
			choices = append(choices, s.name)
		}
	}
	if len(choices) == 0 {
		return "", fmt.Errorf("no matching subscription has available quota; check the account probes or choose a subscription manually")
	}
	return choices[rand.IntN(len(choices))], nil
}

type quota struct {
	tier      string
	capacity  float64
	name      string
	remaining float64
	err       error
}

func (c config) scores(ctx context.Context) []quota {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pending := make(chan quota, len(c.Accounts))
	count := 0
	for name, a := range c.Accounts {
		if len(a.Models[c.model]) == 0 {
			continue
		}
		count++
		go func() {
			if d := remainingBackoff(name); d > 0 {
				pending <- quota{name: name, err: fmt.Errorf("rate limited; retry in %s", d.Round(time.Second))}
				return
			}
			q := a.inspectQuota(ctx)
			q.name = name
			q.capacity = a.capacity()
			if a.Capacity == nil {
				if size := c.Tiers[a.Provider][q.tier]; q.tier != "" && size > 0 {
					q.capacity = size
				} else if a.Provider == "claude" {
					switch q.tier {
					case "max_5x":
						q.capacity = 5
					case "max_20x":
						q.capacity = 20
					}
				}
			}
			pending <- q
		}()
	}
	result := make([]quota, 0, count)
	for range count {
		result = append(result, <-pending)
	}
	return result
}
func (a account) inspectQuota(ctx context.Context) quota {
	if a.Provider == "codex" && len(a.Usage) == 0 && executable(a.Env["CODEX_PATH"], "codex") != "" {
		data, err := a.codexQuota(ctx)
		if err != nil {
			return quota{err: err}
		}
		remaining, err := codexRemaining(data)
		return quota{remaining: remaining, err: err, tier: codexTier(data)}
	}
	tier := make(chan string, 1)
	go func() {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		tier <- a.detectTier(probe)
	}()
	q := quota{remaining: -1}
	if len(a.Usage) != 0 {
		q.remaining, q.err = a.probeRemaining(ctx)
	}
	q.tier = <-tier
	return q
}

func codexTier(data json.RawMessage) string {
	var status struct{ RateLimits struct{ PlanType string } }
	if json.Unmarshal(data, &status) != nil {
		return ""
	}
	return status.RateLimits.PlanType
}

func (a account) detectTier(ctx context.Context) string {
	switch a.Provider {
	case "codex":
		if executable(a.Env["CODEX_PATH"], "codex") != "" {
			data, _ := a.codexQuota(ctx)
			return codexTier(data)
		}
	case "claude":
		if cli := executable(a.Env["CLAUDE_CODE_EXECUTABLE"], "claude"); cli != "" {
			cmd := a.command(ctx, []string{cli, "auth", "status", "--json"})
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

func (a account) probeRemaining(ctx context.Context) (float64, error) {
	cmd := a.command(ctx, a.Usage)
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
