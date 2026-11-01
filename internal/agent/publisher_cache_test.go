package agent

import (
	"encoding/json"
	"testing"
	"time"

	"workshop-monitor/internal/model"
	"workshop-monitor/internal/pkglog"
)

// alwaysUpLink 是一个始终「连得上」的发布通道，用来隔离出「出队凭据」这一件事。
type alwaysUpLink struct{ broker *recordingBroker }

func (l alwaysUpLink) Publish(topic string, payload []byte, retain bool) error {
	return l.broker.Publish(topic, payload, retain)
}
func (l alwaysUpLink) Connected() bool { return true }

// newCachedUplink 组装一个挂了本地队列的上报段。
func newCachedUplink(t *testing.T, c *Cache, broker *recordingBroker) *Uplink {
	t.Helper()
	alloc, err := LoadSeqAllocator("")
	if err != nil {
		t.Fatalf("序号分配器: %v", err)
	}
	u, err := NewUplink(UplinkConfig{
		Esn:    "ESN-ZX-0001",
		Topics: UplinkTopics{Data: "/edge/ESN-ZX-0001/data/up", Event: "/edge/ESN-ZX-0001/event/up", Status: "/edge/ESN-ZX-0001/status/up"},
		Alloc:  alloc,
		Link:   alwaysUpLink{broker: broker},
		Log:    pkglog.New("agent", "ESN-ZX-0001", pkglog.LevelDebug, nil),
		Cache:  c,
	})
	if err != nil {
		t.Fatalf("组装上报段: %v", err)
	}
	return u
}

