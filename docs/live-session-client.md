# Local daemon session API

`internal/livesession` provides the daemon handler and Go client for Linux and macOS. The socket lifecycle and same-user check live in `internal/localdaemon`. The local protocol negotiates its own version before opening sessions. Each session then negotiates ACP with its adapter.

## Wire the daemon and connect

Create the server with `livesession.NewServer(ctx, resolve)`. The resolver loads routing on the daemon host for the requested model and account. Pass `server.Handler()` to `localdaemon.ServeStarted` and call `server.Close()` on shutdown to wait for adapter processes and archive writes.

On the client, pass the connection from `Endpoint.DialOrStart` to `livesession.Connect`. Use `List` to find live sessions. Attach to an existing session by handle or durable `SessionID`. Include `Cwd` with the durable ID when an adapter can reuse IDs across directories. To create or reopen one, call `Open` with a fresh `NewHandle()`, an absolute workspace directory, and any model or account selection. Set `ResumeID` to reopen a durable session. Attach with control, then submit `initialize` and `session/open`. The daemon selects the ACP setup method and supplies the workspace. `Snapshot.Hello`, `Setup`, and `SetupMethod` expose the adapter response and the selected method. When an ACP v1 adapter supports resume without replay, use the workspace archive for the initial transcript.

`Submit` accepts a call and returns its operation number. Start at `Snapshot.NextOperation`. `WaitResult` waits for completion independently of `Next`, and `Result` reads a retained receipt without waiting. Use `Receipt.Err()` for the adapter or daemon error. During setup a frontend must read events concurrently to answer callbacks.

## Replay and control

`Attach` returns a snapshot at `Head`. Call `Next(ctx, cursor)` starting at `Snapshot.After`. Advance the cursor after applying each event. Replay through `Head`, then apply the snapshot's current foreground and pending questions before continuing with live events. Render ACP updates during replay. Use the snapshot for foreground state, current settings, and the pending question set at `Head`. Historical permission events do not open prompts. After `Head`, apply question events as they arrive. ACP v1 `CallStarted` carries the submitted user prompt. ACP v2 supplies its own user messages and foreground updates.

An observer can replay and read results. `Attach` with control or `Claim` takes the exclusive controller lease. Closing the client connection releases the lease and leaves the adapter, delegated children, and pending questions running. Answer a question by ID with the selected option or elicitation result. A host filesystem write or terminal creation asks a `host/approve` question with `allow` and `deny` options. Use `TerminalOutput` to poll a live terminal by reference. The daemon publishes its last output as a `terminal/output` event when the adapter releases it.

`Cancel` targets the prompt operation shown in `Snapshot.Turn`. The daemon sends ACP cancellation after the prompt reaches the adapter and publishes `CancelFinished`. `CloseSession` ends the adapter lifetime. Idle sessions get a short ACP close window. The durable ID stays reserved until cleanup completes, so reopening may briefly return `Conflict`. If the adapter disconnects, the daemon publishes `AdapterExited` and clears the running state. Close that live handle before reopening the durable ID. Use the daemon as the transcript writer and use the workspace store for user edits to record metadata.

## Retry and retention

Retry the same open handle, operation number and payload, or question answer after a dropped reply. Compare `Client.Instance()` after reconnecting. Live events, operation receipts, and questions are scoped to one daemon instance. After a daemon restart, reopen using the durable session ID.

The daemon retains at most 4,096 events and 8 MiB of event JSON per session. If a reader falls behind, `Gap` reports the oldest available cursor. Use the durable archive as the transcript baseline and resume from a fresh snapshot's head. Transient events lost to retention cannot be reconstructed. The archive checkpoints at ACP v1 prompt completion and ACP v2 idle, with a final checkpoint on orderly daemon shutdown. Large attachments still go to the adapter. The journal can retain a shortened display representation of the prompt. Older operation receipts can expire. Expired operation numbers cannot run again.

For ACP v1 steering, a `call_finished` event carries the submitted ACP parameters when the adapter confirms injection. Render that input once when replaying the event. A steering reply that requires a follow-up prompt leaves the event parameters empty.
