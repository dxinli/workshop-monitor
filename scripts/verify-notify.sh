#!/usr/bin/env bash
# 011 验收：N-01 通知接收 + N-02 投递与重试。
#
# 起一个临时 Node-RED（同一个镜像、同一份 flows.json）与一个临时接收端，
# 跑完即删，不动已在跑的全栈。数据库用 compose 里那台（notify_log 是唯一的留痕处）。
#
# 前置：docker 可用、make up 已起 timescaledb 且已 seed、.env 已填 WORKSHOP_NOTIFY_PASSWORD。
#
# 用法：scripts/verify-notify.sh
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ENV_FILE=${NOTIFY_ENV_FILE:-$ROOT/.env}
# shellcheck disable=SC1091
. "$ROOT/deploy/versions.env"

IMAGE="workshop-monitor/node-red:${NODE_RED_VERSION}"
NAME=ws-notify-verify-$$
DB=deploy-timescaledb-1
RECV_PORT=${NOTIFY_RECV_PORT:-18081}
API_PORT=${NOTIFY_API_PORT:-18801}
WORK=$(mktemp -d)
RECV_LOG=$WORK/received.jsonl

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
  [ -n "${RECV_PID:-}" ] && kill "$RECV_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

sql() { docker exec "$DB" psql -U "$(env_get POSTGRES_USER)" -d "$(env_get POSTGRES_DB)" -tAc "$1"; }

# 接收端：只回 200，把收到的报文逐行落文件。
# 012 的 sim/wecom-mock 是正式接收端，这里只要一个能证明「发出去了」的落点。
cat >"$WORK/recv.py" <<'PY'
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

OUT = sys.argv[1]

class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        with open(OUT, "ab") as f:
            f.write(self.rfile.read(n) + b"\n")
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

    def log_message(self, *a):
        pass

HTTPServer(("127.0.0.1", int(sys.argv[2])), H).serve_forever()
PY

# 与 compose 一致：整目录挂 /data。拷一份到临时目录，避免把 Node-RED 的运行时
# 产物写进版本库；拷过去的就是仓库里那两个文件本身。
stage_flows() {
  mkdir -p "$WORK/nr"
  cp "$ROOT/deploy/node-red/settings.js" "$ROOT/deploy/node-red/flows.json" "$WORK/nr/" || return 1
}

start_receiver() {
  python3 "$WORK/recv.py" "$RECV_LOG" "$RECV_PORT" &
  RECV_PID=$!
  for _ in $(seq 1 50); do
    curl -s -m 1 -o /dev/null -X POST "http://127.0.0.1:$RECV_PORT/probe" -d '{}' && return 0
    sleep 0.1
  done
  echo "接收端未就绪" >&2
  return 1
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
    -e NOTIFY_MAX_RETRY=3 -e NOTIFY_CHANNEL=wecom-mock \
    -e "WECOM_WEBHOOK_URL=http://host.docker.internal:$RECV_PORT/webhook" \
    -v "$WORK/nr:/data" \
    "$IMAGE" >/dev/null || return 1

  for _ in $(seq 1 100); do
    if curl -s -m 1 -o /dev/null "http://127.0.0.1:$API_PORT/api/notify" -X POST -d '{}'; then
      return 0
    fi
    sleep 0.2
  done
  echo "Node-RED 未就绪：" >&2
  docker logs "$NAME" >&2
  return 1
}

# post <json> → 输出 "状态码 响应体"
post() {
  curl -s -m 15 -w '\n%{http_code}' -X POST "http://127.0.0.1:$API_PORT/api/notify" \
    -H 'Content-Type: application/json' --data-binary "$1"
}

# 等到 notify_log 出现指定状态，最多等 $2 秒。
wait_status() {
  local id=$1 limit=$2 i
  for i in $(seq 1 "$limit"); do
    local s
    s=$(sql "SELECT status FROM notify_log WHERE notify_id = '$id'")
    if [ -n "$s" ]; then
      echo "$s"
      return 0
    fi
    sleep 1
  done
  echo ""
}

