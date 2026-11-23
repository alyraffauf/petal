#!/usr/bin/env python3
"""Offline stand-in for the claude and codex CLIs in the demo recording.

Petal asks each CLI which model its default resolves to. This answers from
fixtures, so the recording shows concrete model names without an account.
"""

import json
import os
import sys

CLAUDE_MODEL, CODEX_MODEL = "claude-opus-5-5", "gpt-6.2-sol"


def reply_to(request):
    if request.get("type") == "control_request":
        # claude: the settings a new session would apply.
        return {"type": "control_response", "response": {
            "request_id": request["request_id"], "subtype": "success",
            "response": {"applied": {"model": CLAUDE_MODEL}},
        }}
    if "id" in request:
        # codex app-server: only the configured model matters here.
        result = {"config": {"model": CODEX_MODEL}} if request.get("method") == "config/read" else {}
        return {"id": request["id"], "result": result}
    return None


def main():
    if os.path.basename(sys.argv[0]) == "codex" and sys.argv[1:3] == ["debug", "models"]:
        print(json.dumps({"models": []}))
        return
    for line in sys.stdin:
        reply = reply_to(json.loads(line))
        if reply is not None:
            print(json.dumps(reply), flush=True)


main()
