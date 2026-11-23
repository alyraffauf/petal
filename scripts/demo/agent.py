#!/usr/bin/env python3
"""Scripted ACP v1 agent for the demo recording.

Petal starts one copy per account. DEMO_ROLE selects the part to play. The
parent role calls Petal's real MCP tools, so delegation, the follow-up prompt,
and transcript search in the recording are Petal's own behaviour.
"""

import json
import os
import subprocess
import sys
import threading
import time

ROLE = os.environ["DEMO_ROLE"]
CHILD_MODEL = os.environ.get("DEMO_CHILD_MODEL", "")
CHILD_TASK = (
    "Review the retry change in internal/routing/retry.go. "
    "Run go test ./internal/routing and report any failure with its cause."
)
# record.py sends this in a second project, so the control center has a thread to show from it.
SEED_PROMPT = "Draft the launch post"
TEST_OUTPUT = """--- FAIL: TestBackoffCaps (0.00s)
    retry_test.go:41: attempt 6: got delay 0s, want 30s
FAIL
FAIL\tpetal/internal/routing\t0.412s
FAIL"""

stdout_lock = threading.Lock()


def send(message):
    message["jsonrpc"] = "2.0"
    with stdout_lock:
        sys.stdout.write(json.dumps(message) + "\n")
        sys.stdout.flush()


def update(session_id, body):
    send({"method": "session/update", "params": {"sessionId": session_id, "update": body}})


def say(session_id, text):
    update(session_id, {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": text}})


def show_tool(session_id, tool_id, title, kind, output=None):
    body = {"sessionUpdate": "tool_call", "toolCallId": tool_id, "title": title, "status": "completed", "kind": kind}
    if output is not None:
        body["content"] = [{"type": "content", "content": {"type": "text", "text": output}}]
        body["rawOutput"] = output
    update(session_id, body)


class PetalTools:
    """Client for the stdio MCP server that Petal passes in session/new."""

    def __init__(self, server):
        environment = dict(os.environ)
        for variable in server.get("env") or []:
            environment[variable["name"]] = variable["value"]
        self.process = subprocess.Popen(
            [server["command"], *server.get("args", [])],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            env=environment,
            text=True,
        )
        self.request_id = 0
        self.request("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "demo", "version": "1"}})
        self.write({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def write(self, message):
        self.process.stdin.write(json.dumps(message) + "\n")
        self.process.stdin.flush()

    def request(self, method, params):
        self.request_id += 1
        self.write({"jsonrpc": "2.0", "id": self.request_id, "method": method, "params": params})
        while True:
            line = self.process.stdout.readline()
            if not line:
                raise RuntimeError("Petal's MCP server closed the connection")
            reply = json.loads(line)
            if reply.get("id") == self.request_id:
                return reply["result"]

    def call(self, name, arguments):
        result = self.request("tools/call", {"name": name, "arguments": arguments})
        return json.loads(result["content"][0]["text"])


petal_servers = {}
petal_tools = {}
child_ids = {}
titled_sessions = set()


def tools_for(session_id):
    if session_id not in petal_tools:
        petal_tools[session_id] = PetalTools(petal_servers[session_id])
    return petal_tools[session_id]


def delegate_review(session_id):
    time.sleep(1.2)
    show_tool(session_id, "read", "Read internal/routing/retry.go", "read")
    time.sleep(1.0)
    tools = tools_for(session_id)
    tools.call("models", {})
    child_ids[session_id] = tools.call("delegate", {"model": CHILD_MODEL, "task": CHILD_TASK})["childId"]
    show_tool(session_id, "delegate", "delegate · Codex", "other")
    time.sleep(0.8)
    say(session_id, "I asked **Codex** to review the retry change and run the routing tests. Petal will bring its result back here.")


def report_child_result(session_id):
    time.sleep(0.8)
    tools_for(session_id).call("activity", {"child_id": child_ids[session_id], "after": 0})
    show_tool(session_id, "activity", "activity · Codex result", "other")
    time.sleep(0.8)
    say(
        session_id,
        "Codex found one bug. The backoff delay resets to zero after the fifth attempt, so `TestBackoffCaps` fails.\n\n"
        "I capped the delay at 30 seconds in `retry.go`. The routing tests pass now.",
    )


def quote_original_output(session_id):
    time.sleep(0.9)
    tools = tools_for(session_id)
    hits = tools.call("search", {"query": "TestBackoffCaps"})["hits"]
    show_tool(session_id, "search", "search · TestBackoffCaps", "search")
    reference = next(hit["reference"] for hit in hits if hit["role"] == "tool")
    archived = tools.call("open", {"reference": reference})["text"]
    show_tool(session_id, "open", f"open · #{reference}", "read")
    # The archive stores the tool title above its output. Quote the output only.
    output = archived.split("\n\n", 1)[-1].strip()
    time.sleep(0.8)
    say(session_id, f"Here is the original output from Codex's run, reference **#{reference}**.\n\n```\n{output}\n```")


def answer_as_parent(session_id, prompt, request_id):
    if prompt == SEED_PROMPT:
        say(session_id, "The launch post draft is in `posts/launch.md`.")
    elif "second opinion" in prompt:
        delegate_review(session_id)
    elif "exactly" in prompt:
        quote_original_output(session_id)
    else:
        # Petal's follow-up prompt, sent when the child finishes.
        report_child_result(session_id)
    send({"id": request_id, "result": {"stopReason": "end_turn"}})


def answer_as_child(session_id, _prompt, request_id):
    time.sleep(1.5)
    show_tool(session_id, "read", "Read internal/routing/retry.go", "read")
    time.sleep(2.0)
    show_tool(session_id, "test", "go test ./internal/routing", "execute", TEST_OUTPUT)
    time.sleep(2.5)
    say(session_id, "TestBackoffCaps fails. After the fifth attempt the delay resets to 0s. The cap in retry.go compares against the attempt count, not the delay.")
    send({"id": request_id, "result": {"stopReason": "end_turn"}})


def main():
    session_count = 0
    for line in sys.stdin:
        request = json.loads(line)
        method, request_id, params = request.get("method"), request.get("id"), request.get("params") or {}
        if method == "initialize":
            send({"id": request_id, "result": {"protocolVersion": 1, "agentCapabilities": {}}})
        elif method == "session/new":
            session_count += 1
            session_id = f"{ROLE}-{os.getpid()}-{session_count}"
            for server in params.get("mcpServers") or []:
                if server.get("name") == "petal":
                    petal_servers[session_id] = server
            send({"id": request_id, "result": {"sessionId": session_id}})
        elif method == "session/prompt":
            session_id = params["sessionId"]
            prompt = " ".join(block.get("text", "") for block in params.get("prompt", []))
            if ROLE == "parent" and session_id not in titled_sessions:
                titled_sessions.add(session_id)
                title = SEED_PROMPT if prompt == SEED_PROMPT else "Review the retry change"
                update(session_id, {"sessionUpdate": "session_info_update", "title": title})
            answer = answer_as_parent if ROLE == "parent" else answer_as_child
            threading.Thread(target=answer, args=(session_id, prompt, request_id), daemon=True).start()
        elif request_id is not None and method:
            send({"id": request_id, "result": {}})


main()
