#!/usr/bin/env python3
"""模拟慢 HTTP 服务：等待指定时间后返回 OK，仅使用 Python 标准库。"""

import argparse
import math
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def nonnegative_seconds(value):
    seconds = float(value)
    if not math.isfinite(seconds) or seconds < 0:
        raise argparse.ArgumentTypeError("等待时间必须是有限的非负数")
    return seconds


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="0.0.0.0", help="监听地址（默认：0.0.0.0）")
    parser.add_argument("--port", type=int, default=8080, help="监听端口（默认：8080）")
    parser.add_argument("--delay", type=nonnegative_seconds, default=10, help="响应前等待秒数（默认：10，支持小数）")
    args = parser.parse_args()
    if not 0 <= args.port <= 65535:
        parser.error("端口必须在 0 到 65535 之间")

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            time.sleep(args.delay)
            body = b"OK"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                # 测试客户端可能在等待期间主动超时断开。
                pass

        do_POST = do_GET

    with ThreadingHTTPServer((args.host, args.port), Handler) as server:
        print(f"监听 {args.host}:{server.server_port}，每次请求等待 {args.delay:g} 秒后返回 OK", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
