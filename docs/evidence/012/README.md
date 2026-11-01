# 012 通知通道对接 · 验收证据

## 结论

6 条用例全部通过。`acceptance.json` 由 `scripts/accept-012.py` 生成，
连通性自检由 `scripts/check-notify.sh` 输出。

## 链路

`proto-adapter` → `POST /api/notify`（Node-RED N-01）→ 组装卡片 → N-02 投递 →
`POST http://wecom-mock:8081/webhook`（compose sim profile）→ 记录落卷。

接收端从宿主机 venv 形态改为 **compose `sim` profile 的服务**，
容器间走服务名 `wecom-mock:8081`。这是 O-19 的落点，见「O-19 关闭说明」。

## 逐条证据

| 用例 | 实测 |
|---|---|
| 通道地址与凭据走 `.env`，不入版本库 | `.env` 在 `.gitignore`、未被版本库跟踪；`deploy/ scripts/ config/ sim/` 无写死的通道地址与凭据 |
| `check-notify.sh` 验证连通并输出结果 | 7 项全 ok（配置 6 项 + 连通 1 项），退出码 0 |
| 通道可达时卡片七项字段齐全 | `SENT`；`workshop`/`terminalId`/`pointText`/`levelText`/`triggerValue`/`reason` 逐项一致，`occurTime` 非零 |
| 通道不可达时告警照常入库且留痕 | 受理 202，`notify_log.status=FAILED`、`fail_reason=RequestError: connect ECONNREFUSED`，告警行数前后不变 |
| 灰度开关关闭只写 SKIPPED、不发请求 | `status=SKIPPED`、`fail_reason=WECOM_ENABLED=false，灰度开关关闭，未发起投递`；接收端记录数不变 |
| 切换通道只改配置不改代码 | 同一份 `flows.json` 把报文投到第二个接收端，`channel` 字段跟随 `NOTIFY_CHANNEL` |

## O-19 关闭说明

O-19 记的是「Node-RED 在容器内，宿主机上的接收端不可用 `localhost` 直连」。
本任务把它落成一个 compose 服务：

```yaml
wecom-mock:
  build: { context: ../sim/wecom-mock }
  image: workshop-monitor/wecom-mock:${PYTHON_SIM_VERSION}
  profiles: ["sim"]
  ports: ["127.0.0.1:8081:8081"]
  volumes: [ wecom-data:/data ]
```

`.env` 的 `WECOM_WEBHOOK_URL` 改为 `http://wecom-mock:8081/webhook`，
与接口规范附录 B 的默认值一致，不再需要猜宿主机地址。

**实测**：`deploy-node-red-1` 容器内 `wget http://wecom-mock:8081/api/v1/records` 回 200，
容器间直连成立；宿主机侧 `127.0.0.1:8081` 同时可达（`make sim-wecom` 调试用）。

## 三处容易误判的细节

1. **`check-notify.sh` 的连通检查用 GET 打 `/webhook`，回 405 是对的。**
   接收端只收 POST。有没有 HTTP 响应才分得开「连得上但方法不对」和「根本连不上」——
   后者才是 012 要防的那类故障。所以 405 判 ok 并注明「仅证明链路通」。

2. **灰度开关只认字面量 `false`。** 空串、未设置、任何其它取值都视为开启，
   与 adapter 侧 `NOTIFY_ENABLED` 同一口径，避免把「没填」误判成「关」。

3. **`WECOM_ENABLED` 必须显式进 compose 的 node-red 环境块。**
   少了这一行 `env.get` 读到空串，灰度开关永远不生效——而现象是「怎么关都投」，
   在演示现场很难查。