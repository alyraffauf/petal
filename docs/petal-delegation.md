# Petal delegation over MCP

Petal can expose four tools to a parent agent through a Petal-managed stdio MCP server. The parent chooses a configured model, starts a child in the same project directory, and reads the child's activity and final result. Petal routes the model through its configured accounts, including any account pin.

## Disable delegation

To disable the tools, set the top-level option in the Petal configuration used by the ACP proxy.

```toml
delegation = false
```

Delegation is enabled by default. The parent adapter must support stdio MCP servers. Petal reserves the server name `petal-delegation` while delegation is enabled. Petal sets the parent session and working directory when it binds the MCP server. The relay authenticates to a private socket with a per-session credential. Treat adapters running under your OS account as trusted processes. The tools accept neither a parent session ID nor a working directory.

## Tool calls

| Tool | Arguments | Response |
| --- | --- | --- |
| `petal_capabilities` | Optional `search` | Configured model IDs, labels and providers for the current account selection. The initial list shows up to 32 models. Search returns all matches. |
| `petal_delegate` | `model`, `task` | `childId` and `model` after Petal binds the child to the parent and schedules its prompt. |
| `petal_read` | `child_id`, optional `after` | A page of journal entries after the cursor. |
| `petal_wait` | `child_id`, optional `after` and `timeout` | The same page as `petal_read`. Waits for new activity, a final result, or the timeout. The timeout is in seconds, defaults to 30, and ranges from 1 to 60. |

Look up a model before delegating. Send its exact ID and a self-contained brief. The child shares the project directory and receives only the task as its prompt. Keep the returned `childId` and follow the `next` instruction in delegate, read and wait responses.

When the parent finishes its turn, Petal sends a follow-up prompt as soon as a child result is ready. The prompt identifies the child and asks the parent to read its result. If a read or wait already returned the final result, Petal skips the wake-up. This runs on the live parent connection. After disconnecting, the control center marks unread results on the corresponding child threads. Reopen the parent and use `petal_read` or `petal_wait` to retrieve a result. Petal clears the unread marker after the MCP relay delivers the terminal reply to the owning parent. Cancelling the parent clears pending wake-ups. Stored results remain available on retry.

In ACP v1, Petal keeps the editor's original prompt open while tracked child results are pending. Petal sends continuation prompts as results become ready and streams their output within the original editor turn. This happens only after an adapter reply with stop reason `end_turn`. Petal returns other stop reasons immediately. In ACP v2, the editor receives a prompt insertion acknowledgment and Petal schedules the follow-up after the adapter reports idle.

Start with `after: 0`. Read responses contain up to 100 entries in ascending `sequence` order. Each entry has a `kind` and may include JSON `data`. Pass `nextAfter` as the next cursor. When `hasMore` is true, call `petal_read` to drain the remaining entries, even if the response already contains a final result. An empty page keeps the cursor unchanged.

While the child runs, `status` is `running` or `needs_input`. A permission request wakes a waiting parent. Petal collects assistant text before publishing it to the journal.

The `result` field is null until the runtime records a final result. A final result has a `status` of `completed`, `cancelled`, or `failed` and may have JSON `data` with the stop reason and final assistant text in `result.data.answer`, or with failure details. A wait timeout returns an empty page with a null result while the child is still running. Calling `petal_wait` again with the same cursor continues waiting.

`petal_delegate` starts a new run on each successful call. If the response is lost, retrying the call may start another child. Repeating a read or wait with the same cursor can return previously seen activity. Use the sequence within each child to deduplicate entries.

## Ownership and recovery

Petal checks the direct parent-child binding for every journal read. A different parent receives the same error as it would for an unknown child. The parent can access only its own direct children in the project.

A child starts with an empty session MCP server list, including when Petal or the editor configured servers for the parent. Tools configured directly in the child adapter can still be available. Petal accepts the child's session updates for its journal and forwards supported permission requests through the parent session. Child filesystem and terminal callbacks to the editor are unsupported. A resumed child does not receive the Petal delegation MCP server. Cancelling the parent cancels its running children. Cancelling an individual wait stops the wait.

Petal stores bindings, ordered activity, and final results in the project's local session database. A parent can read committed activity and results after reconnecting to the same stored session. Child execution depends on the live runtime. After an abrupt process exit, the next journal read detects the released process lock and records a failed result.

Control Center shows saved delegation links in the thread tree and agent graph. Forgetting either endpoint removes its binding and journal from the local database.
