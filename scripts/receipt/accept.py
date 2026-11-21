import json
import os
import pathlib
import re
import secrets
import shutil
import sqlite3
import subprocess
import sys
import time

from evidence import check_retrieval, check_denial
from httpfixture import server, token

root = pathlib.Path(os.environ["TRIAL_ROOT"])
parent, child = sys.argv[1:]
base = root / (parent + "-to-" + child)
base.mkdir(mode=0o700)
work = base / "work"
work.mkdir()
home = base / "home"
home.mkdir()
env = {
    k: os.environ[k]
    for k in ("PATH", "LANG", "SSL_CERT_FILE", "NIX_SSL_CERT_FILE", "TMPDIR")
    if k in os.environ
}
env.update(
    HOME=str(home),
    XDG_STATE_HOME=str(base / "state"),
    XDG_CONFIG_HOME=str(base / "config"),
    XDG_CACHE_HOME=str(base / "cache"),
    XDG_DATA_HOME=str(base / "data"),
)
adapters = {k: os.environ[k.upper() + "_ACP"] for k in ("claude", "codex")}
cfg = base / "petal.toml"
lines = ["acp_v2 = true", "delegation = true"]
for role, provider in [("parent", parent), ("child", child)]:
    account = base / (role + "-" + provider)
    account.mkdir(mode=0o700)
    if provider == "codex":
        shutil.copyfile(pathlib.Path.home() / ".codex/auth.json", account / "auth.json")
        (account / "auth.json").chmod(0o600)
        if role == "child":
            (account / "config.toml").write_text(
                "[mcp_servers.petal_echo]\nurl = "
                + json.dumps(server["url"])
                + "\nhttp_headers = { Authorization = "
                + json.dumps("Bearer " + token)
                + " }\n"
            )
    else:
        shutil.copyfile(
            pathlib.Path.home() / ".claude/.credentials.json",
            account / ".credentials.json",
        )
        (account / ".credentials.json").chmod(0o600)
        contents = {"hasCompletedOnboarding": True}
        if role == "child":
            contents["mcpServers"] = {
                "petal_echo": {k: server[k] for k in ("type", "url")}
                | {"headers": {"Authorization": "Bearer " + token}}
            }
        (account / ".claude.json").write_text(json.dumps(contents))
    lines.extend(
        [
            "[[accounts]]",
            f'name = "{role}"',
            f'provider = "{provider}"',
            f'home = "{account}"',
            "[[accounts.models]]",
            f'id = "{provider}/{"default" if role == "parent" else "echo"}"',
            "command = " + json.dumps([adapters[provider]]),
        ]
    )
cfg.write_text("\n".join(lines))
sid = None


def wait_for_child(
    base: pathlib.Path, label: str
) -> tuple[str, pathlib.Path, tuple[int, str, str]]:
    receipts = root / "receipts"
    for _ in range(90):
        database = next(base.glob("state/**/index.sqlite"), None)
        if receipts.exists() and database:
            with sqlite3.connect(database) as db:
                for receipt in receipts.read_text().splitlines():
                    if not receipt.startswith(label + " "):
                        continue
                    source = db.execute(
                        """
                        SELECT m.rowid, m.session_id, m.body
                        FROM messages m JOIN child_bindings b ON b.child_id=m.session_id
                        WHERE m.role='assistant' AND instr(m.body,?)>0
                        AND b.result_status IS NOT NULL ORDER BY m.rowid LIMIT 1
                    """,
                        (receipt,),
                    ).fetchone()
                    if source:
                        return receipt, database, source
        time.sleep(1)
    raise RuntimeError("child did not archive an issued receipt before deadline")


def words(updates):
    def t(body):
        if isinstance(body, dict):
            return body.get("text", "")
        if isinstance(body, list):
            return "".join(map(t, body))
        return ""

    return "".join(
        t(u.get("content"))
        for u in updates
        if u["sessionUpdate"].startswith("agent_message")
    )


