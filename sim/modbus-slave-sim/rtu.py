"""Modbus RTU 帧编解码（主站与从站共用）。

从站侧用它组异常帧和故意坏掉的 CRC 帧；核对工具用它作为主站收发与判定。
两边共用一份实现，避免"模拟器算的 CRC 和主站验的 CRC 不是同一套算法"——
那种错误会让三次重试都失败，看起来像线路问题。

本模块不 import pymodbus：从站主体仍由 pymodbus 承担，这里只补它不提供的
两个动作（协议层异常帧、CRC 破坏）以及主站侧的判定。
"""

from __future__ import annotations

# 异常帧：从站地址 1 + 功能码 1 + 异常码 1 + CRC 2。
EXCEPTION_FRAME_LEN = 5
# 正常读响应最短：地址 + 功能码 + 字节数 + CRC 2。
MIN_RESPONSE_LEN = 5


def crc16(data: bytes) -> int:
    """标准 Modbus CRC16（多项式 0xA001，初值 0xFFFF）。"""
    crc = 0xFFFF
    for byte in data:
        crc ^= byte
        for _ in range(8):
            crc = (crc >> 1) ^ 0xA001 if crc & 1 else crc >> 1
    return crc


def with_crc(frame: bytes) -> bytes:
    """追加 CRC，低字节在前（Modbus RTU 线上顺序）。"""
    crc = crc16(frame)
    return frame + bytes((crc & 0xFF, crc >> 8))


def build_register_request(slave: int, function_code: int, address: int, count: int) -> bytes:
    body = bytes((slave, function_code)) + address.to_bytes(2, "big") + count.to_bytes(2, "big")
    return with_crc(body)


def build_read_response(slave: int, function_code: int, registers: list[int]) -> bytes:
    body = bytes((slave, function_code, len(registers) * 2))
    for value in registers:
        body += (value & 0xFFFF).to_bytes(2, "big")
    return with_crc(body)


def build_exception_response(slave: int, function_code: int, exception_code: int) -> bytes:
    """功能码高位置 1：03 → 0x83，04 → 0x84。"""
    return with_crc(bytes((slave, function_code | 0x80, exception_code)))


def corrupt_crc(frame: bytes) -> bytes:
    """把 CRC 两个字节都取反，保持帧长不变。"""
    return frame[:-2] + bytes((frame[-2] ^ 0xFF, frame[-1] ^ 0xFF))


def crc_ok(frame: bytes) -> bool:
    if len(frame) < 4:
        return False
    expected = crc16(frame[:-2])
    return frame[-2] == (expected & 0xFF) and frame[-1] == (expected >> 8)


def parse_exception_code(frame: bytes) -> int:
    """异常帧的异常码在第 3 字节。"""
    return frame[2] if len(frame) >= 3 else 0