RECV_COUNT='grep -c . "$RECV_LOG" 2>/dev/null || echo 0'

card_field() {
  python3 - "$RECV_LOG" "$1" <<'PY'
import json, sys
path, field = sys.argv[1], sys.argv[2]
try:
    with open(path) as f:
        rows = [json.loads(line) for line in f if line.strip()]
except FileNotFoundError:
    rows = []
print(rows[-1]["alarm"].get(field, "<缺失>") if rows else "<无报文>")
PY
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
start_receiver || exit 1
start_node_red || exit 1
TID=ESN-ZX-VERIFY
PID=A.1.motor_temp
sql "DELETE FROM notify_log WHERE notify_id LIKE 'NT-$TID-%'" >/dev/null
ok "临时 Node-RED 与接收端已就绪"

echo "验收："

# 1. 参数校验失败回 400/422，且不写投递日志。
before=$(sql "SELECT count(*) FROM notify_log")
out=$(curl -s -m 10 -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$API_PORT/api/notify" \
  -H 'Content-Type: application/json' --data-binary '{bad')
[ "$out" = "400" ] && ok "坏 JSON 返回 400" || ng "坏 JSON 返回 $out（应为 400）"

out=$(post '{"notifyId":"NT-'$TID'-1"}' | tail -n1)
[ "$out" = "422" ] && ok "必填字段缺失返回 422" || ng "必填字段缺失返回 $out（应为 422）"

out=$(post '{"notifyId":"NT-'$TID'-2","terminalId":"'$TID'","pointId":"A.1.nope","alarmType":"HIGH_LIMIT","level":3,"triggerValue":1,"occurTime":1793500500000}' | tail -n1)
[ "$out" = "422" ] && ok "未知测点返回 422" || ng "未知测点返回 $out（应为 422）"

after=$(sql "SELECT count(*) FROM notify_log")
[ "$before" = "$after" ] && ok "参数校验失败不写投递日志" || ng "参数校验失败写了 $((after - before)) 条日志"

# 2. level 1 只入库不发通知。
recv_before=$(eval "$RECV_COUNT")
resp=$(post '{"notifyId":"NT-'$TID'-L1","terminalId":"ESN-ZX-0001","pointId":"'$PID'","alarmType":"CACHE_WARNING","level":1,"triggerValue":81,"occurTime":1793500501111}')
body=$(printf '%s' "$resp" | head -n1)
code=$(printf '%s' "$resp" | tail -n1)
st=$(wait_status "NT-$TID-L1" 15)
[ "$code" = "200" ] && [ "$st" = "SKIPPED" ] &&
  ok "level 1 受理为 SKIPPED（$body）" || ng "level 1 返回 $code/$st（应为 200/SKIPPED）"
sleep 2
recv_after=$(eval "$RECV_COUNT")
[ "$recv_before" = "$recv_after" ] && ok "level 1 未发出通知" || ng "level 1 发出了通知"

# 3. level 3 立即投递，卡片带 7 个字段。
resp=$(post '{"notifyId":"NT-'$TID'-L3","terminalId":"ESN-ZX-0001","pointId":"'$PID'","alarmType":"HIGH_HIGH_LIMIT","level":3,"triggerValue":108,"occurTime":1793500502222,"reason":"连续 3 次超过高高限 105"}')
code=$(printf '%s' "$resp" | tail -n1)
[ "$code" = "202" ] && ok "level 3 受理返回 202" || ng "level 3 受理返回 $code（应为 202）"

st=$(wait_status "NT-$TID-L3" 20)
[ "$st" = "SENT" ] && ok "level 3 投递成功并写日志 SENT" || ng "level 3 日志状态 $st（应为 SENT）"

missing=""
for pair in "workshop:车间" "pointText:测点" "levelText:级别" "triggerValue:触发值" "occurTime:发生时间" "reason:告警原因" "terminalId:终端"; do
  field=${pair%%:*}
  value=$(card_field "$field")
  case "$value" in
    "" | "<缺失>" | "<无报文>" | "null") missing="$missing $field" ;;
  esac
done
[ -z "$missing" ] && ok "卡片含车间/终端/测点/级别/触发值/发生时间/告警原因 7 项" ||
  ng "卡片缺字段：$missing"

# 4. 同一 notifyId 重复请求丢弃，不再推送。
recv_before=$(eval "$RECV_COUNT")
resp=$(post '{"notifyId":"NT-'$TID'-L3","terminalId":"ESN-ZX-0001","pointId":"'$PID'","alarmType":"HIGH_HIGH_LIMIT","level":3,"triggerValue":108,"occurTime":1793500502222}')
body=$(printf '%s' "$resp" | head -n1)
sleep 2
recv_after=$(eval "$RECV_COUNT")
case "$body" in
  *DUPLICATE*) ;;
  *) ng "重复通知未被识别：$body" ;;