for phase in ("new", "reopen"):
    with (
        (base / (phase + ".stderr")).open("w") as err,
        (base / (phase + ".wire")).open("w") as log,
    ):
        p = subprocess.Popen(
            [str(root / "petal"), "acp", parent + "/default", "--config", str(cfg)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=err,
            text=True,
            env=env,
            start_new_session=False,
            cwd=work,
        )
        updates = []

        def send(q):
            q = dict(jsonrpc="2.0", **q)
            print(json.dumps(["send", q]), file=log, flush=True)
            p.stdin.write(json.dumps(q) + "\n")
            p.stdin.flush()

        def call(i, m, params):
            send({"id": i, "method": m, "params": params})

        def receive(i, stop=False):
            response = None
            done = False
            for line in p.stdout:
                q = json.loads(line)
                print(json.dumps(["recv", q]), file=log, flush=True)
                if "id" in q and "method" in q:
                    a = q.get("params", {})
                    if q["method"] == "session/request_permission":
                        subject = a.get("subject", a)
                        name = subject.get("toolCall", {}).get("name", "")
                        allow = (
                            name.startswith("mcp__petal-delegation__petal_")
                            or name == "mcp__petal_echo__echo"
                        )
                        opt = next(
                            (
                                o
                                for o in a.get("options", [])
                                if o["kind"]
                                == ("allow_once" if allow else "reject_once")
                            ),
                            None,
                        )
                        result = {
                            "outcome": {
                                "outcome": "selected",
                                "optionId": opt["optionId"],
                            }
                            if opt
                            else {"outcome": "cancelled"}
                        }
                        send({"id": q["id"], "result": result})
                    else:
                        send(
                            {
                                "id": q["id"],
                                "error": {
                                    "code": -32601,
                                    "message": "unsupported editor request",
                                },
                            }
                        )
                elif q.get("method") == "session/update":
                    u = q["params"]["update"]
                    updates.append(u)
                    if u.get("state") == "idle" and u.get("stopReason"):
                        done = True
                elif q.get("id") == i:
                    if "error" in q:
                        raise RuntimeError("ACP request rejected")
                    response = q["result"]
                if response is not None and (not stop or done):
                    return response
            raise RuntimeError("ACP stream ended before turn completion")

        try:
            call(
                1,
                "initialize",
                {
                    "protocolVersion": 2,
                    "capabilities": {},
                    "info": {"name": "delegation-live-test", "version": "1"},
                },
            )
            v = receive(1)["protocolVersion"]
            params = {"cwd": str(work), "mcpServers": []}
            if sid:
                params["sessionId"] = sid
                if v == 2:
                    params["replayFrom"] = {"type": "start"}
            call(
                2,
                "session/" + ("new" if not sid else "resume" if v == 2 else "load"),
                params,
            )
            sid = receive(2).get("sessionId", sid)
            print(phase, "ACP", v, flush=True)

            def turn(i, prompt):
                updates.clear()
                call(
                    i,
                    "session/prompt",
                    {"sessionId": sid, "prompt": [{"type": "text", "text": prompt}]},
                )
                receive(i, stop=v == 2)

                if any(u.get("stopReason") == "rate_limit" for u in updates):
                    raise RuntimeError("provider rate limit")

            if phase == "new":
                label = "orchid-" + secrets.token_hex(4)
                turn(
                    3,
                    f"Delegate to child model {child}/echo. Tell the child to call its echo MCP tool with text {label} and save the exact entire returned receipt in its final message. You yourself should just acknowledge completion without quoting or retrieving the receipt, even on wake.",
                )

                receipt, database, (source, session, body) = wait_for_child(base, label)
                for i in (4, 5):
                    turn(i, "/compact")
                    if any(
                        u.get("sessionUpdate") == "compaction_update"
                        and u.get("status") == "completed"
                        for u in updates
                    ):
                        break
                    if parent == "codex":
                        break
                    print(
                        "compaction events:",
                        {
                            u.get("sessionUpdate")
                            for u in updates
                            if "compact" in str(u.get("sessionUpdate"))
                        },
                        flush=True,
                    )
                parent_logs = (base / ("parent-" + parent)).rglob("*.jsonl")
                prior_context = receipt in (base / "new.wire").read_text() or any(
                    receipt in f.read_text() for f in parent_logs
                )
                with sqlite3.connect(database) as db:
                    unrelated = "unrelated-" + secrets.token_hex(8)
                    db.execute(
                        "insert into sessions select ?,model,account,'',title,opened,updated,selections from sessions where id=?",
                        (unrelated, session),
                    )
                    decoy = secrets.token_hex(16)
                    decoy_ref = db.execute(
                        "insert into messages(session_id,role,body) values(?,'assistant',?)",
                        (unrelated, label + " " + decoy),
                    ).lastrowid
                if parent == "codex":
                    compacted = any(
                        json.loads(line).get("type") == "compacted"
                        for f in (base / "parent-codex/sessions").rglob("*.jsonl")
                        for line in f.open()
                    )
                else:
                    compacted = any(
                        u.get("sessionUpdate") == "compaction_update"
                        and u.get("status") == "completed"
                        for u in updates
                    )
            else:
                log.flush()
                replay = receipt in (base / "reopen.wire").read_text()
                turn(
                    3,
                    "What exact receipt did the child get? Quote the original child message and give me its archive reference.",
                )
                answer = words(updates)
                found, opened, prior_tool = check_retrieval(
                    updates, receipt, source, session, body
                )
                leaked = decoy in json.dumps(updates)
                turn(
                    4,
                    f"Check whether archive reference {decoy_ref} can be opened with the transcript reader. Report whether access succeeded.",
                )
                denied = check_denial(updates, decoy_ref, decoy)
                report = {
                    "decoy_exposed": leaked or decoy in json.dumps(updates),
                    "parent_context_contains_receipt": prior_context,
                    "receipt_before_archive_search": prior_tool,
                    "compacted": compacted,
                    "replay_contains_receipt": replay,
                    "search_hit": found,
                    "original_read": opened,
                    "source_reference": source,
                    "exact": receipt in answer,
                    "quoted_reference": bool(
                        re.search(
                            r"(?i)\b(?:ref(?:erence)?|message)\b[\s:*#`=*-]*"
                            + str(source)
                            + r"\b",
                            answer,
                        )
                    ),
                    "decoy_denied": denied,
                }
                print(json.dumps(report), flush=True)
                if (
                    not all(
                        report[k]
                        for k in (
                            "compacted",
                            "search_hit",
                            "original_read",
                            "exact",
                            "quoted_reference",
                            "decoy_denied",
                        )
                    )
                    or prior_context
                    or prior_tool
                    or replay
                    or leaked
                    or decoy in json.dumps(updates)
                ):
                    sys.exit(1)
        finally:
            p.stdin.close()
            try:
                p.wait(timeout=8)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()
