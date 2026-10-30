-- 001 数据库 schema 与初始化
-- 权威定义：docs/03-数据库设计说明书.md §2~§5 §9
-- 只建结构，不灌数据；种子数据由 002 的 make seed 生成 02_seed.sql。
-- 必须幂等：同一库重复执行为空操作（策略类对象用先删后加，避免重复 job）。

CREATE SCHEMA IF NOT EXISTS monitor;
SET search_path TO monitor, public;

-- 本目录整体挂载覆盖了镜像自带的 initdb 脚本，扩展要在这里自己建。
-- 扩展对象装在 pg_catalog 下，search_path 不影响它。
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- ---------------------------------------------------------------- 角色 §6
-- 口令从环境变量取，不在仓库落明文。缺变量直接中止，不建无口令的登录角色。
\getenv adapter_pw   WORKSHOP_ADAPTER_PASSWORD
\getenv dashboard_pw WORKSHOP_DASHBOARD_PASSWORD
\getenv notify_pw    WORKSHOP_NOTIFY_PASSWORD
\getenv readonly_pw  WORKSHOP_READONLY_PASSWORD

-- \getenv 在环境变量未定义时不会设置 psql 变量；统一补成空串再校验。
\if :{?adapter_pw}
\else
  \set adapter_pw ''
\endif
\if :{?dashboard_pw}
\else
  \set dashboard_pw ''
\endif
\if :{?notify_pw}
\else
  \set notify_pw ''
\endif
\if :{?readonly_pw}
\else
  \set readonly_pw ''
\endif

-- 空口令抛真实 SQL 错误：entrypoint 带 ON_ERROR_STOP，初始化中止而不是留下无口令角色。
SELECT (:'adapter_pw'   = '')::int AS adapter_pw_missing   \gset
SELECT (:'dashboard_pw' = '')::int AS dashboard_pw_missing \gset
SELECT (:'notify_pw'    = '')::int AS notify_pw_missing    \gset
SELECT (:'readonly_pw'  = '')::int AS readonly_pw_missing  \gset

\if :adapter_pw_missing
  DO $$ BEGIN RAISE EXCEPTION '初始化失败：环境变量 WORKSHOP_ADAPTER_PASSWORD 未设置或为空'; END $$;
\endif
\if :dashboard_pw_missing
  DO $$ BEGIN RAISE EXCEPTION '初始化失败：环境变量 WORKSHOP_DASHBOARD_PASSWORD 未设置或为空'; END $$;
\endif
\if :notify_pw_missing
  DO $$ BEGIN RAISE EXCEPTION '初始化失败：环境变量 WORKSHOP_NOTIFY_PASSWORD 未设置或为空'; END $$;
\endif
\if :readonly_pw_missing
  DO $$ BEGIN RAISE EXCEPTION '初始化失败：环境变量 WORKSHOP_READONLY_PASSWORD 未设置或为空'; END $$;
\endif

-- 角色幂等：不存在则建，存在则同步口令（口令以 .env 为准）。
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'workshop_adapter', :'adapter_pw')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_adapter')
\gexec
SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', 'workshop_adapter', :'adapter_pw')
WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_adapter')
\gexec

SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'workshop_dashboard', :'dashboard_pw')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_dashboard')
\gexec
SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', 'workshop_dashboard', :'dashboard_pw')
WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_dashboard')
\gexec

SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'workshop_notify', :'notify_pw')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_notify')
\gexec
SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', 'workshop_notify', :'notify_pw')
WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_notify')
\gexec

SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', 'workshop_readonly', :'readonly_pw')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_readonly')
\gexec
SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', 'workshop_readonly', :'readonly_pw')
WHERE EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'workshop_readonly')
\gexec

-- 属主 workshop_owner 由 compose 的 POSTGRES_USER 建立；名字不符时这里会明确报错。
ALTER ROLE workshop_owner     SET search_path TO monitor, public;
ALTER ROLE workshop_adapter   SET search_path TO monitor, public;
ALTER ROLE workshop_dashboard SET search_path TO monitor, public;
ALTER ROLE workshop_notify    SET search_path TO monitor, public;
ALTER ROLE workshop_readonly  SET search_path TO monitor, public;

