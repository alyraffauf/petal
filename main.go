package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"petal/internal/agentmcp"
	"petal/internal/livesession"
	"petal/internal/localdaemon"
	"petal/internal/proxy"
	"petal/internal/routing"
	"petal/internal/tui"
)

func main() {
	var options tui.StartOptions
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
		if model == "" {
			// Start editors on the provider's real default model, not a "/default" alias.
			if cwd, err := os.Getwd(); err == nil {
				c.ResolveDefaults(cmd.Context(), cwd)
			}
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
		dial, err := daemonDialer(settings.GetString("config"), c)
		if err != nil {
			return err
		}
		options.ConfigPath = settings.GetString("config")
		return tui.Run(cmd.Context(), c, options, dial)
	}
	root.Flags().BoolVar(&options.Resume, "resume", false, "choose a saved conversation for this directory")
	root.Flags().StringVar(&options.Session, "session", "", "open this conversation in the project directory")
	root.Flags().StringVar(&options.Directory, "directory", "", "project directory for the terminal UI")
	root.MarkFlagsMutuallyExclusive("resume", "session")
	server.Flags().StringVar(&terminalAuth, "terminal-auth", "", "ACP terminal authentication")
	_ = server.Flags().MarkHidden("terminal-auth")
	bridge := &cobra.Command{
		Use: "delegation-mcp SOCKET", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return agentmcp.Relay(cmd.Context(), args[0], os.Getenv(agentmcp.TokenEnv), os.Stdin, os.Stdout)
		},
	}
	mcp := &cobra.Command{Use: "mcp", Short: "Manage configured MCP servers"}
	mcp.AddCommand(&cobra.Command{Use: "login NAME", Short: "Connect an HTTP MCP server and complete browser login if required", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		config, err := load("")
		if err != nil {
			return err
		}
		for _, server := range config.MCPServers {
			if server.Name == args[0] {
				return server.Login(cmd.Context())
			}
		}
		return fmt.Errorf("unknown MCP server %q", args[0])
	}})
	daemon := &cobra.Command{Use: "daemon", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		service := livesession.NewServer(ctx, func(options livesession.OpenOptions) (routing.Config, error) {
			return routing.Load(settings.GetString("config"), options.Model, options.Account)
		})
		return localdaemon.ServeStarted(ctx, service.Handler(), service.Close)
	}}
	root.AddCommand(server, bridge, mcp, daemon)

	if err := root.Execute(); err != nil {
		log.Fatal(err)
	}
}
