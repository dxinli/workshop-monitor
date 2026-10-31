package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RawRecord 是一行待留存的原始报文。
//
// Seq 为 0 表示报文头解不出序号（整个载荷不是合法 JSON 时只能这样），
// 此时 message_log 也不会有对应行——没有序号就没有幂等键。
type RawRecord struct {
	ReceiveTime time.Time
	TerminalID  string
	Topic       string
	Seq         int64
	Payload     []byte
	ParseStatus string
	FailReason  string
}

// DataRecord 是一条 DATA 报文的落库输入。
type DataRecord struct {
	Header      headerInfo
	Rows        []Row
	ReceiveTime time.Time
	Raw         RawRecord
}

type headerInfo struct {
	MsgType     string
	CollectTime time.Time
	Backfill    bool
	TimeSync    bool
}

// EventRecord 是一条 EVENT 报文的落库输入。
type EventRecord struct {
	TerminalID  string
	BusID       string
	SlaveAddr   int
	EventType   string
	EventTime   time.Time
	Reason      string
	Detail      string
	Backfill    bool
	ReceiveTime time.Time
	Raw         RawRecord
}

// StatusRecord 是一条 STATUS 报文的落库输入。
//
// Changed 为 false 时只留原文，不更新 terminal_state、不记事件——
// 重复上报同一状态不该把状态表变成流水表。
type StatusRecord struct {
	TerminalID  string
	Status      string
	StatusTime  time.Time
	Reason      string
	ReceiveTime time.Time
	LastData    time.Time
	LastSeen    time.Time
	Changed     bool
	Raw         RawRecord
}

// StateSink 是状态合成所需的写入能力。Pipeline 只在注入 TermState 后使用它，
// 不注入时无需实现——008 的调用方因此不用改。
type StateSink interface {
	// WriteTermState 更新 terminal_state 并记一条终端事件。
	WriteTermState(ctx context.Context, snap TermSnapshot) error
	// WriteDeviceEvent 只记设备事件，不碰 terminal_state。
	WriteDeviceEvent(ctx context.Context, rec EventRecord) error
}

// Sink 是接收段的持久化出口。唯一实现是 PGSink；单测用内存实现。
type Sink interface {
	Claimer
	// Release 撤销一次幂等登记，用于业务写入失败后让 QoS1 重投能够重新入库。
	// 没有这一步，「已登记但没写业务表」的消息会被重投判成重复，数据静默丢失。
	Release(ctx context.Context, terminalID string, seq int64) error
	// WriteRaw 留存原文。解析失败也必须调用。
	WriteRaw(ctx context.Context, rec RawRecord) error
	// WriteData 写入历史测量与实时值。
	WriteData(ctx context.Context, rec DataRecord) error
	// WriteEvent 写入设备事件。
	WriteEvent(ctx context.Context, rec EventRecord) error
	// WriteStatus 留存状态报文原文，并按三源合成的结论更新 terminal_state 与事件。
	WriteStatus(ctx context.Context, rec StatusRecord) error
}

// PGSink 用 pgx 写入 TimescaleDB。
type PGSink struct {
	pool *pgxpool.Pool
}

// NewPGSink 从外部注入连接池——连接串与生命周期由 cmd 决定，本包不自己建连接。
func NewPGSink(pool *pgxpool.Pool) *PGSink { return &PGSink{pool: pool} }

