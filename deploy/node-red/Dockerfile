# Node-RED 自定义镜像
#
# 官方镜像不带任何数据库节点，而 N-02 要写 monitor.notify_log。
# 这里只装一个 contrib 包，版本锁补丁号，与 deploy/versions.env 的口径一致。
#
# 装进 /usr/src/node-red/node_modules 而不是 /data/node_modules：
# /data 由 compose 挂成版本库里的 deploy/node-red 目录，装在那里的依赖会被挂载盖掉。
# 官方镜像的 NODE_PATH 已经包含这个目录，扫得到。
FROM nodered/node-red:4.0.5

ARG NODE_RED_PG_NODE_VERSION=0.16.2

USER root
RUN npm install --no-audit --no-fund --omit=dev --prefix /usr/src/node-red \
      "node-red-contrib-postgresql@${NODE_RED_PG_NODE_VERSION}"

USER node-red

# settings.js 与 flows.json 在挂载目录里，不进镜像：
# 交付物要能被 diff，打进镜像就看不见改了哪一行。