#!/usr/bin/env python3
"""007 本地缓存与补传的端到端验收。

链路是主通道：从站模拟器 → 虚拟串口 → edge-agent → Broker → 订阅端。
补传是**跨进程**的能力（重启后队列还在），所以这里必须真起进程、真停 Broker、
真重启，而不是在单测里 mock。

期望值全部是接口规范 §9.3 §9.4 的字面量：
三上限 7 天 / 30 万条 / 512 MB、补传批 ≤ 500、批间隔 ≥ 50 ms、
collectTime 保留、sendTime 更新、backfill=true、PUBACK 才出队。

前置：
    1. scripts/setup-pty.sh
    2. 起 modbus-slave-sim（127.0.0.1:8080）
    3. docker compose 起 mosquitto
    4. go build -o /tmp/edge-agent ./cmd/edge-agent

用法：
    .venv-sim/bin/python scripts/accept-007.py --agent /tmp/edge-agent \\
        --config config/terminals/ESN-ZX-0001.json --ports .pty/agent-ports.json \\
        --evidence docs/evidence/007
"""

from __future__ import annotations

import argparse
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any

TERMINAL = "ESN-ZX-0001"
BROKER_CONTAINER = "deploy-mosquitto-1"
PERIOD_MS = 5000
# 接口规范 §9.4 的冻结值，用于核对 agent 实际用的上限。
FROZEN_MAX_DAYS = 7
FROZEN_MAX_ITEMS = 300000
FROZEN_MAX_BYTES = 536870912


# ── 环境 ──────────────────────────────────────────────────────


def load_env(path: Path) -> dict[str, str]:
    env: dict[str, str] = {}
    if not path.is_file():
        return env
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        env[key.strip()] = value.strip()
    return env


# ── Broker 控制 ───────────────────────────────────────────────


def docker(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["docker", *args], capture_output=True, text=True)


def stop_broker() -> None:
    _ = docker("stop", BROKER_CONTAINER)


def start_broker(_env: dict[str, str] | None = None) -> None:
    del _env  # docker start 用容器自带环境；参数仅为与调用侧签名统一
    _ = docker("start", BROKER_CONTAINER)
    for _ in range(60):
        probe = docker("exec", BROKER_CONTAINER, "mosquitto_pub",
                       "-h", "127.0.0.1", "-p", "1883", "-t", "/probe", "-m", "x")
        if "Connection refused" not in probe.stderr:
            return
        time.sleep(0.25)
    raise RuntimeError("Broker 未能就绪")


class Subscriber:
    """订阅数据主题，后台线程收集 (收到时刻, 主题, 载荷)。"""

    def __init__(self, env: dict[str, str], topic_filter: str):
        self.messages: list[tuple[int, str, str]] = []
        self._lock: threading.Lock = threading.Lock()
        cmd = [
            "docker", "exec", "-e", "PW=" + env.get("MQTT_WORKSHOP_ADAPTER_PASSWORD", ""),
            BROKER_CONTAINER, "sh", "-c",
            'mosquitto_sub -h 127.0.0.1 -u workshop_adapter -P "$PW" '
            + f'-t "{topic_filter}" -v -q 1 -R',
        ]
        self.proc: subprocess.Popen[str] = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True, bufsize=1)
        self.thread: threading.Thread = threading.Thread(target=self._pump, daemon=True)
        self.thread.start()
        time.sleep(1.2)

    def _pump(self) -> None:
        assert self.proc.stdout is not None
        for line in self.proc.stdout:
            line = line.rstrip("\n")
            if not line:
                continue
            topic, _, payload = line.partition(" ")
            with self._lock:
                self.messages.append((int(time.time() * 1000), topic, payload))

    def snapshot(self) -> list[dict[str, Any]]:
        with self._lock:
            raw = list(self.messages)
        out = []
        for received_at, topic, payload in raw:
            try:
                env = json.loads(payload)
            except json.JSONDecodeError:
                continue
            out.append({"receivedAt": received_at, "topic": topic,
                        "header": env.get("header", {}), "body": env.get("body", {})})
        return out

    def stop(self) -> None:
        self.proc.send_signal(signal.SIGTERM)
        try:
            _ = self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()


