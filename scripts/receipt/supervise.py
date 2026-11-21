"""Bound a Linux trial and reap adapters that start their own process groups."""

import ctypes
import os
import signal
import subprocess
import sys
from pathlib import Path


def children():
    # Some kernels omit /proc/thread-self/children. /proc/PID/stat provides
    # the same parent PID even for a child that created a new process group.
    parent = os.getpid()
    for entry in Path("/proc").iterdir():
        if not entry.name.isdecimal():
            continue
        try:
            stat = (entry / "stat").read_bytes()
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
        if int(stat.rsplit(b") ", 1)[1].split()[1]) == parent:
            yield int(entry.name)


def run(command, deadline):
    # PR_SET_CHILD_SUBREAPER makes orphaned grandchildren our direct children.
    if ctypes.CDLL(None).prctl(36, 1, 0, 0, 0) != 0:
        raise OSError("could not supervise descendant processes")
    process = subprocess.Popen(command)
    try:
        return process.wait(timeout=deadline)
    finally:
        while True:
            try:
                pid, _ = os.waitpid(-1, os.WNOHANG)
            except ChildProcessError:
                break
            if pid:
                continue
            for pid in children():
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass


if __name__ == "__main__":
    sys.exit(run(sys.argv[1:], 240))
