package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"workshop-monitor/internal/model"
)

// RetryPolicy 是数据库写入的重试次数与间隔（AGENTS.md 重试表：DB 写 3 次/立即-1s-5s）。
type RetryPolicy struct {
	Delays []time.Duration
}

// DefaultRetryPolicy 对应「立即 / 1s / 5s」。
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Delays: []time.Duration{0, time.Second, 5 * time.Second}}
}

// Result 是一次接收处理的结局，供调用方记日志与计数。
type Result struct {
	Status     string
	TerminalID string
	Seq        int64
	MsgType    string
	PointCount int
	FailReason string
	// Rows 是本次入库的测点读数，由调用方投递给判定段。
	// 投递不放在本包内：ingest 不得 import evaluate，连接它们的是 cmd 装配的有界 channel。
	Rows []Row
}

// Pipeline 串起接收段的七个步骤：主题解析 → 解码 → 归一化 → 幂等 → 原文留存 → 投影 → 投递。
//
// 投递（⑦）不在本包内：本包只返回 Rows，由 cmd 装配的有界 channel 投给判定段，
// 因此 ingest 不 import evaluate。
type Pipeline struct {
	router *Router
	sink   Sink
	idem   *Idem
	retry  RetryPolicy
	now    func() time.Time

	// state 为空时只落库，不做状态合成——008 的行为保持不变。
	state *TermState
	// sweep 是巡检周期，取自终端的采集周期。
	sweep time.Duration
	// lastStateErr 是最近一次状态投影失败，供健康检查暴露。
	lastStateErr error
}

// LastStateError 返回最近一次状态投影失败，健康检查据此降级。
func (p *Pipeline) LastStateError() error { return p.lastStateErr }

// NewPipeline 组装接收段。外部资源（sink、时钟）全部注入，便于 seam 上替换。
func NewPipeline(router *Router, sink Sink, retry RetryPolicy) *Pipeline {
	return &Pipeline{
		router: router,
		sink:   sink,
		idem:   NewIdem(sink),
		retry:  retry,
		now:    time.Now,
	}
}

// WithTermState 注入状态判定器与巡检周期。不调用时接收段只落库，不做状态合成。
func (p *Pipeline) WithTermState(state *TermState, sweepSeconds int) *Pipeline {
	p.state = state
	p.sweep = time.Duration(sweepSeconds) * time.Second
	return p
}

// Sweep 执行一次状态巡检：超时的终端判离线并落库，其余不动。
//
// 由 cmd 装配的定时器调用。判定器自身保证「已离线的终端不再重复判离线」，
// 因此这里可以无条件遍历。
func (p *Pipeline) Sweep(ctx context.Context, now time.Time) error {
	if p.state == nil {
		return nil
	}
	for _, snap := range p.state.Snapshot() {
		before := snap.Status
		tr := p.state.Sweep(snap.TerminalID, now)
		if tr.Status == before {
			continue
		}
		updated := snap
		updated.Status = tr.Status
		updated.Reason = tr.Reason
		if err := p.withRetry(ctx, func(ctx context.Context) error {
			return p.sink.(StateSink).WriteTermState(ctx, updated)
		}); err != nil {
			return fmt.Errorf("巡检写终端状态 %s: %w", snap.TerminalID, err)
		}
	}
	return nil
}

// Handle 处理一条 MQTT 消息，返回落库结果。
//
// 顺序固定：先登记幂等键，再写任何业务表。重复消息在第一步就返回，
// 不写历史、实时值、事件——这是 BR-03 的唯一落点。
func (p *Pipeline) Handle(ctx context.Context, topic string, payload []byte) (Result, error) {
	now := p.now()

	esn, msgType, ok := p.router.Match(topic)
	if !ok {
		return Result{}, fmt.Errorf("主题 %q 不匹配任何上行模板", topic)
	}

	dec, decErr := Decode(esn, msgType, payload)
	status, reason := classify(dec, decErr)
	res := Result{Status: status, TerminalID: esn, Seq: dec.Header.Seq, MsgType: msgType, FailReason: reason}

	// 报文头解不出序号时没有幂等键可用：只落原文，不登记 message_log。
	if dec.Header.Seq <= 0 {
		raw := RawRecord{
			ReceiveTime: now, TerminalID: esn, Topic: topic,
			Payload: payload, ParseStatus: status, FailReason: reason,
		}
		if err := p.withRetry(ctx, func(ctx context.Context) error { return p.sink.WriteRaw(ctx, raw) }); err != nil {
			return res, err
		}
		return res, nil
	}

	info := buildLogInfo(dec, msgType, now, status, reason)
	res.PointCount = info.PointCount
	res.Seq = dec.Header.Seq
	if status == StatusAccepted {
		res.Rows = dec.Rows
	}

	// guardAndWrite 返回的是幂等结论：重复消息为 DUPLICATE，此时没有写任何业务表。
	// 首次消息沿用解码结论（OK / PARSE_ERROR / IDENTITY_MISMATCH），
	// 一律写成 OK 会把「只留了原文」记成「已入库」。
	st, err := p.guardAndWrite(ctx, esn, topic, payload, now, dec, status, reason, info)
	if err != nil {
		return res, err
	}
	if st == StatusDuplicate {
		res.Status = StatusDuplicate
	}
	return res, nil
}