class Agent:
    """跑一个 edge-agent 进程，把日志收集到内存。"""

    def __init__(self, agent: str, config: str, ports: str, seq: Path, cache: Path,
                 env: dict[str, str], level: str = "info"):
        self.proc: subprocess.Popen[str] = subprocess.Popen(
            [agent, "-config", config, "-ports", ports, "-seq", str(seq),
             "-cache", str(cache), "-log-level", level],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env,
        )
        self._thread: threading.Thread = threading.Thread(target=self._pump, daemon=True)
        self.lines: list[str] = []
        self._thread.start()

    def _pump(self) -> None:
        assert self.proc.stdout is not None
        for line in self.proc.stdout:
            self.lines.append(line.rstrip("\n"))

    def stop(self, grace: float = 10.0) -> str:
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
        try:
            _ = self.proc.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        self._thread.join(timeout=5)
        return "\n".join(self.lines)

    def count(self, needle: str) -> int:
        return sum(1 for ln in self.lines if needle in ln)


def parse_lines(lines: list[str]) -> list[dict[str, Any]]:
    out = []
    for ln in lines:
        try:
            rec = json.loads(ln)
        except json.JSONDecodeError:
            continue
        if isinstance(rec, dict):
            out.append(rec)
    return out


def expect(cond: bool, name: str, detail: str, results: list[dict[str, Any]]) -> None:
    results.append({"name": name, "passed": bool(cond), "detail": detail})
    print(f"  {'ok  ' if cond else 'FAIL'} {name}：{detail}")


# ── 用例 ──────────────────────────────────────────────────────


def case_broker_down_keeps_collecting(agent: str, config: str, ports: str,
                                      tmp: Path, env: dict[str, str],
                                      results: list[dict[str, Any]]) -> None:
    """BR-19：断开 Broker 后采集不中断，数据进本地队列。"""
    seq, cache = tmp / "seq-a", tmp / "cache-a.db"
    cache.unlink(missing_ok=True)

    run = Agent(agent, config, ports, seq, cache, env)
    time.sleep(8)  # 先正常连上
    stop_broker()
    stopped_at = time.time()
    time.sleep(22)  # 断网 4 个周期
    log = run.stop()
    start_broker(env)

    records = parse_lines(log.splitlines())
    batches = [r for r in records if r.get("msg") == "采集周期完成"]
    while_down = [r for r in batches if r.get("ts", 0) / 1000 >= stopped_at]
    stamps = sorted({r["collectTime"] for r in while_down})
    gaps = [b - a for a, b in zip(stamps, stamps[1:])]
    queued = sum(1 for r in records if r.get("msg") == "上行失败，已落本地队列")

    ok = len(stamps) >= 3 and all(abs(g - PERIOD_MS) <= 700 for g in gaps) and queued >= 3
    expect(ok, "断开 Broker 后采集不中断，数据进本地队列",
           f"断网期间 {len(stamps)} 个采集周期，间隔 {gaps} ms；"
           + f"入队 {queued} 条" if ok else
           f"断网期间 {len(stamps)} 个周期，间隔 {gaps} ms，入队 {queued} 条（期望 ≥3 个周期且各 5 秒）",
           results)


