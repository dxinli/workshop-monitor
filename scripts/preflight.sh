#!/usr/bin/env bash
# 部署后自检。任一失败即输出原因并退出非 0。
set -uo pipefail

COMPOSE=${COMPOSE:-docker compose -f deploy/docker-compose.yml}
fail=0

check() {
  local name=$1; shift
  if "$@" >/dev/null 2>&1; then
    echo "  ok   $name"
  else
    echo "  FAIL $name"
    fail=1
  fi
}

echo "自检："
check "docker compose 可用" docker compose version
check "versions.env 存在" test -f deploy/versions.env
check "docker-compose.yml 存在" test -f deploy/docker-compose.yml
check ".env 存在" test -f .env
check "config/terminals 存在" test -d config/terminals
check "timescaledb 在跑" $COMPOSE ps --status running timescaledb
check "mosquitto 在跑" $COMPOSE ps --status running mosquitto

if [ "$fail" -ne 0 ]; then
  echo "自检未通过"
  exit 1
fi
echo "自检通过"
