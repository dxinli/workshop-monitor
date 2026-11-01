"""工况预设（接口规范 §12.1 的 ``/api/v1/scene/{name}``）。

预设按**测点编码**声明工程值，不按寄存器地址。理由：寄存器地址属于点表，
点表改了（V-06 块内不重叠、V-07 无孤立点）场景定义不该跟着改；而测点编码
是点表里的稳定标识。

换算用的是点表里的 ``scale`` / ``offset``，与 edge-agent 的 ``toEngineering``
互逆但**不共用代码**：换算只在采集侧做一次（接口规范 §3.5），模拟器这里只是
把工程值还原成设备侧的原始寄存器值。

本模块不 import pymodbus，也不碰串口：纯数据变换，单测不装依赖也能跑。
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

from point_table import Bus, Point

# 场景名取枚举里的固定值：接口规范 §12.1 只定义这三个，多一个少一个都会让
# N-04 的按钮与 sim 的校验对不上。
SCENE_NORMAL = "normal"
SCENE_ALARM = "alarm"
SCENE_SAFETY = "safety"
KNOWN_SCENES = (SCENE_NORMAL, SCENE_ALARM, SCENE_SAFETY)


@dataclass(frozen=True)
class Scene:
    name: str
    description: str
    clear_faults: bool
    points: dict[str, float]


@dataclass(frozen=True)
class SceneSet:
    config_version: str
    scenes: dict[str, Scene]

    def get(self, name: str) -> Scene:
        try:
            return self.scenes[name]
        except KeyError:
            known = "、".join(sorted(self.scenes)) or "（无）"
            raise ValueError(f"未知工况 {name!r}，只允许 {known}") from None


def load(path: Path) -> SceneSet:
    """读 scenes.json。结构不对就抛错，不静默取默认值——场景错了整场演示都是错的。"""
    raw = json.loads(path.read_text(encoding="utf-8"))
    scenes_raw = raw.get("scenes")
    if not isinstance(scenes_raw, dict) or not scenes_raw:
        raise ValueError(f"{path}: 缺少非空的 scenes")

    scenes: dict[str, Scene] = {}
    for name, body in scenes_raw.items():
        if not isinstance(body, dict):
            raise ValueError(f"{path}: 工况 {name} 不是对象")
        points = body.get("points")
        if not isinstance(points, dict) or not points:
            raise ValueError(f"{path}: 工况 {name} 缺少非空的 points")
        converted: dict[str, float] = {}
        for code, value in points.items():
            if not isinstance(value, (int, float)) or isinstance(value, bool):
                raise ValueError(f"{path}: 工况 {name} 的测点 {code} 值不是数字：{value!r}")
            converted[code] = float(value)
        scenes[name] = Scene(
            name=name,
            description=str(body.get("description", "")),
            clear_faults=bool(body.get("clearFaults", False)),
            points=converted,
        )

    config_version = raw.get("configVersion")
    if not isinstance(config_version, str) or not config_version:
        raise ValueError(f"{path}: 缺少 configVersion")
    return SceneSet(config_version=config_version, scenes=scenes)


def engineering_to_raw(point: Point, value: float) -> int:
    """工程值 → 单个原始编码（宽测点的两个字由调用方按 AB 拆）。

    与 ``edge-agent`` 的 ``toEngineering`` 是逆运算：``eng = raw*scale + offset``。
    超出量程直接报错：场景里写了一个点表装不下的值，是配置缺陷，
    静默截断会得到「看起来正常但没有越限」的现场，最难排查。
    """
    if value < point.min_value or value > point.max_value:
        raise ValueError(
            f"测点 {point.code} 的值 {value} 超出量程 "
            f"{point.min_value}~{point.max_value} {point.unit or ''}".rstrip()
        )
    raw = round((value - point.offset) / point.scale)
    if point.data_type.startswith("int"):
        bits = 16 if point.data_type == "int16" else 32
        low, high = -(2 ** (bits - 1)), 2 ** (bits - 1) - 1
    else:
        bits = 16 if point.data_type == "uint16" else 32
        low, high = 0, 2**bits - 1
    if not low <= raw <= high:
        raise ValueError(
            f"测点 {point.code} 的 {value} 换算后 {raw} 超出 {point.data_type} 范围"
        )
    return raw


def plan_bus(bus: Bus, scene: Scene) -> list[tuple[int, int, list[int]]]:
    """把一条总线的场景算出「写哪几个寄存器」。

    返回 ``[(slave_addr, start_addr, words)]``。只返回场景里显式声明的测点：
    没声明的点保持当前值——``normal`` 想彻底复位就把它想要的点全列出来，
    这样「场景 A 切到场景 B，A 独有的点该不该复位」是配置里写明的事，
    而不是靠代码猜。
    """
    writes: list[tuple[int, int, list[int]]] = []
    for device in bus.devices:
        for point in device.points:
            if point.code not in scene.points:
                continue
            raw = engineering_to_raw(point, scene.points[point.code])
            writes.append(
                (
                    device.slave_addr,
                    device.start_addr + point.addr,
                    split_words(raw, point.width),
                )
            )
    return writes


def split_words(raw: int, width: int) -> list[int]:
    """按 AB CD（高字在前）拆成寄存器字，与 004 的核对脚本同一口径。"""
    if width == 1:
        return [raw & 0xFFFF]
    return [(raw >> 16) & 0xFFFF, raw & 0xFFFF]


def unknown_codes(scene: Scene, bus: Bus) -> list[str]:
    """场景里声明了、但这条总线上不存在的测点编码。

    5 台终端的点表同形，正常情况下不会有；出现就说明点表与场景配置漂移了，
    要报出来而不是悄悄跳过。
    """
    known = {p.code for device in bus.devices for p in device.points}
    return sorted(code for code in scene.points if code not in known)