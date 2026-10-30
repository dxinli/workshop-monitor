# 001 数据库 schema 与初始化

> 计划任务：M1（复杂度 M）　状态：待办

## 要建什么

在 TimescaleDB 中建立全部 15 张表、时序超表、连续聚合、保留策略、索引、视图与数据库角色。脚本落在 `deploy/timescaledb/init/`，必须可重复执行。

只建结构，不灌数据。数据由 002 的 `make seed` 导入。

权威定义见 `../../docs/02-设计/03-数据库设计说明书.md` §2~§5，本任务单不复述建表语句。

## 验收

- [ ] init 脚本对同一库执行两次不报错，第二次不产生重复对象
- [ ] `\dt monitor.*` 返回 15 张表
- [ ] `measurement` 是超表，chunk 间隔 1 天
- [ ] `measurement_1m` 连续聚合存在，`add_continuous_aggregate_policy` 生效
- [ ] `message_log` 主键为 `(terminal_id, seq)`
- [ ] `ux_alarm_active` 唯一索引存在且带 `WHERE status='ACTIVE'`
- [ ] `measurement` 与 `raw_message` 保留 7 天，`measurement_1m` 保留 30 天
- [ ] 5 个角色存在，权限符合 §6：`workshop_owner` / `_adapter` / `_dashboard` / `_notify` / `_readonly`

```bash
docker compose -f deploy/docker-compose.yml --env-file deploy/versions.env --env-file .env \
  exec timescaledb psql -U workshop_owner -d workshop_monitor -c "\dt monitor.*"
```

## 阻塞

无，可立即开始。与 003 并行。

## 加载

- rules：`.claude/rules/deploy-config.md`（建表幂等、保留策略、权限分离）

## 偏差

无。表名与列名以数据库设计说明书为准，不在脚本里改名。
