package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// 缓存的两个 bucket：报文按入队顺序存，元数据存累计值。
var (
	bucketItems = []byte("items") // key = seq（大端 int64），value = CacheItem 的 JSON
	bucketMeta  = []byte("meta")  // key = 累计丢弃数等
	metaDropped = []byte("dropped")
)

// 三上限的冻结值（接口规范 §9.4）。可由环境变量覆盖，默认值写在这里而不是散落在调用点。
const (
	DefaultCacheMaxDays   = 7
	DefaultCacheMaxItems  = 300000
	DefaultCacheMaxBytes  = 536870912 // 512 MB
	DefaultCacheWarnRatio = 0.8
	DefaultCacheDropBatch = 1000
)

// CacheSpec 是队列的三上限与预警参数。
type CacheSpec struct {
	MaxAge    time.Duration
	MaxItems  int
	MaxBytes  int64
	WarnRatio float64
	// DropBatch 是「每累计多少条丢弃上报一次」，接口规范 §9.4 固定 1000。
	DropBatch int
}

// DefaultCacheSpec 返回冻结值。调用方按环境变量覆盖后再传进 OpenCache。
func DefaultCacheSpec() CacheSpec {
	return CacheSpec{
		MaxAge:    DefaultCacheMaxDays * 24 * time.Hour,
		MaxItems:  DefaultCacheMaxItems,
		MaxBytes:  DefaultCacheMaxBytes,
		WarnRatio: DefaultCacheWarnRatio,
		DropBatch: DefaultCacheDropBatch,
	}
}

// CacheItem 是队列里的一条待上报报文（接口规范 §9.3 第 2 条要求的最小字段集）。
//
// Payload 是已经序列化好的报文，补传时按原样重发——只在发送前改写报文头的
// collectTime / sendTime / backfill 三个字段（见 PrepareBackfill）。
type CacheItem struct {
	Seq           int64  `json:"seq"`
	CollectTime   int64  `json:"collectTime"`
	Topic         string `json:"topic"`
	Payload       []byte `json:"payload"`
	Retain        bool   `json:"retain"`
	FirstFailTime int64  `json:"firstFailTime"`
	RetryCount    int    `json:"retryCount"`
}

// Cache 是本地持久队列。写入在 Append 内完成裁剪，读取按 seq 升序。
//
// 不做业务判定，只管「存住、裁掉、按序取出」三件事。
type Cache struct {
	db   *bolt.DB
	spec CacheSpec
	now  func() time.Time

	dropped    int64 // 累计丢弃（自上次上报起）
	totalBytes int64 // 队列总字节，随写随算，避免每次裁剪都全表扫
	onDrop     func(count int)
	onWarn     func(dimension string)
	warned     map[string]bool
}

// OpenCache 打开（或新建）本地队列。外部资源从参数传入，不在模块内部 new。
func OpenCache(path string, spec CacheSpec) (*Cache, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("打开本地队列 %s: %w", path, err)
	}

	c := &Cache{db: db, spec: spec, now: time.Now, warned: map[string]bool{}}
	if err := c.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

