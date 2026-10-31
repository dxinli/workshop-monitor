package evaluate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore 用 pgx 读写阈值、活动告警与告警记录。
//
// 只做判定段需要的操作：不建表、不提供确认入口（确认只从 Dashboard 出口出）。
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore 从外部注入连接池。
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// LoadRules 读全量阈值快照。
//
// 每条阈值展开成一个测点一条规则：库里存的就是「终端 × 测点」的投影，
// 判定段不再做模板展开，避免与 seed 的展开逻辑各写一版。
func (s *PGStore) LoadRules(ctx context.Context) ([]Rule, error) {
	const q = `
SELECT alarm_type, COALESCE(terminal_id, ''), COALESCE(bus_id, ''), COALESCE(point_code, ''),
       operator, threshold_value, level, debounce_count
FROM monitor.threshold
WHERE enabled`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("读取阈值: %w", err)
	}
	defer rows.Close()

	var out []Rule
	for rows.Next() {
		var (
			r         Rule
			threshold *float64
			operator  string
		)
		if err := rows.Scan(&r.AlarmType, &r.TerminalID, &r.BusID, &r.PointCode,
			&operator, &threshold, &r.Level, &r.Debounce); err != nil {
			return nil, fmt.Errorf("扫描阈值行: %w", err)
		}
		r.Operator = operator
		if threshold != nil {
			r.Value = *threshold
			r.HasValue = true
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历阈值: %w", err)
	}
	return out, nil
}

// ActiveAlarms 读未恢复的告警，用于启动重建。只取重建需要的四列。
func (s *PGStore) ActiveAlarms(ctx context.Context) ([]ActiveAlarm, error) {
	const q = `
SELECT dedupe_key, terminal_id, point_id, level
FROM monitor.alarm
WHERE status = 'ACTIVE'`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("读取活动告警: %w", err)
	}
	defer rows.Close()

	var out []ActiveAlarm
	for rows.Next() {
		var a ActiveAlarm
		if err := rows.Scan(&a.DedupeKey, &a.TerminalID, &a.PointID, &a.Level); err != nil {
			return nil, fmt.Errorf("扫描活动告警行: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历活动告警: %w", err)
	}
	return out, nil
}

// Raise 写入或合并一条活动告警，返回告警标识。
//
// 重复命中走 DO UPDATE 累加触发次数：ux_alarm_active 只允许一条 ACTIVE，
// 插入冲突即说明该键已在活动，更新而不是再插一条。
func (s *PGStore) Raise(ctx context.Context, a Alarm) (int64, error) {
	const q = `
INSERT INTO monitor.alarm
  (dedupe_key, terminal_id, point_id, alarm_type, level, status, trigger_value,
   threshold_value, occur_time, last_time, trigger_count, backfill, message)
VALUES ($1,$2,$3,$4,$5,'ACTIVE',$6,$7,$8,$8,1,$9,$10)
ON CONFLICT (dedupe_key) WHERE status = 'ACTIVE' DO UPDATE
SET last_time     = GREATEST(alarm.last_time, EXCLUDED.last_time),
    trigger_value = EXCLUDED.trigger_value,
    trigger_count = alarm.trigger_count + 1
RETURNING alarm_id`

	var id int64
	if err := s.pool.QueryRow(ctx, q, a.DedupeKey, a.TerminalID, a.PointID, a.AlarmType, a.Level,
		a.TriggerValue, nullFloat(a.ThresholdValue, a.HasThreshold), a.OccurTime, a.Backfill,
		a.Message).Scan(&id); err != nil {
		return 0, fmt.Errorf("写入告警 %s: %w", a.DedupeKey, err)
	}
	return id, nil
}

// Clear 把活动告警置为已恢复。
//
// 只动 ACTIVE 行：已确认（ACKED）的告警不该被恢复动作改回去，那是确认状态，
// 恢复与否由 occur/recover 两个时间表达，不再改状态列。
func (s *PGStore) Clear(ctx context.Context, key string, at time.Time) error {
	const q = `
UPDATE monitor.alarm
SET status = 'RECOVERED', recover_time = $2
WHERE dedupe_key = $1 AND status = 'ACTIVE'`
	if _, err := s.pool.Exec(ctx, q, key, at); err != nil {
		return fmt.Errorf("恢复告警 %s: %w", key, err)
	}
	return nil
}

// CloseLower 闭合同测点中级别更低的活动告警，返回被闭合的去重键。
//
// 用 RETURNING 一次拿回键，不再查一遍：先查后改在并发下会闭合错行。
func (s *PGStore) CloseLower(ctx context.Context, terminalID, pointID string, level int, at time.Time) ([]string, error) {
	const q = `
UPDATE monitor.alarm
SET status = 'CLOSED', recover_time = $4
WHERE terminal_id = $1 AND point_id = $2 AND status = 'ACTIVE' AND level < $3
RETURNING dedupe_key`

	rows, err := s.pool.Query(ctx, q, terminalID, pointID, level, at)
	if err != nil {
		return nil, fmt.Errorf("闭合低级别告警 %s/%s: %w", terminalID, pointID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("扫描闭合告警: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历闭合告警: %w", err)
	}
	return out, nil
}

func nullFloat(v float64, has bool) *float64 {
	if !has {
		return nil
	}
	return &v
}
