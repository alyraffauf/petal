package main

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"petal/internal/proxy"
	"petal/internal/routing"
	"petal/internal/tui"
)

func main() {
	var resume bool
	var terminalAuth string
	settings := viper.New()
	root := &cobra.Command{Use: "petal", Short: "Open an interactive agent session", Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("config", "", "TOML configuration (default: $XDG_CONFIG_HOME/petal/config.toml)")
	root.PersistentFlags().String("account", "", "pin to a configured account")
	if err := settings.BindPFlags(root.PersistentFlags()); err != nil {
		log.Fatal(err)
	}
	load := func(model string) (routing.Config, error) {
		return routing.Load(settings.GetString("config"), model, settings.GetString("account"))
	}
	server := &cobra.Command{Use: "acp [MODEL]", Short: "Serve ACP over stdin and stdout", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var model string
		if len(args) > 0 {
			model = args[0]
		}
		c, err := load(model)
		if err != nil {
			return err
		}
		if terminalAuth != "" {
			login, err := proxy.LoginCommand(cmd.Context(), c, terminalAuth)
			if err != nil {
				return err
			}
			return login.Run()
		}
		if model == "" && c.DefaultModel() == "" {
			if c.PinnedAccount != "" {
				return fmt.Errorf("no ACP model available for account %q", c.PinnedAccount)
			}
			return fmt.Errorf("no ACP model available")
		}
		proxy.Serve(c)
		return nil
	}}
	root.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := load("")
		if err != nil {
			return err
		}
		return tui.Run(cmd.Context(), c, resume)
	}
	root.Flags().BoolVar(&resume, "resume", false, "choose a saved conversation for this directory")
	server.Flags().StringVar(&terminalAuth, "terminal-auth", "", "ACP terminal authentication")
	_ = server.Flags().MarkHidden("terminal-auth")
	bridge := &cobra.Command{
		Use: "delegation-mcp SOCKET", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return proxy.RelayDelegationMCP(cmd.Context(), args[0], os.Stdin, os.Stdout)
		},
	}
	root.AddCommand(server, bridge)

	if err := root.Execute(); err != nil {
		log.Fatal(err)
	}
}
