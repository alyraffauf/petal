import io
import json
import os
import pathlib
import sqlite3
import subprocess
import sys
import tempfile
import unittest

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


def call(tool, ident, args, result, **overrides):
    return dict(
        {
            "toolCallId": ident,
            "title": "mcp.petal." + tool,
            "status": "completed",
            "rawInput": args,
            "rawOutput": [{"type": "text", "text": json.dumps(result)}],
        },
        **overrides,
    )


class EvidenceTests(unittest.TestCase):
    def test_only_an_ordered_scoped_search_and_original_read_count(self):
        receipt = "orchid abcdef"
        row = {"reference": 12, "session": "child", "role": "tool"}
        search = call(
            "search",
            "s",
            {"query": "orchid"},
            {"hits": [row | {"snippet": receipt}]},
        )
        read = call(
            "open", "r", {"reference": 12}, row | {"text": receipt}
        )

        def grade(events):
            return check_retrieval(events, receipt, 12, "child", receipt)

        self.assertEqual(grade([search, read]), (True, True, False))
        self.assertEqual(grade([search]), (True, False, False))
        for wrong in [
            dict(read, status="failed"),
            dict(read, rawInput={"reference": 13}),
            dict(read, title="other.open"),
        ]:
            self.assertFalse(grade([search, wrong])[1])
        self.assertFalse(grade([read, search])[1])
        journal = [
            {"toolCallId": "j", "content": {"text": part}} for part in ["abc", "def"]
        ]
        self.assertEqual(grade([search, *journal, read]), (True, True, True))

    def test_empty_lifecycle_rows_and_invalid_journal(self):
        events = journal_updates(
            [(None,), (b"",), (b'{"text":"abc"}',), (b'{"text":"def"}',)]
        )
        self.assertTrue(contains(events, "abcdef"))
        with self.assertRaises(ValueError):
            journal_updates([(b"broken",)])

    def test_only_new_native_compaction_in_the_parent_session_counts(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp, "session.jsonl")
            compact = b'{"type":"compacted"}\n'
            path.write_bytes(
                b'{"type":"session_meta","payload":{"id":"parent"}}\n' + compact
            )
            offsets = {path: path.stat().st_size}
            self.assertFalse(native_compacted(offsets, "parent"))
            with path.open("ab") as stream:
                stream.write(compact)
            self.assertFalse(native_compacted(offsets, "other"))
            self.assertTrue(native_compacted(offsets, "parent"))

    def test_missing_decoy_or_substituted_tool_row_invalidates_evidence(self):
        db = sqlite3.connect(":memory:")
        self.addCleanup(db.close)
        db.execute("create table messages(session_id,role,body,source_id)")
        rows = [
            (1, ("child", "tool", "receipt", "echo-id")),
            (2, ("other", "assistant", "decoy", None)),
        ]
        db.executemany("insert into messages values(?,?,?,?)", [row for _, row in rows])
        verify_archive_rows(db, rows)
        for mutation in [
            "delete from messages where rowid=2",
            "update messages set source_id='later' where rowid=1",
        ]:
            db.execute("savepoint original")
            db.execute(mutation)
            with self.assertRaises(RuntimeError):
                verify_archive_rows(db, rows)
            db.execute("rollback to original")

    def test_only_original_echo_and_scoped_reader_denial_count(self):
        echo = {
            "toolCallId": "issued",
            "name": "mcp__petal_echo__echo",
            "rawInput": {"text": "label"},
            "rawOutput": "label abc",
            "status": "completed",
        }
        self.assertEqual(
            echo_sources(
                [echo, dict(echo, toolCallId="copy", name="shell")], "label abc"
            ),
            {"issued"},
        )
        denial = call(
            "open",
            "r",
            {"reference": 17},
            {},
            status="failed",
            rawOutput="no saved message at this reference",
        )
        self.assertTrue(check_denial([denial], 17, "secret"))
        self.assertFalse(
            check_denial(
                [dict(denial, title="other.open")], 17, "secret"
            )
        )
        self.assertFalse(check_denial([denial], 18, "secret"))

    def test_permission_between_requests_and_stale_idle(self):
        # The adapter waits for the permission response before emitting the barrier.
        script = """
import sys, json
def read(): return json.loads(input())
def write(v): print(json.dumps(v), flush=True)
def event(state):
    write({'method':'session/update','params':{'sessionId':'parent','update':{'sessionUpdate':'state_update','state':state,'stopReason':'end_turn'}}})
read()
write({'id':1,'result':{}})
write({'id':9,'method':'session/request_permission','params':{'subject':{'toolCall':{'name':'mcp__petal_echo__echo'}},'options':[{'kind':'allow_once','optionId':'yes'}]}})
assert read()['result']['outcome']['optionId']=='yes'
write({'method':'barrier'})
read()
event('running')
event('idle')
write({'method':'session/update','params':{'sessionId':'parent','update':{'sessionUpdate':'user_message','messageId':'current'}}})
event('running')
event('idle')
write({'method':'session/update','params':{'sessionId':'parent','update':{'sessionUpdate':'user_message','messageId':'wake'}}})
event('running')
event('idle')
write({'id':2,'result':{'messageId':'current'}})
"""
        with subprocess.Popen(
            [sys.executable, "-c", script],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            text=True,
        ) as proc:
            client = Client(proc, io.StringIO())
            try:
                client.request(1, "session/new", {})
                with client.changed:
                    self.assertTrue(
                        client.changed.wait_for(
                            lambda: any(
                                q.get("method") == "barrier" for q in client.events
                            ),
                            3,
                        )
                    )
                _, events = client.request(
                    2, "session/prompt", {"sessionId": "parent"}, wait_idle=True
                )
                self.assertEqual(events[0]["messageId"], "current")
                self.assertEqual(
                    [e["state"] for e in events if "state" in e], ["running", "idle"]
                )
            finally:
                proc.kill()
                proc.wait()
                client.reader.join()

    def test_deadline_removes_credentials_and_detached_descendant(self):
        scripts = pathlib.Path(__file__).resolve().parent
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp)
            # Exercise the runner's actual EXIT trap and supervisor. Substitute
            # a trial that copies a dummy credential and hangs in a new session.
            root = path / "trial"
            for name, body in {
                "mktemp": f'mkdir "{root}" && echo "{root}"',
                "go": "exit 0",
                "python3": f"""exec {sys.executable} -c 'import sys; sys.path.insert(0,"{scripts}"); from supervise import run; run([sys.executable,"{path}/hang.py"],1)'""",
            }.items():
                exe = path / name
                exe.write_text("#!/bin/sh\n" + body)
                exe.chmod(0o755)
            (path / "hang.py").write_text(f"""
import os, pathlib, subprocess, sys
pathlib.Path(os.environ["TRIAL_ROOT"],"auth.json").write_text("dummy")
p = subprocess.Popen([sys.executable,"-c","import time; time.sleep(60)"],start_new_session=True)
pathlib.Path({str(path / "pid")!r}).write_text(str(p.pid))
p.wait()
""")
            env = os.environ | {
                "PYTHONDONTWRITEBYTECODE": "1",
                "PATH": tmp + ":" + os.environ["PATH"],
                "CLAUDE_ACP": sys.executable,
                "CODEX_ACP": sys.executable,
            }
            result = subprocess.run(
                [scripts.parent / "check-receipt", "claude"],
                env=env,
                capture_output=True,
                check=False,
                timeout=5,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(root.exists())
            self.assertFalse(pathlib.Path("/proc", (path / "pid").read_text()).exists())
