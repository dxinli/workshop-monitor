package state

// Entry 是一条活动告警的摘要。
type Entry struct {
	DedupeKey  string
	TerminalID string
	PointID    string
	Level      int
}

// Active 是活动告警集合。
//
// 未恢复期间不重复拉响靠它判断，不查库；库里的 ux_alarm_active 是第二道防线。
// 启动时必须先从库重建，否则订阅后到达的数据会对已告警测点重复拉响。
type Active struct {
	m map[string]Entry
}

// NewActive 建一个空集合。
func NewActive() *Active { return &Active{m: map[string]Entry{}} }

// Add 记入一条活动告警。同一键重复加入取后写的那条，不产生重复。
func (a *Active) Add(e Entry) { a.m[e.DedupeKey] = e }

// Remove 移出活动告警。
func (a *Active) Remove(key string) { delete(a.m, key) }

// Has 判断去重键是否仍在活动。
func (a *Active) Has(key string) bool {
	_, ok := a.m[key]
	return ok
}

// HighestLevelOnPoint 返回某测点上活动告警的最高级别，没有活动告警时为 0。
//
// 高高限触发时要用它避免低限告警继续叠加：只有比它更高级别的规则才允许再拉响。
func (a *Active) HighestLevelOnPoint(terminalID, pointID string) int {
	highest := 0
	for _, e := range a.m {
		if e.TerminalID == terminalID && e.PointID == pointID && e.Level > highest {
			highest = e.Level
		}
	}
	return highest
}

// Len 返回活动告警条数，供健康检查比对库中未恢复数。
func (a *Active) Len() int { return len(a.m) }

// Replace 用从库重建的集合整体替换。启动时调用一次。
func (a *Active) Replace(entries []Entry) {
	a.m = make(map[string]Entry, len(entries))
	for _, e := range entries {
		a.m[e.DedupeKey] = e
	}
}
