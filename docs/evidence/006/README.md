# 006 上行与断连通知 · 验收证据

## 结论

11 条用例全部通过。`acceptance.json` 由 `scripts/accept-006.py` 生成，命令见任务单。

## 链路

主通道：从站模拟器 → 虚拟串口 → `edge-agent` → Mosquitto → 订阅端。
订阅端是锁定镜像里的 `mosquitto_sub`，以接入服务账号 `workshop_adapter` 订阅，
与平台侧同一条读权限路径。

## 逐条证据

| 用例 | 实测 |
|---|---|
| 三类消息分别订阅到，主题随方案展开 | 6 条消息，落在 `/edge/ESN-ZX-0001/{data,status}/up` |
| 报文头 8 个规范字段齐全 | `schemaVersion=2.0`、`msgId=ESN-ZX-0001-00000003`、`esn`、`seq`、`collectTime`、`sendTime`、`backfill`、`msgType` 全在 |
| 同一批共享 `seq` | 4 条 DATA，每条 14 点，`seq` 各不相同 |
| 序号单调且重启不归零 | 首轮末条 `seq=7`，重启后首条 `seq=9`（中间 8 是重启时的 ONLINE） |
| `sendTime` 独立于 `collectTime` | 每条 DATA 的 `collectTime` 都能在本地日志的「采集周期完成」里找到同一值；`sendTime` 晚于它且延迟 < 1 个周期 |
| 失败点仍在数组中且 `value=null` | A 总线 14 点齐全，`motor_temp` 为 `null` + `quality=2` |
| 异常退出 3 个周期内收到 OFFLINE | SIGKILL 后 **3 ms** 收到 `OFFLINE/UNEXPECTED_DISCONNECT`（Broker 检测到 TCP 断开即刻下发遗嘱） |
| Broker 重启后自动重连并补发 ONLINE | 重连 1 次、退避重试 2 次；重连后 Broker 上的 Retain 状态回到 `ONLINE` |
| 主题随 `MQTT_TOPIC_SCHEME` 切换 | 重建 Broker 为 `internal` 方案后消息全部落在 `/workshop/...`，`/edge/` 前缀 0 条 |
| 连续失败报离线、恢复报恢复 | 同一进程内：从站 1 连续 5 次超时 → `SENSOR_OFFLINE`；解除注入后 → `SENSOR_RECOVERED`，各 1 次 |
| `internal/model` 无方法 | 1 个文件，0 处 `func (` 声明 |

## 四处容易误判的细节

1. **状态主题是 Retain 的。** 订阅瞬间会收到上一次运行留下的 `OFFLINE`，混进统计会把
   「序号递增」判成乱序。除重连用例外的订阅统一带 `-R` 跳过 Retain 旧消息。
2. **重连用例要读 Retain，不能靠订阅。** Broker 一重启，`clean session` 的订阅就没了；
   换 `docker exec` 起的订阅端会随容器一起被杀。这条用例改成重启后新起一个订阅，
   直接看 Broker 上留下的 Retain 状态是不是又变回 `ONLINE`。
3. **切方案必须重建 Broker。** ACL 按方案选（`deploy/mosquitto/acl.<scheme>`），
   只改 agent 的环境变量会被 `grid` 的 ACL 拦下，表现为「发不出去」，而不是「发到别的主题」。
   脚本在 `internal` 段结束后强制切回 `grid`，中途失败也走 `finally`。
4. **离线/恢复必须在同一个进程里跑完。** 失败计数是进程内状态，重启会清零，
   第二次运行根本不知道设备还离线着，恢复事件不会产生。