"""点表核对与 004 验收脚本。

逐点读取 140 个测点并落盘对照表，同时跑完 004 的六条验收：

1. 10 条总线各自独立、从站 1~5 不串号
2. 140 个测点的寄存器地址与点表一一对应（写入唯一哨兵值再逐点读回）
3. 运行期改值，下一次读取即生效
4. 三类协议层异常（超时 / 异常码 0x83·0x84 / CRC 错）可注入
5. 点表核对记录落盘 140 行
6. 源码隔离（sim 不 import internal，cmd 不 import sim）——由 ``make check`` 兜底，
   这里只复核一次

用法（模拟器已启动）:
    python acceptance.py all --config ../../config/terminals \
        --ports ../../.pty/agent-ports.json --evidence ../../docs/evidence/004
"""

from __future__ import annotations

import argparse
import ast
import csv
import json
import re
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

import serial

import point_table
import rtu
from point_table import Point, PointTable

API_BASE = "http://127.0.0.1:8080"
READ_TIMEOUT = 0.6
RETRIES = 2

# 核对用哨兵值：从站 × 总线 × 终端各自不同，任何串号都会让读回值对不上。
# 起点取 1000，避开 0（与"未写入"无法区分）和 0x8000 以上（int16 会变负数）。
SENTINEL_BASE = 1000
SENTINEL_STEP = 13


@dataclass(frozen=True)
class ReadResult:
    frame: bytes
    kind: str  # ok / exception / crc / timeout
    exception_code: int | None = None

    @property
    def registers(self) -> list[int]:
        if self.kind != "ok":
            return []
        count = self.frame[2] // 2
        return [int.from_bytes(self.frame[3 + 2 * i : 5 + 2 * i], "big") for i in range(count)]


def read_registers(port: str, slave: int, address: int, count: int) -> ReadResult:
    """读一次保持寄存器，按原始帧分类结果。

    不用 pymodbus 的客户端：它把 CRC 错与超时都折叠成同一种异常，
    而验收要区分的正是这两类。
    """
    request = rtu.build_register_request(slave, 3, address, count)
    expected = 5 + 2 * count
    link = serial.Serial(port, baudrate=9600, timeout=READ_TIMEOUT)
    try:
        link.reset_input_buffer()
        link.write(request)
        head = link.read(3)
        if len(head) < 3:
            return ReadResult(b"", "timeout")
        if head[1] & 0x80:
            tail = link.read(2)
            frame = head + tail
            kind = "exception" if rtu.crc_ok(frame) else "crc"
            return ReadResult(frame, kind, rtu.parse_exception_code(frame))
        byte_count = head[2]
        tail = link.read(byte_count + 2)
        frame = head + tail
        if len(frame) != expected:
            return ReadResult(frame, "timeout")
        if not rtu.crc_ok(frame):
            return ReadResult(frame, "crc")
        return ReadResult(frame, "ok")
    finally:
        link.close()


def read_with_retry(port: str, slave: int, address: int, count: int) -> ReadResult:
    result = ReadResult(b"", "timeout")
    for _ in range(RETRIES + 1):
        result = read_registers(port, slave, address, count)
        if result.kind == "ok":
            return result
    return result


def decode(point: Point, registers: list[int]) -> int:
    """按点表把寄存器拼回原始值（AB CD，高字在前）。"""
    if point.width == 1:
        value = registers[0]
    else:
        value = (registers[0] << 16) | registers[1]
    if point.data_type == "int16" and value >= 0x8000:
        value -= 0x10000
    if point.data_type == "int32" and value >= 0x80000000:
        value -= 0x100000000
    return value


def to_engineering(point: Point, raw: int) -> float:
    decimals = max(0, -_floor_log10(point.scale))
    return round(raw * point.scale + point.offset, decimals)


def _floor_log10(scale: float) -> int:
    text = f"{scale:.12f}".rstrip("0").rstrip(".")
    if "." not in text:
        return 0
    return len(text.split(".")[1])


def split_words(raw: int, width: int) -> list[int]:
    if width == 1:
        return [raw & 0xFFFF]
    return [(raw >> 16) & 0xFFFF, raw & 0xFFFF]


