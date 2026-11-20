<img src="docs/petal.svg" width="96" height="96" alt="Petal’s pixel orchid">

# Petal

Petal is a terminal client for Claude Code and Codex. Pick a model and Petal starts the conversation on the subscription with the most estimated quota left. The thread stays on that account when you resume it. Zed can use the same router through the Agent Client Protocol (ACP).

## Start a conversation

Sign in with `claude` or `codex login`. From the Petal checkout, run:

```sh
nix profile install .#petal
cd /path/to/your/project
petal
```

Petal opens the project control center. Press **Ctrl+N** to start on the default model, or select **new session** to choose one. Returning to a session restores its unsent draft while Petal is running. To choose the account yourself, press **Tab** in the model picker. Petal shows completed replies in a continuous Markdown transcript. A message you send while the agent works steers the current turn when the adapter supports it. Otherwise it becomes a follow-up.

- Type `/` for Petal commands and commands supplied by the agent.
- Use **Page Up** to scroll, **F3** to open the full-screen reader, and **Escape** to cancel a running turn.
- Press **Ctrl+F** in the control center or type `/search` in chat to search this project’s saved messages. Use double quotes for an exact phrase. Press **Enter** to read a match in context and **Esc** to return to results.
- Press **F2** or type `/sessions` for the control center. Type to search by title, model or account. **Shift+Tab** filters by status. Open **conversation turns** to jump to a prompt in the active thread. Start with `petal --resume` to select the most recent saved thread.
- Use `/fork` to branch on adapters that can reopen forks, and `/parent` to return.
- Press **F4** or type `/agents` to focus a subagent in the same tree. Use **Left/Right** to fold branches and **Tab** for full-screen details. **Ctrl+G** opens the agent graph. Select a connection and press **Enter** to follow it. **Escape** restores the tree as you left it. Press **r** to reveal the focused agent in Threads.

The Nix package includes the ACP adapters. For a standalone build, use Go 1.27.3 or later, run `go build -o petal .`, and put `claude-agent-acp` or `codex-acp` on your path alongside its provider CLI.

[Configure subscriptions and Zed](docs/configuration.md) · [ACP support and verification](docs/acp.md) · [MIT license](LICENSE)
