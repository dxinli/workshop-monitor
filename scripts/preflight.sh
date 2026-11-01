#!/usr/bin/env bash
# 部署后自检。12 项，任一失败即输出原因并退出非 0。
#
# 按「部署完能不能用」排列：工具与环境 → 四个容器 → 配置投影 → 时序结构 → 读视图。
# 只用 docker / psql / curl 三种手段，每一项都能在现场独立复核。
#
# 自研服务的 /healthz 不在 compose 里（终端与平台分别部署），
# 需要时用 HEALTH_URLS=http://127.0.0.1:9101/healthz,... 追加检查，不计入 12 项。
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

# running 判定「该服务的容器在跑」。
#
# 不能用 `docker compose ps --status running <svc>` 的退出码：容器停了它只打印表头，
# 退出码仍是 0，检查会永远通过。按行数判：有数据行才说明该服务在跑。
running() {
  local svc=$1
  [ "$($COMPOSE ps --status running "$svc" 2>/dev/null | awk 'NR>1 && NF>0' | wc -l)" -gt 0 ]
}

# 容器内 psql。口令与库名由容器的 POSTGRES_USER/POSTGRES_DB 取，不在命令行落明文。
psql_q() {
  $COMPOSE exec -T timescaledb sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "$1"' _ "$1"
}

echo "自检："

# ---- 1~5 工具与环境
check "1  docker compose 可用" docker compose version
check "2  versions.env 存在" test -f deploy/versions.env
check "3  docker-compose.yml 存在" test -f deploy/docker-compose.yml
check "4  .env 存在" test -f .env
check "5  config/terminals 存在" test -d config/terminals

# ---- 6~9 四个容器
check "6  timescaledb 在跑" running timescaledb
check "7  mosquitto 在跑" running mosquitto
check "8  node-red 在跑" running node-red
check "9  grafana 在跑" running grafana

# ---- 10 配置域投影与 config/ 同步（D-01~D-04）
# 数量不符说明 make seed 没跑或跑在旧配置上：界面与判定都会读到过期点表。
counts=$(psql_q "SELECT (SELECT count(*) FROM monitor.terminal) || '/' ||
                       (SELECT count(*) FROM monitor.device)   || '/' ||
                       (SELECT count(*) FROM monitor.point)    || '/' ||
                       (SELECT count(*) FROM monitor.threshold)" 2>/dev/null | tr -d '[:space:]')
if [ "$counts" = "5/50/140/95" ]; then
  echo "  ok   10 配置域投影与配置一致（$counts）"
else
  echo "  FAIL 10 配置域投影与配置不一致（期望 5/50/140/95，实际 ${counts:-<查询失败>}）"
  echo "       先执行 make seed-sql && make seed"
  fail=1
fi

# ---- 11 时序结构（D-05~D-07）：超表、分钟聚合、两条保留策略
structure=$(psql_q "SELECT
    (SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name = 'measurement') || '/' ||
    (SELECT count(*) FROM timescaledb_information.continuous_aggregates WHERE view_name = 'measurement_1m') || '/' ||
    (SELECT count(*) FROM timescaledb_information.jobs
      WHERE proc_name = 'policy_retention' AND hypertable_name = 'measurement'
        AND config->>'drop_after' = '7 days') || '/' ||
    (SELECT count(*) FROM timescaledb_information.jobs
      WHERE proc_name = 'policy_retention' AND config->>'drop_after' = '30 days')" 2>/dev/null | tr -d '[:space:]')
# 聚合的保留策略挂在内部物化超表（_materialized_hypertable_N）上，按 drop_after 认而不是按名字认：
# 名字随内部编号变化，写死名字的断言会在重建后失效，且失效方式是「看起来没配」。
if [ "$structure" = "1/1/1/1" ]; then
  echo "  ok   11 时序结构齐全（超表 / 分钟聚合 / 明细 7 天 / 聚合 30 天）"
else
  echo "  FAIL 11 时序结构不全（期望 1/1/1/1，实际 ${structure:-<查询失败>}）"
  echo "       超表 / 分钟聚合 / 明细保留 7 天 / 聚合保留 30 天，逐项核对 deploy/timescaledb/init/01_schema.sql"
  fail=1
fi

# ---- 12 读视图：Grafana 报健康，且数据源连得上库
check "12 grafana 健康接口" bash -c '
  curl -sf -m 5 http://localhost:3000/api/health | grep -q "\"database\": *\"ok\""
'

# ---- 附加：自研服务健康检查（不在 compose 里，按需）
if [ -n "${HEALTH_URLS:-}" ]; then
  echo "附加（HEALTH_URLS）："
  IFS=',' read -r -a urls <<<"$HEALTH_URLS"
  for url in "${urls[@]}"; do
    check "  健康检查 $url" curl -sf -m 5 "$url"
  done
fi

if [ "$fail" -ne 0 ]; then
  echo "自检未通过"
  exit 1
fi
echo "自检通过"
