# Configure Petal

Petal looks for Claude Code and Codex on your path. To add accounts, create `~/.config/petal/config.toml`. If you set `XDG_CONFIG_HOME`, use `$XDG_CONFIG_HOME/petal/config.toml`. Pass `--config PATH` to use another file.

## Route across subscriptions

```toml
[[accounts]]
name = "personal"
provider = "codex"
home = "~/.codex"

[[accounts]]
name = "work"
provider = "codex"
home = "~/.codex-work"
capacity = 5

[[accounts]]
name = "claude-work"
provider = "claude"
home = "~/.claude-work"
```

Sign in to each profile with its own home directory.

```sh
CODEX_HOME="$HOME/.codex-work" codex login
CLAUDE_CONFIG_DIR="$HOME/.claude-work" claude
```

Once you name a provider's profiles, list every account you want Petal to use for that provider. Relative home paths resolve against the config file.

Petal compares **capacity × remaining quota fraction** across accounts that offer the model. A 5× account at 30% beats a 1× account at 90%. Ties split randomly. Petal reads Codex limits from `account/rateLimits/read` in `codex app-server`. For Claude, Petal sends a `get_usage` control request to the CLI. Both probes use the account's environment. Petal uses the tightest applicable window, including Claude's model-specific weekly limit when available.

Capacity defaults to 1. Detected Claude Max 5× and 20× tiers use their named weights. Add a `[[tiers]]` entry with `provider = "codex"`, `name = "pro"`, and `capacity = 4` to give all detected Codex Pro profiles a relative weight of 4. An account's `capacity` takes precedence.

Auto uses an unmetered account when no measured account has quota. Failed probes and exhausted accounts are excluded. A rate-limit error starts a five-minute cooldown. Use `petal --account work` to bypass probing and pin the account. For a custom quota source, set `usage` in the work account to a command array that prints the percentage remaining.

## Run a custom ACP server

Petal has built-in discovery and quota checks for Claude Code and Codex. To use another ACP v1 server, configure the command that starts it over stdio. Omit `provider` and `home`. Use the server's own sign-in and configuration.

For example, install OpenCode, run `opencode auth login`, and add this table to your Petal config. [OpenCode ACP setup](https://opencode.ai/docs/cli/acp/)

```toml
[[accounts]]
name = "opencode"

[[accounts.models]]
id = "opencode/default"
command = ["opencode", "acp"]
```

Open **new session** in Petal and choose `opencode/default`, or start `petal acp opencode/default` from your editor. The ID names the route in Petal. OpenCode picks its model from its own settings. For another server, replace the ID and command array with your route name, executable, and arguments.

To route between two custom accounts, give both the same route name. Add a `[[accounts.env]]` entry with `name` and `value` for each profile variable. To compare quota, set each account's `usage` to a command array. Petal runs it with that account's environment and expects one number from 0 to 100 on stdout. Without a probe, Petal treats the account as unmetered.

## Terminal background

Petal uses a solid dark background by default. To use your terminal’s background, put this before the account tables in `config.toml` and restart Petal.

```toml
transparent_background = true
```

With Home Manager, set `programs.petal.settings.transparent_background = true;`.

## Stream replies in the terminal

Petal shows completed replies by default. To preview the current reply as it arrives, put this before the account tables and restart Petal.

```toml
stream_responses = true
```

The preview refreshes up to ten times per second and shows the latest 4 KiB of the current reply. The completed turn replaces the preview in the transcript. This setting applies to the TUI. Editors connected through `petal acp` already receive chunks as they arrive.

## Queue follow-ups

Put `busy_input = "follow_up"` before the account tables to queue messages until the running turn ends. The default is `"steer"`.

## Install with Home Manager

With the Petal flake in `inputs.petal`, add this module:

```nix
{
  imports = [ inputs.petal.homeManagerModules.default ];
  programs.petal = {
    enable = true;
    settings.accounts = [ {
      name = "work";
      provider = "claude";
      home = "~/.claude-work";
    } ];
  };
}
```

Home Manager merges declared settings into the writable TOML file on activation. A declared accounts or tiers list replaces that entire list in the file. Set `programs.petal.mutableSettings = false` for a store-backed file. Keep credentials in the provider's profile.
