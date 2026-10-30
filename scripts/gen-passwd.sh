#!/usr/bin/env bash
# 从 .env 生成 Mosquitto 口令文件。口令文件不入版本库。
#
# 账号清单来自 deploy/mosquitto/acl.<scheme> 的 `user` 行——ACL 是账号的唯一来源，
# 避免 ACL 里有的账号口令文件里没有（连接被拒）或反过来（多余账号）。
# 口令变量名按账号推导：ESN-ZX-0001 → MQTT_ESN_ZX_0001_PASSWORD，
# workshop_adapter → MQTT_WORKSHOP_ADAPTER_PASSWORD。
#
# 用法：scripts/gen-passwd.sh [.env 路径]
# mosquitto_passwd 从锁定的镜像里取，宿主机不必装 Mosquitto。
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ENV_FILE=${1:-$ROOT/.env}
ACL_DIR=$ROOT/deploy/mosquitto
OUT=${PASSWD_FILE:-$ACL_DIR/passwd}

if [ ! -f "$ENV_FILE" ]; then
  echo "缺少 $ENV_FILE，先执行 cp .env.example .env 并填写 MQTT_*_PASSWORD" >&2
  exit 1
fi

# 只做 KEY=VALUE 解析，不 source——.env 里可能有命令替换。
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1; }

# 外部环境变量优先于 .env，与 compose 的取值顺序一致。
SCHEME=${MQTT_TOPIC_SCHEME:-$(env_get MQTT_TOPIC_SCHEME)}
SCHEME=${SCHEME:-grid}

# 镜像标签唯一来源 deploy/versions.env，脚本里不写死版本。
# shellcheck disable=SC1091
. "$ROOT/deploy/versions.env"
IMAGE="eclipse-mosquitto:${MOSQUITTO_VERSION}"

case "$SCHEME" in
  grid | internal) ;;
  *)
    echo "MQTT_TOPIC_SCHEME=$SCHEME 不支持，只能是 grid 或 internal" >&2
    exit 1
    ;;
esac

ACL_FILE=$ACL_DIR/acl.$SCHEME
[ -f "$ACL_FILE" ] || { echo "缺少 $ACL_FILE" >&2; exit 1; }

# 两套方案的账号集合必须一致，否则切换方案时口令文件对不上。
users_of() { awk '$1 == "user" { print $2 }' "$1"; }
if ! diff -q <(users_of "$ACL_DIR/acl.grid") <(users_of "$ACL_DIR/acl.internal") >/dev/null; then
  echo "acl.grid 与 acl.internal 的 user 行不一致，先补齐再生成口令文件" >&2
  exit 1
fi

# ESN-ZX-0001 → MQTT_ESN_ZX_0001_PASSWORD
var_of() { printf 'MQTT_%s_PASSWORD' "$(printf '%s' "$1" | tr 'a-z-' 'A-Z_')"; }

# 口令文件由容器内的 mosquitto_passwd 生成，写到宿主机目录。
# 口令经 --env-file 传给容器，不走命令行参数——命令行会出现在进程列表里。
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
umask 077

mapfile -t USERS < <(users_of "$ACL_FILE")
[ "${#USERS[@]}" -gt 0 ] || { echo "$ACL_FILE 里没有 user 行" >&2; exit 1; }

missing=0
for user in "${USERS[@]}"; do
  var=$(var_of "$user")
  value=$(env_get "$var")
  if [ -z "$value" ]; then
    echo "缺少 $var（对应账号 $user），在 $ENV_FILE 中填写" >&2
    missing=1
    continue
  fi
  # 首个账号带 -c 建文件，其余追加。
  create=; [ ! -e "$tmpdir/passwd" ] && create=-c
  {
    printf 'PW_USER=%s\n' "$user"
    printf 'PW_VALUE=%s\n' "$value"
    printf 'PW_CREATE=%s\n' "$create"
  } >"$tmpdir/env"
  docker run --rm \
    --env-file "$tmpdir/env" \
    -v "$tmpdir:/out" \
    --entrypoint sh "$IMAGE" -c \
    'mosquitto_passwd $PW_CREATE -b /out/passwd "$PW_USER" "$PW_VALUE"'
  rm -f "$tmpdir/env"
done
[ "$missing" -eq 0 ] || exit 1

# broker 以 mosquitto(1883) 身份读口令文件；只给属主读写。
docker run --rm -v "$tmpdir:/out" --entrypoint sh "$IMAGE" -c \
  'chown 1883:1883 /out/passwd && chmod 640 /out/passwd'
mkdir -p "$ACL_DIR"
mv "$tmpdir/passwd" "$OUT"

echo "已生成 $OUT（$SCHEME 方案，${#USERS[@]} 个账号）"
echo "注意：ACL 与口令文件变更需重载 broker：docker compose ... restart mosquitto"