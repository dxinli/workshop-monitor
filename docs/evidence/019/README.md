# 019 通知接收端 wecom-mock · 验收证据

## 结论

6 条用例全部通过。`acceptance.json` 由 `scripts/accept-019.py` 生成，命令见任务单。

## 链路

`curl / Node-RED` → `HTTP POST 127.0.0.1:8081/webhook` → 校验 → 按 `notifyId` 去重 →
SQLite 记录 → `/api/v1/records` 与 `/` 页面可读。

验收脚本自己起一个临时实例（端口 8091、临时记录库），**不 import 被测代码**——
能被 `curl` 打到的东西才叫「可访问的接收地址」。

## 逐条证据

| 用例 | 实测 |
|---|---|
| 完整报文回 200 且可查 | 记录 `NT-ESN-ZX-0001-A.1.motor_temp-1793500200123`，状态 `SENT`，九项字段逐项一致 |
| 同一 `notifyId` 重复投递不产生第二条记录 | 投递 4 次（首次 + 1/5/15 秒重试）仍是 1 条记录，`retryCount=3` |
| 校验失败回 4xx 且不写记录 | 非法 JSON→422；`alarm` 缺 `workshop`→422；缺 `notifyId`→422；`level=9` 越界→422；记录数不变 |
| 记录倒序、`limit` 生效 | 倒序 `[NT-order-2, NT-order-1, NT-order-0]`；`total=3` 时 `limit=2` 返回 2 条 |
| 卡片页渲染七项字段 | 车间 / 终端 / 设备 / 测点 / 级别 / 触发值 / 发生时间 / 告警原因 八项全部出现 |
| 补传标灰、缺 `threshold` 不渲染阈值 | `is-backfill` + 「历史补传」；页面无「阈值」行、无 `None` |

## 与真实上游的联调（人工复核过一次）

没有停在「按文档示例造报文」上。报文的来源是**容器里真实的 Node-RED 函数节点**：

```bash
docker exec deploy-node-red-1 sh -c 'cat /data/flows.json'   # 确认容器内 flows 与仓库一致
docker cp …:/tmp/card-fn.js                                   # 「组装卡片」节点的 func
docker exec deploy-node-red-1 node /tmp/run-card.js           # 在 Node 20 里跑该函数
```

产出与 §11.5 示例的一处关键差异：**`alarm` 里没有 `threshold`**。

```json
{"notifyId":"NT-ESN-ZX-0001-A.1.motor_temp-1793500200123", …,
 "alarm":{"alarmId":"AL-0000123","level":3,"levelText":"严重","workshop":"一号车间",
  "terminalId":"ESN-ZX-0001","deviceText":"电机状态监测仪 · 总线A · 从站1",
  "pointText":"电机绕组温度","alarmType":"HIGH_HIGH_LIMIT","alarmTypeText":"高高限",
  "triggerValue":108,"unit":"℃","occurTime":1793500200123,
  "reason":"连续 3 次超过高高限 105 ℃","backfill":false}}
```

把这份真实报文 `POST` 给接收端：200，记录写入，页面八项齐全、无阈值行。
同一份报文从 `deploy-node-red-1` 容器内再发一次（`http://host.docker.internal:8081/webhook`）：
`{"duplicate":true,"retryCount":1}`，记录数不变。

顺带验证了两件事：

1. **容器可达性**——`host.docker.internal` 在 `deploy-node-red-1` 里可解析、可连通，
   注册为 O-18（`.env.example` 仍是 `localhost:8081`，改与不改由 012 一次定齐）。
2. **去重跨进程有效**——第二次投递来自另一个网络命名空间，仍命中同一条记录。

## 三处容易误判的细节

1. **`threshold` 不是必填。** 文档示例里有、上游不产出。按示例写必填，联调第一条就 422。
2. **重复投递只累加次数，不改写其余字段。** 否则重试带的不同触发值会覆盖真实触发值，
   卡片显示的就是「最后一次重试的值」。接口规范只写了「不产生重复卡片」，没写其余字段。
3. **校验失败不写记录**，与 Node-RED「参数校验失败不写投递日志」同一口径
   （`tasks/011` 偏差 1）：没有可信 `notifyId` 做主键，会产生无主记录。