-- ---------------------------------------------------------------- 配置域 §2
CREATE TABLE IF NOT EXISTS terminal (
  terminal_id    TEXT PRIMARY KEY,               -- 即终端序列号，全局唯一
  esn            TEXT NOT NULL UNIQUE,
  workshop       TEXT NOT NULL,
  line           TEXT NOT NULL,
  agent_version  TEXT,
  enabled        BOOLEAN NOT NULL DEFAULT TRUE,
  config_version TEXT NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bus (
  terminal_id    TEXT NOT NULL REFERENCES terminal(terminal_id) ON DELETE CASCADE,
  bus_id         TEXT NOT NULL CHECK (bus_id IN ('A','B')),
  port           TEXT NOT NULL,
  baud_rate      INT NOT NULL DEFAULT 9600,
  data_bits      SMALLINT NOT NULL DEFAULT 8,
  stop_bits      SMALLINT NOT NULL DEFAULT 1,
  parity         TEXT NOT NULL DEFAULT 'N' CHECK (parity IN ('N','E','O')),
  timeout_ms     INT NOT NULL DEFAULT 1000,
  retry_count    SMALLINT NOT NULL DEFAULT 2,
  inter_frame_ms SMALLINT NOT NULL DEFAULT 4,
  PRIMARY KEY (terminal_id, bus_id)
);

CREATE TABLE IF NOT EXISTS device (
  terminal_id TEXT NOT NULL,
  bus_id      TEXT NOT NULL,
  slave_addr  SMALLINT NOT NULL CHECK (slave_addr BETWEEN 1 AND 247),
  device_name TEXT NOT NULL,
  device_type TEXT NOT NULL,
  PRIMARY KEY (terminal_id, bus_id, slave_addr),
  FOREIGN KEY (terminal_id, bus_id) REFERENCES bus(terminal_id, bus_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS point (
  terminal_id TEXT NOT NULL,
  bus_id      TEXT NOT NULL,
  slave_addr  SMALLINT NOT NULL,
  point_code  TEXT NOT NULL,
  point_id    TEXT GENERATED ALWAYS AS (bus_id || '.' || slave_addr || '.' || point_code) STORED,
  point_name  TEXT NOT NULL,
  si_type     TEXT NOT NULL CHECK (si_type IN ('YC','YX')),
  data_type   TEXT NOT NULL CHECK (data_type IN ('int16','uint16','int32','uint32')),
  addr        INT NOT NULL,
  width       SMALLINT NOT NULL CHECK (width IN (1,2)),
  scale       NUMERIC(12,6) NOT NULL,
  offset_val  NUMERIC(12,6) NOT NULL DEFAULT 0,
  unit        TEXT,
  min_value   NUMERIC(12,4),
  max_value   NUMERIC(12,4),
  PRIMARY KEY (terminal_id, bus_id, slave_addr, point_code),
  FOREIGN KEY (terminal_id, bus_id, slave_addr)
    REFERENCES device(terminal_id, bus_id, slave_addr) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_point_global ON point (terminal_id, point_id);

CREATE TABLE IF NOT EXISTS threshold (
  threshold_id    BIGSERIAL PRIMARY KEY,
  alarm_type      TEXT NOT NULL,
  level           SMALLINT NOT NULL CHECK (level BETWEEN 1 AND 4),
  scope           TEXT NOT NULL CHECK (scope IN ('POINT','BUS','GLOBAL')),
  terminal_id     TEXT,
  bus_id          TEXT,
  point_code      TEXT,
  operator        TEXT NOT NULL CHECK (operator IN ('>','>=','<','<=')),
  threshold_value NUMERIC(12,4),
  debounce_count  SMALLINT NOT NULL DEFAULT 3,
  enabled         BOOLEAN NOT NULL DEFAULT TRUE,
  config_version  TEXT NOT NULL,
  CHECK ( (scope='GLOBAL' AND terminal_id IS NULL AND point_code IS NULL)
       OR (scope='BUS'    AND terminal_id IS NOT NULL AND bus_id IS NOT NULL AND point_code IS NULL)
       OR (scope='POINT'  AND terminal_id IS NOT NULL AND point_code IS NOT NULL) )
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_threshold_scope ON threshold
  (alarm_type, scope, COALESCE(terminal_id,''), COALESCE(bus_id,''), COALESCE(point_code,''));

-- ---------------------------------------------------------------- 测量域 §3
CREATE TABLE IF NOT EXISTS measurement (
  collect_time TIMESTAMPTZ NOT NULL,
  terminal_id  TEXT NOT NULL,
  bus_id       TEXT NOT NULL,
  slave_addr   SMALLINT NOT NULL,
  point_code   TEXT NOT NULL,
  point_id     TEXT NOT NULL,
  si_type      TEXT NOT NULL,
  seq          BIGINT NOT NULL,
  value        DOUBLE PRECISION,                 -- 质量非正常时为 NULL
  quality      SMALLINT NOT NULL,
  backfill     BOOLEAN NOT NULL DEFAULT FALSE,
  send_time    TIMESTAMPTZ,
  receive_time TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (collect_time, terminal_id, point_id)
);
SELECT create_hypertable('measurement','collect_time',
  chunk_time_interval => INTERVAL '1 day', if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS ix_measurement_point_time ON measurement (terminal_id, point_id, collect_time DESC);
CREATE INDEX IF NOT EXISTS ix_measurement_seq        ON measurement (terminal_id, seq);
CREATE INDEX IF NOT EXISTS ix_measurement_backfill   ON measurement (backfill) WHERE backfill;

-- 连续聚合：IF NOT EXISTS 对 timescaledb.continuous 不可靠，先查目录表再建。
SELECT (to_regclass('monitor.measurement_1m') IS NULL) AS need_create_cagg \gset
\if :need_create_cagg
CREATE MATERIALIZED VIEW measurement_1m
WITH (timescaledb.continuous) AS
SELECT time_bucket('1 minute', collect_time) AS bucket,
       terminal_id, point_id, si_type,
       avg(CASE WHEN quality = 0 THEN value END) AS avg_value,
       min(CASE WHEN quality = 0 THEN value END) AS min_value,
       max(CASE WHEN quality = 0 THEN value END) AS max_value,
       count(*) FILTER (WHERE quality = 0)       AS good_count,
       count(*)                                  AS total_count
FROM measurement
GROUP BY bucket, terminal_id, point_id, si_type
WITH NO DATA;
\endif

-- 策略不幂等（重复添加会产生重复 job），先删后加。
SELECT remove_continuous_aggregate_policy('measurement_1m', if_exists => TRUE);
SELECT add_continuous_aggregate_policy('measurement_1m',
  start_offset      => INTERVAL '3 hours',
  end_offset        => INTERVAL '1 minute',
  schedule_interval => INTERVAL '1 minute');

SELECT remove_retention_policy('measurement', if_exists => TRUE);
SELECT add_retention_policy('measurement',    INTERVAL '7 days');
SELECT remove_retention_policy('measurement_1m', if_exists => TRUE);
SELECT add_retention_policy('measurement_1m', INTERVAL '30 days');

CREATE TABLE IF NOT EXISTS realtime_value (
  terminal_id  TEXT NOT NULL,
  point_id     TEXT NOT NULL,
  value        DOUBLE PRECISION,
  quality      SMALLINT NOT NULL,
  collect_time TIMESTAMPTZ NOT NULL,
  seq          BIGINT NOT NULL,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (terminal_id, point_id)            -- 恒定 140 行
);

-- ---------------------------------------------------------------- 报文域 §4.1
CREATE TABLE IF NOT EXISTS message_log (
  terminal_id  TEXT NOT NULL,
  seq          BIGINT NOT NULL,
  msg_type     TEXT NOT NULL,
  collect_time TIMESTAMPTZ NOT NULL,
  receive_time TIMESTAMPTZ NOT NULL DEFAULT now(),
  backfill     BOOLEAN NOT NULL DEFAULT FALSE,
  point_count  SMALLINT,
  good_count   SMALLINT,
  bad_count    SMALLINT,
  parse_status TEXT NOT NULL,   -- OK / DUPLICATE / PARSE_ERROR / IDENTITY_MISMATCH
  fail_reason  TEXT,
  PRIMARY KEY (terminal_id, seq)
);
CREATE INDEX IF NOT EXISTS ix_message_log_time ON message_log (receive_time DESC);

CREATE TABLE IF NOT EXISTS raw_message (
  receive_time  TIMESTAMPTZ NOT NULL,
  terminal_id   TEXT,
  topic         TEXT NOT NULL,
  seq           BIGINT,
  payload       JSONB NOT NULL,                  -- 原文，不裁剪
  payload_bytes INT NOT NULL,
  parse_status  TEXT NOT NULL,
  fail_reason   TEXT,
  PRIMARY KEY (receive_time, terminal_id, seq)
);
SELECT create_hypertable('raw_message','receive_time',
  chunk_time_interval => INTERVAL '1 day', if_not_exists => TRUE);
SELECT remove_retention_policy('raw_message', if_exists => TRUE);
SELECT add_retention_policy('raw_message', INTERVAL '7 days');

-- ---------------------------------------------------------------- 告警域 §4.2
CREATE TABLE IF NOT EXISTS alarm (
  alarm_id        BIGSERIAL PRIMARY KEY,
  dedupe_key      TEXT NOT NULL,                 -- 终端|测点|告警类型
  terminal_id     TEXT NOT NULL,
  point_id        TEXT NOT NULL,
  alarm_type      TEXT NOT NULL,
  level           SMALLINT NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('ACTIVE','RECOVERED','ACKED','CLOSED')),
  trigger_value   DOUBLE PRECISION,
  unit            TEXT,
  threshold_value DOUBLE PRECISION,
  occur_time      TIMESTAMPTZ NOT NULL,          -- 即采集时间
  last_time       TIMESTAMPTZ NOT NULL,
  recover_time    TIMESTAMPTZ,
  trigger_count   INT NOT NULL DEFAULT 1,
  backfill        BOOLEAN NOT NULL DEFAULT FALSE,
  notify_status   TEXT CHECK (notify_status IN ('PENDING','SENT','FAILED','SKIPPED')),
  message         TEXT
);
-- 合并规则的结构级保障：同一去重键只允许一条活动告警
CREATE UNIQUE INDEX IF NOT EXISTS ux_alarm_active ON alarm (dedupe_key) WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS ix_alarm_time  ON alarm (occur_time DESC);
CREATE INDEX IF NOT EXISTS ix_alarm_scope ON alarm (terminal_id, point_id, occur_time DESC);

CREATE TABLE IF NOT EXISTS alarm_ack (
  ack_id     BIGSERIAL PRIMARY KEY,
  alarm_id   BIGINT NOT NULL REFERENCES alarm(alarm_id) ON DELETE CASCADE,
  acked_by   TEXT NOT NULL,
  acked_time TIMESTAMPTZ NOT NULL DEFAULT now(),
  note       TEXT
);

CREATE TABLE IF NOT EXISTS notify_log (
  notify_id   TEXT PRIMARY KEY,                  -- 通知幂等键
  alarm_id    BIGINT REFERENCES alarm(alarm_id),
  channel     TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('SENT','FAILED','SKIPPED')),
  retry_count SMALLINT NOT NULL DEFAULT 0,
  http_code   INT,
  fail_reason TEXT,
  sent_time   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- 事件与运维 §4.3
CREATE TABLE IF NOT EXISTS device_event (
  event_id    BIGSERIAL PRIMARY KEY,
  terminal_id TEXT NOT NULL,
  bus_id      TEXT,
  slave_addr  SMALLINT,
  event_type  TEXT NOT NULL,
  event_time  TIMESTAMPTZ NOT NULL,
  reason      TEXT,
  detail      TEXT,
  backfill    BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS terminal_state (
  terminal_id    TEXT PRIMARY KEY REFERENCES terminal(terminal_id),
  status         TEXT NOT NULL CHECK (status IN ('ONLINE','OFFLINE','UNKNOWN')),
  last_data_time TIMESTAMPTZ,
  last_seen_time TIMESTAMPTZ,
  offline_reason TEXT,
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS service_health (
  checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  service    TEXT NOT NULL,
  instance   TEXT,
  status     TEXT NOT NULL,
  detail     JSONB
);

-- ---------------------------------------------------------------- 视图 §5
CREATE OR REPLACE VIEW v_point_dict AS
SELECT p.terminal_id, p.point_id, p.point_code, p.point_name, p.si_type,
       p.unit, p.bus_id, p.slave_addr, p.min_value, p.max_value,
       d.device_name, d.device_type, t.workshop, t.line
FROM point p
JOIN device   d ON d.terminal_id = p.terminal_id AND d.bus_id = p.bus_id AND d.slave_addr = p.slave_addr
JOIN terminal t ON t.terminal_id = p.terminal_id;

CREATE OR REPLACE VIEW v_realtime AS
SELECT r.terminal_id, r.point_id, r.value, r.quality, r.collect_time, r.seq,
       d.point_name, d.unit, d.si_type, d.bus_id, d.slave_addr,
       d.workshop, d.line, d.device_name,
       (now() - r.collect_time) > INTERVAL '30 seconds' AS is_stale
FROM realtime_value r JOIN v_point_dict d
  ON d.terminal_id = r.terminal_id AND d.point_id = r.point_id;

-- 能耗口径：功率求均值，电能取末值差，绝不求和
CREATE OR REPLACE VIEW v_energy_hourly AS
SELECT time_bucket('1 hour', bucket) AS hour, terminal_id, point_id,
       avg(avg_value)                  AS avg_power_kw,
       max(max_value) - min(max_value) AS delta_kwh
FROM measurement_1m
WHERE point_id LIKE '%.p_total' OR point_id LIKE '%.ep_forward'
GROUP BY 1, 2, 3;

CREATE OR REPLACE VIEW v_alarm_active AS
SELECT a.*, d.point_name, d.unit AS point_unit, d.workshop, d.device_name,
       (a.status <> 'ACKED') AS need_ack,
       (SELECT ack.acked_time FROM alarm_ack ack
         WHERE ack.alarm_id = a.alarm_id ORDER BY ack.acked_time DESC LIMIT 1) AS acked_time,
       (SELECT ack.acked_by   FROM alarm_ack ack
         WHERE ack.alarm_id = a.alarm_id ORDER BY ack.acked_time DESC LIMIT 1) AS acked_by
FROM alarm a LEFT JOIN v_point_dict d
  ON d.terminal_id = a.terminal_id AND d.point_id = a.point_id
WHERE a.status IN ('ACTIVE','RECOVERED');

-- 以下 4 个视图说明书 §5.1 只给用途与数据源，定义按用途补全，见任务单「偏差」。
-- 历史告警：按发生时间倒序，时间范围由面板传入
CREATE OR REPLACE VIEW v_alarm_history AS
SELECT a.*
FROM alarm a
ORDER BY a.occur_time DESC;

-- 质量统计：走分钟聚合表，按桶出好坏点数与良率，时间范围由面板传 bucket
CREATE OR REPLACE VIEW v_quality_stat AS
SELECT bucket, terminal_id, point_id,
       good_count, total_count,
       total_count - good_count AS bad_count,
       round(100.0 * good_count / NULLIF(total_count, 0), 2) AS good_rate_pct
FROM measurement_1m;

-- 最近原始报文：固定条数，不写时间过滤
CREATE OR REPLACE VIEW v_raw_recent AS
SELECT receive_time, terminal_id, topic, seq, payload, payload_bytes,
       parse_status, fail_reason
FROM raw_message
ORDER BY receive_time DESC
LIMIT 200;

-- 终端一览：在线状态、最后上报、活动告警数
CREATE OR REPLACE VIEW v_terminal_overview AS
SELECT t.terminal_id, t.esn, t.workshop, t.line, t.enabled,
       COALESCE(s.status, 'UNKNOWN') AS status,
       s.last_data_time, s.last_seen_time, s.offline_reason,
       (SELECT count(*) FROM alarm a
         WHERE a.terminal_id = t.terminal_id AND a.status = 'ACTIVE') AS active_alarm_count
FROM terminal t
LEFT JOIN terminal_state s ON s.terminal_id = t.terminal_id;