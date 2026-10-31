"""faults 与 rtu 的单元测试。

只测纯逻辑：故障注入状态机与帧编解码。不需要串口、不需要模拟器进程。
运行：make sim-test
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import faults  # noqa: E402
import rtu  # noqa: E402

# 期望值取自 Modbus 应用协议规范 v1.1b3 的公开样例，不由实现推算：
#   §6.12 例 2 读请求：从站 0x11、FC03、地址 0x006B、数量 3，线上 CRC 字节 = 76 87
#   §6.8  读响应：从站 0x11、FC03、3 个寄存器 AE41 5652 4340，线上 CRC 字节 = 49 AD
# 线上顺序是低字节在前，所以 CRC 数值分别是 0x8776 与 0xAD49。
REFERENCE_REQUEST = bytes.fromhex("1103006B00037687")
REFERENCE_RESPONSE = bytes.fromhex("110306AE415652434049ad")


class TestCrc:
    def test_crc_matches_published_example(self):
        assert rtu.crc16(REFERENCE_REQUEST[:-2]) == 0x8776
        assert rtu.crc16(REFERENCE_RESPONSE[:-2]) == 0xAD49

    def test_with_crc_appends_low_byte_first(self):
        assert rtu.with_crc(REFERENCE_REQUEST[:-2]) == REFERENCE_REQUEST

    @pytest.mark.parametrize("frame", [REFERENCE_REQUEST, REFERENCE_RESPONSE])
    def test_crc_ok_accepts_valid_frames(self, frame):
        assert rtu.crc_ok(frame)

    def test_crc_ok_rejects_corrupted_frame(self):
        assert not rtu.crc_ok(rtu.corrupt_crc(REFERENCE_RESPONSE))

    def test_corrupt_crc_keeps_length_and_protected_payload(self):
        broken = rtu.corrupt_crc(REFERENCE_RESPONSE)
        assert len(broken) == len(REFERENCE_RESPONSE)
        assert broken[:-2] == REFERENCE_RESPONSE[:-2]
        assert broken[-2:] != REFERENCE_RESPONSE[-2:]


class TestFrames:
    def test_build_register_request(self):
        assert rtu.build_register_request(0x11, 3, 0x006B, 3) == REFERENCE_REQUEST

    def test_build_read_response(self):
        # 规范样例里的 6 个字节按 AB 拆成 3 个寄存器：AE41 / 5652 / 4340
        assert rtu.build_read_response(0x11, 3, [0xAE41, 0x5652, 0x4340]) == REFERENCE_RESPONSE

    @pytest.mark.parametrize(
        ("function_code", "expected"),
        [(3, 0x83), (4, 0x84)],
    )
    def test_exception_sets_high_bit(self, function_code, expected):
        frame = rtu.build_exception_response(1, function_code, 2)
        assert frame[1] == expected
        assert frame[2] == 2
        assert rtu.crc_ok(frame)

    def test_exception_of_fc04_is_0x84(self):
        frame = rtu.build_exception_response(7, 4, 4)
        assert frame.hex().startswith("0784")


class TestFaultPlan:
    def test_timeout_drops_exactly_count_frames(self):
        plan = faults.parse_plan("timeout", 2, None)
        assert [plan.take().kind for _ in range(2)] == [faults.ActionKind.DROP] * 2
        assert plan.take() is None

    @pytest.mark.parametrize("mode", ["exception", "crc"])
    def test_count_expires(self, mode):
        plan = faults.parse_plan(mode, 1, 4)
        assert plan.take() is not None
        assert plan.take() is None

    def test_exception_defaults_to_slave_failure(self):
        plan = faults.parse_plan("exception", 1, None)
        assert plan.take().exception_code == faults.DEFAULT_EXCEPTION_CODE

    def test_parse_plan_rejects_unknown_mode(self):
        with pytest.raises(ValueError, match="未知故障模式"):
            faults.parse_plan("nack", 1, None)

    @pytest.mark.parametrize("count", [0, -1, faults.MAX_FAULT_COUNT + 1])
    def test_parse_plan_rejects_bad_count(self, count):
        with pytest.raises(ValueError, match="count"):
            faults.parse_plan("timeout", count, None)

    @pytest.mark.parametrize("code", [0, 5, 255])
    def test_parse_plan_rejects_bad_exception_code(self, code):
        with pytest.raises(ValueError, match="异常码"):
            faults.parse_plan("exception", 1, code)


class TestBusState:
    def test_offline_slave_never_answers_even_with_fault_queued(self):
        state = faults.BusState((1, 2))
        state.inject(1, faults.parse_plan("exception", 3, None))
        state.go_offline(1)
        assert state.next_action(1).kind is faults.ActionKind.DROP
        state.go_online(1)
        assert state.next_action(1).kind is faults.ActionKind.EXCEPTION

    def test_offline_is_per_slave(self):
        state = faults.BusState((1, 2))
        state.go_offline(2)
        assert state.next_action(1).kind is faults.ActionKind.NORMAL
        assert state.next_action(2).kind is faults.ActionKind.DROP

    def test_fault_is_consumed_in_order(self):
        state = faults.BusState((1,))
        state.inject(1, faults.parse_plan("crc", 1, None))
        assert state.next_action(1).kind is faults.ActionKind.CORRUPT_CRC
        assert state.next_action(1).kind is faults.ActionKind.NORMAL

    def test_clear_faults_keeps_offline(self):
        state = faults.BusState((1,))
        state.inject(1, faults.parse_plan("timeout", 5, None))
        state.go_offline(1)
        state.clear_faults()
        assert state.is_offline(1)
        assert state.snapshot()["faults"] == {}

    def test_unknown_slave_is_rejected(self):
        state = faults.BusState((1,))
        with pytest.raises(KeyError):
            state.go_offline(9)

    def test_snapshot_lists_remaining_count(self):
        state = faults.BusState((1,))
        state.inject(1, faults.parse_plan("exception", 3, 2))
        state.next_action(1)
        snapshot = state.snapshot()
        assert snapshot["faults"]["1"] == {"mode": "exception", "remaining": 2, "exceptionCode": 2}