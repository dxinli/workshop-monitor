"""Modbus RTU 从站模拟器入口。

启动顺序：读点表 → 按点表建总线服务 → 启动 10 条总线 → 启动控制接口。
点表读不到就退出，不做降级启动——模拟器少一条总线时，采集侧表现为
"这台终端某条总线全超时"，现场很难与真实故障区分。

用法:
    python main.py --config config/terminals --ports .pty/ports.json

``--ports`` 指向 scripts/setup-pty.sh 写出的端点表，键为 ``terminalId.busId``。
没有该文件时按 ``--port-prefix`` 推导 ``{prefix}{busId 序号}``，仅用于单总线调试。
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import signal
from pathlib import Path

import uvicorn

from api import build_app
from point_table import Bus, load_tree
from server import BusServer

log = logging.getLogger("modbus-slave-sim")

DEFAULT_HTTP_HOST = "127.0.0.1"
DEFAULT_HTTP_PORT = 8080


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Modbus RTU 从站模拟器")
    parser.add_argument("--config", default="config/terminals", help="终端点表目录")
    parser.add_argument("--ports", default=".pty/ports.json", help="端点表 JSON")
    parser.add_argument("--host", default=DEFAULT_HTTP_HOST, help="控制接口地址")
    parser.add_argument("--port", type=int, default=DEFAULT_HTTP_PORT, help="控制接口端口")
    parser.add_argument("--log-level", default="INFO", help="日志级别")
    return parser.parse_args(argv)


def load_ports(path: Path, buses: list[Bus]) -> dict[str, str]:
    if not path.is_file():
        raise SystemExit(f"缺少端点表 {path}，先执行 scripts/setup-pty.sh")
    with path.open(encoding="utf-8") as fh:
        raw = json.load(fh)
    if not isinstance(raw, dict):
        raise SystemExit(f"{path}: 顶层必须是对象，键为 terminalId.busId")
    ports: dict[str, str] = {}
    for bus in buses:
        port = raw.get(bus.key)
        if not isinstance(port, str) or not port:
            raise SystemExit(f"{path}: 缺少总线 {bus.key} 的端点")
        ports[bus.key] = port
    return ports


async def run(args: argparse.Namespace) -> int:
    tables = load_tree(Path(args.config))
    buses = [bus for table in tables for bus in table.buses]
    ports = load_ports(Path(args.ports), buses)
    servers = {bus.key: BusServer(bus, ports[bus.key]) for bus in buses}

    for server in servers.values():
        await server.start()
        log.info("总线 %s 已就绪，端点 %s", server.bus.key, server.port)

    app = build_app(servers)
    config = uvicorn.Config(
        app, host=args.host, port=args.port, log_level=args.log_level.lower(), access_log=False
    )
    http = uvicorn.Server(config)
    http.install_signal_handlers = lambda: None  # 信号统一由本函数处理

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stop.set)
        except NotImplementedError:
            # Windows 的事件循环不支持 add_signal_handler，靠 KeyboardInterrupt 退出。
            break

    http_task = asyncio.create_task(http.serve(), name="control-api")
    log.info("控制接口 http://%s:%d，共 %d 条总线", args.host, args.port, len(servers))

    await stop.wait()
    log.info("收到退出信号，停止模拟器")
    http.should_exit = True
    await http_task
    for server in servers.values():
        await server.stop()
    return 0


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=args.log_level.upper(),
        format="ts=%(asctime)s level=%(levelname)s svc=modbus-slave-sim msg=%(message)s",
    )
    try:
        return asyncio.run(run(args))
    except KeyboardInterrupt:
        return 0


if __name__ == "__main__":
    raise SystemExit(main())