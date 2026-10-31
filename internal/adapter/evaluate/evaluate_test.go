package evaluate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"workshop-monitor/internal/adapter/evaluate"
)

// memStore 是判定段的持久化替身。判定结论全部经它观察，不碰数据库。
type memStore struct {
	rules  []evaluate.Rule
	active []evaluate.ActiveAlarm

	raised  []evaluate.Alarm
	cleared []string
	closed  []string

	raiseErr error
}

func (m *memStore) LoadRules(context.Context) ([]evaluate.Rule, error) { return m.rules, nil }

func (m *memStore) ActiveAlarms(context.Context) ([]evaluate.ActiveAlarm, error) {
	return m.active, nil
}

func (m *memStore) Raise(_ context.Context, a evaluate.Alarm) (int64, error) {
	if m.raiseErr != nil {
		return 0, m.raiseErr
	}
	m.raised = append(m.raised, a)
	return int64(len(m.raised)), nil
}

func (m *memStore) Clear(_ context.Context, key string, _ time.Time) error {
	m.cleared = append(m.cleared, key)
	return nil
}

func (m *memStore) CloseLower(_ context.Context, terminalID, pointID string, level int, _ time.Time) ([]string, error) {
	var out []string
	for _, a := range m.raised {
		if a.TerminalID == terminalID && a.PointID == pointID && a.Level < level {
			out = append(out, a.DedupeKey)
			m.closed = append(m.closed, a.DedupeKey)
		}
	}
	return out, nil
}

// memNotifier 只记录要发什么，不真的发。
type memNotifier struct {
	sent []evaluate.NotifyRequest
}

func (m *memNotifier) Deliver(req evaluate.NotifyRequest) { m.sent = append(m.sent, req) }

func pointRule(alarmType string, level, debounce int, threshold float64) evaluate.Rule {
	return evaluate.Rule{
		AlarmType: alarmType, TerminalID: "ESN-ZX-0001", BusID: "A",
		PointCode: "motor_temp", Operator: ">", Value: threshold, HasValue: true,
		Level: level, Debounce: debounce,
	}
}

func stateRule() evaluate.Rule {
	return evaluate.Rule{
		AlarmType: "STATE_ALARM", TerminalID: "ESN-ZX-0001", BusID: "A",
		PointCode: "estop", Operator: ">=", HasValue: false,
		Level: 4, Debounce: 0,
	}
}

// mustEval 判一条读数，判定段报错直接算用例失败。
func mustEval(t *testing.T, ev *evaluate.Evaluator, ctx context.Context, s evaluate.Sample) {
	t.Helper()
	if err := ev.Evaluate(ctx, s); err != nil {
		t.Fatalf("判定不应报错: %v", err)
	}
}

func newEvaluator(t *testing.T, st *memStore) (*evaluate.Evaluator, *memNotifier) {
	t.Helper()
	nt := &memNotifier{}
	ev := evaluate.NewEvaluator(st, nt)
	if err := ev.Reload(context.Background()); err != nil {
		t.Fatalf("载入阈值不应报错: %v", err)
	}
	return ev, nt
}

// tempSample 造一条电机绕组温度读数。
func tempSample(temp float64, atMs int64) evaluate.Sample {
	v := temp
	return evaluate.Sample{
		TerminalID: "ESN-ZX-0001", BusID: "A", PointID: "A.1.motor_temp",
		PointCode: "motor_temp", Value: &v, Quality: 0,
		CollectTime: time.UnixMilli(atMs),
	}
}

// estopSample 造一条急停开关量读数。
func estopSample(v float64, atMs int64) evaluate.Sample {
	val := v
	return evaluate.Sample{
		TerminalID: "ESN-ZX-0001", BusID: "A", PointID: "A.4.estop",
		PointCode: "estop", Value: &val, Quality: 0,
		CollectTime: time.UnixMilli(atMs),
	}
}

// 模拟量必须连续 N 次越限才告警，N 按测点配置（BR-32）。
func TestAnalogAlarmNeedsConsecutiveHits(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 3, 85)}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	for i, v := range []float64{90, 91, 92} {
		if err := ev.Evaluate(ctx, tempSample(v, 1000+int64(i)*5000)); err != nil {
			t.Fatalf("判定不应报错: %v", err)
		}
		if i < 2 && len(st.raised) != 0 {
			t.Fatalf("第 %d 次越限就告警，应为连续 3 次", i+1)
		}
	}
	if len(st.raised) != 1 {
		t.Fatalf("连续 3 次越限应告警 1 次，实际 %d 次", len(st.raised))
	}
	got := st.raised[0]
	if got.AlarmType != "HIGH_LIMIT" || got.Level != 2 {
		t.Fatalf("告警内容为 %+v", got)
	}
	if got.DedupeKey != "ESN-ZX-0001|A.1.motor_temp|HIGH_LIMIT" {
		t.Fatalf("去重键为 %q", got.DedupeKey)
	}
	if got.TriggerValue != 92 {
		t.Fatalf("触发值取最后一次命中值 92，实际 %v", got.TriggerValue)
	}
}

