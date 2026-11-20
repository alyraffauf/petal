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

Set the account's `capacity` to its size relative to your other subscriptions. Capacity defaults to 1. Petal recognizes Claude Max 5× and 20× automatically. To give all detected Codex Pro profiles a weight of 4, add a `[[tiers]]` entry with `provider = "codex"`, `name = "pro"`, and `capacity = 4`. An account's explicit `capacity` takes precedence.

### How quota windows affect routing

For a new session, Petal checks each account that offers the model and uses the lowest remaining percentage among the applicable windows.

| Provider | Windows Petal checks |
| --- | --- |
| Claude Code | The `get_usage` response includes shared `five_hour` and `seven_day` windows when available. If the requested model name contains `fable`, `opus`, `sonnet`, or `haiku`, Petal also checks the matching weekly window, such as `seven_day_fable`. The `claude/default` route uses only the shared windows. |
| Codex | Petal checks the primary and secondary windows from `account/rateLimits/read` in `codex app-server`. Other model-specific limits fall outside this calculation. If Codex disallows ordinary usage, Petal counts the account as exhausted. |

Suppose a Claude account has 80% left for five hours, 60% for the week, and 5% for Fable. Petal uses **5% for Fable** and **60% for Sonnet**, unless Sonnet has a tighter weekly limit of its own. Missing windows and reset times do not affect the score.

Petal ranks accounts by **capacity × remaining fraction**. A 5× subscription at 30% scores 1.5 and beats a 1× subscription at 90%, which scores 0.9. Petal breaks ties randomly. Resuming a conversation keeps its original account.

Both probes run in the account's environment. To supply your own percentage, set `usage` to a command array that prints one number from 0 to 100. Auto prefers an account with measured quota over one with unknown quota. If every matching account is exhausted, on cooldown, or has a failed probe, Petal still opens the thread on the first matching account alphabetically. The provider enforces quota when you send a prompt.

A prompt rate-limit error puts the whole account on a five-minute cooldown for automatic selection. A Fable limit can therefore affect where new Sonnet sessions start. Use `petal --account work` to pin an account and bypass probing.

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

## Give agents access to MCP tools

Add a top-level `[[mcp_servers]]` entry for each stdio Model Context Protocol (MCP) server. The adapter starts the command. Use an absolute executable path if your editor has a different `PATH` from your shell.

```toml
[[mcp_servers]]
name = "project-tools"
command = "/opt/project-tools/bin/mcp-server"
args = ["--stdio"]

[[mcp_servers.env]]
name = "PROJECT_TOOLS_MODE"
value = "read-only"
```

Use the command, arguments, and environment variables that your server supports. You can omit `args` and `env`. Petal passes these definitions when you start, load, resume, or fork a session in the TUI or an editor. If an editor supplies a server with the same case-sensitive name, its definition wins. Reopen the client connection after editing the config.

### Remote MCP servers

For Streamable HTTP, set `url` and omit `command`. Public endpoints and servers that handle OAuth can omit headers.

```toml
[[mcp_servers]]
name = "work-tools"
url = "https://tools.example.org/mcp"

[[mcp_servers.headers]]
name = "Authorization"
from_env = "WORK_TOOLS_AUTHORIZATION"
```

Set the variable in Petal’s environment to the complete header value, including `Bearer ` if required. Use `from_file = "/run/secrets/work-tools-header"` for a file containing that value, or `value` for a literal. Petal reads the value each time it forwards the definition. Keep credentials out of Nix expressions.

The adapter handles OAuth and remembers the login for its profile. Claude Code can request sign-in when you reconnect the server through its MCP command. On first use Codex can ask you to run `codex mcp login <name>`. Use the same Codex home and server URL as the Petal account. If the server exists only in Petal, supply its URL to the CLI with `codex -c 'mcp_servers.work-tools.url="https://tools.example.org/mcp"' mcp login work-tools`.

### Petal delegation

Petal exposes its delegation tools by default. Set `delegation = false` to disable them. [Petal delegation over MCP](petal-delegation.md)

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

### Use shared Home Manager MCP servers

Enable the integration to use the same stdio and HTTP servers in Petal and other Home Manager programs.

```nix
{
  programs.mcp.enable = true;
  programs.mcp.servers.project-tools = {
    command = "/opt/project-tools/bin/mcp-server";
    args = [ "--stdio" ];
  };

  programs.petal.enable = true;
  programs.petal.enableMcpIntegration = true;
}
```

Petal imports enabled stdio and HTTP servers. For an HTTP header, `"{env:VARIABLE}"` reads the entire value from Petal’s environment. An entry in `programs.petal.settings.mcp_servers` overrides a shared server with the same name. Home Manager reads file-backed environment variables when the adapter starts the server. With mutable settings, activation replaces the MCP server list with the declared entries while the integration is enabled.
