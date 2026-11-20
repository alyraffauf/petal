# Configure Petal

Petal discovers signed-in Claude Code and Codex profiles through their CLIs. To add accounts, create `~/.config/petal/config.toml`. If you set `XDG_CONFIG_HOME`, use `$XDG_CONFIG_HOME/petal/config.toml`. Pass `--config PATH` to use another file.

## Route across subscriptions

```toml
[accounts.personal]
provider = "codex"
home = "~/.codex"

[accounts.work]
provider = "codex"
home = "~/.codex-work"
capacity = 5

[accounts.claude-work]
provider = "claude"
home = "~/.claude-work"
```

Sign in to each directory before using it. For example, run `CODEX_HOME="$HOME/.codex-work" codex login` or `CLAUDE_CONFIG_DIR="$HOME/.claude-work" claude`. Once you name a provider's profiles, list every account you want Petal to use for that provider. Relative home paths resolve against the config file.

Petal compares **capacity × remaining quota fraction** across accounts that offer the model. A 5× account at 30% beats a 1× account at 90%. Ties split randomly. Codex uses the tighter rate-limit window. Claude checks its five-hour, weekly and applicable model windows.

Capacity defaults to 1. Detected Claude Max 5× and 20× tiers use their named weights. Set `tiers.codex.pro = 4` at the top level to give all detected Codex Pro profiles a relative weight of 4. An account's `capacity` takes precedence.

Auto uses an unmetered account when no measured account has quota. Failed probes and exhausted accounts are excluded. A rate-limit error starts a five-minute cooldown. Use `petal --account work` to bypass probing and pin the account. For a custom quota source, set `accounts.work.usage` to a command array that prints the percentage remaining.

## Terminal background

Petal uses your terminal’s background by default. To use Petal’s solid dark background, put this before the account tables in `config.toml` and restart Petal.

```toml
transparent_background = false
```

With Home Manager, set `programs.petal.settings.transparent_background = false;`.

## Queue follow-ups

Put `busy_input = "follow_up"` before the account tables to queue messages until the running turn ends. The default is `"steer"`.

## Connect Zed

Add Petal under `agent_servers` in Zed's `settings.json`. Use an absolute executable path if Zed cannot find Petal on its path.

```json
{
  "agent_servers": {
    "Petal": { "type": "custom", "command": "petal", "args": ["acp"] }
  }
}
```

Choose **Petal** when starting an agent thread. To select a provider explicitly, use `["acp", "codex/default"]` or `["acp", "claude/default"]`.

## Install with Home Manager

With the Petal flake in `inputs.petal`, add this module:

```nix
{
  imports = [ inputs.petal.homeManagerModules.default ];
  programs.petal = {
    enable = true;
    settings.accounts.work = {
      provider = "claude";
      home = "~/.claude-work";
    };
  };
}
```

Home Manager merges declared settings into the writable TOML file on activation. Set `programs.petal.mutableSettings = false` for a store-backed file. Keep credentials in the provider's profile.
