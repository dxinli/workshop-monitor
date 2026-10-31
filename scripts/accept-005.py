#!/usr/bin/env python3
"""005 采集轮询与换算的端到端验收。

链路是主通道：从站模拟器 → 虚拟串口 → edge-agent → 日志。
不 import 交付件，也不复用 agent 的换算代码——期望值全部是手算字面量，
照抄实现算出来的「期望」永远不会失败，等于没验。

前置：
    1. scripts/setup-pty.sh          建 10 对 pty
    2. 起 modbus-slave-sim（127.0.0.1:8080）
    3. go build -o /tmp/edge-agent ./cmd/edge-agent

用法：
    .venv-sim/bin/python scripts/accept-005.py --agent /tmp/edge-agent \\
        --config config/terminals/ESN-ZX-0001.json --ports .pty/agent-ports.json \\
        --evidence docs/evidence/005
"""

from __future__ import annotations

import argparse
import json
import signal
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

SIM = "http://127.0.0.1:8080"
TERMINAL = "ESN-ZX-0001"
PERIOD_MS = 5000
# 采集周期带一点容差：socat 与模拟器都有调度抖动，验收判的是「稳定在 5 秒」，
# 不是「精确到毫秒」。超过这个容差才算漂移。
PERIOD_TOLERANCE_MS = 700


