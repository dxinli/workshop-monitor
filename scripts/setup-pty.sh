#!/usr/bin/env bash
# 建立虚拟串口对，模拟 RS485 链路。输出写入 .env 的 PTY_* 变量。
set -euo pipefail

PAIRS=${1:-2}

if ! command -v socat >/dev/null 2>&1; then
  echo "缺少 socat，请先安装（版本 1.8.0.0）" >&2
  exit 1
fi

mkdir -p .pty

for i in $(seq 1 "$PAIRS"); do
  socat -d -d \
    pty,raw,echo=0,link=".pty/bus${i}-a" \
    pty,raw,echo=0,link=".pty/bus${i}-b" \
    >> .pty/socat.log 2>&1 &
  echo "总线 ${i} 已建立：.pty/bus${i}-a / .pty/bus${i}-b"
done

echo "提示：将对应端点写入 .env 的 PTY_A / PTY_B"