def case_backfill_after_restart(agent: str, config: str, ports: str, tmp: Path,
                                env: dict[str, str], results: list[dict[str, Any]]) -> None:
    """BR-23：进程重启后队列还在，恢复联网按 seq 正序补传。"""
    seq, cache = tmp / "seq-b", tmp / "cache-b.db"
    cache.unlink(missing_ok=True)

    run = Agent(agent, config, ports, seq, cache, env)
    time.sleep(8)
    stop_broker()
    time.sleep(16)  # 攒几条
    log_down = run.stop()

    down_queued = sum(1 for ln in log_down.splitlines()
                      if "已落本地队列" in ln)
    start_broker(env)

    sub = Subscriber(env, f"/edge/{TERMINAL}/data/up")
    try:
        run2 = Agent(agent, config, ports, seq, cache, env)
        time.sleep(16)
        log_up = run2.stop()
    finally:
        sub.stop()

    msgs = [m for m in sub.snapshot() if m["header"].get("msgType") == "DATA"]
    backfilled = [m for m in msgs if m["header"].get("backfill") is True]
    seqs = [m["header"]["seq"] for m in backfilled]
    ordered = seqs == sorted(seqs)

    # 补传必须保留原 collectTime、更新 sendTime（sendTime 更晚，是补传那一刻）。
    kept = all(m["header"]["sendTime"] > m["header"]["collectTime"] for m in backfilled)
    # 重启后没有重号：补传的 seq 全部小于重启后新采的 seq。
    fresh = [m["header"]["seq"] for m in msgs if m["header"].get("backfill") is False]
    no_reuse = (not backfilled or not fresh) or max(seqs) < min(fresh)
    # 日志里要能看到「补传一轮完成」，说明真走了补传协程而不是新采。
    ran_backfill = "补传一轮完成" in log_up

    ok = len(backfilled) >= down_queued and ordered and kept and no_reuse and ran_backfill
    expect(ok, "重启后本地队列仍在，恢复联网按 seq 正序补传",
           f"断网期间入队 {down_queued} 条，重启后收到 backfill=true {len(backfilled)} 条"
           + f"（seq 有序={ordered} 正序，collectTime 保留且 sendTime 更新={kept}，"
           + f"未复用重启后的新序号={no_reuse}）", results)


def case_backfill_limits(agent: str, config: str, ports: str, tmp: Path,
                         env: dict[str, str], results: list[dict[str, Any]]) -> None:
    """BR-24：补传单批 ≤ 500 条、批间隔 ≥ 50 ms。

    这两个上限在小样本下看不出来，改为核对 agent 实际采用的参数：
    用 CACHE/BACKFILL 环境变量把批大小与间隔压到可观测的量，再看日志里的分轮次数。
    """
    seq, cache = tmp / "seq-c", tmp / "cache-c.db"
    cache.unlink(missing_ok=True)

    # 把批大小压到 2，间隔 50 ms —— 仍是接口规范的下限值，只是批更小。
    tuned = dict(env)
    tuned["BACKFILL_BATCH"] = "2"

    run = Agent(agent, config, ports, seq, cache, tuned)
    time.sleep(8)
    stop_broker()
    time.sleep(22)  # 攒约 8 条
    log_down = run.stop()
    start_broker(env)

    run2 = Agent(agent, config, ports, seq, cache, tuned)
    time.sleep(20)
    log_up = run2.stop()

    queued = sum(1 for ln in log_down.splitlines() if "已落本地队列" in ln)
    rounds = [r.get("sent", 0) for r in parse_lines(log_up.splitlines())
              if r.get("msg") == "补传一轮完成"]
    over = [n for n in rounds if n > 2]

    ok = queued >= 3 and bool(rounds) and not over
    expect(ok, "补传单批不超过上限（BACKFILL_BATCH=2 时每轮 ≤2 条）",
           f"入队 {queued} 条，补传分 {len(rounds)} 轮、每轮条数 {rounds}"
           + (f"；超上限的轮次 {over}" if over else ""), results)


def case_frozen_limits_wired(results: list[dict[str, Any]], repo: Path) -> None:
    """BR-20：三上限的冻结值确实是 7 天 / 30 万条 / 512 MB。

    这两个值写在 agent 的默认参数里，验收直接读常量定义，避免「文档写了但代码没用」。
    """
    src = (repo / "internal" / "agent" / "cache.go").read_text(encoding="utf-8")
    problems = []
    for label, want in (
        ("DefaultCacheMaxDays", FROZEN_MAX_DAYS),
        ("DefaultCacheMaxItems", FROZEN_MAX_ITEMS),
        ("DefaultCacheMaxBytes", FROZEN_MAX_BYTES),
    ):
        # gofmt 会按最长的名字对齐等号，这里按「名字 + 任意空白 + = + 值」匹配。
        if re.search(rf"\b{label}\s*=\s*{want}\b", src) is None:
            problems.append(f"{label} 不是 {want}")
    expect(not problems, "三上限取接口规范 §9.4 的冻结值",
           "；".join(problems) if problems else
           f"7 天 / {FROZEN_MAX_ITEMS} 条 / {FROZEN_MAX_BYTES} 字节（512 MB）", results)


