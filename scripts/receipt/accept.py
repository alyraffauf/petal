import atexit
import hashlib
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

from client import Client
from evidence import (
    check_denial,
    check_retrieval,
    contains,
    echo_sources,
    journal_updates,
    native_compacted,
    verify_archive_rows,
)
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
for provider, adapter in adapters.items():
    path = pathlib.Path(adapter).resolve()
    version = subprocess.check_output(
        [path, "--version"], text=True, timeout=10
    ).strip()
    print(
        provider,
        path,
        version,
        hashlib.sha256(path.read_bytes()).hexdigest(),
        flush=True,
    )


@atexit.register
def report_models():
    for role, provider in (("parent", parent), ("child", child)):
        models = set()
        for path in (base / (role + "-" + provider)).rglob("*.jsonl"):
            for line in path.read_bytes().splitlines():
                try:
                    record = json.loads(line)
                except ValueError:
                    continue
                if record.get("type") == "turn_context":
                    model = record.get("payload", {}).get("model")
                elif record.get("type") == "assistant":
                    model = record.get("message", {}).get("model")
                else:
                    continue
                if model:
                    models.add(model)
        print(role, "resolved models", sorted(models), flush=True)


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
) -> tuple[str, pathlib.Path, str, tuple[int, str, str] | None, bool, bool]:
    receipts = root / "receipts"
    for _ in range(90):
        database = next(base.glob("state/**/index.sqlite"), None)
        if receipts.exists() and database:
            with sqlite3.connect(database) as db:
                for receipt in receipts.read_text().splitlines():
                    if not receipt.startswith(label + " "):
                        continue
                    bound = db.execute(
                        """
                        SELECT b.child_id, b.result_data FROM child_bindings b
                        JOIN sessions s ON s.id=b.child_id
                        JOIN messages task ON task.session_id=b.child_id
                        WHERE b.result_status='completed' AND b.parent_id=? AND s.account='child' AND s.model=? AND task.role='user'
                        AND instr(task.body,?)>0 LIMIT 1
                        """,
                        (sid, child + "/echo", label),
                    ).fetchone()
                    if not bound:
                        continue
                    session, terminal = bound
                    activities = journal_updates(
                        db.execute(
                            "SELECT data FROM child_activities WHERE child_id=? ORDER BY sequence",
                            (session,),
                        )
                    )
                    keys = echo_sources(activities, receipt)
                    source = next(
                        (
                            row
                            for row in db.execute(
                                "SELECT rowid, body, source_id FROM messages WHERE session_id=? AND role='tool' AND instr(body,?)>0 ORDER BY rowid",
                                (session, receipt),
                            )
                            if row[2] in keys
                        ),
                        None,
                    )
                    suffix = receipt.removeprefix(label).strip()
                    journal_exposed = contains(activities, suffix)
                    result = json.loads(terminal or "{}")
                    final_exposed = contains([result], suffix)
                    print(
                        json.dumps(
                            {
                                "receipt_in_archive": bool(source),
                                "receipt_in_activity": journal_exposed,
                                "receipt_in_terminal_result": final_exposed,
                            }
                        ),
                        flush=True,
                    )
                    return (
                        receipt,
                        database,
                        session,
                        source,
                        journal_exposed,
                        final_exposed,
                    )
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
        client = Client(p, log)
        call = client.request
        try:
            init, _ = call(
                1,
                "initialize",
                {
                    "protocolVersion": 2,
                    "capabilities": {},
                    "info": {"name": "delegation-live-test", "version": "1"},
                },
            )
            v = init["protocolVersion"]
            params = {"cwd": str(work), "mcpServers": []}
            if sid:
                params["sessionId"] = sid
                if v == 2:
                    params["replayFrom"] = {"type": "start"}
            opened_session, _ = call(
                2,
                "session/" + ("new" if not sid else "resume" if v == 2 else "load"),
                params,
            )
            if sid and opened_session.get("sessionId", sid) != sid:
                raise RuntimeError("reopened a different parent")
            sid = opened_session.get("sessionId", sid)
            print(
                "adapter",
                init.get("agentInfo") or init.get("info"),
                "models",
                [
                    o.get("currentValue")
                    for o in opened_session.get("configOptions", [])
                    if o.get("category") == "model"
                ],
                flush=True,
            )
            print(phase, "ACP", v, flush=True)

            def turn(i, prompt, call=call, sid=sid, v=v):
                _, updates = call(
                    i,
                    "session/prompt",
                    {"sessionId": sid, "prompt": [{"type": "text", "text": prompt}]},
                    wait_idle=v == 2,
                )

                if any(u.get("stopReason") == "rate_limit" for u in updates):
                    raise RuntimeError("provider rate limit")
                return updates

            if phase == "new":
                label = "orchid-" + secrets.token_hex(4)
                turn(
                    3,
                    f"Delegate to child model {child}/echo. Have the child obtain a receipt for {label} from its receipt service. Ask it to give only a completion acknowledgment in its final answer. You yourself should just acknowledge completion without quoting or retrieving the receipt, even on wake.",
                )

                receipt, database, session, archived, journal_exposed, final_exposed = (
                    wait_for_child(base, label)
                )
                if not archived:
                    raise RuntimeError("missing original child tool row")
                source, body, tool_id = archived
                native_id = json.loads(sid)[2]
                native_logs = list(
                    (base / "parent-codex/sessions").rglob(f"*-{native_id}.jsonl")
                )
                offsets = {f: f.stat().st_size for f in native_logs}
                for i in (4, 5):
                    updates = turn(i, "/compact")
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
                suffix = receipt.split()[-1]
                prior_context = contains(client.events, suffix)
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
                    compacted = native_compacted(offsets, native_id)
                else:
                    compacted = any(
                        u.get("sessionUpdate") == "compaction_update"
                        and u.get("status") == "completed"
                        for u in updates
                    )
            else:
                log.flush()
                updates = turn(
                    3,
                    "What was the verbatim receipt the child received, and what is the original source reference?",
                )
                replay = contains(client.events[: client.turn_start], suffix)
                answer = words(updates)
                found, opened, prior_tool = check_retrieval(
                    updates, receipt, source, session, body
                )
                leaked = contains(updates, decoy)
                updates = turn(
                    4,
                    f"Check whether archive reference {decoy_ref} is accessible. Report whether access succeeded.",
                )
                with sqlite3.connect(database) as db:
                    if not db.execute(
                        "SELECT 1 FROM child_bindings WHERE child_id=? AND parent_id=? AND result_status='completed'",
                        (session, sid),
                    ).fetchone():
                        raise RuntimeError("child binding changed across reopen")
                    verify_archive_rows(
                        db,
                        [
                            (source, (session, "tool", body, tool_id)),
                            (
                                decoy_ref,
                                (unrelated, "assistant", label + " " + decoy, None),
                            ),
                        ],
                    )
                leaked |= contains(client.events, decoy)
                denied = check_denial(updates, decoy_ref, decoy)
                report = {
                    "journal_contains_receipt": journal_exposed,
                    "decoy_exposed": leaked or contains(updates, decoy),
                    "parent_context_contains_receipt": prior_context,
                    "receipt_outside_archive_before_original_read": prior_tool,
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
                    or final_exposed
                    or prior_context
                    or prior_tool
                    or replay
                    or leaked
                    or contains(updates, decoy)
                ):
                    sys.exit(1)
        finally:
            p.stdin.close()
            try:
                p.wait(timeout=8)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()
            client.reader.join()

    if phase == "new":
        prior_context |= contains(client.events, suffix)
        for path in (base / ("parent-" + parent)).rglob("*.jsonl"):
            prior_context |= contains(
                [json.loads(line) for line in path.read_bytes().splitlines() if line],
                suffix,
            )
