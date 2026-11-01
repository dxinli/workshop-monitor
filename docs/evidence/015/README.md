# 015 验收证据

| 文件 | 内容 |
|---|---|
| `acceptance.json` | 7 条用例的实测结果，由 `scripts/accept-015.py` 生成 |

生成方式（主通道：模拟器 → 虚拟串口 → edge-agent → Broker → proto-adapter）：

```bash
scripts/setup-pty.sh
.venv-sim/bin/python sim/modbus-slave-sim/main.py --config config/terminals --ports .pty/ports.json &
go build -o /tmp/edge-agent ./cmd/edge-agent
go build -o /tmp/proto-adapter ./cmd/proto-adapter
.venv-sim/bin/python scripts/accept-015.py --agent /tmp/edge-agent \
  --adapter /tmp/proto-adapter --evidence docs/evidence/015
```

## 用例与判据

| 用例 | 判据 | 来源 |
|---|---|---|
| edge-agent 全项正常 | 200 / ok，config·serial·mqtt·cache 四项齐全 | 详细设计 §5 |
| MQTT 断开但采集正常 | **200 / degraded**，断网期间仍在采集 | `rules/edge-agent.md` 健康检查 |
| 串口整条总线读不回 | 503 / down，且只有该总线为 closed | 同上 |
| proto-adapter 全项正常 | 200 / ok，evaluate 六个字段齐全 | 详细设计 §5 |
| 活动告警数与库不一致 | 200 / degraded 并给出原因 | `rules/adapter-evaluate.md` 健康表 |
| `make preflight` | 12 项编号项全过 | 任务单验收项 |
| 停掉 grafana 后 preflight | 非 0 退出，且指出是第 9 项 | 「任一项失败输出原因并退出非 0」 |

## 两处必须真起进程才能验的判据

- **MQTT 断开返回 200 而不是 503**：只能停掉真 Broker 才能验。单测覆盖了判定函数，
  但「停掉 Broker 之后采集还在继续」是进程级行为，mock 不出来。
- **串口整条总线读不回返回 503**：走模拟器控制接口把 A 总线 5 个从站全部置离线，
  注入的是协议层真实超时。整条总线离线时每个块都要耗尽重试，一个周期远长于 5 秒，
  实测连续 5 个全失败周期耗时约 81 秒——这也是为什么脚本按「轮询到 A 为 closed」判定，
  而不是等固定秒数。

验收后业务表已清理，回到基线 0，配置域 140/95 未受影响。