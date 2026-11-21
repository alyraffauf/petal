import json
import os
import pathlib
import secrets
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

root = pathlib.Path(os.environ["TRIAL_ROOT"])
token = secrets.token_hex(16)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        self.send_error(405)

    def do_POST(self):
        if self.headers.get("Authorization") != "Bearer " + token:
            self.send_response(401)
            self.send_header("WWW-Authenticate", "Bearer")
            self.end_headers()
            return
        q = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        m = q["method"]
        p = q.get("params", {})
        if "id" not in q:
            self.send_response(202)
            self.end_headers()
            return
        if m == "initialize":
            r = {
                "protocolVersion": p["protocolVersion"],
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "echo", "version": "1"},
            }
        elif m == "tools/list":
            r = {
                "tools": [
                    {
                        "name": "echo",
                        "description": "Echo text and issue a receipt",
                        "inputSchema": {
                            "type": "object",
                            "properties": {"text": {"type": "string"}},
                            "required": ["text"],
                        },
                    }
                ]
            }
        elif m == "tools/call":
            value = p["arguments"]["text"] + " " + secrets.token_hex(8)
            with (root / "receipts").open("a") as f:
                print(value, file=f)
            r = {"content": [{"type": "text", "text": value}]}
        else:
            r = {}
        data = json.dumps({"jsonrpc": "2.0", "id": q["id"], "result": r}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


http = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=http.serve_forever, daemon=True).start()
server = {
    "name": "petal_echo",
    "type": "http",
    "url": f"http://127.0.0.1:{http.server_port}/mcp",
    "headers": [{"name": "Authorization", "value": "Bearer " + token}],
}
