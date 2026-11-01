"""工况预设的单元测试。

只测纯逻辑：配置文件载入、工程值→原始编码、按测点映射到寄存器。
不需要串口，也不需要起模拟器进程。
运行：make sim-test
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import scenes  # noqa: E402
from point_table import Bus, Device, Point, load_tree  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent.parent


def make_point(
    code: str = "motor_temp",
    *,
    addr: int = 0,
    width: int = 1,
    data_type: str = "int16",
    scale: float = 0.1,
    offset: float = 0.0,
    low: float = -20,
    high: float = 200,
    unit: str | None = "℃",
) -> Point:
    return Point(
        code=code,
        name=code,
        addr=addr,
        width=width,
        data_type=data_type,
        byte_order="AB",
        scale=scale,
        offset=offset,
        unit=unit,
        min_value=low,
        max_value=high,
        si_type="YC",
    )


def make_bus(*devices: tuple[int, int, int, tuple[Point, ...]]) -> Bus:
    """``(slave_addr, start_addr, register_count, points)``，可传多个从站。"""
    return Bus(
        terminal_id="ESN-ZX-0001",
        bus_id="A",
        baud_rate=9600,
        data_bits=8,
        stop_bits=1,
        parity="N",
        devices=tuple(
            Device(
                slave_addr=slave,
                name=f"dev{slave}",
                type="t",
                start_addr=start,
                register_count=count,
                points=points,
            )
            for slave, start, count, points in devices
        ),
    )


class TestLoad:
    def test_repository_file_has_three_scenes(self):
        # 这是接口规范 §12.1 定义的三个场景，多一个少一个都会让 N-04 的按钮对不上。
        got = scenes.load(ROOT / "config" / "scenes.json")
        assert sorted(got.scenes) == ["alarm", "normal", "safety"]

    def test_normal_scene_resets_everything_except_cumulative_energy(self):
        got = scenes.load(ROOT / "config" / "scenes.json").get("normal")
        # 14 类测点里 13 类要复位；ep_forward 是累计电能，写回小值等于让电表倒转，
        # 真实设备上不可能发生，场景里也不该假装能复位。
        assert len(got.points) == 13
        assert "ep_forward" not in got.points
        assert got.clear_faults is True

    def test_alarm_scene_covers_three_alarm_types(self):
        rules = json.loads((ROOT / "config" / "thresholds.json").read_text(encoding="utf-8"))["rules"]
        got = scenes.load(ROOT / "config" / "scenes.json").get("alarm")
        hit = set()
        for code, value in got.points.items():
            for rule in rules:
                if code not in rule["pointCodes"]:
                    continue
                op, limit = rule["operator"], rule["thresholdValue"]
                if (op == ">" and value > limit) or (op == "<" and value < limit) or (op == ">=" and value >= limit):
                    hit.add(rule["alarmType"])
        # 演示时要一次看到高高限、高限、低限三种，缺一种现场就少演一类。
        assert hit == {"HIGH_HIGH_LIMIT", "HIGH_LIMIT", "LOW_LIMIT"}

    def test_safety_scene_sets_all_three_outputs(self):
        got = scenes.load(ROOT / "config" / "scenes.json").get("safety")
        assert got.points == {"door": 1.0, "estop": 1.0, "light_curtain": 1.0}

    def test_unknown_scene_lists_known_names(self):
        got = scenes.load(ROOT / "config" / "scenes.json")
        with pytest.raises(ValueError, match="alarm、normal、safety"):
            got.get("bogus")

    def test_missing_points_key_is_rejected(self, tmp_path):
        path = tmp_path / "scenes.json"
        path.write_text(json.dumps({"configVersion": "1.0.0", "scenes": {"x": {}}}), encoding="utf-8")
        with pytest.raises(ValueError, match="points"):
            scenes.load(path)

    def test_bool_value_is_rejected(self, tmp_path):
        # true 在 Python 里是 int 的子类，不显式挡会当成 1 写进寄存器。
        path = tmp_path / "scenes.json"
        path.write_text(
            json.dumps({"configVersion": "1.0.0", "scenes": {"x": {"points": {"door": True}}}}),
            encoding="utf-8",
        )
        with pytest.raises(ValueError, match="不是数字"):
            scenes.load(path)


class TestEngineeringToRaw:
    def test_int16_uses_scale_and_offset(self):
        point = make_point(scale=0.1, offset=0.0)
        assert scenes.engineering_to_raw(point, 60) == 600

    def test_int16_negative_value(self):
        point = make_point(scale=0.1, low=-20)
        assert scenes.engineering_to_raw(point, -5) == -50

    def test_uint16_rounds_to_nearest(self):
        point = make_point(code="ia", data_type="uint16", scale=0.01, low=0, high=200, unit="A")
        assert scenes.engineering_to_raw(point, 85) == 8500

    def test_air_pressure_keeps_three_decimals(self):
        # 0.38 MPa 与 0.001 的 scale 相乘要正好落在 380，不能因为浮点误差变成 379。
        point = make_point(
            code="air_pressure", data_type="uint16", scale=0.001, low=0, high=1.6, unit="MPa"
        )
        assert scenes.engineering_to_raw(point, 0.38) == 380

    def test_uint32_wide_point(self):
        point = make_point(
            code="ep_forward", data_type="uint32", scale=0.01, low=0, high=42949672.95, unit="kWh"
        )
        assert scenes.engineering_to_raw(point, 12345.67) == 1234567

    def test_out_of_range_is_rejected_not_clamped(self):
        # 静默截断会得到「看起来正常但没有越限」的现场，是最难排查的一类错。
        point = make_point(low=-20, high=200)
        with pytest.raises(ValueError, match="超出量程"):
            scenes.engineering_to_raw(point, 300)


class TestPlanBus:
    def test_only_declared_points_are_written(self):
        bus = make_bus(
            (1, 0, 2, (make_point(addr=0), make_point("motor_vib", addr=1, data_type="uint16", scale=0.01, low=0, high=50))),
        )
        scene = scenes.Scene(name="alarm", description="", clear_faults=False, points={"motor_temp": 108})
        got = scenes.plan_bus(bus, scene)
        assert got == [(1, 0, [1080])]

    def test_wide_point_splits_high_word_first(self):
        bus = make_bus(
            (1, 0, 2, (make_point("ep_forward", addr=0, width=2, data_type="uint32", scale=0.01, low=0, high=42949672.95),)),
        )
        scene = scenes.Scene(name="x", description="", clear_faults=False, points={"ep_forward": 12345.67})
        assert scenes.plan_bus(bus, scene) == [(1, 0, [0x0012, 0xD687])]

    def test_offset_inside_device_block(self):
        # 从站块的起始地址不一定是 0，写地址要加上块偏移。
        bus = make_bus(
            (1, 100, 4, (make_point("door", addr=2, data_type="uint16", scale=1, low=0, high=1, unit=None),)),
        )
        scene = scenes.Scene(name="safety", description="", clear_faults=False, points={"door": 1})
        assert scenes.plan_bus(bus, scene) == [(1, 102, [1])]

    def test_unknown_code_is_reported(self):
        bus = make_bus((1, 0, 2, (make_point(addr=0),)))
        scene = scenes.Scene(name="x", description="", clear_faults=False, points={"nope": 1})
        assert scenes.unknown_codes(scene, bus) == ["nope"]

    def test_scene_from_repo_fits_real_point_table(self):
        # 仓库里的 scenes.json 要能对上真实点表，否则模拟器启动后一按场景就 400。
        table = load_tree(ROOT / "config" / "terminals")[0]
        scene_set = scenes.load(ROOT / "config" / "scenes.json")
        for name, scene in scene_set.scenes.items():
            for bus in table.buses:
                assert scenes.unknown_codes(scene, bus) == [], f"{name} 有不在点表里的测点"
                for slave_addr, start, words in scenes.plan_bus(bus, scene):
                    assert all(0 <= w <= 0xFFFF for w in words), f"{name} 写到 {slave_addr}:{start} 的字越界"