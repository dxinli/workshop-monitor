"""卡片渲染与投递记录的单元测试。

只测纯逻辑：渲染规则与去重。不起 HTTP 服务、不连网络——
端到端由 ``scripts/accept-019.py`` 负责。

运行：``make sim-test``
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent))

import card  # noqa: E402
import store  # noqa: E402

# 期望值不取自实现：
#   - 时间串由 `zoneinfo.ZoneInfo("Asia/Shanghai")` 独立换算得出（实现里用的是
#     固定 +8 偏移，两条不同的路径算出同一个结果才算对上）
#   - 字段名与级别中文取自接口规范 §11.5 与 §11.3 的字面量
OCCUR_MS = 1793500200123
OCCUR_TEXT = "2026-11-01 10:30:00"


def _alarm(**overrides: Any) -> card.Alarm:
    """`flows.json`「组装卡片」节点的真实产出形状：没有 threshold。"""
    base = card.Alarm(
        alarmId="AL-0000123",
        level=3,
        levelText="严重",
        workshop="一号车间",
        terminalId="ESN-ZX-0001",
        deviceText="电机状态监测仪 · 总线A · 从站1",
        pointText="电机绕组温度",
        alarmType="HIGH_HIGH_LIMIT",
        alarmTypeText="高高限",
        triggerValue=108.0,
        unit="℃",
        occurTime=OCCUR_MS,
        reason="连续 3 次超过高高限 105 ℃",
        backfill=False,
    )
    # 覆盖值都是测试自备的合法字面量；跳过 pydantic 校验，与直接给字段赋值等价。
    return base.model_copy(update=overrides) if overrides else base


def _payload(notify_id: str = "NT-ESN-ZX-0001-A.1.motor_temp-1793500200123",
             **alarm: Any) -> dict[str, Any]:
    alarm_body = _alarm(**alarm).model_dump()
    return {
        "notifyId": notify_id,
        "channel": "wecom-mock",
        "msgtype": "markdown",
        "alarm": alarm_body,
        "markdown": {"content": "**【严重】电机绕组温度高高限**"},
    }


class TestCardFields:
    def test_time_uses_shanghai_offset(self):
        assert card.format_time(OCCUR_MS) == OCCUR_TEXT

    def test_card_renders_all_seven_fields(self):
        html = card.render_card(_alarm())
        for label, value in [
            ("车间", "一号车间"),
            ("终端", "ESN-ZX-0001"),
            ("设备", "电机状态监测仪 · 总线A · 从站1"),
            ("测点", "电机绕组温度"),
            ("级别", "严重"),
            ("触发值", "108℃"),
            ("发生时间", OCCUR_TEXT),
            ("告警原因", "连续 3 次超过高高限 105 ℃"),
        ]:
            assert label in html, label
            assert value in html, value

    def test_title_combines_point_and_type(self):
        assert card.card_title(_alarm()) == "电机绕组温度高高限"

    def test_absent_threshold_renders_nothing(self):
        """`flows.json` 不产出 threshold，卡片不能出现「阈值 None」。"""
        html = card.render_card(_alarm())
        assert "阈值" not in html
        assert "None" not in html
        assert "null" not in html

    def test_present_threshold_renders_with_unit(self):
        html = card.render_card(_alarm(threshold=105))
        assert "105℃" in html

    def test_absent_unit_renders_bare_number(self):
        assert card.trigger_text(_alarm(triggerValue=108.0, unit=None)) == "108"

    def test_fractional_value_keeps_significant_digits(self):
        assert card.trigger_text(_alarm(triggerValue=2.35, unit="mm/s")) == "2.35mm/s"

    @pytest.mark.parametrize(("level", "text"), [(1, "提示"), (2, "警告"), (3, "严重"), (4, "紧急")])
    def test_level_text_falls_back_to_table(self, level: int, text: str):
        assert card.level_label(_alarm(level=level, levelText=None)) == text

    def test_upstream_level_text_wins(self):
        assert card.level_label(_alarm(level=3, levelText="自定义")) == "自定义"

    def test_absent_optional_fields_are_skipped(self):
        html = card.render_card(_alarm(deviceText=None, reason=None))
        assert "设备" not in html
        assert "告警原因" not in html
        assert "车间" in html

    def test_markup_in_values_is_escaped(self):
        html = card.render_card(_alarm(pointText="<script>alert(1)</script>"))
        assert "<script>" not in html
        assert "&lt;script&gt;" in html


class TestBackfillRendering:
    def test_backfill_card_is_greyed_and_badged(self):
        html = card.render_card(_alarm(backfill=True))
        assert "is-backfill" in html
        assert "历史补传" in html

    def test_live_card_has_no_backfill_mark(self):
        html = card.render_card(_alarm(backfill=False))
        assert "is-backfill" not in html
        assert "历史补传" not in html


class TestStore:
    def test_first_save_is_not_duplicate(self):
        s = store.RecordStore(":memory:")
        record, duplicate = s.save(card.NotifyCard(**_payload()), 1_000)
        assert duplicate is False
        assert record.retryCount == 0
        assert record.status == store.STATUS_SENT

    def test_repeat_save_is_deduplicated(self):
        s = store.RecordStore(":memory:")
        _ = s.save(card.NotifyCard(**_payload()), 1_000)
        _, duplicate = s.save(card.NotifyCard(**_payload()), 1_000)
        assert duplicate is True
        assert s.count() == 1

    def test_repeat_save_counts_retries(self):
        s = store.RecordStore(":memory:")
        # Node-RED 首次 + 1/5/15 秒重试，共 4 次（BR-41）
        records = [s.save(card.NotifyCard(**_payload()), 1_000)[0] for _ in range(4)]
        assert records[-1].retryCount == 3
        assert s.count() == 1

    def test_repeat_save_does_not_overwrite_first_values(self):
        """重试带了不同触发值时，卡片必须还是首次那一份。"""
        s = store.RecordStore(":memory:")
        _ = s.save(card.NotifyCard(**_payload(triggerValue=108.0)), 1_000)
        record, _ = s.save(card.NotifyCard(**_payload(triggerValue=120.0)), 2_000)
        assert record.triggerValue == 108.0
        assert record.receiveTime == 1_000

    def test_recent_is_reverse_chronological(self):
        s = store.RecordStore(":memory:")
        for i, ts in enumerate([1_000, 2_000, 3_000]):
            _ = s.save(card.NotifyCard(**_payload(notify_id=f"NT-{i}")), ts)
        assert [r.notifyId for r in s.recent(10)] == ["NT-2", "NT-1", "NT-0"]

    def test_recent_breaks_same_timestamp_by_insert_desc(self):
        s = store.RecordStore(":memory:")
        _ = s.save(card.NotifyCard(**_payload(notify_id="NT-a")), 1_000)
        _ = s.save(card.NotifyCard(**_payload(notify_id="NT-b")), 1_000)
        assert [r.notifyId for r in s.recent(10)] == ["NT-b", "NT-a"]

    def test_recent_respects_limit(self):
        s = store.RecordStore(":memory:")
        for i in range(5):
            _ = s.save(card.NotifyCard(**_payload(notify_id=f"NT-{i}")), 1_000 + i)
        assert len(s.recent(2)) == 2
        assert s.count() == 5

    def test_records_survive_reopen(self, tmp_path: Path):
        path = tmp_path / "records.db"
        first = store.RecordStore(str(path))
        _ = first.save(card.NotifyCard(**_payload()), 1_000)
        first.close()

        second = store.RecordStore(str(path))
        try:
            assert second.count() == 1
            assert second.recent(1)[0].notifyId.startswith("NT-ESN-ZX-0001")
        finally:
            second.close()

    def test_clear_empties_records(self):
        s = store.RecordStore(":memory:")
        _ = s.save(card.NotifyCard(**_payload()), 1_000)
        s.clear()
        assert s.count() == 0
        assert s.recent(10) == []

    def test_backfill_flag_round_trips(self):
        s = store.RecordStore(":memory:")
        _ = s.save(card.NotifyCard(**_payload(backfill=True)), 1_000)
        assert s.recent(1)[0].backfill is True


class TestNotifyCardValidation:
    """校验在 pydantic 层，必填缺失即抛；HTTP 层据此回 4xx 且不写记录。"""

    def test_full_payload_is_accepted(self):
        assert card.NotifyCard(**_payload()).notifyId.startswith("NT-")

    @pytest.mark.parametrize("field", ["notifyId", "msgtype", "alarm", "markdown"])
    def test_missing_top_level_field_rejected(self, field: str):
        payload = _payload()
        payload.pop(field)
        with pytest.raises(Exception):
            _ = card.NotifyCard(**payload)

    @pytest.mark.parametrize("field", ["level", "workshop", "terminalId", "pointText",
                                       "alarmType", "triggerValue", "occurTime"])
    def test_missing_alarm_field_rejected(self, field: str):
        alarm = _alarm().model_dump()
        alarm.pop(field)
        payload = _payload()
        payload["alarm"] = alarm
        with pytest.raises(Exception):
            _ = card.NotifyCard(**payload)

    @pytest.mark.parametrize("value", [0, 5])
    def test_level_outside_one_to_four_rejected(self, value: int):
        payload = _payload()
        payload["alarm"]["level"] = value
        with pytest.raises(Exception):
            _ = card.NotifyCard(**payload)

    def test_unknown_msgtype_rejected(self):
        payload = _payload()
        payload["msgtype"] = "text"
        with pytest.raises(Exception):
            _ = card.NotifyCard(**payload)

    def test_threshold_and_alarm_id_may_be_absent(self):
        """上游不产出 threshold、告警回查不到时 alarmId 为 null，都不能当必填。"""
        alarm = _alarm().model_dump()
        alarm.pop("threshold", None)
        alarm["alarmId"] = None
        alarm["unit"] = None
        alarm["reason"] = None
        payload = _payload()
        payload["alarm"] = alarm
        assert card.NotifyCard(**payload).alarm.alarmId is None