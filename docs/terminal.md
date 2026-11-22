# Use Petal in the terminal

## Start a conversation

Run `petal` in your project directory. Select **new session** to choose a model, or press **Ctrl+N** to use the default model. To pick the subscription yourself, press **Tab** in the model picker.

Send a prompt to start the turn. Press **Shift+Enter** for a new line. Press **Escape** to cancel a running turn.

If you send another message while the agent works, Petal steers the turn when the adapter supports steering. Otherwise, Petal queues the message as a follow-up. To always queue, see [Queue follow-ups](configuration.md#queue-follow-ups).

When the agent asks for permission, type the number of an option. **Escape** declines.

## Run a command

Type `/` to list the commands from Petal and from the agent. Petal shows only the commands that apply to the current conversation.

![Slash commands above the composer, with the selected command and its Run action](screenshots/commands.png)

| Command                            | Action                                                    |
| ---------------------------------- | --------------------------------------------------------- |
| `/new`, `/resume`                  | Start a thread, or resume a saved one.                    |
| `/new <directory>`                 | Start a fresh thread there using the current model. Relative paths start from the current thread’s directory. |
| `/model`, `/subscription`          | Change the model, or start a thread on a chosen account.  |
| `/effort`, `/mode`, `/permissions` | Change reasoning effort, agent mode, or the Codex preset. |
| `/config`                          | Change any other setting that the agent offers.           |
| `/fork`, `/parent`                 | Branch the conversation, or return to its parent.         |
| `/sessions`, `/agents`             | Open the control center, or focus a subagent in it.       |
| `/search`, `/history`              | Search project transcripts, or read this conversation.    |
| `/attach`, `/paste-image`          | Send a file, or an image from the clipboard.              |
| `/usage`                           | Show context usage and session cost.                      |
| `/delete`, `/logout`, `/quit`      | Delete the conversation, sign out, or exit.               |

If an agent command has the same name as a Petal command, type `/agent:` before its name.

## Find a conversation or message

Press **F2** or type `/sessions` to open the control center. Type to filter threads by title, model, or account. Press **Shift+Tab** to filter by status.

![The control center lists a completed delegated subagent under its parent thread, with the subagent's details on the right](screenshots/control-center.png)

The control center lists saved threads across projects. Select a thread to reopen it in its original directory. Older archives show up under **Earlier project** until Petal learns their directory from an adapter or you open Petal there.

To search the saved messages in this project, press **Ctrl+F** in the control center or type `/search` in chat. Use double quotes for an exact phrase. Press **Enter** to read a match in context. Press **Escape** to return to the results.

Press **Page Up** to scroll back, or press **F3** to open the full-screen reader. In the control center, open **conversation turns** to jump to a prompt in the active thread.

`petal --resume` starts with your most recent saved thread selected in the control center. While Petal runs, it keeps the unsent draft of each thread that you switch away from.

## Follow subagents

Press **F4** or type `/agents` to focus a subagent in the thread tree. Use the arrow keys to expand or collapse a branch. Press **Tab** to view the details of the subagent.

For a child that an agent delegated through Petal, you can do more.

- Press **Enter** to open the child as a session.
- Press **Ctrl+X** to cancel the child while it runs.
- Look for the unread marker on a child whose result the parent has not read.

In the control center, press **Ctrl+G** to open the agent graph. Select a connection and press **Enter** to follow it. Press **Escape** to return to the tree, or press **r** to reveal the focused agent in **threads**.

To branch a conversation, type `/fork`. The adapter must be able to reopen forks. Type `/parent` to return.

## Send an image

1. Paste the image into the composer.
2. Add a caption beside `[pasted image]`.
3. Press **Enter**.

Clipboard support depends on your terminal, including over SSH. To request an image from the terminal clipboard, press **Ctrl+V** or type `/paste-image`. Petal accepts one pasted image of up to 7 MiB per draft.

To send a file, type `/attach path/to/image.png`. A file over 7 MiB, or a type that the adapter cannot take inline, goes as a file reference.

In a direct Kitty or Ghostty session, Petal previews PNG and JPEG files and the first frame of a GIF. In tmux, in screen, and in other terminals, Petal shows the caption only. Transcript search indexes the caption.

## Sign in to an MCP server

When an adapter asks you to visit a URL, you can keep typing and sending prompts.

1. Press **F6** to review the link.
2. Visit the link.
3. On the review screen, press **Enter** if the request is still open. A matching completion from the adapter clears the request automatically.

On the review screen, press **d** to decline the request. Press **Escape** to return to your draft and keep the request pending.

Over SSH, a browser on your laptop needs a route to the loopback callback on the host that runs Petal. If the callback uses port 8765, for example, run `ssh -L 8765:127.0.0.1:8765 host` before you complete the sign-in.
