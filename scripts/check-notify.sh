#!/usr/bin/env bash
# 012 通道连通性自检：验证通知通道的地址与凭据走配置、且真能投到接收端。
#
# 三件事分开查，缺哪一件都能单独看出来：
#   1. 配置在 .env 里，版本库里没有写死的通道地址与凭据
#   2. 接收端在 Node-RED 看得见的网络里可达（不是只在宿主机可达）
#   3. 真发一条探针卡片，接收端留痕并能回读
#
# 第 2 项必须从 Node-RED 容器内探：通道是它投的，容器里的 localhost 是容器自己，
# 宿主机上 curl 得通不代表它投得通——这正是 O-19 记的坑。
#
# 用法：scripts/check-notify.sh [--probe]
#   --probe  发一条真实探针卡片。会写一条 notifyId 为 NT-CHECK-* 的记录，
#            用完自己删；不加则只做可达性检查，不产生任何痕迹。
set -uo pipefail

COMPOSE=${COMPOSE:-docker compose -f deploy/docker-compose.yml}
ENV_FILE=${NOTIFY_ENV_FILE:-.env}
NODE_RED=${NOTIFY_NODE_RED:-node-red}
PROBE=0
[ "${1:-}" = "--probe" ] && PROBE=1

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

echo "通道配置检查："

# ---- 1. 配置走 .env，不入版本库
if [ -f .gitignore ] && grep -qx '\.env' .gitignore; then
  ok ".env 在 .gitignore 里"
else
  ng ".env 不在 .gitignore，口令会随提交进版本库"
fi

if git ls-files --error-unmatch "$ENV_FILE" >/dev/null 2>&1; then
  ng "$ENV_FILE 已被版本库跟踪，凭据会进仓库"
else
  ok "$ENV_FILE 未入版本库"
fi

url=$(env_get WECOM_WEBHOOK_URL)
channel=$(env_get NOTIFY_CHANNEL)
enabled=$(env_get WECOM_ENABLED)
[ -n "$url" ] && ok "WECOM_WEBHOOK_URL 已配置" || ng "WECOM_WEBHOOK_URL 为空"
[ -n "$channel" ] && ok "NOTIFY_CHANNEL=$channel" || ng "NOTIFY_CHANNEL 为空"

# 版本库里不许出现写死的通道地址：配置只有 .env 一个来源，
# 界面/流程/脚本里写死一处，切换通道就得改代码（BR-42 要的正是不改代码）。
# 排除检查脚本自身——它们必须写出这些特征串才能找它们。
hardcoded=$(grep -rn --include='*.json' --include='*.sh' --include='*.py' --include='*.js' \
  -e 'wecom-mock:8081' -e 'qyapi\.weixin\.qq\.com' \
  deploy/ scripts/ config/ sim/ 2>/dev/null \
  | grep -vE -e 'check-notify\.sh' -e 'accept-01[0-9]\.py' -e '(^|/)\.env' || true)
if [ -n "$hardcoded" ]; then
  ng "版本库里出现写死的通道地址："
  printf '%s\n' "$hardcoded" | sed 's/^/       /'
else
  ok "无写死的通道地址"
fi

case "${enabled:-true}" in
  false | FALSE) ok "灰度开关关闭（只写 SKIPPED，不投递）" ;;
  *) ok "灰度开关开启" ;;
esac

echo
echo "通道连通检查："

# ---- 2. 从 Node-RED 容器内探接收端
# 用 compose 判服务在不在：docker ps 出的是容器名（deploy-node-red-1），
# 拿服务名去比永远不相等，会把在跑的判成没跑。
node_red_running() {
  [ "$($COMPOSE ps --status running "$NODE_RED" 2>/dev/null | awk 'NR>1 && NF>0' | wc -l)" -gt 0 ]
}

if ! node_red_running; then
  ng "服务 $NODE_RED 没在跑，无法从投递方视角检查（先 make up）"
elif [ -z "$url" ]; then
  ng "WECOM_WEBHOOK_URL 为空，无从检查连通"
else
  # 容器内不一定有 curl，Node-RED 镜像有 busybox wget。
  #
  # 这里只判「通不通」，不判「方法对不对」：wget 用 GET 打 /webhook，
  # 接收端只收 POST，回 405 正说明它活着。有没有 HTTP 响应才分得开
  # 「连得上但方法不对」和「根本连不上」——后者是 012 要防的那类故障。
  err=$($COMPOSE exec -T "$NODE_RED" wget -S -O /dev/null \
    --timeout=5 "$url" 2>&1)
  code=$(printf '%s' "$err" | awk '/^  HTTP/{print $2}' | tail -n 1)
  case "$code" in
    2*) ok "接收端可达（$code）" ;;
    "") ng "接收端不可达（$url）：连接超时或被拒" ;;
    *) ok "接收端可达（$code，GET 打 /webhook 的预期回码，仅证明链路通）" ;;
  esac
fi

# ---- 3. 发一条探针卡片，回读确认留痕
if [ "$PROBE" -eq 1 ]; then
  echo
  echo "探针投递："
  # level 3 才会真的投出去（level 1 只入库不投递，BR-39），
  # 所以探针用 level 3，才能验到「真落到接收端」这一段。
  notify_id="NT-CHECK-$(date +%s%3N)"
  now_ms=$(date +%s%3N)
  probe=$(cat <<JSON
{"notifyId":"$notify_id","terminalId":"ESN-ZX-0001","pointId":"A.1.motor_temp",
 "alarmType":"HIGH_LIMIT","level":3,"triggerValue":0,"occurTime":$now_ms,
 "reason":"通道连通自检探针"}
JSON
)
  code=$(curl -s -m 15 -o /dev/null -w '%{http_code}' -X POST \
    "${NOTIFY_ENDPOINT:-http://localhost:1880/api/notify}" \
    -H 'Content-Type: application/json' --data-binary "$probe")
  if [ "$code" = "202" ]; then
    ok "探针已受理（$notify_id）"
    sleep 3
    if curl -sf -m 5 "${RECORDS_URL:-http://localhost:8081/api/v1/records?limit=20}" 2>/dev/null \
      | grep -q "$notify_id"; then
      ok "接收端留痕，记录可回读"
    else
      ng "接收端查不到探针记录（投递可能失败，查 notify_log 的 status）"
    fi
  else
    ng "探针受理失败，返回 $code"
  fi
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "通道检查通过"
else
  echo "通道检查未通过" >&2
fi
exit "$fail"
