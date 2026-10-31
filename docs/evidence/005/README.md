# 005 验收证据

| 文件 | 内容 |
|---|---|
| `acceptance.json` | 9 条端到端用例的实测结果，由 `scripts/accept-005.py` 生成 |

生成方式（主通道：模拟器 → pty → edge-agent → 日志）：

```bash
scripts/setup-pty.sh
.venv-sim/bin/python sim/modbus-slave-sim/main.py \
  --config config/terminals --ports .pty/ports.json &
go build -o /tmp/edge-agent ./cmd/edge-agent
.venv-sim/bin/python scripts/accept-005.py --agent /tmp/edge-agent \
  --config config/terminals/ESN-ZX-0001.json --ports .pty/agent-ports.json \
  --evidence docs/evidence/005
```

用例与任务单验收项一一对应，另有两条补充：同批共享 `collectTime`、故障注入前的正常值基线。
判据都是大幅度的（值相等、质量码相等、周期间隔在 ±700 ms 内），不做模糊阈值。
