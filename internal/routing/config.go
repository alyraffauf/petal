package routing

import (
	"fmt"
	"path/filepath"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// Keep identifiers in values because Viper folds configuration keys to lowercase.
type fileSettings struct {
	TransparentBackground bool   `mapstructure:"transparent_background"`
	BusyInput             string `mapstructure:"busy_input"`
	Accounts              []struct {
		Name, Provider, Home string
		Capacity             *float64
		Usage                []string
		Env                  []struct{ Name, Value string }
		Models               []struct {
			ID      string
			Command []string
		}
	}
	Tiers []struct {
		Provider, Name string
		Capacity       float64
	}
}

func (c *Config) readSettings(path string) error {
	settings := viper.New()
	settings.SetConfigFile(path)
	settings.SetConfigType("toml")
	if filepath.Ext(path) == ".json" {
		settings.SetConfigType("json")
	}
	if err := settings.ReadInConfig(); err != nil {
		return err
	}
	var file fileSettings
	if err := settings.UnmarshalExact(&file, func(dc *mapstructure.DecoderConfig) {
		dc.WeaklyTypedInput = false
		dc.DecodeHook = nil
	}); err != nil {
		return err
	}
	c.BusyInput = file.BusyInput
	c.TransparentBackground = file.TransparentBackground
	c.Accounts = make(map[string]Account, len(file.Accounts))
	for _, profile := range file.Accounts {
		if profile.Name == "" {
			return fmt.Errorf("account needs a name")
		}
		if _, exists := c.Accounts[profile.Name]; exists {
			return fmt.Errorf("duplicate account %q", profile.Name)
		}
		a := Account{Provider: profile.Provider, Home: profile.Home, Capacity: profile.Capacity, Usage: profile.Usage,
			Env: make(map[string]string), Models: make(map[string][]string)}
		for _, env := range profile.Env {
			if env.Name == "" {
				return fmt.Errorf("account %q has an empty environment variable name", profile.Name)
			}
			if _, exists := a.Env[env.Name]; exists {
				return fmt.Errorf("account %q repeats environment variable %q", profile.Name, env.Name)
			}
			a.Env[env.Name] = env.Value
		}
		for _, model := range profile.Models {
			if model.ID == "" || len(model.Command) == 0 || model.Command[0] == "" {
				return fmt.Errorf("account %q needs an ID and command for each model", profile.Name)
			}
			if _, exists := a.Models[model.ID]; exists {
				return fmt.Errorf("account %q repeats model %q", profile.Name, model.ID)
			}
			a.Models[model.ID] = model.Command
		}
		c.Accounts[profile.Name] = a
	}
	c.Tiers = make(map[string]map[string]float64)
	for _, tier := range file.Tiers {
		if tier.Provider == "" || tier.Name == "" {
			return fmt.Errorf("tier needs a provider and name")
		}
		if c.Tiers[tier.Provider] == nil {
			c.Tiers[tier.Provider] = make(map[string]float64)
		}
		if _, exists := c.Tiers[tier.Provider][tier.Name]; exists {
			return fmt.Errorf("provider %q repeats tier %q", tier.Provider, tier.Name)
		}
		c.Tiers[tier.Provider][tier.Name] = tier.Capacity
	}
	return nil
}
