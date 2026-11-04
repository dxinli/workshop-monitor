# wecom-mock

企业微信机器人接收端模拟。承接 Node-RED 投递出来的告警卡片，按机器人消息的样子渲染、
留痕，并支持按时间倒序回查。

仿真件不是交付物，`sim/` 整体可删（`.claude/rules/sim.md`）。本目录不 import `internal/`，
报文结构按 `docs/04-接口规范.md` §11.5 自行定义。

## 组成

| 文件 | 作用 |
|---|---|
| `main.py` | 入口：开记录库、起 HTTP 服务 |
| `card.py` | 报文模型与卡片渲染（校验口径、级别表、时间格式） |
| `store.py` | 投递记录：按 `notifyId` 去重、按接收时间倒序查 |
| `api.py` | 三条路由与卡片页面 |
| `Dockerfile` | 容器化，供 compose `sim` profile 用（012 加） |
| `test_wecom_mock.py` | 渲染与去重的单元测试，不起服务 |

记录库用标准库 `sqlite3`，不引 ORM：`requirements.txt` 的三个版本在 `AGENTS.md`
里锁死，为这点持久化多一个依赖不划算。

## 运行

主形态是 **compose `sim` profile 的服务**（012 容器化）：

```bash
make up
docker compose -f deploy/docker-compose.yml --env-file deploy/versions.env --env-file .env --profile sim up -d wecom-mock   # 127.0.0.1:8081，记录库落卷
```

容器间走服务名 `wecom-mock:8081`，`.env.example` 的 `WECOM_WEBHOOK_URL` 就是它。
记录库挂在 `wecom-data` 卷上，容器重建后投递记录还在。

宿主机 venv 形态仍保留，只用于单条卡片的调试：

```bash
python3.12 -m venv .venv-sim
.venv-sim/bin/pip install -r sim/wecom-mock/requirements.txt pytest
make sim-wecom                 # 127.0.0.1:8081，记录库 var/wecom-mock.db
```

## 接口

宿主机侧只在 `127.0.0.1:8081` 上，不对外暴露。容器内监听 `0.0.0.0` 是为了能被
同网络的其它服务访问——判据看宿主机侧端口映射（`.claude/rules/sim.md` §部署）。

| 接口 | 方法 | 说明 |
|---|---|---|
| `/webhook` | POST | 接收卡片，回 `{"errcode":0,"errmsg":"ok","duplicate":…,"retryCount":…}` |
| `/api/v1/records?limit=N` | GET | 投递记录，**按接收时间倒序**，`limit` 1~500，默认 50 |
| `/api/v1/records` | DELETE | 清空记录，供验收重复执行 |
| `/` | GET | 卡片页面，企业微信机器人消息样式，5 秒自刷新 |

`duplicate` 与 `retryCount` 不是企业微信的字段，给出来只为让联调方一眼看出
「重复投递被去重了」还是「真的又发了一张新卡」。

## 三条容易被写错的口径

### 去重按 `notifyId`，且重复时只累加次数

`notifyId` 是幂等键。Node-RED 按 1 / 5 / 15 秒重试，同一 `notifyId` 会到 4 次
（`BR-41`），**重试不产生第二张卡**。但重复次数要计入 `retryCount`——
它是判断重试阶梯在不在工作的唯一依据。

重复时除 `retry_count` 外**任何字段都不改写**。重试若带了不同的触发值，
卡片必须还是首次那一份，否则显示的会是「最后一次重试的值」而不是真实触发值。

### 校验失败回 4xx 且不写记录

与 Node-RED「参数校验失败不写投递日志」同一口径（`tasks/011` 偏差 1）：
没有可信 `notifyId` 做主键，强写会产生无主记录。断在保存之前，不是写完回滚。

### 必填字段按上游实际产出定，不按文档示例定

`deploy/node-red/flows.json` 的「组装卡片」节点与 §11.5 的示例有三处不同：

| 字段 | 文档示例 | 上游实际 |
|---|---|---|
| `alarm.threshold` | 有 | **不产出** |
| `alarm.alarmId` | 有值 | 告警回查不到时为 `null` |
| `alarm.unit` / `alarm.reason` | 有值 | 未配置时为 `null` |

把这三个当必填，联调第一条就走不通。`threshold` 缺省时卡片不渲染阈值行——
不能出现「阈值 None」。

### 卡片按 `alarm.*` 结构化字段渲染，不复刻 `markdown.content`

真实企业微信只渲染 `markdown.content` 那段文本。这里改用结构化字段重新画，
因为 012 的验收判据是「卡片含车间、终端、测点、级别、触发值、发生时间、告警原因」——
按结构体画，缺哪个字段页面上就直接少一行，`markdown.content` 漏了哪项看不出来。

`markdown.content` 原样存进记录（`GET /api/v1/records` 可取），接真实通道时复用。
两边不一致时以 `alarm.*` 为准，页面上的值就是记录里的值。

## 为什么要容器化

019 交付的是宿主机 venv 形态（沿用 `modbus-slave-sim` 的做法）。但 Node-RED 在容器里，
容器内的 `localhost` 是容器自己，`.env` 指 `localhost:8081` 会连到不存在的端口上。
012 因此补了 `Dockerfile` 与 compose `sim` profile 服务，容器间走服务名直连，
开放项 O-19 就此关闭。**`modbus-slave-sim` 仍是宿主机 venv**——它要 pty 直连端点，
没有这个诉求。

单独发一条卡片的联调不必起全栈：

```bash
.venv-sim/bin/python sim/wecom-mock/main.py --db var/wecom-mock.db &
curl -X POST localhost:8081/webhook -H 'Content-Type: application/json' -d @card.json
curl -s 'localhost:8081/api/v1/records?limit=5' | python3 -m json.tool
open http://localhost:8081/
```

## 验收

```bash
.venv-sim/bin/python scripts/accept-019.py --evidence docs/evidence/019
```

脚本自己起一个临时实例（端口 8091、临时记录库），六条用例全走 HTTP，
不 import 被测代码。产出 `docs/evidence/019/acceptance.json`。

## 与 012 的边界

本组件只做「接住 + 渲染 + 留痕」。**灰度开关 `WECOM_ENABLED` 不在这里**——
它的落点是 Node-RED 的投递准备节点，属 `tasks/012-通知通道对接.md`。
重试也不在这里，重试在编排层（`BR-41`）。本组件唯一的退路是回 4xx，让上游按既有阶梯重试。