// Claim 在 message_log 上登记幂等键。
//
// 单条 INSERT ... ON CONFLICT DO NOTHING，用影响行数判断是否首次。
// 先查后插在并发下会双双写业务表，因此这里不允许拆成两步。
func (s *PGSink) Claim(ctx context.Context, terminalID string, seq int64, info LogInfo) (bool, error) {
	const q = `
INSERT INTO monitor.message_log
  (terminal_id, seq, msg_type, collect_time, receive_time, backfill,
   point_count, good_count, bad_count, parse_status, fail_reason)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (terminal_id, seq) DO NOTHING`

	tag, err := s.pool.Exec(ctx, q, terminalID, seq, info.MsgType, info.CollectTime,
		info.ReceiveTime, info.Backfill, info.PointCount, info.GoodCount, info.BadCount,
		info.ParseStatus, nullString(info.FailReason))
	if err != nil {
		return false, fmt.Errorf("登记 message_log %s|%d: %w", terminalID, seq, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Release 删除一条幂等登记。
//
// 只在业务写入最终失败后调用。正常路径上 message_log 与业务表同生共死；
// 失败时删掉登记，重投的消息才能重新走一遍完整流程。
func (s *PGSink) Release(ctx context.Context, terminalID string, seq int64) error {
	const q = `DELETE FROM monitor.message_log WHERE terminal_id = $1 AND seq = $2`
	if _, err := s.pool.Exec(ctx, q, terminalID, seq); err != nil {
		return fmt.Errorf("撤销幂等登记 %s|%d: %w", terminalID, seq, err)
	}
	return nil
}

// WriteRaw 留存原文。payload 不是合法 JSON 时按字符串存入，
// 因为列类型是 jsonb——原文不能丢，也不能为了迁就列类型把它们丢掉。
func (s *PGSink) WriteRaw(ctx context.Context, rec RawRecord) error {
	payload, err := jsonPayload(rec.Payload)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO monitor.raw_message
  (receive_time, terminal_id, topic, seq, payload, payload_bytes, parse_status, fail_reason)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

	if _, err := s.pool.Exec(ctx, q, rec.ReceiveTime, rec.TerminalID, rec.Topic, rec.Seq,
		payload, len(rec.Payload), rec.ParseStatus, nullString(rec.FailReason)); err != nil {
		return fmt.Errorf("留存原文 %s|%d: %w", rec.TerminalID, rec.Seq, err)
	}
	return nil
}

func jsonPayload(b []byte) ([]byte, error) {
	if IsValidJSON(b) {
		return b, nil
	}
	wrapped, err := json.Marshal(string(b))
	if err != nil {
		return nil, fmt.Errorf("包装非 JSON 原文: %w", err)
	}
	return wrapped, nil
}

// WriteData 在单事务里写历史测量与实时值。
//
// 历史用 ON CONFLICT DO NOTHING：同一测点同一采集时间只留一条（BR-13）。
// 实时值只在非补传时写，且更晚的 collectTime（或同时间更大的 seq）才覆盖。
func (s *PGSink) WriteData(ctx context.Context, rec DataRecord) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertRawTx(ctx, tx, rec.Raw); err != nil {
			return err
		}
		if err := insertMeasurements(ctx, tx, rec); err != nil {
			return err
		}
		return upsertRealtime(ctx, tx, rec)
	})
}

func (s *PGSink) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}

func insertRawTx(ctx context.Context, tx pgx.Tx, rec RawRecord) error {
	payload, err := jsonPayload(rec.Payload)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO monitor.raw_message
  (receive_time, terminal_id, topic, seq, payload, payload_bytes, parse_status, fail_reason)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	if _, err := tx.Exec(ctx, q, rec.ReceiveTime, rec.TerminalID, rec.Topic, rec.Seq,
		payload, len(rec.Payload), rec.ParseStatus, nullString(rec.FailReason)); err != nil {
		return fmt.Errorf("留存原文 %s|%d: %w", rec.TerminalID, rec.Seq, err)
	}
	return nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// insertMeasurements 批量写历史测量。
//
// 主键 (collect_time, terminal_id, point_id) 是幂等的第二道防线：
// 消息级幂等已挡住重复 seq，这里再挡「同测点同采集时间」的重复行（BR-13）。
func insertMeasurements(ctx context.Context, tx pgx.Tx, rec DataRecord) error {
	if len(rec.Rows) == 0 {
		return nil
	}
	const q = `
INSERT INTO monitor.measurement
  (collect_time, terminal_id, bus_id, slave_addr, point_code, point_id, si_type,
   seq, value, quality, backfill, send_time, receive_time)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (collect_time, terminal_id, point_id) DO NOTHING`

	batch := &pgx.Batch{}
	for _, r := range rec.Rows {
		// sendTime 不在判定输入里，历史表的 send_time 允许为空：上报延迟由 agent 侧计算，
		// adapter 不推断发送时刻。
		batch.Queue(q, r.CollectTime, r.TerminalID, r.BusID, r.SlaveAddr, r.PointCode,
			r.PointID, r.SiType, r.Seq, r.Value, r.Quality, r.Backfill, nil, rec.ReceiveTime)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("写历史测量 %s|%d: %w", rec.Raw.TerminalID, rec.Raw.Seq, err)
	}
	return nil
}

// upsertRealtime 合并实时值。
//
// 「只被更晚 collectTime 覆盖」的条件必须落在 SQL 的 WHERE 里：
// 拆成应用层「先查再写」在并发下必然回退旧值。
// 补传行在这里就被过滤，不进入实时值通道（BR-12）。
func upsertRealtime(ctx context.Context, tx pgx.Tx, rec DataRecord) error {
	if rec.Header.Backfill {
		return nil
	}
	const q = `
INSERT INTO monitor.realtime_value
  (terminal_id, point_id, value, quality, collect_time, seq, updated_at)
VALUES ($1,$2,$3,$4,$5,$6, now())
ON CONFLICT (terminal_id, point_id) DO UPDATE
SET value = EXCLUDED.value, quality = EXCLUDED.quality,
    collect_time = EXCLUDED.collect_time, seq = EXCLUDED.seq, updated_at = now()
WHERE EXCLUDED.collect_time > realtime_value.collect_time
   OR (EXCLUDED.collect_time = realtime_value.collect_time
       AND EXCLUDED.seq > realtime_value.seq)`

	batch := &pgx.Batch{}
	for _, r := range rec.Rows {
		batch.Queue(q, r.TerminalID, r.PointID, r.Value, r.Quality, r.CollectTime, r.Seq)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("合并实时值 %s|%d: %w", rec.Raw.TerminalID, rec.Raw.Seq, err)
	}
	return nil
}

// WriteEvent 写设备事件（含原文）并留存原文。
func (s *PGSink) WriteEvent(ctx context.Context, rec EventRecord) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertRawTx(ctx, tx, rec.Raw); err != nil {
			return err
		}
		return insertEventTx(ctx, tx, rec)
	})
}

