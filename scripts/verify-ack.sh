#!/usr/bin/env bash
# 014 验收：N-03 告警确认 + N-04 写视图。
#
# 起一个临时 Node-RED（同一镜像、同一份 flows.json）与一个临时模拟器桩，
# 跑完即删，不动已在跑的全栈。数据库用 compose 里那台。
#
# 前置：docker 可用、make up 已起 timescaledb 且已 seed、.env 已填
#       WORKSHOP_NOTIFY_PASSWORD 与 WORKSHOP_DASHBOARD_PASSWORD。
#
# 用法：scripts/verify-ack.sh
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ENV_FILE=${EDIT_ENV_FILE:-$ROOT/.env}
# shellcheck disable=SC1091
. "$ROOT/deploy/versions.env"

IMAGE="workshop-monitor/node-red:${NODE_RED_VERSION}"
NAME=ws-ack-verify-$$
DB=deploy-timescaledb-1
API_PORT=${EDIT_API_PORT:-18804}
SIM_PORT=${EDIT_SIM_PORT:-18086}
WORK=$(mktemp -d)

fail=0
ok() { echo "  ok   $*"; }
ng() {
  echo "  FAIL $*"
  fail=1
}
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1; }

[ -f "$ENV_FILE" ] || {
  echo "缺少 $ENV_FILE" >&2
  exit 1
}

cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  [ -n "${SIM_PID:-}" ] && kill "$SIM_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

sql() { docker exec "$DB" psql -U "$(env_get POSTGRES_USER)" -d "$(env_get POSTGRES_DB)" -tAc "$1"; }

# 模拟器桩：只实现 /api/v1/scene/{name}，回一份与真模拟器同形的响应。
# 真模拟器要 pty 与 pymodbus，验收脚本不该把这两样也拖进来。
cat >"$WORK/sim_stub.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

HITS = sys.argv[1]

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        self.rfile.read(n)
        with open(HITS, "a", encoding="utf-8") as fh:
            fh.write(self.path + "\n")
        name = self.path.rsplit("/", 1)[-1]
        body = json.dumps({
            "ok": True,
            "scene": name,
            "description": "stub",
            "clearFaults": name == "normal",
            "buses": [{"bus": "ESN-ZX-000%d.A" % i, "writes": 3} for i in range(1, 6)],
        }).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass

HTTPServer(("0.0.0.0", int(sys.argv[2])), H).serve_forever()
PY

start_sim() {
  python3 "$WORK/sim_stub.py" "$WORK/sim_hits.txt" "$SIM_PORT" &
  SIM_PID=$!
  for _ in $(seq 1 50); do
    curl -s -m 1 -o /dev/null -X POST "http://127.0.0.1:$SIM_PORT/probe" -d '{}' && return 0
    sleep 0.1
  done
  echo "模拟器桩未就绪" >&2
  return 1
}

stage_flows() {
  mkdir -p "$WORK/nr"
  cp "$ROOT/deploy/node-red/settings.js" "$WORK/nr/" || return 1
  # 给三个场景函数各补一个 inject 节点，只写进临时目录。
  # 按钮是 UI 事件，脚本点不动；inject 打的是同一条函数 + http 节点链路，
  # 验的是「按钮按下之后发生什么」，不是「按钮长得对不对」。
  python3 - "$ROOT/deploy/node-red/flows.json" "$WORK/nr/flows.json" <<'PY'
import json, sys

src, dst = sys.argv[1], sys.argv[2]
flows = json.load(open(src, encoding="utf-8"))
targets = [n for n in flows if str(n.get("name", "")).startswith("场景 →")]

out = []
for n in flows:
    out.append(n)
for n in targets:
    scene = n["name"].split("→", 1)[1].strip()
    out.append({
        "id": "verify-inject-" + scene,
        "type": "inject",
        "z": n["z"],
        "name": "验收触发 " + scene,
        "props": [{"p": "payload"}],
        "repeat": "",
        "crontab": "",
        "once": False,
        "onceDelay": 0.1,
        "topic": "",
        "payload": scene,
        "payloadType": "str",
        "x": n["x"] - 180,
        "y": n["y"],
        "wires": [[n["id"]]],
    })
json.dump(out, open(dst, "w", encoding="utf-8"), ensure_ascii=False, indent=4)
open(dst, "a").write("\n")
PY
}

start_node_red() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker run -d --name "$NAME" \
    --network deploy_default -p "127.0.0.1:$API_PORT:1880" \
    --add-host host.docker.internal:host-gateway \
    -e PORT=1880 -e TZ=Asia/Shanghai \
    -e PG_HOST=timescaledb -e PG_PORT=5432 \
    -e PG_DATABASE="$(env_get POSTGRES_DB)" -e PG_USER=workshop_notify \
    -e PG_PASSWORD="$(env_get WORKSHOP_NOTIFY_PASSWORD)" \
    -e PG_DASHBOARD_USER=workshop_dashboard \
    -e PG_DASHBOARD_PASSWORD="$(env_get WORKSHOP_DASHBOARD_PASSWORD)" \
    -e SELF_URL=http://127.0.0.1:1880 \
    -e "SIM_URL=http://host.docker.internal:$SIM_PORT" \
    -e GRAFANA_URL=http://localhost:3000 \
    -v "$WORK/nr:/data" \
    "$IMAGE" >/dev/null || return 1

  for _ in $(seq 1 100); do
    if curl -s -m 1 -o /dev/null "http://127.0.0.1:$API_PORT/api/health"; then
      return 0
    fi
    sleep 0.2
  done
  echo "Node-RED 未就绪：" >&2
  docker logs "$NAME" >&2
  return 1
}

