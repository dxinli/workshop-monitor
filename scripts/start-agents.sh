#!/usr/bin/env bash
# 启动 5 台 edge-agent（自研交付件，仿真形态）。终端与平台分别部署，不在 compose 里。
#
# 5 台仅配置与健康口不同：ESN-ZX-0001~0005 对应 9101~9105。
# -ports 指向仿真端点表；真实部署不传，按点表 port 打开串口。
# .env 里的 MQTT_BROKER_URL 与各终端口令只在启动前导入本进程环境，不进命令行。
# 前台等待 5 个子进程，Ctrl-C 一次全停。
#
# 用法：scripts/start-agents.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

[ -f .env ] || { echo "缺少 .env，先执行 cp .env.example .env" >&2; exit 1; }
[ -x bin/edge-agent ] || { echo "缺少 bin/edge-agent，先执行 make build" >&2; exit 1; }
[ -f .pty/agent-ports.json ] || { echo "缺少 .pty/agent-ports.json，先执行 make sim-pty" >&2; exit 1; }
mkdir -p var

set -a
# shellcheck disable=SC1091
. ./.env
set +a

pids=()
cleanup() {
  # 子进程可能已自行退出，清理失败不视为启动错误。
  kill "${pids[@]}" 2>/dev/null || true
}
trap cleanup INT TERM EXIT

for n in 1 2 3 4 5; do
  HEALTH_ADDR="127.0.0.1:910$n" bin/edge-agent \
    -config "config/terminals/ESN-ZX-000$n.json" \
    -ports .pty/agent-ports.json &
  pids+=("$!")
done

wait