// 中间夹一次正常读数就要重新数，波动不能被累加成越限（BR-32）。
func TestSingleFluctuationDoesNotAlarm(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 3, 85)}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	for i, v := range []float64{90, 91, 80, 90, 91, 80, 90, 91} {
		if err := ev.Evaluate(ctx, tempSample(v, 1000+int64(i)*5000)); err != nil {
			t.Fatalf("判定不应报错: %v", err)
		}
	}
	if len(st.raised) != 0 {
		t.Fatalf("从未连续 3 次越限却告警 %d 次", len(st.raised))
	}
}

// 恢复要连续 N 次不满足，与触发对称（BR-33）。首次未命中不恢复。
func TestRecoveryNeedsConsecutiveMisses(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 3, 85)}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := ev.Evaluate(ctx, tempSample(90, 1000+int64(i)*5000)); err != nil {
			t.Fatalf("判定不应报错: %v", err)
		}
	}
	if len(st.raised) != 1 {
		t.Fatalf("前置条件失败：应已告警 1 次，实际 %d", len(st.raised))
	}

	// 一次未命中就恢复会让数值抖动一次即恢复、下一周期立刻重新拉响。
	mustEval(t, ev, ctx, tempSample(80, 20000))
	if len(st.cleared) != 0 {
		t.Fatalf("首次未命中就恢复，应为连续 3 次")
	}

	mustEval(t, ev, ctx, tempSample(80, 25000))
	if len(st.cleared) != 0 {
		t.Fatalf("连续 2 次未命中仍不应恢复")
	}

	mustEval(t, ev, ctx, tempSample(80, 30000))
	if len(st.cleared) != 1 {
		t.Fatalf("连续 3 次未命中应恢复 1 次，实际 %d 次", len(st.cleared))
	}
	if st.cleared[0] != "ESN-ZX-0001|A.1.motor_temp|HIGH_LIMIT" {
		t.Fatalf("恢复的去重键为 %q", st.cleared[0])
	}
}

// 未恢复期间不重复拉响，也不重复通知（BR-38）。
func TestNoRepeatWhileActive(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 3, 85)}}
	ev, nt := newEvaluator(t, st)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		mustEval(t, ev, ctx, tempSample(90, 1000+int64(i)*5000))
	}
	if len(st.raised) != 1 {
		t.Fatalf("持续越限期间应只拉响 1 次，实际 %d 次", len(st.raised))
	}
	if len(nt.sent) != 1 {
		t.Fatalf("应只发 1 条通知，实际 %d 条", len(nt.sent))
	}
}

// 开关量当次即告警、当次即恢复，不去抖（BR-34）。
func TestStateAlarmIsImmediate(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{stateRule()}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	mustEval(t, ev, ctx, estopSample(1, 1000))
	if len(st.raised) != 1 {
		t.Fatalf("开关量当次即告警，实际 %d 次", len(st.raised))
	}
	mustEval(t, ev, ctx, estopSample(0, 2000))
	if len(st.cleared) != 1 {
		t.Fatalf("开关量当次即恢复，实际 %d 次", len(st.cleared))
	}
}

// 高高限触发时闭合同测点的高限活动告警（接口规范 §11.4 分级）。
func TestHighHighClosesHigh(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{
		pointRule("HIGH_LIMIT", 2, 1, 85),
		pointRule("HIGH_HIGH_LIMIT", 3, 1, 105),
	}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	mustEval(t, ev, ctx, tempSample(90, 1000)) // 只命中高限
	if len(st.raised) != 1 || st.raised[0].AlarmType != "HIGH_LIMIT" {
		t.Fatalf("先只应产生高限告警，实际 %+v", st.raised)
	}

	mustEval(t, ev, ctx, tempSample(110, 2000)) // 两个都命中
	if len(st.raised) != 2 || st.raised[1].AlarmType != "HIGH_HIGH_LIMIT" {
		t.Fatalf("应再产生高高限告警，实际 %+v", st.raised)
	}
	if len(st.closed) != 1 || st.closed[0] != "ESN-ZX-0001|A.1.motor_temp|HIGH_LIMIT" {
		t.Fatalf("高高限应闭合同测点高限，实际 closed=%v", st.closed)
	}
}

