---
paths:
  - "internal/adapter/ingest/**"
---

# ingest 接收段

订阅上行报文 → 主题解析 → 解码 → 归一化 → 幂等 → 留存原文 → 投影历史/实时值/终端状态 → 投递判定队列。

不 import `internal/adapter/evaluate`。唯一连接是有界 channel。

## 处理链顺序固定

```text
① 主题解析    按模板提取 terminalId 与消息类型   ← 模板可配，不硬编码
② 报文解码    Codec 按配置选择（客户规范 / 内部标准）
③ 归一化      四遥类型、单位、质量码、三时间
④ 幂等判定    terminalId + seq                    ← 必须在写业务表之前
⑤ 原文留存    不论解析成功与否
⑥ 投影        历史测量、实时值、终端状态
⑦ 投递判定    入库成功后推入有界 channel
```

## 幂等

```text
ok = INSERT INTO message_log(...) ON CONFLICT (terminal_id, seq) DO NOTHING
if !ok: return DUPLICATE                     # 不写历史、实时值、事件

BEGIN TX
    raw_message.insert(...)
    if parseStatus != OK: COMMIT; return      # 解析失败只留原文
    measurement.bulkInsert(rows)              # 单事务批量
    upsertRealtime(rows)
    updateTerminalState(...)
COMMIT
publishToEvaluate(rows)                       # 事务外投递
```

幂等必须在写任何业务表之前。先写后去重会产生脏数据，删除脏数据又会影响已聚合的分钟视图。

`message_log` 主键 `(terminal_id, seq)`。`measurement` 主键 `(collect_time, terminal_id, point_id)` 是第二道防线。

报文头 `esn` 与 Topic 中的 `{esn}` 不一致时整条判 `IDENTITY_MISMATCH`，仍留存原文。

## 实时值合并

```sql
INSERT INTO realtime_value(...) VALUES(...)
ON CONFLICT (terminal_id, point_id) DO UPDATE
SET value = EXCLUDED.value, quality = EXCLUDED.quality,
    collect_time = EXCLUDED.collect_time, seq = EXCLUDED.seq, updated_at = now()
WHERE EXCLUDED.collect_time > realtime_value.collect_time
   OR (EXCLUDED.collect_time = realtime_value.collect_time
       AND EXCLUDED.seq > realtime_value.seq);
```

条件必须写在 SQL 的 `WHERE` 里。改成应用层"先查再写"在并发下必然失效。

调用前已过滤补传行——补传旧数据不进实时值通道。

## 终端状态三源合成

```text
优先级：显式离线（LWT / 优雅退出）> 数据超时 > 有非补传数据即在线

onStatus: status==OFFLINE ? setState(OFFLINE, reason) : setState(ONLINE, CONNECTED)
onData:   lastData = collectTime; setState(ONLINE, DATA_RECEIVED)
巡检:     if ONLINE and now-lastData > 3 * interval:
              setState(OFFLINE, DATA_TIMEOUT)
```

`lastData` 只按非补传数据推进。补传数据不得把已超时的终端刷回在线——补传时断连期间数据批量到达，参与状态推进会抹掉离线事件。

## 错误处理

| 场景 | 处理 |
|---|---|
| 报文解析失败 | 留原文 + 失败原因，不写历史 |
| 数据库写入失败 | 重试 3 次（立即 / 1s / 5s），仍失败记服务健康异常 |
| 判定队列满 | 丢弃并计数，记服务健康；入库不受影响 |
| 幂等冲突 | 计数重复，直接返回 |

数据库重试不能阻塞接收协程，也不能阻塞判定侧。

## 自检

- [ ] 幂等在写业务表之前，`message_log` 有 `(terminal_id, seq)` 主键
- [ ] 解析失败只留原文，不写历史
- [ ] 实时值条件在 SQL `WHERE`，非应用层先查后写
- [ ] 补传行不进实时值通道
- [ ] `lastData` 不被补传数据推进
- [ ] 无 `internal/adapter/evaluate` import
- [ ] 主题字符串来自配置模板，代码里无字面量