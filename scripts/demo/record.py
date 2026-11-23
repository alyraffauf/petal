"""Drive Petal inside tmux and record its screen as ANSI text.

Usage: record.py PETAL_BINARY OUTPUT_JSON

The output holds the timed frames of the workbench session and three named
stills. render.py turns it into the GIF and the screenshots.

agent.py plays the ACP bridges and provider.py plays the claude and codex CLIs,
so Petal discovers its accounts and resolves their default models as usual.
"""

import glob
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path

# One row taller than the orchid needs beside six thread rows. render.py sizes cells to match.
COLUMNS, ROWS = 96, 28
# The project path shows in the footer, so it is short and fixed.
PROJECT = Path(os.environ.get("DEMO_PROJECT", "/tmp/petal"))
# A second project with one saved thread, which the control center lists under its path.
OTHER_PROJECT = PROJECT.with_name(PROJECT.name + "-website")
OTHER_PROMPT = "Draft the launch post"  # agent.py answers this prompt and titles the thread with it.
AGENT = Path(__file__).with_name("agent.py").resolve()
PROVIDER = Path(__file__).with_name("provider.py").resolve()
# The name provider.py reports for the Claude default. Its arrival means the labels have resolved.
CLAUDE_MODEL = "claude-opus-5-5"
TMUX = ["tmux", "-L", "petal-demo", "-f", "/dev/null"]


def write_config(root):
    """Two Claude accounts with different quota, and one Codex account."""
    accounts = [
        ("work", "claude", "parent", 30, 5),
        ("personal", "claude", "parent", 90, 1),
        ("team", "codex", "child", 80, 1),
    ]
    lines = ['[history.embeddings]', 'provider = "off"', ""]
    for name, provider, role, quota_left, capacity in accounts:
        executable_env = {"claude": "CLAUDE_CODE_EXECUTABLE", "codex": "CODEX_PATH"}[provider]
        lines += [
            "[[accounts]]",
            f'name = "{name}"',
            f'provider = "{provider}"',
            f"capacity = {capacity}",
            f'usage = ["echo", "{quota_left}"]',
            "[[accounts.env]]",
            f'name = "{executable_env}"',
            f'value = {json.dumps(str(root / "bin" / provider))}',
            "[[accounts.env]]",
            'name = "DEMO_ROLE"',
            f'value = "{role}"',
            "[[accounts.env]]",
            'name = "DEMO_CHILD_MODEL"',
            'value = "codex/default"',
            "",
        ]
    config = root / "config.toml"
    config.write_text("\n".join(lines))
    return config


def isolated_path(root, petal):
    """A PATH where the scripted agent and provider stand in for the real bridges and CLIs."""
    bin_directory = root / "bin"
    bin_directory.mkdir()
    (bin_directory / "petal").symlink_to(petal)
    for tool in ("python3", "git", "echo"):
        (bin_directory / tool).symlink_to(shutil.which(tool))
    for bridge in ("claude-agent-acp", "codex-acp"):
        (bin_directory / bridge).symlink_to(AGENT)
    for cli in ("claude", "codex"):
        (bin_directory / cli).symlink_to(PROVIDER)
    return bin_directory


