import json
import threading


class Client:
    def __init__(self, proc, log):
        self.proc = proc
        self.log = log
        self.events = []
        self.changed = threading.Condition()
        self.closed = False
        self.reader = threading.Thread(target=self.pump, daemon=True)
        self.reader.start()

    def send(self, packet):
        with self.changed:
            packet = dict(jsonrpc="2.0", **packet)
            print(json.dumps(["send", packet]), file=self.log, flush=True)
            self.proc.stdin.write(json.dumps(packet) + "\n")
            self.proc.stdin.flush()
            return len(self.events)

    def pump(self):
        try:
            for line in self.proc.stdout:
                packet = json.loads(line)
                with self.changed:
                    print(json.dumps(["recv", packet]), file=self.log, flush=True)
                    self.events.append(packet)
                    if "method" in packet and "id" in packet:
                        self.send(self.reply(packet))
                    self.changed.notify_all()
        finally:
            with self.changed:
                self.closed = True
                self.changed.notify_all()

    @staticmethod
    def reply(packet):
        reply = {"id": packet["id"]}
        if packet["method"] != "session/request_permission":
            return reply | {
                "error": {"code": -32601, "message": "unsupported editor request"}
            }
        params = packet["params"]
        name = params.get("subject", params).get("toolCall", {}).get("name", "")
        allow = (
            name in {f"mcp__petal__{tool}" for tool in (
                "search", "recall", "open", "models", "delegate", "activity", "wait"
            )}
            or name == "mcp__petal_echo__echo"
        )
        kind = "allow_once" if allow else "reject_once"
        for option in params.get("options", []):
            if option["kind"] == kind:
                return reply | {
                    "result": {
                        "outcome": {
                            "outcome": "selected",
                            "optionId": option["optionId"],
                        }
                    }
                }
        return reply | {"result": {"outcome": {"outcome": "cancelled"}}}

    def request(self, ident, method, params, wait_idle=False):
        cursor = self.send({"id": ident, "method": method, "params": params})
        self.turn_start = cursor
        message_id = None
        updates = []
        completed = {}
        response = None
        running = False
        start = cursor
        with self.changed:
            while True:
                if response is not None:
                    if not wait_idle:
                        return response, updates
                    if response.get("messageId") in completed:
                        self.turn_start, turn_updates = completed[response["messageId"]]
                        return response, turn_updates
                self.changed.wait_for(
                    lambda cursor=cursor: cursor < len(self.events) or self.closed
                )
                if cursor == len(self.events):
                    raise RuntimeError("ACP stream closed")
                packet = self.events[cursor]
                cursor += 1
                if packet.get("method") == "session/update":
                    payload = packet["params"]
                    if payload["sessionId"] != params.get("sessionId"):
                        continue
                    update = payload["update"]
                    if wait_idle and update.get("sessionUpdate") == "user_message":
                        message_id = update.get("messageId")
                        running = False
                        updates = []
                        start = cursor - 1
                    updates.append(update)
                    if update.get("sessionUpdate") == "state_update":
                        if update.get("state") == "running":
                            running = True
                        elif (
                            message_id and running
                            and update.get("state") == "idle"
                            and update.get("stopReason")
                        ):
                            completed.setdefault(message_id, (start, updates.copy()))
                            running = False
                elif "method" not in packet and packet.get("id") == ident:
                    if "error" in packet:
                        raise RuntimeError("ACP request rejected")
                    response = packet["result"]
