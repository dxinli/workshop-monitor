package evaluate

import (
	"sort"
	"strings"
)

// 作用域权重。数值越大越具体：测点 > 总线 > 全局（接口规范 §11.1 的配置优先级）。
const (
	scopeGlobal = 1
	scopeBus    = 2
	scopePoint  = 3
)

// Rule 是一条已展开到作用域的阈值规则。
//
// 空字符串表示该维度不限定：TerminalID 为空即适用全部终端。
// 三个维度一起决定作用域，因此不再单设 Scope 字段——多存一份就会多一份不一致的机会。
type Rule struct {
	AlarmType  string
	TerminalID string
	BusID      string
	PointCode  string
	Operator   string
	Value      float64
	// HasValue 为 false 表示没有阈值（状态报警不配数值），按「非零即命中」判定。
	HasValue bool
	Level    int
	Debounce int
}

// matches 判断规则是否覆盖这个测点。
func (r Rule) matches(terminalID, busID, pointCode string) bool {
	if r.TerminalID != "" && r.TerminalID != terminalID {
		return false
	}
	if r.BusID != "" && r.BusID != busID {
		return false
	}
	if r.PointCode != "" && r.PointCode != pointCode {
		return false
	}
	return true
}

// specificity 返回作用域权重。
func (r Rule) specificity() int {
	switch {
	case r.PointCode != "":
		return scopePoint
	case r.BusID != "":
		return scopeBus
	default:
		return scopeGlobal
	}
}

// hit 判断一次读数是否命中阈值。
func (r Rule) hit(v float64) bool {
	if !r.HasValue {
		return v != 0
	}
	switch r.Operator {
	case ">":
		return v > r.Value
	case ">=":
		return v >= r.Value
	case "<":
		return v < r.Value
	case "<=":
		return v <= r.Value
	default:
		// V-11 已把运算符限制在四个之内；真出现别的值只能判不命中，
		// 猜一个方向会让错误配置变成误报。
		return false
	}
}

// dedupeKey 是告警去重键：终端|测点|告警类型（接口规范 §11.4）。
func dedupeKey(terminalID, pointID, alarmType string) string {
	return strings.Join([]string{terminalID, pointID, alarmType}, "|")
}

// Snapshot 是一份不可变的阈值快照。热加载整体替换，不逐条改。
//
// 逐条就地更新会让同一次求值读到新旧混杂的规则，这类瞬态误判最难排查。
type Snapshot struct {
	rules []Rule
}

// newSnapshot 复制一份规则，避免调用方之后改动切片影响已发布的快照。
func newSnapshot(rules []Rule) *Snapshot {
	return &Snapshot{rules: append([]Rule(nil), rules...)}
}

// forPoint 返回该测点生效的规则，每个告警类型只留最具体的那条。
//
// 同一告警类型在测点与总线各配一条时，测点级覆盖总线级而不是两条都算：
// 两条都算会共用同一个去重键，一次命中拉响两次。
func (s *Snapshot) forPoint(terminalID, busID, pointCode string) []Rule {
	best := make(map[string]Rule, 4)
	score := make(map[string]int, 4)

	for _, r := range s.rules {
		if !r.matches(terminalID, busID, pointCode) {
			continue
		}
		if cur, ok := score[r.AlarmType]; ok && r.specificity() <= cur {
			continue
		}
		best[r.AlarmType] = r
		score[r.AlarmType] = r.specificity()
	}

	out := make([]Rule, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	// 固定顺序：同一次求值里告警类型的处理次序不能随 map 迭代变化。
	// 按级别从高到低，是为了让同一次求值中更严重的规则先拉响——
	// 反过来的话高限会先拉响一次、随即被高高限闭合，白白产生一条告警与一次通知。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level > out[j].Level
		}
		return out[i].AlarmType < out[j].AlarmType
	})
	return out
}
