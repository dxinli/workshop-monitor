---
paths:
  - "internal/agent/**"
  - "cmd/edge-agent/**"
---

# edge-agent

终端上的采集进程。按点表周期读寄存器 → 换算工程值 → 标质量 → MQTT 上报 → 上行不可用时落本地队列 → 恢复后按序补传。

不做：告警判定、去抖、业务存储、直连数据库、调平台接口、协议对齐。判定在 adapter，边缘不做有状态时间窗口。

## 并发

并发单元数量由**总线数**决定，与终端数、测点数无关。每终端 2 条总线 → 每进程 2 个轮询协程。

| 单元 | 数量 | 持有 |
|---|---|---|
| 轮询 | 1 / 总线 | 串口句柄、本总线点表 |
| 上报 | 1 / 进程 | MQTT 连接 |
| 补传 | 1 / 进程 | 队列游标 |
| 健康检查 | 1 / 进程 | 只读快照 |

共享状态只有两处：序号分配器（原子递增）、本地队列（带锁）。总线之间不共享任何锁——一条总线阻塞不影响另一条。

## 启动顺序

强依赖，任一步失败即终止，不做降级启动：

```text
1. 读配置
2. 校验 V-01~V-12 → 失败输出具体字段并退出（非 0）
3. 打开串口 → 打不开即退出
4. 打开本地队列，读最大序号，续接序号分配
5. 建 MQTT 连接，注册 LWT
6. 启动总线轮询协程
7. 启动上报、补传协程
8. 启动 /healthz
9. 等退出信号 → 停采集 → 刷盘 → 断连
```

带病启动会产出难以解释的错值，现场排查成本远高于拒绝启动。

## 采集调度

```text
for range ticker(collectIntervalMs):
    t0 = now(); batch = empty
    for device in bus.devices:            # 从站地址升序
        for block in device.blocks:
            resp = modbusRead(bus, slaveAddr, block, timeout, retry=2)
            batch += decode(block, resp)  # 失败点也进 batch，值为空
    batch.collectTime = t0                # 取本批开始时刻
    egress.push(batch)                    # 非阻塞，不等上报
```

- 采集循环不等待上报结果。上行断开时采集继续。
- `collectTime` 取本批**开始**时刻。逐点取时间会让同批测点时间不一致，曲线出锯齿。
- 失败点也进 batch，值为空 + 质量码。

## 工程值换算

```text
v = (width==2) ? (hi<<16)|lo : raw        # 高字在前
if int16 and v >= 0x8000:      v -= 0x10000
if int32 and v >= 0x80000000:  v -= 0x100000000
eng = round(v*scale + offset, decimalsOf(scale))
if eng < min or eng > max: return {value: eng, quality: OUT_OF_RANGE}
return {value: eng, quality: GOOD}
```

超量程**保留原值并标记**，不置空。置空会丢失"值异常但可读"的信息。

## 上报与出队

```text
if !mqttConnected():      cache.append(batch); return
token = Publish(topic, payload, qos=1)
if WaitTimeout(pubAckTimeout) != nil:
                          cache.append(batch); return
if backfill:              cache.remove(batch.seq)
```

出队凭据是 Broker 确认，不是"已发出"。这是断网不丢数的唯一保障。

## 补传

```text
for {
    items = cache.peek(limit = 500)
    if empty: break
    for it in items: send(it, backfill=true)   # 保留 collectTime，更新 sendTime
    sleep(50ms)
}
```

批 ≤ 500 条、间隔 ≥ 50 ms、序号正序、采集不暂停。补传标记用报文头 `backfill`，**不用时间差判断**。

`backfill` 是报文头布尔字段，不是质量码。补传报文的测点质量码仍是 `GOOD`，只是 `backfill=true`。

## 队列裁剪

```text
trim(cache):                            # 每次入队后
    for limit in [7d, 300k, 512MB]:
        while over(limit): dropOldest(); dropped++
    if dropped >= 1000:
        publish CACHE_DROPPED {droppedCount}; dropped = 0
    if any(usage >= 80%):
        publish CACHE_WARNING {dimension, usagePct}   # 同级不重复
```

三上限任一先到即生效。裁剪与补传共用队列锁，但裁剪不得阻塞采集循环。

## 序号

终端内单调递增，进程内原子分配。**重启后不得归零**：启动时读队列最大序号续接。同一次采集的多个测点共享同一序号。

## 错误与质量码

| 错误 | 判定 | 质量码 | 处理 |
|---|---|---|---|
| 通信超时 | 响应超时 | `TIMEOUT` (2) | 重试 2 次，仍失败记本点失败 |
| 设备异常应答 | 响应含异常码 | `DEVICE_EXCEPTION` (1) | 不重试，直接标记 |
| 校验错误 | CRC 不匹配 | `CRC_ERROR` (3) | 重试 1 次，仍失败标记 |
| 解析失败 | 帧长度/字段越界 | `PARSE_ERROR` (4) | 标记 + 记原始帧 |
| 超量程 | 工程值越界 | `OUT_OF_RANGE` (5) | 保留数值 + 标记 |

边缘只产生 0~5，不产生 `STALE`（平台侧）与 `SIMULATED`/`MANUAL`（仿真侧）。任何失败都不沿用上一次正常值。

## 健康检查

`GET /healthz` → 200（就绪/降级）/ 503（不可用）。

| 判据 | 结论 |
|---|---|
| MQTT 断开但采集正常 | **降级，HTTP 200** |
| 串口任一总线打不开 | 不可用，503 |
| 缓存达 80% | 降级 |

MQTT 断开必须返回 200。返回 503 会被编排重启容器，打断采集并丢内存中未刷盘数据。

## 部署形态

容器（仿真 5 实例）与静态二进制（终端实机）共用同一产物，配置来源一致（文件 + 环境变量），**代码零差异**。出现 `if os.Getenv("IN_DOCKER")` 之类分支即为缺陷。

## 自检

- [ ] `collectTime` 取批次开始时刻，非逐点
- [ ] 失败点为空值 + 质量码，无 0 值、无沿用旧值
- [ ] 超量程保留数值
- [ ] 出队条件是 PUBACK，非"已发送"
- [ ] 补传批 ≤ 500、间隔 ≥ 50 ms、正序、采集未暂停
- [ ] 序号重启不归零
- [ ] 三上限裁剪，丢弃每满 1000 上报，无静默丢弃
- [ ] 一条总线一个协程，无跨总线共享锁
- [ ] `/healthz` 在 MQTT 断开时返回 200
- [ ] 无数据库驱动 import、无告警判定代码