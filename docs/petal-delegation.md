# Orchestrate subagents

An agent can delegate a task to a subagent on another configured model. Claude can delegate to Codex, and Codex to Claude. Petal runs the subagent in the same project directory, routes it through your accounts, and returns its result to the parent.

Delegation goes one level deep. A subagent cannot delegate further.

## Watch and cancel subagents

Open the control center with **F2**. A delegated subagent appears under its parent as **Delegated**, with its status.

- **Enter** opens the subagent as a session.
- **Ctrl+X** cancels a running subagent. The parent keeps running.
- **Ctrl+G** opens the agent graph.
- A subagent with a result that its parent has not read is marked unread.

Petal does not title a delegated subagent, so it shows as "untitled conversation" until you open it and send a prompt.

## Disable delegation

Delegation is enabled by default. To disable it, set the top-level option in your config. The option also removes the search tools, because one MCP server provides them all.

```toml
delegation = false
```

The parent adapter must support stdio MCP servers. While delegation is enabled, Petal reserves the MCP server name `petal`.

## Tools

Petal gives each parent agent these tools through an MCP server that Petal manages.

| Tool       | Arguments                                              | Response                                                                                            |
| ---------- | ------------------------------------------------------ | --------------------------------------------------------------------------------------------------- |
| `models`   | Optional `search`                                      | The configured model IDs, labels, and providers. The initial list shows up to 32 models.            |
| `delegate` | `model`, `task`                                        | The `childId` of a new subagent. The subagent receives only `task` as its prompt.                   |
| `activity` | `child_id`, optional `after`                           | Up to 100 entries from the activity journal of the subagent, and its final result.                  |
| `wait`     | `child_id`, optional `after` and `timeout`             | The same page as `activity`, after new activity or a timeout of 1 to 60 seconds. The default is 30. |
| `search`   | `query`, optional `before`                             | Keyword matches from the saved messages of the parent and its subagents, newest first.              |
| `recall`   | `query`                                                | One page of semantic and keyword matches. Available when an embeddings provider is enabled.         |
| `open`     | Optional `reference`, `offset`, and `context` (0 to 8) | An archived message and up to 8 neighbors on each side, within 4096 bytes.                          |

`delegate` starts a new subagent on each successful call. If the response is lost, a retry can start a second subagent.

A subagent has a `status` of `running` or `needs_input` while it works. Its final result has a `status` of `completed`, `cancelled`, or `failed`. The final assistant text is in `result.data.answer`.

## How a result reaches the parent

When the parent ends its turn and a subagent result is ready, Petal sends the parent a follow-up prompt. The prompt names the subagent and tells the parent to continue its task. If a read or a wait already returned the final result, Petal skips the follow-up prompt.

The follow-up prompt needs the live parent connection. If the parent has disconnected, reopen it and call `activity` to retrieve the result. Cancelling the parent clears the pending follow-up prompts. Stored results remain available.

Petal sends a follow-up prompt only after a turn that ends with the stop reason `end_turn`.

- **ACP v1.** Petal keeps the editor's original prompt open and streams the follow-up output within the original turn.
- **ACP v2.** Petal schedules the follow-up prompt after the adapter reports idle.

## Find earlier work

The archive holds the original text of every message and tool result, including those of subagents. An agent uses it to answer a question that needs an exact quote or a source.

1. Call `search` with an identifier or a keyword from the original task. Call `recall` when you remember the idea but not the wording.
2. Pass the `reference` of a hit to `open` to read the original text.
3. Cite that reference.

`recall` needs an embeddings provider. [Enable semantic history search](configuration.md#enable-semantic-history-search)

## What a subagent can do

- A subagent starts with no session MCP servers, even when the parent has some. Tools configured directly in its adapter can still be available.
- Petal forwards supported permission requests from the subagent through the parent session.
- Petal does not support filesystem and terminal callbacks from the subagent to the editor.
- Cancelling the parent cancels its running subagents.

## Ownership and security

Petal sets the parent session and the working directory when it binds the MCP server. The tools accept neither a session ID nor a working directory. The relay authenticates to a private socket with a per-session credential. Treat adapters that run under your OS account as trusted processes.

A parent can read only its own direct subagents in the project. A different parent receives the same error as it would for an unknown subagent.

## Storage and recovery

Petal stores the parent-child links, the ordered activity, and the final results in the project's local session database. A parent can read them after it reconnects to the same stored session.

A subagent runs only while its Petal process lives. After an abrupt process exit, the next journal read records a failed result. Forgetting either the parent or the subagent removes the link and the journal.

To check delegation against live accounts, see [Test and verify Petal](testing.md#check-delegation-against-live-accounts).
