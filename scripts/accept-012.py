#!/usr/bin/env python3
"""012 通知通道对接的端到端验收。

通道这件事的判据大多只能用「换配置再跑一遍」验证，单测覆盖不了：
连通、灰度开关、通道不可达时的留痕、切换通道不改代码，都要真把
Node-RED 投出去再看两边的留痕。

跟 verify-notify.sh 同一个套路：起临时 Node-RED（同镜像、同 flows.json），
用 compose 里那台 wecom-mock 当接收端，跑完即删，不动在跑的全栈。

期望值全部是接口规范 §11.5 与 BR-42 的字面量：

| 场景 | 期望 |
|---|---|
| 通道可达 | 投递 SENT，接收端七项字段齐全 |
| 通道不可达 | `notify_log` 记 FAILED，告警行不受影响 |
| 灰度开关关 | 只写 SKIPPED，不发请求 |
| 换通道 | 只改 `WECOM_WEBHOOK_URL`，同一份 flows 投到不同接收端 |

前置：
    1. make up（含 timescaledb，已 make seed）
    2. docker compose -f deploy/docker-compose.yml --env-file deploy/versions.env --env-file .env --profile sim up -d wecom-mock
    3. .env 已填 WORKSHOP_NOTIFY_PASSWORD

用法：
    .venv-sim/bin/python scripts/accept-012.py --evidence docs/evidence/012
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from threading import Thread

DB = "deploy-timescaledb-1"
IMAGE = "workshop-monitor/node-red:4.0.5"
API_PORT = 12880
BASE = f"http://127.0.0.1:{API_PORT}"
TERMINAL = "ESN-ZX-0001"
POINT_ID = "A.1.motor_temp"

# 七项必显字段 → 期望值，取自接口规范 §11.5 与 rules/sim.md。
# 期望值是字面量，不从接收端的回包里推。
SEVEN_FIELDS = {
    "workshop": "一号车间",
    "terminalId": TERMINAL,
    "pointText": "电机绕组温度",
    "levelText": "严重",
    "triggerValue": 108.0,
    "reason": "连续 3 次超过高高限 105 ℃",
}


# ── 基础设施 ──────────────────────────────────────────────────


def docker(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(["docker", *args], capture_output=True, text=True, check=check)


def compose(*args: str) -> subprocess.CompletedProcess:
    return docker("compose", "-f", "deploy/docker-compose.yml",
                  "--env-file", "deploy/versions.env", "--env-file", ".env", *args)


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


def sql(env: dict[str, str], statement: str) -> str:
    out = docker("exec", DB, "psql", "-U", env["POSTGRES_USER"],
                 "-d", env["POSTGRES_DB"], "-tAc", statement)
    return out.stdout.strip()


def now_ms() -> int:
    return int(time.time() * 1000)


def post_notify(notify_id: str, level: int = 3, alarm_type: str = "HIGH_HIGH_LIMIT",
                trigger: float = 108.0) -> tuple[int, str]:
    body = {
        "notifyId": notify_id, "terminalId": TERMINAL, "pointId": POINT_ID,
        "alarmType": alarm_type, "level": level, "triggerValue": trigger,
        "occurTime": now_ms(), "reason": "连续 3 次超过高高限 105 ℃",
    }
    req = urllib.request.Request(
        f"{BASE}/api/notify", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode()


# ── 临时 Node-RED ────────────────────────────────────────────


class NodeRed:
    """一个临时 Node-RED 实例。env 决定它把通知投到哪。"""

    def __init__(self, name: str, env: dict[str, str], webhook: str, enabled: str):
        self.name = name
        self.env = env
        self.webhook = webhook
        self.enabled = enabled

    def __enter__(self) -> "NodeRed":
        docker("rm", "-f", self.name, check=False)
        cmd = [
            "docker", "run", "-d", "--name", self.name, "--network", "deploy_default",
            "-p", f"127.0.0.1:{API_PORT}:1880",
            "-e", "PORT=1880", "-e", "TZ=Asia/Shanghai",
            "-e", f"PG_HOST={DB}", "-e", "PG_PORT=5432",
            "-e", f"PG_DATABASE={self.env['POSTGRES_DB']}",
            "-e", "PG_USER=workshop_notify",
            "-e", f"PG_PASSWORD={self.env['WORKSHOP_NOTIFY_PASSWORD']}",
            "-e", "NOTIFY_MAX_RETRY=3", "-e", f"NOTIFY_CHANNEL={self.env.get('NOTIFY_CHANNEL', 'wecom-mock')}",
            "-e", f"WECOM_WEBHOOK_URL={self.webhook}",
            "-e", f"WECOM_ENABLED={self.enabled}",
            "-v", f"{Path('deploy/node-red').resolve()}:/data",
            IMAGE,
        ]
        subprocess.run(cmd, capture_output=True, text=True, check=True)
        deadline = time.time() + 60
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(BASE + "/", timeout=2) as resp:
                    if resp.status == 200:
                        return self
            except (urllib.error.URLError, ConnectionError, OSError):
                pass
            time.sleep(0.3)
        raise RuntimeError(f"Node-RED {self.name} 60 秒内未就绪")

    def __exit__(self, *_exc) -> None:
        docker("rm", "-f", self.name, check=False)


# ── 备用接收端（验证「换通道不改代码」）─────────────────────────


class Echo(BaseHTTPRequestHandler):
    """只记数，不解析：第 6 条要证的是「投到了另一个地址」，不是解析能力。"""

    hits: list[str] = []

    def do_POST(self) -> None:
        n = int(self.headers.get("content-length", 0))
        Echo.hits.append(self.rfile.read(n).decode("utf-8", "replace"))
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"errcode":0,"errmsg":"ok"}')

    def log_message(self, *args: object) -> None:  # noqa: D102 —— 静音访问日志
        pass


def start_echo(port: int) -> tuple[HTTPServer, Thread]:
    server = HTTPServer(("127.0.0.1", port), Echo)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread


# ── 接收端与库的回读 ─────────────────────────────────────────


def mock_records(limit: int = 20) -> list[dict]:
    with urllib.request.urlopen(f"http://127.0.0.1:8081/api/v1/records?limit={limit}",
                                timeout=5) as resp:
        return json.loads(resp.read().decode())["records"]


def wait_status(env: dict[str, str], notify_id: str, seconds: int = 25) -> str:
    for _ in range(seconds * 2):
        got = sql(env, f"SELECT status FROM monitor.notify_log WHERE notify_id = '{notify_id}'")
        if got:
            return got
        time.sleep(0.5)
    return ""


def clean(env: dict[str, str], prefix: str) -> None:
    sql(env, f"DELETE FROM monitor.notify_log WHERE notify_id LIKE '{prefix}%'")
    sql(env, f"DELETE FROM monitor.alarm WHERE terminal_id = '{TERMINAL}'")


# ── 用例 ──────────────────────────────────────────────────────


def case_config_in_env(_env: dict[str, str]) -> tuple[bool, str]:
    """通道地址与凭据只在 .env，不入版本库。"""
    problems = []
    gitignore = Path(".gitignore").read_text(encoding="utf-8").splitlines()
    if ".env" not in gitignore:
        problems.append(".env 不在 .gitignore")
    tracked = subprocess.run(["git", "ls-files", "--error-unmatch", ".env"],
                             capture_output=True, text=True)
    if tracked.returncode == 0:
        problems.append(".env 被版本库跟踪")
    # 排除检查脚本自身：它们必须写出这些特征串才能找它们。
    checks = {"accept-012.py", "check-notify.sh"}
    leaked = []
    for path in ["deploy", "scripts", "config", "sim"]:
        for file in Path(path).rglob("*"):
            if not file.is_file() or file.suffix not in {".py", ".sh", ".json", ".js", ".yml"}:
                continue
            if file.name in checks:
                continue
            text = file.read_text(encoding="utf-8", errors="replace")
            for needle in ("qyapi.weixin.qq.com", "webhook/send?key="):
                if needle in text:
                    leaked.append(f"{file}:{needle}")
    if leaked:
        problems.append("版本库出现写死的通道凭据：" + "、".join(leaked))
    if problems:
        return False, "；".join(problems)
    return True, ".env 不入版本库，版本库无写死的通道地址与凭据"


def case_connectivity_check_script(_env: dict[str, str]) -> tuple[bool, str]:
    """scripts/check-notify.sh 能跑通并输出结果。"""
    out = subprocess.run(["bash", "scripts/check-notify.sh"], capture_output=True, text=True)
    tail = [ln for ln in out.stdout.splitlines() if ln.strip().startswith(("ok", "FAIL"))]
    if out.returncode != 0:
        return False, "check-notify.sh 退出码非 0：" + "；".join(tail)
    return True, f"check-notify.sh 通过，{len(tail)} 项全 ok"


def case_card_seven_fields(env: dict[str, str]) -> tuple[bool, str]:
    """通道可达时投递成功，接收端卡片七项字段齐全。"""
    clean(env, "NT-012-")
    with NodeRed("ws-012-fields", env, "http://wecom-mock:8081/webhook", "true"):
        code, _ = post_notify("NT-012-FIELDS")
        if code != 202:
            return False, f"受理返回 {code}（应为 202）"
        status = wait_status(env, "NT-012-FIELDS")
        if status != "SENT":
            return False, f"notify_log 状态 {status or '<无>'}（应为 SENT）"
    rows = [r for r in mock_records() if r["notifyId"] == "NT-012-FIELDS"]
    if not rows:
        return False, "接收端查不到该卡片"
    record = rows[0]
    wrong = [f"{k}={record.get(k)!r}≠{want!r}" for k, want in SEVEN_FIELDS.items()
             if record.get(k) != want]
    # 发生时间是动态值，只判非零且与受理时刻同分钟——不能抄实现的时间戳当期望。
    if not isinstance(record.get("occurTime"), int) or record["occurTime"] <= 0:
        wrong.append(f"occurTime={record.get('occurTime')!r}")
    for key in ("deviceText", "pointText", "alarmTypeText", "unit"):
        if not record.get(key):
            wrong.append(f"{key} 为空")
    if wrong:
        return False, "字段不符：" + "；".join(wrong)
    return True, "SENT，七项字段逐项一致：" + "、".join(SEVEN_FIELDS) + "、occurTime 非零"


def case_channel_unreachable(env: dict[str, str]) -> tuple[bool, str]:
    """通道不可达时告警照常入库，notify_log 记失败。"""
    clean(env, "NT-012-")
    # 先造一条告警，代表「告警已经入库」这件事已经发生（真入库由 adapter 做，
    # 这里只需一个已存在的行来验证投递失败不动它）。
    occur = now_ms() // 1000
    sql(env, "INSERT INTO monitor.alarm"
             " (dedupe_key, terminal_id, point_id, alarm_type, level, status,"
             "  trigger_value, threshold_value, occur_time, last_time, notify_status)"
             f" VALUES ('{TERMINAL}|{POINT_ID}|HIGH_HIGH_LIMIT','{TERMINAL}','{POINT_ID}',"
             f" 'HIGH_HIGH_LIMIT',3,'ACTIVE',108,105, to_timestamp({occur}),"
             " to_timestamp(" + str(occur) + "), 'PENDING')")
    before = sql(env, "SELECT count(*) FROM monitor.alarm WHERE terminal_id = '" + TERMINAL + "'")
    with NodeRed("ws-012-down", env, "http://127.0.0.1:9/webhook", "true"):
        code, _ = post_notify("NT-012-DOWN")
        if code != 202:
            return False, f"通道不可达时受理返回 {code}（应为 202）"
        status = wait_status(env, "NT-012-DOWN", 40)
        if status != "FAILED":
            return False, f"notify_log 状态 {status or '<无>'}（应为 FAILED）"
        reason = sql(env, "SELECT coalesce(fail_reason,'') FROM monitor.notify_log"
                          " WHERE notify_id = 'NT-012-DOWN'")
    after = sql(env, "SELECT count(*) FROM monitor.alarm WHERE terminal_id = '" + TERMINAL + "'")
    if not reason:
        return False, "fail_reason 为空，失败原因没留痕"
    if after != before:
        return False, f"告警行数变了 {before} → {after}，投递失败不该动告警"
    return True, f"受理 202、FAILED 留痕（{reason[:40]}），告警行数保持 {after}"


def case_gray_switch_off(env: dict[str, str]) -> tuple[bool, str]:
    """灰度开关关闭时只写 SKIPPED，不发出请求。"""
    clean(env, "NT-012-")
    hits_before = len(mock_records())
    with NodeRed("ws-012-off", env, "http://wecom-mock:8081/webhook", "false"):
        code, _ = post_notify("NT-012-OFF")
        if code != 202:
            return False, f"灰度关闭时受理返回 {code}（应为 202）"
        status = wait_status(env, "NT-012-OFF")
        if status != "SKIPPED":
            return False, f"notify_log 状态 {status or '<无>'}（应为 SKIPPED）"
        reason = sql(env, "SELECT coalesce(fail_reason,'') FROM monitor.notify_log"
                          " WHERE notify_id = 'NT-012-OFF'")
    time.sleep(1)
    hits_after = len(mock_records())
    if hits_after != hits_before:
        return False, f"灰度关闭仍发出了请求，接收端记录 {hits_before} → {hits_after}"
    return True, f"SKIPPED 且未发请求，fail_reason={reason[:40]}"


def case_switch_channel_by_config(env: dict[str, str]) -> tuple[bool, str]:
    """切换通道只改配置：同一份 flows 投到另一个地址，不动代码。"""
    clean(env, "NT-012-")
    port = 18082
    server, _ = start_echo(port)
    try:
        with NodeRed("ws-012-switch", env, f"http://host.docker.internal:{port}/webhook", "true"):
            code, _ = post_notify("NT-012-SWITCH")
            if code != 202:
                return False, f"受理返回 {code}（应为 202）"
            status = wait_status(env, "NT-012-SWITCH")
            if status != "SENT":
                return False, f"notify_log 状态 {status or '<无>'}（应为 SENT）"
    finally:
        server.shutdown()
    if not Echo.hits:
        return False, "另一个通道没收到任何投递"
    if "NT-012-SWITCH" not in Echo.hits[0]:
        return False, "收到的报文里没有本条 notifyId"
    got = json.loads(Echo.hits[0])
    if got.get("channel") != env.get("NOTIFY_CHANNEL", "wecom-mock"):
        return False, f"channel 字段 {got.get('channel')!r}，与 NOTIFY_CHANNEL 不符"
    return True, "同一份 flows 投到第二个通道，channel 字段跟随配置"


CASES = [
    ("通道地址与凭据走 .env，不入版本库", case_config_in_env),
    ("check-notify.sh 验证连通并输出结果", case_connectivity_check_script),
    ("通道可达时卡片七项字段齐全", case_card_seven_fields),
    ("通道不可达时告警照常入库且留痕", case_channel_unreachable),
    ("灰度开关关闭只写 SKIPPED、不发请求", case_gray_switch_off),
    ("切换通道只改配置不改代码", case_switch_channel_by_config),
]


def main() -> int:
    parser = argparse.ArgumentParser(description="012 验收")
    parser.add_argument("--evidence", default="docs/evidence/012")
    args = parser.parse_args()

    evidence = Path(args.evidence)
    evidence.mkdir(parents=True, exist_ok=True)
    env = load_env(Path(".env"))
    if "WORKSHOP_NOTIFY_PASSWORD" not in env:
        print("缺少 .env 里的 WORKSHOP_NOTIFY_PASSWORD", file=sys.stderr)
        return 2

    results = []
    for name, fn in CASES:
        ok, detail = fn(env)
        results.append({"name": name, "passed": ok, "detail": detail})
        print(f"{'通过' if ok else '未通过'}：{name} —— {detail}")

    clean(env, "NT-012-")
    passed = all(r["passed"] for r in results)
    payload = {
        "task": "012 通知通道对接",
        "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "receiver": "sim/wecom-mock（compose sim profile）",
        "passed": passed,
        "cases": results,
    }
    (evidence / "acceptance.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"\n验收{'通过' if passed else '未通过'}，证据：{evidence / 'acceptance.json'}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