// init 建 bucket 并算出当前占用（重启后 totalBytes 要能对上磁盘上的存量）。
func (c *Cache) init() error {
	return c.db.Update(func(tx *bolt.Tx) error {
		items, err := tx.CreateBucketIfNotExists(bucketItems)
		if err != nil {
			return fmt.Errorf("建队列 bucket: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists(bucketMeta); err != nil {
			return fmt.Errorf("建元数据 bucket: %w", err)
		}
		var total int64
		if err := items.ForEach(func(_, v []byte) error {
			total += int64(len(v))
			return nil
		}); err != nil {
			return fmt.Errorf("统计队列占用: %w", err)
		}
		c.totalBytes = total
		return nil
	})
}

// Close 关闭队列。
func (c *Cache) Close() error { return c.db.Close() }

// OnDropped 注册丢弃回调：每累计 DropBatch 条调用一次，参数是本批丢弃数。
func (c *Cache) OnDropped(fn func(count int)) { c.onDrop = fn }

// OnWarning 注册预警回调：某维度首次跨过 WarnRatio 时调用一次，参数是维度名。
func (c *Cache) OnWarning(fn func(dimension string)) { c.onWarn = fn }

// Append 入队一条报文并裁剪。
//
// 裁剪在上行路径的同一把锁里完成：队列超限时先丢最旧，采集侧不会因为磁盘满而失败。
func (c *Cache) Append(item CacheItem) error {
	if item.FirstFailTime == 0 {
		item.FirstFailTime = c.now().UnixMilli()
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("序列化队列项 seq=%d: %w", item.Seq, err)
	}

	if err := c.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketItems).Put(seqKey(item.Seq), raw); err != nil {
			return fmt.Errorf("写入队列项 seq=%d: %w", item.Seq, err)
		}
		c.totalBytes += int64(len(raw))
		return nil
	}); err != nil {
		return err
	}
	return c.trim()
}

// Peek 取出最多 limit 条，按 seq 升序。补传正序重放靠这个顺序。
func (c *Cache) Peek(limit int) ([]CacheItem, error) {
	var out []CacheItem
	err := c.db.View(func(tx *bolt.Tx) error {
		cur := tx.Bucket(bucketItems).Cursor()
		for k, v := cur.First(); k != nil && (limit <= 0 || len(out) < limit); k, v = cur.Next() {
			var item CacheItem
			if err := json.Unmarshal(v, &item); err != nil {
				return fmt.Errorf("解析队列项 key=%x: %w", k, err)
			}
			out = append(out, item)
		}
		return nil
	})
	return out, err
}

// Remove 出队。只在收到 PUBACK 之后调用（接口规范 §9.3 第 4 条）。
func (c *Cache) Remove(seq int64) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketItems)
		key := seqKey(seq)
		old := b.Get(key)
		if old == nil {
			return nil // 已被裁剪掉，重放时按已出队处理
		}
		c.totalBytes -= int64(len(old))
		return b.Delete(key)
	})
}

// MaxSeq 返回队列里的最大序号。重启续接时与序号文件的高水位取 max。
func (c *Cache) MaxSeq() (int64, error) {
	var max int64
	err := c.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket(bucketItems).Cursor().Last()
		if k == nil {
			return nil
		}
		max = int64(binary.BigEndian.Uint64(k))
		return nil
	})
	return max, err
}

// Stats 是队列当前占用的快照，供健康检查与预警判定使用。
type CacheStats struct {
	Items int
	Bytes int64
	// AgeOfOldest 是最旧一条已滞留多久，时长为 0 表示队列空。
	AgeOfOldest time.Duration
}

// Stats 读一次队列占用。条数与最旧时长要遍历，占用字节用增量值。
func (c *Cache) Stats() (CacheStats, error) {
	st := CacheStats{Bytes: c.totalBytes}
	now := c.now()
	err := c.db.View(func(tx *bolt.Tx) error {
		cur := tx.Bucket(bucketItems).Cursor()
		k, v := cur.First()
		if k == nil {
			return nil
		}
		var oldest CacheItem
		if err := json.Unmarshal(v, &oldest); err != nil {
			return fmt.Errorf("解析队首: %w", err)
		}
		st.AgeOfOldest = now.Sub(time.UnixMilli(oldest.CollectTime))
		for ; k != nil; k, _ = cur.Next() {
			st.Items++
		}
		return nil
	})
	return st, err
}

// trim 执行一次裁剪：三上限任一先到即生效，按 seq 从旧到新丢。
//
// 丢弃必须留痕：每累计 DropBatch 条回调一次（接口规范 §9.4），不许静默。
func (c *Cache) trim() error {
	dropped := 0
	err := c.db.Update(func(tx *bolt.Tx) error {
		n, err := c.trimInTx(tx)
		dropped = n
		return err
	})
	if err != nil {
		return err
	}

	c.reportDropped(dropped)
	c.checkWarning()
	return nil
}

