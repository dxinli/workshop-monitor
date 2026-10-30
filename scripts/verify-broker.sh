#!/usr/bin/env bash
# 003 验收：逐项验证 Mosquitto 认证与授权。
#
# 起一个临时 broker（不动已在跑的全栈），跑完即删；两套命名方案各跑一遍。
# 前置：docker 可用、.env 已填 MQTT_*_PASSWORD、已执行 scripts/gen-passwd.sh。
#
# 用法：scripts/verify-broker.sh [.env 路径]
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ENV_FILE=${1:-$ROOT/.env}
CONF_DIR=$ROOT/deploy/mosquitto
# shellcheck disable=SC1091
. "$ROOT/deploy/versions.env"
IMAGE="eclipse-mosquitto:${MOSQUITTO_VERSION}"
NAME=ws-mqtt-verify-$$
PORT=18883

fail=0
ok() { echo "  ok   $*"; }
ng() {
  echo "  FAIL $*"
  fail=1
}
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1; }
var_of() { printf 'MQTT_%s_PASSWORD' "$(printf '%s' "$1" | tr 'a-z-' 'A-Z_')"; }
users_of() { awk '$1 == "user" { print $2 }' "$1"; }

[ -f "$ENV_FILE" ] || { echo "缺少 $ENV_FILE" >&2; exit 1; }
[ -f "$CONF_DIR/passwd" ] || {
  echo "缺少 deploy/mosquitto/passwd，先执行 scripts/gen-passwd.sh" >&2
  exit 1
}

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# 临时 broker：与 compose 同样的挂载与入口脚本，只是换个名字与端口。
start_broker() {
  docker run -d --name "$NAME" -p "127.0.0.1:$PORT:1883" \
    -e MQTT_TOPIC_SCHEME="$1" \
    -v "$CONF_DIR:/mosquitto/config:ro" \
    --entrypoint /mosquitto/config/entrypoint.sh "$IMAGE" >/dev/null || return 1

  local user pw
  user=ESN-ZX-0001
  pw=$(env_get "$(var_of "$user")")
  for _ in $(seq 1 50); do
    if can_auth "$user" "/$1/ESN-ZX-0001/data/up" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  echo "broker 未就绪：" >&2
  docker logs "$NAME" >&2
  return 1
}

# 先取完整输出再判定。直接 `| grep -q` 会让 grep 提前退出、
# 给 mosquitto_pub 发 SIGPIPE，配合 pipefail 时结果随机。
denied() {
  local user=$1 pw topic=$2 out
  pw=$(env_get "$(var_of "$user")")
  out=$(docker exec "$NAME" mosquitto_pub -V mqttv5 -d -h localhost -p 1883 \
    -u "$user" -P "$pw" -t "$topic" -m probe -q 1 2>&1)
  case "$out" in
    *"Not authorized"*) return 0 ;;
    *) return 1 ;;
  esac
}

can_auth() {
  local user=$1 topic=$2 pw out
  pw=$(env_get "$(var_of "$user")")
  out=$(docker exec "$NAME" mosquitto_pub -V mqttv5 -d -h localhost -p 1883 \
    -u "$user" -P "$pw" -t "$topic" -m probe -q 1 2>&1)
  case "$out" in
    *"CONNACK (0)"*) return 0 ;;
    *) return 1 ;;
  esac
}