// WriteStatus 在单事务里留存原文、更新终端状态、按变化记事件。
//
// 状态为 UNKNOWN 且本来就没有行时不插入：terminal_state 只记录已知结论，
// 「还没收到任何信号」不是一种状态，把它写成一行反而会被界面当成真的未知终端。
func (s *PGSink) WriteStatus(ctx context.Context, rec StatusRecord) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertRawTx(ctx, tx, rec.Raw); err != nil {
			return err
		}
		if !rec.Changed {
			return nil
		}
		if err := upsertTerminalState(ctx, tx, rec); err != nil {
			return err
		}
		return insertEventTx(ctx, tx, EventRecord{
			TerminalID: rec.TerminalID,
			EventType:  eventTypeOf(rec.Status),
			EventTime:  rec.StatusTime,
			Reason:     rec.Reason,
			Raw:        rec.Raw,
		})
	})
}

// WriteTermState 只更新 terminal_state 并记事件（数据超时、数据到达两类判定走这里）。
func (s *PGSink) WriteTermState(ctx context.Context, snap TermSnapshot) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		// LastData / LastSeen 必须一起带上：漏掉会让 terminal_state 的
		// last_data_time 永远是空值，巡检也就无从判断超时。
		rec := StatusRecord{
			TerminalID: snap.TerminalID,
			Status:     snap.Status,
			Reason:     snap.Reason,
			StatusTime: snap.LastSeen,
			LastData:   snap.LastData,
			LastSeen:   snap.LastSeen,
		}
		if err := upsertTerminalState(ctx, tx, rec); err != nil {
			return err
		}
		return insertEventTx(ctx, tx, EventRecord{
			TerminalID: snap.TerminalID,
			EventType:  eventTypeOf(snap.Status),
			EventTime:  snap.LastSeen,
			Reason:     snap.Reason,
		})
	})
}

// WriteDeviceEvent 记设备离线与恢复事件。设备状态不落单独的表，只落事件。
func (s *PGSink) WriteDeviceEvent(ctx context.Context, rec EventRecord) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return insertEventTx(ctx, tx, rec) })
}

func upsertTerminalState(ctx context.Context, tx pgx.Tx, rec StatusRecord) error {
	const q = `
INSERT INTO monitor.terminal_state
  (terminal_id, status, last_data_time, last_seen_time, offline_reason, updated_at)
VALUES ($1,$2,$3,$4,$5, now())
ON CONFLICT (terminal_id) DO UPDATE
SET status = EXCLUDED.status,
    last_data_time = GREATEST(terminal_state.last_data_time, EXCLUDED.last_data_time),
    last_seen_time = GREATEST(terminal_state.last_seen_time, EXCLUDED.last_seen_time),
    offline_reason = EXCLUDED.offline_reason,
    updated_at = now()`
	if _, err := tx.Exec(ctx, q, rec.TerminalID, rec.Status, nullTime(rec.LastData),
		nullTime(rec.LastSeen), nullString(rec.Reason)); err != nil {
		return fmt.Errorf("更新终端状态 %s: %w", rec.TerminalID, err)
	}
	return nil
}

func insertEventTx(ctx context.Context, tx pgx.Tx, rec EventRecord) error {
	const q = `
INSERT INTO monitor.device_event
  (terminal_id, bus_id, slave_addr, event_type, event_time, reason, detail, backfill)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	if _, err := tx.Exec(ctx, q, rec.TerminalID, nullString(rec.BusID), nullInt(rec.SlaveAddr),
		rec.EventType, rec.EventTime, nullString(rec.Reason), nullString(rec.Detail),
		rec.Backfill); err != nil {
		return fmt.Errorf("写设备事件 %s/%s: %w", rec.TerminalID, rec.EventType, err)
	}
	return nil
}

func eventTypeOf(status string) string {
	if status == StatusOffline {
		return EventTerminalOffline
	}
	return EventTerminalOnline
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}