// 质量非 GOOD 不参与求值，在入口即返回（BR-18）。
func TestBadQualityIsIgnored(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 1, 85)}}
	ev, nt := newEvaluator(t, st)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		s := tempSample(999, 1000+int64(i)*5000)
		s.Quality = 2 // TIMEOUT
		mustEval(t, ev, ctx, s)
	}
	if len(st.raised) != 0 {
		t.Fatalf("坏点不应参与求值，实际告警 %d 次", len(st.raised))
	}
	if len(nt.sent) != 0 {
		t.Fatalf("坏点不应产生通知，实际 %d 条", len(nt.sent))
	}
}

// 补传数据只记告警、不发通知（BR-36）。
func TestBackfillRecordsButDoesNotNotify(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 1, 85)}}
	ev, nt := newEvaluator(t, st)
	ctx := context.Background()

	s := tempSample(200, 1000)
	s.Backfill = true
	mustEval(t, ev, ctx, s)

	if len(st.raised) != 1 {
		t.Fatalf("补传越限仍应落库 1 条告警，实际 %d 条", len(st.raised))
	}
	if !st.raised[0].Backfill {
		t.Fatal("告警应标记 backfill")
	}
	if len(nt.sent) != 0 {
		t.Fatalf("补传不通知，实际发出 %d 条", len(nt.sent))
	}
}

// level 1 只入库不发通知；level 3 立即发（BR-39）。
func TestNotifyByLevel(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{
		pointRule("HIGH_LIMIT", 1, 1, 85),
		pointRule("HIGH_HIGH_LIMIT", 3, 1, 105),
	}}
	ev, nt := newEvaluator(t, st)
	ctx := context.Background()

	mustEval(t, ev, ctx, tempSample(90, 1000)) // 只命中 level 1
	if len(st.raised) != 1 {
		t.Fatalf("level 1 仍应入库，实际 %d 条", len(st.raised))
	}
	if len(nt.sent) != 0 {
		t.Fatalf("level 1 不应发通知，实际 %d 条", len(nt.sent))
	}

	mustEval(t, ev, ctx, tempSample(110, 2000)) // 高高限 level 3
	if len(nt.sent) != 1 {
		t.Fatalf("level 3 应立即通知，实际 %d 条", len(nt.sent))
	}
	req := nt.sent[0]
	if req.NotifyID != "NT-ESN-ZX-0001-A.1.motor_temp-2000" {
		t.Fatalf("通知标识为 %q", req.NotifyID)
	}
	if req.OccurTime != 2000 {
		t.Fatalf("通知发生时间取 collectTime，实际 %d", req.OccurTime)
	}
}

// 重启后活动集合从库重建，已告警测点不重复拉响。
func TestBootstrapRestoresActiveSet(t *testing.T) {
	st := &memStore{
		rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 1, 85)},
		active: []evaluate.ActiveAlarm{{
			DedupeKey:  "ESN-ZX-0001|A.1.motor_temp|HIGH_LIMIT",
			TerminalID: "ESN-ZX-0001", PointID: "A.1.motor_temp", Level: 2,
		}},
	}
	ev, nt := newEvaluator(t, st)
	if err := ev.Bootstrap(context.Background()); err != nil {
		t.Fatalf("重建活动集合不应报错: %v", err)
	}

	mustEval(t, ev, context.Background(), tempSample(90, 1000))
	if len(st.raised) != 0 {
		t.Fatalf("已告警测点不应重复拉响，实际 %d 条", len(st.raised))
	}
	if len(nt.sent) != 0 {
		t.Fatalf("已告警测点不应重复通知，实际 %d 条", len(nt.sent))
	}
}

// 阈值热加载：换快照后新阈值立即生效，旧快照的判定不影响新快照。
func TestReloadSwapsSnapshot(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 1, 85)}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	mustEval(t, ev, ctx, tempSample(90, 1000))
	if len(st.raised) != 1 {
		t.Fatalf("阈值 85 时 90 应告警，实际 %d 条", len(st.raised))
	}

	st.rules = []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 1, 200)}
	if err := ev.Reload(ctx); err != nil {
		t.Fatalf("重载不应报错: %v", err)
	}

	// 新阈值 200 下 90 不命中：连续未命中到 1 次即恢复。
	mustEval(t, ev, ctx, tempSample(90, 2000))
	if len(st.cleared) != 1 {
		t.Fatalf("新阈值应立即生效并恢复告警，实际 %d 次恢复", len(st.cleared))
	}
}

