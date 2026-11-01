package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"workshop-monitor/internal/model"
	"workshop-monitor/internal/pkglog"
)

// recordingBroker 记下补传发出去的东西，可选让前 n 次发布失败。
type recordingBroker struct {
	mu        sync.Mutex
	published []recorded
	failTimes int
}

func (r *recordingBroker) Publish(topic string, payload []byte, retain bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failTimes > 0 {
		r.failTimes--
		return errPublish
	}
	r.published = append(r.published, recorded{topic: topic, payload: payload, retain: retain})
	return nil
}

func (r *recordingBroker) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.published)
}

func (r *recordingBroker) snapshot() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.published...)
}

var errPublish = &publishError{}

type publishError struct{}

func (*publishError) Error() string { return "broker 不可用" }

// cacheAt 造一个带 now 覆盖的队列，用来放已知的三条报文。
func cacheAt(t *testing.T, now time.Time) *Cache {
	t.Helper()
	c := openTestCache(t, testCacheLimits(1000, 7*24*time.Hour, 1<<40), now)
	return c
}

// seedQueued 往队列里放三条 DATA：seq 3、4、5，collectTime 依次递增。
func seedQueued(t *testing.T, c *Cache, now time.Time) {
	t.Helper()
	for i, seq := range []int64{3, 4, 5} {
		collectTime := now.Add(time.Duration(i) * time.Second).UnixMilli()
		h := HeaderFor("ESN-ZX-0001", model.MsgTypeData, seq, collectTime,
			collectTime+11, true)
		body := model.DataBody{
			BusID: "A",
			YC:    []model.PointValue{{Code: "motor_temp", SlaveAddr: 1, Value: f64(62.5), Quality: 0}},
			Stat:  model.Stat{Total: 1, Good: 1},
		}
		payload, err := EncodeUplink(h, body)
		if err != nil {
			t.Fatalf("造报文 seq=%d: %v", seq, err)
		}
		if err := c.Append(CacheItem{
			Seq: seq, CollectTime: collectTime, Topic: "/edge/ESN-ZX-0001/data/up", Payload: payload,
		}); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}
}

