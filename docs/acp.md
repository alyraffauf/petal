# ACP support and verification

Petal speaks ACP v1 over stdio. As an agent, `petal acp` routes editor requests to Claude or Codex and translates session IDs. As a client, the TUI handles prompts, permissions, project files, terminals, elicitation, session controls, and history. It stores thread IDs and settings in `$XDG_STATE_HOME/petal/conversations`. The adapter stores and replays the transcript.

## Capability negotiation

Editors see the capabilities shared by eligible accounts. After routing, each session keeps its original adapter. Pinning one account exposes that adapter's capabilities. Petal forwards reply chunks to editors. Its own TUI waits for the completed reply.

The TUI asks before writing files or running commands. When a command exits, Petal stops processes still in its terminal process group, preserves the command's exit status, and gives the output streams up to one second to drain. Killing or releasing a running terminal also stops its process group. Terminal output stays available to an attached tool result after release.

Attachments use the negotiated content type up to 7 MiB when the encoded request fits. Larger files and unsupported inline types use a file reference.

## Reproduce the checks

```sh
CGO_ENABLED=0 go test ./...
FUZZ_TIME=10s scripts/check-properties
ACP_TCK_DIR=/path/to/acp-tck scripts/check-acp /tmp/petal-acp.json matrix
```

Use Go with a C compiler for `go test -race ./...`. The matrix also needs `uv` and the pinned kit checkout. It runs five adapter fixtures, writes per-fixture reports, and checks that every registered requirement passed somewhere in the matrix. All 134 requirement IDs passed, with zero failures. Some optional checks pass when the adapter emits no corresponding traffic. The upstream kit tests the agent role. Client evidence comes from Petal's wire, PTY, and property tests. [Pinned ACP test kit](https://github.com/agentclientprotocol/acp-tck/tree/7c71c4d1af0994e73bdc100c9f7f6b98f2319cda) · [Upstream coverage limits](https://agentclientprotocol.com/libraries/testing)

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