// 判定顺序按级别从高到低：同一次求值里更高限先拉响，不产生先报后闭合的抖动。
func TestHigherLevelRuleEvaluatedFirst(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{
		pointRule("HIGH_LIMIT", 2, 1, 85),
		pointRule("HIGH_HIGH_LIMIT", 3, 1, 105),
	}}
	ev, _ := newEvaluator(t, st)

	mustEval(t, ev, context.Background(), tempSample(110, 1000))
	if len(st.raised) != 1 {
		t.Fatalf("高高限已覆盖高限时只应产生 1 条告警，实际 %d 条", len(st.raised))
	}
	if st.raised[0].AlarmType != "HIGH_HIGH_LIMIT" {
		t.Fatalf("应只产生高高限告警，实际 %q", st.raised[0].AlarmType)
	}
	if len(st.closed) != 0 {
		t.Fatalf("不应产生先报后闭合的抖动，closed=%v", st.closed)
	}
}

// 通知队列满时丢弃并计数，不阻塞判定（不能把网络问题带进判定）。
func TestNotifyQueueDropsWhenFull(t *testing.T) {
	q := evaluate.NewNotifyQueue(&blockedSender{}, 1)
	for i := 0; i < 5; i++ {
		q.Deliver(evaluate.NotifyRequest{NotifyID: "NT-x"})
	}
	if q.Dropped() != 4 {
		t.Fatalf("容量 1 时投递 5 条应丢 4 条，实际丢 %d 条", q.Dropped())
	}
}

// blockedSender 永不返回，用来占住队列里的位置。
type blockedSender struct{}

func (blockedSender) Send(context.Context, evaluate.NotifyRequest) error { select {} }

// 迟到的旧读数不参与判定：它会倒退去抖计数，让告警在抖动中反复拉响与恢复（BR-35）。
func TestStaleReadingIsIgnored(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 2, 85)}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	mustEval(t, ev, ctx, tempSample(90, 10_000))
	mustEval(t, ev, ctx, tempSample(90, 15_000))
	if len(st.raised) != 1 {
		t.Fatalf("连续 2 次越限应告警，实际 %d 条", len(st.raised))
	}

	// 一条采集时间更早的"正常"读数迟到：若参与判定会让计数倒退并误恢复。
	mustEval(t, ev, ctx, tempSample(80, 5_000))
	if len(st.cleared) != 0 {
		t.Fatalf("迟到的旧读数不应触发恢复，实际恢复 %d 次", len(st.cleared))
	}
}

// 判定队列满时丢弃并计数，不阻塞入库；坏点在入口就不占队列。
func TestQueueDropsWhenFullAndSkipsBadQuality(t *testing.T) {
	q := evaluate.NewQueue(1)
	q.Push(tempSample(90, 1000))
	q.Push(tempSample(91, 2000))
	if q.Dropped() != 1 {
		t.Fatalf("容量 1 时压入 2 条应丢 1 条，实际丢 %d 条", q.Dropped())
	}

	bad := tempSample(999, 3000)
	bad.Quality = 2
	q.Push(bad)
	if q.Dropped() != 1 {
		t.Fatalf("坏点应在入口丢弃，不应挤掉可用读数；实际丢弃 %d 条", q.Dropped())
	}
}

// 恢复后再次越限要能重新拉响：去抖计数与活动集合都要被清干净。
func TestReraiseAfterRecovery(t *testing.T) {
	st := &memStore{rules: []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 1, 85)}}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	mustEval(t, ev, ctx, tempSample(90, 1000))
	mustEval(t, ev, ctx, tempSample(80, 2000))
	mustEval(t, ev, ctx, tempSample(90, 3000))

	if len(st.raised) != 2 {
		t.Fatalf("恢复后再次越限应重新拉响，累计告警 %d 条", len(st.raised))
	}
}

// 告警落库失败后下一次命中要能重试。
//
// 去抖计数停在 N 时，后续命中都满足 n > N 而不再尝试，一次瞬时的写库失败
// 就会让这条告警再也拉不响，直到数值回落把计数清零。
func TestRaiseRetriesAfterWriteFailure(t *testing.T) {
	st := &memStore{
		rules:    []evaluate.Rule{pointRule("HIGH_LIMIT", 2, 2, 85)},
		raiseErr: errors.New("库连接中断"),
	}
	ev, _ := newEvaluator(t, st)
	ctx := context.Background()

	if err := ev.Evaluate(ctx, tempSample(90, 1000)); err != nil {
		t.Fatalf("第 1 次命中不应报错: %v", err)
	}
	if err := ev.Evaluate(ctx, tempSample(91, 2000)); err == nil {
		t.Fatal("第 2 次命中写库失败应把错误报出来")
	}
	if len(st.raised) != 0 {
		t.Fatalf("写失败时不应记入告警，实际 %d 条", len(st.raised))
	}

	st.raiseErr = nil
	if err := ev.Evaluate(ctx, tempSample(92, 3000)); err != nil {
		t.Fatalf("库恢复后应能告警: %v", err)
	}
	if len(st.raised) != 1 {
		t.Fatalf("库恢复后应重新拉响，实际 %d 条", len(st.raised))
	}
}