# 一个方案跑一遍：$1 方案名，$2 主题前缀。
suite() {
  local scheme=$1 prefix=$2 pw_adapter user topic n

  echo "[$scheme] 启动临时 broker"
  start_broker "$scheme" || { ng "$scheme 启动"; return; }

  pw_adapter=$(env_get "$(var_of workshop_adapter)")

  # 接入服务可订阅全部上行：5 个账号 × data/event/status 共 15 条，全部收到。
  docker exec "$NAME" mosquitto_sub -h localhost -p 1883 \
    -u workshop_adapter -P "$pw_adapter" -t "$prefix/#" -v -C 15 -W 15 \
    >"/tmp/ws-sub-$scheme.out" 2>&1 &
  local subpid=$!
  sleep 1

  n=0
  for user in $(users_of "$CONF_DIR/acl.$scheme" | grep '^ESN-'); do
    for topic in data event status; do
      docker exec "$NAME" mosquitto_pub -h localhost -p 1883 \
        -u "$user" -P "$(env_get "$(var_of "$user")")" \
        -t "$prefix/$user/$topic/up" -m "{\"from\":\"$user\",\"kind\":\"$topic\"}" -q 1
      n=$((n + 1))
    done
  done
  wait "$subpid"

  if [ "$(grep -c . "/tmp/ws-sub-$scheme.out")" -eq "$n" ]; then
    ok "[$scheme] 接入服务订阅到全部 $n 条上行"
  else
    ng "[$scheme] 接入服务只收到 $(grep -c . "/tmp/ws-sub-$scheme.out")/$n 条上行"
  fi

  if denied ESN-ZX-0001 "$prefix/ESN-ZX-0002/data/up"; then
    ok "[$scheme] 终端 1 发布终端 2 的主题被拒"
  else
    ng "[$scheme] 终端 1 发布终端 2 的主题未被拒"
  fi

  if denied ESN-ZX-0001 "$prefix/ESN-ZX-0002/status/up"; then
    ok "[$scheme] 终端 1 发布终端 2 的状态主题被拒"
  else
    ng "[$scheme] 终端 1 发布终端 2 的状态主题未被拒"
  fi

  if denied workshop_adapter "$prefix/ESN-ZX-0001/data/up"; then
    ok "[$scheme] 接入服务不得发布上行"
  else
    ng "[$scheme] 接入服务可以发布上行"
  fi

  # 越权发布不得进入主题：订阅目标主题，期间以终端 1 身份尝试发布，2 秒内应无消息。
  # 不能只看文件是否为空——mosquitto_sub 超时会把 "Timed out" 写进 stderr。
  docker exec "$NAME" mosquitto_sub -h localhost -p 1883 \
    -u workshop_adapter -P "$pw_adapter" -t "$prefix/ESN-ZX-0002/data/up" \
    -C 1 -W 2 -v >"/tmp/ws-eaves-$scheme.out" 2>&1 &
  subpid=$!
  sleep 1
  denied ESN-ZX-0001 "$prefix/ESN-ZX-0002/data/up" || true
  wait "$subpid"
  if grep -q "$prefix/ESN-ZX-0002/data/up" "/tmp/ws-eaves-$scheme.out"; then
    ng "[$scheme] 越权发布的消息被订阅方收到"
  else
    ok "[$scheme] 越权发布的消息未进入主题"
  fi

  # LWT：持有连接的上报进程被强杀，订阅方应收到 OFFLINE。
  docker exec "$NAME" mosquitto_sub -h localhost -p 1883 \
    -u workshop_adapter -P "$pw_adapter" \
    -t "$prefix/ESN-ZX-0001/status/up" -C 1 -W 10 -v \
    >"/tmp/ws-lwt-$scheme.out" 2>&1 &
  subpid=$!
  sleep 1
  docker exec -d "$NAME" sh -c "sleep 60 | mosquitto_pub -h localhost -p 1883 \
    -u ESN-ZX-0001 -P '$(env_get "$(var_of ESN-ZX-0001)")' \
    -t '$prefix/ESN-ZX-0001/data/up' -l \
    --will-topic '$prefix/ESN-ZX-0001/status/up' --will-payload OFFLINE --will-qos 1"
  sleep 2
  docker exec "$NAME" pkill -9 -x mosquitto_pub || true
  wait "$subpid"
  if grep -q 'OFFLINE' "/tmp/ws-lwt-$scheme.out"; then
    ok "[$scheme] 异常断开发出 OFFLINE"
  else
    ng "[$scheme] 异常断开未收到 OFFLINE"
  fi

  # 匿名连接必须被拒（broker 回 CONNACK 码 5，客户端打印 not authorised）。
  local anon_out
  anon_out=$(docker exec "$NAME" mosquitto_sub -h localhost -p 1883 -t "$prefix/#" \
    -C 1 -W 2 2>&1)
  case "$anon_out" in
    *"not authori"*) ok "[$scheme] 匿名连接被拒" ;;
    *) ng "[$scheme] 匿名连接未被拒：$anon_out" ;;
  esac

  rm -f "/tmp/ws-sub-$scheme.out" "/tmp/ws-eaves-$scheme.out" "/tmp/ws-lwt-$scheme.out"
  docker rm -f "$NAME" >/dev/null 2>&1 || true
}

echo "验收："
suite grid /edge
suite internal /workshop

# 账号集合：两套方案一致，且口令文件与 ACL 对齐。
if diff -q <(users_of "$CONF_DIR/acl.grid") <(users_of "$CONF_DIR/acl.internal") >/dev/null; then
  ok "两套方案的账号集合一致"
else
  ng "两套方案的账号集合不一致"
fi

# 口令文件属主是容器内的 mosquitto(1883)，宿主机读不到，用同一镜像读。
if diff -q <(users_of "$CONF_DIR/acl.grid" | sort) \
  <(docker run --rm -v "$CONF_DIR:/m:ro" --entrypoint sh "$IMAGE" \
    -c 'cut -d: -f1 /m/passwd' | sort) >/dev/null; then
  ok "口令文件与 ACL 账号一一对应"
else
  ng "口令文件与 ACL 账号不一致"
fi

# 口令文件不入版本库。
if git -C "$ROOT" check-ignore -q deploy/mosquitto/passwd; then
  ok "口令文件已被 .gitignore 忽略"
else
  ng "口令文件未进 .gitignore"
fi
if git -C "$ROOT" ls-files --error-unmatch deploy/mosquitto/passwd >/dev/null 2>&1; then
  ng "口令文件已被提交到版本库"
else
  ok "口令文件未进版本库"
fi

if [ "$fail" -ne 0 ]; then
  echo "验收未通过"
  exit 1
fi
echo "验收通过"