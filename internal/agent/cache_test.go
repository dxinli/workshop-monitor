package agent

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// 队列裁剪的 seam（CLAUDE.md Testing 表）：三上限先到、FIFO、丢弃计数。
// 期望值全部是手算字面量，不引用实现里的常量。

// testCacheLimits 给测试用的上限：远比冻结值小，好让几条数据就跨过阈值。
func testCacheLimits(maxItems int, maxAge time.Duration, maxBytes int64) CacheSpec {
	return CacheSpec{
		MaxAge:    maxAge,
		MaxItems:  maxItems,
		MaxBytes:  maxBytes,
		WarnRatio: 0.8,
		DropBatch: 1000,
	}
}

func openTestCache(t *testing.T, spec CacheSpec, now time.Time) *Cache {
	t.Helper()
	c, err := OpenCache(filepath.Join(t.TempDir(), "cache.db"), spec)
	if err != nil {
		t.Fatalf("打开缓存: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.now = func() time.Time { return now }
	return c
}

// itemAt 造一条缓存项：seq 递增、采集时间为「now 往前 ago」。
func itemAt(seq int64, now time.Time, ago time.Duration, size int) CacheItem {
	return CacheItem{
		Seq:           seq,
		CollectTime:   now.Add(-ago).UnixMilli(),
		Topic:         "/edge/ESN-ZX-0001/data/up",
		Payload:       make([]byte, size),
		FirstFailTime: now.UnixMilli(),
	}
}

// TestTrimByItemCount 条数上限：8 条超过 5 条，丢最旧的 3 条，留最新 5 条。
func TestTrimByItemCount(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := openTestCache(t, testCacheLimits(5, 7*24*time.Hour, 1<<30), now)

	for seq := int64(1); seq <= 8; seq++ {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}

	got, err := c.Peek(100)
	if err != nil {
		t.Fatalf("取出: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("裁剪后剩 %d 条，期望 5 条", len(got))
	}
	// FIFO 丢最旧 → 留下的是 4,5,6,7,8，顺序升序。
	want := []int64{4, 5, 6, 7, 8}
	for i, w := range want {
		if got[i].Seq != w {
			t.Fatalf("第 %d 条 seq=%d，期望 %d（应丢最旧的 1~3）", i, got[i].Seq, w)
		}
	}
}

// TestTrimByAge 时长上限：7 天前的项被删，未超期的留着。
func TestTrimByAge(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := openTestCache(t, testCacheLimits(300000, 7*24*time.Hour, 1<<30), now)

	// 手算：8 天前 → 删；7 天零 1 分钟前 → 删；6 天前、刚刚 → 留。
	items := []CacheItem{
		itemAt(1, now, 8*24*time.Hour, 10),
		itemAt(2, now, 7*24*time.Hour+time.Minute, 10),
		itemAt(3, now, 6*24*time.Hour, 10),
		itemAt(4, now, 0, 10),
	}
	for _, it := range items {
		if err := c.Append(it); err != nil {
			t.Fatalf("入队 seq=%d: %v", it.Seq, err)
		}
	}

	got, err := c.Peek(100)
	if err != nil {
		t.Fatalf("取出: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("剩余 seq = %v，期望 [3 4]（1、2 超过 7 天）", seqsOf(got))
	}
}

// itemFootprint 量一条缓存项在队列里的实际占用。
//
// 不能拿 payload 长度当占用：存进去的是整条 JSON（含 topic、时间戳、seq），
// 比 payload 大一截。先量一次，再据此手算「这些字节能装几条」。
func itemFootprint(t *testing.T, now time.Time, payloadSize int) int64 {
	t.Helper()
	c := openTestCache(t, testCacheLimits(1000, 7*24*time.Hour, 1<<40), now)
	if err := c.Append(itemAt(1, now, 0, payloadSize)); err != nil {
		t.Fatalf("量占用: %v", err)
	}
	st, err := c.Stats()
	if err != nil {
		t.Fatalf("读占用: %v", err)
	}
	if st.Items != 1 {
		t.Fatalf("量占用后队列里有 %d 条，期望 1 条", st.Items)
	}
	return st.Bytes
}

// TestTrimByBytes 磁盘上限：量出一条的占用 one，上限设成 4.5 条，
// 就只该留 4 条（4×one ≤ 上限 < 5×one）。
func TestTrimByBytes(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	one := itemFootprint(t, now, 100)
	limit := one * 9 / 2 // 4.5 条

	c := openTestCache(t, testCacheLimits(300000, 7*24*time.Hour, limit), now)
	for seq := int64(1); seq <= 10; seq++ {
		if err := c.Append(itemAt(seq, now, 0, 100)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}

	got, err := c.Peek(100)
	if err != nil {
		t.Fatalf("取出: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("裁剪后剩 %d 条，期望 4 条（上限 4.5 条）", len(got))
	}
	if got[0].Seq != 7 || got[3].Seq != 10 {
		t.Fatalf("剩余 seq = %v，期望 [7 8 9 10]（FIFO 丢最旧）", seqsOf(got))
	}
}

// TestTrimTakesWhicheverLimitHitsFirst 三上限任一先到即生效：
// 条数上限放得很松、字节上限只够 2.5 条，就只按字节裁；反过来只按条数裁。
func TestTrimTakesWhicheverLimitHitsFirst(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	one := itemFootprint(t, now, 100)

	byBytes := openTestCache(t, testCacheLimits(1000, 7*24*time.Hour, one*5/2), now)
	for seq := int64(1); seq <= 6; seq++ {
		if err := byBytes.Append(itemAt(seq, now, 0, 100)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}
	got, _ := byBytes.Peek(100)
	if len(got) != 2 {
		t.Fatalf("按字节上限应剩 2 条（上限 2.5 条），实际 %d 条", len(got))
	}

	byItems := openTestCache(t, testCacheLimits(3, 7*24*time.Hour, 1<<40), now)
	for seq := int64(1); seq <= 6; seq++ {
		if err := byItems.Append(itemAt(seq, now, 0, 100)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}
	got2, _ := byItems.Peek(100)
	if len(got2) != 3 {
		t.Fatalf("按条数上限应剩 3 条，实际 %d 条", len(got2))
	}
}

// TestTrimReportsDroppedBatchwise 丢弃不得静默：每满 DropBatch 条上报一次，
// 计数清零重新累计。这里把批次设成 5，便于用 12 次丢弃验出两次上报。
func TestTrimReportsDroppedBatchwise(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	spec := testCacheLimits(1, 7*24*time.Hour, 1<<30)
	spec.DropBatch = 5

	var reports []int
	c := openTestCache(t, spec, now)
	c.OnDropped(func(n int) { reports = append(reports, n) })

	// 每次入队都会把上一条挤掉，共丢 12 次。
	for seq := int64(1); seq <= 13; seq++ {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}

	// 第 5、第 10 次丢弃各报一次，每次报的累计值是 5；剩 2 次不足一批，不报。
	if len(reports) != 2 {
		t.Fatalf("丢弃上报 %d 次，期望 2 次（12 次丢弃、每 5 次一批）", len(reports))
	}
	for i, n := range reports {
		if n != 5 {
			t.Fatalf("第 %d 次上报 droppedCount=%d，期望 5", i+1, n)
		}
	}
}

// TestTrimWarnsOncePerDimension 达 80% 上报一次 CACHE_WARNING，同级不重复。
func TestTrimWarnsOncePerDimension(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	// 条数上限 10，80% 即 8 条。
	spec := testCacheLimits(10, 7*24*time.Hour, 1<<30)

	var warnings []string
	c := openTestCache(t, spec, now)
	c.OnWarning(func(dimension string) { warnings = append(warnings, dimension) })

	for seq := int64(1); seq <= 10; seq++ {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}
	// 到 8 条时跨过 80%，之后 9、10 条都还在 80% 以上——只该报一次。
	if len(warnings) != 1 {
		t.Fatalf("CACHE_WARNING 报了 %d 次，期望 1 次（同级不重复）", len(warnings))
	}
	if warnings[0] != "items" {
		t.Fatalf("预警维度 = %q，期望 items", warnings[0])
	}
}

// TestPeekIsFifoAndRemoveBySeq 验出入队顺序与按 seq 出队（补传要按 seq 正序重放）。
func TestPeekIsFifoAndRemoveBySeq(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	c := openTestCache(t, testCacheLimits(100, 7*24*time.Hour, 1<<30), now)

	for seq := int64(5); seq >= 3; seq-- {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}

	got, err := c.Peek(2)
	if err != nil {
		t.Fatalf("取出: %v", err)
	}
	// 入队顺序是 5,4,3，Peek 必须按 seq 升序给出 3,4——补传正序靠它。
	if len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("Peek(2) = %v，期望按 seq 升序 [3 4]", seqsOf(got))
	}

	if err := c.Remove(3); err != nil {
		t.Fatalf("出队 seq=3: %v", err)
	}
	got, _ = c.Peek(100)
	if len(got) != 2 || got[0].Seq != 4 {
		t.Fatalf("出队后 = %v，期望 [4 5]", seqsOf(got))
	}
}

// TestMaxSeqSurvivesReopen 序号高水位跨重启保留：重启后仍能读到队列里的最大 seq，
// 006 的分配器要靠它与文件高水位取 max 来续接。
func TestMaxSeqSurvivesReopen(t *testing.T) {
	now := time.UnixMilli(1793500200123)
	path := filepath.Join(t.TempDir(), "cache.db")

	c, err := OpenCache(path, testCacheLimits(100, 7*24*time.Hour, 1<<30))
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	c.now = func() time.Time { return now }
	for _, seq := range []int64{7, 11, 9} {
		if err := c.Append(itemAt(seq, now, 0, 10)); err != nil {
			t.Fatalf("入队 seq=%d: %v", seq, err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	re, err := OpenCache(path, testCacheLimits(100, 7*24*time.Hour, 1<<30))
	if err != nil {
		t.Fatalf("重开: %v", err)
	}
	defer func() { _ = re.Close() }()

	got, err := re.MaxSeq()
	if err != nil {
		t.Fatalf("读最大序号: %v", err)
	}
	if got != 11 {
		t.Fatalf("重开后最大序号 = %d，期望 11", got)
	}
}

func seqsOf(items []CacheItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.Seq)
	}
	return out
}

var _ = fmt.Sprintf
