# 011 验收证据

| 文件 | 内容 |
|---|---|
| `acceptance.json` | 10 条用例的实测结果 |

生成方式：

```bash
make up                    # 含 node-red 服务（会构建自定义镜像）
make seed                  # 库内配置域投影
make verify-notify         # 逐项跑验收，跑完自清理临时容器与临时接收端
```

`verify-notify.sh` 起一个临时 Node-RED（同一镜像、同一份 `flows.json`）与一个临时接收端，
用 compose 里那台 TimescaleDB 作为唯一留痕处，跑完即删。数据库里的 `NT-ESN-ZX-VERIFY-*`
记录也会清掉。`NOTIFY_ENV_FILE` 可以指向另一份 `.env`。

端到端那一条（adapter 判定 → 通知 → 接收端）用真实进程跑：

```bash
# 终端 1
PGPASSWORD=$WORKSHOP_ADAPTER_PASSWORD MQTT_WORKSHOP_ADAPTER_PASSWORD=$MQTT_WORKSHOP_ADAPTER_PASSWORD \
  NOTIFY_ENDPOINT=http://localhost:1880/api/notify go run ./cmd/proto-adapter -config config/adapter.json
# 终端 2：喂一批越限读数，看 notify_log 与接收端的落盘
```

2026-10-31 的实测：`A.4.estop` 拉响 `STATE_ALARM`（level 4）→ 卡片「【紧急】急停按钮状态报警」；
`B.1.motor_temp` 连续 3 次越过 85 ℃ 拉响 `HIGH_LIMIT`（level 2）→ 卡片「【警告】电机绕组温度高限」，
`notify_log` 两条均 `SENT / http_code=200`，`alarm_id` 回填到对应告警行。

验收用的临时数据已清理，业务表回到基线。