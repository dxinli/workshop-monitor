# 001 数据库 schema 与初始化

> 计划任务：M1（复杂度 M）　状态：待办

## 要建什么

在 TimescaleDB 中建立全部 15 张表、时序超表、连续聚合、保留策略、索引、视图与数据库角色。脚本落在 `deploy/timescaledb/init/`，必须可重复执行。

只建结构，不灌数据。数据由 002 的 `make seed` 导入。

权威定义见 `../docs/03-数据库设计说明书.md` §2~§5，本任务单不复述建表语句。

## 验收

- [x] init 脚本对同一库执行两次不报错，第二次不产生重复对象
- [x] `\dt monitor.*` 返回 15 张表
- [x] `measurement` 是超表，chunk 间隔 1 天
- [x] `measurement_1m` 连续聚合存在，`add_continuous_aggregate_policy` 生效
- [x] `message_log` 主键为 `(terminal_id, seq)`
- [x] `ux_alarm_active` 唯一索引存在且带 `WHERE status='ACTIVE'`
- [x] `measurement` 与 `raw_message` 保留 7 天，`measurement_1m` 保留 30 天
- [x] 5 个角色存在，权限符合 §6：`workshop_owner` / `_adapter` / `_dashboard` / `_notify` / `_readonly`

```bash
docker compose -f deploy/docker-compose.yml --env-file deploy/versions.env --env-file .env \
  exec timescaledb psql -U workshop_owner -d workshop_monitor -c "\dt monitor.*"
```

## 阻塞

无，可立即开始。与 003 并行。

## 加载

- rules：`.claude/rules/deploy-config.md`（建表幂等、保留策略、权限分离）
- 表结构、索引、视图、授权：`../docs/03-数据库设计说明书.md` §2~§6
  - 文档相对本任务单位于 `../docs/`，不是 `../../docs/02-设计/`——仓库内没有 `02-设计` 子目录。
  - 本任务单引用的数据库设计说明书在仓库中只有单文件，路径按实际位置书写。

## 偏差

1. **连续聚合用条件判断代替 `IF NOT EXISTS`。** `CREATE MATERIALIZED VIEW ... WITH (timescaledb.continuous)` 的 `IF NOT EXISTS` 在 TimescaleDB 2.17.2 下对同库重复执行并不可靠，改为先查 `to_regclass('monitor.measurement_1m')` 再建。语义与「幂等」一致。
2. **保留与聚合策略先 `remove_*_policy(if_exists => TRUE)` 再 `add_*_policy`。** 两个 add 都不幂等，直接重复执行会产生重复 job；先删后加保证重复执行后 job 只有一份（验证：`policy_retention` 3 条、`policy_refresh_continuous_aggregate` 1 条）。
3. **`03_grant.sql` 与 `01_schema.sql` 均会创建/同步角色。** 角色口令由 `\getenv` 从 `WORKSHOP_*_PASSWORD` 读取，空值或未定义时抛错中止（entrypoint 带 `ON_ERROR_STOP`），不落明文到脚本。`deploy/docker-compose.yml` 与 `.env.example` 相应增加了 4 个变量。
4. **视图 `v_alarm_history` / `v_quality_stat` / `v_raw_recent` / `v_terminal_overview` 的定义由本任务补写。** 说明书 §5.1 只给了用途与数据源，§5.2 只给了另外 4 个视图的定义。补写版本均不含内嵌时间过滤，时间由面板传入。`v_quality_stat` 走 `measurement_1m` 而非明细表，避免 30 天查询扫全表。
5. **`adapter` 补授 `SELECT ON alarm_ack`。** §6 的「无 `alarm_ack` 写」被理解为只读放行、禁止写；验证结果：adapter 可读、写被拒，dashboard 可读写，notify 只能读写 `notify_log`。
6. **本目录整体挂载覆盖了镜像自带的 initdb 脚本，`timescaledb` 扩展需在 `01_schema.sql` 里自行 `CREATE EXTENSION IF NOT EXISTS timescaledb`。** 否则 `create_hypertable` 不存在。
7. **`02_seed.sql` 不存在**，本任务只建结构；种子由 002 的 `make seed` 生成。
8. **`workshop_owner` 由 compose 的 `POSTGRES_USER` 建立**，脚本只对 4 个业务角色做 CREATE/ALTER；本机 `.env` 已按 `.env.example` 对齐为 `POSTGRES_DB=workshop_monitor` / `POSTGRES_USER=workshop_owner`。
