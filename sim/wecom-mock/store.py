"""投递记录存储（按 `notifyId` 去重，按接收时间倒序可查）。

字段刻意与 `deploy/timescaledb/init/01_schema.sql` 的 `notify_log` 对齐
（`notify_id` / `channel` / `status` / `retry_count` / `http_code` / `fail_reason` /
`sent_time`），这样「Node-RED 说发出了几条、mock 收到了几条」可以逐条对照，
而不是各数各的。

去重是唯一有状态的行为：`notifyId` 是幂等键，Node-RED 按 1/5/15 秒重试时
同一个 `notifyId` 会到 4 次。**重试不产生新卡片**，但重复次数要计入 `retry_count`——
它是判断「通道到底重试了几次」的唯一依据，吞掉就看不出重试阶梯在不在工作。

用标准库 `sqlite3` 而不加新依赖：`requirements.txt` 的版本在 AGENTS.md 里锁死，
为这点持久化再引一个 ORM 不划算。WAL + 单连接 + 互斥锁足够——
FastAPI 的同步端点跑在线程池里，真的会并发进到这里。
"""

from __future__ import annotations

import sqlite3
import threading
from dataclasses import asdict, dataclass
from typing import Any

from card import NotifyCard

STATUS_SENT = "SENT"

_SCHEMA = """
CREATE TABLE IF NOT EXISTS record (
    seq           INTEGER PRIMARY KEY AUTOINCREMENT,
    notify_id     TEXT NOT NULL UNIQUE,
    channel       TEXT NOT NULL,
    status        TEXT NOT NULL,
    retry_count   INTEGER NOT NULL DEFAULT 0,
    http_code     INTEGER,
    fail_reason   TEXT,
    sent_time     INTEGER NOT NULL,   -- 首次接收时刻，毫秒；对应 notify_log.sent_time
    level         INTEGER NOT NULL,
    level_text    TEXT,
    workshop      TEXT NOT NULL,
    terminal_id   TEXT NOT NULL,
    device_text   TEXT,
    point_text    TEXT NOT NULL,
    alarm_type    TEXT NOT NULL,
    alarm_type_text TEXT,
    trigger_value REAL NOT NULL,
    unit          TEXT,
    threshold     REAL,
    occur_time    INTEGER NOT NULL,
    reason        TEXT,
    backfill      INTEGER NOT NULL DEFAULT 0,
    alarm_id      TEXT,
    content       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS record_sent_time ON record (sent_time DESC, seq DESC);
"""


@dataclass(frozen=True)
class Record:
    """一条投递记录。字段名对 JSON 用小驼峰，对库列用下划线。"""

    seq: int
    notifyId: str
    channel: str
    status: str
    retryCount: int
    httpCode: int | None
    failReason: str | None
    receiveTime: int
    level: int
    levelText: str
    workshop: str
    terminalId: str
    deviceText: str | None
    pointText: str
    alarmType: str
    alarmTypeText: str
    triggerValue: float
    unit: str | None
    threshold: float | None
    occurTime: int
    reason: str | None
    backfill: bool
    alarmId: str | None
    content: str

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


