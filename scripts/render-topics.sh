#!/usr/bin/env bash
# 把 config/topics.json 渲染成某一套命名方案的主题实例，供部署与验收核对。
#
# 用法：scripts/render-topics.sh [grid|internal] [esn...]
#   scheme 缺省取 .env 的 MQTT_TOPIC_SCHEME，再缺省为 grid。
#   esn 缺省取 deploy/mosquitto/acl.<scheme> 的终端 user 行。
#
# 输出三列：终端 / 数据 / 事件 / 状态。这是唯一渲染实现，
# 采集应用与接入服务的启动日志按同一模板打印。
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT" || exit 1

env_get() { [ -f .env ] && sed -n "s/^$1=//p" .env | tail -n 1; }

SCHEME=${1:-${MQTT_TOPIC_SCHEME:-$(env_get MQTT_TOPIC_SCHEME)}}
SCHEME=${SCHEME:-grid}
shift 2>/dev/null || true

case "$SCHEME" in
  grid | internal) ;;
  *)
    echo "MQTT_TOPIC_SCHEME=$SCHEME 不支持，只能是 grid 或 internal" >&2
    exit 1
    ;;
esac

ACL=deploy/mosquitto/acl.$SCHEME
[ -f "$ACL" ] || { echo "缺少 $ACL" >&2; exit 1; }

if [ $# -gt 0 ]; then
  ESN_LIST=("$@")
else
  mapfile -t ESN_LIST < <(awk '$1 == "user" && $2 != "workshop_adapter" { print $2 }' "$ACL")
fi

# 用 python 读 JSON：模板里有 {esn} 占位符，sed 在主题里加花括号容易出错。
python3 -I - "$SCHEME" "${ESN_LIST[@]}" <<'PY'
import json, sys

scheme = sys.argv[1]
esns = sys.argv[2:]
topics = json.load(open("config/topics.json", encoding="utf-8"))["schemes"][scheme]

rows = []
for esn in esns:
    rows.append((esn, *[t.replace("{esn}", esn) for t in
                        (topics["dataUp"], topics["eventUp"], topics["statusUp"])]))

w = [max(len(r[i]) for r in rows) for i in range(4)]
print(f"{'终端':<{w[0]}}  {'数据':<{w[1]}}  {'事件':<{w[2]}}  {'状态':<{w[3]}}")
for r in rows:
    print(f"{r[0]:<{w[0]}}  {r[1]:<{w[1]}}  {r[2]:<{w[2]}}  {r[3]:<{w[3]}}")
PY