class Session:
    """One Petal process in a detached tmux window."""

    def __init__(self, root, petal, config, state, project):
        self.state = state
        self.frames = []
        self.stills = {}
        self.started = time.monotonic()
        self.tmux("kill-server")
        environment = {
            "COLORTERM": "truecolor",
            "HOME": str(root),
            "XDG_STATE_HOME": str(self.state),
            "XDG_CONFIG_HOME": str(root / "config"),
            "PATH": str(root / "bin"),
        }
        command = ["new-session", "-d", "-s", "demo", "-x", str(COLUMNS), "-y", str(ROWS), "-c", str(project)]
        for name, value in environment.items():
            command += ["-e", f"{name}={value}"]
        # Petal only emits 24-bit color when it does not detect tmux.
        # The shell tmux starts may rebuild PATH, so set it again on the command itself.
        launch = f"env -u NO_COLOR -u TMUX -u TMUX_PANE TERM=xterm-256color PATH={root / 'bin'} {petal} --config {config}"
        started = self.tmux(*command, launch)
        if started.returncode != 0:
            raise RuntimeError(started.stderr)
        self.tmux("set-option", "-g", "status", "off")
        self.tmux("set-option", "-sg", "escape-time", "0")

    def tmux(self, *arguments):
        return subprocess.run(TMUX + list(arguments), capture_output=True, text=True)

    def screen(self, with_color=False):
        flags = ["-p", "-e"] if with_color else ["-p"]
        return self.tmux("capture-pane", "-t", "demo", *flags).stdout

    def record(self, seconds):
        """Sample the screen, keeping a frame each time it changes."""
        deadline = time.monotonic() + seconds
        while True:
            frame = self.screen(with_color=True)
            if not self.frames or self.frames[-1][1] != frame:
                self.frames.append((time.monotonic() - self.started, frame))
            if time.monotonic() >= deadline:
                return
            time.sleep(0.03)

    def wait_for(self, text, timeout_seconds=30):
        deadline = time.monotonic() + timeout_seconds
        while text not in self.screen():
            if time.monotonic() > deadline:
                raise TimeoutError(f"waited for {text!r}, screen was:\n{self.screen()}")
            self.record(0.08)

    def press(self, key, pause_seconds=0.0):
        self.tmux("send-keys", "-t", "demo", key)
        self.record(pause_seconds)

    def type(self, text):
        for character in text:
            self.tmux("send-keys", "-t", "demo", "-l", character)
            self.record(1 / 30)

    def keep_still(self, name):
        self.stills[name] = self.screen(with_color=True)

    def title_child(self, title):
        """Petal leaves a delegated child untitled. Title it so the thread tree reads well."""
        for index in glob.glob(f"{self.state}/petal/conversations/*/index.sqlite"):
            database = sqlite3.connect(index, timeout=5)
            database.execute("update sessions set title = ? where title = ''", (title,))
            database.commit()
            database.close()

    def close(self):
        self.tmux("kill-server")


def save_other_thread(session):
    """Hold one finished conversation in the second project."""
    session.wait_for("control center")
    session.press("C-n")
    session.wait_for("ready")
    session.type(OTHER_PROMPT)
    session.press("Enter")
    session.wait_for("launch.md")


def start_review(session):
    session.wait_for(OTHER_PROMPT)
    session.wait_for(CLAUDE_MODEL)
    # Open on the thread saved in the other project, once the orchid has faded in.
    for _ in range(4):
        session.press("Down")
    session.record(1.5)
    # Drop the frames so far, so the animation does not start on a blank screen.
    session.frames.clear()
    session.started = time.monotonic()
    session.record(2.2)
    for _ in range(4):
        session.press("Up", 0.14)
    session.record(0.3)
    session.press("Enter", 1.8)  # The models, by the names their defaults resolve to.
    session.press("Tab", 2.6)  # Show the accounts and their quota.
    session.press("Enter", 0.5)
    session.wait_for("ready")
    session.record(0.7)
    session.type("Review the retry change. Get a second opinion from Codex.")
    session.record(0.4)
    session.press("Enter")
    session.wait_for("delegate · Codex")
    session.title_child("Run the routing tests")


def record_workbench(session):
    start_review(session)
    session.record(1.3)
    session.press("F2", 1.0)
    session.press("Down", 0.6)
    session.keep_still("workbench")
    session.wait_for("completed")
    session.record(2.2)
    session.keep_still("control-center")
    session.press("Escape", 0.3)
    session.wait_for("tests pass now")
    session.record(3.2)
    session.type("What exactly failed?")
    session.record(0.4)
    session.press("Enter")
    session.wait_for("0.412s")
    session.record(0.5)


def record_commands(session):
    start_review(session)
    session.wait_for("tests pass now")
    session.record(1.0)
    session.type("/")
    session.record(1.5)
    session.keep_still("commands")


def main():
    petal, output = Path(sys.argv[1]).resolve(), Path(sys.argv[2])
    for project in (PROJECT, OTHER_PROJECT):
        project.mkdir(exist_ok=True)
        subprocess.run(["git", "init", "-q", "-b", "main"], cwd=project, check=True)
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        config = write_config(root)
        isolated_path(root, petal)
        stills = {}
        for scene in (record_commands, record_workbench):
            state = root / f"state-{scene.__name__}"
            state.mkdir()
            seed = Session(root, petal, config, state, OTHER_PROJECT)
            try:
                save_other_thread(seed)
            finally:
                seed.close()
            session = Session(root, petal, config, state, PROJECT)
            try:
                scene(session)
            finally:
                session.close()
            stills.update(session.stills)
        # The workbench scene runs last, so its frames are the animation.
        output.write_text(json.dumps({"columns": COLUMNS, "rows": ROWS, "frames": session.frames, "stills": stills}))
    print(f"recorded {len(session.frames)} frames over {session.frames[-1][0]:.1f} seconds")


main()
