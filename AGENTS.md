# 车间设备状态与能耗监测系统

车间设备数据采集与告警平台。5 台边缘终端经 RS485 轮询 50 台设备的 140 个测点，MQTT 上报平台，平台入库、越限判定、告警通知、界面展示。

## Tech Stack

- 交付件：Go **1.27.3**，`CGO_ENABLED=0` 静态编译，module `workshop-monitor`
- 仿真件：Python **3.12.7**（仅 `sim/`，不进交付镜像）
- 消息代理：Mosquitto **2.0.20**（MQTT 3.1.1 / QoS 1）
- 存储：TimescaleDB **2.17.2-pg16**
- 编排：Node-RED **4.0.5**；可视化：Grafana **11.3.0**；虚拟串口：socat **1.8.0.0**

Go 依赖：`eclipse/paho.mqtt.golang` v1.4.3、`go.etcd.io/bbolt` v1.3.11、`jackc/pgx/v5` v5.7.1、`rs/zerolog` v1.33.0、`goburrow/serial` v0.1.0
Python 依赖：`pymodbus` 3.7.4、`fastapi` 0.115.5、`uvicorn` 0.32.1、`pydantic` 2.9.2

版本一律锁补丁号，镜像标签唯一来源 `deploy/versions.env`。出现 `latest` 或 `^`/`~` 范围视为缺陷。

## Commands

```bash
make build           # 构建二进制与镜像
make check           # 提交前总检查：verify + fmt + lint + test
make hooks           # 安装 git 钩子到 .githooks（首次克隆后跑一次）
make verify          # 校验依赖版本与镜像标签一致
make test            # 单元测试
make vet             # 官方 go vet
make lint            # golangci-lint（.golangci.yml，七项）
make lint-install    # 安装 golangci-lint
make fmt             # 格式化
make up              # 启动全栈
make down            # 停止全栈
make seed            # 导入 deploy/timescaledb/init/02_seed.sql
make seed-sql        # 从 config/ 重新生成 02_seed.sql（改配置后跑）
make verify-config   # 配置校验 V-01~V-11，逐条造坏配置验证报错内容
make preflight       # 部署后 12 项自检
make sim-pty         # 建 10 对虚拟串口，端点表写到 .pty/
make sim-test        # 仿真件单元测试（需 .venv-sim）
make sim-up          # 起 Modbus 从站模拟器
make logs            # 跟随全部容器日志

# 单组件调试
go run ./cmd/config-tool verify            # 等价于 make verify-config 的校验部分
go run ./cmd/config-tool seed -out /tmp/02_seed.sql
go run ./cmd/edge-agent    -config config/terminals/ESN-ZX-0001.json
go run ./cmd/edge-agent    -config config/terminals/ESN-ZX-0001.json -ports .pty/agent-ports.json   # 接模拟器
go run ./cmd/proto-adapter -config config/adapter.toml
docker compose --profile sim up modbus-slave-sim
```

## Structure

```text
CONTEXT.md              术语表，只放领域语言，不放实现
CLAUDE.md               本文件
cmd/edge-agent/         入口：参数解析 + 装配，不含业务逻辑
cmd/proto-adapter/      入口：同上
cmd/config-tool/        入口：配置校验与种子生成（verify / seed）
internal/agent/         poller 轮询 · modbus 帧 · codec 换算 · mqttsink 上报 · cache 队列 · health
internal/adapter/ingest/     router · codec_* · idem 幂等 · sink_pg 投影 · termstate 状态
internal/adapter/evaluate/   queue · threshold · debounce · statemachine · notify · alarm_repo
internal/adapter/state/      active 活动告警集合 · counters 去抖计数
internal/config/        点表 / 阈值 / 主题模板的载入、V-01~V-11 校验、种子 SQL 生成
internal/model/         与接口规范一一对应的结构体，无方法无校验
internal/pkglog/        统一日志字段
sim/                    modbus-slave-sim · bypass-sim · wecom-mock（Python，不进交付镜像）
tasks/                  任务单，一次会话一个文件
config/                 terminals/ · thresholds.json · topics.json · schema/
deploy/                 docker-compose.yml · versions.env · mosquitto/ · timescaledb/init/ · node-red/ · grafana/
scripts/                setup-pty.sh · preflight.sh · verify-config.sh · render-topics.sh
.githooks/              git 钩子：commit-msg · pre-push（\make hooks` 安装）`
.github/workflows/      ci.yml（PR 与主干）· release.yml（v* tag）· integration.yml（手动）
```

## Conventions

术语一律用 `CONTEXT.md` 的词，并按它的 `_Avoid_` 清单避坑。代码标识映射固定：

