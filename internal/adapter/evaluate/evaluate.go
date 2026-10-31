package evaluate

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"workshop-monitor/internal/adapter/state"
	"workshop-monitor/internal/model"
)

// 告警状态取值，与 alarm.status 的 CHECK 约束一致。
const (
	StatusActive    = "ACTIVE"
	StatusRecovered = "RECOVERED"
	StatusClosed    = "CLOSED"
)

// Sample 是一条待判定的测点读数。由接收段经有界队列投递。
//
// 判定与排序只用 CollectTime，不用 ReceiveTime（BR-35）。
type Sample struct {
	TerminalID  string
	BusID       string
	PointID     string
	PointCode   string
	Value       *float64
	Quality     int
	CollectTime time.Time
	Seq         int64
	Backfill    bool
}

// Alarm 是一条待落库的告警。
type Alarm struct {
	DedupeKey      string
	TerminalID     string
	PointID        string
	AlarmType      string
	Level          int
	TriggerValue   float64
	ThresholdValue float64
	HasThreshold   bool
	OccurTime      time.Time
	Backfill       bool
	Message        string
}

// ActiveAlarm 是启动重建时从库里读出的一条活动告警。
type ActiveAlarm struct {
	DedupeKey  string
	TerminalID string
	PointID    string
	Level      int
}

// Store 是判定段的持久化出口。唯一实现是 PGStore，单测用内存实现。
type Store interface {
	// LoadRules 读全量阈值。热加载每次整表读，不做增量。
	LoadRules(ctx context.Context) ([]Rule, error)
	// ActiveAlarms 读未恢复的告警，用于启动重建活动集合。
	ActiveAlarms(ctx context.Context) ([]ActiveAlarm, error)
	// Raise 写入或合并一条活动告警，返回落库后的告警标识。
	Raise(ctx context.Context, a Alarm) (int64, error)
	// Clear 把一条活动告警置为已恢复。
	Clear(ctx context.Context, key string, at time.Time) error
	// CloseLower 闭合同测点中级别更低的同批活动告警，返回被闭合的去重键。
	CloseLower(ctx context.Context, terminalID, pointID string, level int, at time.Time) ([]string, error)
}

// Notifier 接收通知请求。唯一实现是 NotifyQueue；单测用记录型替身。
//
// 只有 Deliver 一个方法，且必须非阻塞：判定协程不能在这里等网络。
type Notifier interface {
	Deliver(req NotifyRequest)
}

// Evaluator 是判定段。单消费者、单写者，因此 active 与 counters 不加锁。
//
// 通知不在这里发：只往有界队列投递请求，投递由 cmd 装配的协程完成。
// 这样判定耗时不受网络影响，也符合「通知异步发出，adapter 内无重试」。
type Evaluator struct {
	store  Store
	notify Notifier

	// snap 整表原子替换。逐条就地更新会让同一次求值读到新旧混杂的规则。
	snap atomic.Pointer[Snapshot]

	active   *state.Active
	counters *state.Counters

	// lastLive 记每个测点最近一次已判定的非补传采集时间。
	// 迟到的旧读数不参与判定：它会倒退去抖计数，让告警在抖动中反复拉响与恢复。
	lastLive map[string]time.Time
}

// NewEvaluator 组装判定段。外部资源（存储、通知出口）全部注入。
func NewEvaluator(store Store, notify Notifier) *Evaluator {
	return &Evaluator{
		store:    store,
		notify:   notify,
		active:   state.NewActive(),
		counters: state.NewCounters(),
		lastLive: map[string]time.Time{},
	}
}

// Bootstrap 从库重建活动告警集合。
//
// 必须在订阅之前完成，否则订阅后立即到达的数据会对已告警测点重复拉响。
// 去抖计数不重建：它只是内存里的连续次数，从 0 开始。
func (e *Evaluator) Bootstrap(ctx context.Context) error {
	rows, err := e.store.ActiveAlarms(ctx)
	if err != nil {
		return fmt.Errorf("重建活动告警集合: %w", err)
	}
	entries := make([]state.Entry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, state.Entry{
			DedupeKey: r.DedupeKey, TerminalID: r.TerminalID, PointID: r.PointID, Level: r.Level,
		})
	}
	e.active.Replace(entries)
	return nil
}

