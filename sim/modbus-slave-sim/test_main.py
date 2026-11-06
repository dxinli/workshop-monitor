"""main 的装配测试。

钉住信号归属契约：SIGINT/SIGTERM 由 run() 统一接管，控制接口服务的
capture_signals() 不得替换进程信号处理器——uvicorn 0.30+ 默认会抢注。
"""

from __future__ import annotations

import signal
import sys
from pathlib import Path

import uvicorn

sys.path.insert(0, str(Path(__file__).resolve().parent))

import main  # noqa: E402


def test_capture_signals_does_not_replace_handlers():
    server = main.ControlApiServer(uvicorn.Config(app="unused:unused", log_level="warning"))
    before = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
    with server.capture_signals():
        during = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
    assert during == before
