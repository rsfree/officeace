#!/usr/bin/env python3
"""
上游图片生成端点频控探测（阶梯加压 + 滚动统计）

设计要点
- 阶梯推进：串行低频 → 串行高频 → 逐级提高并发，观察 429/异常出现的拐点
- 滚动统计：每 60s 打印 1m/5m/15m/1h 四个窗口的 QPS、成功率、耗时分位
- 自动降级：连续 2 次 429 就回退一档，并记录"触发时的并发/间隔"
- 熔断：出现 401/403（凭证或风控）立刻停机，避免把账号打坏
- 不落图片数据：只记录状态码与字节数，日志体积可控

启动
  nohup python3 rate-probe.py > rate.out 2>&1 &
"""
import json
import os
import random
import signal
import sys
import threading
import time
import urllib.error
import urllib.request

URL = "https://xiaohuanxiong.com/api/web/llm/v2/images/gen"
ACCOUNTS = "/opt/officeace2api/data/accounts.json"
LOG = "/opt/officeace2api/probe/rate.jsonl"

# 阶段：(开始分钟, 并发, 间隔秒, 名称)
PHASES = [
    (0,   1, 25, "P1 串行-慢"),
    (20,  1, 15, "P2 串行-中"),
    (40,  1,  8, "P3 串行-快"),
    (60,  2,  8, "P4 并发2"),
    (80,  3,  8, "P5 并发3"),
    (100, 5,  5, "P6 并发5"),
    (120, 2, 10, "P7 稳态"),
]

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
lock = threading.Lock()
CURRENT = {"name": ""}   # 当前阶段名，worker 每轮自检，阶段切换后自行退出
records = []          # 内存环形，用于滚动统计
stop_flag = threading.Event()
consecutive_429 = 0
degrade_until = 0.0   # 降级后的恢复时间
degrade_gap_mul = 1.0


def load_token():
    with open(ACCOUNTS) as f:
        accs = json.load(f)
    if isinstance(accs, list):
        return accs[0]["token"]
    return list(accs.values())[0]["token"]


TOKEN = load_token()


def call_once(prompt):
    body = json.dumps({
        "model": "sn-image",
        "prompt": prompt,
        "n": 1,
        "quality": "low",
        "size": "1024x1024",
    }).encode()
    req = urllib.request.Request(
        URL, data=body, method="POST",
        headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"},
    )
    t0 = time.time()
    code, size, err = 0, 0, None
    try:
        with opener.open(req, timeout=240) as r:
            raw = r.read()
            code, size = r.status, len(raw)
    except urllib.error.HTTPError as e:
        code = e.code
        raw = e.read(400).decode("utf-8", "replace")
        err = raw[:200]
        ra = e.headers.get("Retry-After")
        if ra:
            err = f"Retry-After={ra} {err}"
    except Exception as e:
        err = f"{type(e).__name__}: {e}"[:200]
    return {
        "t": round(t0, 3),
        "code": code,
        "dur": round(time.time() - t0, 2),
        "bytes": size,
        "err": err,
    }


def log(rec):
    with lock:
        records.append(rec)
        if len(records) > 20000:
            del records[:5000]
    with open(LOG, "a") as f:
        f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def worker(worker_id, gap, phase_name):
    global consecutive_429, degrade_until, degrade_gap_mul
    seq = 0
    while not stop_flag.is_set():
        if CURRENT["name"] != phase_name:
            return
        if time.time() < degrade_until:
            time.sleep(1)
            continue
        seq += 1
        rec = call_once(f"抽象几何图形 {worker_id}-{seq}")
        rec["w"] = worker_id
        rec["ph"] = phase_name
        rec["gap"] = round(gap * degrade_gap_mul, 1)
        log(rec)

        if rec["code"] == 401 or rec["code"] == 403:
            print(f"[FATAL] {rec['code']} 凭证/风控异常，停机", flush=True)
            stop_flag.set()
            return
        if rec["code"] == 429:
            with lock:
                consecutive_429 += 1
                hits = consecutive_429
            if hits >= 2:
                degrade_gap_mul = min(degrade_gap_mul * 2, 4.0)
                degrade_until = time.time() + 60
                print(f"[DEGRADE] 连续 429 x{hits}，间隔 x{degrade_gap_mul}，暂停 60s", flush=True)
        else:
            consecutive_429 = 0
        # 平滑：成功后逐步恢复间隔
        if rec["code"] == 200 and degrade_gap_mul > 1.0:
            degrade_gap_mul = max(1.0, degrade_gap_mul * 0.9)

        time.sleep(max(0.5, gap * degrade_gap_mul))


def pct(vals, p):
    if not vals:
        return 0
    vals = sorted(vals)
    k = min(len(vals) - 1, int(len(vals) * p))
    return vals[k]


def reporter(start_ts):
    windows = [(60, "1m"), (300, "5m"), (900, "15m"), (3600, "1h")]
    last_phase = None
    while not stop_flag.wait(60):
        now = time.time()
        with lock:
            snap = list(records)
        parts = []
        for sec, label in windows:
            sub = [r for r in snap if now - r["t"] <= sec]
            if not sub:
                parts.append(f"{label}: -")
                continue
            ok = sum(1 for r in sub if r["code"] == 200)
            rl = sum(1 for r in sub if r["code"] == 429)
            parts.append(f"{label}: {len(sub)}req {ok}ok {rl}rl")
        durs = [r["dur"] for r in snap if now - r["t"] <= 300 and r["code"] == 200]
        line = " | ".join(parts)
        print(f"[{time.strftime('%H:%M')}] {line} | p50={pct(durs,0.5)}s p95={pct(durs,0.95)}s", flush=True)

    # 收尾
    with lock:
        snap = list(records)
    total = len(snap)
    ok = sum(1 for r in snap if r["code"] == 200)
    print(f"[DONE] 总 {total} 次 | 成功 {ok} | 时长 {(time.time()-start_ts)/3600:.1f}h", flush=True)


def main():
    os.makedirs(os.path.dirname(LOG), exist_ok=True)
    start = time.time()
    print(f"[START] {time.strftime('%Y-%m-%d %H:%M:%S')} pid={os.getpid()}", flush=True)

    def on_term(*_):
        print("[SIGNAL] 收到终止信号，收尾", flush=True)
        stop_flag.set()
    signal.signal(signal.SIGTERM, on_term)
    signal.signal(signal.SIGINT, on_term)

    threading.Thread(target=reporter, args=(start,), daemon=True).start()

    current = None
    threads = []
    while not stop_flag.is_set():
        elapsed_min = (time.time() - start) / 60
        phase = PHASES[-1]
        for p in PHASES:
            if elapsed_min >= p[0]:
                phase = p
        _, conc, gap, name = phase
        if name != current:
            threads = []
            print(f"[PHASE] {name} conc={conc} gap={gap}s", flush=True)
            CURRENT["name"] = name
            current = name
            for i in range(conc):
                th = threading.Thread(target=worker, args=(i, gap, name), daemon=True)
                th.start()
                threads.append(th)
        time.sleep(5)

    for t in threads:
        t.join(timeout=270)
    print("[EXIT]", flush=True)


if __name__ == "__main__":
    main()