```text
terminal / terminalId    bus / busId    slaveAddr
point / pointCode / pointId
collectTime    backfill    debounce    dedupeKey
```

命名：包小写无下划线无复数；文件小写下划线；类型大驼峰；单方法接口加 `-er`；函数小驼峰动词开头；配置键大写下划线。JSON 字段小驼峰，数据库列小写下划线。

线上报文头用 `esn`，代码用 `terminalId`，库列用 `terminal_id`。三者不统一，统一会破冻结线 A。

标识拼接顺序固定，不得调整：

```text
设备 = terminalId + busId + slaveAddr
测点 = terminalId + busId + slaveAddr + pointCode
去重键 = terminalId|pointId|alarmType
通知标识 = NT-{terminalId}-{pointId}-{occurTime}
```

## Must Follow

- `cmd/` 只做参数解析、依赖装配、启动调用。
- `internal/agent` 与 `internal/adapter` 不得互相 import。跨侧共享代码只放 `internal/model`、`internal/pkglog`；
  `internal/config` 是两侧都要用的配置载入与校验，允许被 `internal/agent`、`internal/adapter` 与 `cmd/` 引用，
  但它不 import 任何业务包，也不做 IO 以外的判定。
- `internal/adapter/ingest` 与 `internal/adapter/evaluate` 不得互相 import，唯一连接是有界 channel。
- `internal/model` 只放结构体，不含方法。
- `sim/` 不 import `internal/`；`cmd/` 不 import `sim/`。
- 判定逻辑（阈值求值、去抖、告警状态机）只在 `internal/adapter/evaluate`。
- 建表语句只在 `deploy/timescaledb/init/`，且必须幂等。
- 单函数 ≤ 50 行。
- **序号在终端内单调递增，重启后从队列最大序号续接，不归零。** 归零会让整段补传被幂等判成重复。
- **实时值只被更晚 `collectTime` 覆盖**；条件写在 SQL `WHERE` 里，不在应用层先查后写（并发下必失效）。
- **质量非 `GOOD` 不参与告警求值**，在判定入口就返回。
- 外部资源（DB、MQTT、串口、时钟）从构造函数传入，**不在模块内部创建**。内部 new 出来的依赖无法在 seam 上替换，也就测不了。
- 没有第二个实现就不要抽接口。一个适配器是假 seam，两个才是真 seam。
- 日志固定字段 `ts`/`level`/`svc`/`inst`/`msg`，涉及终端时带 `esn`，涉及测点时带 `pointId`/`seq`。
- 错误带上下文：`fmt.Errorf("读取总线 %s 从站 %d: %w", busId, addr, err)`。
- 重试必须有次数与间隔：Modbus 读超时 2 次、CRC 1 次、DB 写 3 次（立即/1s/5s）、通知 4 次（立即/1s/5s/15s）。

上面四条有机器兜底，写错了 `make lint` 会拦：import 边界（`depguard`，含 `internal/config` 不得反向依赖业务包）、单函数 ≤ 50 行（`funlen`）、错误吞掉（`errcheck`）、错误不带 `%w`（`errorlint`）。其余各条仍靠自觉。要新增机器检查，先在 `.golangci.yml` 加规则并在这里写出对应条目，两边不要脱节。

## Never Do

- 不要吞错误：空 `catch`、`_ = err`、只打日志不返回。
- 不要让采集失败沿用上一次值或填 0 —— 返回空值 + 真实质量码。
- 不要先写业务表再查重 —— 幂等判定在写任何业务表之前。
- 不要用接收时间做业务时间轴 —— 曲线、告警判定、排序、补传落位一律用 `collectTime`。
- 不要静默丢弃缓存 —— 计数，每满 1000 条上报事件。
- 不要在 Mosquitto / Node-RED / 数据库触发器里做业务判定。
- 不要硬编码主题字符串 —— 走模板配置 + `MQTT_TOPIC_SCHEME`。
- 不要硬编码连接串、口令、阈值、点表。
- 不要在采集循环内打印每点成功日志 —— 140 点 × 5 秒会淹没日志，改周期聚合。
- 不要打印完整报文内容、连接串、口令。
- 不要无限重试。
- 不要给 Grafana 账号写权限。
- 不要在视图里内嵌时间过滤条件 —— 时间由面板传入，否则无法分区裁剪。
- 不要只在界面改面板或流程 —— 必须导出 JSON 提交。
- 不要在任务单与实现冲突时先按能跑的方式写 —— 先停车确认哪边错，改文档再改代码。回头补文档永远不会发生。

## Ask First

动手前先出方案等我确认：

