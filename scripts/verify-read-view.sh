#!/usr/bin/env bash
# 013 验收：逐项验证读视图面板的可查、只读与口径。
#
# 前置：docker 可用、全栈已起（make up）、.env 已填、数据库已 seed。
# 用法：scripts/verify-read-view.sh [Grafana 地址]，默认 http://localhost:3000
#
# 不依赖浏览器：面板 JSON 由 Grafana 读文件纳管，这里直接走它的 API 与查询接口，
# 校验的是 Grafana 实际执行的那条 SQL，而不是另写一份对账 SQL。
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GRAFANA=${1:-${GRAFANA_URL:-http://localhost:3000}}
DASH_DIR=$ROOT/deploy/grafana/dashboards
PANELS=(overview realtime switch history energy alarms raw config)

fail=0
ok() { echo "  ok   $*"; }
ng() {
  echo "  FAIL $*"
  fail=1
}

# 把面板里的 Grafana 宏替换成可执行的字面量，用于独立校验 SQL。
# $__timeFilter / $__timeTo / $__timeFrom / $__interval 由 Grafana 在服务端展开，
# 这里用固定窗口模拟，验证的是同一条 SQL 在四种时间范围下都能执行。
expand_sql() {
  python3 - "$1" "$2" "$3" "$4" <<'PY'
import sys
sql, span, frm, to = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
for col in ("collect_time", "receive_time", "bucket", "m.collect_time",
            "x.bucket", "a.occur_time"):
    sql = sql.replace("$__timeFilter(%s)" % col, "%s BETWEEN '%s' AND '%s'" % (col, frm, to))
sql = sql.replace("$__timeTo()::timestamptz", "'%s'::timestamptz" % to)
sql = sql.replace("$__timeFrom()::timestamptz", "'%s'::timestamptz" % frm)
sql = sql.replace("$__interval", "%dms" % max(1000, span * 1000))
# 变量取一组固定值，多选按 Grafana 的 sqlstring 规则展开成列表
sql = (sql.replace("${workshop:sqlstring}", "'一号车间','二号车间'")
          .replace("${esn:sqlstring}", "'ESN-ZX-0001','ESN-ZX-0002'")
          .replace("${point:sqlstring}", "'A.1.motor_temp','A.2.p_total'")
          .replace("${parse_status:sqlstring}", "'OK','PARSE_ERROR'")
          .replace("${seq:raw}", ""))
print(sql)
PY
}

# 走 Grafana 自己的查询接口执行面板 SQL，得到与面板一致的结果。
run_panel_query() {
  python3 - "$GRAFANA" "$1" "$2" "$3" "$4" <<'PY'
import json, sys, urllib.error, urllib.request
base, sql, fmt, frm, to = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), int(sys.argv[5])
body = {"queries": [{"refId": "A", "format": fmt, "rawSql": sql,
                     "datasource": {"uid": "workshop-pg",
                                    "type": "grafana-postgresql-datasource"},
                     "intervalMs": 60000, "maxDataPoints": 1000}],
        "from": str(frm), "to": str(to)}
req = urllib.request.Request(base + "/api/ds/query",
                             data=json.dumps(body).encode(),
                             headers={"Content-Type": "application/json"})
try:
    r = json.load(urllib.request.urlopen(req))["results"]["A"]
except urllib.error.HTTPError as e:
    print("HTTP", e.code, e.read().decode()[:200])
    sys.exit(1)
if r.get("error"):
    print("ERR", r["error"])
    sys.exit(1)
frames = r.get("frames", [])
rows = sum(len(f.get("data", {}).get("values", [[]])[0]) for f in frames if f.get("data", {}).get("values"))
print("ok", len(frames), rows)
PY
}

http_code() {
  curl -s -o /dev/null -w '%{http_code}' "$@"
}

panels_of() {
  python3 - "$1" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for p in d["panels"]:
    for t in p.get("targets", []):
        if t.get("rawSql"):
            print("\t".join([p["title"].replace("\t", " "), t.get("format", "table"),
                             t["rawSql"].replace("\n", " ")]))
PY
}

# 临时文件按退出清理；SQL 带换行与引号，放文件而不是命令行参数。
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "验收：$GRAFANA"

# ---------------------------------------------------------------- 1 服务可达且匿名可读
if [ "$(http_code "$GRAFANA/api/health")" = "200" ]; then
  ok "Grafana 健康检查通过"
else
  ng "Grafana 不可达（$GRAFANA/api/health）"
  echo "自检未通过"
  exit 1
fi

search=$(curl -s "$GRAFANA/api/search")
count=$(printf '%s' "$search" | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
if [ "$count" = "8" ]; then
  ok "匿名访客可列出 8 个面板"
else
  ng "匿名访客只列出 $count 个面板（期望 8）"
fi

missing=0
for uid in "${PANELS[@]}"; do
  [ "$(http_code "$GRAFANA/api/dashboards/uid/workshop-$uid")" = "200" ] || {
    ng "面板 workshop-$uid 不可读"
    missing=1
  }
done
[ "$missing" -eq 0 ] && ok "8 个面板全部可读"

# ---------------------------------------------------------------- 2 只读：任何写操作都被拒
for path in /api/dashboards/db /api/datasources /api/admin/users; do
  code=$(http_code -X POST "$GRAFANA$path" -H 'Content-Type: application/json' -d '{}')
  if [ "$code" = "403" ] || [ "$code" = "401" ]; then
    ok "写接口 $path 被拒（$code）"
  else
    ng "写接口 $path 未被拒（$code）"
  fi
done
code=$(http_code -X DELETE "$GRAFANA/api/dashboards/uid/workshop-overview")
if [ "$code" = "403" ] || [ "$code" = "401" ]; then
  ok "删除面板被拒（$code）"
else
  ng "删除面板未被拒（$code）"
fi
code=$(http_code -u admin:admin "$GRAFANA/api/user")
if [ "$code" = "401" ] || [ "$code" = "403" ]; then
  ok "默认管理员账号不可用（$code）"
else
  ng "默认管理员账号仍可登录（$code）"
fi

# ---------------------------------------------------------------- 3 数据源是只读库账号
ds=$(curl -s "$GRAFANA/api/datasources")
if printf '%s' "$ds" | python3 -c '
import json, sys
ds = json.load(sys.stdin)
assert len(ds) >= 1, "没有数据源"
for d in ds:
    assert d["user"] == "workshop_readonly", "数据源用户不是 workshop_readonly：%s" % d["user"]
'; then
  ok "数据源以 workshop_readonly 连接"
else
  ng "数据源未使用只读账号"
fi

# ---------------------------------------------------------------- 4 面板元数据：刷新、时区、时间范围
meta_bad=0
for uid in "${PANELS[@]}"; do
  if ! curl -s "$GRAFANA/api/dashboards/uid/workshop-$uid" | python3 -c '
import json, sys
d = json.load(sys.stdin)["dashboard"]
want = ["1h", "24h", "7d", "30d"]
assert d["refresh"] == "5s", "刷新为 %s" % d["refresh"]
assert d["timezone"] == "Asia/Shanghai", "时区为 %s" % d["timezone"]
got = d["timepicker"]["time_options"]
assert got == want, "时间范围为 %s" % got
'; then
    ng "面板 workshop-$uid 的刷新/时区/时间范围不符"
    meta_bad=1
  fi
done
[ "$meta_bad" -eq 0 ] && ok "8 个面板均为 5 秒刷新、Asia/Shanghai、1h/24h/7d/30d"

# ---------------------------------------------------------------- 5 四种时间范围下每条面板 SQL 可执行
ranges=("3600:1h" "86400:24h" "604800:7d" "2592000:30d")
total=0
bad=0
for uid in "${PANELS[@]}"; do
  while IFS=$'\t' read -r title fmt sql; do
    for r in "${ranges[@]}"; do
      span=${r%%:*}
      label=${r##*:}
      to=$(($(date +%s) * 1000))
      from=$((to - span * 1000))
      from_iso=$(date -u -d "@$((from / 1000))" +%Y-%m-%dT%H:%M:%SZ)
      to_iso=$(date -u -d "@$((to / 1000))" +%Y-%m-%dT%H:%M:%SZ)
      printf '%s' "$sql" >"$TMP/q.sql"
      expanded=$(expand_sql "$(cat "$TMP/q.sql")" "$span" "$from_iso" "$to_iso")
      total=$((total + 1))
      if ! out=$(run_panel_query "$expanded" "$fmt" "$from" "$to"); then
        ng "[$uid/$title][$label] $out"
        bad=$((bad + 1))
      fi
    done
  done < <(panels_of "$DASH_DIR/$uid.json")
done
if [ "$bad" -eq 0 ]; then
  ok "8 个面板共 $total 条查询在 1h/24h/7d/30d 下全部可执行"
else
  ng "$bad/$total 条查询执行失败"
fi

# ---------------------------------------------------------------- 6 30 天走分钟聚合，不查明细
plan=$(python3 - "$ROOT" <<'PY'
import json
d = json.load(open("deploy/grafana/dashboards/history.json"))
print(d["panels"][0]["targets"][0]["rawSql"])
PY
)
printf '%s' "$plan" >"$TMP/curve.sql"
to=$(($(date +%s) * 1000))
from=$((to - 30 * 86400 * 1000))
from_iso=$(date -u -d "@$((from / 1000))" +%Y-%m-%dT%H:%M:%SZ)
to_iso=$(date -u -d "@$((to / 1000))" +%Y-%m-%dT%H:%M:%SZ)
expanded=$(expand_sql "$(cat "$TMP/curve.sql")" 2592000 "$from_iso" "$to_iso")
{
  echo 'EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF)'
  echo "$expanded;"
} >"$TMP/plan.sql"
planout=$(docker compose -f "$ROOT/deploy/docker-compose.yml" \
  --env-file "$ROOT/deploy/versions.env" --env-file "$ROOT/.env" \
  exec -T timescaledb sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <"$TMP/plan.sql" 2>&1) || true
if printf '%s' "$planout" | grep -q "on measurement (never executed)"; then
  ok "30 天曲线未扫描明细表（One-Time Filter 剪枝）"
else
  ng "30 天曲线仍在扫描明细表"
fi
if printf '%s' "$planout" | grep -q "_materialized_hypertable"; then
  ok "30 天曲线走了分钟聚合"
else
  ng "30 天曲线未走分钟聚合"
fi

# ---------------------------------------------------------------- 7 最近正常值同屏带时间与质量
if python3 - "$DASH_DIR/realtime.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
p = [x for x in d["panels"] if x["title"] == "最近一次正常值"][0]
sql = p["targets"][0]["rawSql"]
for need in ("最近一次正常值", "采集时间", "质量", "m.quality = 0"):
    assert need in sql, "缺少 %s" % need
PY
then
  ok "「最近一次正常值」同屏包含数值、采集时间、质量"
else
  ng "「最近一次正常值」缺少时间或质量列"
fi

# ---------------------------------------------------------------- 8 界面用语：无配电术语
if python3 - "$DASH_DIR" <<'PY'
import glob, json, sys
bad = ["遥测", "遥信", "遥调", "遥控", "配电", "母线", "断路器", "馈线", "SCADA"]
hits = []
for f in glob.glob(sys.argv[1] + "/*.json"):
    raw = open(f, encoding="utf-8").read()
    hits += ["%s: %s" % (f, t) for t in bad if t in raw]
print("\n".join(hits))
sys.exit(1 if hits else 0)
PY
then
  ok "面板无配电术语"
else
  ng "面板出现配电术语"
fi

# ---------------------------------------------------------------- 9 能耗口径：功率求均值、电能取末值差
if python3 - "$DASH_DIR/energy.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
sql = " ".join(t["rawSql"] for p in d["panels"] for t in p.get("targets", []))
assert "max(m.value) - min(m.value)" in sql, "电能未取末值差"
assert "sum(s.delta)" in sql or "sum(delta)" in sql, "电能未按测点差值汇总"
assert "avg(m.value)" in sql or "avg(x.avg_value)" in sql, "功率未取均值"
assert "sum(m.value)" not in sql, "功率被直接求和"
PY
then
  ok "能耗口径正确：功率取均值、电能取末值差"
else
  ng "能耗口径有误"
fi

if [ "$fail" -ne 0 ]; then
  echo "验收未通过"
  exit 1
fi
echo "验收通过"