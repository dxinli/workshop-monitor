"""控制接口（接口规范 §12.1，按 004 偏差扩到终端维度）。

监听 ``127.0.0.1:8080``，不对外暴露（.claude/rules/sim.md）。

路径里带 ``terminalId`` 的原因：一条物理总线的唯一标识是
``terminalId + busId``，而 ``busId`` 只有 A / B（接口规范 §2.2）。只写
``busId`` 时 10 条总线会塌缩成 2 条，改 A 的值会同时改掉 5 台终端的 A 总线——
这正是「从站地址不串号」这条验收要防的事。

写寄存器接受两种形式：
- ``{"value": 625}``：单寄存器原始值，625 表示 62.5 ℃（接口规范 §12.1）
- ``{"points": [{"code": "motor_temp", "value": 625}]}``：一次写整个测点，
  多寄存器测点按 AB CD（高字在前）自动拆字；这是 140 点核对脚本要用的形式，
  避免核对工具自己再实现一遍拆字规则
"""

from __future__ import annotations

from typing import Annotated

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

from faults import parse_plan
from server import BusServer

MAX_REGISTER_VALUE = 0xFFFF


class RegisterWrite(BaseModel):
    """写单个寄存器。"""

    value: Annotated[int, Field(ge=0, le=MAX_REGISTER_VALUE)]


class PointWrite(BaseModel):
    code: Annotated[str, Field(min_length=1)]
    value: Annotated[int, Field(ge=-(2**31), le=2**32 - 1)]


class PointWriteBatch(BaseModel):
    points: Annotated[list[PointWrite], Field(min_length=1)]


class FaultRequest(BaseModel):
    mode: str
    count: Annotated[int, Field(ge=1, le=1000)] = 1
    exceptionCode: Annotated[int | None, Field(ge=1, le=4)] = None


def build_app(buses: dict[str, BusServer]) -> FastAPI:
    """``buses`` 以 ``terminalId.busId`` 为键。"""
    app = FastAPI(title="modbus-slave-sim", docs_url=None, redoc_url=None)

    def bus_of(terminal_id: str, bus_id: str) -> BusServer:
        bus = buses.get(f"{terminal_id}.{bus_id}")
        if bus is None:
            known = "、".join(sorted(buses)) or "（无）"
            raise HTTPException(
                status_code=404,
                detail=f"未知总线 {terminal_id}/{bus_id}，已知：{known}",
            )
        return bus

    @app.get("/api/v1/health")
    def health() -> dict:
        return {
            "status": "ok",
            "busCount": len(buses),
            "buses": [
                {"terminalId": b.bus.terminal_id, "busId": b.bus.bus_id, "port": b.port}
                for b in sorted(buses.values(), key=lambda b: b.bus.key)
            ],
        }

    @app.get("/api/v1/reg/{terminal_id}/{bus_id}/{slave_addr}")
    def read_slave(terminal_id: str, bus_id: str, slave_addr: int) -> dict:
        bus = bus_of(terminal_id, bus_id)
        try:
            registers = bus.store.snapshot(slave_addr)
        except KeyError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from None
        return {
            "terminalId": terminal_id,
            "busId": bus_id,
            "slaveAddr": slave_addr,
            "registers": registers,
            "state": bus.state.snapshot(),
        }

    @app.put("/api/v1/reg/{terminal_id}/{bus_id}/{slave_addr}/{addr}")
    def write_register(
        terminal_id: str, bus_id: str, slave_addr: int, addr: int, body: RegisterWrite
    ) -> dict:
        bus = bus_of(terminal_id, bus_id)
        try:
            bus.store.write(slave_addr, addr, body.value)
        except (KeyError, ValueError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from None
        return {"ok": True, "addr": addr, "value": body.value}

    @app.put("/api/v1/reg/{terminal_id}/{bus_id}/{slave_addr}")
    def write_points(
        terminal_id: str, bus_id: str, slave_addr: int, body: PointWriteBatch
    ) -> dict:
        bus = bus_of(terminal_id, bus_id)
        written = []
        try:
            device = bus.store.device(slave_addr)
            by_code = {p.code: p for p in device.points}
            for item in body.points:
                point = by_code.get(item.code)
                if point is None:
                    codes = "、".join(sorted(by_code))
                    raise ValueError(f"从站 {slave_addr} 没有测点 {item.code}，可选：{codes}")
                bus.store.write_many(
                    slave_addr, device.start_addr + point.addr, _split_words(item.value, point.width)
                )
                written.append({"code": item.code, "value": item.value})
        except (KeyError, ValueError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from None
        return {"ok": True, "slaveAddr": slave_addr, "points": written}

    @app.post("/api/v1/offline/{terminal_id}/{bus_id}/{slave_addr}")
    def go_offline(terminal_id: str, bus_id: str, slave_addr: int) -> dict:
        bus = bus_of(terminal_id, bus_id)
        try:
            bus.state.go_offline(slave_addr)
        except KeyError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from None
        return {"ok": True, "offline": sorted(bus.state.snapshot()["offline"])}

    @app.post("/api/v1/online/{terminal_id}/{bus_id}/{slave_addr}")
    def go_online(terminal_id: str, bus_id: str, slave_addr: int) -> dict:
        bus = bus_of(terminal_id, bus_id)
        try:
            bus.state.go_online(slave_addr)
        except KeyError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from None
        return {"ok": True, "offline": sorted(bus.state.snapshot()["offline"])}

    @app.post("/api/v1/fault/{terminal_id}/{bus_id}/{slave_addr}")
    def inject_fault(
        terminal_id: str, bus_id: str, slave_addr: int, body: FaultRequest
    ) -> dict:
        bus = bus_of(terminal_id, bus_id)
        try:
            plan = parse_plan(body.mode, body.count, body.exceptionCode)
            bus.state.inject(slave_addr, plan)
        except (KeyError, ValueError) as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from None
        return {"ok": True, "faults": bus.state.snapshot()["faults"]}

    @app.delete("/api/v1/fault/{terminal_id}/{bus_id}")
    def clear_faults(terminal_id: str, bus_id: str) -> dict:
        bus = bus_of(terminal_id, bus_id)
        bus.state.clear_faults()
        return {"ok": True, "faults": {}}

    return app


def _split_words(value: int, width: int) -> list[int]:
    """测点值拆成寄存器字，AB CD（高字在前）。"""
    limit = 0xFFFF if width == 1 else 0xFFFFFFFF
    if not 0 <= value <= limit:
        raise ValueError(f"测点值 {value} 超出 {width} 个字宽范围 0~{limit}")
    if width == 1:
        return [value]
    return [(value >> 16) & 0xFFFF, value & 0xFFFF]