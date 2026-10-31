#!/usr/bin/env python3
"""006 上行与断连通知的端到端验收。

链路是主通道：从站模拟器 → 虚拟串口 → edge-agent → Broker → 订阅端。
订阅端用锁定的 Mosquitto 镜像里的 ``mosquitto_sub``（``docker exec``），
不引入新的 Python 依赖。

期望值全部是接口规范里的字面量，不读实现里的常量：
报文头 8 个规范字段、``msgId = {esn}-{seq:08d}``、序号重启不归零、
同一批共享 ``seq``、失败点 ``value=null``、LWT 在 3 个上报周期内到达。

前置：
    1. scripts/setup-pty.sh
    2. 起 modbus-slave-sim（127.0.0.1:8080）
    3. docker compose 起 mosquitto（含 passwd 与 ACL）
    4. go build -o /tmp/edge-agent ./cmd/edge-agent

用法：
    .venv-sim/bin/python scripts/accept-006.py --agent /tmp/edge-agent \\
        --config config/terminals/ESN-ZX-0001.json --ports .pty/agent-ports.json \\
        --evidence docs/evidence/006
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
import urllib.request
from pathlib import Path

SIM = "http://127.0.0.1:8080"
TERMINAL = "ESN-ZX-0001"
PERIOD_MS = 5000
# LWT 的判据是「3 个上报周期内」（接口规范 §6.5），即 15 秒。
LWT_DEADLINE_MS = 3 * PERIOD_MS
BROKER_CONTAINER = "deploy-mosquitto-1"


# ── 环境与进程 ────────────────────────────────────────────────


def load_env(path: Path) -> dict[str, str]:
    """读 .env 的 KEY=VALUE。不 source，.env 里可能有命令替换。"""
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


def agent_env(base: dict[str, str], scheme: str) -> dict[str, str]:
    env = os.environ.copy()
    env.update(base)
    env["MQTT_TOPIC_SCHEME"] = scheme
    return env


class Subscriber:
    """在 Broker 侧订阅一个通配主题，后台线程收集 (收到时刻, 主题, 载荷)。

    默认用 ``-R`` 跳过 Retain 的旧消息：状态主题是 Retain 的，上一次运行留下的
    OFFLINE 会在订阅瞬间送上门，混进本轮统计会把「重启不归零」判成乱序。
    保留语义由 ``case_status_retained`` 单独验。
    """

    def __init__(self, env: dict[str, str], topic_filter: str, qos: int = 1,
                 skip_retained: bool = True):
        self.messages: list[tuple[int, str, str]] = []
        self._lock = threading.Lock()
        flag = " -R" if skip_retained else ""
        cmd = [
            "docker", "exec", "-e", "PW=" + env.get("MQTT_WORKSHOP_ADAPTER_PASSWORD", ""),
            BROKER_CONTAINER, "sh", "-c",
            f'mosquitto_sub -h 127.0.0.1 -u workshop_adapter -P "$PW" '
            f'-t "{topic_filter}" -v -q {qos}{flag}',
        ]
        self.proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True, bufsize=1)
        self.thread = threading.Thread(target=self._pump, daemon=True)
        self.thread.start()
        time.sleep(1.2)  # 等订阅在 Broker 上确认，避免漏掉第一秒

    def _pump(self) -> None:
        assert self.proc.stdout is not None
        for line in self.proc.stdout:
            line = line.rstrip("\n")
            if not line:
                continue
            topic, _, payload = line.partition(" ")
            with self._lock:
                self.messages.append((int(time.time() * 1000), topic, payload))

    def snapshot(self) -> list[tuple[int, str, str]]:
        with self._lock:
            return list(self.messages)

    def stop(self) -> None:
        self.proc.send_signal(signal.SIGTERM)
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()


class StandaloneSubscriber(Subscriber):
    """独立容器里的订阅端，用于「Broker 重启」这类会连带杀掉 `docker exec` 的用例。

    用锁定镜像本身起一次性容器并共享宿主网络：Broker 容器重启时它不受影响，
    否则订阅端和被观察对象一起消失，看到的是「重启后没有消息」而不是真相。
    """

    def __init__(self, env: dict[str, str], topic_filter: str, qos: int = 1):
        self.messages: list[tuple[int, str, str]] = []
        self._lock = threading.Lock()
        cmd = [
            "docker", "run", "--rm", "-i", "--network", "host",
            f"eclipse-mosquitto:{env.get('MOSQUITTO_VERSION', '2.0.20')}",
            "mosquitto_sub", "-h", "127.0.0.1", "-p", "1883",
            "-u", "workshop_adapter",
            "-P", env.get("MQTT_WORKSHOP_ADAPTER_PASSWORD", ""),
            "-t", topic_filter, "-v", "-q", str(qos), "-R",
        ]
        self.proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, text=True, bufsize=1)
        self.thread = threading.Thread(target=self._pump, daemon=True)
        self.thread.start()
        time.sleep(1.5)


def run_agent(agent: str, config: str, ports: str, seq: Path, seconds: float,
              env: dict[str, str], level: str = "info") -> tuple[str, subprocess.Popen]:
    """起一次 edge-agent 跑 seconds 秒后 SIGTERM，返回全部日志。"""
    proc = subprocess.Popen(
        [agent, "-config", config, "-ports", ports, "-seq", str(seq), "-log-level", level],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env,
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
    return out, proc


# ── 模拟器控制 ────────────────────────────────────────────────


def api(method: str, path: str, body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(SIM + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=5) as resp:
        return json.loads(resp.read())


def write_points(bus: str, slave: int, points: dict[str, int]) -> None:
    api("PUT", f"/api/v1/reg/{TERMINAL}/{bus}/{slave}",
        {"points": [{"code": c, "value": v} for c, v in points.items()]})


def inject(bus: str, slave: int, mode: str, count: int = 200) -> None:
    api("POST", f"/api/v1/fault/{TERMINAL}/{bus}/{slave}", {"mode": mode, "count": count})


def clear_faults(bus: str) -> None:
    api("DELETE", f"/api/v1/fault/{TERMINAL}/{bus}")


# ── 报文解析 ──────────────────────────────────────────────────


def parse(messages: list[tuple[int, str, str]], kind: str | None = None) -> list[dict]:
    """把订阅到的原始行解析成 {receivedAt, topic, header, body}，可按 msgType 过滤。"""
    out = []
    for received_at, topic, payload in messages:
        try:
            env = json.loads(payload)
        except json.JSONDecodeError:
            continue
        rec = {"receivedAt": received_at, "topic": topic,
               "header": env.get("header", {}), "body": env.get("body", {})}
        if kind is None or rec["header"].get("msgType") == kind:
            out.append(rec)
    return out


def expect(cond: bool, name: str, detail: str, results: list[dict]) -> None:
    results.append({"name": name, "passed": bool(cond), "detail": detail})
    print(f"  {'ok  ' if cond else 'FAIL'} {name}：{detail}")


SPEC_HEADER = ["schemaVersion", "msgType", "msgId", "esn", "seq",
               "collectTime", "sendTime", "backfill"]


# ── 用例 ──────────────────────────────────────────────────────


def case_three_kinds(agent: str, config: str, ports: str, tmp: Path,
                     envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """三类消息都能订阅到；报文头 8 个规范字段齐全、msgId 格式正确。"""
    sub = Subscriber(envs["grid"], "/edge/+/+/up")
    try:
        write_points("A", 1, {"motor_temp": 625, "motor_vib": 235})
        write_points("B", 1, {"motor_temp": 625, "motor_vib": 235})
        run_agent(agent, config, ports, tmp / "seq-a.state", 12, envs["grid"])
    finally:
        sub.stop()

    msgs = parse(sub.snapshot())
    kinds = {m["header"].get("msgType") for m in msgs}
    topics = {m["topic"] for m in msgs}
    problems = []
    for want in ("DATA", "STATUS"):
        if want not in kinds:
            problems.append(f"未收到 {want}")
    if "/edge/ESN-ZX-0001/data/up" not in topics:
        problems.append("数据主题不是 /edge/ESN-ZX-0001/data/up")
    if "/edge/ESN-ZX-0001/status/up" not in topics:
        problems.append("状态主题不是 /edge/ESN-ZX-0001/status/up")
    expect(not problems, "数据与状态两类消息分别订阅到，主题随方案展开",
           "；".join(problems) if problems else
           f"收到 {len(msgs)} 条，主题 {sorted(topics)}", results)

    data = parse(sub.snapshot(), "DATA")
    if not data:
        expect(False, "报文头 8 个规范字段齐全", "没有 DATA 报文可查", results)
        return
    header = data[0]["header"]
    missing = [f for f in SPEC_HEADER if f not in header]
    msg_id_ok = re.fullmatch(r"ESN-ZX-0001-\d{8,}", str(header.get("msgId", ""))) is not None
    esn_ok = header.get("esn") == "ESN-ZX-0001"
    schema_ok = header.get("schemaVersion") == "2.0"
    expect(not missing and msg_id_ok and esn_ok and schema_ok,
           "报文头 8 个规范字段齐全",
           f"缺 {missing}" if missing else
           f"schemaVersion={header['schemaVersion']} msgId={header['msgId']} esn={header['esn']} "
           f"seq={header['seq']} collectTime={header['collectTime']} "
           f"sendTime={header['sendTime']} backfill={header['backfill']}", results)


def case_points_share_seq(agent: str, config: str, ports: str, tmp: Path,
                          envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """一次采集一条报文，14 个测点共享同一 seq；yc+yx 等于 stat.total。"""
    sub = Subscriber(envs["grid"], "/edge/+/+/up")
    try:
        run_agent(agent, config, ports, tmp / "seq-b.state", 12, envs["grid"])
    finally:
        sub.stop()

    data = parse(sub.snapshot(), "DATA")
    if not data:
        expect(False, "同一次采集的多个测点共享同一 seq", "没有 DATA 报文", results)
        return
    problems = []
    for m in data:
        body = m["body"]
        values = list(body.get("yc", [])) + list(body.get("yx", []))
        if len(values) != body.get("stat", {}).get("total"):
            problems.append(f"seq={m['header']['seq']} yc+yx={len(values)} "
                            f"stat.total={body.get('stat', {}).get('total')}")
    per_seq: dict[int, int] = {}
    for m in data:
        per_seq.setdefault(m["header"]["seq"], 0)
        per_seq[m["header"]["seq"]] += len(m["body"].get("yc", [])) + len(m["body"].get("yx", []))
    expect(not problems and all(n == 14 for n in per_seq.values()),
           "同一次采集的多个测点共享同一 seq",
           "；".join(problems) if problems else
           f"{len(data)} 条 DATA，每条 14 点，seq 各不重复：{sorted(per_seq)[:5]}", results)


def case_seq_monotonic(agent: str, config: str, ports: str, tmp: Path,
                       envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """序号单调递增；重启后从最大序号续接，不归零。"""
    seq_file = tmp / "seq-restart.state"
    seq_file.unlink(missing_ok=True)

    sub = Subscriber(envs["grid"], "/edge/+/+/up")
    try:
        run_agent(agent, config, ports, seq_file, 11, envs["grid"])
        first = [m["header"]["seq"] for m in parse(sub.snapshot())]
        run_agent(agent, config, ports, seq_file, 7, envs["grid"])
        all_seqs = [m["header"]["seq"] for m in parse(sub.snapshot())]
    finally:
        sub.stop()

    if not first or len(all_seqs) <= len(first):
        expect(False, "序号单调递增且重启不归零",
               f"两次运行收到的序号数 {len(first)} / {len(all_seqs)}", results)
        return

    second = all_seqs[len(first):]
    monotonic = all(b > a for a, b in zip(all_seqs, all_seqs[1:]))
    continued = min(second) > max(first)
    expect(monotonic and continued, "序号单调递增且重启不归零",
           f"第一次运行末条 seq={max(first)}，重启后首条 seq={min(second)}"
           f"（{'大于' if continued else '不大于'}前次最大值；单调={monotonic}）", results)


def case_send_collect_time(agent: str, config: str, ports: str, tmp: Path,
                           envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """sendTime 由上报段写入，collectTime 沿用 005 的批次开始时刻，两者不混用。"""
    sub = Subscriber(envs["grid"], "/edge/+/+/up")
    try:
        log, _ = run_agent(agent, config, ports, tmp / "seq-c.state", 8, envs["grid"])
    finally:
        sub.stop()

    batch_starts: dict[str, set[int]] = {}
    for line in log.splitlines():
        try:
            rec = json.loads(line)
        except json.JSONDecodeError:
            continue
        if rec.get("msg") != "采集周期完成":
            continue
        batch_starts.setdefault(rec.get("busId", ""), set()).add(int(rec["collectTime"]))

    problems = []
    checked = 0
    for m in parse(sub.snapshot(), "DATA"):
        body, header = m["body"], m["header"]
        bus = body.get("busId", "")
        checked += 1
        if header["collectTime"] not in batch_starts.get(bus, set()):
            problems.append(f"bus {bus} 的 collectTime={header['collectTime']} 不是任何批次开始时刻")
        # collectTime 取批次开始，sendTime 取发布时刻，故 sendTime 不得早于 collectTime，
        # 且同批的 sendTime 与 collectTime 之间是毫秒级延迟，不是 0（0 说明两者被混用）。
        if header["sendTime"] <= header["collectTime"]:
            problems.append(f"bus {bus} sendTime={header['sendTime']} 不晚于 collectTime={header['collectTime']}")
        delay = header["sendTime"] - header["collectTime"]
        if delay > PERIOD_MS:
            problems.append(f"bus {bus} 上报延迟 {delay}ms 超过一个采集周期")
    expect(not problems and checked > 0, "sendTime 独立于 collectTime 且 collectTime 沿用批次时刻",
           "；".join(problems) if problems else
           f"{checked} 条 DATA 的 collectTime 均命中批次开始时刻，sendTime 均晚于它", results)


def case_failed_points(agent: str, config: str, ports: str, tmp: Path,
                       envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """采集失败的点仍在数组里且 value=null，不省略、不沿用旧值。"""
    clear_faults("A")
    write_points("A", 1, {"motor_temp": 625, "motor_vib": 235})
    inject("A", 1, "timeout")
    sub = Subscriber(envs["grid"], "/edge/+/+/up")
    try:
        run_agent(agent, config, ports, tmp / "seq-d.state", 12, envs["grid"])
    finally:
        sub.stop()
        clear_faults("A")

    problems = []
    checked = 0
    for m in parse(sub.snapshot(), "DATA"):
        if m["body"].get("busId") != "A":
            continue
        values = list(m["body"].get("yc", [])) + list(m["body"].get("yx", []))
        if len(values) != 14:
            problems.append(f"seq={m['header']['seq']} 只有 {len(values)} 点")
            continue
        motor = next((p for p in values if p.get("code") == "motor_temp"), None)
        if motor is None:
            problems.append(f"seq={m['header']['seq']} 缺少 motor_temp")
            continue
        checked += 1
        if motor.get("value") is not None:
            problems.append(f"seq={m['header']['seq']} motor_temp 值为 {motor['value']}，应为 null")
        if motor.get("quality") != 2:
            problems.append(f"seq={m['header']['seq']} motor_temp 质量码 {motor.get('quality')}，期望 2")
    expect(not problems and checked > 0, "采集失败的点仍在数组中且 value=null",
           "；".join(problems) if problems else
           f"{checked} 条 A 总线报文均含 14 点，motor_temp 为 null + quality=2", results)


def case_lwt(agent: str, config: str, ports: str, tmp: Path,
             envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """终端异常退出后，订阅方在 3 个上报周期内收到 OFFLINE。"""
    sub = Subscriber(envs["grid"], "/edge/ESN-ZX-0001/status/up")
    proc = subprocess.Popen(
        [agent, "-config", config, "-ports", ports, "-seq", str(tmp / "seq-e.state"),
         "-log-level", "warn"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=envs["grid"],
    )
    try:
        time.sleep(6)
        killed_at = int(time.time() * 1000)
        proc.kill()  # SIGKILL：不走优雅退出，只能靠 LWT
        proc.wait(timeout=5)
        deadline = killed_at + LWT_DEADLINE_MS
        while int(time.time() * 1000) < deadline + 2000:
            hit = [m for m in parse(sub.snapshot(), "STATUS")
                   if m["body"].get("status") == "OFFLINE"
                   and m["body"].get("reason") == "UNEXPECTED_DISCONNECT"
                   and m["receivedAt"] >= killed_at]
            if hit:
                break
            time.sleep(0.2)
    finally:
        if proc.poll() is None:
            proc.kill()
        sub.stop()

    offline = [m for m in parse(sub.snapshot(), "STATUS")
               if m["body"].get("status") == "OFFLINE"
               and m["body"].get("reason") == "UNEXPECTED_DISCONNECT"
               and m["receivedAt"] >= killed_at]
    if not offline:
        expect(False, "异常退出后 3 个上报周期内收到 OFFLINE（LWT）",
               "进程被 SIGKILL 后未收到 OFFLINE/UNEXPECTED_DISCONNECT", results)
        return
    delay = offline[0]["receivedAt"] - killed_at
    expect(delay <= LWT_DEADLINE_MS, "异常退出后 3 个上报周期内收到 OFFLINE（LWT）",
           f"kill 后 {delay} ms 收到 OFFLINE/UNEXPECTED_DISCONNECT（上限 {LWT_DEADLINE_MS} ms）",
           results)


def case_events(agent: str, config: str, ports: str, tmp: Path,
                envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """连续失败 5 次报 SENSOR_OFFLINE，恢复后报 SENSOR_RECOVERED。

    必须在同一个进程里走完「失败 → 恢复」：失败计数是进程内状态，
    重启会把计数清零，第二次运行根本不知道设备还离线着，恢复事件不会产生。
    """
    clear_faults("A")
    inject("A", 1, "timeout", count=1000)
    sub = Subscriber(envs["grid"], "/edge/+/+/up")
    proc = subprocess.Popen(
        [agent, "-config", config, "-ports", ports, "-seq", str(tmp / "seq-f.state"),
         "-log-level", "warn"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=envs["grid"],
    )
    try:
        # 从站 1 每轮 3 次尝试 × 1000 ms 超时 ≈ 3 秒，5 次失败约 15 秒，留到 30 秒。
        time.sleep(30)
        clear_faults("A")
        time.sleep(12)
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        clear_faults("A")
        sub.stop()

    events = parse(sub.snapshot(), "EVENT")
    offline = [e for e in events if e["body"].get("eventType") == "SENSOR_OFFLINE"]
    recovered = [e for e in events if e["body"].get("eventType") == "SENSOR_RECOVERED"]
    problems = []
    if len(offline) != 1:
        problems.append(f"SENSOR_OFFLINE 收到 {len(offline)} 次，期望 1 次")
    if len(recovered) != 1:
        problems.append(f"SENSOR_RECOVERED 收到 {len(recovered)} 次，期望 1 次")
    if offline and offline[0]["body"].get("slaveAddr") != 1:
        problems.append(f"离线事件从站 {offline[0]['body'].get('slaveAddr')}，期望 1")
    if offline and offline[0]["body"].get("failureCount") != 5:
        problems.append(f"failureCount={offline[0]['body'].get('failureCount')}，期望 5")
    detail = "；".join(problems) if problems else (
        f"SENSOR_OFFLINE（从站 1，连续 {offline[0]['body'].get('failureCount')} 次，"
        f"reason={offline[0]['body'].get('reason')}）→ SENSOR_RECOVERED")
    expect(not problems, "连续失败报离线事件、恢复报恢复事件", detail, results)


def compose(env: dict[str, str], *args: str) -> subprocess.CompletedProcess:
    """跑 docker compose。ACL 由 MQTT_TOPIC_SCHEME 选，切换方案必须重建 Broker。"""
    return subprocess.run(
        ["docker", "compose", "-f", "deploy/docker-compose.yml",
         "--env-file", "deploy/versions.env", "--env-file", ".env", *args],
        capture_output=True, text=True, env=env,
    )


def restart_broker(env: dict[str, str], scheme: str) -> None:
    """以指定方案重建 Broker 容器，等它就绪。"""
    env = dict(env)
    env["MQTT_TOPIC_SCHEME"] = scheme
    proc = compose(env, "up", "-d", "--force-recreate", "mosquitto")
    if proc.returncode != 0:
        raise RuntimeError(f"重建 Broker 失败（{scheme}）：{proc.stderr.strip()}")
    for _ in range(50):
        probe = subprocess.run(
            ["docker", "exec", BROKER_CONTAINER, "mosquitto_pub",
             "-h", "127.0.0.1", "-p", "1883", "-t", "/probe", "-m", "x"],
            capture_output=True, text=True,
        )
        # 匿名发布被拒（退出码非 0）说明 Broker 已经在按 ACL 拒绝，
        # 这正是「就绪」的信号——还没起来的 Broker 会连接被拒。
        if "Connection refused" not in probe.stderr:
            return
        time.sleep(0.2)
    raise RuntimeError("Broker 重建后未就绪")


def case_reconnect(agent: str, config: str, ports: str, tmp: Path,
                   envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """Broker 重启后自动重连，并补发一条 ONLINE。

    两个判据缺一不可：连接要自己回来（不靠进程重启），回来之后要重新声明 ONLINE——
    只重连不发 ONLINE，平台侧看到的仍是离线。

    怎么读「重连后的 ONLINE」：状态主题是 Retain 的，重连后那条 ONLINE 会留在 Broker 上。
    Broker 一重启，之前的订阅（clean session）就没了，等消息只能靠时间窗去卡；
    改读 Retain 值则是确定性的：只要仍挂着旧 OFFLINE，就说明没补发。
    """
    proc = subprocess.Popen(
        [agent, "-config", config, "-ports", ports, "-seq", str(tmp / "seq-h.state"),
         "-log-level", "info"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=envs["grid"],
    )
    try:
        time.sleep(8)
        restart_broker(envs["grid"], "grid")
        time.sleep(20)
        # 允许 Retain：这里要的正是「重连后留下的最后状态」。
        probe = Subscriber(envs["grid"], "/edge/ESN-ZX-0001/status/up", skip_retained=False)
        time.sleep(1.5)
        retained = parse(probe.snapshot(), "STATUS")
        probe.stop()
    finally:
        if proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
        try:
            log, _ = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            log, _ = proc.communicate()

    reconnected = log.count("MQTT 已恢复")
    retries = log.count("MQTT 重连失败")
    last = retained[-1] if retained else None
    status = last["body"].get("status") if last else "（无 Retain 状态）"
    ok = reconnected >= 1 and last is not None and status == "ONLINE"
    expect(ok, "Broker 重启后自动重连并补发 ONLINE",
           f"重连成功 {reconnected} 次、退避重试 {retries} 次；"
           f"重连后 Broker 上的 Retain 状态 = {status}", results)


def case_scheme_switch(agent: str, config: str, ports: str, tmp: Path,
                       envs: dict[str, dict[str, str]], results: list[dict]) -> None:
    """MQTT_TOPIC_SCHEME=internal 时同一份代码发到 /workshop/ 前缀。

    ACL 是按方案选的（deploy/mosquitto/acl.<scheme>），所以切换方案必须重建 Broker；
    只改 agent 的环境变量会被 grid 的 ACL 拦下，表现为「发不出去」而不是「发到别的主题」。
    """
    try:
        restart_broker(envs["internal"], "internal")
        edge = Subscriber(envs["internal"], "/edge/+/+/up")
        shop = Subscriber(envs["internal"], "/workshop/+/+/up")
        try:
            run_agent(agent, config, ports, tmp / "seq-g.state", 8, envs["internal"])
        finally:
            edge.stop()
            shop.stop()
        shop_topics = {m["topic"] for m in parse(shop.snapshot())}
        edge_count = len(parse(edge.snapshot()))
    finally:
        restart_broker(envs["grid"], "grid")

    want = {"/workshop/ESN-ZX-0001/data/up", "/workshop/ESN-ZX-0001/status/up"}
    ok = want <= shop_topics and edge_count == 0
    expect(ok, "主题随 MQTT_TOPIC_SCHEME 切换",
           f"internal 方案下收到 {sorted(shop_topics)}；"
           f"/edge/ 前缀消息 {edge_count} 条（应为 0）", results)


def case_model_is_pure(agent_dir: Path, results: list[dict]) -> None:
    """internal/model 只有结构体：不得出现方法声明。"""
    model_dir = agent_dir / "internal" / "model"
    offenders = []
    for path in sorted(model_dir.glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        for no, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if re.match(r"func\s+\(", line):
                offenders.append(f"{path.name}:{no} {line.strip()}")
    expect(not offenders, "internal/model 只有结构体，无方法无校验",
           "；".join(offenders) if offenders else
           f"{len(list(model_dir.glob('*.go')))} 个文件均无方法声明", results)


def main() -> int:
    parser = argparse.ArgumentParser(description="006 验收")
    parser.add_argument("--agent", required=True, help="edge-agent 可执行文件")
    parser.add_argument("--config", default="config/terminals/ESN-ZX-0001.json")
    parser.add_argument("--ports", default=".pty/agent-ports.json")
    parser.add_argument("--evidence", default="docs/evidence/006")
    parser.add_argument("--env-file", default=".env")
    args = parser.parse_args()

    evidence = Path(args.evidence)
    evidence.mkdir(parents=True, exist_ok=True)
    tmp = evidence / "_tmp"
    tmp.mkdir(exist_ok=True)

    base = load_env(Path(args.env_file))
    envs = {"grid": agent_env(base, "grid"), "internal": agent_env(base, "internal")}

    results: list[dict] = []
    case_three_kinds(args.agent, args.config, args.ports, tmp, envs, results)
    case_points_share_seq(args.agent, args.config, args.ports, tmp, envs, results)
    case_seq_monotonic(args.agent, args.config, args.ports, tmp, envs, results)
    case_send_collect_time(args.agent, args.config, args.ports, tmp, envs, results)
    case_failed_points(args.agent, args.config, args.ports, tmp, envs, results)
    case_lwt(args.agent, args.config, args.ports, tmp, envs, results)
    case_reconnect(args.agent, args.config, args.ports, tmp, envs, results)
    case_scheme_switch(args.agent, args.config, args.ports, tmp, envs, results)
    case_events(args.agent, args.config, args.ports, tmp, envs, results)
    case_model_is_pure(Path.cwd(), results)

    passed = all(r["passed"] for r in results)
    payload = {
        "task": "006 上行与断连通知",
        "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "terminal": TERMINAL,
        "topicScheme": "grid（另有一段 internal 对照）",
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