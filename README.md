<div align="center">
    <img src="docs/petal.svg" width="128" height="128" alt="Petal's pixel orchid">
    <h1>Petal</h1>
</div>

Petal is a unified workbench for agents. It runs Claude Code and Codex in one terminal, with multi-agent orchestration, semantic search, and load balancing across the subscriptions you already pay for.

- **Orchestrate.** Claude can delegate a task to Codex, and Codex to Claude. Petal runs each [cross-vendor subagent](docs/petal-delegation.md) in the same directory and returns its result to the parent.
- **Remember.** Agents get [semantic search](docs/configuration.md#enable-semantic-history-search) over every transcript. Petal keeps them on your machine, so compaction never erases the original.
- **Route.** Petal [load balances](docs/configuration.md#route-across-subscriptions) across your work and personal subscriptions. Each new session starts on the account with the most quota left.
- **Integrate.** Run `petal acp` to add the same orchestration, memory, and routing to [any editor](docs/editors.md) that speaks the [Agent Client Protocol](https://agentclientprotocol.com/).

![A Petal session. Petal picks the work account for Claude. Claude delegates a test run to Codex, which runs as a child in the control center and then completes. Claude reports the bug, then searches the saved transcript and quotes the original failing test output.](docs/screenshots/workbench.gif)

Petal does not replace the Claude Code or Codex CLI. Each model runs in its vendor's CLI, and Petal controls that CLI through standard, supported APIs. Using Petal does not violate the vendor's terms of service.

## Start a session

1. Install the Claude Code or Codex CLI. Sign in with `claude` or `codex login`.
2. Run `nix profile add github:alyraffauf/petal`. From a checkout, run `nix profile add .` instead. The package includes both ACP adapters.
3. Run `petal` in your project directory. Select **new session**, choose a model, and send a prompt.

Petal discovers the default profiles. If you have separate work and personal logins, [configure your subscriptions](docs/configuration.md#route-across-subscriptions).

To build without Nix, run `go build -o petal .` with Go 1.27.3 or later. Install `claude-agent-acp` or `codex-acp` on your path alongside the corresponding CLI.

## Use Petal

- [Use the terminal UI](docs/terminal.md)
- [Connect an editor](docs/editors.md)
- [Orchestrate subagents](docs/petal-delegation.md)
- [Configure accounts, MCP servers, and semantic search](docs/configuration.md)
- [ACP support](docs/acp.md)
- [Test and verify Petal](docs/testing.md)

## License

Petal is released under the [MIT license](LICENSE.md).
