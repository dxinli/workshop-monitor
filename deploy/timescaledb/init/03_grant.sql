-- 001 角色授权（§6）
-- 由 docker-entrypoint 在 01_schema.sql 之后、按文件名顺序执行；可重复执行。
-- 先 REVOKE 再 GRANT：重复执行时权限只会收敛到 §6，不会被放大。

SET search_path TO monitor, public;

GRANT USAGE ON SCHEMA monitor TO workshop_adapter, workshop_dashboard, workshop_notify, workshop_readonly;

REVOKE ALL ON ALL TABLES    IN SCHEMA monitor FROM workshop_adapter, workshop_dashboard, workshop_notify, workshop_readonly;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA monitor FROM workshop_adapter, workshop_dashboard, workshop_notify, workshop_readonly;

-- workshop_readonly：全部表与视图只读 §6
GRANT SELECT ON ALL TABLES IN SCHEMA monitor TO workshop_readonly;

-- workshop_adapter：配置域只读；测量、报文、告警、事件读写；无 alarm_ack 写 §6
GRANT SELECT ON terminal, bus, device, point, threshold TO workshop_adapter;
GRANT SELECT ON alarm_ack TO workshop_adapter;
GRANT SELECT, INSERT, UPDATE, DELETE ON
  measurement, realtime_value, message_log, raw_message,
  alarm, device_event, terminal_state, service_health
TO workshop_adapter;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA monitor TO workshop_adapter;

-- workshop_dashboard：alarm 与 alarm_ack 读写；配置域只读；读视图 §6
GRANT SELECT ON terminal, bus, device, point, threshold TO workshop_dashboard;
GRANT SELECT, INSERT, UPDATE, DELETE ON alarm, alarm_ack TO workshop_dashboard;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA monitor TO workshop_dashboard;
GRANT SELECT ON
  v_point_dict, v_realtime, v_alarm_active, v_alarm_history,
  v_energy_hourly, v_quality_stat, v_raw_recent, v_terminal_overview
TO workshop_dashboard;

-- workshop_notify：notify_log 读写；配置域 + 告警只读 §6
GRANT SELECT ON terminal, bus, device, point, threshold TO workshop_notify;
-- alarm 只读：卡片要带 alarmId，notify_log.alarm_id 是它的外键。
-- 只读是刻意的——通知失败不回滚告警，编排层不该有改告警状态的能力。
GRANT SELECT ON alarm TO workshop_notify;
GRANT SELECT, INSERT, UPDATE, DELETE ON notify_log TO workshop_notify;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA monitor TO workshop_notify;

-- adapter 不得写 alarm_ack（确认只从 Dashboard 出口出）
REVOKE INSERT, UPDATE, DELETE ON alarm_ack FROM workshop_adapter;

-- 连续聚合由属主刷新，业务角色只读它。
GRANT SELECT ON measurement_1m TO workshop_adapter, workshop_dashboard, workshop_notify, workshop_readonly;