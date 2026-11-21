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
            name.startswith("mcp__petal-delegation__petal_")
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
        response = None
        running = idle = False
        with self.changed:
            while response is None or (
                wait_idle
                and (
                    not idle
                    or not message_id
                    or message_id != response.get("messageId")
                )
            ):
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
                        running = idle = False
                        updates.clear()
                        self.turn_start = cursor - 1
                    updates.append(update)
                    if update.get("sessionUpdate") == "state_update":
                        if update.get("state") == "running":
                            running, idle = True, False
                        elif (
                            running
                            and update.get("state") == "idle"
                            and update.get("stopReason")
                        ):
                            idle = True
                elif "method" not in packet and packet.get("id") == ident:
                    if "error" in packet:
                        raise RuntimeError("ACP request rejected")
                    response = packet["result"]
        return response, updates
