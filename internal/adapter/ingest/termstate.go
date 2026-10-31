package ingest

import (
	"sort"
	"sync"
	"time"
)

// 终端状态取值，与 terminal_state.status 的 CHECK 约束一致。
const (
	StatusOnline  = "ONLINE"
	StatusOffline = "OFFLINE"
	StatusUnknown = "UNKNOWN"
)

// 状态原因的固定取值。写进 offline_reason，供界面与排障使用。
const (
	ReasonConnected            = "CONNECTED"
	ReasonDataReceived         = "DATA_RECEIVED"
	ReasonDataTimeout          = "DATA_TIMEOUT"
	ReasonUnexpectedDisconnect = "UNEXPECTED_DISCONNECT"
	ReasonGracefulShutdown     = "GRACEFUL_SHUTDOWN"
	ReasonNoDataYet            = "NO_DATA_YET"
)

// 设备事件类型。
const (
	EventSensorOffline   = "SENSOR_OFFLINE"
	EventSensorRecovered = "SENSOR_RECOVERED"
	EventTerminalOffline = "TERMINAL_OFFLINE"
	EventTerminalOnline  = "TERMINAL_ONLINE"
)

// TermStateConfig 是状态判定参数。全部由调用方注入，本包不读配置。
type TermStateConfig struct {
	// OfflinePeriods 是判数据超时的周期数，接口规范 §6.5 固定为 3。
	OfflinePeriods int
	// CollectInterval 是采集周期，来自终端点表 collectIntervalMs。
	CollectInterval time.Duration
	// DeviceFailureLimit 是判设备离线的连续失败次数，BR-28 固定为 5。
	DeviceFailureLimit int
}

// Transition 是一次状态判定的结果。
//
// Changed 表示状态与上一次不同，只有变化才写 terminal_state 与 device_event——
// 每次上报都刷一行会让状态表退化成流水表。
type Transition struct {
	Status  string
	Reason  string
	Changed bool
}

// DeviceTransition 是设备状态判定的结果。
type DeviceTransition struct {
	Offline   bool
	Recovered bool
	Changed   bool
}

// TermState 是终端与设备状态的内存判定器。
//
// 单写者：由 cmd 在订阅回调里串行调用，内部仍加锁，避免巡检协程与回调并发写时数据竞争。
type TermState struct {
	cfg TermStateConfig

	mu       sync.Mutex
	terminal map[string]*terminalEntry
	devices  map[string]*deviceEntry
}

type terminalEntry struct {
	status   string
	reason   string
	lastData time.Time
	seen     time.Time
}

type deviceEntry struct {
	failures int
	offline  bool
}

// NewTermState 组装状态判定器。
func NewTermState(cfg TermStateConfig) *TermState {
	if cfg.OfflinePeriods <= 0 {
		cfg.OfflinePeriods = 3
	}
	if cfg.DeviceFailureLimit <= 0 {
		cfg.DeviceFailureLimit = 5
	}
	return &TermState{
		cfg:      cfg,
		terminal: map[string]*terminalEntry{},
		devices:  map[string]*deviceEntry{},
	}
}

// ApplyStatus 处理 STATUS 报文（含 LWT 与优雅退出）。
//
// 显式倒线的权重最高：无论数据多新，收到 OFFLINE 立刻判离线。
func (t *TermState) ApplyStatus(terminalID, status, reason string, statusTime, receiveTime time.Time) Transition {
	t.mu.Lock()
	defer t.mu.Unlock()

	e := t.entry(terminalID)
	e.seen = receiveTime
	if status == StatusOffline {
		return t.setLocked(e, StatusOffline, reason)
	}
	return t.setLocked(e, StatusOnline, ReasonConnected)
}

// ApplyData 处理 DATA 报文。
//
// 补传数据不推进 lastData，也不改变状态：断连期间的数据批量到达时若参与推进，
// 会把已经判定的离线事件抹掉，而那条离线事件正是需要留证的。
func (t *TermState) ApplyData(terminalID string, collectTime time.Time, backfill bool, receiveTime time.Time) Transition {
	t.mu.Lock()
	defer t.mu.Unlock()

	e := t.entry(terminalID)
	e.seen = receiveTime
	if backfill {
		return Transition{Status: e.status, Reason: e.reason}
	}
	if collectTime.After(e.lastData) {
		e.lastData = collectTime
	}
	return t.setLocked(e, StatusOnline, ReasonDataReceived)
}

// Sweep 巡检单台终端：超时判离线，否则维持原状。
//
// 只在 ONLINE 时检查，OFFLINE 不因为「时间过了更久」再判一次——
// 否则每次巡检都会产生一条重复的离线事件。
func (t *TermState) Sweep(terminalID string, now time.Time) Transition {
	t.mu.Lock()
	defer t.mu.Unlock()

	e := t.entry(terminalID)
	if e.status != StatusOnline || e.lastData.IsZero() {
		return Transition{Status: e.status, Reason: e.reason}
	}
	if now.Sub(e.lastData) <= time.Duration(t.cfg.OfflinePeriods)*t.cfg.CollectInterval {
		return Transition{Status: e.status, Reason: e.reason}
	}
	return t.setLocked(e, StatusOffline, ReasonDataTimeout)
}