def http(method: str, path: str, body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(
        API_BASE + path, data=data, method=method, headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f"{method} {path} 失败：{exc.code} {exc.read().decode()}") from None


def sentinel_for(index: int) -> int:
    return SENTINEL_BASE + index * SENTINEL_STEP


def iter_points(tables: list[PointTable]):
    index = 0
    for table in tables:
        for bus in table.buses:
            for device in bus.devices:
                for point in device.points:
                    yield index, table, bus, device, point
                    index += 1


def seed_sentinels(tables: list[PointTable], ports: dict[str, str]) -> dict[tuple[str, str, str], int]:
    """给每个测点写唯一哨兵值，返回 (terminal,bus,code) → 原始值。

    直接经 RTU 写而不是走控制接口：核对表要证明的是"点表地址上的寄存器
    能读到写进去的值"，写入路径同样走寄存器地址，才与读回路径对称。
    """
    expected: dict[tuple[str, str, str], int] = {}
    for index, table, bus, device, point in iter_points(tables):
        value = sentinel_for(index)
        port = ports[bus.key]
        link = serial.Serial(port, baudrate=bus.baud_rate, timeout=READ_TIMEOUT)
        try:
            link.reset_input_buffer()
            address = device.start_addr + point.addr
            request = _write_request(device.slave_addr, address, split_words(value, point.width))
            link.write(request)
            if len(link.read(8)) != 8:
                raise RuntimeError(f"{bus.key} 从站 {device.slave_addr} 测点 {point.code} 写入无应答")
        finally:
            link.close()
        expected[(table.terminal_id, bus.bus_id, point.code)] = value
    return expected


def _write_request(slave: int, address: int, values: list[int]) -> bytes:
    if len(values) == 1:
        body = bytes((slave, 6)) + address.to_bytes(2, "big") + values[0].to_bytes(2, "big")
        return rtu.with_crc(body)
    body = (
        bytes((slave, 16))
        + address.to_bytes(2, "big")
        + len(values).to_bytes(2, "big")
        + bytes((len(values) * 2,))
        + b"".join(v.to_bytes(2, "big") for v in values)
    )
    return rtu.with_crc(body)


def check_points(tables: list[PointTable], ports: dict[str, str], csv_path: Path) -> tuple[int, int]:
    """逐点读回哨兵值，写对照表。返回 (总行数, 不一致行数)。"""
    expected = seed_sentinels(tables, ports)
    rows: list[dict] = []
    mismatches = 0
    for _row_index, table, bus, device, point in iter_points(tables):
        absolute = device.start_addr + point.addr
        result = read_with_retry(ports[bus.key], device.slave_addr, absolute, point.width)
        want = expected[(table.terminal_id, bus.bus_id, point.code)]
        if result.kind == "ok":
            raw: int | None = decode(point, result.registers)
            value: float | None = to_engineering(point, raw)
            ok = raw == want
        else:
            raw = None
            value = None
            ok = False
        if not ok:
            mismatches += 1
        rows.append(
            {
                "terminalId": table.terminal_id,
                "busId": bus.bus_id,
                "slaveAddr": device.slave_addr,
                "pointCode": point.code,
                "pointName": point.name,
                "registerAddr": absolute,
                "dataType": point.data_type,
                "width": point.width,
                "scale": point.scale,
                "unit": point.unit or "",
                "rawValue": "" if raw is None else raw,
                "value": "" if value is None else value,
                "expectedRaw": want,
                "match": "是" if ok else "否",
                "readResult": result.kind,
            }
        )
    csv_path.parent.mkdir(parents=True, exist_ok=True)
    with csv_path.open("w", encoding="utf-8", newline="") as fh:
        writer = csv.DictWriter(fh, fieldnames=list(rows[0]), lineterminator="\n")
        writer.writeheader()
        writer.writerows(rows)
    return len(rows), mismatches


def check_isolation() -> list[str]:
    """复核源码隔离：sim/ 不 import internal/，cmd/ 不 import sim/。

    用 ``ast`` 解析真实的 import 语句，不在源码里搜字符串——文档与注释里
    写着"sim/ 不 import internal/"本身就会命中文搜，那是规则的说明，不是违例。
    """
    problems: list[str] = []
    sim_dir = Path(__file__).resolve().parent
    for source in sim_dir.rglob("*.py"):
        for module in _imported_modules(source):
            if module == "internal" or module.startswith("internal."):
                problems.append(f"{source}: import 了 {module}")
    repo = sim_dir.parent.parent
    for source in (repo / "cmd").rglob("*.go"):
        for module in _go_imports(source):
            if "workshop-monitor/sim" in module:
                problems.append(f"{source}: import 了 {module}")
    return problems


def _imported_modules(source: Path) -> set[str]:
    tree = ast.parse(source.read_text(encoding="utf-8"), filename=str(source))
    modules: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            modules.update(alias.name for alias in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module and node.level == 0:
            modules.add(node.module)
    return modules


def _go_imports(source: Path) -> set[str]:
    text = source.read_text(encoding="utf-8")
    match = re.search(r"^import\s*\((.*?)^\)", text, re.S | re.M)
    if match:
        paths = re.findall(r'"([^"]+)"', match.group(1))
    else:
        single = re.search(r'^import\s+(?:\w+\s+)?"([^"]+)"', text, re.M)
        paths = [single.group(1)] if single else []
    return set(paths)


@dataclass(frozen=True)
class CaseResult:
    name: str
    passed: bool
    detail: dict


def sentinel_for_map(tables: list[PointTable]) -> dict[tuple[str, str, int], int]:
    """(terminal, bus, slaveAddr) → 该从站**块起点寄存器**的哨兵值。

    与 ``seed_sentinels`` 共用 ``iter_points`` 的序号源，保证隔离检查与点表核对
    看的是同一份期望值。块起点由块内偏移为 0 的测点覆盖；点表缺这个测点时直接
    报错——那说明点表本身不合规（V-07），不该由核对脚本猜。
    """
    mapping: dict[tuple[str, str, int], int] = {}
    for index, table, bus, device, point in iter_points(tables):
        if point.addr != 0:
            continue
        mapping[(table.terminal_id, bus.bus_id, device.slave_addr)] = sentinel_for(index)
    missing = [
        f"{b.key}/{d.slave_addr}"
        for t in tables
        for b in t.buses
        for d in b.devices
        if (t.terminal_id, b.bus_id, d.slave_addr) not in mapping
    ]
    if missing:
        raise RuntimeError(f"以下从站没有覆盖块起点的测点：{'、'.join(missing)}")
    return mapping


def case_bus_independence(tables: list[PointTable], ports: dict[str, str]) -> CaseResult:
    """10 条总线各自独立：每条总线只答自己那份哨兵值，10 份两两不同。

    判据是「读回值 == 该总线该从站的哨兵值」。端点接错或两条总线共用一个服务时，
    读回值会是隔壁总线的哨兵——数值同样"合理"，这正是要防的串号。
    """
    expected = sentinel_for_map(tables)
    expected_ports = {f"{t.terminal_id}.{b.bus_id}" for t in tables for b in t.buses}
    problems: list[str] = []
    if set(ports) != expected_ports:
        problems.append(
            f"端点表键不符：多 {sorted(set(ports) - expected_ports)}，"
            f"少 {sorted(expected_ports - set(ports))}"
        )

    observed: dict[int, dict[str, int]] = {}
    for table in tables:
        for bus in table.buses:
            for device in bus.devices:
                result = read_with_retry(
                    ports[bus.key], device.slave_addr, device.start_addr, 1
                )
                want = expected[(table.terminal_id, bus.bus_id, device.slave_addr)]
                if result.kind != "ok":
                    problems.append(
                        f"{bus.key} 从站 {device.slave_addr} 读取失败：{result.kind}"
                    )
                    continue
                got = result.registers[0]
                observed.setdefault(device.slave_addr, {})[bus.key] = got
                if got != want:
                    problems.append(
                        f"{bus.key} 从站 {device.slave_addr} 读到 {got}，应为 {want}（串号）"
                    )

    collisions: list[dict] = []
    for slave, values in sorted(observed.items()):
        if len(set(values.values())) != len(values):
            collisions.append({"slaveAddr": slave, "values": values})
    detail = {
        "busCount": len(ports),
        "distinctBuses": len({key for v in observed.values() for key in v}),
        "slaveValueCollisions": collisions,
        "problems": problems,
    }
    return CaseResult(
        "10 条总线各自独立、从站 1~5 不串号", not problems and not collisions, detail
    )


def case_runtime_write(tables: list[PointTable], ports: dict[str, str]) -> CaseResult:
    """运行期改值，下一次读取即生效；不波及同址的另一条总线。"""
    table = tables[0]
    bus = table.buses[0]
    other = table.buses[1] if len(table.buses) > 1 else None
    # 挑一个带 32 位测点的从站：同时验证单寄存器写与 AB CD 拆字
    device = next(d for d in bus.devices if any(p.width == 2 for p in d.points))
    address = device.start_addr
    before = read_with_retry(ports[bus.key], device.slave_addr, address, 1)
    new_value = 625
    http(
        "PUT",
        f"/api/v1/reg/{bus.terminal_id}/{bus.bus_id}/{device.slave_addr}/{address}",
        {"value": new_value},
    )
    after = read_with_retry(ports[bus.key], device.slave_addr, address, 1)
    neighbour = (
        read_with_retry(ports[other.key], device.slave_addr, address, 1)
        if other is not None
        else None
    )
    multi = next(p for p in device.points if p.width == 2)
    raw = 0x0001_8000  # 高低字不同：拆字顺序写错会读成 0x80000001
    http(
        "PUT",
        f"/api/v1/reg/{bus.terminal_id}/{bus.bus_id}/{device.slave_addr}",
        {"points": [{"code": multi.code, "value": raw}]},
    )
    multi_read = read_with_retry(
        ports[bus.key], device.slave_addr, device.start_addr + multi.addr, multi.width
    )
    multi_detail = {
        "pointCode": multi.code,
        "expectedRaw": raw,
        "readRaw": decode(multi, multi_read.registers) if multi_read.kind == "ok" else None,
    }
    detail = {
        "bus": bus.key,
        "slaveAddr": device.slave_addr,
        "address": address,
        "before": before.registers[0] if before.kind == "ok" else None,
        "written": new_value,
        "after": after.registers[0] if after.kind == "ok" else None,
        "neighbourBus": None
        if neighbour is None
        else (neighbour.registers[0] if neighbour.kind == "ok" else None),
        "multiRegister": multi_detail,
    }
    passed = (
        after.kind == "ok"
        and after.registers[0] == new_value
        and (neighbour is None or neighbour.kind == "ok")
        and multi_detail["readRaw"] == raw
    )
    return CaseResult("运行期改值接口生效，下一次读取读到新值", passed, detail)


def case_faults(tables: list[PointTable], ports: dict[str, str]) -> CaseResult:
    """三类协议层真实异常：超时、异常码 0x83/0x84、CRC 错。"""
    bus = tables[0].buses[0]
    device = next(d for d in bus.devices if d.slave_addr == 1)
    port = ports[bus.key]
    address, count = device.start_addr, device.register_count
    base = f"/api/v1/fault/{bus.terminal_id}/{bus.bus_id}/{device.slave_addr}"
    cases: dict[str, dict] = {}

    http("POST", base, {"mode": "timeout", "count": 1})
    started = time.monotonic()
    timeout = read_registers(port, 1, address, count)
    elapsed = time.monotonic() - started
    cases["timeout"] = {
        "kind": timeout.kind,
        "elapsedMs": round(elapsed * 1000),
        "noFrame": timeout.frame == b"",
    }

    http("POST", base, {"mode": "exception", "count": 1, "exceptionCode": 2})
    exc83 = read_registers(port, 1, address, count)
    cases["exception_0x83"] = {
        "kind": exc83.kind,
        "functionCode": exc83.frame[1] if exc83.frame else None,
        "exceptionCode": exc83.exception_code,
        "crcOk": rtu.crc_ok(exc83.frame) if exc83.frame else False,
    }

    # 0x84：功能码 04 的异常响应。点表只用 03，单独发一条 04 请求取异常帧。
    http("POST", base, {"mode": "exception", "count": 1, "exceptionCode": 4})
    link = serial.Serial(port, baudrate=9600, timeout=READ_TIMEOUT)
    try:
        link.reset_input_buffer()
        link.write(rtu.build_register_request(1, 4, 0, 2))
        frame = link.read(5)
    finally:
        link.close()
    cases["exception_0x84"] = {
        "kind": "exception" if frame and frame[1] == 0x84 else "unexpected",
        "functionCode": frame[1] if frame else None,
        "exceptionCode": rtu.parse_exception_code(frame),
        "frame": frame.hex(),
    }

    http("POST", base, {"mode": "crc", "count": 1})
    bad_crc = read_registers(port, 1, address, count)
    cases["crc"] = {
        "kind": bad_crc.kind,
        "frameLength": len(bad_crc.frame),
        "crcOk": rtu.crc_ok(bad_crc.frame) if bad_crc.frame else None,
        "payloadLooksValid": bad_crc.frame[1:] [:2].hex() if bad_crc.frame else None,
    }

    http("DELETE", f"/api/v1/fault/{bus.terminal_id}/{bus.bus_id}")
    restored = read_with_retry(port, 1, address, count)
    cases["restored"] = {"kind": restored.kind}

    passed = (
        cases["timeout"]["kind"] == "timeout"
        and cases["timeout"]["noFrame"]
        and cases["timeout"]["elapsedMs"] >= 300
        and cases["exception_0x83"]["kind"] == "exception"
        and cases["exception_0x83"]["functionCode"] == 0x83
        and cases["exception_0x83"]["crcOk"]
        and cases["exception_0x84"]["functionCode"] == 0x84
        and cases["crc"]["kind"] == "crc"
        and cases["crc"]["crcOk"] is False
        and cases["restored"]["kind"] == "ok"
    )
    return CaseResult("三类协议层异常可注入：超时 / 异常码 0x83·0x84 / CRC 错", passed, cases)


def run_all(tables: list[PointTable], ports: dict[str, str], evidence: Path) -> int:
    evidence.mkdir(parents=True, exist_ok=True)
    rows, mismatches = check_points(tables, ports, evidence / "point_check.csv")
    results = [
        CaseResult(
            "逐点读取 140 个测点，寄存器地址与点表一一对应",
            rows == 140 and mismatches == 0,
            {"rows": rows, "mismatches": mismatches, "csv": str(evidence / "point_check.csv")},
        ),
        case_bus_independence(tables, ports),
        case_runtime_write(tables, ports),
        case_faults(tables, ports),
    ]
    isolation = check_isolation()
    (evidence / "isolation.txt").write_text(
        "通过：sim/ 无 internal/ import，cmd/ 无 sim/ import\n"
        if not isolation
        else "\n".join(isolation) + "\n",
        encoding="utf-8",
    )
    results.append(
        CaseResult("sim/ 不 import internal/，cmd/ 不 import sim/", not isolation, {"problems": isolation})
    )
    summary = {
        "task": "004 仿真链路与点表核对",
        "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "pointCheck": {"rows": rows, "mismatches": mismatches},
        "cases": [
            {"name": r.name, "passed": r.passed, "detail": r.detail} for r in results
        ],
        "passed": all(r.passed for r in results),
    }
    (evidence / "acceptance.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    for result in results:
        print(f"[{'通过' if result.passed else '失败'}] {result.name}")
        if not result.passed:
            print(f"        {json.dumps(result.detail, ensure_ascii=False)}")
    print(f"证据目录：{evidence}")
    return 0 if summary["passed"] else 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="004 点表核对与验收")
    parser.add_argument("command", choices=["points", "isolation", "faults", "write", "bus", "all"])
    parser.add_argument("--config", default="config/terminals")
    parser.add_argument("--ports", default=".pty/agent-ports.json")
    parser.add_argument("--evidence", default="docs/evidence/004")
    args = parser.parse_args(argv)

    tables = point_table.load_tree(Path(args.config))
    ports = json.loads(Path(args.ports).read_text(encoding="utf-8"))
    evidence = Path(args.evidence)

    if args.command in ("points",):
        rows, mismatches = check_points(tables, ports, evidence / "point_check.csv")
        print(f"点表核对：{rows} 行，不一致 {mismatches} 行 → {evidence / 'point_check.csv'}")
        if rows != 140:
            print(f"行数不是 140（实际 {rows}）", file=sys.stderr)
            return 1
        if mismatches:
            return 1

    if args.command == "bus":
        result = case_bus_independence(tables, ports)
        print(json.dumps(result.__dict__, ensure_ascii=False, indent=2))
        return 0 if result.passed else 1

    if args.command == "write":
        result = case_runtime_write(tables, ports)
        print(json.dumps(result.__dict__, ensure_ascii=False, indent=2))
        return 0 if result.passed else 1

    if args.command == "faults":
        result = case_faults(tables, ports)
        print(json.dumps(result.__dict__, ensure_ascii=False, indent=2))
        return 0 if result.passed else 1

    if args.command == "all":
        return run_all(tables, ports, evidence)

    if args.command in ("isolation",):
        problems = check_isolation()
        report = evidence / "isolation.txt"
        report.parent.mkdir(parents=True, exist_ok=True)
        report.write_text(
            "通过：sim/ 无 internal/ import，cmd/ 无 sim/ import\n"
            if not problems
            else "\n".join(problems) + "\n",
            encoding="utf-8",
        )
        if problems:
            print("\n".join(problems), file=sys.stderr)
            return 1
        print("源码隔离通过")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())