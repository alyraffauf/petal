# Connect an editor to Petal

## Start a Petal thread in Zed

1. Install Petal and sign in to the accounts you want to use. [Configure subscriptions](configuration.md#route-across-subscriptions)
2. In Zed's **Agent Settings**, open **External Agents** and choose **Add Agent**, then **Add Custom Agent**.
3. Add the following entry to `agent_servers` in your Zed settings. Keep any other agents you use.

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

In the Agent Panel, start a new thread and choose **Petal**. Send a prompt in the thread.

If Zed cannot launch Petal, run `command -v petal` in your terminal. Set `command` to the absolute path it prints. [Zed external agents](https://zed.dev/docs/ai/external-agents#custom-agents)

Zed 1.28.1 tries ACP v2 when Zed enables its `acp-beta` feature flag. Release builds restrict local flag overrides to staff. Petal's `acp_v2` setting enables v2 on the Petal side. [Zed's connection logic](https://github.com/zed-industries/zed/blob/v1.28.1/crates/agent_servers/src/acp.rs) · [Zed's flag rules](https://github.com/zed-industries/zed/blob/v1.28.1/crates/feature_flags/src/store.rs#L99-L107) · [Enable v2 in Petal](acp.md#opt-in-to-acp-v2)

## Choose a model or account

Set `args` to `["acp", "codex/default"]` to use the default Codex model and balance new sessions across your Codex accounts. Use `["acp", "claude/default"]` for Claude Code. To pin a configured account, use `["acp", "codex/default", "--account", "work"]`.

Without a model argument, Petal picks the first route ending in `/default` in alphabetical order. If no route ends in `/default`, it picks the first route alphabetically.

## Connect another ACP client

Choose the client's custom stdio agent option. Set the executable to `petal` and the argument list to `["acp"]`. To share a specific configuration, append `--config` and the absolute path to your TOML file as separate arguments.
