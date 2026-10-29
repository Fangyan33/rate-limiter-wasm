"""仅供隔离的 Kubernetes E2E 使用：可观测的上游及 Counter Service 故障代理。"""

import http.client
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


STATE = {"acquire": "normal", "renew": "normal", "release": "normal", "events": []}
LOCK = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    def body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            chunks = []
            while True:
                size = int(self.rfile.readline().strip().split(b";", 1)[0], 16)
                if not size:
                    while self.rfile.readline().strip():
                        pass
                    return b"".join(chunks)
                chunks.append(self.rfile.read(size))
                self.rfile.read(2)
        return self.rfile.read(int(self.headers.get("Content-Length", 0)))

    def reply(self, status, data, content_type="application/json"):
        if not isinstance(data, bytes):
            data = json.dumps(data).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/control":
            with LOCK:
                self.reply(200, dict(STATE))
        elif self.path == "/health":
            self.reply(200, {"ok": True})
        elif self.path == "/wasm/invalid.wasm":
            self.reply(200, b"not a wasm module", "application/octet-stream")
        elif self.path == "/wasm/slow.wasm":
            time.sleep(2)
            try:
                self.reply(200, b"not a wasm module", "application/octet-stream")
            except (BrokenPipeError, ConnectionResetError):
                pass
        elif self.path == "/wasm/fault.wasm":
            with open("/app/fault.wasm", "rb") as module:
                self.reply(200, module.read(), "application/octet-stream")
        else:
            self.reply(404, {"error": "not found"})

    def do_PUT(self):
        payload = json.loads(self.body())
        if self.path != "/control":
            return self.reply(404, {})
        with LOCK:
            for name in ("acquire", "renew", "release"):
                if name in payload:
                    STATE[name] = payload[name]
            if payload.get("clear"):
                STATE["events"] = []
        self.reply(200, {"ok": True})

    def do_POST(self):
        try:
            self.post()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def post(self):
        raw = self.body()
        payload = json.loads(raw) if raw else {}
        endpoint = self.path.strip("/")
        if endpoint in ("acquire", "renew", "release"):
            with LOCK:
                mode = STATE.get(endpoint, "normal")
            if mode in ("500", "503"):
                status, data = int(mode), b'{"error":"injected"}'
            elif mode == "invalid":
                status, data = 200, b"not-json"
            elif mode in ("empty", "null", "missing_allowed", "unknown_denial", "internal_error", "invalid_config", "api_key_disabled", "config_not_found", "oversize"):
                status = 200
                data = {"empty": b"", "null": b"null", "missing_allowed": b"{}",
                        "unknown_denial": b'{"allowed":false,"reason":"unknown"}',
                        "internal_error": b'{"allowed":false,"reason":"internal_error"}',
                        "invalid_config": b'{"allowed":false,"reason":"invalid_config"}',
                        "api_key_disabled": b'{"allowed":false,"reason":"api_key_disabled"}',
                        "config_not_found": b'{"allowed":false,"reason":"config_not_found"}',
                        "oversize": b"x" * (64*1024+1)}[mode]
            elif mode == "missing":
                status, data = 200, b'{"renewed":false,"reason":"lease_not_found"}'
            elif mode == "timeout":
                time.sleep(2)
                status, data = 503, b'{"error":"injected timeout"}'
            else:
                conn = http.client.HTTPConnection(os.environ["COUNTER_ADDR"], timeout=10)
                try:
                    conn.request("POST", self.path, raw, {"Content-Type": "application/json"})
                    response = conn.getresponse()
                    status, data = response.status, response.read()
                finally:
                    conn.close()
            with LOCK:
                STATE["events"].append({"path": self.path, "time": time.time(),
                                        "request": payload, "status": status,
                                        "body": data.decode()})
            return self.reply(status, data)

        received = {"body": payload, "headers": dict(self.headers)}
        usage = {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
        if not payload.get("stream"):
            result = {"id": "e2e-upstream", "received": received, "usage": usage}
            if payload.get("large_response"):
                result["padding"] = "x" * (2*1024*1024)
            return self.reply(200, result)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        deadline = time.monotonic() + float(payload.get("hold_seconds", 0))
        self.wfile.write(("data: " + json.dumps({"received": received}) + "\n\n").encode())
        self.wfile.flush()
        while time.monotonic() < deadline:
            self.wfile.write(b'data: {"choices":[{"delta":{"content":"tick"}}]}\n\n')
            self.wfile.flush()
            time.sleep(0.2)
        if payload.get("invalid_frame"):
            self.wfile.write(b"data: {not json}\n\n")
        self.wfile.write(("data: " + json.dumps({"usage": usage}) + "\n\ndata: [DONE]\n\n").encode())
        self.wfile.flush()


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
