//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"petal/internal/livesession"
	"petal/internal/localdaemon"
	"petal/internal/paths"
	"petal/internal/routing"
	"petal/internal/tui"
)

// The daemon inherits its launching shell's provider discovery environment.
// Profile the effective config and archive paths as well as the environment
// used by routing discovery and model catalogs.
var discoveryVariables = []string{
	"HOME", "PATH", "CODEX_PATH", "CODEX_HOME", "CLAUDE_CODE_EXECUTABLE", "CLAUDE_CONFIG_DIR",
	"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME",
	"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME",
	"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL_NAME",
	"GODEBUG",
}

type daemonProfile struct {
	config, state, directory string
	discovery                []string
	relativeDiscovery        bool
}

func newDaemonProfile(config, state string, getenv func(string) string) daemonProfile {
	profile := daemonProfile{config: config, state: state, relativeDiscovery: relativeProviderPaths(getenv)}
	for _, key := range discoveryVariables {
		profile.discovery = append(profile.discovery, getenv(key))
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		profile.relativeDiscovery = profile.relativeDiscovery || !filepath.IsAbs(dir)
	}
	return profile
}

func (p *daemonProfile) includeMCPEnvironment(servers []routing.MCPServer) {
	for _, server := range servers {
		for _, header := range server.Headers {
			if header.FromEnv != "" {
				value, present := os.LookupEnv(header.FromEnv)
				p.discovery = append(p.discovery, header.FromEnv, strconv.FormatBool(present), value)
			}
		}
	}
}

func relativeProviderPaths(getenv func(string) string) bool {
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if value := getenv(key); value != "" && !filepath.IsAbs(value) {
			return true
		}
	}
	for _, key := range []string{"CODEX_PATH", "CLAUDE_CODE_EXECUTABLE"} {
		if value := getenv(key); strings.ContainsRune(value, '/') && !filepath.IsAbs(value) {
			return true
		}
	}
	return false
}

func (p daemonProfile) endpoint(runtimeRoot string) (localdaemon.Endpoint, error) {
	identity, _ := json.Marshal(append([]string{p.config, p.state, p.directory}, p.discovery...))
	return localdaemon.DiscoverProfile(runtimeRoot, string(identity))
}

func daemonDialer(configPath string, config routing.Config) (tui.DaemonDialer, error) {
	defaultPath, err := paths.XDG("CONFIG", "config.toml")
	if configPath == "" {
		if err != nil {
			return nil, err
		}
		configPath = defaultPath
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	statePath, err := paths.XDG("STATE", "")
	if err != nil {
		return nil, err
	}
	profile := newDaemonProfile(configPath, statePath, os.Getenv)
	profile.includeMCPEnvironment(config.MCPServers)
	for _, account := range config.Accounts {
		profile.relativeDiscovery = profile.relativeDiscovery || relativeProviderPaths(func(key string) string { return account.Env[key] })
	}
	if profile.relativeDiscovery {
		profile.directory, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	endpoint, err := profile.endpoint("")
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"daemon"}
	if configPath != defaultPath {
		args = append(args, "--config", configPath)
	}
	command := localdaemon.Command{Path: executable, Args: args, Dir: profile.directory}
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
