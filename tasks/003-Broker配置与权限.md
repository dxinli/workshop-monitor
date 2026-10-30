# 003 Broker 配置与权限

> 计划任务：M3（复杂度 S）　状态：待办

## 要建什么

Mosquitto 的配置文件、ACL、账号口令文件，以及生成口令的脚本。要求按终端隔离发布权限。

## 验收

- [x] 5 个终端账号，各自只能发布自己的上行主题
- [x] 用终端 1 账号发布终端 2 的主题被拒（broker 返回 not authorized）
- [x] 接入服务账号可订阅全部上行主题
- [x] LWT 已启用，终端异常断开时状态主题收到 `OFFLINE`
- [x] 口令文件不入版本库，由 `scripts/gen-passwd.sh` 从 `.env` 生成
- [x] 两套命名方案都能配：`MQTT_TOPIC_SCHEME=grid` 与 `internal`

```bash
mosquitto_pub -h localhost -p 1883 -u ESN-ZX-0001 -P "$PWD1" \
  -t '/edge/ESN-ZX-0002/data/up' -m '{}'
```

注意 `mosquitto_pub` 在 3.1.1 与 v5 下对越权发布的返回值不同：**v5 才回 `Not authorized` 且退出码非 0**，
3.1.1 只把 CONNACK 的拒绝留在日志里，退出码仍是 0。验收脚本统一用 `-V mqttv5`。

## 阻塞

无，可立即开始。与 001 并行。

## 加载

- rules：`.claude/rules/deploy-config.md`（Mosquitto 段）
- 主题契约：`../../docs/02-设计/04-接口规范.md` §5

## 偏差

1. **主题契约取不到，按本任务单样例补齐并经确认。** `../../docs/02-设计/04-接口规范.md` 不在仓库内，
   仓库只有 `docs/03-数据库设计说明书.md`；主题方案原只有 `/edge/ESN-ZX-0002/data/up` 一个样例。
   两套方案定为同形、仅前缀不同，已确认后实现：

   | 方案 | data | event | status（LWT） |
   |---|---|---|---|
   | `grid` | `/edge/{esn}/data/up` | `/edge/{esn}/event/up` | `/edge/{esn}/status/up` |
   | `internal` | `/workshop/{esn}/data/up` | `/workshop/{esn}/event/up` | `/workshop/{esn}/status/up` |

   该表属于冻结线 A，006 / 008 的主题模板按它实现；改这里要同步改那两份任务单。

2. **终端只给 `write`，不给 `readwrite`。** 终端没有读回自己主题的用例；LWT 由 broker 代发，
   不查客户端的写权限，`status` 主题仍需显式授权（已授权）。

3. **接入服务账号名为 `workshop_adapter`**，与库角色 `workshop_owner` / `workshop_adapter` 同一套前缀。

4. **ACL 不直接挂到 `/mosquitto/acl`。** `/mosquitto/config` 是整目录只读挂载，容器无法在其下建挂载点；
   从版本库直接挂进去时文件属主是宿主机用户，mosquitto 2.0.20 会告警
   `owner is not mosquitto` 并声明后续版本将拒绝加载。改为 `deploy/mosquitto/entrypoint.sh`
   启动时按 `MQTT_TOPIC_SCHEME` 复制 `acl.<scheme>` 到容器内 `/mosquitto/acl` 并 `chown`、`chmod 600`。
   代价是 compose 里 mosquitto 以 root 起容器（broker 进程仍降权到 `mosquitto`）。

5. **账号清单的唯一来源是 `acl.<scheme>` 的 `user` 行。** `scripts/gen-passwd.sh` 从它取账号，
   口令变量名按账号推导（`ESN-ZX-0001` → `MQTT_ESN_ZX_0001_PASSWORD`）。缺任一变量即报出变量名与账号并退出。
   `.env.example` 增加 6 个变量。`make up` 先跑 `make passwd`，避免新克隆缺口令文件导致 broker 起不来。

6. **新增持久化卷 `mosquitto-data`。** `persistence true` 是 adapter 持久会话（clean session = false）
   在 broker 重启后不丢离线消息的前提；没有卷时容器重建即丢。

7. **新增 `scripts/verify-broker.sh` 与 `make verify-broker`。** 逐条执行本节验收项，两套方案各跑一遍：
   起临时容器（不动全栈）、跑完即删。`make check` 不含它，因为需要 docker 与 `.env` 口令。