// headerOf 解出补传报文的报文头。
func headerOf(t *testing.T, payload []byte) model.Header {
	t.Helper()
	var env struct {
		Header model.Header `json:"header"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("解析补传报文: %v", err)
	}
	return env.Header
}

// TestBackfillKeepsCollectTimeUpdatesSendTime 验 BR-23 的三个字段：
// collectTime 原样、sendTime 更新、backfill=true。
func TestBackfillKeepsCollectTimeUpdatesSendTime(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	seedQueued(t, c, now)

	broker := &recordingBroker{}
	b := newTestBackfill(t, c, broker, now)
	if _, err := b.drain(context.Background()); err != nil {
		t.Fatalf("补传: %v", err)
	}

	msgs := broker.snapshot()
	if len(msgs) != 3 {
		t.Fatalf("补传发了 %d 条，期望 3 条", len(msgs))
	}
	wantCollect := []int64{
		now.UnixMilli(),
		now.Add(time.Second).UnixMilli(),
		now.Add(2 * time.Second).UnixMilli(),
	}
	for i, m := range msgs {
		h := headerOf(t, m.payload)
		if h.Seq != int64(i+3) {
			t.Errorf("第 %d 条 seq=%d，期望 %d（正序重放）", i, h.Seq, i+3)
		}
		if h.CollectTime != wantCollect[i] {
			t.Errorf("seq=%d collectTime=%d，期望保留原值 %d", h.Seq, h.CollectTime, wantCollect[i])
		}
		if h.SendTime != now.UnixMilli() {
			t.Errorf("seq=%d sendTime=%d，期望更新为补传时刻 %d", h.Seq, h.SendTime, now.UnixMilli())
		}
		if !h.Backfill {
			t.Errorf("seq=%d backfill=false，期望 true", h.Seq)
		}
	}
}

// TestBackfillRemovesOnlyAfterPublish 验 BR-26：发布成功才出队。
func TestBackfillRemovesOnlyAfterPublish(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	seedQueued(t, c, now)

	broker := &recordingBroker{}
	b := newTestBackfill(t, c, broker, now)
	if _, err := b.drain(context.Background()); err != nil {
		t.Fatalf("补传: %v", err)
	}

	left, err := c.Peek(100)
	if err != nil {
		t.Fatalf("读队列: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("补传成功后队列还剩 %d 条，期望 0 条", len(left))
	}
}

// TestBackfillKeepsItemWhenPublishFails 验 BR-26 的反面：没有 PUBACK 就不出队。
func TestBackfillKeepsItemWhenPublishFails(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	seedQueued(t, c, now)

	broker := &recordingBroker{failTimes: 1}
	b := newTestBackfill(t, c, broker, now)
	if _, err := b.drain(context.Background()); err != nil {
		t.Fatalf("补传: %v", err)
	}

	left, err := c.Peek(100)
	if err != nil {
		t.Fatalf("读队列: %v", err)
	}
	if len(left) != 3 {
		t.Fatalf("首条发布失败后队列剩 %d 条，期望 3 条（不出一条也没出）", len(left))
	}
	if broker.count() != 0 {
		t.Fatalf("失败后仍记录了 %d 条已发布，期望 0 条", broker.count())
	}

	// 再补一轮，这次能成功：三条依次出队。
	if _, err := b.drain(context.Background()); err != nil {
		t.Fatalf("第二轮补传: %v", err)
	}
	left, _ = c.Peek(100)
	if len(left) != 0 {
		t.Fatalf("第二轮后队列剩 %d 条，期望 0 条", len(left))
	}
}

// TestBackfillBatchLimit 验 BR-24：单批 ≤ Batch 条。
func TestBackfillBatchLimit(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	seedQueued(t, c, now)

	broker := &recordingBroker{}
	b := newTestBackfill(t, c, broker, now)
	b.spec.Batch = 2 // 上限 2 条

	if _, err := b.drain(context.Background()); err != nil {
		t.Fatalf("补传: %v", err)
	}
	if broker.count() != 2 {
		t.Fatalf("单批发了 %d 条，期望 2 条（上限 2）", broker.count())
	}
	left, _ := c.Peek(100)
	if len(left) != 1 || left[0].Seq != 5 {
		t.Fatalf("剩余 = %v，期望只剩 seq=5", seqsOf(left))
	}
}

// TestBackfillDoesNotBlockOnDisconnect 验 BR-25 的一半：断网时补传直接让出，
// 不空转、不发送。
func TestBackfillDoesNotBlockOnDisconnect(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	seedQueued(t, c, now)

	broker := &recordingBroker{}
	b, err := NewBackfill(c, broker, DefaultBackfillSpec(),
		pkglog.New("agent", "ESN-ZX-0001", pkglog.LevelDebug, nil), func() bool { return false })
	if err != nil {
		t.Fatalf("组装: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	b.Run(ctx)

	if broker.count() != 0 {
		t.Fatalf("断网期间发了 %d 条，期望 0 条", broker.count())
	}
	left, _ := c.Peek(100)
	if len(left) != 3 {
		t.Fatalf("断网期间队列剩 %d 条，期望 3 条原封不动", len(left))
	}
}

// TestBackfillDrainsUntilEmpty 验连接恢复后能把队列清空（配合 Notify 叫醒）。
func TestBackfillDrainsUntilEmpty(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	seedQueued(t, c, now)

	broker := &recordingBroker{}
	b := newTestBackfill(t, c, broker, now)
	b.spec.Gap = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b.Notify()
	b.Run(ctx)

	left, _ := c.Peek(100)
	if len(left) != 0 {
		t.Fatalf("补传结束后队列还剩 %d 条，期望清空", len(left))
	}
	if broker.count() != 3 {
		t.Fatalf("补传发了 %d 条，期望 3 条", broker.count())
	}
}

// TestPrepareBackfillKeepsBodyIntact 验补传只改报文头，报文体（含质量码）原样保留。
func TestPrepareBackfillKeepsBodyIntact(t *testing.T) {
	const collectTime = int64(1793500200123)
	h := HeaderFor("ESN-ZX-0001", model.MsgTypeData, 7, collectTime, collectTime+11, true)
	body := model.DataBody{
		BusID: "A",
		YC: []model.PointValue{
			{Code: "motor_temp", SlaveAddr: 1, Value: f64(62.5), Unit: "℃", Quality: model.QualityGood},
			{Code: "env_temp", SlaveAddr: 3, Value: nil, Unit: "℃", Quality: model.QualityTimeout},
		},
		Stat: model.Stat{Total: 2, Good: 1, Bad: 1},
	}
	payload, err := EncodeUplink(h, body)
	if err != nil {
		t.Fatalf("造报文: %v", err)
	}

	out, err := PrepareBackfill(payload, 1793500299999)
	if err != nil {
		t.Fatalf("改写: %v", err)
	}
	var env struct {
		Header model.Header   `json:"header"`
		Body   model.DataBody `json:"body"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("解析改写结果: %v", err)
	}
	if env.Header.Backfill != true || env.Header.SendTime != 1793500299999 {
		t.Fatalf("报文头 = %+v，期望 backfill=true 且 sendTime 更新", env.Header)
	}
	if env.Header.CollectTime != collectTime {
		t.Fatalf("collectTime=%d，期望保留 %d", env.Header.CollectTime, collectTime)
	}
	if len(env.Body.YC) != 2 || env.Body.YC[1].Quality != model.QualityTimeout || env.Body.YC[1].Value != nil {
		t.Fatalf("报文体被改动：%+v", env.Body.YC)
	}
	if env.Body.Stat.Total != 2 {
		t.Fatalf("stat.total=%d，期望 2", env.Body.Stat.Total)
	}
}

// newTestBackfill 造一个时钟固定的补传段，连接恒为可用。
func newTestBackfill(t *testing.T, c *Cache, broker *recordingBroker, now time.Time) *Backfill {
	t.Helper()
	b, err := NewBackfill(c, broker, DefaultBackfillSpec(),
		pkglog.New("agent", "ESN-ZX-0001", pkglog.LevelDebug, nil), func() bool { return true })
	if err != nil {
		t.Fatalf("组装补传段: %v", err)
	}
	b.now = func() time.Time { return now }
	return b
}
