# 003 Broker 配置与权限

> 计划任务：M3（复杂度 S）　状态：待办

## 要建什么

Mosquitto 的配置文件、ACL、账号口令文件，以及生成口令的脚本。要求按终端隔离发布权限。

## 验收

- [ ] 5 个终端账号，各自只能发布自己的上行主题
- [ ] 用终端 1 账号发布终端 2 的主题被拒（broker 返回 not authorized）
- [ ] 接入服务账号可订阅全部上行主题
- [ ] LWT 已启用，终端异常断开时状态主题收到 `OFFLINE`
- [ ] 口令文件不入版本库，由 `scripts/gen-passwd.sh` 从 `.env` 生成
- [ ] 两套命名方案都能配：`MQTT_TOPIC_SCHEME=grid` 与 `internal`

```bash
# 越权发布，应失败
mosquitto_pub -h localhost -p 1883 -u ESN-ZX-0001 -P "$PWD1" \
  -t '/edge/ESN-ZX-0002/data/up' -m '{}'
```

## 阻塞

无，可立即开始。与 001 并行。

## 加载

- rules：`.claude/rules/deploy-config.md`（Mosquitto 段）
- 主题契约：`../../docs/02-设计/04-接口规范.md` §5

## 偏差

无。ACL 变更需重载配置，不支持热生效——演示期固定 5 终端，新增终端走脚本。
