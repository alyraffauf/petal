# Proposal: Petal delegation over MCP

Petal can expose three tools to a parent agent through a Petal-managed stdio MCP server. The parent chooses a configured model, starts a child in the same project directory, and reads the child's activity and final result. Petal routes the model through its configured accounts, including any account pin.

## Enable delegation

Set the top-level option in the Petal configuration used by the ACP proxy.

```toml
delegation = true
```

Delegation defaults to off. The parent adapter must support stdio MCP servers. Petal reserves the server name `petal-delegation` while delegation is enabled. Petal sets the parent session and working directory when it binds the MCP server. The relay authenticates to a private socket with a per-session credential. Treat adapters running under your OS account as trusted processes. The tools accept neither a parent session ID nor a working directory.

## Tool calls

| Tool | Arguments | Response |
| --- | --- | --- |
| `petal_delegate` | `model`, `task` | `childId` and `model` after Petal binds the child to the parent and schedules its prompt. |
| `petal_read` | `child_id`, optional `after` | A page of journal entries after the cursor. |
| `petal_wait` | `child_id`, optional `after` and `timeout` | The same page as `petal_read`. Waits for new activity, a final result, or the timeout. The timeout is in seconds, defaults to 30, and ranges from 1 to 60. |

Start with `after: 0`. Read responses contain up to 100 entries in ascending `sequence` order. Each entry has a `kind` and may include JSON `data`. Pass `nextAfter` as the next cursor. When `hasMore` is true, call `petal_read` to drain the remaining entries, even if the response already contains a final result. An empty page keeps the cursor unchanged.

While the child runs, `status` is `running` or `needs_input`. A permission request wakes a waiting parent. Petal collects assistant text before publishing it to the journal.

The `result` field is null until the runtime records a final result. A final result has a `status` of `completed`, `cancelled`, or `failed` and may have JSON `data` with the stop reason and final assistant text in `result.data.answer`, or with failure details. A wait timeout returns an empty page with a null result while the child is still running. Calling `petal_wait` again with the same cursor continues waiting.

`petal_delegate` starts a new run on each successful call. If the response is lost, retrying the call may start another child. Repeating a read or wait with the same cursor can return previously seen activity. Use the sequence within each child to deduplicate entries.

## Ownership and recovery

Petal checks the direct parent-child binding for every journal read. A different parent receives the same error as it would for an unknown child. The parent can access only its own direct children in the project.

Child tools and permissions remain subject to the adapter and Petal's permission handling. Petal forwards supported child permission requests through the parent session. The child starts without the Petal delegation MCP server. Cancelling the parent cancels its running children. Cancelling an individual wait stops the wait.

Petal stores bindings, ordered activity, and final results in the project's local session database. A parent can read committed activity and results after reconnecting to the same stored session. Child execution depends on the live runtime. After an abrupt process exit, the next journal read detects the released process lock and records a failed result.

Control Center shows saved delegation links in the thread tree and agent graph. Forgetting either endpoint removes its binding and journal from the local database.
