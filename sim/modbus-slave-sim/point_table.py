"""点表载入。

点表的权威定义在 config/terminals/*.json（002 交付）。模拟器只读它，
不复制一份寄存器清单——两份清单必然漂移，漂移的方向恰好是
「点表改了、模拟器还在旧地址上应答」，而 004 的全部验收都建立在
「读回值来自点表声明的地址」上。

sim/ 不 import internal/：这里按接口规范 §3.7 的 JSON 形状自行解析。
"""

from __future__ import annotations

import json
import re
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import cast

# 一个从站里寄存器块的编号：模拟器只按功能码 3 应答（接口规范 §3.2）。
FUNCTION_CODE_HOLDING = 3
# 单次读取上限 125 个寄存器（接口规范 §3.2），模拟器按同样的上限校验块。
MAX_BLOCK_REGISTERS = 125


@dataclass(frozen=True)
class Point:
    code: str
    name: str
    addr: int  # 块内偏移
    width: int
    data_type: str
    byte_order: str
    scale: float
    offset: float
    unit: str | None
    min_value: float
    max_value: float
    si_type: str


@dataclass(frozen=True)
class Device:
    slave_addr: int
    name: str
    type: str
    start_addr: int
    register_count: int
    points: tuple[Point, ...]

    @property
    def end_addr(self) -> int:
        """块的结束地址（含）。"""
        return self.start_addr + self.register_count - 1


@dataclass(frozen=True)
class Bus:
    terminal_id: str
    bus_id: str
    baud_rate: int
    data_bits: int
    stop_bits: int
    parity: str
    devices: tuple[Device, ...]

    @property
    def key(self) -> str:
        """总线在模拟器内的唯一键。

        busId 只有 A / B（接口规范 §2.2），跨终端会重名，所以键必须带终端。
        """
        return f"{self.terminal_id}.{self.bus_id}"


@dataclass(frozen=True)
class PointTable:
    terminal_id: str
    config_version: str
    buses: tuple[Bus, ...]

    @property
    def point_count(self) -> int:
        return sum(len(d.points) for b in self.buses for d in b.devices)


_TERMINAL_FILE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*\.json$")


def load_terminal(path: Path) -> PointTable:
    """载入一份终端点表。形状错误直接抛错，不做容错解析。"""
    with path.open(encoding="utf-8") as fh:
        loaded = cast(object, json.load(fh))
    if not isinstance(loaded, dict):
        raise TypeError(f"{path}: 顶层必须是对象")
    raw = cast(dict[str, object], loaded)
    where = str(path)
    terminal_id = _str(raw, "terminalId", where)
    config_version = _str(raw, "configVersion", where)
    buses = tuple(
        _bus(item, terminal_id, where, i)
        for i, item in enumerate(_obj_list(raw, "buses", where))
    )
    if not buses:
        raise ValueError(f"{path}: buses 为空")
    return PointTable(terminal_id, config_version, buses)


def load_tree(directory: Path) -> list[PointTable]:
    """载入目录下全部终端点表，按终端标识排序，保证总线顺序稳定。"""
    if not directory.is_dir():
        raise ValueError(f"{directory}: 不是目录")
    tables = [
        load_terminal(p)
        for p in sorted(directory.iterdir())
        if p.is_file() and _TERMINAL_FILE.match(p.name)
    ]
    if not tables:
        raise ValueError(f"{directory}: 没有终端点表")
    return sorted(tables, key=lambda t: t.terminal_id)


def _bus(raw: Mapping[str, object], terminal_id: str, path: str, index: int) -> Bus:
    where = f"{path}: buses[{index}]"
    bus_id = _str(raw, "busId", where)
    serial_keys = ("baudRate", "dataBits", "stopBits", "parity")
    serial = {k: raw.get(k) for k in serial_keys}
    missing = [k for k, v in serial.items() if v is None]
    if missing:
        raise ValueError(f"{where}: 缺少串口参数 {', '.join(missing)}")
    devices = tuple(_device(item, where) for item in _obj_list(raw, "devices", where))
    if not devices:
        raise ValueError(f"{where}: devices 为空")
    addrs = [d.slave_addr for d in devices]
    if len(addrs) != len(set(addrs)):
        raise ValueError(f"{where}: 从站地址重复 {addrs}")
    return Bus(
        terminal_id=terminal_id,
        bus_id=bus_id,
        baud_rate=_int(raw, "baudRate", where),
        data_bits=_int(raw, "dataBits", where),
        stop_bits=_int(raw, "stopBits", where),
        parity=_str(raw, "parity", where),
        devices=tuple(sorted(devices, key=lambda d: d.slave_addr)),
    )


