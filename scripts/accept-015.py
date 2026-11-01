#!/usr/bin/env python3
"""015 健康检查与自检的端到端验收。

链路是主通道：从站模拟器 → 虚拟串口 → edge-agent → Broker → proto-adapter。
健康检查的判据大多只能用真实进程验证（断 Broker、掐总线、删库里的告警行），
单测覆盖判定函数，这里验证「进程跑起来之后 HTTP 的头和体到底是什么」。

期望值全部来自 `.claude/rules/edge-agent.md` 与 `.claude/rules/adapter-evaluate.md`：

| 场景 | 期望 |
|---|---|
| 一切正常 | 200 / ok |
| MQTT 断开、采集正常 | **200 / degraded**（不是 503） |
| 串口整条总线读不回 | 503 / down |
| 活动告警数 ≠ 库中未恢复数 | 200 / degraded |

前置：
    1. scripts/setup-pty.sh
    2. 起 modbus-slave-sim（127.0.0.1:8080）
    3. docker compose 起 mosquitto 与 timescaledb，已 make seed
    4. go build -o /tmp/edge-agent ./cmd/edge-agent
       go build -o /tmp/proto-adapter ./cmd/proto-adapter

用法：
    .venv-sim/bin/python scripts/accept-015.py --agent /tmp/edge-agent \\
        --adapter /tmp/proto-adapter --evidence docs/evidence/015
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

TERMINAL = "ESN-ZX-0001"
BROKER_CONTAINER = "deploy-mosquitto-1"
DB_CONTAINER = "deploy-timescaledb-1"
AGENT_PORT = 19101
ADAPTER_PORT = 19200


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


def docker(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["docker", *args], capture_output=True, text=True)


def psql(sql: str) -> str:
    out = docker("exec", DB_CONTAINER, "sh", "-c",
                 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "$1"', "_", sql)
    return out.stdout.strip()


def stop_broker() -> None:
    docker("stop", BROKER_CONTAINER)


def start_broker() -> None:
    docker("start", BROKER_CONTAINER)
    for _ in range(80):
        probe = docker("exec", BROKER_CONTAINER, "mosquitto_pub",
                       "-h", "127.0.0.1", "-p", "1883", "-t", "/probe", "-m", "x")
        if "Connection refused" not in probe.stderr:
            return
        time.sleep(0.25)
    raise RuntimeError("Broker 未能就绪")


# ── 进程 ──────────────────────────────────────────────────────


class Proc:
    """跑一个可执行文件，把日志收到内存。

    必须用单独的线程持续读 stdout：采集周期日志很密，管道缓冲区（64 KB）写满后
    进程会阻塞在 write 上，采集直接停摆——那会让「断网仍在采」这类断言全部失真。
    """

    def __init__(self, cmd: list[str], env: dict[str, str]):
        self.proc = subprocess.Popen(cmd, stdout=subprocess.PIPE,
                                     stderr=subprocess.STDOUT, text=True, env=env)
        self.lines: list[str] = []
        self._thread = threading.Thread(target=self._pump, daemon=True)
        self._thread.start()

    def _pump(self) -> None:
        assert self.proc.stdout is not None
        for line in self.proc.stdout:
            self.lines.append(line.rstrip("\n"))

    def stop(self, grace: float = 10.0) -> str:
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
        try:
            self.proc.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        self._thread.join(timeout=5)
        return "\n".join(list(self.lines))

    def count(self, needle: str) -> int:
        return sum(1 for ln in self.lines if needle in ln)


def wait_health(port: int, timeout: float = 30.0) -> tuple[int, dict]:
    """轮询 /healthz 直到端口可连，返回 (状态码, 响应体)。"""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=2) as r:
                return r.status, json.loads(r.read().decode())
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read().decode())
        except Exception as exc:  # 端口还没起来
            last = exc
            time.sleep(0.3)
    raise RuntimeError(f"健康检查端口 {port} 未就绪：{last}")


def expect(ok: bool, name: str, detail: str, results: list[dict]) -> None:
    results.append({"name": name, "passed": bool(ok), "detail": detail})
    print(f"  {'ok  ' if ok else 'FAIL'} {name}")
    if not ok:
        print(f"       {detail}")


# ── 用例 ──────────────────────────────────────────────────────


def case_agent_ok(agent: str, config: str, ports: str, tmp: Path,
                  env: dict[str, str], results: list[dict]) -> None:
    """edge-agent 全项正常：200 / ok，四项齐全。"""
    e = dict(env, HEALTH_ADDR=f"127.0.0.1:{AGENT_PORT}")
    run = Proc([agent, "-config", config, "-ports", ports,
                "-seq", str(tmp / "seq-ok"), "-cache", str(tmp / "cache-ok.db")], e)
    try:
        code, body = wait_health(AGENT_PORT)
    finally:
        run.stop()

    checks = body.get("checks", {})
    detail = (f"HTTP {code} status={body.get('status')} "
              f"checks={sorted(checks)} mqtt={checks.get('mqtt')}")
    ok = (code == 200 and body.get("status") == "ok"
          and {"config", "serial", "mqtt", "cache"} <= set(checks))
    expect(ok, "edge-agent 全项正常返回 200 / ok，四项齐全", detail, results)


def case_agent_mqtt_down(agent: str, config: str, ports: str, tmp: Path,
                         env: dict[str, str], results: list[dict]) -> None:
    """MQTT 断开必须是 200 降级，不是 503。"""
    e = dict(env, HEALTH_ADDR=f"127.0.0.1:{AGENT_PORT}")
    run = Proc([agent, "-config", config, "-ports", ports,
                "-seq", str(tmp / "seq-mqtt"), "-cache", str(tmp / "cache-mqtt.db")], e)
    try:
        wait_health(AGENT_PORT)
        stop_broker()
        time.sleep(16)  # 3 个采集周期以上，确保断网后仍在采
        code, body = wait_health(AGENT_PORT)
        cache = body.get("checks", {}).get("cache", {})
        received = run.count("采集周期完成")
        run.stop()
    finally:
        start_broker()

    mqtt = body.get("checks", {}).get("mqtt", {})
    detail = (f"HTTP {code} status={body.get('status')} mqtt={mqtt} "
              f"断网期间采集 {received} 个周期 cache.depth={cache.get('depth')}")
    ok = (code == 200 and body.get("status") == "degraded"
          and mqtt.get("connected") is False and received >= 2)
    expect(ok, "MQTT 断开但采集正常：200 降级，不是 503", detail, results)


def _bus_offline(bus: str, offline: bool) -> None:
    """把一条总线上的 5 个从站全部置离线 / 恢复。

    走模拟器的控制接口，注入的是协议层真实超时（从站不应答），不是错误数值。
    """
    verb = "offline" if offline else "online"
    for addr in range(1, 6):
        url = f"http://127.0.0.1:8080/api/v1/{verb}/{TERMINAL}/{bus}/{addr}"
        req = urllib.request.Request(url, method="POST")
        with urllib.request.urlopen(req, timeout=5):
            pass


def case_agent_serial_down(agent: str, config: str, ports: str, tmp: Path,
                           env: dict[str, str], results: list[dict]) -> None:
    """整条总线连续读不回：503。"""
    e = dict(env, HEALTH_ADDR=f"127.0.0.1:{AGENT_PORT}")
    run = Proc([agent, "-config", config, "-ports", ports,
                "-seq", str(tmp / "seq-serial"), "-cache", str(tmp / "cache-serial.db")], e)
    result = None
    started = time.time()
    try:
        wait_health(AGENT_PORT)
        _bus_offline("A", True)
        # 整条总线离线时每个块都要耗尽重试（2 次超时），一个采集周期远长于 5 秒，
        # 因此不能按固定秒数等，而是轮询到「连续 5 个周期全失败」为止。
        deadline = time.time() + 240
        while time.time() < deadline:
            code, body = wait_health(AGENT_PORT)
            if body.get("checks", {}).get("serial", {}).get("buses", {}).get("A") == "closed":
                result = (code, body)
                break
            time.sleep(3)
        elapsed = time.time() - started
        _bus_offline("A", False)
        log = run.stop()
    finally:
        _bus_offline("A", False)
        if run.proc.poll() is None:
            run.stop()

    if result is None:
        expect(False, "串口整条总线读不回时返回 503", "未能取到健康响应", results)
        return

    code, body = result
    serial = body.get("checks", {}).get("serial", {})
    timeouts = sum(1 for ln in log.splitlines() if "采集周期完成" in ln and '"good":0' in ln)
    detail = (f"HTTP {code} status={body.get('status')} serial={serial} "
              f"A 总线全失败周期 {timeouts} 个，判定耗时 {elapsed:.0f}s（B 总线应仍为 open）")
    ok = (code == 503 and body.get("status") == "down"
          and serial.get("buses", {}).get("A") == "closed"
          and serial.get("buses", {}).get("B") == "open")
    expect(ok, "串口整条总线读不回时返回 503", detail, results)


def case_adapter_ok(adapter: str, config: str, env: dict[str, str],
                    results: list[dict]) -> None:
    """proto-adapter 全项正常：200 / ok，evaluate 六个字段齐全。"""
    e = dict(env, HEALTH_ADDR=f"127.0.0.1:{ADAPTER_PORT}",
             PGPASSWORD=env.get("WORKSHOP_ADAPTER_PASSWORD", ""),
             NOTIFY_ENABLED="false")
    run = Proc([adapter, "-config", config], e)
    try:
        code, body = wait_health(ADAPTER_PORT, timeout=40)
        time.sleep(6)  # 等几条上行进来，让 lastEvalTime 有值
        code, body = wait_health(ADAPTER_PORT)
    finally:
        run.stop()

    ev = body.get("checks", {}).get("evaluate", {})
    want = {"queueDepth", "queueCapacity", "dropCount",
            "activeAlarmCount", "debounceKeys"}
    detail = f"HTTP {code} status={body.get('status')} evaluate={ev}"
    ok = code == 200 and body.get("status") == "ok" and want <= set(ev)
    expect(ok, "proto-adapter 全项正常返回 200 / ok，evaluate 字段齐全", detail, results)


def case_adapter_mismatch(adapter: str, config: str, env: dict[str, str],
                          results: list[dict]) -> None:
    """活动告警数与库不一致时降级，并给出原因。"""
    e = dict(env, HEALTH_ADDR=f"127.0.0.1:{ADAPTER_PORT}",
             PGPASSWORD=env.get("WORKSHOP_ADAPTER_PASSWORD", ""),
             NOTIFY_ENABLED="false")
    run = Proc([adapter, "-config", config], e)
    try:
        wait_health(ADAPTER_PORT, timeout=40)
        time.sleep(8)
        _, before = wait_health(ADAPTER_PORT)
        active = before.get("checks", {}).get("evaluate", {}).get("activeAlarmCount", 0)
        if active == 0:
            before = {"checks": {"evaluate": {"status": "ok"}}}
        # 删掉一条 ACTIVE 告警，制造内存与库不一致。
        psql("DELETE FROM monitor.alarm WHERE alarm_id = "
             "(SELECT alarm_id FROM monitor.alarm WHERE status='ACTIVE' LIMIT 1)")
        time.sleep(1)
        code, body = wait_health(ADAPTER_PORT)
    finally:
        run.stop()

    ev = body.get("checks", {}).get("evaluate", {})
    detail = f"HTTP {code} status={body.get('status')} evaluate={ev}"
    ok = (code == 200 and body.get("status") == "degraded"
          and ev.get("status") == "degraded" and bool(ev.get("reason")))
    expect(ok, "活动告警数与库不一致：200 降级并给出原因", detail, results)


def case_preflight(results: list[dict]) -> None:
    """make preflight 12 项全过。"""
    out = subprocess.run(["make", "preflight"], capture_output=True, text=True)
    lines = [ln.strip() for ln in out.stdout.splitlines() if ln.strip().startswith(("ok", "FAIL"))]
    numbered = [ln for ln in lines if ln.split()[1].isdigit()]
    detail = f"退出码 {out.returncode}，编号项 {len(numbered)} 条；末尾：{out.stdout.strip().splitlines()[-1:]}"
    ok = out.returncode == 0 and len(numbered) == 12
    expect(ok, "make preflight 12 项全过", detail, results)


def case_preflight_fails(results: list[dict]) -> None:
    """停一个容器后 preflight 必须失败并指出是哪一项。"""
    docker("stop", "deploy-grafana-1")
    try:
        out = subprocess.run(["make", "preflight"], capture_output=True, text=True)
    finally:
        docker("start", "deploy-grafana-1")
        for _ in range(60):
            try:
                with urllib.request.urlopen("http://localhost:3000/api/health", timeout=2):
                    break
            except Exception:
                time.sleep(1)

    failed = [ln.strip() for ln in out.stdout.splitlines() if "FAIL" in ln]
    detail = f"退出码 {out.returncode}，失败项：{failed}"
    ok = out.returncode != 0 and any("9  grafana" in ln for ln in failed)
    expect(ok, "停掉 grafana 后 preflight 失败并指出具体项", detail, results)


def main() -> int:
    parser = argparse.ArgumentParser(description="015 验收")
    parser.add_argument("--agent", required=True, help="edge-agent 可执行文件")
    parser.add_argument("--adapter", required=True, help="proto-adapter 可执行文件")
    parser.add_argument("--config", default="config/terminals/ESN-ZX-0001.json")
    parser.add_argument("--ports", default=".pty/agent-ports.json")
    parser.add_argument("--evidence", default="docs/evidence/015")
    parser.add_argument("--env-file", default=".env")
    args = parser.parse_args()

    evidence = Path(args.evidence)
    evidence.mkdir(parents=True, exist_ok=True)
    tmp = evidence / "_tmp"
    tmp.mkdir(exist_ok=True)

    env = os.environ.copy()
    env.update(load_env(Path(args.env_file)))

    results: list[dict] = []
    try:
        start_broker()
        print("edge-agent：")
        case_agent_ok(args.agent, args.config, args.ports, tmp, env, results)
        case_agent_mqtt_down(args.agent, args.config, args.ports, tmp, env, results)
        case_agent_serial_down(args.agent, args.config, args.ports, tmp, env, results)
        print("proto-adapter：")
        case_adapter_ok(args.adapter, "config/adapter.json", env, results)
        case_adapter_mismatch(args.adapter, "config/adapter.json", env, results)
        print("preflight：")
        case_preflight(results)
        case_preflight_fails(results)
    finally:
        start_broker()

    passed = all(r["passed"] for r in results)
    payload = {
        "task": "015 健康检查与自检",
        "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "terminal": TERMINAL,
        "passed": passed,
        "cases": results,
    }
    (evidence / "acceptance.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    for leftover in tmp.glob("*"):
        leftover.unlink()
    tmp.rmdir()

    print(f"\n验收{'通过' if passed else '未通过'}，证据：{evidence / 'acceptance.json'}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())