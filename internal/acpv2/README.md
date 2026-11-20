# ACP v2 transcript integration

Pass the `update` object from each `session/update` notification to
`Transcript.Apply` in receive order. Use one transcript per session.
`Entries` returns a snapshot with Petal's existing roles: `user`, `assistant`,
`system` (agent thoughts), and `tool`. The first update for an ID fixes its
position within the transcript, even if its content is empty. An ID can be
shared across message types and tool calls.

Message chunks append text without a separator. Tool content items are
separated by blank lines. An upsert with a content array replaces the
accumulated content. Both `null` and `[]` clear it, and omission preserves it.
A tool label uses the title, name, or ID in that order. Its default status
is `pending`. The projection includes text resources, link labels and URIs,
media placeholders, diffs, and terminal and session references. Terminal
output, plans, compactions, and inter-session messages have their own
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

Convert a complete authoritative snapshot to `[]sessions.Message` and call
`sessions.SaveTranscript(cwd, id, messages, true)`. The existing store can
atomically replace the indexed rows, clear them when the snapshot is empty,
and preserve row IDs when the stored role and text sequence is identical.
Use replacement on later authoritative checkpoints too, since an upsert can
rewrite an older entry. The existing TUI indexing helper skips empty input,
so allow empty authoritative snapshots through the v2 path.

The search database stores the text projection. A snapshot read from that
database can serve as a display fallback when resume requests no replay.
It has no ACP IDs or replay cursors. If there is cached history and only a
partial in-memory projection, preserve the cached prefix and avoid replacing
the index with that projection. Use full replay when the caller needs an
authoritative snapshot that can update prior IDs.

[ACP v2 schema](https://raw.githubusercontent.com/agentclientprotocol/agent-client-protocol/main/schema/v2/schema.json)
