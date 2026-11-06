#!/usr/bin/env bash
# 启动 proto-adapter（自研交付件）。终端与平台分别部署，不在 compose 里。
#
# .env 里的订阅口令与 PGPASSWORD 只在启动前导入本进程环境，不进命令行
# （database.url 不带口令，pgx 从 PGPASSWORD 补）。二进制由 make build 产出。
#
# 用法：scripts/start-adapter.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

[ -f .env ] || { echo "缺少 .env，先执行 cp .env.example .env" >&2; exit 1; }
[ -x bin/proto-adapter ] || { echo "缺少 bin/proto-adapter，先执行 make build" >&2; exit 1; }

set -a
# shellcheck disable=SC1091
. ./.env
set +a
: "${WORKSHOP_ADAPTER_PASSWORD:?缺少 WORKSHOP_ADAPTER_PASSWORD，在 .env 中填写}"
export PGPASSWORD="$WORKSHOP_ADAPTER_PASSWORD"

exec bin/proto-adapter -config config/adapter.json
