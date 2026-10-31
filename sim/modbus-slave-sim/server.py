"""一条总线上的 RTU 从站服务。

每条总线一个独立实例、一个独立 pty 端点、一个独立事件循环。总线之间不共享
任何状态——这是 004 验收第一条「10 条总线各自独立，从站地址 1~5 不串号」的落点。

应答链路的三个分支：

1. 正常：交给 pymodbus 按 datastore 组帧回
2. 异常码 / CRC 错：用自己的 RTU 帧覆盖 pymodbus 的应答，直接写串口
3. 超时 / 离线：不应答

pymodbus 的 ``response_manipulator`` 只能操作 PDU、不能把 CRC 改坏，
所以异常与 CRC 两类走 ``skip_encoding`` 通道自己组帧。
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import time

from pymodbus.datastore import ModbusServerContext
from pymodbus.framer import FramerType
from pymodbus.server import ModbusSerialServer
from pymodbus.server.async_io import ModbusServerRequestHandler

import rtu
from faults import Action, ActionKind, BusState
from point_table import Bus
from registers import RegisterStore

log = logging.getLogger("modbus-slave-sim")


class _SafeSerialHandler(ModbusServerRequestHandler):
    """丢弃「空应答」而不是把 None 写进串口。

    pymodbus 3.7.4 的 ``server_send`` 在 ``skip_encoding=True`` 且 message 为 None 时
    仍然调用 ``self.send(None)``，随后 SerialTransport 的写回调
    ``b"".join([...None...])`` 抛 TypeError，该总线的写通道从此卡死——
    表现是"注入过一次超时之后，这条总线再也不应答"。超时注入正是靠返回 None
    实现的，因此必须在这里截住：无应答 = 什么都不写。
    """

    def server_send(self, message, addr, **kwargs) -> None:
        if kwargs.get("skip_encoding", False):
            if message:
                self.send(message, addr=addr)
            return
        super().server_send(message, addr, **kwargs)


class BusServer:
    """一条总线的从站模拟。"""

    def __init__(self, bus: Bus, port: str) -> None:
        self.bus = bus
        self.port = port
        self.store = RegisterStore(bus)
        self.state = BusState(tuple(d.slave_addr for d in bus.devices))
        self._server: ModbusSerialServer | None = None
        self._task: asyncio.Task | None = None
        self._started_at = time.time()

    @property
    def context(self) -> ModbusServerContext:
        return self.store.context

    async def start(self) -> None:
        self._server = ModbusSerialServer(
            self.store.context,
            framer=FramerType.RTU,
            port=self.port,
            baudrate=self.bus.baud_rate,
            bytesize=self.bus.data_bits,
            parity=self.bus.parity,
            stopbits=self.bus.stop_bits,
            timeout=1,
            ignore_missing_slaves=True,
            response_manipulator=self._manipulate,
            request_tracer=self._trace,
        )
        self._server.callback_new_connection = lambda: _SafeSerialHandler(self._server)
        self._task = asyncio.create_task(self._serve(), name=f"bus-{self.bus.key}")
        await asyncio.sleep(0)

    async def stop(self) -> None:
        if self._server is not None:
            with contextlib.suppress(Exception):
                await self._server.shutdown()
        if self._task is not None:
            self._task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._task
        self._task = None
        self._server = None

    async def _serve(self) -> None:
        assert self._server is not None
        try:
            await self._server.serve_forever()
        except asyncio.CancelledError:
            raise
        except Exception:
            log.exception("总线 %s 服务异常退出", self.bus.key)
            raise

    def _trace(self, request, *_addr) -> None:
        """记录请求，供控制接口回读"上一帧请求"（核对工具不依赖它）。"""
        log.debug(
            "bus=%s slave=%s fc=%s addr=%s count=%s",
            self.bus.key,
            request.slave_id,
            request.function_code,
            getattr(request, "address", None),
            getattr(request, "count", None),
        )

    def _manipulate(self, response):
        """按注入计划改写本次应答。

        返回 ``(response, skip_encoding)``：跳过 pymodbus 组帧时自己写串口。
        """
        slave = response.slave_id
        action = self.state.next_action(slave)
        if action.kind is ActionKind.NORMAL:
            return response, False
        if action.kind is ActionKind.DROP:
            log.info("bus=%s slave=%s 注入超时：不应答", self.bus.key, slave)
            return None, True
        function_code = response.function_code & 0x7F
        frame = self._build_fault_frame(action, slave, function_code)
        log.info(
            "bus=%s slave=%s 注入 %s：%s",
            self.bus.key,
            slave,
            action.kind.value,
            frame.hex(),
        )
        return frame, True

    def _build_fault_frame(self, action: Action, slave: int, function_code: int) -> bytes:
        if action.kind is ActionKind.EXCEPTION:
            assert action.exception_code is not None
            return rtu.build_exception_response(slave, function_code, action.exception_code)
        return rtu.corrupt_crc(
            rtu.build_read_response(slave, function_code, self._registers_for(slave))
        )

    def _registers_for(self, slave: int) -> list[int]:
        """CRC 错那一类要有一条内容正确的帧，只把校验改坏。

        读块取从站声明的整块，与点表块划分一致。
        """
        device = self.store.device(slave)
        return self.store.read(slave, device.start_addr, device.register_count)

    def status(self) -> dict:
        return {
            "terminalId": self.bus.terminal_id,
            "busId": self.bus.bus_id,
            "port": self.port,
            "baudRate": self.bus.baud_rate,
            "slaves": [
                {"slaveAddr": d.slave_addr, "name": d.name, "type": d.type}
                for d in self.bus.devices
            ],
            "uptimeSec": round(time.time() - self._started_at, 3),
            **self.state.snapshot(),
        }


__all__ = ["BusServer"]