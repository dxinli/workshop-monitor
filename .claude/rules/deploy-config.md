---
paths:
  - "deploy/**"
  - "config/**"
---

# deploy 与 config

单机容器化，4 核 / 8 GB / 50 GB，镜像总体积 ≤ 3 GB。三组 profile：`sim`（模拟器）、`edge`（agent ×5，9101~9105）、`core`（Broker 1883 / adapter 9200 / DB 5432 / Node-RED 1880·1881 / 接收端 8081 / Grafana 3000）。

## Mosquitto

- MQTT 3.1.1，QoS 1
- 按终端独立账号，按终端隔离发布权限（终端只能发布自己的主题）
- 启用 LWT，adapter 据此判显式离线
- adapter 订阅端启用持久会话（`clean session = false`）
- **不做**字段转换、入库规则、规则计算

ACL 变更需重载配置，不支持热生效。演示期固定 5 终端，新增终端走脚本。

- 账号清单唯一来源是 `deploy/mosquitto/acl.<scheme>` 的 `user` 行，`scripts/gen-passwd.sh` 据此从 `.env` 生成口令文件
- `MQTT_TOPIC_SCHEME` 在 compose 里选 `acl.grid` / `acl.internal`，由 `deploy/mosquitto/entrypoint.sh` 复制到容器内 `/mosquitto/acl`
  - 不要直接把 ACL 挂到 `/mosquitto/acl`：`/mosquitto/config` 整目录只读，挂不进去；从版本库挂进去属主不对，2.0.20 会告警且后续版本拒绝加载
- 验收用 `make verify-broker`。越权发布只在 `-V mqttv5` 下返回 `Not authorized`，3.1.1 的退出码是 0

## TimescaleDB

### 建表脚本

- 位置 `deploy/timescaledb/init/`，按编号排序执行
- **必须幂等**：`CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`
- 权威定义是 `../docs/02-设计/03-数据库设计说明书.md` §3

adapter 内不得出现建表语句。要加表就改本目录并同步数据库设计文档。

### 保留策略

| 数据 | 形态 | 保留 |
|---|---|---|
| 配置域 | 关系表 | 长期 |
| 历史测量明细 | 时序超表 | 7 天 |
| 度量分钟聚合 | 连续聚合 | 30 天 |
| 实时值 | 关系表，每测点 1 行 | 仅最新 |
| 原始报文 | 时序超表 | 7 天 |
| 告警与确认 | 关系表 | 长期 |
| 通知记录、设备事件 | 关系表 | 30 天 |

30 天曲线走分钟聚合而非明细：140 点 × 5 秒 × 30 天约 7250 万行，单机 50 GB 承载不了明细。产品对 30 天的要求是"可查询趋势"。

### 视图

**视图内不写时间过滤条件。** 时间由面板传入，否则无法分区裁剪，30 天查询会全表扫描。

### 权限

| 账号 | 权限 |
|---|---|
| adapter | 配置域读，测量/报文/事件/告警域写 |
| Grafana 读视图 | 只读 |
| Dashboard 写视图 | 仅告警表读写 |
| 编排层（Node-RED） | 配置域与 `alarm` 读，`notify_log` 读写 |

编排层对 `alarm` 只读：卡片要带 `alarmId`，`notify_log.alarm_id` 是它的外键，
但通知失败不回滚告警，它不该有改告警状态的能力。

Grafana 不得授予写权限。写操作只从 Dashboard 一个出口出。

## Node-RED

流程：`N-01` 通知接收 `POST /api/notify`、`N-02` 投递与重试、`N-03` 告警确认 `POST /api/ack`、`N-04` 演示面板。

- **不做**阈值求值、去抖、告警状态判定
- **无状态**：不配置 flow context 持久化
- 重试 1 / 5 / 15 秒，末端记失败原因并终止，不无限重试
- 通知失败不回滚告警
- 参数校验失败时不写投递日志——没有可信 `notifyId` 做主键，会产生无主记录
- 失败信息不回传 adapter
- 不改写 `notifyId`

**参数校验失败不写日志**是反直觉但正确：这类错误属调用方缺陷，应在 adapter 日志暴露。

`flows.json` 必须在编辑器导出后提交，禁止只在界面修改。

官方镜像不带数据库节点，N-02 写 `notify_log` 靠 `node-red-contrib-postgresql`
（版本锁在 `deploy/versions.env`，镜像由 `deploy/node-red/Dockerfile` 构建）。
`deploy/node-red` 整目录挂到容器 `/data`，运行时产物已在 `.gitignore`——
`git status` 里只该出现 `flows.json` 与 `settings.js`。