// guardAndWrite 走「登记 → 写业务表 → 失败则撤销登记」的一次完整流程。
//
// 撤销这一步是必要的：QoS1 会重投，登记留着而业务表没写，重投就会被判成重复后静默丢失。
func (p *Pipeline) guardAndWrite(ctx context.Context, esn, topic string, payload []byte, now time.Time,
	dec Decoded, status, reason string, info LogInfo) (string, error) {

	var writeErr error
	st, err := p.idem.Guard(ctx, esn, dec.Header.Seq, info, func(ctx context.Context) error {
		writeErr = p.persist(ctx, esn, topic, payload, now, dec, status, reason)
		return writeErr
	})
	if err != nil && writeErr != nil {
		if relErr := p.sink.Release(ctx, esn, dec.Header.Seq); relErr != nil {
			return "", errors.Join(err, relErr)
		}
	}
	return st, err
}

// buildLogInfo 汇总 message_log 的计数列。非 DATA 报文的点数为 0。
func buildLogInfo(dec Decoded, msgType string, now time.Time, status, reason string) LogInfo {
	info := LogInfo{
		MsgType:     msgType,
		CollectTime: time.UnixMilli(dec.Header.CollectTime),
		ReceiveTime: now,
		Backfill:    dec.Header.Backfill,
		ParseStatus: status,
		FailReason:  reason,
	}
	if status != StatusAccepted || msgType != model.MsgTypeData {
		return info
	}
	info.PointCount = len(dec.Rows)
	for _, r := range dec.Rows {
		if r.Quality == model.QualityGood {
			info.GoodCount++
		}
	}
	info.BadCount = info.PointCount - info.GoodCount
	return info
}

// persist 写原文并投影。解析失败的报文只留原文就返回，不写历史。
func (p *Pipeline) persist(ctx context.Context, esn, topic string, payload []byte, now time.Time,
	dec Decoded, status, reason string) error {

	// 原文的 terminal_id 取主题里的 esn：身份不一致时报文头那个值不可信，
	// 而 raw_message 是排查这类问题的唯一依据。
	raw := RawRecord{
		ReceiveTime: now, TerminalID: esn, Topic: topic,
		Seq: dec.Header.Seq, Payload: payload, ParseStatus: status, FailReason: reason,
	}
	if status != StatusAccepted {
		return p.withRetry(ctx, func(ctx context.Context) error { return p.sink.WriteRaw(ctx, raw) })
	}

	rec := DataRecord{
		Header: headerInfo{
			MsgType:     dec.Header.MsgType,
			CollectTime: time.UnixMilli(dec.Header.CollectTime),
			Backfill:    dec.Header.Backfill,
			TimeSync:    dec.Header.TimeSync,
		},
		Rows:        dec.Rows,
		ReceiveTime: now,
		Raw:         raw,
	}

	return p.writeByType(ctx, dec, rec, raw, now)
}

// writeByType 按消息类型分派。状态判定在写库成功之后推进：
// 入库失败时状态机不应前移，否则重投会看到「已经在线」而跳过事件。
func (p *Pipeline) writeByType(ctx context.Context, dec Decoded, rec DataRecord, raw RawRecord, now time.Time) error {
	var err error
	switch dec.Header.MsgType {
	case model.MsgTypeData:
		err = p.withRetry(ctx, func(ctx context.Context) error { return p.sink.WriteData(ctx, rec) })
		if err == nil {
			p.applyData(dec, now)
		}
	case model.MsgTypeEvent:
		ev := eventRecord(dec, raw, now)
		err = p.withRetry(ctx, func(ctx context.Context) error { return p.sink.WriteEvent(ctx, ev) })
		if err == nil {
			p.applyDeviceEvent(ev)
		}
	case model.MsgTypeStatus:
		err = p.writeStatus(ctx, dec, raw, now)
	default:
		err = fmt.Errorf("%w: 未知 msgType %q", ErrParse, dec.Header.MsgType)
	}
	return err
}

