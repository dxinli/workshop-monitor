# 013 验收证据

| 文件 | 内容 |
|---|---|
| `acceptance.json` | 10 条用例的实测结果 |
| `verify.txt` | `make verify-read-view` 的完整输出 |
| `plan-30d.txt` | 30 天曲线查询的 `EXPLAIN ANALYZE`，用于证明明细分支被剪枝 |

生成方式：

```bash
make up                      # 含 grafana 服务
make seed                    # 库内配置域投影
make verify-read-view        # 逐项跑验收（GRAFANA_URL 可覆盖地址）
```

面板 JSON 由 Grafana 读文件纳管（`allowUiUpdates=false`），所以验收走 Grafana 自己的
`/api/ds/query` 执行面板 SQL，而不是另写一份对账 SQL——校验的就是界面上真正跑的查询。

`plan-30d.txt` 的判据是 `on measurement (never executed)`：明细分支被 One-Time Filter 剪掉，
只扫描 `_materialized_hypertable`。这是「30 天走分钟聚合」的直接证据，不是看 SQL 里写了什么。

界面验证用的临时数据（时序、实时值、告警、报文）在验收后已清理，业务表回到基线 0。