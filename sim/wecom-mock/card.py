"""通知报文模型与卡片渲染（接口规范 §11.5）。

校验口径按**上游实际产出**定，不按文档示例定。`deploy/node-red/flows.json`
的「组装卡片」节点有三处与 §11.5 的示例不同：

- 不产出 `threshold`（示例里有）
- 告警回查不到时 `alarmId` 为 `null`
- 测点没配单位时 `unit` 为 `null`，没有原因时 `reason` 为 `null`

把这三个当必填，联调第一条就走不通。这里只有六项「渲染必需」是必填：
`notifyId`、`level`、`workshop`、`terminalId`、`pointText`、`triggerValue`、`occurTime`、
`alarmType`、`markdown.content`——少任何一项卡片都画不完整，其余允许缺省或为空。
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from html import escape
from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field

# 中国自 1991 年起不实行夏令时，固定 +8 与 Asia/Shanghai 等价。
# 用固定偏移而不是 ZoneInfo：宿主或容器里没装 tzdata 时 ZoneInfo 会抛异常，
# 而时间显示是每张卡片都要走的路。
CST = timezone(timedelta(hours=8))

LEVEL_TEXT = {1: "提示", 2: "警告", 3: "严重", 4: "紧急"}


class Alarm(BaseModel):
    """告警的结构化字段。供渲染与后续接真实通道。"""

    model_config = ConfigDict(extra="allow")

    alarmId: str | None = None
    level: Annotated[int, Field(ge=1, le=4)]
    levelText: str | None = None
    workshop: Annotated[str, Field(min_length=1)]
    terminalId: Annotated[str, Field(min_length=1)]
    deviceText: str | None = None
    pointText: Annotated[str, Field(min_length=1)]
    alarmType: Annotated[str, Field(min_length=1)]
    alarmTypeText: str | None = None
    triggerValue: float
    unit: str | None = None
    threshold: float | None = None
    occurTime: int
    reason: str | None = None
    backfill: bool = False


class Markdown(BaseModel):
    content: Annotated[str, Field(min_length=1)]


class NotifyCard(BaseModel):
    """`POST /webhook` 的请求体。"""

    model_config = ConfigDict(extra="allow")

    notifyId: Annotated[str, Field(min_length=1)]
    channel: str = ""
    # 只支持 markdown：企业微信机器人还有 text / news / template_card 等类型，
    # 本模拟器只承接流程实际会发的这一种，收到别的类型按校验失败处理。
    msgtype: Literal["markdown"]
    alarm: Alarm
    markdown: Markdown


def format_time(occur_ms: int) -> str:
    return datetime.fromtimestamp(occur_ms / 1000, CST).strftime("%Y-%m-%d %H:%M:%S")


def level_label(alarm: Alarm) -> str:
    """优先用上游给的 `levelText`，缺省时按级别表补齐。"""
    return alarm.levelText or LEVEL_TEXT.get(alarm.level, str(alarm.level))


def trigger_text(alarm: Alarm) -> str:
    """触发值带单位。没有单位时只给数字，不补一个假的。"""
    return f"{alarm.triggerValue:g}{alarm.unit or ''}"


def threshold_text(alarm: Alarm) -> str:
    """阈值为空返回空串：`flows.json` 不产出该字段，卡片不该出现「阈值 None」。"""
    if alarm.threshold is None:
        return ""
    return f"{alarm.threshold:g}{alarm.unit or ''}"


def card_fields(alarm: Alarm) -> list[tuple[str, str]]:
    """卡片上要显示的字段，顺序即渲染顺序。

    `.claude/rules/sim.md` 要求的六项（车间、终端、测点、级别、触发值、发生时间、
    告警原因）全在这里；`deviceText` 是第 7 项，012 的验收按 7 个字段核对。
    """
    fields = [
        ("车间", alarm.workshop),
        ("终端", alarm.terminalId),
        ("设备", alarm.deviceText or ""),
        ("测点", alarm.pointText),
        ("级别", level_label(alarm)),
        ("触发值", trigger_text(alarm)),
        ("发生时间", format_time(alarm.occurTime)),
        ("告警原因", alarm.reason or ""),
    ]
    return [(name, value) for name, value in fields if value]


def card_title(alarm: Alarm) -> str:
    suffix = alarm.alarmTypeText or alarm.alarmType
    return f"{alarm.pointText}{suffix}"


def render_card(alarm: Alarm) -> str:
    """渲染一张企业微信机器人风格的卡片。

    `backfill=true` 的卡片带 `is-backfill` 类：整卡降透明度、去饱和，并加角标。
    补传的告警是历史事件，视觉上不能与实时告警同权重。
    """
    classes = "card is-backfill" if alarm.backfill else "card"
    rows = "".join(
        f'<div class="row"><span class="k">{escape(name)}</span>'
        f'<span class="v">{escape(value)}</span></div>'
        for name, value in card_fields(alarm)
    )
    threshold = threshold_text(alarm)
    threshold_row = (
        f'<div class="row"><span class="k">阈值</span>'
        f'<span class="v">{escape(threshold)}</span></div>'
        if threshold
        else ""
    )
    badge = '<span class="badge">历史补传</span>' if alarm.backfill else ""
    return (
        f'<div class="{classes}" data-level="{alarm.level}">'
        f'<div class="head"><span class="level level-{alarm.level}">'
        f"{escape(level_label(alarm))}</span>{badge}</div>"
        f'<div class="title">{escape(card_title(alarm))}</div>'
        f'<div class="body">{rows}{threshold_row}</div>'
        f"</div>"
    )