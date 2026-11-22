# Connect an editor to Petal

`petal acp` is an [Agent Client Protocol](https://agentclientprotocol.com/) server. An editor that connects to it gets the same orchestration, memory, and routing as the terminal UI.

## Start a Petal thread in Zed

1. Install Petal and sign in to the accounts that you want to use. [Configure subscriptions](configuration.md#route-across-subscriptions)
2. In Zed's **Agent Settings**, open **External Agents**. Choose **Add Agent**, then **Add Custom Agent**.
3. Add the following entry to `agent_servers` in your Zed settings. Keep any other agents that you use.

```json
{
    "agent_servers": {
        "Petal": {
            "type": "custom",
            "command": "petal",
            "args": ["acp"]
        }
    }
}
```

4. In the Agent Panel, start a new thread and choose **Petal**.
5. Send a prompt in the thread.

If Zed cannot launch Petal, run `command -v petal` in your terminal. Set `command` to the absolute path that the command prints. [Zed external agents](https://zed.dev/docs/ai/external-agents#custom-agents)

## Connect another ACP client

Choose the client's option for a custom stdio agent. Set the executable to `petal` and the argument list to `["acp"]`. To use a specific configuration, append `--config` and the absolute path of your TOML file as separate arguments.

## Choose a model or account

One editor entry offers the models from all discovered and configured accounts. Choose Claude or Codex when you start a thread. Petal routes the session to an account that offers that model. When you reopen the thread, Petal uses the same account.

To limit the entry to one route, change `args`.

| `args`                                          | Result                                                                                    |
| ----------------------------------------------- | ----------------------------------------------------------------------------------------- |
| `["acp", "codex/default"]`                      | Uses the default Codex model and balances new sessions across your Codex accounts.        |
| `["acp", "claude/default"]`                     | Uses the default Claude Code model and balances new sessions across your Claude accounts. |
| `["acp", "codex/default", "--account", "work"]` | Pins the entry to the configured account `work`.                                          |

If you send a prompt without choosing a model, Petal picks the first route that ends in `/default`, in alphabetical order. If no route ends in `/default`, Petal picks the first route alphabetically.

## Use ACP v2 in Zed

When one entry offers both Claude and Codex accounts, Petal uses ACP v1, which both stock adapters support. An entry limited to one provider or one model can negotiate ACP v2. [Opt in to ACP v2](acp.md#opt-in-to-acp-v2)

Zed 1.28.1 tries ACP v2 when Zed enables its `acp-beta` feature flag. Release builds restrict local flag overrides to staff. [Zed's connection logic](https://github.com/zed-industries/zed/blob/v1.28.1/crates/agent_servers/src/acp.rs) · [Zed's flag rules](https://github.com/zed-industries/zed/blob/v1.28.1/crates/feature_flags/src/store.rs#L99-L107)
