#!/usr/bin/env python3
"""模拟上游：只为验证网关的请求构造，不产生真实图片、不消耗额度。

记录每个到达的请求（方法、路径、content-type、解析后的字段），
并按请求里的 size 生成一张对应分辨率的纯色 PNG 回包。

    python3 scripts/mock-upstream.py --port 8799 --log /tmp/mock-upstream.json
"""
from __future__ import annotations

import argparse
import base64
import io
import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

LOCK = threading.Lock()
RECORDS: list[dict] = []
LOG_PATH = Path("/tmp/mock-upstream.json")


def make_png(size: str, color=(90, 160, 200)) -> bytes:
    from PIL import Image

    m = re.fullmatch(r"(\d+)x(\d+)", size or "")
    w, h = (int(m.group(1)), int(m.group(2))) if m else (1024, 1024)
    buf = io.BytesIO()
    Image.new("RGB", (w, h), color).save(buf, format="PNG")
    return buf.getvalue()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # 静音
        pass

    def _record(self, path: str, body: bytes):
        ctype = self.headers.get("Content-Type", "")
        fields: dict = {}
        files: list = []
        if "multipart/form-data" in ctype:
            boundary = ctype.split("boundary=")[-1].strip('"')
            for part in body.split(b"--" + boundary.encode()):
                if b"\r\n\r\n" not in part:
                    continue
                head, _, payload = part.partition(b"\r\n\r\n")
                payload = payload.rstrip(b"\r\n-")
                m = re.search(rb'name="([^"]+)"(?:; filename="([^"]*)")?', head)
                if not m:
                    continue
                name = m.group(1).decode()
                if m.group(2) is not None:
                    files.append({"field": name, "filename": m.group(2).decode(),
                                  "bytes": len(payload)})
                else:
                    fields[name] = payload.decode("utf-8", "replace")
        else:
            try:
                fields = json.loads(body or b"{}")
            except Exception:
                fields = {"_raw": body[:200].decode("utf-8", "replace")}

        rec = {
            "path": path,
            "auth": (self.headers.get("Authorization") or "")[:20] + "…",
            "content_type": ctype.split(";")[0],
            "fields": fields,
            "files": files,
        }
        with LOCK:
            RECORDS.append(rec)
            LOG_PATH.write_text(json.dumps(RECORDS, ensure_ascii=False, indent=2))
        return fields

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n)
        fields = self._record(self.path, body)

        size = str(fields.get("size") or "1024x1024")
        png = make_png(size)
        n_req = fields.get("n") or 1
        try:
            count = max(1, min(4, int(n_req)))
        except Exception:
            count = 1

        refs = self._ref_count(body)
        resp = {
            "created": 0,
            "data": [{"b64_json": base64.b64encode(png).decode()} for _ in range(count)],
            "size": size,
            "quality": fields.get("quality"),
            "output_format": "png",
            "usage": {
                "input_tokens": 20,
                "output_tokens": 196 * count,
                # 有垫图就报非 0，触发不了网关的 image_refs_dropped 哨兵
                "input_tokens_details": {"image_tokens": 1024 * refs if refs else 0},
            },
        }
        out = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    @staticmethod
    def _ref_count(body: bytes) -> int:
        return body.count(b'name="image')

    def do_GET(self):
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()


def main():
    global LOG_PATH
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8799)
    ap.add_argument("--log", default="/tmp/mock-upstream.json")
    args = ap.parse_args()
    LOG_PATH = Path(args.log)
    LOG_PATH.write_text("[]")
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"mock upstream on http://127.0.0.1:{args.port}  log={LOG_PATH}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
