import json


def fragments(value):
    """Extract content in delivery order, including serialized MCP journal pages."""
    if isinstance(value, list):
        for part in value:
            yield from fragments(part)
    elif isinstance(value, dict):
        for key, part in value.items():
            if key in {
                "text",
                "delta",
                "output",
                "answer",
                "snippet",
                "rawOutput",
                "content",
            } and isinstance(part, str):
                yield part
                if part.startswith(("{", "[")):
                    try:
                        yield from fragments(json.loads(part))
                    except ValueError:
                        pass
            else:
                yield from fragments(part)


def contains(events, secret):
    if secret in json.dumps(events) or secret in "".join(fragments(events)):
        return True
    streams = {}
    for packet in events:
        if not isinstance(packet, dict):
            continue
        update = packet.get("params", {}).get("update", packet)
        key = (
            update.get("toolCallId")
            or update.get("messageId")
            or update.get("sessionUpdate")
        )
        for field in ("content", "rawOutput"):
            value = update.get(field)
            pieces = [value] if isinstance(value, str) else fragments(value)
            streams.setdefault((key, field), []).extend(pieces)
    return any(secret in "".join(parts) for parts in streams.values())


def tool_events(updates):
    calls = {}
    for update in updates:
        key = update.get("toolCallId")
        call = calls.setdefault(key, {}) if key else {}
        first = not call
        call.update(update)
        tool = None
        raw = call.get("rawInput") or {}
        for candidate in ("search", "recall", "open"):
            names = {
                f"mcp.petal.{candidate}",
                f"mcp__petal__{candidate}",
            }
            if (
                key
                and (call.get("name") or call.get("title")) in names
                and raw.get("server", "petal") == "petal"
                and raw.get("tool", candidate) == candidate
            ):
                tool = candidate
        output = call.get("rawOutput")
        failed = call.get("status") == "failed"
        if isinstance(output, dict):
            failed |= bool(output.get("error"))
            output = output.get("result", output)
            failed |= bool(output.get("isError"))
            output = output.get("content")
        text = (
            "".join(part.get("text", "") for part in output if isinstance(part, dict))
            if isinstance(output, list)
            else output
            if isinstance(output, str)
            else ""
        )
        yield (
            update,
            first,
            tool,
            raw.get("arguments", raw),
            call.get("status"),
            failed,
            text,
        )


def check_retrieval(updates, receipt, source, session, body):
    found = opened = exposed = False
    eligible_reads = set()
    earlier = []
    for update, first, tool, args, status, failed, text in tool_events(updates):
        if found and first:
            eligible_reads.add(update.get("toolCallId"))
        try:
            data = json.loads(text)
        except ValueError:
            data = None
        hit = False
        if status == "completed" and not failed and isinstance(data, dict):
            if (
                tool in ("search", "recall")
                and isinstance(args.get("query"), str)
                and args["query"].strip()
            ):
                hit = any(
                    h.get("reference") == source
                    and h.get("session") == session
                    and h.get("role") == "tool"
                    for h in data.get("hits", [])
                )
                found |= hit
            if (
                tool == "open"
                and update.get("toolCallId") in eligible_reads
            ):
                opened |= (
                    args.get("reference") == source
                    and data.get("reference") == source
                    and data.get("session") == session
                    and data.get("role") == "tool"
                    and data.get("text") == body
                )
        if not opened and not hit:
            earlier.append(update)
        elif hit and not opened:
            earlier.append({"rawInput": args})
    exposed = contains(earlier, receipt.split()[-1])
    return found, opened, exposed


def check_denial(updates, decoy_ref, decoy):
    return not contains(updates, decoy) and any(
        tool == "open"
        and args.get("reference") == decoy_ref
        and (failed or status == "failed")
        and text.strip() == "no saved message at this reference"
        for _, _, tool, args, status, failed, text in tool_events(updates)
    )


def journal_updates(rows):
    return [json.loads(data) for (data,) in rows if data]


def echo_sources(updates, receipt):
    calls = {}
    for u in updates:
        key = u.get("toolCallId")
        if key:
            calls.setdefault(key, {}).update(u)
    sources = set()
    for key, call in calls.items():
        tool = call.get("name") or call.get("title")
        raw = call.get("rawInput") or {}
        args = raw.get("arguments", raw)
        if (
            tool in {"mcp.petal_echo.echo", "mcp__petal_echo__echo"}
            and args.get("text") == receipt.rsplit(" ", 1)[0]
            and call.get("status") == "completed"
            and contains([call.get("rawOutput"), call.get("content")], receipt)
        ):
            sources.add(key)
    return sources


def native_compacted(offsets, session_id):
    for path, offset in offsets.items():
        with path.open("rb") as stream:
            header = json.loads(stream.readline())
            if (
                header.get("type") != "session_meta"
                or header.get("payload", {}).get("id") != session_id
            ):
                continue
            stream.seek(offset)
            if any(json.loads(line).get("type") == "compacted" for line in stream):
                return True
    return False


def verify_archive_rows(db, rows):
    for reference, expected in rows:
        actual = db.execute(
            "SELECT session_id, role, body, source_id FROM messages WHERE rowid=?",
            (reference,),
        ).fetchone()
        if actual != expected:
            raise RuntimeError("archive row changed across reopen")