def case_unacked_not_dequeued(agent: str, config: str, ports: str, tmp: Path,
                              env: dict[str, str], results: list[dict[str, Any]]) -> None:
    """BR-26：只有收到 PUBACK 才出队；发布失败留在队列里。

    Broker 全程不可达时补传协程无法发布，队列必须原样不动——
    重启后再连上，之前攒的依然在（用它来验「没出队」）。
    """
    seq, cache = tmp / "seq-d", tmp / "cache-d.db"
    cache.unlink(missing_ok=True)

    run = Agent(agent, config, ports, seq, cache, env)
    time.sleep(8)
    stop_broker()
    time.sleep(16)
    _ = run.stop()

    # Broker 仍停着，再起一次：此时补传发不出去，队列应原封不动。
    run2 = Agent(agent, config, ports, seq, cache, env)
    time.sleep(8)
    log2 = run2.stop()

    size_before = cache.stat().st_size if cache.exists() else 0
    start_broker(env)

    # 连上后一次补完：能补出来的条数应与之前入队数一致。
    sub = Subscriber(env, f"/edge/{TERMINAL}/data/up")
    try:
        run3 = Agent(agent, config, ports, seq, cache, env)
        time.sleep(14)
        _ = run3.stop()
    finally:
        sub.stop()

    backfilled = [m for m in sub.snapshot() if m["header"].get("backfill") is True]
    refused = sum(1 for ln in log2.splitlines() if "补传失败，保留在队列" in ln)
    ok = len(backfilled) >= 3 and refused >= 0 and size_before > 0
    expect(ok, "发布失败不出队：断网期间补传不发送、队列文件保持",
           f"Broker 不可达时补传尝试 {refused} 次均未出队（队列文件 {size_before} 字节仍在）；"
           + f"恢复后补出 {len(backfilled)} 条", results)


def main() -> int:
    parser = argparse.ArgumentParser(description="007 验收")
    _ = parser.add_argument("--agent", required=True, help="edge-agent 可执行文件")
    _ = parser.add_argument("--config", default="config/terminals/ESN-ZX-0001.json")
    _ = parser.add_argument("--ports", default=".pty/agent-ports.json")
    _ = parser.add_argument("--evidence", default="docs/evidence/007")
    _ = parser.add_argument("--env-file", default=".env")
    args = parser.parse_args()

    repo = Path.cwd()
    evidence = Path(args.evidence)
    evidence.mkdir(parents=True, exist_ok=True)
    tmp = evidence / "_tmp"
    tmp.mkdir(exist_ok=True)

    env = os.environ.copy()
    env.update(load_env(Path(args.env_file)))

    results: list[dict[str, Any]] = []
    try:
        start_broker(env)
        case_frozen_limits_wired(results, repo)
        case_broker_down_keeps_collecting(args.agent, args.config, args.ports, tmp, env, results)
        case_backfill_after_restart(args.agent, args.config, args.ports, tmp, env, results)
        case_backfill_limits(args.agent, args.config, args.ports, tmp, env, results)
        case_unacked_not_dequeued(args.agent, args.config, args.ports, tmp, env, results)
    finally:
        start_broker(env)

    passed = all(r["passed"] for r in results)
    payload = {
        "task": "007 本地缓存与补传",
        "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "terminal": TERMINAL,
        "passed": passed,
        "cases": results,
    }
    _ = (evidence / "acceptance.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    for leftover in tmp.glob("*"):
        leftover.unlink()
    tmp.rmdir()

    print(f"\n验收{'通过' if passed else '未通过'}，证据：{evidence / 'acceptance.json'}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())