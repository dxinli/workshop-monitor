---
paths:
  - "internal/adapter/evaluate/**"
  - "internal/adapter/state/**"
---

# evaluate 判定段

消费有界 channel → 阈值求值 → 去抖 → 告警状态机 → 通知请求。

不做：通知投递（重试责任在编排层）、定义表结构、执行告警确认（那是 Dashboard 的写操作）。不 import `internal/adapter/ingest`。

## 单消费者，单写者

判定协程必须唯一。多消费者会让 `counters` 与 `active` 变成多写者，必须加锁，且顺序错乱会让去抖计数跳跃。

```text
# 接收侧（ingest）：入库成功后投递，绝不等待
select {
case queue <- record:                        # 有界 channel，容量 4096
default: metrics.dropCount++; log.warn(...)  # 满则丢弃并计数
}

# 判定侧：单协程串行
for e := range queue: evaluate(e)
```

## 求值

```text
evaluate(e):
    if e.quality != GOOD: return                    # 坏点不参与
    ths = snapshot.thresholdsFor(e.pointId)         # 测点 > 总线 > 全局
    for th in ths:
        hit = compare(e.value, th.operator, th.value)
        key = terminalId + "|" + pointId + "|" + alarmType
        if th.debounceCount == 0:                   # 开关量不去抖
            hit ? raise(key,e,th) : clear(key,e); continue
        n = counters[key]; hit ? n++ : n=0; counters[key]=n
        ...
```

质量非 `GOOD` 在入口即返回，不在阈值函数内判断。

## 去抖与恢复

- 模拟量：连续 N 次满足才 `raise`，N 按测点配置。`BR-32`
- 模拟量恢复：**连续 N 次不满足**才 `clear`。`BR-33`
- 开关量（`debounceCount == 0`）：当次即产生、当次即恢复。`BR-34`

**恢复条件存在文档冲突**：详细设计 A9 伪码写 `if !hit && n == 0` 即首次未命中恢复。按此实现会让告警在数值抖动一次就恢复、下一周期立刻重新拉响，产生刷屏，且 AC-05 不通过。

**按连续 N 次实现**（与 `raise` 对称），在任务单标注该偏差，并登记到 `06-项目记录/02-待确认项与开放决策.md` 的 O-08。

## 告警状态机

```text
ACTIVE → RECOVERED → ACKED → CLOSED
```

恢复与确认是两个独立状态，不得合并成"已处理"。

```text
raise(key,e,th):
    closeLowerSamePoint(key)          # 高高限触发时闭合同测点高限
    UPSERT alarm ON CONFLICT(dedupe_key) WHERE status='ACTIVE'
        DO UPDATE SET last_time=..., trigger_value=..., trigger_count=trigger_count+1
    active.add(key)
    if !e.backfill:                   # 补传不通知
        go postNotify(...)            # 异步

clear(key,e):
    UPDATE alarm SET recover_time=..., status='RECOVERED'
        WHERE dedupe_key=... AND status='ACTIVE'
    active.remove(key); counters[key]=0
```

- 未恢复期间不重复产生告警：靠 `active` 集合判断，不查库；`ux_alarm_active` 唯一索引兜底。`BR-38`
- 补传告警记历史越限，不发通知。`BR-36`
- 判定与排序全用 `collectTime`。`BR-35`
- 确认由 Dashboard 发起，写 `ackedBy` / `ackedTime`，adapter 不提供确认入口。`BR-37`

## 通知

按级别分流（接口规范 §11.3）：

| level | 通知策略 |
|---:|---|
| 1 | 仅入库 |
| 2 | 入库 + 通知（5 分钟合并） |
| 3 | 入库 + 立即通知 |
| 4 | 入库 + 立即通知（置顶） |

`POST {NOTIFY_ENDPOINT}`：

| 字段 | 类型 | 必填 | 说明 |
|---|---|:--:|---|
| `notifyId` | string | ✓ | `NT-{terminalId}-{pointId}-{occurTime}` |
| `terminalId` | string | ✓ | |
| `pointId` | string | ✓ | |
| `alarmType` | string | ✓ | |
| `level` | int | ✓ | 1 提示 / 2 警告 / 3 严重 / 4 紧急 |
| `triggerValue` | number | ✓ | 工程值 |
| `occurTime` | int64 | ✓ | 毫秒，采集时间轴 |
| `scene` | string | | 工况场景 |
| `reason` | string | | 人类可读原因 |

- 只发不等结果，短超时，失败只记投递日志。`BR-41`
- **重试责任在编排层**，adapter 不做重试。
- 编排层不可达时记一条失败记录并继续，不回滚告警落库。
- `notifyId` 由 adapter 生成，**编排层不得重新生成**——两侧不一致会导致去重失效。

`level = 2` 的 5 分钟合并在接口规范里有定义但详细设计未落点。本期按立即通知实现并在任务单标注偏差，不要 adapter 与 Node-RED 各写一版。

## 阈值快照

```text
for range ticker(60s):
    newSnapshot = loadThresholdsFromDB()
    atomic.StorePointer(&snapshot, newSnapshot)   # 只换指针
```

整表原子替换。逐条就地更新会让同一次求值读到新旧混杂的规则，瞬态误判最难排查。

## 启动重建

活动告警集合必须在 MQTT 订阅**之前**从数据库重建完成：

```text
for row in SELECT dedupe_key FROM alarm WHERE status='ACTIVE':
    active.add(row.dedupe_key)
# counters 保持空，去抖计数从 0 开始
```

顺序反了，订阅后立即到达的数据会对已告警测点重复拉响，制造假告警风暴。

## 健康

`/healthz` 的 `evaluate` 字段：`queueDepth` / `queueCapacity` / `dropCount` / `activeAlarmCount` / `debounceKeys` / `lastEvalTime`。

| 判据 | 结论 |
|---|---|
| `dropCount` 持续增长 | 降级，判定跟不上入库，告警会漏 |
| 活动告警数 ≠ DB 未恢复告警数 | 启动重建失败或中途异常，需重启 |
| 判定段字段整体缺失 | 判定协程未启动，503 |
| 通知请求失败 | 不影响健康结论 |

## 自检

- [ ] 判定消费者唯一，`counters` 与 `active` 无锁单写
- [ ] 队列投递 `select default` 非阻塞，满则计数不阻塞
- [ ] `quality != GOOD` 在入口返回
- [ ] 恢复按连续 N 次，与触发对称
- [ ] 通知异步发出，adapter 内无重试
- [ ] `notifyId` 格式正确，未被下游重算
- [ ] 阈值整表原子替换
- [ ] 启动先重建 `active` 再订阅
- [ ] 无建表语句、无 HTTP 投递重试代码
- [ ] 无 `internal/adapter/ingest` import