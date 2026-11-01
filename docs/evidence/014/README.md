# 014 验收证据

| 文件 | 内容 |
|---|---|
| `acceptance.json` | 7 条验收项的实测结果 |

生成方式：

```bash
make up              # 含 node-red 服务（会构建带 dashboard 插件的镜像）
make seed            # 库内配置域投影
make verify-ack      # 逐项跑验收，跑完自清理临时容器、模拟器桩与告警
```

`verify-ack.sh` 起一个临时 Node-RED（同一镜像、同一份 `flows.json`）与一个模拟器桩，
用 compose 里那台 TimescaleDB，自造两条告警（一条 `ACTIVE`、一条 `RECOVERED`），跑完即删。
`EDIT_ENV_FILE` 可以指向另一份 `.env`。

## 为什么确认不改 `alarm.status`

任务单原文写「确认后告警状态转 `ACKED`」，但状态机是 `ACTIVE → RECOVERED → ACKED`，
现场常常在告警还 `ACTIVE` 时就确认。此时若把 `status` 改成 `ACKED`：

- 010 的 `Clear` 只更新 `status='ACTIVE'` 的行（`alarm_repo.go:119`），这条告警的真实恢复时间再也落不了库；
- `ux_alarm_active` 只覆盖 `ACTIVE` 行，改状态后同一去重键的下一次告警会另起一行，历史被切成两段。

已实测：`UPDATE ... WHERE dedupe_key=... AND status='ACTIVE'` 对 `ACKED` 行影响 0 行，
`recover_time` 永远为空。

裁决为**确认只写 `alarm_ack`，不改 `alarm.status`**，登记 O-18。活动清单的「是否已确认」
从 `alarm_ack` 的最新一条读，不看 `status`。

## 场景切换的验证方式

按钮是 UI 事件，脚本点不动。`verify-ack.sh` 在临时目录里给三个场景函数各补一个 `inject`
节点，打的是同一条「函数 → http request → 模拟器」链路——验的是按下按钮之后发生什么，
不是按钮长得对不对。仓库里的 `flows.json` 不含这些 inject 节点。