// TestFailedPublishEntersCache 验 BR-19 的落点：发布失败就进本地队列，不丢。
func TestFailedPublishEntersCache(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	broker := &recordingBroker{failTimes: 1}
	u := newCachedUplink(t, c, broker)

	if err := u.SendBatch(goodBatch(now.UnixMilli())); err == nil {
		t.Fatal("发布失败时应返回错误")
	}

	items, err := c.Peek(10)
	if err != nil {
		t.Fatalf("读队列: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("发布失败后队列里有 %d 条，期望 1 条", len(items))
	}
	if items[0].Topic != "/edge/ESN-ZX-0001/data/up" {
		t.Fatalf("入队 topic=%q，期望数据主题", items[0].Topic)
	}
	if items[0].Seq != 1 {
		t.Fatalf("入队 seq=%d，期望 1", items[0].Seq)
	}
}

// TestDeviceFailuresCountedEvenWhenBrokerDown 验断网期间设备失败照常计数。
//
// 这是 006 留下的缺陷：SendBatch 在数据发不出去时提前 return，失败计数根本不推进，
// 断网期间永远攒不出 SENSOR_OFFLINE。有了本地队列，数据入队之后事件判定必须继续。
func TestDeviceFailuresCountedEvenWhenBrokerDown(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	broker := &recordingBroker{failTimes: 1000} // Broker 全程不可用
	u := newCachedUplink(t, c, broker)

	failed := Batch{
		TerminalID: "ESN-ZX-0001", BusID: "A", CollectTime: now.UnixMilli(),
		Points: []PointReading{{Code: "motor_temp", SlaveAddr: 1, SiType: "YC",
			Quality: model.QualityTimeout}},
		Bad: 1,
	}
	for i := 0; i < 5; i++ {
		_ = u.SendBatch(failed)
	}

	st := u.watch.state[deviceKey{busID: "A", slave: 1}]
	if st == nil {
		t.Fatal("断网期间设备失败没有被计数")
	}
	if st.failures < 5 || !st.offline {
		t.Fatalf("5 轮连续失败后 failures=%d offline=%v，期望 ≥5 且已判离线", st.failures, st.offline)
	}

	// 离线事件本身也要落盘，否则恢复联网后平台不知道这台设备曾离线。
	items, _ := c.Peek(100)
	hasEvent := false
	for _, it := range items {
		if it.Topic == "/edge/ESN-ZX-0001/event/up" {
			hasEvent = true
		}
	}
	if !hasEvent {
		t.Fatalf("队列里 %d 条，缺少 SENSOR_OFFLINE 事件（断网期间的离线必须攒下来）", len(items))
	}
}

// TestSuccessfulPublishDoesNotEnterCache 验正常路径不入队，避免队列白占磁盘。
func TestSuccessfulPublishDoesNotEnterCache(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	broker := &recordingBroker{}
	u := newCachedUplink(t, c, broker)

	if err := u.SendBatch(goodBatch(now.UnixMilli())); err != nil {
		t.Fatalf("上报: %v", err)
	}
	items, _ := c.Peek(10)
	if len(items) != 0 {
		t.Fatalf("上报成功后队列里不应有货，实际 %d 条", len(items))
	}
}

// TestCacheDropPublishesEvent 验 BR-21：丢弃攒够一批就上报 CACHE_DROPPED。
func TestCacheDropPublishesEvent(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	c.spec.MaxItems = 1
	c.spec.DropBatch = 3

	broker := &recordingBroker{}
	u := newCachedUplink(t, c, broker)
	u.WireCacheEvents()

	// 每条入队都挤掉上一条：入 4 条丢 3 次，正好满一批。
	for seq := int64(1); seq <= 4; seq++ {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}

	found := false
	for _, m := range broker.snapshot() {
		if m.topic == "/edge/ESN-ZX-0001/event/up" &&
			containsEventType(t, m.payload, "CACHE_DROPPED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("丢弃 3 条后没有上报 CACHE_DROPPED，共发出 %d 条", broker.count())
	}
}

// TestCacheWarnPublishesEvent 验 BR-22：某维度达 80% 上报 CACHE_WARNING，同级不重复。
func TestCacheWarnPublishesEvent(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	c.spec.MaxItems = 10
	c.spec.WarnRatio = 0.8

	broker := &recordingBroker{}
	u := newCachedUplink(t, c, broker)
	u.WireCacheEvents()

	for seq := int64(1); seq <= 10; seq++ {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}

	warns := 0
	for _, m := range broker.snapshot() {
		if m.topic == "/edge/ESN-ZX-0001/event/up" &&
			containsEventType(t, m.payload, "CACHE_WARNING") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("CACHE_WARNING 发了 %d 次，期望 1 次（同级不重复）", warns)
	}
}

// TestSeedsAllocatorFromCache 验 006 的交接条件：序号续接取 max(文件高水位, 队列最大 seq)。
func TestSeedsAllocatorFromCache(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	for _, seq := range []int64{3, 40, 17} {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队: %v", err)
		}
	}

	// 文件高水位低于队列：应取队列的 40。
	alloc, err := LoadSeqAllocator("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alloc.Next(); err != nil { // 文件高水位 = 1
		t.Fatal(err)
	}
	if err := alloc.ResumeTo(mustMaxSeq(t, c)); err != nil {
		t.Fatalf("续接: %v", err)
	}
	next, err := alloc.Next()
	if err != nil {
		t.Fatalf("续接后分配: %v", err)
	}
	if next != 41 {
		t.Fatalf("续接后首个序号 = %d，期望 41（队列最大 40 之后）", next)
	}
}

// TestSeedsAllocatorKeepsFileWhenHigher 验反向：文件高水位高于队列时不能倒退。
func TestSeedsAllocatorKeepsFileWhenHigher(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	if err := c.Append(itemAt(5, now, 0, 10)); err != nil {
		t.Fatal(err)
	}

	alloc, err := LoadSeqAllocator("")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ { // 文件高水位推到 100
		if _, err := alloc.Next(); err != nil {
			t.Fatal(err)
		}
	}
	if err := alloc.ResumeTo(mustMaxSeq(t, c)); err != nil {
		t.Fatalf("续接: %v", err)
	}
	next, _ := alloc.Next()
	if next != 101 {
		t.Fatalf("续接后首个序号 = %d，期望 101（文件高水位 100 更高，不得倒退）", next)
	}
}

func mustMaxSeq(t *testing.T, c *Cache) int64 {
	t.Helper()
	max, err := c.MaxSeq()
	if err != nil {
		t.Fatalf("读队列最大序号: %v", err)
	}
	return max
}

// containsEventType 判断事件报文的 body.eventType。
func containsEventType(t *testing.T, payload []byte, want string) bool {
	t.Helper()
	var env struct {
		Body model.EventBody `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return false
	}
	return env.Body.EventType == want
}

// TestStatusReportsRealCacheDepth 验状态报文里的 cacheDepth / cacheBytes 取自真实队列，
// 而不是像 006 那样固定报 0——平台侧看缓存积压就靠这两个字段。
func TestStatusReportsRealCacheDepth(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := cacheAt(t, now)
	broker := &recordingBroker{}
	u := newCachedUplink(t, c, broker)

	for seq := int64(1); seq <= 3; seq++ {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队: %v", err)
		}
	}
	if err := u.SendStatus(StatusOnline, ReasonConnected); err != nil {
		t.Fatalf("发状态: %v", err)
	}

	var env struct {
		Body model.StatusBody `json:"body"`
	}
	if err := json.Unmarshal(broker.snapshot()[0].payload, &env); err != nil {
		t.Fatalf("解析状态报文: %v", err)
	}
	if env.Body.CacheDepth != 3 {
		t.Fatalf("cacheDepth=%d，期望 3", env.Body.CacheDepth)
	}
	if env.Body.CacheBytes <= 0 {
		t.Fatalf("cacheBytes=%d，期望大于 0", env.Body.CacheBytes)
	}
}
