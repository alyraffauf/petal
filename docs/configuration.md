# Configure Petal

Petal looks for Claude Code and Codex on your path. To add accounts or change settings, create `~/.config/petal/config.toml`. If you set `XDG_CONFIG_HOME`, use `$XDG_CONFIG_HOME/petal/config.toml`. Pass `--config PATH` to use another file.

Top-level options such as `acp_v2` and `delegation` go before the first table in the file. Petal rejects a config that contains a key it does not know.

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

Once you name one profile for a provider, list every account that you want Petal to use for that provider. Relative home paths resolve against the config file.

Set the `capacity` of an account to its size relative to your other subscriptions. The default is 1. Petal recognizes Claude Max 5× and 20× automatically.

To give all detected Codex Pro profiles a weight of 4, add a `[[tiers]]` entry with `provider = "codex"`, `name = "pro"`, and `capacity = 4`. An explicit `capacity` on an account takes precedence.

### How quota windows affect routing

For a new session, Petal checks each account that offers the model. For each account, Petal uses the lowest remaining percentage among the applicable windows.

| Provider    | Windows Petal checks                                                                                                                                                                                                                                                                                       |
| ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Claude Code | The `get_usage` response includes shared `five_hour` and `seven_day` windows when available. If the requested model name contains `fable`, `opus`, `sonnet`, or `haiku`, Petal also checks the matching weekly window, such as `seven_day_fable`. The `claude/default` route uses only the shared windows. |
| Codex       | Petal checks the primary and secondary windows from `account/rateLimits/read` in `codex app-server`. Other model-specific limits fall outside this calculation. If Codex disallows ordinary usage, Petal counts the account as exhausted.                                                                  |

Suppose a Claude account has 80% left for five hours, 60% for the week, and 5% for Fable. Petal uses **5% for Fable** and **60% for Sonnet**, unless Sonnet has a tighter weekly limit of its own. Missing windows and reset times do not affect the score.

Petal ranks accounts by **capacity × remaining fraction**. A 5× subscription at 30% scores 1.5 and beats a 1× subscription at 90%, which scores 0.9. Petal breaks ties randomly. A resumed conversation keeps its original account.

Both probes run in the environment of the account. To supply your own percentage, set `usage` to a command array that prints one number from 0 to 100.

Automatic selection prefers an account with measured quota over an account with unknown quota. If every matching account is exhausted, on cooldown, or has a failed probe, Petal still opens the thread on the first matching account alphabetically. The provider enforces quota when you send a prompt.

A rate-limit error on a prompt puts the whole account on a five-minute cooldown for automatic selection. A Fable limit can therefore affect where new Sonnet sessions start. To pin an account and bypass the probes, run `petal --account work`.

## Run a custom ACP server

Petal has built-in discovery and quota checks for Claude Code and Codex. To use another ACP v1 server, configure the command that starts the server over stdio. Omit `provider` and `home`. Use the server's own sign-in and configuration.

Petal discovers OpenCode on your path. Sign in with `opencode auth login`, then choose the OpenCode model by its display name in **new session** or run `petal acp opencode/default`. Petal launches `opencode acp`. The picker reads the selected model name from a temporary ACP connection. If the lookup fails, it shows **OpenCode default**. Petal has no built-in quota probe for OpenCode.

For another harness, set the command to its ACP stdio entry point. For example, if your harness starts with `my-agent --acp`, configure it as follows.

```toml
[[accounts]]
name = "my-agent"

[[accounts.models]]
id = "my-agent/default"
command = ["my-agent", "--acp"]
```

Choose `my-agent/default` in the same picker or pass it to `petal acp`.

To route between two custom accounts, give both accounts the same route name. Add a `[[accounts.env]]` entry with `name` and `value` for each profile variable.

To compare quota, set the `usage` of each account to a command array. Petal runs the command with the environment of that account and expects one number from 0 to 100 on stdout. Without a probe, the quota of the account is unknown, so automatic selection prefers any measured account that has quota left.

## Give agents access to MCP tools

Add a top-level `[[mcp_servers]]` entry for each stdio Model Context Protocol (MCP) server. The adapter starts the command. If your editor has a different `PATH` from your shell, use an absolute executable path.

```toml
[[mcp_servers]]
name = "project-tools"
command = "/opt/project-tools/bin/mcp-server"
args = ["--stdio"]

[[mcp_servers.env]]
name = "PROJECT_TOOLS_MODE"
value = "read-only"
```

Use the command, arguments, and environment variables that your server supports. You can omit `args` and `env`.

Petal passes these definitions when you start, load, resume, or fork a session in the TUI or in an editor. If an editor supplies a server with the same case-sensitive name, the editor's definition wins. Reopen the client connection after you edit the config.

On ACP v2, `command` must be an absolute path.

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

Set the variable in Petal's environment to the complete header value, including `Bearer ` if the server requires it. A header takes its value from one of three keys.

| Key         | Source of the header value                                                |
| ----------- | ------------------------------------------------------------------------- |
| `from_env`  | The named environment variable.                                           |
| `from_file` | A file that contains the value, such as `/run/secrets/work-tools-header`. |
| `value`     | A literal in the config.                                                  |

