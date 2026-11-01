# Node-RED 自定义镜像
#
# 官方镜像不带任何 contrib 节点。这里装两个，版本都锁补丁号，
# 与 deploy/versions.env 的口径一致：
#   node-red-contrib-postgresql  写 notify_log（N-02）、读写告警（N-03/N-04）
#   @flowfuse/node-red-dashboard 写视图面板（N-04）
#
# 装进 /usr/src/node-red/node_modules 而不是 /data/node_modules：
# /data 由 compose 挂成版本库里的 deploy/node-red 目录，装在那里的依赖会被挂载盖掉。
# 官方镜像的 NODE_PATH 已经包含这个目录，扫得到。
FROM nodered/node-red:4.0.5

ARG NODE_RED_PG_NODE_VERSION=0.16.2
ARG NODE_RED_DASHBOARD_VERSION=1.34.1

USER root
RUN npm install --no-audit --no-fund --omit=dev --prefix /usr/src/node-red \
      "node-red-contrib-postgresql@${NODE_RED_PG_NODE_VERSION}" \
      "@flowfuse/node-red-dashboard@${NODE_RED_DASHBOARD_VERSION}"

USER node-red

# settings.js 与 flows.json 在挂载目录里，不进镜像：
# 交付物要能被 diff，打进镜像就看不见改了哪一行。