// writeStatus 写状态报文。注入判定器时由它决定是否更新 terminal_state，
// 未注入时退化为 008 的行为：留存状态记录与原文。
func (p *Pipeline) writeStatus(ctx context.Context, dec Decoded, raw RawRecord, now time.Time) error {
	rec := statusRecord(dec, raw, now)
	if p.state != nil && dec.Status != nil {
		tr := p.state.ApplyStatus(raw.TerminalID, dec.Status.Status, dec.Status.Reason,
			time.UnixMilli(dec.Status.StatusTime), now)
		rec.Changed = tr.Changed
		rec.Status = tr.Status
		rec.Reason = tr.Reason
		snap := p.state.SnapshotOf(raw.TerminalID)
		rec.LastData = snap.LastData
		rec.LastSeen = snap.LastSeen
	}
	return p.withRetry(ctx, func(ctx context.Context) error { return p.sink.WriteStatus(ctx, rec) })
}

// applyData 推进 lastData 与在线状态。补传不推进，由 TermState 内部判定。
//
// 状态真的变化时才写 terminal_state：首次数据把终端从 UNKNOWN 带到 ONLINE 需要落库，
// 之后每个周期数据到达都不该再刷一行。
func (p *Pipeline) applyData(dec Decoded, now time.Time) {
	if p.state == nil {
		return
	}
	tr := p.state.ApplyData(dec.Header.Esn, time.UnixMilli(dec.Header.CollectTime),
		dec.Header.Backfill, now)
	if !tr.Changed {
		return
	}
	p.persistTermSnapshot(context.Background(), dec.Header.Esn)
}

// persistTermSnapshot 把判定器的当前结论写进 terminal_state。
// 写入失败只记录在返回错误里；调用方已在事务外，不能回滚已入库的测量。
func (p *Pipeline) persistTermSnapshot(ctx context.Context, terminalID string) {
	sink, ok := p.sink.(StateSink)
	if !ok {
		return
	}
	// 状态投影失败不回滚业务数据：状态会在下一个周期或巡检时重新写出。
	// 这里不能把错误抛给调用方——报文已入库，抛错会让上层把整条消息当失败处理。
	if err := p.withRetry(ctx, func(ctx context.Context) error {
		return sink.WriteTermState(ctx, p.state.SnapshotOf(terminalID))
	}); err != nil {
		p.lastStateErr = err
	}
}

// applyDeviceEvent 处理设备离线/恢复事件。终端级事件不在这里推进：
// 那由 STATUS 报文与巡检负责，混在一起会让两级事件互相覆盖。
func (p *Pipeline) applyDeviceEvent(ev EventRecord) {
	if p.state == nil || ev.BusID == "" || ev.SlaveAddr == 0 {
		return
	}
	switch ev.EventType {
	case EventSensorOffline:
		p.state.MarkDeviceOffline(ev.TerminalID, ev.BusID, ev.SlaveAddr)
	case EventSensorRecovered:
		p.state.MarkDeviceRecovered(ev.TerminalID, ev.BusID, ev.SlaveAddr)
	}
}

func eventRecord(dec Decoded, raw RawRecord, now time.Time) EventRecord {
	rec := EventRecord{
		TerminalID: raw.TerminalID, ReceiveTime: now, Raw: raw,
		Backfill: dec.Header.Backfill,
	}
	if dec.Event != nil {
		rec.BusID = dec.Event.BusID
		rec.SlaveAddr = dec.Event.SlaveAddr
		rec.EventType = dec.Event.EventType
		rec.Reason = dec.Event.Reason
		rec.Detail = dec.Event.Detail
		rec.EventTime = time.UnixMilli(dec.Event.EventTime)
	}
	return rec
}

func statusRecord(dec Decoded, raw RawRecord, now time.Time) StatusRecord {
	rec := StatusRecord{TerminalID: raw.TerminalID, ReceiveTime: now, Raw: raw}
	if dec.Status != nil {
		rec.Status = dec.Status.Status
		rec.Reason = dec.Status.Reason
		rec.StatusTime = time.UnixMilli(dec.Status.StatusTime)
	}
	return rec
}

// withRetry 按 立即 / 1s / 5s 重试。次数与间隔固定，不无限重试。
func (p *Pipeline) withRetry(ctx context.Context, fn func(context.Context) error) error {
	var lastErr error
	for _, d := range p.retry.Delays {
		if d > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("重试被取消: %w", ctx.Err())
			case <-time.After(d):
			}
		}
		if err := fn(ctx); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("重试 %d 次仍失败: %w", len(p.retry.Delays), lastErr)
}

// classify 把解码错误映射为 message_log.parse_status。
func classify(dec Decoded, err error) (status, reason string) {
	switch {
	case err == nil:
		return StatusAccepted, ""
	case errors.Is(err, ErrIdentityMismatch):
		return StatusIdentityMismatch, err.Error()
	default:
		return StatusParseError, err.Error()
	}
}