post() {
  curl -s -m 15 -w '\n%{http_code}' -X POST "http://127.0.0.1:$API_PORT$1" \
    -H 'Content-Type: application/json' --data-binary "$2"
}
get() { curl -s -m 15 -w '\n%{http_code}' "http://127.0.0.1:$API_PORT$1"; }

TID=ESN-ZX-0140
# 造两条活动告警：一条 ACTIVE 未确认、一条 RECOVERED 未确认。
seed_alarms() {
  sql "DELETE FROM alarm WHERE terminal_id = '$TID'" >/dev/null
  sql "INSERT INTO alarm (dedupe_key, terminal_id, point_id, alarm_type, level, status,
        trigger_value, unit, threshold_value, occur_time, last_time, message)
       VALUES
        ('$TID|A.1.motor_temp|HIGH_LIMIT', '$TID', 'A.1.motor_temp', 'HIGH_LIMIT', 2, 'ACTIVE',
         92, '℃', 85, now() - interval '2 min', now(), '连续 3 次超过高限 > 85'),
        ('$TID|A.4.estop|STATE_ALARM', '$TID', 'A.4.estop', 'STATE_ALARM', 4, 'RECOVERED',
         1, null, 1, now() - interval '5 min', now() - interval '3 min', '状态报警：读数 >= 1')" >/dev/null
  sql "SELECT string_agg(alarm_id::text, ' ') FROM alarm WHERE terminal_id = '$TID'"
}

echo "准备："
docker inspect "$DB" >/dev/null 2>&1 || {
  echo "缺少 $DB，先执行 make up" >&2
  exit 1
}
docker image inspect "$IMAGE" >/dev/null 2>&1 || {
  echo "缺少镜像 $IMAGE，先执行 make up（会按 deploy/node-red 构建）" >&2
  exit 1
}
stage_flows || exit 1
start_sim || exit 1
start_node_red || exit 1
ids=$(seed_alarms)
ACTIVE_ID=${ids%% *}
RECOVERED_ID=${ids##* }
ok "临时 Node-RED、模拟器桩与两条告警已就绪（$ACTIVE_ID / $RECOVERED_ID）"

echo "验收："

# 1. 列出活动告警：ACTIVE 与 RECOVERED 都要在，按级别降序。
body=$(get /api/alarms/active)
code=$(printf '%s' "$body" | tail -n1)
json=$(printf '%s' "$body" | head -n1)
[ "$code" = "200" ] && ok "GET /api/alarms/active 返回 200" || ng "GET /api/alarms/active 返回 $code"

listing=$(printf '%s' "$json" | python3 -c '
import json, sys
rows = json.load(sys.stdin)
mine = [r for r in rows if r["terminalId"] == "ESN-ZX-0140"]
print(len(mine), mine[0]["level"] if mine else "", mine[0]["acknowledged"] if mine else "")
' 2>/dev/null)
case "$listing" in
  "2 4 False") ok "活动清单含 ACTIVE 与 RECOVERED 两条，按级别降序，未确认" ;;
  *) ng "活动清单异常：$listing" ;;
esac

# 2. 确认：写 alarm_ack，不改 alarm.status。
body=$(post /api/ack '{"alarmId":'$ACTIVE_ID',"ackedBy":"张三","note":"到场检查"}')
code=$(printf '%s' "$body" | tail -n1)
resp=$(printf '%s' "$body" | head -n1)
[ "$code" = "200" ] && ok "确认返回 200" || ng "确认返回 $code"
case "$resp" in
  *'"status":"ACKED"'*) ok "确认结果为 ACKED" ;;
  *) ng "确认结果异常：$resp" ;;
esac

st=$(sql "SELECT a.status || '|' || (k.acked_by IS NOT NULL) || '|' || k.note
            FROM alarm a JOIN alarm_ack k ON k.alarm_id = a.alarm_id
           WHERE a.alarm_id = $ACTIVE_ID")
[ "$st" = "ACTIVE|true|到场检查" ] && ok "写 alarm_ack 且 alarm.status 仍是 ACTIVE" ||
  ng "确认后状态为 $st（应为 ACTIVE|true|到场检查）"

# 3. 确认后不再重复提醒：再确认一次不产生第二条记录。
before=$(sql "SELECT count(*) FROM alarm_ack WHERE alarm_id = $ACTIVE_ID")
body=$(post /api/ack '{"alarmId":'$ACTIVE_ID',"ackedBy":"李四"}')
resp=$(printf '%s' "$body" | head -n1)
after=$(sql "SELECT count(*) FROM alarm_ack WHERE alarm_id = $ACTIVE_ID")
case "$resp" in
  *ALREADY_ACKED*) ok "重复确认返回 ALREADY_ACKED" ;;
  *) ng "重复确认返回 $resp" ;;