esac
[ "$recv_before" = "$recv_after" ] && ok "重复通知未重复推送" || ng "重复通知被推送了 $((recv_after - recv_before)) 次"

# 5. 投递失败按 1/5/15 秒重试，共 4 次，末端记 FAILED。
# 通道不可达：把 webhook 指到一个没有监听的口上。
kill "$RECV_PID" 2>/dev/null
RECV_PID=""
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" \
  --network deploy_default -p "127.0.0.1:$API_PORT:1880" \
  -e PORT=1880 -e TZ=Asia/Shanghai \
  -e PG_HOST=timescaledb -e PG_PORT=5432 \
  -e PG_DATABASE="$(env_get POSTGRES_DB)" -e PG_USER=workshop_notify \
  -e PG_PASSWORD="$(env_get WORKSHOP_NOTIFY_PASSWORD)" \
  -e NOTIFY_MAX_RETRY=3 -e NOTIFY_CHANNEL=wecom-mock \
  -e WECOM_WEBHOOK_URL=http://127.0.0.1:9/webhook \
  -v "$WORK/nr:/data" \
  "$IMAGE" >/dev/null
for _ in $(seq 1 100); do
  curl -s -m 1 -o /dev/null "http://127.0.0.1:$API_PORT/api/notify" -X POST -d '{}' && break
  sleep 0.2
done

start=$(date +%s)
resp=$(post '{"notifyId":"NT-'$TID'-FAIL","terminalId":"ESN-ZX-0001","pointId":"'$PID'","alarmType":"HIGH_LIMIT","level":2,"triggerValue":92,"occurTime":1793500503333}')
code=$(printf '%s' "$resp" | tail -n1)
elapsed=$(( $(date +%s) - start ))
[ "$code" = "202" ] && [ "$elapsed" -lt 5 ] &&
  ok "通道不可达时受理仍立即返回 202（${elapsed}s）" || ng "通道不可达时受理返回 $code，耗时 ${elapsed}s"

st=$(wait_status "NT-$TID-FAIL" 40)
[ "$st" = "FAILED" ] && ok "通道不可达时末端记 FAILED" || ng "通道不可达时日志状态 $st（应为 FAILED）"

retries=$(sql "SELECT retry_count FROM notify_log WHERE notify_id = 'NT-$TID-FAIL'")
[ "$retries" = "3" ] && ok "重试次数 3（共 4 次尝试）" || ng "重试次数 $retries（应为 3）"

reason=$(sql "SELECT coalesce(fail_reason,'') FROM notify_log WHERE notify_id = 'NT-$TID-FAIL'")
[ -n "$reason" ] && ok "失败原因已记录" || ng "失败原因为空"

echo
if [ "$fail" -eq 0 ]; then
  echo "011 验收通过"
else
  echo "011 验收未通过" >&2
fi
exit "$fail"