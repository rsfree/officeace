#!/usr/bin/env python3
"""
高压冲刺探测：并发 10、无间隔，用于撞出 429 拐点

与 rate-probe.py 的区别
- 不做阶梯推进，一上来就是目标压力
- worker 完成即发下一个请求（无 sleep），压力完全由并发数决定
- 一旦出现 429 立刻记录"第几秒 / 第几个请求"，这是阈值的关键证据
- 连续 5 次 429 自动停机（说明已经压到限流，再打无意义）
"""
import json
import os
import signal
import threading
import time
import urllib.error
import urllib.request

URL = "https://xiaohuanxiong.com/api/web/llm/v2/images/gen"
ACCOUNTS = "/opt/officeace2api/data/accounts.json"
LOG = "/opt/officeace2api/probe/burst.jsonl"

CONCURRENCY = 10
DURATION_SEC = 12 * 60
STOP_ON_429_STREAK = 5

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
lock = threading.Lock()
records = []
stop = threading.Event()
seq_counter = {"n": 0}
first_429 = {"t": None, "seq": None}
streak = {"n": 0}


def load_token():
    with open(ACCOUNTS) as f:
        accs = json.load(f)
    return (accs[0] if isinstance(accs, list) else list(accs.values())[0])["token"]


TOKEN = load_token()


def call_once(state):
    body = json.dumps({
        "model": "sn-image",
        "prompt": f"压力测试图形 {state}",
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
        with opener.open(req, timeout=300) as r:
            raw = r.read()
            code, size = r.status, len(raw)
    except urllib.error.HTTPError as e:
        code = e.code
        err = e.read(300).decode("utf-8", "replace")[:200]
        ra = e.headers.get("Retry-After")
        if ra:
            err = f"Retry-After={ra} | " + err
    except Exception as e:
        err = f"{type(e).__name__}: {e}"[:200]
    return {"t": round(t0, 3), "code": code, "dur": round(time.time() - t0, 2),
            "bytes": size, "err": err}


def log(rec):
    with lock:
        records.append(rec)
    with open(LOG, "a") as f:
        f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def worker(wid):
    while not stop.is_set():
        with lock:
            seq_counter["n"] += 1
            seq = seq_counter["n"]
        rec = call_once(f"{wid}-{seq}")
        rec["w"] = wid
        rec["seq"] = seq
        log(rec)

        if rec["code"] in (401, 403):
            print(f"[FATAL] {rec['code']} 凭证/风控异常，停机", flush=True)
            stop.set()
            return

        if rec["code"] == 429:
            with lock:
                streak["n"] += 1
                hits = streak["n"]
                if first_429["t"] is None:
                    first_429["t"] = rec["t"]
                    first_429["seq"] = seq
            print(f"[429] seq={seq} worker={wid} streak={hits} err={rec['err']}", flush=True)
            if hits >= STOP_ON_429_STREAK:
                print(f"[STOP] 连续 {hits} 次 429，判定已触限流", flush=True)
                stop.set()
                return
        else:
            with lock:
                streak["n"] = 0


def reporter(start):
    last_n = 0
    while not stop.wait(30):
        now = time.time()
        with lock:
            snap = list(records)
        recent = [r for r in snap if now - r["t"] <= 60]
        n_total = len(snap)
        window = n_total - last_n
        last_n = n_total
        ok = sum(1 for r in recent if r["code"] == 200)
        rl = sum(1 for r in recent if r["code"] == 429)
        err = len(recent) - ok - rl
        durs = sorted(r["dur"] for r in recent if r["code"] == 200)
        p50 = durs[len(durs) // 2] if durs else 0
        elapsed = int(now - start)
        print(f"[{elapsed//60:02d}:{elapsed%60:02d}] 累计 {n_total} | 近60s {len(recent)}req "
              f"{ok}ok {rl}429 {err}err | 30s新 {window} | p50={p50:.1f}s", flush=True)


def main():
    os.makedirs(os.path.dirname(LOG), exist_ok=True)
    start = time.time()
    print(f"[BURST] 并发 {CONCURRENCY} 无间隔 时长 {DURATION_SEC//60}min "
          f"pid={os.getpid()} at {time.strftime('%H:%M:%S')}", flush=True)

    def on_term(*_):
        stop.set()
    signal.signal(signal.SIGTERM, on_term)
    signal.signal(signal.SIGINT, on_term)

    threading.Thread(target=reporter, args=(start,), daemon=True).start()

    threads = [threading.Thread(target=worker, args=(i,), daemon=True)
               for i in range(CONCURRENCY)]
    for t in threads:
        t.start()

    while time.time() - start < DURATION_SEC and not stop.is_set():
        time.sleep(1)
    stop.set()
    for t in threads:
        t.join(timeout=300)

    with lock:
        snap = list(records)
    ok = sum(1 for r in snap if r["code"] == 200)
    rl = sum(1 for r in snap if r["code"] == 429)
    dur = time.time() - start
    print(f"\n[DONE] 总 {len(snap)} | 成功 {ok} | 429 {rl} | 其他 {len(snap)-ok-rl} "
          f"| 平均 QPS {len(snap)/dur:.2f} | 峰值窗口见上方日志", flush=True)
    if first_429["t"]:
        print(f"[拐点] 首次 429 在第 {first_429['t']-start:.0f} 秒 / 第 {first_429['seq']} 个请求", flush=True)
    else:
        print("[拐点] 全程无 429 —— 并发 10 仍未触达限流", flush=True)


if __name__ == "__main__":
    main()
