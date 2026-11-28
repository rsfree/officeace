#!/usr/bin/env python3
"""多形态出图探针：对线上网关跑一组形态矩阵，记录每个形态的真实行为。

用法:
    python3 scripts/multiform-probe.py [--only 尺寸,批量,图生图,边界] [--quality low]

形态分组:
    尺寸    5 种宽高比（1:1 / 3:2 / 2:3 / 7:4 / 4:7）
    批量    n=2 一次出两张
    图生图  带 1 张参考图（走 /images/edits 反向适配）
    边界    应当被拒的入参（512 / 4096 / xhigh），验证准入是否真的生效

只发 quality=low（low 的图像 token 约为 high 的 1/36），把成本压到最低。
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

PROJECT = Path(__file__).resolve().parent.parent
GATEWAY = os.environ.get("GATEWAY", "https://officeace.1task.cn")
OUTDIR = PROJECT / "out" / time.strftime("multiform-%Y-%m-%d")

SIZE_CASES = [
    ("1024x1024", "1:1 方图"),
    ("1536x1024", "3:2 横图"),
    ("1024x1536", "2:3 竖图"),
    ("1792x1024", "7:4 宽横幅"),
    ("1024x1792", "4:7 长竖幅"),
]

PROMPT = "一枚哑光金属质感的六边形徽章，居中，纯净浅灰背景，柔和顶光"

BOUNDARY_CASES = [
    ("512x512", "low", "尺寸低于像素预算下限"),
    ("4096x4096", "low", "超过最长边 3840 上限"),
    ("1024x1024", "xhigh", "不支持的 quality 档"),
]


def api_key() -> str:
    if os.environ.get("API_KEY"):
        return os.environ["API_KEY"]
    cfg = json.loads((PROJECT / "data" / "config.json").read_text())
    return cfg["api_key"]


def post(body: dict, key: str, timeout: int = 300) -> tuple[int, float, dict | str]:
    req = urllib.request.Request(
        GATEWAY.rstrip("/") + "/v1/images/generations",
        data=json.dumps(body, ensure_ascii=False).encode(),
        headers={
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
        },
        method="POST",
    )
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    start = time.time()
    try:
        with opener.open(req, timeout=timeout) as resp:
            raw = resp.read()
            code = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        code = e.code
    except Exception as e:  # noqa: BLE001
        return 0, time.time() - start, "连接失败: " + repr(e)
    elapsed = time.time() - start
    try:
        return code, elapsed, json.loads(raw)
    except json.JSONDecodeError:
        return code, elapsed, raw[:400].decode("utf-8", "replace")


def summarize(code: int, elapsed: float, payload) -> dict:
    row = {"http": code, "elapsed_s": round(elapsed, 1)}
    if not isinstance(payload, dict):
        row["note"] = str(payload)[:200]
        return row
    usage = payload.get("usage") or {}
    row["in_tok"] = usage.get("input_tokens")
    row["out_tok"] = usage.get("output_tokens")
    if usage.get("input_tokens_details"):
        row["img_tok"] = usage["input_tokens_details"].get("image_tokens")
    data = payload.get("data")
    if isinstance(data, list):
        row["n_returned"] = len(data)
        row["size_echo"] = payload.get("size")
        row["quality_echo"] = payload.get("quality")
    err = payload.get("error")
    if err:
        row["error_code"] = err.get("code") if isinstance(err, dict) else None
        row["error_msg"] = (err.get("message") if isinstance(err, dict) else str(err))[:180]
    return row


def save_images(payload, stem: str) -> list[str]:
    saved = []
    data = payload.get("data") if isinstance(payload, dict) else None
    if not isinstance(data, list):
        return saved
    for i, item in enumerate(data):
        b64 = item.get("b64_json") if isinstance(item, dict) else None
        if not b64:
            continue
        path = OUTDIR / (f"{stem}.png" if len(data) == 1 else f"{stem}-{i}.png")
        path.write_bytes(base64.b64decode(b64))
        saved.append(str(path.relative_to(PROJECT)))
    return saved


def main() -> int:
    global OUTDIR
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="尺寸,批量,图生图,边界")
    ap.add_argument("--quality", default="low")
    ap.add_argument("--out", default="", help="输出子目录名（默认 multiform-YYYY-MM-DD）")
    args = ap.parse_args()

    if args.out:
        OUTDIR = PROJECT / "out" / args.out

    only = {s.strip() for s in args.only.split(",")}
    OUTDIR.mkdir(parents=True, exist_ok=True)
    key = api_key()
    results: dict[str, list[dict]] = {}

    def run(group: str, name: str, body: dict, stem: str):
        code, elapsed, payload = post(body, key)
        row = {"case": name, **summarize(code, elapsed, payload)}
        if code == 200:
            row["files"] = save_images(payload, stem)
        results.setdefault(group, []).append(row)
        flag = "✓" if code == 200 else "✗"
        extra = row.get("error_msg") or ""
        print(f"{flag} [{group}] {name:24s} HTTP {code:<4} {row['elapsed_s']:>6}s "
              f"out_tok={row.get('out_tok')} {extra}", flush=True)

    if "尺寸" in only:
        for size, label in SIZE_CASES:
            run("尺寸", f"{size} {label}",
                {"model": "sn-image", "prompt": PROMPT, "n": 1,
                 "quality": args.quality, "size": size},
                f"size-{size.replace('x', 'X')}")

    if "批量" in only:
        run("批量", "n=2 一次两张",
            {"model": "sn-image", "prompt": PROMPT, "n": 2,
             "quality": args.quality, "size": "1024x1024"}, "batch-n2")

    if "图生图" in only:
        ref = PROJECT / "out" / "img-low-20261121-045043.png"
        if ref.exists():
            b64 = base64.b64encode(ref.read_bytes()).decode()
            run("图生图", "单图垫图 1:1",
                {"model": "sn-image", "prompt": "把这张图改成青绿色调，保持构图不变",
                 "n": 1, "quality": args.quality, "size": "1024x1024",
                 "image": "data:image/png;base64," + b64}, "img2img-1ref")
        else:
            print(f"! 跳过图生图：参考图不存在 {ref}", flush=True)

    if "边界" in only:
        for size, quality, expect in BOUNDARY_CASES:
            run("边界", f"{size} {quality} ({expect})",
                {"model": "sn-image", "prompt": PROMPT, "n": 1,
                 "quality": quality, "size": size},
                f"boundary-{size}-{quality}")

    report = OUTDIR / "report.json"
    report.write_text(json.dumps(results, ensure_ascii=False, indent=2))
    print(f"\n报告: {report.relative_to(PROJECT)}", flush=True)

    ok = sum(1 for g in results.values() for r in g if r["http"] == 200)
    total = sum(len(g) for g in results.values())
    print(f"通过 {ok}/{total}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
