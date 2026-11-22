package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

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
		path := settings.GetString("config")
		if path != "" {
			path, err = filepath.Abs(path)
			if err != nil {
				return err
			}
		}
		dial, err := daemonDialer(path)
		if err != nil {
			return err
		}
		return tui.Run(cmd.Context(), c, resume, dial)
	}
	root.Flags().BoolVar(&resume, "resume", false, "choose a saved conversation for this directory")
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
		defer service.Close()
		return localdaemon.ServeStarted(ctx, service.Handler())
	}}
	root.AddCommand(server, bridge, mcp, daemon)

	if err := root.Execute(); err != nil {
		log.Fatal(err)
	}
}

func daemonDialer(configPath string) (tui.DaemonDialer, error) {
	endpoint, err := localdaemon.DiscoverProfile("", configPath)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"daemon"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	command := localdaemon.Command{Path: executable, Args: args}
	return func(ctx context.Context) (*livesession.Client, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, err := endpoint.DialOrStart(ctx, command)
		if err != nil {
			return nil, fmt.Errorf("connect to Petal daemon: %w", err)
		}
		return livesession.Connect(ctx, conn)
	}, nil
}
