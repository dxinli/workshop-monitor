"""point_table 与 registers 的单元测试。

只测纯逻辑：点表解析、寄存器默认值与边界。不碰串口与网络。
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import point_table  # noqa: E402
from registers import RegisterStore  # noqa: E402

ROOT = Path(__file__).resolve().parents[2]


def _terminal(point_overrides: dict | None = None, **root_overrides) -> dict:
    point = {
        "code": "motor_temp",
        "name": "电机绕组温度",
        "siType": "YC",
        "dataType": "int16",
        "byteOrder": "AB",
        "addr": 0,
        "width": 1,
        "scale": 0.1,
        "offset": 0,
        "unit": "℃",
        "minValue": -20,
        "maxValue": 200,
    }
    point.update(point_overrides or {})
    data = {
        "terminalId": "ESN-ZX-9999",
        "configVersion": "1.0.0",
        "workshop": "一号车间",
        "line": "机加工产线",
        "collectIntervalMs": 5000,
        "publishBatch": False,
        "buses": [
            {
                "busId": "A",
                "mode": "rtu-serial",
                "port": "/dev/null",
                "baudRate": 9600,
                "dataBits": 8,
                "stopBits": 1,
                "parity": "N",
                "timeoutMs": 1000,
                "retryCount": 2,
                "interFrameMs": 4,
                "devices": [
                    {
                        "slaveAddr": 1,
                        "name": "电机状态监测仪",
                        "type": "motor_monitor",
                        "blocks": [
                            {
                                "functionCode": 3,
                                "startAddr": 0,
                                "registerCount": 1,
                                "points": [point],
                            }
                        ],
                    }
                ],
            }
        ],
    }
    data.update(root_overrides)
    return data


def _write(tmp_path: Path, data: dict) -> Path:
    path = tmp_path / "ESN-ZX-9999.json"
    path.write_text(json.dumps(data, ensure_ascii=False), encoding="utf-8")
    return path


def test_loads_repository_point_tables():
    tables = point_table.load_tree(ROOT / "config" / "terminals")
    assert [t.terminal_id for t in tables] == [f"ESN-ZX-000{n}" for n in range(1, 6)]
    assert sum(t.point_count for t in tables) == 140
    assert sum(len(t.buses) for t in tables) == 10


def test_bus_key_includes_terminal():
    tables = point_table.load_tree(ROOT / "config" / "terminals")
    keys = {bus.key for table in tables for bus in table.buses}
    assert len(keys) == 10
    # busId 只有 A / B，键不带终端就会塌缩成 2 条
    assert {bus.bus_id for table in tables for bus in table.buses} == {"A", "B"}
    assert "ESN-ZX-0001.A" in keys


def test_terminal_schema_shape_is_enforced(tmp_path):
    data = _terminal()
    del data["terminalId"]
    with pytest.raises(ValueError, match="terminalId"):
        point_table.load_terminal(_write(tmp_path, data))


def test_width_must_match_data_type(tmp_path):
    with pytest.raises(ValueError, match="宽度应为 2"):
        point_table.load_terminal(
            _write(tmp_path, _terminal({"dataType": "uint32", "width": 1}))
        )


def test_unknown_data_type_rejected(tmp_path):
    with pytest.raises(ValueError, match="不支持的数据类型"):
        point_table.load_terminal(_write(tmp_path, _terminal({"dataType": "float32"})))


def test_point_must_fit_block(tmp_path):
    with pytest.raises(ValueError, match="超出块"):
        point_table.load_terminal(_write(tmp_path, _terminal({"addr": 5})))


def test_duplicate_slave_rejected(tmp_path):
    data = _terminal()
    data["buses"][0]["devices"].append(data["buses"][0]["devices"][0])
    with pytest.raises(ValueError, match="从站地址重复"):
        point_table.load_terminal(_write(tmp_path, data))


def test_non_holding_function_code_rejected(tmp_path):
    data = _terminal()
    data["buses"][0]["devices"][0]["blocks"][0]["functionCode"] = 4
    with pytest.raises(ValueError, match="功能码只能是 03"):
        point_table.load_terminal(_write(tmp_path, data))


class TestRegisterStore:
    def _store(self) -> tuple[point_table.Bus, RegisterStore]:
        bus = point_table.load_tree(ROOT / "config" / "terminals")[0].buses[0]
        return bus, RegisterStore(bus)

    def test_baseline_is_midpoint_of_range(self):
        _bus, store = self._store()
        # 电机绕组温度量程 -20~200，中点 90.0 ℃，scale 0.1 → 原始值 900
        assert store.read(1, 0, 1) == [900]

    def test_baseline_splits_32bit_high_word_first(self):
        bus, store = self._store()
        device = next(d for d in bus.devices if d.slave_addr == 2)
        point = next(p for p in device.points if p.width == 2)
        # 电能 0~42949672.95，中点 21474836.475 kWh ÷ 0.01 = 2147483647.5，
        # 四舍五入到偶数得 2147483648（0x80000000）→ 高字 0x8000、低字 0x0000。
        assert store.point_raw(2, point) == [0x8000, 0x0000]

    def test_write_then_read(self):
        _bus, store = self._store()
        store.write(1, 0, 625)
        assert store.read(1, 0, 1) == [625]

    def test_write_rejects_out_of_range_register(self):
        _bus, store = self._store()
        with pytest.raises(ValueError, match="uint16"):
            store.write(1, 0, 0x10000)

    def test_write_rejects_address_outside_block(self):
        _bus, store = self._store()
        with pytest.raises(ValueError, match="超出块"):
            store.write(1, 99, 1)

    def test_unknown_slave_rejected(self):
        _bus, store = self._store()
        with pytest.raises(KeyError):
            store.read(9, 0, 1)

    def test_snapshot_covers_whole_block(self):
        _bus, store = self._store()
        snapshot = store.snapshot(2)
        assert [row["addr"] for row in snapshot] == list(range(7))