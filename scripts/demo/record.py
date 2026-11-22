"""Drive Petal inside tmux and record its screen as ANSI text.

Usage: record.py PETAL_BINARY OUTPUT_JSON

The output holds the timed frames of the workbench session and three named
stills. render.py turns it into the GIF and the screenshots.
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

COLUMNS, ROWS = 96, 27
# The project path shows in the footer, so it is short and fixed.
PROJECT = Path(os.environ.get("DEMO_PROJECT", "/tmp/petal"))
AGENT = Path(__file__).with_name("agent.py")
TMUX = ["tmux", "-L", "petal-demo", "-f", "/dev/null"]


def write_config(root):
    """Two Claude accounts with different quota, and one Codex account."""
    accounts = [
        ("work", "claude/default", "parent", 30, 5),
        ("personal", "claude/default", "parent", 90, 1),
        ("team", "codex/default", "child", 80, 1),
    ]
    lines = ['[history.embeddings]', 'provider = "off"', ""]
    for name, model, role, quota_left, capacity in accounts:
        lines += [
            "[[accounts]]",
            f'name = "{name}"',
            f"capacity = {capacity}",
            f'usage = ["echo", "{quota_left}"]',
            "[[accounts.env]]",
            'name = "DEMO_ROLE"',
            f'value = "{role}"',
            "[[accounts.env]]",
            'name = "DEMO_CHILD_MODEL"',
            'value = "codex/default"',
            "[[accounts.models]]",
            f'id = "{model}"',
            f'command = ["python3", "{AGENT}"]',
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
    return bin_directory


class Session:
    """One Petal process in a detached tmux window."""

    def __init__(self, root, petal, config):
        self.state = root / f"state-{time.monotonic_ns()}"
        self.state.mkdir()
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
        command = ["new-session", "-d", "-s", "demo", "-x", str(COLUMNS), "-y", str(ROWS), "-c", str(PROJECT)]
        for name, value in environment.items():
            command += ["-e", f"{name}={value}"]
        # Petal only emits 24-bit color when it does not detect tmux.
        launch = f"env -u TMUX -u TMUX_PANE TERM=xterm-256color {petal} --config {config}"
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
        database = sqlite3.connect(glob.glob(f"{self.state}/petal/conversations/*/index.sqlite")[0], timeout=5)
        database.execute("update sessions set title = ? where title = ''", (title,))
        database.commit()
        database.close()

    def close(self):
        self.tmux("kill-server")


def start_review(session):
    session.wait_for("control center")
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
            session = Session(root, petal, config)
            try:
                scene(session)
            finally:
                session.close()
            stills.update(session.stills)
        # The workbench scene runs last, so its frames are the animation.
        output.write_text(json.dumps({"columns": COLUMNS, "rows": ROWS, "frames": session.frames, "stills": stills}))
    print(f"recorded {len(session.frames)} frames over {session.frames[-1][0]:.1f} seconds")


main()