def _device(raw: Mapping[str, object], where: str) -> Device:
    blocks = _obj_list(raw, "blocks", where)
    if len(blocks) != 1:
        raise ValueError(f"{where}: 本任务只支持每个从站一个块，实际 {len(blocks)} 个")
    block = blocks[0]
    function_code = _int(block, "functionCode", where)
    if function_code != FUNCTION_CODE_HOLDING:
        raise ValueError(f"{where}: 功能码只能是 03，实际 {function_code}")
    start = _int(block, "startAddr", where)
    count = _int(block, "registerCount", where)
    if not 1 <= count <= MAX_BLOCK_REGISTERS:
        raise ValueError(f"{where}: registerCount 越界 {count}")
    points = tuple(_point(p, where) for p in _obj_list(block, "points", where))
    for p in points:
        if p.addr < 0 or p.addr + p.width > count:
            raise ValueError(f"{where}: 测点 {p.code} 的地址 {p.addr}+{p.width} 超出块 {count}")
    return Device(
        slave_addr=_int(raw, "slaveAddr", where),
        name=_str(raw, "name", where),
        type=_str(raw, "type", where),
        start_addr=start,
        register_count=count,
        points=points,
    )


def _point(raw: Mapping[str, object], where: str) -> Point:
    data_type = _str(raw, "dataType", where)
    width = _int(raw, "width", where)
    expected = {"int16": 1, "uint16": 1, "uint32": 2, "int32": 2}.get(data_type)
    if expected is None:
        raise ValueError(f"{where}: 不支持的数据类型 {data_type}")
    if width != expected:
        raise ValueError(f"{where}: {data_type} 的宽度应为 {expected}，实际 {width}")
    unit = raw.get("unit")
    return Point(
        code=_str(raw, "code", where),
        name=_str(raw, "name", where),
        addr=_int(raw, "addr", where),
        width=width,
        data_type=data_type,
        byte_order=_str(raw, "byteOrder", where),
        scale=_float(raw, "scale", where),
        offset=_float(raw, "offset", where, default=0.0),
        unit=None if unit is None else str(unit),
        min_value=_float(raw, "minValue", where),
        max_value=_float(raw, "maxValue", where),
        si_type=_str(raw, "siType", where),
    )


def _req(raw: Mapping[str, object], key: str, where: str) -> object:
    if key not in raw or raw[key] is None:
        raise ValueError(f"{where}: 缺少字段 {key}")
    return raw[key]


def _int(raw: Mapping[str, object], key: str, where: str) -> int:
    value = _req(raw, key, where)
    if isinstance(value, bool) or not isinstance(value, int | float | str):
        raise TypeError(f"{where}: {key} 必须是整数，实际 {value!r}")
    return int(value)


def _float(
    raw: Mapping[str, object], key: str, where: str, default: float | None = None
) -> float:
    value = _req(raw, key, where) if default is None else raw.get(key, default)
    if isinstance(value, bool) or not isinstance(value, int | float | str):
        raise TypeError(f"{where}: {key} 必须是数值，实际 {value!r}")
    return float(value)


def _str(raw: Mapping[str, object], key: str, where: str) -> str:
    value = _req(raw, key, where)
    if not isinstance(value, str):
        raise TypeError(f"{where}: {key} 必须是字符串，实际 {value!r}")
    if not value:
        raise ValueError(f"{where}: {key} 必须是非空字符串")
    return value


def _list(raw: Mapping[str, object], key: str, where: str) -> list[object]:
    value = _req(raw, key, where)
    if not isinstance(value, list):
        raise TypeError(f"{where}: {key} 必须是数组，实际 {value!r}")
    if not value:
        raise ValueError(f"{where}: {key} 必须是非空数组")
    return cast(list[object], value)


def _obj_list(
    raw: Mapping[str, object], key: str, where: str
) -> list[Mapping[str, object]]:
    """对象数组：每一项都必须是 JSON 对象，否则报错而不是继续解析。"""
    out: list[Mapping[str, object]] = []
    for index, item in enumerate(_list(raw, key, where)):
        if not isinstance(item, dict):
            raise TypeError(f"{where}: {key}[{index}] 必须是对象")
        out.append(cast(dict[str, object], item))
    return out
