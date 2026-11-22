#!/usr/bin/env python3
"""Offline provider metadata for the recorded Petal session."""

import json
import sys

if sys.argv[1:3] == ["debug", "models"]:
    print(json.dumps({"models": []}))
else:
    for line in sys.stdin:
        request = json.loads(line)
        if request.get("type") == "control_request":
            reply = {"type": "control_response", "response": {
                "request_id": request["request_id"], "subtype": "success",
                "response": {"applied": {"model": "claude-opus-5-5"}},
            }}
        elif "id" in request:
            result = {"config": {"model": "gpt-6.2-sol"}} if request["method"] == "config/read" else {}
            reply = {"id": request["id"], "result": result}
        else:
            continue
        print(json.dumps(reply), flush=True)
