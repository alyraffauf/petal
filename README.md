<img src="assets/icon.svg" width="64" height="64" alt="">

# Petal

Petal starts an ACP session on the subscription with the most remaining quota for the selected model. The session stays on that adapter. Use the Bubble Tea and Bloom terminal client directly, or run the proxy as an ACP agent for another editor.

## Nix and Home Manager

Run the local flake with `nix run . -- tui --demo`, or build the binary with `nix build .#petal`. The lock file pins nixos-unstable and Home Manager master.

Add Petal as a flake input named `petal`, then import its module into your Home Manager configuration:

```nix
{
  imports = [ inputs.petal.homeManagerModules.default ];
  programs.petal = {
    enable = true;
  };
}
```

Settings are mutable by default. On activation Home Manager merges the declared TOML keys over local values, preserving undeclared keys. Set `programs.petal.mutableSettings = false` for a store-backed config. Keep credentials outside Nix settings, since declared values enter the store.

## Try Petal

Build with Go 1.27.3 or later. The demo runs locally without an adapter or account.

```sh
go build -o petal .
./petal tui --demo
```

Press **Ctrl+R** to replay a sample turn in demo mode.

The transcript is a continuous Markdown conversation. It includes user prompts, completed replies, tool results, permission decisions, and session notes. While the agent works, the TUI buffers text and shows its status. A successful turn publishes the reply in one update. Interrupted turns discard unfinished text. **F3** opens the whole conversation in a scrollable reader. Escape returns to the composer.

## Connect an agent

Run `petal` or `petal tui` in the project directory. Type to filter the model list and press **Enter** to start with automatic routing. To pick the subscription yourself, highlight a model, press **Tab**, then select an account. **Ctrl+N** opens the picker for a new thread when the current turn is idle. Escape returns to your draft.

Petal discovers Claude Code and Codex on PATH. The Nix package includes ACP bridges for locally installed Claude Code and Codex. For a standalone Go build, install `claude-agent-acp` or `codex-acp` on PATH. Sign in using each provider's CLI. Petal offers Claude model aliases and visible entries from each Codex profile’s model cache. Each provider also has a `provider/default` entry for its configured model. Discovered concrete model selections are sent to the adapter over ACP before the first prompt.

### Route across subscriptions

Each configured account represents one signed-in profile. Declare profile directories in `$XDG_CONFIG_HOME/petal/config.toml` (default `~/.config/petal/config.toml`):

```toml
[accounts.work]
capacity = 5 # relative to the baseline subscription
provider = "codex"
home = "~/.codex-work"

[accounts.personal]
provider = "codex"
home = "~/.codex"

[accounts.claude-work]
provider = "claude"
home = "~/.claude-work"
```

Petal expands `~/` using your home directory and resolves relative paths against the config file. The `home` setting supplies `CODEX_HOME` or `CLAUDE_CONFIG_DIR` to the agent and its quota probe and takes precedence over that variable in the account environment. Sign in to each profile using the same directory, for example `CLAUDE_CONFIG_DIR="$HOME/.claude-work" claude` or `CODEX_HOME="$HOME/.codex-work" codex login`.

Named profiles replace the automatically discovered default account for that provider. Explicit `accounts.NAME.models.MODEL` command arrays override discovered launch commands. Those commands must start an authenticated stdio ACP agent configured for the model.

Petal reads the Codex plan ID from the local app server and the Claude tier from `claude auth status --json` in each profile. Press Tab on a model to see its accounts and detected tiers. Claude’s explicit `max_5x` and `max_20x` tiers supply estimated weights of 5 and 20 relative to Pro. For other tiers the default weight is 1. You can set a weight once for every account reporting the same provider tier. For example, if your measured Codex Pro workload supports four times as much usage as your baseline:

```toml
[tiers.codex]
pro = 4
```

Auto probes accounts that support the requested model in parallel. For Codex it asks the local app server for rate limits and takes the lowest remaining percentage across its primary and secondary windows. Set `accounts.NAME.usage = ["/path/to/probe", "arg"]` to override this or provide quota data for Claude Code. A probe inherits the profile environment and prints a number from 0 to 100.

- Set `accounts.NAME.capacity` to the relative size of a full subscription for the workload you route. An account setting overrides tier detection. Auto picks the largest estimated remaining capacity (capacity multiplied by the remaining fraction). For example, a 5× account at 30% has 1.5 baseline subscriptions left and beats a 1× account at 90%. Equal scores split new sessions randomly.
- Accounts with no quota source are labeled **unmetered**. Auto uses them when no measured account has capacity. Exhausted, invalid, timed-out and failed probes are excluded.
- A structured rate-limit error puts the account on a five-minute Auto cooldown shared between Petal processes. A manual pin bypasses probes and cooldowns.
- Each thread keeps its original account. Petal never replays a prompt on another subscription after a failure.

```sh
petal tui sonnet                 # Auto
petal tui gpt-6.2-sol --account work
petal tui sonnet --resume
petal serve sonnet
```

Enter sends the draft, arrow keys edit it, the mouse wheel, trackpad, or Page Up and Page Down scroll the backlog, Ctrl+End returns to the latest message, Escape cancels, and Ctrl+C quits. When an agent requests permission, enter the number of an offered option, or press Escape to decline. The client displays agent text, thoughts, and tool activity. Adapters must provide their own filesystem and terminal facilities.

Petal saves the last session ID for each model and working directory under `$XDG_STATE_HOME/petal/sessions` (default `~/.local/state/petal/sessions`). The adapter owns conversation history and must support ACP session loading to resume. Overriding an existing route requires the selected account to have access to that history.

Both commands accept `--config PATH`. Existing editor registrations can keep using `petal CONFIG MODEL [ACCOUNT]`, including explicit legacy JSON files.

## Property tests

Run `CGO_ENABLED=0 go test ./...` to replay deterministic regressions and every fuzz target's seeds. Run `scripts/check-properties` to replay them and then generate new cases for each target. The default budget is five seconds per target with two workers. A longer campaign can use:

```sh
FUZZ_TIME=1m FUZZ_WORKERS=4 scripts/check-properties
```

The targets use Go's native fuzzer. A failure prints a replay command and saves its minimized input under `testdata/fuzz/<target>/`. Keep that input or add an equivalent deterministic regression when fixing the bug. For a single target, run:

```sh
CGO_ENABLED=0 go test -run '^$' -fuzz '^FuzzSessionResumePlacement$' -fuzztime=1m .
```

The session target uses local ACP subprocesses and temporary state directories. It creates and resumes sessions across two accounts, projects, and models, then asks the running adapter which account received the request. The Auto routing target compares eligible winners against an independent exact-arithmetic model. Both run offline.

The transcript target generates interleaved message chunks, tool updates, cancellations, and history replay. The five rendering targets check frame dimensions, scrolling through a changing backlog, Markdown cache consistency, visible content, and reader layout. These campaigns cover bounded histories and synthetic adapters. Live provider behavior, terminal input delivery, and the timing of concurrent external requests need separate integration checks.
