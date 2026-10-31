# modbus-slave-sim

Modbus RTU 从站模拟器。替代 50 台现场传感器，提供 10 条独立总线 × 每线 5 个从站
× 单线 14 个测点 = 140 个寄存器点。

仿真件不是交付物，`sim/` 整体可删（`.claude/rules/sim.md`）。本目录不 import
`internal/`，点表按接口规范 §3.7 的 JSON 形状自行解析。

## 组成

| 文件 | 作用 |
|---|---|
| `main.py` | 入口：读点表与端点表，起 10 条总线与控制接口 |
| `point_table.py` | 读 `config/terminals/*.json`，点表是唯一来源 |
| `registers.py` | 每条总线的寄存器存储，默认值取量程中点 |
| `server.py` | 一条总线一个 RTU 从站服务（pymodbus） |
| `faults.py` | 离线与故障注入状态机（纯逻辑，无依赖） |
| `rtu.py` | 帧编解码与 CRC，主站与从站共用 |
| `api.py` | 控制接口 `127.0.0.1:8080` |
| `acceptance.py` | 点表核对与 004 验收，产出 `docs/evidence/004/` |

## 运行

```bash
# 1. 依赖（Python 3.12.7）。venv 放在仓库根的 .venv-sim，与 CI 同名
python3.12 -m venv .venv-sim
.venv-sim/bin/pip install -r sim/modbus-slave-sim/requirements.txt pytest

# 2. 建 10 对虚拟串口，端点表写到 .pty/
make sim-pty

# 3. 起模拟器
make sim-up
```

`scripts/setup-pty.sh` 生成两份端点表：`.pty/ports.json` 给模拟器，
`.pty/agent-ports.json` 给采集应用（005）。键都是 `{terminalId}.{busId}`。

## 控制接口

只监听 `127.0.0.1:8080`，不对公网暴露。

| 接口 | 方法 | 说明 |
|---|---|---|
| `/api/v1/health` | GET | 总线清单与端口 |
| `/api/v1/reg/{terminalId}/{busId}/{slaveAddr}` | GET | 读回该从站全部寄存器 |
| `/api/v1/reg/{terminalId}/{busId}/{slaveAddr}/{addr}` | PUT | 写单个原始寄存器 |
| `/api/v1/reg/{terminalId}/{busId}/{slaveAddr}` | PUT | 按测点编码写，多寄存器自动拆字 |
| `/api/v1/offline/{terminalId}/{busId}/{slaveAddr}` | POST | 暂停应答 |
| `/api/v1/online/{terminalId}/{busId}/{slaveAddr}` | POST | 恢复应答 |
| `/api/v1/fault/{terminalId}/{busId}/{slaveAddr}` | POST | 注入故障，`{"mode":"timeout\|exception\|crc","count":5,"exceptionCode":2}` |
| `/api/v1/fault/{terminalId}/{busId}` | DELETE | 清除该总线全部注入 |

```bash
curl -X PUT localhost:8080/api/v1/reg/ESN-ZX-0001/A/1/0 -H 'Content-Type: application/json' -d '{"value":625}'
curl -X POST localhost:8080/api/v1/fault/ESN-ZX-0001/A/1 -H 'Content-Type: application/json' \
  -d '{"mode":"exception","count":3,"exceptionCode":4}'
```

写入的是**原始寄存器值**，不缩放：625 表示 62.5 ℃。

## 三类故障是协议层真实异常

| mode | 现象 | 采集侧应判为 |
|---|---|---|
| `timeout` | 收到请求，整帧不应答 | `TIMEOUT`(2) |
| `exception` | 回异常帧，功能码高位置 1：03 → 0x83、04 → 0x84 | `DEVICE_EXCEPTION`(1) |
| `crc` | 内容正确、CRC 两字节取反 | `CRC_ERROR`(3) |

返回错误数值是应用层容错，不是通信容错，不作为故障注入手段。

## 验收

```bash
.venv-sim/bin/python sim/modbus-slave-sim/acceptance.py all \
  --config config/terminals --ports .pty/agent-ports.json \
  --evidence docs/evidence/004
```

产出：

| 文件 | 内容 |
|---|---|
| `point_check.csv` | 140 行点表核对：终端 / 总线 / 从站 / 寄存器地址 / 类型 / 缩放 / 读回值 |
| `acceptance.json` | 五条验收用例的结论与实测数据 |
| `isolation.txt` | import 隔离复核结果 |

核对方式：先给每个测点写一个全局唯一的哨兵原始值，再逐点读回比对。
哨兵值按 `1000 + 序号×13` 生成，任何地址错位、总线串号、拆字顺序错误都会
让读回值与预期不符——这类错误在"看起来合理"的数值里最难发现。