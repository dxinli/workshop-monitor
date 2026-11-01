"""通知接收端入口。

启动顺序：开记录库 → 起 HTTP 服务。库打不开就退出，不做内存降级——
记录是 012 验收的判据，进程活着但记不住，比进程起不来更难发现。

用法:
    python main.py --db var/wecom-mock.db
"""

from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path

import uvicorn

from api import build_app
from store import RecordStore

log = logging.getLogger("wecom-mock")

DEFAULT_HTTP_HOST = "127.0.0.1"
DEFAULT_HTTP_PORT = 8081
DEFAULT_DB = "var/wecom-mock.db"


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="企业微信机器人接收端模拟")
    parser.add_argument("--host", default=DEFAULT_HTTP_HOST, help="监听地址")
    parser.add_argument("--port", type=int, default=DEFAULT_HTTP_PORT, help="监听端口")
    parser.add_argument("--db", default=DEFAULT_DB, help="记录库路径，:memory: 为纯内存")
    parser.add_argument("--log-level", default="INFO", help="日志级别")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=args.log_level.upper(),
        format="ts=%(asctime)s level=%(levelname)s svc=wecom-mock msg=%(message)s",
    )

    path = Path(args.db)
    if args.db != ":memory:":
        path.parent.mkdir(parents=True, exist_ok=True)
    try:
        store = RecordStore(args.db)
    except Exception as exc:  # noqa: BLE001 —— 起不来要给出路径，不能只抛栈
        log.error("记录库打不开 %s: %s", path, exc)
        return 1

    app = build_app(store)
    config = uvicorn.Config(
        app, host=args.host, port=args.port, log_level=args.log_level.lower(), access_log=False
    )
    server = uvicorn.Server(config)

    log.info("接收端 http://%s:%d/webhook，记录库 %s，已有 %d 条",
             args.host, args.port, args.db, store.count())
    try:
        # 不接管信号：uvicorn 默认的处理是把 should_exit 置位再优雅退出，
        # 这正是我们要的。自己包一层 raise KeyboardInterrupt 会绕过优雅退出，
        # `store.close()` 拿不到 WAL 检查点，重启后记录库要重放一遍。
        server.run()
    finally:
        store.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())