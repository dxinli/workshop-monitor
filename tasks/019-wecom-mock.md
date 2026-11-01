# 019 通知接收端 wecom-mock

> 计划任务：M11 前置（复杂度 S）　状态：已完成
> 交付形态是宿主机 venv，不接 Compose，见「偏差」。**012 已把它改为 Compose `sim`
> profile 服务，现行形态见 `tasks/012-通知通道对接.md` 与 `sim/wecom-mock/README.md`。**

## 要建什么

`sim/wecom-mock`：充当 `WECOM_WEBHOOK_URL` 指向的接收端，把 011 投递出来的告警卡片
接住、按企业微信机器人消息的样子渲染、留痕，并支持按时间倒序回查。

一条通知从入口穿到可观察结果：`POST /webhook` → 校验 → 按 `notifyId` 去重 → 落记录 →
页面与查询接口可读。中途任何一步失败都要有确定的结果码，而不是静默吞掉。

012 的六条验收里，「接收端渲染 7 个字段」「通道不可达时的失败记录」「灰度开关」三条
拿真实企业微信都验不了（O-03 无凭据），本组件是它们的落脚点。

不同时做：不实现 `WECOM_ENABLED` 灰度开关（落点在 Node-RED，属 012），
不实现重试（重试在编排层，`BR-41`），不外发任何请求。

## 验收

- [x] `POST /webhook` 收到 §11.5 的完整报文回 200，记录可查
- [x] 同一 `notifyId` 重复投递不产生第二条记录（重试不重复出卡），且重复次数被计入
- [x] 报文缺必填字段 / 非法 JSON 回 4xx，且**不写投递记录**
- [x] `GET /api/v1/records?limit=N` 按接收时间**倒序**返回
- [x] `GET /` 渲染企业微信卡片，含车间、终端、设备、测点、级别、触发值、发生时间、告警原因
- [x] `alarm.backfill=true` 的卡片标灰并带「历史补传」标识
- [x] 报文里**没有 `threshold`** 时卡片照常渲染（`flows.json` 的「组装卡片」不产出该字段）
- [x] `make sim-test` 通过，新增用例只测纯逻辑，不起服务
- [x] `scripts/accept-019.py --evidence docs/evidence/019` 六条用例全过

验收命令：

```bash
python3.12 -m venv .venv-sim
.venv-sim/bin/pip install -r sim/modbus-slave-sim/requirements.txt \
  -r sim/wecom-mock/requirements.txt pytest==8.3.3
make sim-test

make sim-wecom &
.venv-sim/bin/python scripts/accept-019.py --evidence docs/evidence/019
```

`accept-019.py` 自己起临时实例（端口 8091、临时记录库），**不依赖常驻的 8081 实例**，
先起 `make sim-wecom` 只是为了让 `curl` 联调方便。

## 阻塞

无，可立即开始。它自己成为 012 的阻塞项。

## 加载

- 报文契约：`../docs/04-接口规范.md` §11.5（通知报文）
- 卡片必需字段：`.claude/rules/sim.md`「通知接收端」段
- 记录字段参照：`../deploy/timescaledb/init/01_schema.sql` 的 `notify_log`（§4.3）
- 上游实际产出：`../deploy/node-red/flows.json` 的「组装卡片」节点——**照它的实际字段写，
  不照文档示例写**，文档示例多一个 `threshold`
- rules：`.claude/rules/sim.md`

## 偏差

1. **必填字段按上游实际产出定，不按文档示例定。** `flows.json` 的「组装卡片」节点与
   §11.5 的示例有三处不同：不产出 `alarm.threshold`；告警回查不到时 `alarmId` 为 `null`；
   `unit` / `reason` 未配置时为 `null`。把这三个当必填，联调第一条就 422。
   实现里只有「渲染必需」的九项是必填，其余允许缺省。已用 `deploy/node-red` 容器里
   真实的函数节点跑出报文投稿验证，见 `docs/evidence/019/README.md`。

2. **记录库用标准库 `sqlite3`，不新增第三方依赖。** `fastapi` / `uvicorn` / `pydantic`
   三个版本与 `sim/modbus-slave-sim/requirements.txt` 一致，未改 `AGENTS.md` 的依赖清单。
   为这点持久化引一个 ORM 不划算，而 `requirements.txt` 是锁补丁号的。

3. **不接 Compose `sim` profile，仍是宿主机 venv 形态。** 与 004（O-13）同一处理：
   不伪造未验证的 Compose 定义。直接后果：Node-RED 在容器里，`localhost:8081` 指容器自身，
   容器内联调要用 `http://host.docker.internal:8081/webhook`。已在容器里实测这条路径可达
   （见证据）。**`.env.example` 的 `WECOM_WEBHOOK_URL` 仍是 `localhost:8081` 没改**——
   改它会破坏宿主机侧的 `scripts/verify-notify.sh` 口径，而 012 才决定最终指向。
   这一点登记为 O-19（原 O-18，与 014 撞号后改号）。
   **后续**：012 已补 `Dockerfile` 与 compose `sim` profile 服务，`.env.example` 改指
   `http://wecom-mock:8081/webhook`，O-19 关闭。本条描述的是 019 当时的形态，不改。

4. **`make check` 与 CI 的 pytest 目标从 `sim/modbus-slave-sim` 扩到 `sim`。**
   两个仿真件各自有测试，只跑一个会让另一个的用例永远不执行。CI 的装依赖命令同时列出
   两份 `requirements.txt`。`make sim-test` 同理。

5. **重复投递除 `retry_count` 外不改写任何字段。** 重试若带了不同触发值，
   卡片必须还是首次那一份，否则显示的是「最后一次重试的值」而不是真实触发值。
   接口规范只写了「重试不产生重复卡片」，没写重复时其余字段怎么办，这里按幂等键的
   常规语义定：首写者胜。

6. **`GET /api/v1/records` 额外回 `total` 与 `duplicate`/`retryCount`。**
   企业微信的响应体只有 `errcode`/`errmsg`。多出的字段只为联调方一眼看出
   「被去重了」还是「真的又发了一张新卡」，不影响按 `errcode == 0` 判成功。