"""协议层故障注入。

三种异常都是**协议层真实异常**，不是错误数值（.claude/rules/sim.md）：

- ``timeout``   请求收到、整帧不应答，主站等到超时
- ``exception`` 回一条异常响应帧，功能码字节为 ``请求功能码 | 0x80``（0x83 / 0x84），
                异常码在 payload 里（01~04）
- ``crc``       正常组帧后把 CRC 两字节改坏，主站校验失败

返回错误数值验证的是应用层容错，不是通信容错，所以这里只有这三种。

本模块不 import pymodbus：它是一个纯状态机，单测不装依赖也能跑。
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum

# 异常码取 Modbus 标准码：01 非法功能、02 非法地址、03 非法数据、04 从站故障。
# 默认 04，接口规范 §3.6 要求把细分码记进 detail 供 SIM-17 取证。
DEFAULT_EXCEPTION_CODE = 4
MIN_EXCEPTION_CODE = 1
MAX_EXCEPTION_CODE = 4
# 单次注入的响应条数上限。注入是临时动作，count 不设上限等于让从站永久失联，
# 调试完忘了恢复就会在验收现场变成"设备坏了"。
MAX_FAULT_COUNT = 1000


class FaultMode(str, Enum):
    TIMEOUT = "timeout"
    EXCEPTION = "exception"
    CRC = "crc"


class ActionKind(str, Enum):
    NORMAL = "normal"
    DROP = "drop"
    EXCEPTION = "exception"
    CORRUPT_CRC = "corrupt_crc"


@dataclass(frozen=True)
class Action:
    kind: ActionKind
    exception_code: int | None = None


NORMAL = Action(ActionKind.NORMAL)
DROP = Action(ActionKind.DROP)


@dataclass
class FaultPlan:
    """一条注入计划：还有 ``remaining`` 个请求按 ``mode`` 处理。"""

    mode: FaultMode
    remaining: int
    exception_code: int = DEFAULT_EXCEPTION_CODE

    def take(self) -> Action | None:
        """消耗一次注入。计数归零时返回 None 表示本次按正常处理。"""
        if self.remaining <= 0:
            return None
        self.remaining -= 1
        if self.mode is FaultMode.TIMEOUT:
            return DROP
        if self.mode is FaultMode.EXCEPTION:
            return Action(ActionKind.EXCEPTION, self.exception_code)
        return Action(ActionKind.CORRUPT_CRC)


class BusState:
    """一条总线上全部从站的模拟状态。

    只在 asyncio 事件循环线程里读写：RTU 是半双工主从协议，同一时刻只有一个请求
    在途，因此不需要锁。真跨线程使用必须先加锁，否则注计数会丢。
    """

    def __init__(self, slave_addrs: tuple[int, ...]) -> None:
        if not slave_addrs:
            raise ValueError("总线至少要有一个从站")
        self._offline: set[int] = set()
        self._plans: dict[int, FaultPlan] = {}
        self._slave_addrs = frozenset(slave_addrs)

    def known(self, slave_addr: int) -> bool:
        return slave_addr in self._slave_addrs

    def go_offline(self, slave_addr: int) -> None:
        self._require(slave_addr)
        self._offline.add(slave_addr)

    def go_online(self, slave_addr: int) -> None:
        self._require(slave_addr)
        self._offline.discard(slave_addr)

    def is_offline(self, slave_addr: int) -> bool:
        return slave_addr in self._offline

    def inject(self, slave_addr: int, plan: FaultPlan) -> None:
        self._require(slave_addr)
        self._plans[slave_addr] = plan

    def clear_faults(self) -> None:
        self._plans.clear()

    def next_action(self, slave_addr: int) -> Action:
        """本次请求怎么处理。离线优先于注入：离线时从站根本不存在。"""
        if slave_addr in self._offline:
            return DROP
        plan = self._plans.get(slave_addr)
        if plan is None:
            return NORMAL
        action = plan.take()
        if plan.remaining <= 0:
            del self._plans[slave_addr]
        return NORMAL if action is None else action

    def snapshot(self) -> dict:
        return {
            "offline": sorted(self._offline),
            "faults": {
                str(addr): {
                    "mode": plan.mode.value,
                    "remaining": plan.remaining,
                    "exceptionCode": plan.exception_code,
                }
                for addr, plan in sorted(self._plans.items())
            },
        }

    def _require(self, slave_addr: int) -> None:
        if slave_addr not in self._slave_addrs:
            raise KeyError(f"总线上没有从站 {slave_addr}")


def parse_plan(mode: str, count: int, exception_code: int | None) -> FaultPlan:
    """校验控制接口入参。非法值直接报错，不静默纠正。"""
    try:
        parsed = FaultMode(mode)
    except ValueError:
        allowed = "、".join(m.value for m in FaultMode)
        raise ValueError(f"未知故障模式 {mode!r}，只允许 {allowed}") from None
    if not 1 <= count <= MAX_FAULT_COUNT:
        raise ValueError(f"count 必须在 1~{MAX_FAULT_COUNT}，实际 {count}")
    code = DEFAULT_EXCEPTION_CODE if exception_code is None else exception_code
    if not MIN_EXCEPTION_CODE <= code <= MAX_EXCEPTION_CODE:
        raise ValueError(
            f"异常码必须在 {MIN_EXCEPTION_CODE}~{MAX_EXCEPTION_CODE}，实际 {code}"
        )
    return FaultPlan(parsed, count, code)