// Snapshot 返回当前全部终端状态的快照，供巡检协程遍历。
func (t *TermState) Snapshot() []TermSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]TermSnapshot, 0, len(t.terminal))
	for id, e := range t.terminal {
		out = append(out, TermSnapshot{
			TerminalID: id, Status: e.status, Reason: e.reason,
			LastData: e.lastData, LastSeen: e.seen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TerminalID < out[j].TerminalID })
	return out
}

// SnapshotOf 返回单台终端的当前状态快照。终端未知时返回零值。
func (t *TermState) SnapshotOf(terminalID string) TermSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.terminal[terminalID]
	if !ok {
		return TermSnapshot{TerminalID: terminalID, Status: StatusUnknown, Reason: ReasonNoDataYet}
	}
	return TermSnapshot{
		TerminalID: terminalID, Status: e.status, Reason: e.reason,
		LastData: e.lastData, LastSeen: e.seen,
	}
}

// TermSnapshot 是 terminal_state 一行的投影。
type TermSnapshot struct {
	TerminalID string
	Status     string
	Reason     string
	LastData   time.Time
	LastSeen   time.Time
}

func (t *TermState) entry(terminalID string) *terminalEntry {
	e, ok := t.terminal[terminalID]
	if !ok {
		e = &terminalEntry{status: StatusUnknown, reason: ReasonNoDataYet}
		t.terminal[terminalID] = e
	}
	return e
}

func (t *TermState) setLocked(e *terminalEntry, status, reason string) Transition {
	changed := e.status != status
	e.status = status
	e.reason = reason
	return Transition{Status: status, Reason: reason, Changed: changed}
}

// DeviceFailure 记一次采集失败，返回是否触发设备离线。
//
// 补传期间的历史失败不计入：那些失败在当时没被上报，恢复后才批量补传，
// 计入会让恢复瞬间立刻判离线，正好与事实相反。
func (t *TermState) DeviceFailure(terminalID, busID string, slaveAddr int, backfill bool) DeviceTransition {
	if backfill {
		return DeviceTransition{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	key := deviceKey(terminalID, busID, slaveAddr)
	d := t.device(key)
	changed := false
	if !d.offline {
		d.failures++
		if d.failures >= t.cfg.DeviceFailureLimit {
			d.offline = true
			changed = true
		}
	}
	return DeviceTransition{Offline: d.offline, Changed: changed}
}

// DeviceSuccess 记一次采集成功。离线设备首次成功即恢复，产生恢复事件。
func (t *TermState) DeviceSuccess(terminalID, busID string, slaveAddr int) DeviceTransition {
	t.mu.Lock()
	defer t.mu.Unlock()

	d := t.device(deviceKey(terminalID, busID, slaveAddr))
	recovered := d.offline
	d.offline = false
	d.failures = 0
	return DeviceTransition{Recovered: recovered, Changed: recovered}
}

// MarkDeviceOffline 依据终端上报的 SENSOR_OFFLINE 事件把设备置为离线。
//
// 不经失败计数：事件本身就代表「连续 5 次失败」已经成立，再数一遍等于把
// 同一个判定做两次，恢复后还会因为计数残留立刻判离线。
func (t *TermState) MarkDeviceOffline(terminalID, busID string, slaveAddr int) DeviceTransition {
	t.mu.Lock()
	defer t.mu.Unlock()

	d := t.device(deviceKey(terminalID, busID, slaveAddr))
	if d.offline {
		return DeviceTransition{Offline: true}
	}
	d.offline = true
	d.failures = t.cfg.DeviceFailureLimit
	return DeviceTransition{Offline: true, Changed: true}
}

// MarkDeviceRecovered 依据 SENSOR_RECOVERED 事件把设备置回在线。
func (t *TermState) MarkDeviceRecovered(terminalID, busID string, slaveAddr int) DeviceTransition {
	t.mu.Lock()
	defer t.mu.Unlock()

	d := t.device(deviceKey(terminalID, busID, slaveAddr))
	recovered := d.offline
	d.offline = false
	d.failures = 0
	return DeviceTransition{Recovered: recovered, Changed: recovered}
}

// DeviceOffline 返回设备当前是否处于离线。
func (t *TermState) DeviceOffline(terminalID, busID string, slaveAddr int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.devices[deviceKey(terminalID, busID, slaveAddr)]
	return ok && d.offline
}

func (t *TermState) device(key string) *deviceEntry {
	d, ok := t.devices[key]
	if !ok {
		d = &deviceEntry{}
		t.devices[key] = d
	}
	return d
}

func deviceKey(terminalID, busID string, slaveAddr int) string {
	return terminalID + "|" + busID + "|" + itoa(slaveAddr)
}
