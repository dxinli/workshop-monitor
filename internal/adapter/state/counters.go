package state

// Counters 是去抖计数。命中与未命中分开累计：
// 触发要连续 N 次命中，恢复要连续 N 次未命中，两者对称。
type Counters struct {
	m map[string]counter
}

type counter struct {
	hits   int
	misses int
}

// NewCounters 建一个空计数表。
func NewCounters() *Counters { return &Counters{m: map[string]counter{}} }

// Hit 记一次命中，返回连续命中次数，并把未命中计数清零。
func (c *Counters) Hit(key string) int {
	v := c.m[key]
	v.hits++
	v.misses = 0
	c.m[key] = v
	return v.hits
}

// Miss 记一次未命中，返回连续未命中次数，并把命中计数清零。
func (c *Counters) Miss(key string) int {
	v := c.m[key]
	v.misses++
	v.hits = 0
	c.m[key] = v
	return v.misses
}

// Reset 清掉一个键的计数。告警恢复或被更高级别告警吸收时调用。
func (c *Counters) Reset(key string) { delete(c.m, key) }

// Len 返回正在计数的键数，供健康检查观察。
func (c *Counters) Len() int { return len(c.m) }
