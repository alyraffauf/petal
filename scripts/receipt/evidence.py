import json


def check_retrieval(updates, receipt, source, session, body):
    calls = {}
    hits = set()
    opened = False
    prior_tool = False
    for u in updates:
        key = u.get("toolCallId")
        if (
            source not in hits
            and receipt in json.dumps(u)
            and (
                not key
                or not calls.get(key, {})
                .get("title", "")
                .endswith("petal_search_history")
            )
        ):
            prior_tool = True
        if not key:
            continue
        event = calls.setdefault(key, {})
        event.update(u)
        raw = u.get("rawOutput")
        if isinstance(raw, dict):
            raw = (raw.get("result") or {}).get("content")
        if not isinstance(raw, list):
            continue
        tool = event.get("title", "").split(".")[-1].split("__")[-1]
        for part in raw:
            try:
                data = json.loads(part.get("text", ""))
            except ValueError:
                continue
            if not isinstance(data, dict):
                continue
            if tool == "petal_search_history":
                hits.update(
                    x["reference"]
                    for x in data.get("hits", [])
                    if x.get("session") == session
                )
            if tool == "petal_read_transcript" and source in hits:
                opened |= (
                    data.get("reference") == source
                    and data.get("session") == session
                    and data.get("text") == body
                )
    return source in hits, opened, prior_tool


def check_denial(updates, decoy_ref, decoy):
    denied = False
    readers = set()
    args = {}
    for u in updates:
        key = u.get("toolCallId")
        if u.get("title", "").endswith("petal_read_transcript"):
            readers.add(key)
        if "rawInput" in u:
            raw = u["rawInput"] or {}
            args[key] = raw.get("arguments", raw)
        if (
            key in readers
            and args.get(key, {}).get("reference") == decoy_ref
            and "rawOutput" in u
        ):
            # Match the actual scoped-reader error returned by Petal.
            denied |= "no saved message at this reference" in json.dumps(
                u["rawOutput"]
            ).lower() and decoy not in json.dumps(u)
    return denied
