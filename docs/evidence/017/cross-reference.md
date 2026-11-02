# AC 用例 → 既有证据的交叉引用（归集部分）

017 的 20 个用例中，12 个的执行证据已在交付各任务时落档并全数通过。
本文件把用例与证据逐条对上；补跑部分（AC-04~08、14、15、17、19 与 MV 实测）
见同目录 `README.md` 的执行记录。

**通道声明**：AC-01~03、AC-09~13、AC-16 的证据全部产生于主通道
（模拟器 → 虚拟串口 → 采集应用 → Broker → 平台），未使用旁路构造器。

| 用例 | 结论 | 证据出处 | 关键实测 |
|---|---|---|---|
| AC-01 全网 140 测点 5 秒周期 | 通过 | `docs/evidence/016/timeseries.json`（61 采样 × 5 台） | 每 2 分钟增量均值 3358 行（理论 140 点 × 24 周期 = 3360）；周期间隔 [5044, 5000] ms（005） |
| AC-02 寄存器与点表一一对应 | 通过 | `docs/evidence/004/point_check.csv` | 140 行哨兵逐点比对，mismatches=0 |
| AC-03 工程值换算正确 | 通过 | `docs/evidence/005/acceptance.json` | int16/uint16/uint32 高低字、符号扩展、超量程保留数值标 5，9 用例全过 |
| AC-09 断开上行采集不中断 | 通过 | `docs/evidence/007/acceptance.json` | 断网期间 5 周期间隔 [5001, 5000, 5000, 5026] ms，入队 10 条 |
| AC-10 三上限 FIFO 丢最旧并上报 | 通过 | `docs/evidence/007/acceptance.json` | 7 天 / 30 万条 / 512 MB 冻结值；BACKFILL_BATCH=2 每轮 [2,2,2,2,2] |
| AC-11 持久队列跨重启 | 通过 | `docs/evidence/007/acceptance.json` | 断网入队 6 条，重启后 backfill=true 6 条，seq 正序、未复用新序号 |
| AC-12 补传后实时值不回退 | 通过 | `docs/evidence/016/acceptance.json` | 哨兵 625→62.5，实时值 62.5 未回退；补传 8820 行后 collect_time 保留原时刻 |
| AC-13 按 seq 正序补传，collectTime 保留 | 通过 | `docs/evidence/016/acceptance.json` | 滞后 <1 秒的 0 行，平均滞后 165 秒——collectTime 是原时刻，sendTime 才是补传时刻 |
| AC-16 补传限速且不拖慢采集 | 通过 | `docs/evidence/007/acceptance.json` | 补传 5 轮每轮 ≤2 条；补传期间采集协程周期间隔不变 |
| AC-18 通知触达与留痕 | 通过 | `docs/evidence/011/`、`docs/evidence/012/` | 卡片七项齐全；level 1 只入库、level 3/4 立即发；失败 1/5/15 秒重试 4 次留痕；通道不可达时告警照常入库 |

## 佐证证据（支撑 MV 指标）

| 指标 | 证据出处 | 关键实测 |
|---|---|---|
| MV-03 界面刷新 5s | `docs/evidence/013/acceptance.json` + `deploy/grafana/dashboards/*.json` | 8 面板 `"refresh": "5s"`，时区 Asia/Shanghai |
| MV-05 健康检查与积压指标 | `docs/evidence/015/acceptance.json` | healthz 四项（cache/config/mqtt/serial），cache 含 depth/bytes/dropCount；降级/503 语义正确 |
| MV-06 存储与保留 | `docs/evidence/016/acceptance.json` | 造旧数据真跑策略：聚合 1→0、明细 10→0；job 1001(7 days)/1002(30 days) |
| MV-07 30 天查询走聚合 | `docs/evidence/013/plan-30d.txt` | EXPLAIN ANALYZE 明细 chunk 全部 never executed，只扫物化超表 |
| MV-09 采集性能 | `docs/evidence/016/timeseries.json` | 120 分钟 305 次存活检查全过，行数增量与理论分毫不差 |

## 出处标注说明

`docs/08-测试用例.md` 的 `[引]`/`[推]` 标注与本文件无关：那标注的是**用例定义**
的出处（O-20 待确认项），本文件标注的是**执行证据**的出处。执行证据全部是实测。
