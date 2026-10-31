package agent

import (
	"fmt"
	"sort"

	"workshop-monitor/internal/model"
)

// 设备事件类型（接口规范 §6.4）。缓存类事件归 007；配置错误事件发生在拒绝启动时，
// 那时还没连上 Broker，无法上报。
const (
	EventSensorOffline   = "SENSOR_OFFLINE"
	EventSensorRecovered = "SENSOR_RECOVERED"
)

const (
	reasonContinuousTimeout   = "CONTINUOUS_TIMEOUT"
	reasonContinuousException = "CONTINUOUS_DEVICE_EXCEPTION"
	reasonContinuousCRC       = "CONTINUOUS_CRC_ERROR"
	reasonContinuousParse     = "CONTINUOUS_PARSE_ERROR"
	reasonRecovered           = "SUCCESS"
)

// defaultOfflineThreshold 是判设备离线的连续失败次数（接口规范 §6.4 表，附录 B 的
// SENSOR_OFFLINE_THRESHOLD 默认值 5）。
const defaultOfflineThreshold = 5

type deviceKey struct {
	busID string
	slave int
}

type deviceState struct {
	failures int
	offline  bool
}

// deviceWatch 按（总线, 从站）统计连续采集失败次数：跨过阈值报一次离线，
// 离线后首次成功报一次恢复。这是计数，不是时间窗口——边缘不做有状态时间判定。
type deviceWatch struct {
	threshold int
	state     map[deviceKey]*deviceState
}

func newDeviceWatch(threshold int) *deviceWatch {
	if threshold <= 0 {
		threshold = defaultOfflineThreshold
	}
	return &deviceWatch{threshold: threshold, state: map[deviceKey]*deviceState{}}
}

// Observe 观察一批读数，返回本批产生的设备事件，按从站地址升序。
//
// 一台设备只要有任一点是通信类失败（质量码 1~4），本周期就算这次采集失败：
// 块读失败时块内全部点是同一个失败码，按「任一点失败」判定不会把整块失败漏判。
// 超量程（5）是「读到了但值越界」，不算通信失败。
func (w *deviceWatch) Observe(b Batch) []model.EventBody {
	failures, codes := collectFailures(b)

	keys := make([]deviceKey, 0, len(failures)+len(w.state))
	for key := range failures {
		keys = append(keys, key)
	}
	for key := range w.state {
		if key.busID == b.BusID {
			if _, ok := failures[key]; !ok {
				keys = append(keys, key)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].slave < keys[j].slave })

	out := make([]model.EventBody, 0, len(keys))
	for _, key := range keys {
		_, failed := failures[key]
		if ev, ok := w.observeOne(b, key, failed, codes[key]); ok {
			out = append(out, ev)
		}
	}
	return out
}

// collectFailures 汇总本批各设备的通信失败点数与首个失败质量码。
func collectFailures(b Batch) (map[deviceKey]int, map[deviceKey]int) {
	failures := map[deviceKey]int{}
	codes := map[deviceKey]int{}
	for _, pt := range b.Points {
		if !isCommFailure(pt.Quality) {
			continue
		}
		key := deviceKey{busID: b.BusID, slave: pt.SlaveAddr}
		failures[key]++
		if _, ok := codes[key]; ok {
			continue
		}
		codes[key] = pt.Quality
	}
	return failures, codes
}

// observeOne 推进一台设备的状态，必要时产出事件。
func (w *deviceWatch) observeOne(b Batch, key deviceKey, failed bool, quality int) (model.EventBody, bool) {
	st := w.state[key]
	if st == nil {
		st = &deviceState{}
		w.state[key] = st
	}

	if !failed {
		recovered := st.offline
		st.offline = false
		st.failures = 0
		if !recovered {
			return model.EventBody{}, false
		}
		return w.event(b, key, EventSensorRecovered, reasonRecovered, 0,
			"恢复后首次读取成功"), true
	}

	// 已报过离线的不再重复报：未恢复期间每周期一条会让平台收到事件风暴。
	if st.offline {
		return model.EventBody{}, false
	}
	st.failures++
	if st.failures < w.threshold {
		return model.EventBody{}, false
	}
	st.offline = true
	return w.event(b, key, EventSensorOffline, reasonOfQuality(quality), st.failures,
		fmt.Sprintf("连续 %d 次采集失败，最近一次质量码 %d", st.failures, quality)), true
}

func (w *deviceWatch) event(b Batch, key deviceKey, eventType, reason string,
	failureCount int, detail string) model.EventBody {
	return model.EventBody{
		EventType:    eventType,
		BusID:        key.busID,
		SlaveAddr:    key.slave,
		EventTime:    b.CollectTime,
		Reason:       reason,
		FailureCount: failureCount,
		Detail:       detail,
	}
}

// reasonOfQuality 把失败质量码翻成事件原因。接口规范只固定了 CONTINUOUS_TIMEOUT 一个值，
// 另外三个是同一命名法的补全——现场要能一眼看出「连续超时」和「连续异常码」是两种故障。
func reasonOfQuality(quality int) string {
	switch quality {
	case model.QualityDeviceException:
		return reasonContinuousException
	case model.QualityCRCError:
		return reasonContinuousCRC
	case model.QualityParseError:
		return reasonContinuousParse
	default:
		return reasonContinuousTimeout
	}
}

// isCommFailure 判断质量码是否属于通信/解析类失败（1~4）。
// 0 是正常；5 是读到了但越界；6~8 由平台侧与仿真侧产生，边缘不产生。
func isCommFailure(q int) bool {
	return q >= model.QualityDeviceException && q <= model.QualityParseError
}
