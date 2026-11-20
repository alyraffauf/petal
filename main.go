package main

import (
	"context"
	"errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"log"
	"os"
	"path/filepath"
)

func xdg(kind, name string) (string, error) {
	dir := os.Getenv("XDG_" + kind + "_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, map[string]string{"CONFIG": ".config", "STATE": ".local/state"}[kind])
	}
	return filepath.Join(dir, "petal", name), nil
}
func main() {
	var file, accountName string
	var resume, demo bool
	settings := viper.New()
	root := &cobra.Command{Use: "petal", Short: "Route ACP sessions across model subscriptions", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&file, "config", "", "TOML configuration (default: $XDG_CONFIG_HOME/petal/config.toml)")
	root.PersistentFlags().StringVar(&accountName, "account", "", "pin to a configured account")
	load := func(model string) (config, error) {
		explicit := file != ""
		if file == "" {
			var err error
			file, err = xdg("CONFIG", "config.toml")
			if err != nil {
				return config{}, err
			}
		}
		settings.SetConfigFile(file)
		if err := settings.ReadInConfig(); err != nil {
			if !explicit && errors.Is(err, os.ErrNotExist) {
				c := config{model: model, forced: accountName}
				c.discover()
				if model != "" {
					return c, c.validate()
				}
				return c, nil
			}
			return config{}, err
		}
		args := []string{file, model}
		if accountName != "" {
			args = append(args, accountName)
		}
		// Decode the adapter tables with their original case. Viper normalizes map keys,
		// including environment variables and provider-specific model identifiers.
		return readConfig(args)
	}
	server := &cobra.Command{Use: "serve MODEL", Short: "Serve ACP over stdin and stdout", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		c, err := load(args[0])
		if err == nil {
			serve(c)
		}
		return err
	}}
	tui := &cobra.Command{Use: "tui [MODEL]", Short: "Open an interactive agent session", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var c config
		if !demo {
			model := ""
			if len(args) > 0 {
				model = args[0]
			}
			var err error
			c, err = load(model)
			if err == nil && model == "" {
				var selected bool
				c, selected, err = pick(c, resume)
				if err == nil && !selected {
					return nil
				}
			}
			if err != nil {
				return err
			}
		}
		return runTUI(context.Background(), c, resume, demo)
	}}
	tui.Flags().BoolVar(&demo, "demo", false, "explore a local sample conversation")
	tui.Flags().BoolVar(&resume, "resume", false, "load the last session for this model and directory")
	root.AddCommand(server, tui)

	root.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return tui.RunE(cmd, args)
		}
		c, err := readConfig(args)
		if err == nil {
			serve(c)
		}
		return err
	}

	if err := root.Execute(); err != nil {
		log.Fatal(err)
	}
}
