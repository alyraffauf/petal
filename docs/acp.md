# ACP support and verification

Petal speaks ACP v1 over stdio by default. As an agent, `petal acp` routes editor requests to the selected account's ACP server and translates session IDs. Petal includes integrations for Claude Code and Codex. You can also [configure a custom server](configuration.md#run-a-custom-acp-server). As a client, the TUI handles prompts, permissions, project files, terminals, elicitation, session controls, and history. Petal stores session IDs, titles, activity, fork relationships, and settings in `$XDG_STATE_HOME/petal/conversations/<project-hash>/index.sqlite`. The TUI and editor sessions served by `petal acp` store transcript text in the same database and SQLite FTS5 index. On ACP v1 the proxy records the submitted prompt before forwarding it and commits received output when the prompt returns. On ACP v2 it indexes echoed messages and tool updates when the session reaches idle. An interrupted process can lose output from an unfinished turn. Search covers messages Petal has recorded locally, including tool output. Opening a session that supports ACP history replay indexes the replayed messages. Deleting a session removes its local messages and search entries.

## Opt in to ACP v2

Put this before the first table in `config.toml`.

```toml
acp_v2 = true
```

The TUI requests v2. When an editor requests v2, `petal acp` checks the adapters that offer the selected model. If any negotiate v2, Petal routes among those accounts. If none do, it opens fresh v1 connections. An editor that requests v1 continues to use v1.

On v2, the prompt response acknowledges the user's message. The TUI keeps the session busy until the adapter reports idle. Reopening requests a full replay. Petal replaces the local search index after the replay and resume succeed.

## Capability negotiation

Editors see the capabilities shared by eligible accounts. After routing, each session keeps its original adapter. Pinning one account exposes that adapter's capabilities. Petal forwards reply chunks to editors. Its TUI waits for the completed reply by default. [Configure streaming previews](configuration.md#stream-replies-in-the-terminal)

The TUI asks before writing files or running commands. When a command exits, Petal stops processes still in its terminal process group, preserves the command's exit status, and gives the output streams up to one second to drain. Killing or releasing a running terminal also stops its process group. Terminal output stays available to an attached tool result after release.

Attachments use the negotiated content type up to 7 MiB when the encoded request fits. Larger files and unsupported inline types use a file reference.

## Reproduce the checks

```sh
CGO_ENABLED=0 go test ./...
FUZZ_TIME=10s scripts/check-properties
ACP_TCK_DIR=/path/to/acp-tck scripts/check-acp /tmp/petal-acp.json matrix
ACP_TCK_DIR=/path/to/acp-tck scripts/check-acp /tmp/petal-v2.json v2-full
```

The regular Go suite runs the fuzz seeds and deterministic transcript histories. On pull requests and pushes to `main`, CI also generates new inputs for every Go fuzz target for three seconds each. Go mutates seed inputs and retains inputs that reach new code paths. Set `FUZZ_TIME` for a longer local run. If fuzzing finds a failure, download the `fuzz-inputs` CI artifact, extract it at the repository root, and run the replay command printed in the failed step.

The pinned v2 kit reported **CONFORMANT** for the baseline and full fixture profiles. The full profile passed 252 checks and skipped 63. Those results cover Petal as a proxy with the reference adapter. A separate live check exercised a tool call and full replay with bundled Claude ACP 0.100.0 over v2, and with Codex ACP 2.1.1 over v1.

Session queries live in `internal/sessions/queries.sql`. Add schema changes as versioned Goose migrations in `internal/sessions/migrations/`, then run `nix shell --inputs-from . nixpkgs#sqlc -c sqlc generate` at the repository root and commit the generated Go files. Keep version 2 reserved for databases created by the earlier importer.

Use Go with a C compiler for `go test -race ./...`. The v1 matrix and v2 profiles also need `uv` and the pinned kit checkout. The v1 matrix runs five adapter fixtures, writes per-fixture reports, and checks that every registered requirement passed somewhere in the matrix. All 134 requirement IDs passed, with zero failures. Some optional checks pass when the adapter emits no corresponding traffic. The upstream kit tests the agent role. Client evidence comes from Petal's wire, PTY, and property tests. [Pinned ACP test kit](https://github.com/agentclientprotocol/acp-tck/tree/7c71c4d1af0994e73bdc100c9f7f6b98f2319cda) · [Upstream coverage limits](https://agentclientprotocol.com/libraries/testing)

## Live comparisons

| Compared on November 19, 2026 | Observed |
| --- | --- |
| Codex CLI 0.170.0 and the Petal TUI | Both completed the same GPT-6.2-Sol prompt. Petal resumed the thread and recalled its answer. Codex ACP reset an explicitly selected Read-only preset on load. Petal now asks before continuing with changed settings and can restore the saved choice. |
| Zed 1.28.1 with Codex ACP 2.1.1 directly and through Petal | Both paths completed a file-read turn, replayed it after restarting Zed, and cancelled a live turn. All 48 session updates crossing the proxy matched after session ID translation. Reasoning reverted to Low on both paths. |

The Zed comparison used one authenticated account. Fresh sign-in, destructive operations, and multimodal turns were outside that run. Petal's TUI exposes adapter commands and settings. Native Codex also offers speed tiers, presets, IDE selection, key remapping and an agent dashboard. Petal supports whole-conversation forks and a project control center with read-only subagent details. Cursor selection and prompt suggestions have no dedicated TUI controls.

## Long sessions

```sh
CGO_ENABLED=0 go test ./internal/tui -run '^$' -bench 'Benchmark(WarmBacklog|OpenBacklog|OpenLargeToolResult|SessionFrame)' -benchmem
CGO_ENABLED=0 go test ./internal/acphost -run '^$' -bench BenchmarkTerminalOutputAtLimit -benchmem
```

Opening a 20-row viewport on 4,000 Markdown messages fell from about 143 ms to 0.27 ms on the test machine. A warm 100×30 TUI frame took 0.60–0.72 ms with 4,000 messages. For a 16,384-line plain-text fence, tokenizing the log as a single span reduced first layout from 114 ms and 208 MB to 76–93 ms and about 37 MB. Resizing while scrolled back can also reflow earlier messages. These figures exclude provider replay and terminal painting.
