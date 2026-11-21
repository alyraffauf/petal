"""Bound a Linux trial and reap adapters that start their own process groups."""

import ctypes
import os
import signal
import subprocess
import sys
from pathlib import Path


def run(command, deadline):
    # PR_SET_CHILD_SUBREAPER makes orphaned grandchildren our direct children.
    if ctypes.CDLL(None).prctl(36, 1, 0, 0, 0) != 0:
        raise OSError("could not supervise descendant processes")
    process = subprocess.Popen(command)
    try:
        return process.wait(timeout=deadline)
    finally:
        children = Path("/proc/thread-self/children")
        while pids := children.read_text().split():
            for pid in pids:
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            os.waitpid(-1, 0)


if __name__ == "__main__":
    sys.exit(run(sys.argv[1:], 240))