def api(method: str, path: str, body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(SIM + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=5) as resp:
        return json.loads(resp.read())


def write_points(bus: str, slave: int, points: dict[str, int]) -> None:
    body = {"points": [{"code": code, "value": v} for code, v in points.items()]}
    api("PUT", f"/api/v1/reg/{TERMINAL}/{bus}/{slave}", body)


def inject(bus: str, slave: int, mode: str, count: int = 200, code: int | None = None) -> None:
    body: dict = {"mode": mode, "count": count}
    if code is not None:
        body["exceptionCode"] = code
    api("POST", f"/api/v1/fault/{TERMINAL}/{bus}/{slave}", body)


def clear_faults(bus: str) -> None:
    api("DELETE", f"/api/v1/fault/{TERMINAL}/{bus}")


def num(v) -> float | None:
    """日志里的数值可能是 int 也可能是 float（JSON 编码按值本身决定），统一按浮点比。"""
    return None if v is None else float(v)


def parse_line(line: str) -> dict:
    """日志是 Zerolog 的 JSON 单行；非 JSON 行（启动期提示）直接忽略。"""
    try:
        rec = json.loads(line)
    except json.JSONDecodeError:
        return {}
    return rec if isinstance(rec, dict) else {}


def run_agent(agent: str, config: str, ports: str, seconds: float, level: str = "debug") -> str:
    """起一次 edge-agent，跑 seconds 秒后发 SIGTERM，返回它写出的全部日志。"""
    proc = subprocess.Popen(
        [agent, "-config", config, "-ports", ports, "-log-level", level],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        time.sleep(seconds)
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            out, _ = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            out, _ = proc.communicate()
    return out


def readings(log: str, bus: str | None = None) -> list[dict]:
    rows = []
    for line in log.splitlines():
        rec = parse_line(line)
        if rec.get("msg") != "测点读数":
            continue
        if bus is not None and rec.get("busId") != bus:
            continue
        rows.append(rec)
    return rows


def batches(log: str, bus: str | None = None) -> list[dict]:
    rows = []
    for line in log.splitlines():
        rec = parse_line(line)
        if rec.get("msg") != "采集周期完成":
            continue
        if bus is not None and rec.get("busId") != bus:
            continue
        rows.append(rec)
    return rows


def latest(rows: list[dict], point_id: str) -> dict | None:
    hit = [r for r in rows if r.get("pointId") == point_id]
    return hit[-1] if hit else None


def expect(cond: bool, name: str, detail: str, results: list[dict]) -> None:
    results.append({"name": name, "passed": bool(cond), "detail": detail})
    mark = "ok  " if cond else "FAIL"
    print(f"  {mark} {name}：{detail}")


def case_values(agent: str, config: str, ports: str, results: list[dict]) -> None:
    """14 点工程值与模拟器设定值一致，含 32 位累计量高低字拼装。"""
    expected = {
        "motor_temp": 62.5,      # 625 × 0.1
        "motor_vib": 2.35,       # 235 × 0.01
        "ia": 35.2,              # 3520 × 0.01
        "ib": 34.8,              # 3480 × 0.01
        "ic": 36.1,              # 3610 × 0.01
        "uab": 382.4,            # 3824 × 0.1
        "p_total": 21.48,        # 2148 × 0.01
        "ep_forward": 12345.67,  # 0x0012D687 = 1234567 × 0.01，高字 0x0012 在前
        "env_temp": 28.4,        # 284 × 0.1
        "env_humi": 56.5,        # 565 × 0.1
        "door": 0.0,
        "estop": 0.0,
        "light_curtain": 0.0,
        "air_pressure": 0.62,    # 620 × 0.001
    }
    for bus in ("A", "B"):
        write_points(bus, 1, {"motor_temp": 625, "motor_vib": 235})
        write_points(bus, 2, {"ia": 3520, "ib": 3480, "ic": 3610, "uab": 3824,
                              "p_total": 2148, "ep_forward": 0x0012D687})
        write_points(bus, 3, {"env_temp": 284, "env_humi": 565})
        write_points(bus, 4, {"door": 0, "estop": 0, "light_curtain": 0})
        write_points(bus, 5, {"air_pressure": 620})

    log = run_agent(agent, config, ports, seconds=7)
    rows = readings(log)
    seen: dict[str, int] = {}
    bad: list[str] = []
    for bus in ("A", "B"):
        bus_rows = [r for r in rows if r.get("busId") == bus]
        latest_by_code = {}
        for r in bus_rows:
            latest_by_code[r["pointId"].rsplit(".", 1)[1]] = r
        seen[bus] = len(latest_by_code)
        if len(latest_by_code) != 14:
            bad.append(f"{bus} 总线测点数 {len(latest_by_code)}")
        for code, want in expected.items():
            r = latest_by_code.get(code)
            if r is None:
                bad.append(f"{bus}.{code} 缺失")
                continue
            if r["quality"] != 0:
                bad.append(f"{bus}.{code} 质量码 {r['quality']}")
                continue
            got = num(r["value"])
            if abs(got - want) > 1e-9:
                bad.append(f"{bus}.{code} = {got}，期望 {want}")
    expect(not bad, "两条总线各 14 点工程值与设定值一致（含 uint32 高低字）",
           "；".join(bad) if bad else f"A 总线 {seen.get('A')} 点、B 总线 {seen.get('B')} 点全部一致",
           results)


def case_period(agent: str, config: str, ports: str, results: list[dict]) -> None:
    """采集周期稳定 5 秒；同批测点共享同一 collectTime。"""
    log = run_agent(agent, config, ports, seconds=17)
    rows = batches(log, bus="A")
    stamps = sorted({int(r["collectTime"]) for r in rows})
    gaps = [b - a for a, b in zip(stamps, stamps[1:])]
    drift = [g for g in gaps if abs(g - PERIOD_MS) > PERIOD_TOLERANCE_MS]
    expect(len(stamps) >= 3 and not drift, "采集周期稳定 5 秒",
           f"周期数 {len(stamps)}，间隔 {gaps} ms" if drift or len(stamps) < 3
           else f"{len(stamps)} 个周期，间隔 {gaps} ms",
           results)

    # 同批：一个 collectTime 下 14 个测点。
    per_batch: dict[str, int] = {}
    for r in readings(log, bus="A"):
        per_batch[r["collectTime"]] = per_batch.get(r["collectTime"], 0) + 1
    ok = bool(per_batch) and all(n == 14 for n in per_batch.values())
    expect(ok, "同批 14 个测点共享同一 collectTime",
           f"每批测点数 {sorted(per_batch.values())}", results)


def case_quality(agent: str, config: str, ports: str, results: list[dict]) -> None:
    """超时 → 2、异常码 → 1、CRC 错 → 3，三者值为空；失败不沿用旧值。"""
    clear_faults("A")
    clear_faults("B")
    for bus in ("A", "B"):
        write_points(bus, 1, {"motor_temp": 700, "motor_vib": 300})
    expect_rows = run_agent(agent, config, ports, seconds=6)
    before = readings(expect_rows, bus="A")
    had_good = latest(before, "A.1.motor_temp")
    expect(had_good is not None and had_good["quality"] == 0 and num(had_good["value"]) == 70,
           "故障注入前先读到正常值",
           f"A.1.motor_temp={had_good['value'] if had_good else '缺失'}",
           results)

    inject("A", 1, "timeout")
    inject("A", 3, "exception", code=2)
    inject("A", 5, "crc")
    log = run_agent(agent, config, ports, seconds=8)
    rows = readings(log, bus="A")

    checks = {
        "A.1.motor_temp": (2, "超时"),
        "A.3.env_temp": (1, "异常码"),
        "A.5.air_pressure": (3, "CRC 错"),
    }
    problems = []
    detail = []
    for point_id, (want_q, label) in checks.items():
        rec = latest(rows, point_id)
        if rec is None:
            problems.append(f"{point_id} 未出现在日志里（失败点不得省略）")
            continue
        detail.append(f"{point_id}({label}) quality={rec['quality']} value={'null' if rec['value'] is None else rec['value']}")
        if rec["quality"] != want_q:
            problems.append(f"{point_id} 质量码 {rec['quality']}，期望 {want_q}")
        if rec["value"] is not None:
            problems.append(f"{point_id} 值为 {rec['value']}，失败时必须为空（不得沿用旧值 70）")
    expect(not problems, "超时/异常码/CRC 错分别标记 2/1/3 且值为空",
           "；".join(problems) if problems else "；".join(detail), results)

    b_rows = readings(log, bus="B")
    b_bad = [r for r in b_rows if r.get("quality") != 0]
    expect(not b_bad, "A 总线故障不影响 B 总线",
           f"B 总线 {len(b_rows)} 条读数全部 GOOD" if not b_bad else f"B 有 {len(b_bad)} 条非 GOOD",
           results)
    clear_faults("A")


def case_bus_independence(agent: str, config: str, ports: str, results: list[dict]) -> None:
    """两条总线各一个协程：A 总线五个从站全部超时，B 总线周期不受影响。"""
    for slave in (1, 2, 3, 4, 5):
        inject("A", slave, "timeout", count=1000)
    log = ""
    try:
        log = run_agent(agent, config, ports, seconds=16)
    finally:
        clear_faults("A")

    b_stamps = sorted({int(r["collectTime"]) for r in batches(log, bus="B")})
    a_stamps = sorted({int(r["collectTime"]) for r in batches(log, bus="A")})
    b_gaps = [y - x for x, y in zip(b_stamps, b_stamps[1:])]

    # 判据两半：B 的间隔始终在 5 秒容差内（没被拖慢），且 A 同一时段明显更少周期
    # （注入确实生效，否则「B 正常」可能只是因为 A 根本没被卡住）。
    # A 每个从站耗尽 2 次重试 = 3 × 1000 ms，5 个从站一轮约 15 秒，比 B 的两个周期还长。
    b_ok = len(b_stamps) >= 3 and all(abs(g - PERIOD_MS) <= PERIOD_TOLERANCE_MS for g in b_gaps)
    a_slower = len(a_stamps) < len(b_stamps)
    expect(b_ok and a_slower, "A 总线阻塞时 B 总线仍按 5 秒周期采集",
           f"{len(b_stamps)} 个 B 周期，间隔 {b_gaps} ms；"
           f"同一时段 A 只完成 {len(a_stamps)} 个周期（每从站耗尽重试）", results)


def case_out_of_range(agent: str, config: str, ports: str, results: list[dict]) -> None:
    """超量程保留数值并标 5，不置空。"""
    clear_faults("A")
    write_points("A", 1, {"motor_temp": 2500, "motor_vib": 6000})  # 250 ℃ / 60 mm/s
    log = run_agent(agent, config, ports, seconds=6)
    rows = readings(log, bus="A")
    problems = []
    for point_id, want in (("A.1.motor_temp", "250"), ("A.1.motor_vib", "60")):
        rec = latest(rows, point_id)
        if rec is None:
            problems.append(f"{point_id} 缺失")
            continue
        if rec["quality"] != 5:
            problems.append(f"{point_id} 质量码 {rec['quality']}，期望 5")
        if num(rec["value"]) != float(want):
            problems.append(f"{point_id} 值 {rec['value']}，期望保留 {want}")
    expect(not problems, "超量程保留数值并标记 5", "；".join(problems) if problems else
           "motor_temp=250℃ / motor_vib=60mm/s 均保留原值，质量码 5", results)


def case_reject_bad_config(agent: str, config: str, ports: str, results: list[dict], tmp: Path) -> None:
    """配置非法时拒绝启动（非 0 退出）并输出具体字段。"""
    broken = tmp / "broken.json"
    data = json.loads(Path(config).read_text(encoding="utf-8"))
    data["buses"][0]["devices"][0]["blocks"][0]["points"][0]["scale"] = 0
    broken.write_text(json.dumps(data, ensure_ascii=False), encoding="utf-8")

    proc = subprocess.run([agent, "-config", str(broken), "-ports", ports],
                          capture_output=True, text=True, timeout=20)
    out = proc.stdout + proc.stderr
    issue = next((ln for ln in out.splitlines() if "V-05" in ln), "")
    ok = proc.returncode != 0 and "V-05" in out and "scale" in out
    expect(ok, "配置非法时拒绝启动并指出字段",
           f"退出码 {proc.returncode}；{issue.strip() or '未输出 V-05 行'}", results)


def main() -> int:
    parser = argparse.ArgumentParser(description="005 验收")
    parser.add_argument("--agent", required=True, help="edge-agent 可执行文件")
    parser.add_argument("--config", default="config/terminals/ESN-ZX-0001.json")
    parser.add_argument("--ports", default=".pty/agent-ports.json")
    parser.add_argument("--evidence", default="docs/evidence/005")
    args = parser.parse_args()

    evidence = Path(args.evidence)
    evidence.mkdir(parents=True, exist_ok=True)
    tmp = evidence / "_tmp"
    tmp.mkdir(exist_ok=True)

    results: list[dict] = []
    case_values(args.agent, args.config, args.ports, results)
    case_period(args.agent, args.config, args.ports, results)
    case_quality(args.agent, args.config, args.ports, results)
    case_bus_independence(args.agent, args.config, args.ports, results)
    case_out_of_range(args.agent, args.config, args.ports, results)
    case_reject_bad_config(args.agent, args.config, args.ports, results, tmp)

    passed = all(r["passed"] for r in results)
    payload = {
        "task": "005 采集轮询与换算",
        "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "terminal": TERMINAL,
        "passed": passed,
        "cases": results,
    }
    (evidence / "acceptance.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (tmp / "broken.json").unlink(missing_ok=True)
    tmp.rmdir()

    print(f"\n验收{'通过' if passed else '未通过'}，证据：{evidence / 'acceptance.json'}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())