"""寄存器存储：模拟器里"设备侧"的原始寄存器。

存的是**原始值**，不做任何缩放（接口规范 §3.5：换算只在 edge-agent 做一次）。
控制接口写进来的值也是原始值，625 表示 62.5 ℃。

初值取各测点量程的中点换算回原始值，让默认工况看起来正常；
004 的点表核对则用控制接口写入的哨兵值，与初值无关。
"""

from __future__ import annotations

from pymodbus.datastore import (
    ModbusSequentialDataBlock,
    ModbusServerContext,
    ModbusSlaveContext,
)

from point_table import Bus, Device, Point

# 功能码 03 / 04 在 datastore 里都落在保持寄存器的同一个块上。
# 点表只用 03；04 用于让 0x84 这类异常码有真实请求可回。
DATABLOCK_FUNCTION_CODES = (3, 4)
TYPE_BITS = {"int16": 16, "uint16": 16, "uint32": 32, "int32": 32}


class RegisterStore:
    """一条总线的寄存器。按从站地址分区，地址空间互相独立。"""

    def __init__(self, bus: Bus) -> None:
        self._bus = bus
        self._slaves = {d.slave_addr: d for d in bus.devices}
        self._contexts = {
            d.slave_addr: ModbusSlaveContext(
                # zero_mode=True：点表里的 0x0000 就是协议地址 0，不额外 +1。
                # 模拟器若默认加 1，读回值会整体错位一格，而错位在数值上看起来"合理"，
                # 是最难发现的一类映射错误。
                zero_mode=True,
                hr=ModbusSequentialDataBlock(d.start_addr, _baseline(d)),
            )
            for d in bus.devices
        }
        self._context = ModbusServerContext(slaves=self._contexts, single=False)

    @property
    def context(self) -> ModbusServerContext:
        return self._context

    def device(self, slave_addr: int) -> Device:
        try:
            return self._slaves[slave_addr]
        except KeyError:
            raise KeyError(f"总线 {self._bus.bus_id} 没有从站 {slave_addr}") from None

    def read(self, slave_addr: int, addr: int, count: int) -> list[int]:
        device = self.device(slave_addr)
        if addr < device.start_addr or addr + count > device.start_addr + device.register_count:
            raise ValueError(
                f"从站 {slave_addr} 的寄存器 {addr}~{addr + count - 1} 超出块 "
                f"{device.start_addr}~{device.end_addr}"
            )
        return [int(v) for v in self._contexts[slave_addr].getValues(3, addr, count)]

    def write(self, slave_addr: int, addr: int, value: int) -> None:
        self.write_many(slave_addr, addr, [value])

    def write_many(self, slave_addr: int, addr: int, values: list[int]) -> None:
        device = self.device(slave_addr)
        for offset, value in enumerate(values):
            _check_register_value(addr + offset, value)
        if addr < device.start_addr or addr + len(values) > device.start_addr + device.register_count:
            raise ValueError(
                f"从站 {slave_addr} 的寄存器 {addr}~{addr + len(values) - 1} 超出块 "
                f"{device.start_addr}~{device.end_addr}"
            )
        self._contexts[slave_addr].setValues(3, addr, values)

    def snapshot(self, slave_addr: int) -> list[dict]:
        device = self.device(slave_addr)
        values = self.read(slave_addr, device.start_addr, device.register_count)
        return [
            {"addr": device.start_addr + i, "value": v} for i, v in enumerate(values)
        ]

    def point_raw(self, slave_addr: int, point: Point) -> list[int]:
        device = self.device(slave_addr)
        return self.read(slave_addr, device.start_addr + point.addr, point.width)


def _check_register_value(addr: int, value: int) -> None:
    if not 0 <= value <= 0xFFFF:
        raise ValueError(f"寄存器 {addr} 的值 {value} 超出 uint16 范围 0~65535")


def _baseline(device: Device) -> list[int]:
    """按测点量程中点生成初值。未被任何测点覆盖的寄存器填 0。"""
    values = [0] * device.register_count
    for point in device.points:
        raw = _midpoint_raw(point)
        words = _split_words(raw, point.width)
        for i, word in enumerate(words):
            values[point.addr + i] = word
    return values


def _midpoint_raw(point: Point) -> int:
    mid = (point.min_value + point.max_value) / 2
    raw = round((mid - point.offset) / point.scale)
    bits = TYPE_BITS[point.data_type]
    low, high = (-(2 ** (bits - 1)), 2 ** (bits - 1) - 1) if point.data_type.startswith("int") else (0, 2**bits - 1)
    return max(low, min(high, int(raw)))


def _split_words(raw: int, width: int) -> list[int]:
    """按 AB CD（高字在前）拆成寄存器字。"""
    if width == 1:
        return [raw & 0xFFFF]
    return [(raw >> 16) & 0xFFFF, raw & 0xFFFF]