// Reload 整表读取阈值并原子替换快照。启动时调用一次，之后每 60 秒一次。
func (e *Evaluator) Reload(ctx context.Context) error {
	rules, err := e.store.LoadRules(ctx)
	if err != nil {
		return fmt.Errorf("载入阈值: %w", err)
	}
	e.snap.Store(newSnapshot(rules))
	return nil
}

// ReloadLoop 每 60 秒整表重载。只换快照指针，不逐条改。
//
// onErr 只接收重载失败，不中断判定：拿不到新规则不代表旧规则失效。
func (e *Evaluator) ReloadLoop(ctx context.Context, every time.Duration, onErr func(error)) {
	if every <= 0 {
		every = 60 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 重载失败保留上一份快照继续判定：拿不到新规则不代表旧规则失效。
			if err := e.Reload(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// Evaluate 判定一条读数，返回写库失败的原因。
//
// 质量非 GOOD 在入口即返回：坏点不参与求值，也不推进去抖计数（BR-18）。
func (e *Evaluator) Evaluate(ctx context.Context, s Sample) error {
	if s.Quality != model.QualityGood || s.Value == nil {
		return nil
	}
	if !e.acceptTime(s) {
		return nil
	}
	snap := e.snap.Load()
	if snap == nil {
		return nil
	}
	for _, r := range snap.forPoint(s.TerminalID, s.BusID, s.PointCode) {
		if err := e.apply(ctx, s, r); err != nil {
			return err
		}
	}
	return nil
}

// acceptTime 拒绝迟到的旧读数。
//
// 补传行按 collectTime 各自独立判，不进这张表：补传的时间轴本来就是过去，
// 拿它比会把它全部拒掉，而补传告警是要落库的。
func (e *Evaluator) acceptTime(s Sample) bool {
	if s.Backfill {
		return true
	}
	if last, ok := e.lastLive[s.PointID]; ok && s.CollectTime.Before(last) {
		return false
	}
	e.lastLive[s.PointID] = s.CollectTime
	return true
}

// apply 对单条规则做去抖与状态迁移。
func (e *Evaluator) apply(ctx context.Context, s Sample, r Rule) error {
	key := dedupeKey(s.TerminalID, s.PointID, r.AlarmType)
	hit := r.hit(*s.Value)

	// 开关量不去抖：当次即告警、当次即恢复（BR-34）。
	if r.Debounce == 0 {
		switch {
		case hit && !e.active.Has(key):
			return e.raise(ctx, s, r, key)
		case !hit && e.active.Has(key):
			return e.clear(ctx, s, key)
		}
		return nil
	}
	// 用 >= 而不是 ==：落库失败时计数停在第 N 次，必须让后续命中继续重试，
	// 否则一次瞬时的写库失败就足以让这条告警再也拉不响。
	if hit {
		if n := e.counters.Hit(key); n >= r.Debounce && !e.active.Has(key) {
			return e.raise(ctx, s, r, key)
		}
		return nil
	}
	// 恢复要连续 N 次不满足，与触发对称（BR-33）。
	if n := e.counters.Miss(key); n >= r.Debounce && e.active.Has(key) {
		return e.clear(ctx, s, key)
	}
	return nil
}

// raise 产生一条活动告警。
//
// 顺序固定：更高级别已在拉响时不再叠加，否则先闭合同测点更低级别，再落库，
// 再记入内存集合，最后投递通知。落库失败时不记入集合——
// 下次命中重试比让内存与库不一致安全。
func (e *Evaluator) raise(ctx context.Context, s Sample, r Rule, key string) error {
	if e.active.HighestLevelOnPoint(s.TerminalID, s.PointID) > r.Level {
		e.counters.Reset(key)
		return nil
	}
	if err := e.closeLower(ctx, s, r.Level); err != nil {
		return err
	}

	alarm := Alarm{
		DedupeKey: key, TerminalID: s.TerminalID, PointID: s.PointID,
		AlarmType: r.AlarmType, Level: r.Level,
		TriggerValue: *s.Value, ThresholdValue: r.Value, HasThreshold: r.HasValue,
		OccurTime: s.CollectTime, Backfill: s.Backfill, Message: reasonOf(r),
	}
	if _, err := e.store.Raise(ctx, alarm); err != nil {
		return err
	}
	e.active.Add(state.Entry{
		DedupeKey: key, TerminalID: s.TerminalID, PointID: s.PointID, Level: r.Level,
	})

	// 补传只记历史不发通知（BR-36）；level 1 只入库（BR-39）。
	if s.Backfill || r.Level <= 1 || e.notify == nil {
		return nil
	}
	e.notify.Deliver(NotifyRequest{
		NotifyID:     notifyID(s),
		TerminalID:   s.TerminalID,
		PointID:      s.PointID,
		AlarmType:    r.AlarmType,
		Level:        r.Level,
		TriggerValue: *s.Value,
		OccurTime:    s.CollectTime.UnixMilli(),
		Reason:       reasonOf(r),
	})
	return nil
}

// closeLower 闭合同测点中级别更低的活动告警，并把它们移出内存集合。
func (e *Evaluator) closeLower(ctx context.Context, s Sample, level int) error {
	closed, err := e.store.CloseLower(ctx, s.TerminalID, s.PointID, level, s.CollectTime)
	if err != nil {
		return err
	}
	for _, k := range closed {
		e.active.Remove(k)
		e.counters.Reset(k)
	}
	return nil
}

// clear 把一条活动告警置为已恢复。
func (e *Evaluator) clear(ctx context.Context, s Sample, key string) error {
	if err := e.store.Clear(ctx, key, s.CollectTime); err != nil {
		return err
	}
	e.active.Remove(key)
	e.counters.Reset(key)
	return nil
}

// Stats 是判定段的内存指标，供健康检查读取。
//
// 队列自身的指标在 Queue 上（深度、容量、丢弃数），这里只报判定侧。
type Stats struct {
	ActiveAlarmCount int
	DebounceKeys     int
}

// Stats 返回当前指标。判定协程单写、健康检查只读，瞬时误差可接受。
func (e *Evaluator) Stats() Stats {
	return Stats{ActiveAlarmCount: e.active.Len(), DebounceKeys: e.counters.Len()}
}

// notifyID 是通知幂等键，格式固定：NT-{terminalId}-{pointId}-{occurTime}。
// 编排层不得重新生成，两侧不一致会让去重失效。
func notifyID(s Sample) string {
	return fmt.Sprintf("NT-%s-%s-%d", s.TerminalID, s.PointID, s.CollectTime.UnixMilli())
}

// reasonOf 生成人类可读的告警原因，写进 alarm.message 与通知卡片。
//
// 分支依据是去抖次数，不是有没有阈值：开关量的阈值行同样带 thresholdValue
// （`>= 1`），按「有没有阈值」分支会写出「连续 0 次状态报警 >= 1」。
func reasonOf(r Rule) string {
	text := alarmTypeText[r.AlarmType]
	if text == "" {
		text = r.AlarmType
	}
	if r.Debounce > 0 {
		return fmt.Sprintf("连续 %d 次%s %s %g", r.Debounce, text, r.Operator, r.Value)
	}
	if !r.HasValue {
		return fmt.Sprintf("%s：触发", text)
	}
	return fmt.Sprintf("%s：读数 %s %g", text, r.Operator, r.Value)
}

// alarmTypeText 是告警类型的中文名，取值见接口规范 §11.2。
var alarmTypeText = map[string]string{
	"HIGH_LIMIT":      "超过高限",
	"HIGH_HIGH_LIMIT": "超过高高限",
	"LOW_LIMIT":       "低于低限",
	"STATE_ALARM":     "状态报警",
}
