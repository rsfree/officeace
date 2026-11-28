#!/usr/bin/env python3
"""从 Chrome 的 cookie 里取小浣熊 IAM token。

用 Chrome Safe Storage 主密钥解密 xiaohuanxiong.com 下的 JWT cookie，
挑 exp 最晚的一个输出。适合「网页版登录后直接取票」，省掉客户端。

    python3 scripts/token-from-browser.py              # 列出候选与有效期
    python3 scripts/token-from-browser.py --dump       # 打印完整 token
    python3 scripts/token-from-browser.py --push       # 提取并推到线上网关

前置：在 Chrome 里打开 https://xiaohuanxiong.com/login 完成登录。
注意：Chrome 关闭时才保证把 cookie 刷盘；刚登录可等几秒或先退出 Chrome。
"""
from __future__ import annotations

import argparse
import base64
import datetime
import hashlib
import json
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

PROJECT = Path(__file__).resolve().parent.parent
GATEWAY = "https://officeace.1task.cn"
CHROME_ROOT = Path.home() / "Library" / "Application Support" / "Google" / "Chrome"
IAM_ISSUER = "xiaohuanxiong.com/iam"


def safe_storage_key() -> bytes | None:
    try:
        pw = subprocess.check_output(
            ["security", "find-generic-password", "-s", "Chrome Safe Storage", "-w"],
            stderr=subprocess.DEVNULL, timeout=10,
        ).strip()
    except Exception:
        return None
    return hashlib.pbkdf2_hmac("sha1", pw, b"saltysalt", 1003, 16)


def decrypt(val: bytes, key: bytes) -> str:
    from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

    if not val or val[:3] not in (b"v10", b"v11") or len(val[3:]) % 16:
        return ""  # v20 = app-bound encryption，不支持
    dec = Cipher(algorithms.AES(key), modes.CBC(b" " * 16)).decryptor()
    out = dec.update(val[3:]) + dec.finalize()
    pad = out[-1]
    if 1 <= pad <= 16:
        out = out[:-pad]
    # Chrome 24+ 在明文前加了 32 字节 domain hash
    for strip in (32, 0):
        try:
            txt = out[strip:].decode("utf-8")
        except UnicodeDecodeError:
            continue
        if txt.startswith("eyJ"):
            return txt
    return ""


def jwt_iam_exp(tok: str) -> int | None:
    parts = tok.split(".")
    if len(parts) != 3:
        return None
    try:
        pl = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
    except Exception:
        return None
    if pl.get("iss") != IAM_ISSUER:
        return None
    exp = pl.get("exp")
    return exp if isinstance(exp, int) else None


def scan() -> list[dict]:
    key = safe_storage_key()
    if key is None:
        print("✗ 拿不到 Chrome Safe Storage 主密钥（钥匙串拒绝）", file=sys.stderr)
        return []
    out, seen = [], set()
    for prof in sorted(CHROME_ROOT.glob("*")):
        db = prof / "Cookies"
        if not db.is_file():
            continue
        tmp = Path(tempfile.mkdtemp()) / "c.db"
        try:
            shutil.copy(db, tmp)
            con = sqlite3.connect(tmp)
            rows = con.execute(
                "select host_key,name,encrypted_value from cookies "
                "where host_key like '%xiaohuanxiong%'"
            ).fetchall()
            con.close()
        except Exception:
            continue
        for host, name, enc in rows:
            tok = decrypt(enc, key)
            exp = jwt_iam_exp(tok) if tok else None
            if exp and tok not in seen:
                seen.add(tok)
                out.append({"exp": exp, "token": tok, "profile": prof.name, "name": name, "host": host})
    return sorted(out, key=lambda x: -x["exp"])


def push(token: str, panel_key: str) -> None:
    body = json.dumps({"token": token, "note": "浏览器登录提取", "identity": "personal"}).encode()
    req = urllib.request.Request(
        GATEWAY.rstrip("/") + "/panel/api/accounts", data=body,
        headers={"Authorization": "Bearer " + panel_key, "Content-Type": "application/json"},
        method="POST")
    op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with op.open(req, timeout=30) as r:
        print("推送结果:", r.read().decode()[:200])


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--push", action="store_true")
    ap.add_argument("--dump", action="store_true", help="打印完整 token")
    args = ap.parse_args()

    cands = scan()
    if not cands:
        print("✗ Chrome 里没找到小浣熊 IAM token")
        print("  → 先在 Chrome 打开 https://xiaohuanxiong.com/login 登录，再重跑")
        return 1

    now = datetime.datetime.now().timestamp()
    print(f"找到 {len(cands)} 个候选：")
    for c in cands:
        left = (c["exp"] - now) / 3600
        tag = "有效" if left > 0 else "已过期"
        print(f"  {datetime.datetime.fromtimestamp(c['exp'])} | {tag:4s} "
              f"剩余 {left:9.1f}h | {c['profile']}/{c['host']}/{c['name']}")

    best = cands[0]
    if best["exp"] <= now:
        print("\n✗ 最新一个也已过期，请在浏览器重新登录")
        return 1

    tok = best["token"]
    print("\ntoken:", tok if args.dump else f"{tok[:24]}…{tok[-8:]}  (len={len(tok)})")

    if args.push:
        cfg = json.loads((PROJECT / "data" / "config.json").read_text())
        push(tok, cfg["panel_key"])
    return 0


if __name__ == "__main__":
    sys.exit(main())
