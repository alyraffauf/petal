# ACP support

Petal speaks the [Agent Client Protocol](https://agentclientprotocol.com/) (ACP) v1 over stdio by default. It has two roles.

- **Agent.** `petal acp` routes editor requests to the ACP server of the selected account and translates session IDs. Petal includes integrations for Claude Code and Codex. You can also [configure a custom server](configuration.md#run-a-custom-acp-server).
- **Client.** The TUI handles prompts, permissions, project files, terminals, elicitation, session controls, and history.

For conformance results, see [Test and verify Petal](testing.md).

## Session storage

Petal stores session IDs, titles, activity, fork relationships, settings, and transcript text in `$XDG_STATE_HOME/petal/conversations/<project-hash>/index.sqlite`. TUI sessions and editor sessions share that database and its SQLite FTS5 index.

- **ACP v1.** Petal records the prompt before it forwards the prompt. Petal records the output when the prompt returns.
- **ACP v2.** Petal records echoed messages and tool updates when the session reaches idle.

An interrupted process can lose the output of an unfinished turn. Deleting a session removes its messages and its search entries.

## Opt in to ACP v2

Put this option before the first table in `config.toml`.

```toml
acp_v2 = true
```

The TUI then requests v2. When an editor requests v2, `petal acp` checks the adapters that offer the selected model. If any adapter negotiates v2, Petal routes among those accounts. If none do, Petal opens v1 connections. An editor that requests v1 continues to use v1.

On v2, reopening a session requests a full replay. Petal adds the newly observed messages and compaction events to the archive.

- Earlier text and its search references survive an empty replay or a compacted replay.
- A revised message body gets its own row. Petal never rewrites a stored message.
- For messages without adapter IDs, replay matches occurrences by role and exact text. Petal cannot always tell identical anonymous messages apart across separate compacted snapshots.

## Capability negotiation

Editors see the capabilities that the eligible accounts share. After routing, each session keeps its original adapter. If you pin one account, the editor sees the capabilities of that adapter.

## Skills

Codex advertises each skill as a command named `$name`. Some editors, such as T3 Code, list only those commands as skills. Claude Code advertises skills as plain commands and marks the source at the end of the description, such as `(user)`, `(project)`, or `(plugin:…)`.

For a Claude Code session, Petal also advertises each marked skill as `$name`. When a prompt starts with one of those names, Petal sends it to Claude Code as `/name`. A `$name` later in the prompt stays as written. Built-in commands carry no source mark, so Petal does not list them as skills.

## Permissions and terminals

The TUI asks before it writes files or runs commands.

When a command exits, Petal stops the processes that remain in the command's process group. Petal preserves the exit status and gives the output streams up to one second to drain. Killing or releasing a running terminal also stops its process group.

## Attachments

Petal sends an attachment inline when the file is at most 7 MiB and the adapter accepts its type. Larger files and other types go as a file reference.

## Limits of the TUI

- Key bindings are fixed. Petal has no key remapping.
- The mouse scrolls and clicks. It does not select or copy text.
- The TUI does not show prompt suggestions or editor cursors.
- A fork copies the whole conversation. You cannot fork from an earlier turn.
- A subagent that the adapter starts itself is read-only in the control center. A subagent delegated through Petal can be opened and cancelled.

## Local semantic history search

Agents search the archive with two tools. `search` matches keywords, identifiers, and quoted phrases. `recall` matches by meaning and needs an embeddings provider.

By default, `recall` uses Ollama at `127.0.0.1:11434` with `all-minilm:latest`. To install the model, run `ollama pull all-minilm`. To change or disable the provider, see [Enable semantic history search](configuration.md#enable-semantic-history-search). With the provider set to `off`, Petal does not offer `recall`.

`recall` returns one page of up to ten hits. The page alternates keyword hits and semantic hits, starting with a keyword hit. Each hit has a `source` of `keyword` or `semantic`.

`recall` returns keyword hits only in these cases.

- The provider is unavailable or busy. The response then has `retrieval: "fts"`.
- The query looks like an identifier or contains a number. `ERR_SOCKET_902`, `v2`, `#4`, and `3` all count.

The identifier rule keeps `ERR_SOCKET_902` from matching `ERR_SOCKET_901` by vector similarity.

### What Petal sends to the provider

While a history-enabled session is open, Petal sends its saved messages and those of its delegated descendants to the configured embeddings provider in background batches of up to eight. Each input contains the first 1,024 characters of a message. This includes older transcripts, even before anyone calls `recall`. Set the provider to `off` to disable indexing and recall.

A semantic query sends the query text and scores all indexed messages in scope. There is no message-age cutoff. The response reports the indexed count in `coverage.indexed` and the total scoped count in `coverage.window`. While `coverage.partial` is true, keyword search can still find messages awaiting indexing.

### Limits

- One search embeds at a time in each Petal process.
- A search has ten seconds in total. If the provider fails or runs out of time, Petal returns the keyword hits.
- The archive stores four bytes per embedding dimension for each indexed message. Deleting a message deletes its vector.
- Petal rebuilds the vectors when the Ollama model digest changes. For an OpenAI-compatible endpoint, use a versioned model ID when you replace a deployment.
