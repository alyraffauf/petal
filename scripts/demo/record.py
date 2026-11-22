"""Drive Petal inside tmux and record its screen as ANSI text.

Usage: record.py PETAL_BINARY OUTPUT_JSON

The output holds the timed frames of the workbench session and three named
stills. render.py turns it into the GIF and the screenshots.
"""

import hashlib
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path

COLUMNS, ROWS = 96, 27
# The project path shows in the footer, so it is short and fixed.
PROJECT = Path(os.environ.get("DEMO_PROJECT", "/tmp/petal"))
AGENT = Path(__file__).with_name("agent.py")
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
        lines += [
            "[[accounts]]",
            f'name = "{name}"',
            f'provider = "{provider}"',
            f"capacity = {capacity}",
            f'usage = ["echo", "{quota_left}"]',
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
    """A PATH without the real claude and codex, so Petal discovers only the demo accounts."""
    bin_directory = root / "bin"
    bin_directory.mkdir()
    (bin_directory / "petal").symlink_to(petal)
    for tool in ("python3", "git", "echo"):
        (bin_directory / tool).symlink_to(shutil.which(tool))
    for name in ("codex-acp", "claude-agent-acp"):
        (bin_directory / name).symlink_to(AGENT.resolve())
    for name in ("claude", "codex"):
        (bin_directory / name).symlink_to(Path(__file__).with_name("provider.py").resolve())
    return bin_directory


class Session:
    """One Petal process in a detached tmux window."""

    def __init__(self, root, petal, config, state, project):
        self.state = state
        self.project = project
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
        launch = f"env -u TMUX -u TMUX_PANE TERM=xterm-256color PATH={root}/bin {petal} --config {config}"
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
        project_key = hashlib.sha256(os.fsencode(self.project)).hexdigest()
        database = sqlite3.connect(self.state / "petal/conversations" / project_key / "index.sqlite", timeout=5)
        database.execute("update sessions set title = ? where title = ''", (title,))
        database.commit()
        database.close()

    def close(self):
        self.tmux("kill-server")


def start_review(session):
    session.wait_for("Draft release notes")
    session.wait_for("gpt-6.2-sol")
    session.wait_for("claude-opus-5-5")
    session.record(1.8)
    session.press("Enter", 1.0)
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
    PROJECT.mkdir(exist_ok=True)
    subprocess.run(["git", "init", "-q", "-b", "main"], cwd=PROJECT, check=True)
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        config = write_config(root)
        isolated_path(root, petal)
        stills = {}
        for scene in (record_commands, record_workbench):
            state = root / f"state-{scene.__name__}"
            state.mkdir()
            notes = PROJECT / "release-notes"
            notes.mkdir(exist_ok=True)
            seed = Session(root, petal, config, state, notes)
            try:
                seed.wait_for("claude-opus-5-5")
                seed.press("C-n")
                seed.wait_for("ready")
                seed.type("Draft release notes")
                seed.press("Enter")
                seed.wait_for("Release draft saved")
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