- 改冻结线 A 契约：Modbus 点表、上行报文 Payload、主题命名
- 改表结构、加表、改索引
- 新增第三方依赖
- 跨组件重命名或移动文件
- 改 `deploy/versions.env` 里的版本
- 新增 seam、新增共享包、改动 `CONTEXT.md` 的术语定义

自动执行：格式化、lint、单测、按既有 Spec 实现、补测试。

## Workflow

任务清单在 `tasks/00-INDEX.md`，18 份任务单对应计划 M1~M17。

1. 读 `tasks/00-INDEX.md`，按依赖挑一个状态为「待办」且阻塞已清的
2. 读该任务单的 `## 加载` 段，只读它列出的规则来源与 rules
3. 按照 [Git](#Git) Flow 的方式管理分支
4. 按下面 Testing 段在 seam 上做红-绿，**一次会话只做一份任务单**
5. 按 Done Check 两轴各过一遍，把 INDEX 里该行状态改成「已完成」
6. `make check` 通过后提交到当前 `feat/*` 分支，开 PR 合入 `main`

没有任务单覆盖的改动，先建任务单再动手。新建任务单读 `tasks/_TEMPLATE.md`，不要照抄已有任务单——它们的「加载」段是按各自内容填的。

## Git

**Message**：`<类型>(<模块>): <摘要>`，类型 `feat`/`fix`/`chore`，模块 `agent`/`adapter`/`sim`/`deploy`/`config`/`docs`。写错提交不进去——`commit-msg` 钩子会拦并给出示例。写在这里只为省你一次往返，不必逐字记。

**Flow**：使用 Git Flow 删减版，删除 develop 分支

- **不直接提交到 `main`。** main 的语义是「已交付」，只有 release / hotfix 能合进去。改代码一律在 `feat/*` 上。
- **开工前先看当前分支。** 在 `main` 上就 `git switch -c feat/<模块>-<英文简述>` 再动手，不要在集成分支上写代码。
- **分支名不擅自改。** 已存在的不合规分支留着，新建的按 `<模块>-<英文简述>`。看不准就先问。

## Testing

测试只写在下面四个 seam 上，不写在内部实现上。这是已确认的 seam 清单，不要另开：

| Seam         | 位置                                         | 验什么                                         |
| ------------ | -------------------------------------------- | ---------------------------------------------- |
| 换算         | `internal/agent/codec.go` 的 `toEngineering` | 缩放、字节序、符号扩展、量程、质量码           |
| 去抖与状态机 | `internal/adapter/evaluate` 的求值入口       | N 次触发、N 次恢复、开关量不去抖、未恢复不重复 |
| 幂等         | `internal/adapter/ingest/idem.go`            | 首次 true、重复 false、冲突时不写业务表        |
| 队列裁剪     | `internal/agent/cache.go` 的 `trim`          | 三上限先到、FIFO、丢弃计数                     |

签名按实现需要定，**seam 位置不能动**。想把测试写到 seam 里面，说明模块形状不对。

循环规则：

- **红先绿后**。先写失败测试，再写刚好让它通过的实现。不预判后面的测试。
- **一个 seam → 一个测试 → 一个实现**，不要先批量写测试再批量写实现。横向切片测的是想象中的行为。
- 测试只走公开接口。不 mock 内部协作者，不测私有函数，不查数据库验证（要验就经接口读回）。
- 期望值是独立来源的字面量或算例，**不能照抄实现的算法**。照抄的断言永远通过，等于没测。
- 单测不依赖数据库与网络：DB 从构造函数注入，测试用内存实现。
- **修 bug 先写能复现它的失败测试**，再改代码。回归测试留在仓库，别修完就删。
- 重构不属于红绿循环，留给评审。

## Done Check

做完不要说"已完成"。按两个轴分别过，**不要把两轴合成一个结论**：

**Standards —— 是否符合本仓规范**

- [ ] `make check` 通过
- [ ] 在 `feat/*` 分支上，未直接提交到 `main`
- [ ] 失败路径不产生 0 值、不沿用旧值
- [ ] 新增重试已进重试次数表
- [ ] 改动涉及的 seam 有测试，且未 mock 内部协作者
- [ ] 无新增硬编码、无静默吞错
- [ ] 相关 `.claude/rules/` 末尾自检项全过

**Spec —— 是否只做了任务要求的事**

- [ ] 任务单的验收项逐条达成
- [ ] 没有实现 Spec 没要求的扩展点（预判式抽象一律删掉）
- [ ] 与 `Must Follow` 里的跨任务铁律无冲突（序号、时间轴、实时值、质量门槛）
- [ ] 与既有文档的偏差已写进任务单的「偏差」段

两轴的结论分别报，允许"Standards 过、Spec 不过"。合成一句"已完成"会把其中一个轴盖掉。
