package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"workshop-monitor/internal/model"
	"workshop-monitor/internal/pkglog"
)

// 补传限速的冻结值（接口规范 §9.3 第 5 条）。
const (
	DefaultBackfillBatch = 500
	DefaultBackfillGap   = 50 * time.Millisecond
)

// BackfillSpec 是补传段的参数。
type BackfillSpec struct {
	Batch int
	Gap   time.Duration
}

// DefaultBackfillSpec 返回冻结值。
func DefaultBackfillSpec() BackfillSpec {
	return BackfillSpec{Batch: DefaultBackfillBatch, Gap: DefaultBackfillGap}
}

// Backfill 是补传协程：连接恢复后按 seq 正序重放队列，收到 PUBACK 才出队。
//
// 它与采集协程完全独立：补传慢不会拖住采集（BR-25），采集也不会等补传腾地方。
type Backfill struct {
	cache *Cache
	send  Publisher
	spec  BackfillSpec
	log   *pkglog.Logger
	// connected 报告当前能否发布。返回 false 时补传让出，等下一轮。
	connected func() bool
	// wake 是「该补传了」的信号：连接恢复、或新的一批落盘时叫醒。
	wake chan struct{}
	now  func() time.Time
}

// NewBackfill 组装补传段。connected 为 nil 视为装配错误——
// 不注入连接状态就只能盲目重试，会在断网期间空转刷日志。
func NewBackfill(cache *Cache, send Publisher, spec BackfillSpec, log *pkglog.Logger,
	connected func() bool) (*Backfill, error) {
	switch {
	case cache == nil:
		return nil, fmt.Errorf("补传段缺少本地队列")
	case send == nil:
		return nil, fmt.Errorf("补传段缺少发布通道")
	case log == nil:
		return nil, fmt.Errorf("补传段缺少日志器")
	case connected == nil:
		return nil, fmt.Errorf("补传段缺少连接状态查询")
	}
	if spec.Batch <= 0 {
		spec.Batch = DefaultBackfillBatch
	}
	if spec.Gap < 0 {
		spec.Gap = DefaultBackfillGap
	}
	return &Backfill{
		cache: cache, send: send, spec: spec, log: log, connected: connected,
		wake: make(chan struct{}, 1), now: time.Now,
	}, nil
}

// Notify 叫醒补传协程。非阻塞：上一次还没处理完就合并在一次里。
func (b *Backfill) Notify() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Run 持续补传，直到 ctx 取消。
//
// 每轮把队列里的前 Batch 条发掉再睡 Gap。一轮发不完就下一轮继续——
// 分轮的意义是让出网络与 CPU，不是等全部发完。
func (b *Backfill) Run(ctx context.Context) {
	ticker := time.NewTicker(b.spec.Gap)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.wake:
		case <-ticker.C:
		}

		if !b.connected() {
			continue
		}
		n, err := b.drain(ctx)
		if err != nil {
			b.log.Warn("补传中断", pkglog.F("err", err.Error()))
			continue
		}
		if n >= b.spec.Batch {
			// 这一轮装满了，说明队列还有货：立刻再叫醒自己，不等下一个 tick。
			b.Notify()
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.spec.Gap):
		}
	}
}

// drain 发一轮，返回本轮处理的条数（成功与失败都算，用于判断队列是否还有货）。
func (b *Backfill) drain(ctx context.Context) (int, error) {
	items, err := b.cache.Peek(b.spec.Batch)
	if err != nil {
		return 0, fmt.Errorf("读队列: %w", err)
	}
	done := 0
	for _, it := range items {
		select {
		case <-ctx.Done():
			return done, nil
		default:
		}

		payload, err := PrepareBackfill(it.Payload, b.now().UnixMilli())
		if err != nil {
			// 报文本身解不开：留着只会每次补传都失败一次。丢掉并留痕。
			b.log.Error("补传报文无法改写，丢弃",
				pkglog.F("seq", it.Seq), pkglog.F("err", err.Error()))
			if rerr := b.cache.Remove(it.Seq); rerr != nil {
				return done, fmt.Errorf("丢弃 seq=%d: %w", it.Seq, rerr)
			}
			done++
			continue
		}
		if err := b.send.Publish(it.Topic, payload, it.Retain); err != nil {
			// 没有 PUBACK 就不出队（接口规范 §9.3 第 4 条）。累计一次重试后返回，
			// 交给下一轮——同一轮里继续发后面的只会把失败排成一串。
			b.log.Warn("补传失败，保留在队列",
				pkglog.F("seq", it.Seq), pkglog.F("retryCount", it.RetryCount),
				pkglog.F("err", err.Error()))
			return done, nil
		}
		if err := b.cache.Remove(it.Seq); err != nil {
			return done, fmt.Errorf("补传出队 seq=%d: %w", it.Seq, err)
		}
		done++
	}
	if done > 0 {
		b.log.Info("补传一轮完成", pkglog.F("sent", done))
	}
	return done, nil
}

// PrepareBackfill 改写一条待补传报文：保留原 collectTime、更新 sendTime、置 backfill=true
// （接口规范 §9.3 第 3 条，BR-23）。
//
// body 原样保留——补传报文的测点质量码仍是 GOOD，backfill 是报文头布尔字段，不是质量码。
func PrepareBackfill(payload []byte, sendTime int64) ([]byte, error) {
	var env struct {
		Header *model.Header   `json:"header"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("解析报文: %w", err)
	}
	if env.Header == nil {
		return nil, fmt.Errorf("报文缺少 header")
	}
	if len(env.Body) == 0 {
		return nil, fmt.Errorf("报文缺少 body")
	}

	header := *env.Header
	header.SendTime = sendTime
	header.Backfill = true
	// collectTime 保持原值：业务时间轴不得被补传改写。

	return EncodeUplink(header, env.Body)
}
