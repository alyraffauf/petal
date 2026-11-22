# Test and verify Petal

## Run the checks

```sh
CGO_ENABLED=0 go test ./...
FUZZ_TIME=10s scripts/check-properties
ACP_TCK_DIR=/path/to/acp-tck scripts/check-acp /tmp/petal-acp.json matrix
ACP_TCK_DIR=/path/to/acp-tck scripts/check-acp /tmp/petal-v2.json v2-full
```

The regular Go suite runs the fuzz seeds and the deterministic transcript histories. The offline terminal tests use separate PTYs and a private Unix socket. They exercise ACP v1 and v2 reattachment, pending permissions, child completion while detached, and reopening a saved transcript. CI runs them on Linux and macOS. For `go test -race ./...`, use Go with a C compiler. The v1 matrix and the v2 profiles also need `uv` and a checkout of the pinned kit. [Pinned ACP test kit](https://github.com/agentclientprotocol/acp-tck/tree/7c71c4d1af0994e73bdc100c9f7f6b98f2319cda)

## Format the code

Run `nix fmt` at the repository root. It formats Go with gofmt, Nix with alejandra, and Markdown, JSON, and YAML with Prettier. `nix flake check` fails if a file is not formatted.

The website formats itself. In `website/`, run `npm run format`.

## Fuzz the Go targets

On pull requests and on pushes to `main`, CI generates new inputs for every Go fuzz target for three seconds each. Go mutates the seed inputs and retains the inputs that reach new code paths. Set `FUZZ_TIME` for a longer local run.

If fuzzing finds a failure in CI, follow these steps.

1. Download the `fuzz-inputs` CI artifact.
2. Extract the artifact at the repository root.
3. Run the replay command that the failed step printed.

## ACP conformance results

The pinned v2 kit reported **CONFORMANT** for the baseline and the full fixture profiles. The full profile passed 252 checks and skipped 63. Those results cover Petal as a proxy with the reference adapter.

The v1 matrix runs five adapter fixtures and writes a report for each fixture. The matrix checks that every registered requirement passed in at least one fixture. All 134 requirement IDs passed, with zero failures. Some optional checks pass when the adapter emits no corresponding traffic.

The upstream kit tests the agent role. Evidence for the client role comes from Petal's wire, PTY, and property tests. [Upstream coverage limits](https://agentclientprotocol.com/libraries/testing)

A separate live check exercised a tool call and a full replay with bundled Claude ACP 0.100.0 over v2, and with Codex ACP 2.1.1 over v1.

## Live comparisons

| Compared on November 19, 2026                              | Observed                                                                                                                                                                                                                                                  |
| ---------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Codex CLI 0.170.0 and the Petal TUI                        | Both completed the same GPT-6.2-Sol prompt. Petal resumed the thread and recalled its answer. Codex ACP reset an explicitly selected Read-only preset on load. Petal now asks before it continues with changed settings and can restore the saved choice. |
| Zed 1.28.1 with Codex ACP 2.1.1 directly and through Petal | Both paths completed a file-read turn, replayed it after a Zed restart, and cancelled a live turn. All 48 session updates that crossed the proxy matched after session ID translation. Reasoning reverted to Low on both paths.                           |

The Zed comparison used one authenticated account. Fresh sign-in, destructive operations, and multimodal turns were outside that run.

## Benchmark long sessions

```sh
CGO_ENABLED=0 go test ./internal/tui -run '^$' -bench 'Benchmark(WarmBacklog|OpenBacklog|OpenLargeToolResult|SessionFrame)' -benchmem
CGO_ENABLED=0 go test ./internal/acphost -run '^$' -bench BenchmarkTerminalOutputAtLimit -benchmem
```

These figures come from the test machine. They exclude provider replay and terminal painting.

| Case                                              | Result                                                                                                    |
| ------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| Open a 20-row viewport on 4,000 Markdown messages | Fell from about 143 ms to 0.27 ms.                                                                        |
| Draw a warm 100×30 TUI frame with 4,000 messages  | 0.60 to 0.72 ms.                                                                                          |
| First layout of a 16,384-line plain-text fence    | Fell from 114 ms and 208 MB to 76 to 93 ms and about 37 MB. Petal now tokenizes the log as a single span. |

Resizing while scrolled back can also reflow earlier messages.

## Check delegation against live accounts

The check asks a parent on one provider to delegate to a child on the other provider, then asks the parent to retrieve the child's original output from the archive. Run one direction at a time from a checkout on Linux. The argument selects the parent.

```sh
scripts/check-receipt claude
scripts/check-receipt codex
```

Each run uses both local subscriptions and can consume quota.

Install Go, Python 3.11 or later, and the stock Claude and Codex ACP adapters. If the adapter executables are outside `PATH`, set `CLAUDE_ACP` and `CODEX_ACP` to their absolute paths.

The runner copies `~/.claude/.credentials.json` and `~/.codex/auth.json` into disposable account homes. It builds the current checkout, limits the live phase to four minutes, and removes the temporary directory on exit.

### What the check does

Only the child receives the authenticated receipt tool. The child's final answer should acknowledge completion without quoting the receipt. The journal can persist the receipt.

1. The runner asks the parent to delegate.
2. The runner compacts the parent and restarts Petal.
3. The runner asks the parent for the child's original receipt and its archive reference.
4. The runner checks the answer against the fixture. It also correlates the archived child text with the actual search and read responses.
5. A separate prompt probes a known reference in an unrelated session that the runner seeded in the same project database.

### What fails the check

The final JSON reports individual observations. Each of these outcomes fails the check.

- The final result of the child exposes the receipt.
- The parent receives the receipt before it retrieves the receipt from the archive.
- The compaction did not complete.
- The receipt is visible in the captured parent context or in the replay.
- The parent's answer lacks the verbatim receipt or does not cite its archive reference.
- Retrieval evidence is missing.
- The text of the unrelated session leaks, or its denial is inconclusive.

A runtime error, such as a provider rate limit, also stops the run.

With a Codex parent, the check uses the native compacted record of the parent account as evidence. The check can observe local adapter records and protocol traffic. It cannot inspect provider-side model state.

## Record the demo

`scripts/record-demo` records the GIF and the screenshots that the README, the terminal guide, and the website use. It needs Go, tmux, uv, and Nix. It uses no provider accounts and no quota.

```sh
scripts/record-demo
```

The script runs the Petal binary from your checkout inside tmux. A scripted agent, `scripts/demo/agent.py`, plays both Claude Code and Codex. Delegation, the follow-up prompt, the control center states, and the search results all come from Petal.

Three things are staged.

- The account names and model names come from a generated config.
- The recorder writes the title of the delegated child to the session database, because Petal leaves a child untitled.
- Embeddings are off, so the search in the recording is a keyword search.

To change what the agents say, edit `scripts/demo/agent.py`. To change the keys and the timing, edit the scenes in `scripts/demo/record.py`. The recorder creates `/tmp/petal` as the project directory, because the path shows in the footer. Set `DEMO_PROJECT` to use another directory.

## Evaluate semantic search

With Ollama running, reproduce the fixture.

```sh
PETAL_EVAL_EMBEDDINGS=1 go test -count=1 -v ./internal/sessions -run TestLocalHybridEvaluation
```

These results are for the all-minilm model ID `1b226e2802db`.

- Both modes ranked both exact identifiers first.
- Hybrid ranked both paraphrases first. FTS found neither paraphrase.
- Both modes returned zero hits for two unrelated queries and for an identifier that differed by one digit.
- In a mixed query, a literal example of the search terms ranked first. The older paraphrased answer appeared second after coverage grew from 8/11 to 11/11 on the second call.

Keyword-first ordering can favor literal distractors. This small fixture exercises scope, reference reads, invalidation, and offline fallback. Its cosine cutoff of 0.4 has not been evaluated on a representative production corpus.

## Change the session schema

Session queries live in `internal/sessions/queries.sql`.

1. Add the schema change as a versioned Goose migration in `internal/sessions/migrations/`. Keep version 2 reserved for databases that the earlier importer created.
2. At the repository root, run `nix shell --inputs-from . nixpkgs#sqlc -c sqlc generate`.
3. Commit the generated Go files.