// trimInTx 在同一事务里依次执行三个上限。返回本次丢弃条数。
func (c *Cache) trimInTx(tx *bolt.Tx) (int, error) {
	b := tx.Bucket(bucketItems)
	dropped := 0

	n, err := c.dropExpired(b)
	if err != nil {
		return dropped, err
	}
	dropped += n

	n, err = c.dropOverCount(b)
	if err != nil {
		return dropped, err
	}
	dropped += n

	n, err = c.dropOverBytes(b)
	return dropped + n, err
}

// dropExpired 删超过保留时长的项。seq 与 collectTime 同向，删到第一个未超期即可停。
func (c *Cache) dropExpired(b *bolt.Bucket) (int, error) {
	cutoff := c.now().Add(-c.spec.MaxAge).UnixMilli()
	dropped := 0
	for k, v := b.Cursor().First(); k != nil; k, v = b.Cursor().First() {
		var item CacheItem
		if err := json.Unmarshal(v, &item); err != nil {
			return dropped, fmt.Errorf("解析队列项: %w", err)
		}
		if item.CollectTime > cutoff {
			break
		}
		if err := b.Delete(k); err != nil {
			return dropped, fmt.Errorf("删除超期项: %w", err)
		}
		c.totalBytes -= int64(len(v))
		dropped++
	}
	return dropped, nil
}

// dropOverCount 删到条数不超上限。
func (c *Cache) dropOverCount(b *bolt.Bucket) (int, error) {
	items := b.Stats().KeyN
	dropped := 0
	for items > c.spec.MaxItems {
		k, v := b.Cursor().First()
		if k == nil {
			break
		}
		if err := b.Delete(k); err != nil {
			return dropped, fmt.Errorf("按条数删除: %w", err)
		}
		c.totalBytes -= int64(len(v))
		dropped++
		items--
	}
	return dropped, nil
}

// dropOverBytes 删到占用不超上限。
func (c *Cache) dropOverBytes(b *bolt.Bucket) (int, error) {
	dropped := 0
	for c.totalBytes > c.spec.MaxBytes {
		k, v := b.Cursor().First()
		if k == nil {
			break
		}
		if err := b.Delete(k); err != nil {
			return dropped, fmt.Errorf("按磁盘占用删除: %w", err)
		}
		c.totalBytes -= int64(len(v))
		dropped++
	}
	return dropped, nil
}

// reportDropped 按批上报丢弃：满 DropBatch 条报一次并清零，不足一批继续累计。
func (c *Cache) reportDropped(n int) {
	if n == 0 {
		return
	}
	c.dropped += int64(n)
	batch := c.spec.DropBatch
	if batch <= 0 {
		return
	}
	for c.dropped >= int64(batch) {
		c.dropped -= int64(batch)
		if c.onDrop != nil {
			c.onDrop(batch)
		}
	}
}

// checkWarning 判定三上限是否达预警线。**同级不重复上报**：同一维度只报一次，
// 直到降回线下才允许再报（接口规范 §9.4）。
func (c *Cache) checkWarning() {
	st, err := c.Stats()
	if err != nil {
		return
	}
	ratios := map[string]float64{
		"items": float64(st.Items) / float64(c.spec.MaxItems),
		"bytes": float64(st.Bytes) / float64(c.spec.MaxBytes),
		"age":   st.AgeOfOldest.Seconds() / c.spec.MaxAge.Seconds(),
	}
	for _, dim := range []string{"items", "bytes", "age"} {
		over := ratios[dim] >= c.spec.WarnRatio
		if over && !c.warned[dim] {
			c.warned[dim] = true
			if c.onWarn != nil {
				c.onWarn(dim)
			}
		}
		if !over {
			c.warned[dim] = false
		}
	}
}

// seqKey 把序号编成大端 key：bolt 按字节序排列，大端才能保证数值顺序与遍历顺序一致。
func seqKey(seq int64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(seq))
	return buf[:]
}