4.0.5 只有一个 HTTP 监听口（`httpNodePort` 已移除），编辑器与 `/api/notify` 共用 1880。

## Grafana

- 只读账号，无任何写权限
- 面板 JSON `deploy/grafana/dashboards/`，数据源 `deploy/grafana/provisioning/`
- 刷新 5 秒，时区 `Asia/Shanghai`，时间范围变量 1h / 24h / 7d / 30d
- "最近一次正常值"必须同屏标注采集时间与质量
- 全部中文工业用语，**禁用配电术语**（遥测/遥信只出现在代码层）
- 校验错误、超量程等非正常值必须视觉可区分

## 配置

配置文件是唯一权威源，数据库是供界面与编排读取的投影。不一致时以文件为准并重新 `make seed`。

| 配置 | 文件 | 生效 |
|---|---|---|
| 终端与点表 | `config/terminals/ESN-ZX-000N.json` | 启动读取，改后重启 |
| 告警阈值 | `config/thresholds.json` | 启动载入，改后 60 秒内生效 |
| 主题模板 | `config/topics.json` | 重启 agent 与 adapter |
| 工况预设 | `config/scenes.json` | 不入库，模拟器启动读取 |
| 校验 schema | `config/schema/` | 与上表同步更新 |

主题字符串的唯一来源是 `config/topics.json`，代码里不出现 `/edge/` 字面量。
两套方案同形、仅前缀不同（`grid` 用 `/edge/`，`internal` 用 `/workshop/`），
`deploy/mosquitto/acl.<scheme>` 必须与它逐条一致——`make verify-config` 会比对。

校验与种子共用 `cmd/config-tool`，规则在 `internal/config`。改校验规则时同时改这里与
`docs/02-详细设计说明书.md` §6.2，不要只改代码。

### 校验规则

| 编号 | 规则 | 失败 |
|---|---|---|
| V-01 | 必填字段齐备 | 拒绝启动 |
| V-02 | 从站地址 1~247 且总线内唯一 | 拒绝启动 |
| V-03 | 测点编码设备内唯一，匹配 `^[a-z][a-z0-9_]{1,31}$` | 拒绝启动 |
| V-04 | 数据类型与寄存器宽度匹配 | 拒绝启动 |
| V-05 | 缩放系数非零且量程下限 < 上限 | 拒绝启动 |
| V-06 | 块内 `地址 + 宽度` 不重叠且不越界 | 拒绝启动 |
| V-07 | 寄存器块覆盖全部测点，无孤立点 | 拒绝启动 |
| V-08 | 四遥类型属遥测或遥信 | 拒绝启动 |
| V-09 | 单从站寄存器数 ≤ 125 | 拒绝启动 |
| V-10 | 5 × 2 × 14 = 140 | 告警不拒绝 |
| V-11 | 阈值类型属枚举、级别 1~4 | 拒绝启动 |
| V-12 | 配置版本变更记一条配置加载事件 | 仅记录 |

校验失败必须输出**具体字段与原始值**，不允许只输出"配置错误"。

### 生效边界

| 变更 | 生效 | 重启 |
|---|---|---|
| 阈值数值 / 级别 | 60 秒重载，原子替换 | 否 |
| 工况预设 | 下次场景切换 | 否 |
| 点表地址 / 数量 / 字节序 | 重启对应终端容器 | 是 |
| 串口参数 | 重启 | 是 |
| 主题命名方案 | 重启 agent 与 adapter | 是 |

**只对纯数值类配置做热加载。** 点表热加载会出现"半个周期用旧块、半个周期用新块"的错值，这类问题在验收现场最难解释，所以强制重启。

## 交付物纳管

面板 JSON、`flows.json`、数据源配置、建表脚本、配置模板、`versions.env` 全部进仓库。

界面里改完不导出，现场重装就丢；建表脚本不幂等，重复执行失败会让现场卡在部署第 5 步。

## 自检

- [ ] 镜像标签全部来自 `deploy/versions.env`，无 `latest`
- [ ] 建表脚本幂等，重复执行不报错
- [ ] 视图无内嵌时间过滤
- [ ] Grafana 账号只读
- [ ] `flows.json` 与面板 JSON 已导出提交
- [ ] Node-RED 无 context 持久化、无阈值求值
- [ ] 配置校验失败输出具体字段
- [ ] 点表类配置未做热加载
- [ ] 界面无配电术语，最近正常值带时间与质量