esac
[ "$before" = "$after" ] && ok "重复确认不写第二条记录" || ng "重复确认写了 $((after - before)) 条"

# 4. 恢复的告警同样可确认。
body=$(post /api/ack '{"alarmId":'$RECOVERED_ID',"ackedBy":"王五"}')
resp=$(printf '%s' "$body" | head -n1)
case "$resp" in
  *'"status":"ACKED"'*) ok "已恢复（RECOVERED）的告警可确认" ;;
  *) ng "已恢复的告警确认返回 $resp" ;;
esac

# 5. 参数校验失败回 400/422 且不落库。
before=$(sql "SELECT count(*) FROM alarm_ack")
out=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$API_PORT/api/ack" \
  -H 'Content-Type: application/json' --data-binary '{bad')
[ "$out" = "400" ] && ok "坏 JSON 返回 400" || ng "坏 JSON 返回 $out"

for payload in '{"alarmId":'"$ACTIVE_ID"'}' '{"ackedBy":"张三"}' '{"alarmId":0,"ackedBy":"张三"}' '{"alarmId":"x","ackedBy":"张三"}'; do
  code=$(post /api/ack "$payload" | tail -n1)
  [ "$code" = "422" ] && ok "非法入参 422：$payload" || ng "非法入参返回 $code：$payload"
done
after=$(sql "SELECT count(*) FROM alarm_ack")
[ "$before" = "$after" ] && ok "参数校验失败不落库" || ng "参数校验失败写了 $((after - before)) 条"

# 6. 不存在的告警号返回 404。
code=$(post /api/ack '{"alarmId":99999999,"ackedBy":"张三"}' | tail -n1)
[ "$code" = "404" ] && ok "不存在的告警号返回 404" || ng "不存在的告警号返回 $code"

# 7. 健康查询。
body=$(get /api/health)
code=$(printf '%s' "$body" | tail -n1)
json=$(printf '%s' "$body" | head -n1)
[ "$code" = "200" ] && ok "GET /api/health 返回 200" || ng "GET /api/health 返回 $code"
case "$json" in
  *'"database":"ok"'*) ok "健康查询报数据库正常" ;;
  *) ng "健康查询异常：$json" ;;
esac

# 8. 场景切换：三个按钮各自打到模拟器桩上。
scene_hits() { grep -c . "$WORK/sim_hits.txt" 2>/dev/null || echo 0; }
for scene in normal alarm safety; do
  before=$(scene_hits)
  curl -s -m 5 -o /dev/null -X POST "http://127.0.0.1:$API_PORT/inject/verify-inject-$scene" -d '{}'
  sleep 1
  after=$(scene_hits)
  if [ "$after" -gt "$before" ] && grep -qx "/api/v1/scene/$scene" "$WORK/sim_hits.txt"; then
    ok "场景按钮「$scene」把请求打到模拟器"
  else
    ng "场景按钮「$scene」未打到模拟器"
  fi
done

# 9. 写视图页面可访问。
code=$(curl -s -m 10 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$API_PORT/dashboard/ops")
[ "$code" = "200" ] && ok "写视图页面 /dashboard/ops 可访问" || ng "写视图页面返回 $code"

# 10. 写视图账号权限：只能写 alarm 与 alarm_ack。
dash_pw=$(env_get WORKSHOP_DASHBOARD_PASSWORD)
can_write_alarm=$(docker exec -e PGPASSWORD="$dash_pw" "$DB" \
  psql -U workshop_dashboard -d "$(env_get POSTGRES_DB)" -tAc \
  "SELECT has_table_privilege('workshop_dashboard','alarm','UPDATE') AND
          has_table_privilege('workshop_dashboard','alarm_ack','INSERT')" 2>/dev/null)
[ "$can_write_alarm" = "t" ] && ok "写视图账号对 alarm / alarm_ack 有写权限" ||
  ng "写视图账号写权限异常：$can_write_alarm"

cannot_config=$(docker exec -e PGPASSWORD="$dash_pw" "$DB" \
  psql -U workshop_dashboard -d "$(env_get POSTGRES_DB)" -tAc \
  "SELECT has_table_privilege('workshop_dashboard','threshold','UPDATE') OR
          has_table_privilege('workshop_dashboard','point','UPDATE')" 2>/dev/null)
[ "$cannot_config" = "f" ] && ok "写视图账号对配置域无写权限" || ng "写视图账号可写配置域"

# 11. adapter 不提供确认接口：确认只在 Node-RED 一处。
[ -z "$(grep -rn 'api/ack' "$ROOT/internal" "$ROOT/cmd" 2>/dev/null)" ] &&
  ok "adapter 侧无确认接口" || ng "adapter 出现了确认接口"

echo
if [ "$fail" -eq 0 ]; then
  echo "014 验收通过"
else
  echo "014 验收未通过" >&2
fi
exit "$fail"