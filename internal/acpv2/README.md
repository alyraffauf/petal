# ACP v2 transcript integration

Pass the `update` object from each `session/update` notification to
`Transcript.Apply` in receive order. Use one transcript per session.
`Entries` returns a snapshot with Petal's existing roles: `user`, `assistant`,
`system` (agent thoughts), `tool`, and `compaction`. The first update for an ID fixes its
position within the transcript, even if its content is empty. An ID can be
shared across message types and tool calls.

Message chunks append text without a separator. Adjacent ACP v2 text items in a tool call also concatenate. Other tool blocks
are separated by blank lines. A tool call with no display content field can
use MCP text from raw output. An explicit display field takes precedence,
including when the adapter clears it. An upsert with a content array replaces the
accumulated content. Both `null` and `[]` clear it, and omission preserves it.
A tool label uses the title, name, or ID in that order. Its default status
is `pending`. The projection includes text resources, link labels and URIs,
media placeholders, diffs, and terminal and session references. Terminal
output, plans, and inter-session messages have their own
update paths. Keep the original wire payload if a caller needs to preserve
unrecognized updates or content. Apply the renderer's usual sanitization to
projected text.

## Replay and search

For a full `session/resume` replay from `{"type":"start"}`, collect updates
into a fresh transcript. Treat `replay_complete` as the end of the requested
replay and publish the new snapshot after resume succeeds. Keep the prior
snapshot and search index if replay fails or is truncated. The marker alone
leaves the projection unchanged. Resume without `replayFrom` keeps any
existing live projection. A cursor-based replay is inclusive. A caller using
one must handle overlap before applying chunks, since two identical chunks
can legitimately belong to the same message.

Use `sessions.ProjectTranscript` to retain source IDs when archiving the projection. A successful replay reconciles observed entries with the immutable archive. SQLite row IDs remain stable, including for messages missing from a compacted or empty replay. A changed body is a new observation. Session deletion removes those rows and their full-text index entries.

[ACP v2 schema](https://raw.githubusercontent.com/agentclientprotocol/agent-client-protocol/main/schema/v2/schema.json)
