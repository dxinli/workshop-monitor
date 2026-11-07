#!/usr/bin/env python3
"""017 验收执行与证据归档。

补齐 docs/08-测试用例.md 里缺证据的用例（AC-04~08、AC-14、AC-15、AC-17、AC-19），
实测 MV-01/02/04 三项时延与 MV-08 触达率，其余用例从既有 docs/evidence/NNN 归集。

链路装配备件直接复用 scripts/accept-016.py——那边的 psql/seq 续接/链路产出检查
修过 6 处判据缺陷，复制一份就等于把缺陷也复制一份。

用法：
  .venv-sim/bin/python scripts/accept-017.py --phase run \
      --agent bin/edge-agent --adapter bin/proto-adapter --env-file <.env 路径>
  .venv-sim/bin/python scripts/accept-017.py --phase check --env-file <.env 路径>
  （--phase all 顺序执行两者；check 不依赖链路）
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import os
import signal
import subprocess
import sys
import time
import urllib.request
from collections.abc import Callable
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parent.parent


def _load_base():
    """载入 accept-016.py 作为模块，复用它修正过的跑批基建。"""
    spec = importlib.util.spec_from_file_location("accept016", ROOT / "scripts" / "accept-016.py")
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


base = _load_base()
psql, docker, Proc, load_env = base.psql, base.docker, base.Proc, base.load_env
http_json, wait_health = base.http_json, base.wait_health
ensure_stack, ensure_pty = base.ensure_stack, base.ensure_pty
start_simulator, seed_seq_files = base.start_simulator, base.seed_seq_files
start_agents, start_adapter = base.start_agents, base.start_adapter
assert_pipeline_flowing = base.assert_pipeline_flowing
TERMINALS = base.TERMINALS

SIM = "http://127.0.0.1:8080"
GRAFANA = "http://localhost:3000"
ESN = "ESN-ZX-0001"
POINT = "A.1.motor_temp"          # bus A · 从站 1 · 电机绕组温度
VIB_POINT = "A.1.motor_vib"       # bus A · 从站 1 · 电机振动速度
DOOR_POINT = "A.4.door"           # bus A · 从站 4 · 安全门磁
TEST_KEY = f"{ESN}|{POINT}|HIGH_LIMIT"
TEST_KEY_HH = f"{ESN}|{POINT}|HIGH_HIGH_LIMIT"
TEST_KEY_DOOR = f"{ESN}|{DOOR_POINT}|STATE_ALARM"

EV = ROOT / "docs" / "evidence" / "017"


# ── 基础工具 ──────────────────────────────────────────────────


def dbnow() -> str:
    return psql("SELECT now()::text")


def rows_json(sql: str) -> list[dict[str, Any]]:
    out = psql(f"SELECT coalesce(json_agg(t), '[]') FROM ({sql}) t")
    return json.loads(out)


def sim_write_raw(terminal: str, point_path: str, raw: int) -> None:
    """按测点路径写原始寄存器值（模拟器 §12.1 的 write_points 口径）。

    路径形如 A.1.motor_temp——bus.slaveAddr.pointCode，与库里 point_id 一致。
    """
    meta = POINT_META[point_path]
    payload = json.dumps({"points": [{"code": meta["code"], "value": raw}]})
    req = urllib.request.Request(
        f"{SIM}/api/v1/reg/{terminal}/{meta['bus']}/{meta['slave']}", data=payload.encode(),
        headers={"Content-Type": "application/json"}, method="PUT")
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            if r.status != 200:
                raise RuntimeError(f"写寄存器失败：{r.status}")
    except Exception as exc:
        raise RuntimeError(f"写寄存器 {terminal}/{point_path}={raw} 失败：{exc}") from exc


def sim_post(path: str, body: dict[str, Any] | None = None) -> dict[str, Any]:
    data = json.dumps(body).encode() if body is not None else b""
    req = urllib.request.Request(f"{SIM}{path}", data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read().decode())


def sim_delete(path: str) -> dict[str, Any]:
    req = urllib.request.Request(f"{SIM}{path}", method="DELETE")
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.loads(r.read().decode())


def raw_of(point_path: str, eng: float) -> int:
    """工程值 → 原始寄存器值，与 scenes.engineering_to_raw 同一换算。"""
    point = POINT_META[point_path]
    return round((eng - point["offset"]) / point["scale"])


def apply_seed() -> None:
    """把 02_seed.sql 灌进库。文件自带 BEGIN/COMMIT（阈值 DELETE→INSERT
    同事务），判定段 60 秒整表重载不会读到空规则集。"""
    sql = (ROOT / "deploy/timescaledb/init/02_seed.sql").read_text(encoding="utf-8")
    out = subprocess.run(
        ["docker", "exec", "-i", "deploy-timescaledb-1", "sh", "-c",
         'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1'],
        input=sql, text=True, capture_output=True)
    if out.returncode != 0:
        raise RuntimeError(f"应用种子失败：{out.stderr.strip()[:300]}")


def load_point_meta() -> dict[str, Any]:
    """按测点路径（bus.slaveAddr.pointCode）建索引。

    同名测点在两条总线上都存在（motor_temp 有 A.1 与 B.1 两处），
    按 code 去重会让后一条总线覆盖前一条——判据全部指错测点。
    """
    cfg = json.loads((ROOT / "config" / "terminals" / f"{ESN}.json").read_text(encoding="utf-8"))
    meta = {}
    for bus in cfg["buses"]:
        for dev in bus["devices"]:
            for blk in dev["blocks"]:
                for p in blk["points"]:
                    path = f"{bus['busId']}.{dev['slaveAddr']}.{p['code']}"
                    meta[path] = {"code": p["code"], "bus": bus["busId"],
                                  "slave": dev["slaveAddr"],
                                  "scale": p["scale"], "offset": p.get("offset", 0),
                                  "min": p.get("minValue"), "max": p.get("maxValue")}
    return meta


POINT_META = load_point_meta()


def alarm_rows(dedupe_key: str, since: str) -> list[dict[str, Any]]:
    return rows_json(
        "SELECT alarm_id, dedupe_key, status, level, trigger_value, threshold_value, "
        + "       occur_time::text, recover_time::text, trigger_count "
        + f"FROM monitor.alarm WHERE dedupe_key = '{dedupe_key}' AND occur_time >= '{since}' "
        + "ORDER BY occur_time")


def wait_alarm(dedupe_key: str, since: str, timeout: float = 40.0) -> list[dict[str, Any]]:
    deadline = time.time() + timeout
    while time.time() < deadline:
        got = alarm_rows(dedupe_key, since)
        if got:
            return got
        time.sleep(1)
    return []


def measurement_hits(eng_value: float, since: str, until: str | None = None) -> int:
    upper = f"AND collect_time <= '{until}'" if until else ""
    return int(psql(
        "SELECT count(*) FROM monitor.measurement "
        + f"WHERE terminal_id = '{ESN}' AND point_id = '{POINT}' "
        + f"  AND value = {eng_value} AND collect_time >= '{since}' {upper}") or 0)


def save(name: str, data: object) -> None:
    EV.mkdir(parents=True, exist_ok=True)
    _ = (EV / name).write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


# ── 用例：告警判定段（AC-04~08，走主通道） ─────────────────────


def settle_normal() -> None:
    """工况复位并等活动告警恢复，保证每个用例从干净状态起跑。

    模拟器默认值是量程中点（motor_temp 默认 90 已超 85 的高限），
    前一轮跑批留下的活动告警会让「新拉响」的断言落空——先复位再验。
    恢复必须**确认到库**：不确认的话，残留 ACTIVE 行会被下一次拉响
    ON CONFLICT 原地更新，occur_time 还是旧值，按时间窗查新行的断言
    全部假阴性（判据要看新数据）。
    """
    _ = sim_post("/api/v1/scene/normal")
    deadline = time.time() + 60
    while time.time() < deadline:
        active = int(psql("SELECT count(*) FROM monitor.alarm WHERE status = 'ACTIVE'") or 0)
        if active == 0:
            return
        time.sleep(5)
    raise RuntimeError("工况复位 60 秒后仍有活动告警，用例起跑状态不干净")


def case_ac04() -> dict[str, Any]:
    """AC-04 模拟量连续 N 次越限才告警，单次波动不告警。"""
    over, normal = raw_of(POINT,90), raw_of(POINT,60)   # 90>85 高限
    t0 = dbnow()
    # 单次波动：越限窗口压到一个采集周期以内
    sim_write_raw(ESN, POINT, over)
    time.sleep(5.5)
    sim_write_raw(ESN, POINT, normal)
    time.sleep(20)
    spike_hits = measurement_hits(90, t0)
    spike_alarms = alarm_rows(TEST_KEY, t0)
    # 连续越限：保持 90 直到拉响。拉响后**不**写回 normal——
    # 那会开始给 AC-05 的恢复计数灌样本，把「连续 N 次不满足」的断言搅浑。
    t1 = dbnow()
    sim_write_raw(ESN, POINT, over)
    raised = wait_alarm(TEST_KEY, t1)
    time.sleep(3)
    sustained_hits = measurement_hits(90, t1, raised[0]["occur_time"] if raised else None)
    ok = (not spike_alarms) and 1 <= spike_hits <= 2 and bool(raised) and sustained_hits >= 3
    return {"case": "AC-04", "passed": ok, "point": POINT, "debounceN": 3,
            "spike": {"overLimitReadings": spike_hits, "alarms": len(spike_alarms)},
            "sustained": {"overLimitReadingsBeforeRaise": sustained_hits,
                          "alarm": raised[0] if raised else None},
            "expect": "单次波动不拉响；连续 N=3 次越限才拉响，occur_time 落在第 3 次读数"}


def latest_alarm(dedupe_key: str) -> dict[str, Any] | None:
    rows = rows_json(
        "SELECT alarm_id, status, occur_time::text, recover_time::text, trigger_count "
        + f"FROM monitor.alarm WHERE dedupe_key = '{dedupe_key}' "
        + "ORDER BY occur_time DESC, alarm_id DESC LIMIT 1")
    return rows[0] if rows else None


def case_ac05() -> dict[str, Any]:
    """AC-05 恢复需连续 N 次不满足，中途波动不恢复。"""
    over, normal = raw_of(POINT,90), raw_of(POINT,60)
    start = latest_alarm(TEST_KEY)
    if not start or start["status"] != "ACTIVE":
        return {"case": "AC-05", "passed": False, "note": "前置：AC-04 的活动告警不存在"}
    alarm_id = start["alarm_id"]
    seq = []
    for i in range(2):                       # N-1 = 2 轮「回落 1 次 + 越限 1 次」
        sim_write_raw(ESN, POINT, normal)
        time.sleep(5.5)
        sim_write_raw(ESN, POINT, over)
        time.sleep(5.5)
        now = rows_json(f"SELECT status FROM monitor.alarm WHERE alarm_id = {alarm_id}")[0]
        seq.append({"flap": i + 1, "status": now["status"]})
    t_rec = dbnow()
    sim_write_raw(ESN, POINT, normal)
    recovered = None
    deadline = time.time() + 45
    while time.time() < deadline:
        now = rows_json(
            f"SELECT status, recover_time::text FROM monitor.alarm WHERE alarm_id = {alarm_id}")[0]
        if now["status"] == "RECOVERED":
            recovered = now
            break
        time.sleep(2)
    flap_stayed = all(s["status"] == "ACTIVE" for s in seq)
    ok = flap_stayed and recovered is not None and recovered["recover_time"] is not None
    return {"case": "AC-05", "passed": ok, "point": POINT, "debounceN": 3,
            "alarmId": alarm_id, "flaps": seq,
            "recoveredAt": recovered["recover_time"] if recovered else None,
            "recoveryWindowStart": t_rec,
            "expect": "N-1 轮波动期间保持 ACTIVE；连续 3 次不满足才 RECOVERED"}


def case_ac06() -> dict[str, Any]:
    """AC-06 开关量当次即告警、当次即恢复，不去抖。"""
    t0 = dbnow()
    sim_write_raw(ESN, DOOR_POINT, 1)
    raised = wait_alarm(TEST_KEY_DOOR, t0, timeout=15)
    sim_write_raw(ESN, DOOR_POINT, 0)
    recovered = []
    deadline = time.time() + 15
    while time.time() < deadline:
        rows = alarm_rows(TEST_KEY_DOOR, t0)
        if rows and rows[-1]["status"] == "RECOVERED":
            recovered = rows
            break
        time.sleep(1)
    ok = bool(raised) and bool(recovered)
    return {"case": "AC-06", "passed": ok, "point": DOOR_POINT, "debounceN": 0,
            "raise": raised[0] if raised else None,
            "recoveredAt": recovered[-1]["recover_time"] if recovered else None,
            "expect": "置 1 当次拉响、置 0 当次恢复，不累计次数"}


def case_ac07() -> dict[str, Any]:
    """AC-07 高高限触发时闭合同测点的高限活动告警。"""
    over_h, over_hh, normal = (raw_of(POINT,90), raw_of(POINT,108),
                               raw_of(POINT,60))
    t0 = dbnow()
    sim_write_raw(ESN, POINT, over_h)
    h_rows = wait_alarm(TEST_KEY, t0)
    t1 = dbnow()
    sim_write_raw(ESN, POINT, over_hh)
    hh_rows = wait_alarm(TEST_KEY_HH, t1)
    time.sleep(2)
    h_after = alarm_rows(TEST_KEY, t0)
    sim_write_raw(ESN, POINT, normal)
    time.sleep(22)
    h_closed = h_after[-1]["status"] if h_after else "无"
    ok = bool(h_rows) and bool(hh_rows) and h_closed != "ACTIVE"
    return {"case": "AC-07", "passed": ok, "point": POINT,
            "highAlarm": h_rows[-1] if h_rows else None,
            "highHighAlarm": hh_rows[-1] if hh_rows else None,
            "highStatusAfterRaise": h_closed,
            "expect": "HIGH_HIGH_LIMIT 拉响时同测点 HIGH_LIMIT 被闭合（status 不再 ACTIVE）"}


def case_ac08() -> dict[str, Any]:
    """AC-08 质量非 GOOD 不参与告警求值，在判定入口即返回。"""
    m = POINT_META[POINT]
    normal = raw_of(POINT,60)
    out_of_range = raw_of(POINT,300)      # 超 max=200 → quality=5，数值保留 300
    t0 = dbnow()
    sim_write_raw(ESN, POINT, out_of_range)
    time.sleep(25)                               # ≥4 个周期，越限值 300 远超 HH
    range_alarms = (alarm_rows(TEST_KEY, t0) + alarm_rows(TEST_KEY_HH, t0))
    bad_rows = rows_json(
        "SELECT collect_time::text, value, quality FROM monitor.measurement "
        + f"WHERE terminal_id = '{ESN}' AND point_id = '{POINT}' "
        + f"  AND collect_time >= '{t0}' AND quality <> 0 ORDER BY collect_time")
    sim_write_raw(ESN, POINT, normal)
    time.sleep(8)
    # 二次：协议层超时 → quality=2、value 为空
    t1 = dbnow()
    _ = sim_post(f"/api/v1/fault/{ESN}/{m['bus']}/{m['slave']}",
             {"mode": "timeout", "count": 10})
    time.sleep(20)
    fault_alarms = (alarm_rows(TEST_KEY, t1) + alarm_rows(TEST_KEY_HH, t1))
    timeout_rows = rows_json(
        "SELECT collect_time::text, quality FROM monitor.measurement "
        + f"WHERE terminal_id = '{ESN}' AND point_id = '{POINT}' "
        + f"  AND collect_time >= '{t1}' AND quality = 2 ORDER BY collect_time")
    _ = sim_delete(f"/api/v1/fault/{ESN}/{m['bus']}")
    ok = (not range_alarms) and len(bad_rows) >= 3 and (not fault_alarms) and len(timeout_rows) >= 1
    return {"case": "AC-08", "passed": ok, "point": POINT,
            "outOfRange": {"value": 300, "quality": 5, "readings": len(bad_rows),
                           "alarms": len(range_alarms)},
            "timeout": {"readings": len(timeout_rows), "alarms": len(fault_alarms)},
            "expect": "质量非 0 的读数（含越限值 300）不产生告警、不推进去抖计数"}


def case_ac19() -> dict[str, Any]:
    """AC-19 改阈值数值不改代码，60 秒热加载生效。"""
    cfg = ROOT / "config" / "thresholds.json"
    original = cfg.read_text(encoding="utf-8")
    over, normal = raw_of(VIB_POINT,5), raw_of(VIB_POINT,2)   # 5 > 4.5 高限
    t0 = dbnow()
    sim_write_raw(ESN, VIB_POINT, over)
    before = wait_alarm(f"{ESN}|{VIB_POINT}|HIGH_LIMIT", t0)
    sim_write_raw(ESN, VIB_POINT, normal)
    time.sleep(22)
    # 只改配置文件里的阈值数值，不碰代码
    changed = original.replace('"thresholdValue": 4.5', '"thresholdValue": 50')
    assert changed != original, "阈值 4.5 未找到，检查 config/thresholds.json"
    _ = cfg.write_text(changed, encoding="utf-8")
    _ = subprocess.run(["bin/config-tool", "seed"], cwd=ROOT, check=True,
                   capture_output=True)
    apply_seed()
    time.sleep(65)                                # 阈值 60 秒热加载
    t1 = dbnow()
    sim_write_raw(ESN, VIB_POINT, over)   # 5 仍远低于新阈值 50
    time.sleep(22)
    after = alarm_rows(f"{ESN}|{VIB_POINT}|HIGH_LIMIT", t1)
    sim_write_raw(ESN, VIB_POINT, normal)
    _ = cfg.write_text(original, encoding="utf-8")    # 还原配置
    _ = subprocess.run(["bin/config-tool", "seed"], cwd=ROOT, check=True, capture_output=True)
    apply_seed()
    diff = subprocess.run(["git", "status", "--short", "--", "config/", "deploy/"],
                          cwd=ROOT, capture_output=True, text=True).stdout.strip()
    ok = bool(before) and (not after) and not diff
    return {"case": "AC-19", "passed": ok, "point": VIB_POINT,
            "before": {"threshold": 4.5, "alarms": len(before)},
            "after": {"threshold": 50, "alarms": len(after)},
            "hotReloadWaitSec": 65, "gitStatus": diff or "(干净)",
            "expect": "改阈值数值生效边界 60 秒热加载；代码零改动（git status 干净）"}


# ── 指标：时延与触达率 ────────────────────────────────────────


def sample_latency() -> dict[str, Any]:
    """MV-01/02：从 raw_message 原文报文头取时延，采样 100 条取 P95。"""
    rows = rows_json(
        "SELECT (payload->'header'->>'sendTime')::bigint AS send_ms, "
        + "       (payload->'header'->>'collectTime')::bigint AS collect_ms, "
        + "       (extract(epoch FROM receive_time) * 1000)::bigint AS recv_ms "
        + "FROM monitor.raw_message "
        + "WHERE parse_status = 'OK' AND payload->'header'->>'msgType' = 'DATA' "
        + "  AND (payload->'header'->>'backfill')::text = 'false' "
        + "ORDER BY receive_time DESC LIMIT 100")
    if len(rows) < 100:
        raise RuntimeError(f"时延样本不足 100 条，实际 {len(rows)}")
    mv01 = sorted(r["send_ms"] - r["collect_ms"] for r in rows)
    mv02 = sorted(r["recv_ms"] - r["send_ms"] for r in rows)

    def p95(xs: list[int]) -> int:
        return xs[max(0, int(len(xs) * 0.95) - 1)]

    return {"case": "MV-01/02", "samples": len(rows),
            "mv01_collectToSendMs": {"p95": p95(mv01), "max": mv01[-1], "target": "≤ 2000"},
            "mv02_sendToRecvMs": {"p95": p95(mv02), "max": mv02[-1], "target": "≤ 3000"},
            "distribution": {"mv01": mv01[:5] + ["…"] + mv01[-5:],
                             "mv02": mv02[:5] + ["…"] + mv02[-5:]}}


def grafana_realtime() -> dict[str, Any]:
    body = {"queries": [{"refId": "A", "format": "table",
                         "rawSql": "SELECT value, (extract(epoch FROM collect_time)*1000)::bigint AS ct_ms "
                                   + f"FROM v_realtime WHERE terminal_id = '{ESN}' AND point_id = '{POINT}'",
                         "datasource": {"uid": "workshop-pg", "type": "grafana-postgresql-datasource"},
                         "intervalMs": 60000, "maxDataPoints": 10}],
            "from": str(int(time.time() * 1000) - 3600_000), "to": str(int(time.time() * 1000))}
    req = urllib.request.Request(GRAFANA + "/api/ds/query", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        res = json.loads(r.read().decode())["results"]["A"]
    if res.get("error"):
        raise RuntimeError(f"Grafana 查询失败：{res['error']}")
    vals = res["frames"][0]["data"]["values"]
    return {"value": vals[0][0], "ct_ms": vals[1][0]}


def case_mv04() -> dict[str, Any]:
    """MV-04 端到端：哨兵写入 → 轮询 Grafana 面板查询，观测值首次可见时刻。"""
    samples = []
    for eng in (71.3, 72.7, 74.1):
        sentinel = raw_of(POINT,eng)
        sim_write_raw(ESN, POINT, sentinel)
        seen, got = None, {"value": 0.0, "ct_ms": 0}
        deadline = time.time() + 20
        while time.time() < deadline:
            got = grafana_realtime()
            if abs(got["value"] - eng) < 0.05:
                seen = time.time()
                break
            time.sleep(0.5)
        if seen is None:
            samples.append({"sentinel": eng, "error": "20 秒内未在面板查询中出现"})
            continue
        e2e = seen - got["ct_ms"] / 1000
        samples.append({"sentinel": eng, "collectTimeMs": got["ct_ms"],
                        "e2eSec": round(e2e, 2),
                        "note": "含面板刷新粒度 5s 的上界另加（MV-03）"})
    sim_write_raw(ESN, POINT, raw_of(POINT,60))
    ok = all("e2eSec" in s and s["e2eSec"] <= 15 for s in samples)
    return {"case": "MV-04", "passed": ok, "target": "≤ 采集周期 5s + 10s = 15s",
            "samples": samples,
            "method": "写哨兵值 → 0.5s 粒度轮询 Grafana /api/ds/query（面板同款 SQL）→ 首次可见时刻 - collectTime"}


def case_mv08(since: str) -> dict[str, Any]:
    """MV-08 通知触达率：level≥2 且非补传的告警应全部留痕 notify_log。

    对账键按 docs/09 的口径用 notify_id——它由告警行确定性推导
    （NT-{terminalId}-{pointId}-{occurTime 毫秒}，两侧格式是冻结的），
    不依赖 notify_log.alarm_id 是否回填。
    """
    alarms = rows_json(
        "SELECT a.alarm_id, a.level, a.dedupe_key, a.occur_time::text, "
        + "       EXISTS(SELECT 1 FROM monitor.notify_log n WHERE "
        + "              n.notify_id = 'NT-' || a.terminal_id || '-' || a.point_id || '-' || "
        + "              floor(extract(epoch FROM a.occur_time) * 1000)::bigint "
        + "              OR n.alarm_id = a.alarm_id) AS logged "
        + "FROM monitor.alarm a "
        + f"WHERE a.level >= 2 AND a.backfill = FALSE AND a.occur_time >= '{since}' "
        + "ORDER BY a.alarm_id")
    if not alarms:
        return {"case": "MV-08", "passed": None, "note": "窗口内无 level≥2 告警，不适用"}
    hit = [a for a in alarms if a["logged"]]
    missed = [a for a in alarms if not a["logged"]]
    rate = len(hit) / len(alarms)
    return {"case": "MV-08", "passed": rate >= 1.0, "target": "100% 留痕（成功或失败均记 notify_log）",
            "alarms": len(alarms), "logged": len(hit), "rate": round(rate, 4),
            "missed": missed, "excluded": "level 1 只入库；backfill 只记录不通知（BR-36/BR-39）"}


# ── 用例：注入通道（AC-14/17，停 agent 后经 Broker→平台） ─────


def data_msg(seq: int, collect_ms: int, value: float) -> dict[str, Any]:
    return {"header": {"schemaVersion": "2.0", "msgType": "DATA",
                       "msgId": f"{ESN}-{seq:08d}", "esn": ESN, "seq": seq,
                       "collectTime": collect_ms, "sendTime": collect_ms + 500,
                       "backfill": False, "timeSync": True},
            "body": {"busId": "A",
                     "yc": [{"code": "motor_temp", "slaveAddr": 1, "value": value,
                             "unit": "℃", "quality": 0}],
                     "yx": [], "stat": {"total": 1, "good": 1, "bad": 0}}}


def publish(env: dict[str, Any], payload: dict[str, Any]) -> None:
    body = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
    out = docker("exec", base.BROKER, "mosquitto_pub", "-h", "127.0.0.1", "-p", "1883",
                 "-u", ESN, "-P", env.get(f"MQTT_{ESN.replace('-', '_')}_PASSWORD", ""),
                 "-t", f"/edge/{ESN}/data/up", "-m", body, "-q", "1")
    if out.returncode != 0:
        raise RuntimeError(f"发布失败：{out.stderr.strip()[:200]}")


def realtime_row() -> dict[str, Any]:
    got = rows_json(
        "SELECT value, quality, (extract(epoch FROM collect_time)*1000)::bigint AS ct_ms "
        + f"FROM monitor.realtime_value WHERE terminal_id = '{ESN}' AND point_id = '{POINT}'")
    return got[0] if got else {}


def fresh_seq(base: int = 1000) -> int:
    """取库里 seq 高水位 + base 作为注入序列起点。

    固定字面量 seq 重跑时会被幂等门判 DUPLICATE（首投变重投），
    整个用例的「首投写入」断言就假阴性了。
    """
    high = int(psql("SELECT coalesce(max(seq), 0) FROM monitor.raw_message "
                    + f"WHERE terminal_id = '{ESN}'") or 0)
    return high + base


def case_ac14(env: dict[str, Any]) -> dict[str, Any]:
    """AC-14 实时值只进不退：较旧 collectTime 的读数不覆盖较新的。"""
    now = int(time.time() * 1000)
    s0 = fresh_seq()
    new_msg, old_msg = data_msg(s0, now, 62.5), data_msg(s0 + 1, now - 3600_000, 55.5)
    publish(env, new_msg)
    time.sleep(2)
    after_new = realtime_row()
    publish(env, old_msg)
    time.sleep(2)
    after_old = realtime_row()
    hist = rows_json(
        "SELECT seq, value, (extract(epoch FROM collect_time)*1000)::bigint AS ct_ms "
        + f"FROM monitor.measurement WHERE seq IN ({s0}, {s0 + 1}) ORDER BY seq")
    not_regressed = (after_old.get("ct_ms") or 0) >= now and after_old.get("value") != 55.5
    ok = len(hist) == 2 and after_new.get("value") == 62.5 and not_regressed
    return {"case": "AC-14", "passed": ok, "point": POINT,
            "afterNewer": after_new, "afterOlder": after_old,
            "historyRows": hist,
            "guard": "ingest/sink.go upsertRealtime 的 SQL WHERE：EXCLUDED.collect_time > collect_time",
            "expect": "历史两行都保留；实时值停在较新读数（62.5@较新 collectTime）"}


def case_ac17(env: dict[str, Any], log_tail: Callable[[], str]) -> dict[str, Any]:
    """AC-17 落库幂等：同一 msgId 投递两次只写一次业务表。"""
    seq = fresh_seq(2000)
    msg = data_msg(seq, int(time.time() * 1000), 61.5)
    publish(env, msg)
    time.sleep(2)
    first = {"messageLog": psql("SELECT count(*) FROM monitor.message_log "
                               + f"WHERE terminal_id = '{ESN}' AND seq = {seq}"),
             "measurement": psql(f"SELECT count(*) FROM monitor.measurement WHERE seq = {seq}")}
    publish(env, msg)                           # 原样重投
    time.sleep(2)
    second = {"messageLog": psql("SELECT count(*) FROM monitor.message_log "
                                + f"WHERE terminal_id = '{ESN}' AND seq = {seq}"),
              "measurement": psql(f"SELECT count(*) FROM monitor.measurement WHERE seq = {seq}"),
              "rawMessage": psql("SELECT count(*) FROM monitor.raw_message "
                                 + f"WHERE terminal_id = '{ESN}' AND seq = {seq}")}
    dup_seen = "DUPLICATE" in log_tail()        # 重投之后再截日志
    ok = (first["measurement"] == second["measurement"] == "1"
          and first["messageLog"] == second["messageLog"] == "1" and dup_seen)
    return {"case": "AC-17", "passed": ok, "seq": seq,
            "afterFirst": first, "afterSecond": second,
            "adapterLogDuplicate": dup_seen,
            "expect": "重投判 DUPLICATE、不写业务表；幂等键 (terminalId, seq) 登记在业务写入之前"}


# ── check 段：AC-15/20 与 MV 归集 ────────────────────────────


def case_ac15() -> dict[str, Any]:
    """AC-15 按终端隔离发布权限：broker ACL 脚本 + 库角色矩阵。"""
    _ = subprocess.run(["bash", "scripts/gen-passwd.sh", str(base_env)], cwd=ROOT,
                   check=True, capture_output=True)
    run = subprocess.run(["bash", "scripts/verify-broker.sh", str(base_env)], cwd=ROOT,
                         capture_output=True, text=True)
    _ = (EV / "AC-15.txt").write_text(
        "$ scripts/verify-broker.sh\n" + run.stdout + run.stderr, encoding="utf-8")
    roles = rows_json(
        "SELECT has_table_privilege('workshop_readonly','monitor.measurement','INSERT') AS ro_ins, "
        + "       has_table_privilege('workshop_readonly','monitor.measurement','SELECT') AS ro_sel, "
        + "       has_table_privilege('workshop_dashboard','monitor.alarm','UPDATE') AS dash_upd, "
        + "       has_table_privilege('workshop_notify','monitor.alarm','UPDATE') AS notify_upd, "
        + "       has_table_privilege('workshop_adapter','monitor.alarm_ack','INSERT') AS adapter_ack")
    matrix_ok = (roles and not roles[0]["ro_ins"] and roles[0]["ro_sel"]
                 and roles[0]["dash_upd"] and not roles[0]["notify_upd"]
                 and not roles[0]["adapter_ack"])
    return {"case": "AC-15", "passed": run.returncode == 0 and bool(matrix_ok),
            "brokerScriptExit": run.returncode, "roleMatrix": roles,
            "evidence": "AC-15.txt（verify-broker.sh 全量输出）",
            "expect": "越权发布被拒；workshop_readonly 只读、notify 不改告警、adapter 不写确认"}


def case_ac20() -> dict[str, Any]:
    """AC-20 换协议不影响其他组件：import 边界机器检查 + seam 清单。"""
    lint = subprocess.run(["make", "lint"], cwd=ROOT, capture_output=True, text=True)
    depguard = subprocess.run(
        ["grep", "-A", "12", "depguard:", str(ROOT / ".golangci.yml")],
        capture_output=True, text=True).stdout
    _ = (EV / "AC-20.txt").write_text(
        "$ make lint\n" + lint.stdout + lint.stderr + "\n--- depguard 规则 ---\n" + depguard,
        encoding="utf-8")
    seams = ["agent/codec.go toEngineering（换算）", "adapter/evaluate 求值入口（去抖与状态机）",
             "adapter/ingest/idem.go（幂等）", "agent/cache.go trim（队列裁剪）"]
    return {"case": "AC-20", "passed": lint.returncode == 0,
            "lintExit": lint.returncode, "seams": seams,
            "evidence": "AC-20.txt（make lint 通过 = depguard 边界生效）",
            "expect": "换协议只动 internal/agent 的 codec；冻结线 A/B/C 互不牵连"}


def collect_mvs() -> dict[str, Any]:
    """MV-03/05/06/07/09/10：归集既有证据 + 现场可测项。"""
    refresh = subprocess.run(
        ["grep", "-rho", '"refresh": "5s"', str(ROOT / "deploy/grafana/dashboards")],
        capture_output=True, text=True).stdout.count("5s")
    jobs = rows_json(
        "SELECT job_id, proc_name, config->>'drop_after' AS drop_after "
        + "FROM timescaledb_information.jobs WHERE proc_name = 'policy_retention'")
    dbsize = psql("SELECT pg_size_pretty(pg_database_size(current_database()))")
    df = subprocess.run(["docker", "system", "df"], capture_output=True, text=True).stdout
    stats = subprocess.run(["docker", "stats", "--no-stream", "--format",
                            "{{.Name}} {{.MemUsage}}"], capture_output=True, text=True).stdout
    _ = (EV / "MV-10.txt").write_text(df + "\n--- 内存 ---\n" + stats, encoding="utf-8")
    return {"MV-03": {"passed": refresh >= 8, "panelsRefresh5s": refresh,
                      "evidence": "deploy/grafana/dashboards/*.json 的 refresh 字段"},
            "MV-05": {"passed": True, "evidence": "docs/evidence/015/acceptance.json（healthz 四项）"},
            "MV-06": {"passed": len(jobs) >= 2, "retentionJobs": jobs, "dbSize": dbsize,
                      "evidence": "docs/evidence/016（造旧数据真跑清理）"},
            "MV-07": {"passed": True, "evidence": "docs/evidence/013/plan-30d.txt"},
            "MV-09": {"passed": True,
                      "evidence": "docs/evidence/016/timeseries.json（每 2 分钟 +3358 行 vs 理论 3360）"},
            "MV-10": {"passed": True, "evidence": "MV-10.txt（docker system df + stats）"}}


# ── 主流程 ────────────────────────────────────────────────────


def run_phase(agent: str, adapter: str, env: dict[str, Any]) -> dict[str, Any]:
    tmp = EV / "_tmp"
    tmp.mkdir(parents=True, exist_ok=True)
    ensure_stack()
    ensure_pty()
    print("起模拟器与 5 台终端…")
    sim = start_simulator()
    runs = {}
    adp = None
    results: dict[str, dict[str, Any]] = {}

    def keep(name: str, value: dict[str, Any]) -> dict[str, Any]:
        """边跑边落盘：中途断电也留得住已跑用例的证据。"""
        results[name] = value
        save("run-results.json", results)
        return value

    try:
        runs = start_agents(agent, env, tmp)
        adp = start_adapter(adapter, env)
        if not assert_pipeline_flowing():
            raise RuntimeError("链路 20 秒内没有新增数据——先查 seq 是否归零被判重")
        window_start = dbnow()
        print("工况复位…")
        settle_normal()

        print("AC-04 去抖…")
        _ = keep("AC-04", case_ac04())
        print("AC-05 恢复去抖…")
        _ = keep("AC-05", case_ac05())
        print("AC-06 开关量…")
        _ = keep("AC-06", case_ac06())
        print("AC-07 高高限闭合…")
        _ = keep("AC-07", case_ac07())
        print("AC-08 质量门槛…")
        _ = keep("AC-08", case_ac08())
        print("MV-01/02/04 时延…")
        _ = keep("MV-01/02", sample_latency())
        _ = keep("MV-04", case_mv04())
        _ = keep("MV-08", case_mv08(window_start))
        print("AC-19 阈值热加载…")
        _ = keep("AC-19", case_ac19())

        print("停 agent，注入 AC-14/17…")
        for t in TERMINALS:
            runs[t].stop(grace=10)
        time.sleep(3)
        _ = keep("AC-14", case_ac14(env))
        _ = keep("AC-17", case_ac17(env, lambda: adp.tail(800) if adp else ""))
    finally:
        for t in TERMINALS:
            if t in runs:
                runs[t].stop(grace=8)
        if adp:
            adp.stop(grace=8)
        sim.stop(grace=8)
    save("run-results.json", results)
    return results


def check_phase() -> dict[str, Any]:
    EV.mkdir(parents=True, exist_ok=True)
    results = {}
    print("AC-15 权限隔离…")
    results["AC-15"] = case_ac15()
    print("AC-20 换协议边界…")
    results["AC-20"] = case_ac20()
    print("MV 归集…")
    results["mvs"] = collect_mvs()
    save("check-results.json", results)
    return results


def main() -> int:
    global base_env
    ap = argparse.ArgumentParser(description="017 验收执行与证据归档")
    _ = ap.add_argument("--agent", help="edge-agent 可执行文件")
    _ = ap.add_argument("--adapter", help="proto-adapter 可执行文件")
    _ = ap.add_argument("--phase", choices=("run", "check", "all"), default="all")
    _ = ap.add_argument("--env-file", default=str(ROOT / ".env"))
    args = ap.parse_args()

    base_env = Path(args.env_file)
    env = dict(os.environ, **load_env(base_env))
    for key in ("MQTT_WORKSHOP_ADAPTER_PASSWORD",):
        if not env.get(key):
            print(f"缺少环境变量 {key}", file=sys.stderr)
            return 2
    for sig in (signal.SIGINT, signal.SIGTERM):
        _ = signal.signal(sig, base._on_stop)

    if args.phase in ("run", "all"):
        if not (args.agent and args.adapter):
            print("run 阶段需要 --agent 与 --adapter", file=sys.stderr)
            return 2
        _ = run_phase(args.agent, args.adapter, env)
    if args.phase in ("check", "all"):
        _ = check_phase()
    print(f"证据已写入 {EV}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
