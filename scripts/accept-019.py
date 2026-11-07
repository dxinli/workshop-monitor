#!/usr/bin/env python3
"""019 通知接收端 wecom-mock 的验收。

自己起一个接收端进程（临时记录库），六条用例全部走真实 HTTP，
不 import 被测代码、不调内部函数——能被 curl 打到的东西才叫「可访问的接收地址」。

期望值全部是接口规范 §11.5 的字面量：字段名、`notifyId` 不改写、
`backfill=true` 标灰。时间串另用 `ZoneInfo("Asia/Shanghai")` 独立换算，
与被测实现里那条固定 +8 的路径不同源，对上了才算数。

用法：
    .venv-sim/bin/python scripts/accept-019.py --evidence docs/evidence/019
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime
from http.client import HTTPResponse
from pathlib import Path
from typing import TypedDict, cast
from zoneinfo import ZoneInfo

CST = ZoneInfo("Asia/Shanghai")
SCRIPT = Path("sim/wecom-mock/main.py")


class _CardAlarm(TypedDict):
    alarmId: str
    level: int
    levelText: str
    workshop: str
    terminalId: str
    deviceText: str
    pointText: str
    alarmType: str
    alarmTypeText: str
    triggerValue: float
    unit: str
    threshold: int
    occurTime: int
    reason: str
    backfill: bool


class _Card(TypedDict):
    notifyId: str
    channel: str
    msgtype: str
    alarm: _CardAlarm
    markdown: dict[str, str]


# 接口规范 §11.5 示例报文。刻意保留 `threshold`——mock 要能吃下这个字段；
# 另一条用例会发不带 `threshold` 的版本，因为 flows.json 不产出它。
CARD: _Card = {
    "notifyId": "NT-ESN-ZX-0001-A.1.motor_temp-1793500200123",
    "channel": "wecom-mock",
    "msgtype": "markdown",
    "alarm": {
        "alarmId": "AL-0000123",
        "level": 3,
        "levelText": "严重",
        "workshop": "一号车间",
        "terminalId": "ESN-ZX-0001",
        "deviceText": "电机状态监测仪 · 总线A · 从站1",
        "pointText": "电机绕组温度",
        "alarmType": "HIGH_HIGH_LIMIT",
        "alarmTypeText": "高高限",
        "triggerValue": 108.0,
        "unit": "℃",
        "threshold": 105,
        "occurTime": 1793500200123,
        "reason": "连续 3 次超过高高限 105 ℃",
        "backfill": False,
    },
    "markdown": {"content": "**【严重】电机绕组温度高高限**\n> 车间：一号车间"},
}

# 六个必需字段 → 卡片上应出现的字面量。取自 §11.5 与 rules/sim.md，
# 第 7 项 deviceText 是 012 验收口径。
EXPECTED_FIELDS = {
    "车间": "一号车间",
    "终端": "ESN-ZX-0001",
    "设备": "电机状态监测仪 · 总线A · 从站1",
    "测点": "电机绕组温度",
    "级别": "严重",
    "触发值": "108℃",
    "发生时间": "2026-11-01 10:30:00",
    "告警原因": "连续 3 次超过高高限 105 ℃",
}


def post(url: str, body: bytes) -> tuple[int, str | dict[str, object]]:
    req = urllib.request.Request(
        url, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )
    return _send(req)


def get(url: str) -> tuple[int, str | dict[str, object]]:
    return _send(urllib.request.Request(url, method="GET"))


def delete(url: str) -> tuple[int, str | dict[str, object]]:
    return _send(urllib.request.Request(url, method="DELETE"))


def _send(req: urllib.request.Request) -> tuple[int, str | dict[str, object]]:
    try:
        with cast(HTTPResponse, urllib.request.urlopen(req, timeout=5)) as resp:
            return resp.status, _decode(resp.read())
    except urllib.error.HTTPError as exc:
        return exc.code, _decode(exc.read())


def _decode(raw: bytes) -> str | dict[str, object]:
    text = raw.decode("utf-8", "replace")
    try:
        return cast(dict[str, object], json.loads(text))
    except json.JSONDecodeError:
        return text


# ── 进程 ──────────────────────────────────────────────────────


def wait_ready(base: str, proc: subprocess.Popen[bytes], seconds: float = 15.0) -> None:
    deadline = time.time() + seconds
    while time.time() < deadline:
        if proc.poll() is not None:
            raise SystemExit(f"接收端提前退出，退出码 {proc.returncode}")
        try:
            code, _ = get(f"{base}/api/v1/records?limit=1")
            if code == 200:
                return
        except (urllib.error.URLError, ConnectionError):
            pass
        time.sleep(0.2)
    raise SystemExit(f"接收端 {seconds:.0f} 秒内没有就绪")


# ── 用例 ──────────────────────────────────────────────────────


def case_full_card(base: str) -> tuple[bool, str]:
    code, body = post(f"{base}/webhook", json.dumps(CARD).encode())
    if code != 200:
        return False, f"期望 200，实际 {code}：{body}"
    _, listing_raw = get(f"{base}/api/v1/records?limit=10")
    listing = cast(dict[str, object], listing_raw)
    records = cast(list[dict[str, object]], listing["records"])
    if len(records) != 1:
        return False, f"期望 1 条记录，实际 {len(records)}"
    got = records[0]
    problems = [
        f"{k}={got.get(k)!r}（期望 {v!r}）"
        for k, v in [
            ("notifyId", CARD["notifyId"]),
            ("level", 3),
            ("workshop", "一号车间"),
            ("terminalId", "ESN-ZX-0001"),
            ("pointText", "电机绕组温度"),
            ("triggerValue", 108.0),
            ("occurTime", 1793500200123),
            ("status", "SENT"),
        ]
        if got.get(k) != v
    ]
    if problems:
        return False, "字段不符：" + "；".join(problems)
    return True, f"记录 {got['notifyId']}，状态 {got['status']}，字段逐项一致"


def case_dedupe(base: str) -> tuple[bool, str]:
    """Node-RED 按 1/5/15 秒重试，同一 notifyId 会到 4 次。"""
    _, body_before = get(f"{base}/api/v1/records?limit=1")
    before = cast(dict[str, object], body_before)["total"]
    marks: list[object] = []
    for _ in range(3):  # 已经投过 1 次，再补 3 次共 4 次
        _, body = post(f"{base}/webhook", json.dumps(CARD).encode())
        marks.append(cast(dict[str, object], body).get("duplicate"))
    _, body_after = get(f"{base}/api/v1/records?limit=1")
    after = cast(dict[str, object], body_after)["total"]
    if after != before:
        return False, f"重复投递新增了记录：{before} → {after}"
    if marks != [True, True, True]:
        return False, f"duplicate 标记期望全为 True，实际 {marks}"
    _, listing_raw = get(f"{base}/api/v1/records?limit=10")
    record = cast(list[dict[str, object]],
                  cast(dict[str, object], listing_raw)["records"])[0]
    if record["retryCount"] != 3:
        return False, f"retryCount 期望 3，实际 {record['retryCount']}"
    return True, f"投递 4 次仍是 {after} 条记录，retryCount={record['retryCount']}"


def case_invalid_not_recorded(base: str) -> tuple[bool, str]:
    _, body_before = get(f"{base}/api/v1/records?limit=1")
    before = cast(dict[str, object], body_before)["total"]
    probes: list[tuple[str, int]] = []
    bad_json = post(f"{base}/webhook", b'{"notifyId": ')
    probes.append(("非法 JSON", bad_json[0]))
    alarms = dict(CARD)
    alarm = dict(CARD["alarm"])
    del alarm["workshop"]
    alarms["alarm"] = alarm
    probes.append(("alarm 缺 workshop", post(f"{base}/webhook", json.dumps(alarms).encode())[0]))
    no_id = dict(CARD)
    del no_id["notifyId"]
    probes.append(("缺 notifyId", post(f"{base}/webhook", json.dumps(no_id).encode())[0]))
    bad_level = dict(CARD)
    bad_level["alarm"] = {**CARD["alarm"], "level": 9}
    probes.append(("level=9 越界", post(f"{base}/webhook", json.dumps(bad_level).encode())[0]))
    _, body_after = get(f"{base}/api/v1/records?limit=1")
    after = cast(dict[str, object], body_after)["total"]
    bad = [f"{name} 返回 {code}" for name, code in probes if not 400 <= code < 500]
    if bad:
        return False, "以下请求未按 4xx 拒绝：" + "；".join(bad)
    if after != before:
        return False, f"校验失败却写了记录：{before} → {after}"
    detail = "；".join(f"{name}→{code}" for name, code in probes)
    return True, f"{detail}；记录数保持 {after}"


def case_reverse_chronological(base: str) -> tuple[bool, str]:
    _ = delete(f"{base}/api/v1/records")
    for index, occur in enumerate([1793500200123, 1793500300123, 1793500400123]):
        card = cast(dict[str, object], json.loads(json.dumps(CARD)))
        card["notifyId"] = f"NT-order-{index}"
        alarm = cast(dict[str, object], card["alarm"])
        alarm["occurTime"] = occur
        _ = post(f"{base}/webhook", json.dumps(card).encode())
        time.sleep(0.02)  # 接收时刻同毫秒时按写入顺序倒序，这里只是让时序更真实
    _, listing_raw = get(f"{base}/api/v1/records?limit=10")
    listing = cast(dict[str, object], listing_raw)
    order = [r["notifyId"] for r in cast(list[dict[str, object]], listing["records"])]
    expected = ["NT-order-2", "NT-order-1", "NT-order-0"]
    if order != expected:
        return False, f"顺序期望 {expected}，实际 {order}"
    _, limited_raw = get(f"{base}/api/v1/records?limit=2")
    limited = cast(dict[str, object], limited_raw)
    limited_records = cast(list[dict[str, object]], limited["records"])
    if len(limited_records) != 2 or limited["total"] != 3:
        return False, f"limit 未生效：count={len(limited_records)} total={limited['total']}"
    return True, f"倒序 {order}；total=3 时 limit=2 返回 {len(limited_records)} 条"


def case_html_card(base: str) -> tuple[bool, str]:
    # 页面展示的是最近 N 条，先只留一条已知报文，免得 $limit 内混进别的卡。
    _ = delete(f"{base}/api/v1/records")
    _ = post(f"{base}/webhook", json.dumps(CARD).encode())
    code, html = get(f"{base}/")
    if code != 200 or not isinstance(html, str):
        return False, f"页面返回 {code}"
    missing = [
        f"{label}（{value}）" for label, value in EXPECTED_FIELDS.items()
        if label not in html or value not in html
    ]
    if missing:
        return False, "卡片缺字段：" + "、".join(missing)
    return True, f"{len(EXPECTED_FIELDS)} 项全部渲染：{'、'.join(EXPECTED_FIELDS)}"


def case_backfill_and_missing_threshold(base: str) -> tuple[bool, str]:
    _ = delete(f"{base}/api/v1/records")
    # flows.json 的「组装卡片」不产出 threshold，alarmId 也可能因回查不到而为 null。
    card = cast(dict[str, object], json.loads(json.dumps(CARD)))
    card["notifyId"] = "NT-backfill-1"
    alarm = cast(dict[str, object], card["alarm"])
    del alarm["threshold"]
    alarm["backfill"] = True
    code, _ = post(f"{base}/webhook", json.dumps(card).encode())
    if code != 200:
        return False, f"补传卡片投递返回 {code}"
    _, html = get(f"{base}/")
    if "is-backfill" not in html or "历史补传" not in html:
        return False, "补传卡片未标灰或缺少「历史补传」标识"
    if "阈值" in html or "None" in html:
        return False, "报文没有 threshold，页面却渲染了阈值行"
    _, listing_raw = get(f"{base}/api/v1/records?limit=1")
    record = cast(list[dict[str, object]],
                  cast(dict[str, object], listing_raw)["records"])[0]
    if record["backfill"] is not True or record["threshold"] is not None:
        return False, f"记录 backfill={record['backfill']} threshold={record['threshold']}"
    return True, "补传卡片带 is-backfill 与「历史补传」，无 threshold 时不渲染阈值行"


CASES = [
    ("完整报文回 200 且可查", case_full_card),
    ("同一 notifyId 重复投递不产生第二条记录", case_dedupe),
    ("校验失败回 4xx 且不写记录", case_invalid_not_recorded),
    ("记录按接收时间倒序，limit 生效", case_reverse_chronological),
    ("卡片页渲染七项字段", case_html_card),
    ("补传标灰，缺 threshold 不渲染阈值", case_backfill_and_missing_threshold),
]


def main() -> int:
    parser = argparse.ArgumentParser(description="019 验收")
    _ = parser.add_argument("--evidence", default="docs/evidence/019")
    _ = parser.add_argument("--port", type=int, default=8091, help="验收专用端口，避开常驻实例")
    args = parser.parse_args()

    evidence = Path(cast(str, args.evidence))
    evidence.mkdir(parents=True, exist_ok=True)
    db = evidence / "_records.db"
    if db.exists():
        db.unlink()
    port = cast(int, args.port)
    base = f"http://127.0.0.1:{port}"

    proc = subprocess.Popen(
        [sys.executable, str(SCRIPT), "--port", str(port), "--db", str(db)],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    results: list[dict[str, object]] = []
    try:
        wait_ready(base, proc)
        for name, fn in CASES:
            ok, detail = fn(base)
            results.append({"name": name, "passed": ok, "detail": detail})
            print(f"{'通过' if ok else '未通过'}：{name} —— {detail}")
    finally:
        proc.terminate()
        try:
            _ = proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()

    passed = all(item["passed"] for item in results)
    payload = {
        "task": "019 通知接收端 wecom-mock",
        "generatedAt": datetime.now(CST).strftime("%Y-%m-%dT%H:%M:%S%z"),
        "endpoint": f"{base}/webhook",
        "passed": passed,
        "cases": results,
    }
    _ = (evidence / "acceptance.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    for leftover in evidence.glob("_records.db*"):
        leftover.unlink()
    print(f"\n验收{'通过' if passed else '未通过'}，证据：{evidence / 'acceptance.json'}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