Petal reads the value each time it forwards the definition. Keep credentials out of Nix expressions.

Before the first prompt in a new or resumed chat, Petal cancels session-scoped URL sign-in requests so optional MCP servers can’t hold up the conversation. A server that needs sign-in may be unavailable.

For a configured HTTP server without headers, install the npm package `mcp-remote` on Petal's PATH and connect once from a terminal:

```sh
npm install -g mcp-remote
petal mcp login SERVER_NAME
```

Replace `SERVER_NAME` with the configured server name and use the same `--config` as your Petal session. Use HTTPS for remote login, or HTTP on loopback. The command opens a browser if the endpoint requests OAuth. After the first successful connection, new and reopened sessions receive a stdio bridge for that server. Codex and Claude share the bridge's credentials under `$XDG_STATE_HOME/petal/mcp` (default `~/.local/state/petal/mcp`). Repeat login if authorization is revoked. Public HTTP servers without headers also require this one-time connection. Servers with configured headers use HTTP directly.

### Built-in Petal tools

Petal gives agents its search and delegation tools through an MCP server named `petal`. To disable the server, set `delegation = false`. [Orchestrate subagents](petal-delegation.md)

## Enable semantic history search

Petal keeps every transcript on your machine. Agents search them by keyword with the `search` tool and by meaning with the `recall` tool. With no settings, `recall` uses the local Ollama model `all-minilm:latest`. To install the model, run `ollama pull all-minilm`. If the model or the service is unavailable, `recall` returns keyword matches.

To offer keyword search only, set `provider = "off"` under `[history.embeddings]`. This setting replaces the `PETAL_HISTORY_EMBEDDINGS` environment variable. If an earlier installation left embeddings disabled, set `off` to keep keyword-only searches.

To use an OpenAI-compatible embeddings API, add the following table to `config.toml`. Opening a session then starts sending transcript excerpts from that session and its delegated descendants to the API in background batches, including older messages. Recall sends the search query separately.

```toml
[history.embeddings]
provider = "openai-compatible"
base_url = "https://inference.example.org/v1"
model = "my-embedding-model"
api_key_env = "INFERENCE_TOKEN"
```

Petal appends `/embeddings` to the base URL. Set `model` to the embedding model that the endpoint serves. If the endpoint accepts anonymous requests, omit `api_key_env`.

For native Ollama, set `provider = "ollama"`. The `model` defaults to `all-minilm:latest` and the `base_url` defaults to `http://127.0.0.1:11434`.

Both providers support HTTP and HTTPS on any host. Petal follows no redirects. When Petal makes a request, it reads the bearer token from the named environment variable. If the token is missing, the search uses keywords. [Search scope, coverage, and limits](acp.md#local-semantic-history-search)

## Use your terminal's background

Petal uses a solid dark background by default. To use the background of your terminal, add this option and restart Petal.

```toml
transparent_background = true
```

With Home Manager, set `programs.petal.settings.transparent_background = true;`.

## Stream replies in the terminal

Petal shows completed replies by default. To preview the current reply as it arrives, add this option and restart Petal.

```toml
stream_responses = true
```

On ACP v1, the preview refreshes up to ten times per second and shows the latest 4 KiB of the current reply. The completed turn replaces the preview in the transcript. This setting applies to the TUI. Editors connected through `petal acp` already receive chunks as they arrive.

## Queue follow-ups

By default, a message that you send during a running turn steers that turn. To queue messages until the turn ends, add this option.

```toml
busy_input = "follow_up"
```

The default value is `"steer"`.

## Install with Home Manager

With the Petal flake in `inputs.petal`, add this module.

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

On activation, Home Manager merges the declared settings into the writable TOML file. A declared accounts list or tiers list replaces that entire list in the file. For a store-backed file, set `programs.petal.mutableSettings = false`. Keep credentials in the provider's profile.

### Use shared Home Manager MCP servers

Enable the integration to use the same stdio and HTTP servers in Petal and in other Home Manager programs.

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

Petal imports the enabled stdio and HTTP servers. For an HTTP header, `"{env:VARIABLE}"` reads the entire value from Petal's environment. An entry in `programs.petal.settings.mcp_servers` overrides a shared server with the same name.

Home Manager reads file-backed environment variables when the adapter starts the server. With mutable settings, activation replaces the MCP server list with the declared entries while the integration is enabled.

## Diagnose slow ACP requests

Set `PETAL_ACP_TRACE` to a file path in the environment of the ACP server, then restart the connection. Use an absolute path, because an editor can start the server in any directory. Petal appends JSON lines to the file. The parent directory of the file must exist. To stop tracing, unset the variable and restart. The file grows until you disable tracing.

Each record has a timestamp. Records for a request also have the elapsed microseconds. Filter by `request` to follow a prompt or a configuration change.

- Compare `adapter_response_received` with `editor_response_write` to see how long Petal held a reply. A successful write means that the editor transport accepted the bytes.
- On ACP v2, the prompt reply acknowledges insertion. Match `adapter_session` with a later `adapter_idle` event to see when the adapter reported idle.

The trace records model and effort selections, including the value that the adapter acknowledged when the adapter supplies one. The trace omits message bodies, tool arguments, and raw session IDs.
