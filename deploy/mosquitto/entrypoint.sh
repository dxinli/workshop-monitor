#!/bin/sh
# Mosquitto 启动入口：把选定方案的 ACL 复制到容器内可写位置并修正属主，再拉起 broker。
#
# 为什么不直接挂载 acl.<scheme> 到 /mosquitto/acl：
#   - /mosquitto/config 是整目录只读挂载，容器无法在其下建挂载点；
#   - 直接从版本库挂进去时文件属主是宿主机用户，mosquitto 2.0.20 告警
#     "owner is not mosquitto"，并说明后续版本将直接拒绝加载。
# 复制后仓库里的 ACL 仍是只读挂载，容器内那份改完即弃。
set -e

scheme=${MQTT_TOPIC_SCHEME:-grid}
case "$scheme" in
  grid | internal) ;;
  *)
    echo "MQTT_TOPIC_SCHEME=$scheme 不支持，只能是 grid 或 internal" >&2
    exit 1
    ;;
esac

cp "/mosquitto/config/acl.$scheme" /mosquitto/acl
chown mosquitto:mosquitto /mosquitto/acl
chmod 600 /mosquitto/acl

# 命名卷首次挂载时继承镜像内属主；这里再兜一次，失败不阻断启动。
if [ "$(id -u)" = '0' ]; then
  chown -R mosquitto:mosquitto /mosquitto/data 2>/dev/null || true
fi

exec mosquitto -c /mosquitto/config/mosquitto.conf