class RecordStore:
    """记录的落库与查询。`path` 为 `":memory:"` 时纯内存，测试用。"""

    def __init__(self, path: str) -> None:
        self._lock: threading.Lock = threading.Lock()
        self._conn: sqlite3.Connection = sqlite3.connect(path, check_same_thread=False)
        self._conn.row_factory = sqlite3.Row
        if path != ":memory:":
            # WAL 让「页面读」不阻塞「投递写」，演示时页面开着也不会卡住接收。
            _ = self._conn.execute("PRAGMA journal_mode=WAL")
        _ = self._conn.executescript(_SCHEMA)
        self._conn.commit()

    def close(self) -> None:
        with self._lock:
            self._conn.close()

    def save(self, card: NotifyCard, receive_time: int) -> tuple[Record, bool]:
        """写入一条记录，返回 (记录, 是否重复)。

        重复时**只累加 `retry_count`**，其余字段一律不动：`notifyId` 是幂等键，
        同一键的第二次投递不该改写第一次的触发值与发生时间——
        否则重试比首次带了不同数值时，卡片显示的会是最新一次而不是真实触发。
        """
        alarm = card.alarm
        with self._lock:
            # 先查后判，不用 `ON CONFLICT` 的 rowcount：UPSERT 走 DO UPDATE 时
            # sqlite3 的 rowcount 同样是 1，判不出「这次是新增还是命中已有」。
            # 这里的「先查后写」不违反并发安全——整段在同一个互斥锁内。
            existing = self._conn.execute(
                "SELECT retry_count FROM record WHERE notify_id = ?", (card.notifyId,)
            ).fetchone()
            if existing is None:
                _ = self._conn.execute(
                    """
                    INSERT INTO record (notify_id, channel, status, retry_count, http_code,
                                        sent_time, level, level_text, workshop, terminal_id,
                                        device_text, point_text, alarm_type, alarm_type_text,
                                        trigger_value, unit, threshold, occur_time, reason,
                                        backfill, alarm_id, content)
                    VALUES (?, ?, ?, 0, 200, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        card.notifyId,
                        card.channel,
                        STATUS_SENT,
                        receive_time,
                        alarm.level,
                        alarm.levelText,
                        alarm.workshop,
                        alarm.terminalId,
                        alarm.deviceText,
                        alarm.pointText,
                        alarm.alarmType,
                        alarm.alarmTypeText,
                        alarm.triggerValue,
                        alarm.unit,
                        alarm.threshold,
                        alarm.occurTime,
                        alarm.reason,
                        1 if alarm.backfill else 0,
                        alarm.alarmId,
                        card.markdown.content,
                    ),
                )
                duplicate = False
            else:
                _ = self._conn.execute(
                    "UPDATE record SET retry_count = retry_count + 1 WHERE notify_id = ?",
                    (card.notifyId,),
                )
                duplicate = True
            self._conn.commit()
            row = self._conn.execute(
                "SELECT * FROM record WHERE notify_id = ?", (card.notifyId,)
            ).fetchone()
        return _to_record(row), duplicate

    def recent(self, limit: int) -> list[Record]:
        """按接收时间倒序取最近 `limit` 条。同毫秒到达的按写入顺序倒序。"""
        with self._lock:
            rows = self._conn.execute(
                "SELECT * FROM record ORDER BY sent_time DESC, seq DESC LIMIT ?",
                (limit,),
            ).fetchall()
        return [_to_record(row) for row in rows]

    def count(self) -> int:
        with self._lock:
            return int(self._conn.execute("SELECT COUNT(*) FROM record").fetchone()[0])

    def clear(self) -> None:
        """清空全部记录。验收脚本要能重复跑，每次从零开始。"""
        with self._lock:
            _ = self._conn.execute("DELETE FROM record")
            self._conn.commit()


def _to_record(row: sqlite3.Row) -> Record:
    return Record(
        seq=row["seq"],
        notifyId=row["notify_id"],
        channel=row["channel"],
        status=row["status"],
        retryCount=row["retry_count"],
        httpCode=row["http_code"],
        failReason=row["fail_reason"],
        receiveTime=row["sent_time"],
        level=row["level"],
        levelText=row["level_text"] or "",
        workshop=row["workshop"],
        terminalId=row["terminal_id"],
        deviceText=row["device_text"],
        pointText=row["point_text"],
        alarmType=row["alarm_type"],
        alarmTypeText=row["alarm_type_text"] or "",
        triggerValue=row["trigger_value"],
        unit=row["unit"],
        threshold=row["threshold"],
        occurTime=row["occur_time"],
        reason=row["reason"],
        backfill=bool(row["backfill"]),
        alarmId=row["alarm_id"],
        content=row["content"],
    )