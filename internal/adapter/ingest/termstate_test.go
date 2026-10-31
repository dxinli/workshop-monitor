package ingest_test

import (
	"testing"
	"time"

	"workshop-monitor/internal/adapter/ingest"
)

func at(ms int64) time.Time { return time.UnixMilli(ms) }

// TermState 是终端状态与设备状态的内存模型，测试通过它观察判定结果。
func newState() *ingest.TermState {
	return ingest.NewTermState(ingest.TermStateConfig{
		OfflinePeriods:  3,
		CollectInterval: 5 * time.Second,
	})
}

// 收到 ONLINE 状态消息即在线，原因为 CONNECTED。
func TestTermStateOnlineOnStatusMessage(t *testing.T) {
	st := newState()
	tr := st.ApplyStatus("ESN-ZX-0001", "ONLINE", "CONNECTED", at(1000), at(1000))

	if tr.Status != ingest.StatusOnline {
		t.Fatalf("状态为 %q，期望 ONLINE", tr.Status)
	}
	if tr.Reason != ingest.ReasonConnected {
		t.Fatalf("原因为 %q，期望 CONNECTED", tr.Reason)
	}
}

// 显式离线优先级最高：数据刚到达也立刻判离线。
func TestTermStateOfflineBeatsFreshData(t *testing.T) {
	st := newState()
	st.ApplyData("ESN-ZX-0001", at(10_000), false, at(10_000))
	tr := st.ApplyStatus("ESN-ZX-0001", "OFFLINE", "UNEXPECTED_DISCONNECT", at(10_100), at(10_100))

	if tr.Status != ingest.StatusOffline {
		t.Fatalf("显式离线应压过数据新鲜度，实际 %q", tr.Status)
	}
	if tr.Reason != "UNEXPECTED_DISCONNECT" {
		t.Fatalf("原因为 %q", tr.Reason)
	}
}

// 数据超时：超过 3 个采集周期无数据判离线。
func TestTermStateDataTimeout(t *testing.T) {
	st := newState()
	st.ApplyData("ESN-ZX-0001", at(10_000), false, at(10_000))

	cases := []struct {
		now  time.Time
		want string
	}{
		{at(10_000 + 14_000), ingest.StatusOnline},  // 未到 15 秒
		{at(10_000 + 15_001), ingest.StatusOffline}, // 超过 3 × 5 秒
	}
	for _, c := range cases {
		tr := st.Sweep("ESN-ZX-0001", c.now)
		if tr.Status != c.want {
			t.Fatalf("now=%d 时状态为 %q，期望 %q", c.now.UnixMilli(), tr.Status, c.want)
		}
		if c.want == ingest.StatusOffline && tr.Reason != ingest.ReasonDataTimeout {
			t.Fatalf("超时离线原因应为 DATA_TIMEOUT，实际 %q", tr.Reason)
		}
	}
}

// 补传数据不推进 lastData，也不把已超时的终端刷回在线。
func TestTermStateBackfillDoesNotRevive(t *testing.T) {
	st := newState()
	st.ApplyData("ESN-ZX-0001", at(10_000), false, at(10_000))
	st.Sweep("ESN-ZX-0001", at(10_000+20_000)) // 判超时离线

	// 断连期间的数据在恢复后批量补传，collectTime 都很新。
	tr := st.ApplyData("ESN-ZX-0001", at(10_000+21_000), true, at(10_000+21_000))
	if tr.Status != ingest.StatusOffline {
		t.Fatalf("补传不得把终端刷回在线，实际 %q", tr.Status)
	}
	if tr.Reason != ingest.ReasonDataTimeout {
		t.Fatalf("原因应保持 DATA_TIMEOUT，实际 %q", tr.Reason)
	}
}

// 非补传数据到达即在线，原因为 DATA_RECEIVED。
func TestTermStateDataBringsOnline(t *testing.T) {
	st := newState()
	st.ApplyStatus("ESN-ZX-0001", "OFFLINE", "UNEXPECTED_DISCONNECT", at(1000), at(1000))

	tr := st.ApplyData("ESN-ZX-0001", at(2000), false, at(2000))
	if tr.Status != ingest.StatusOnline {
		t.Fatalf("非补传数据应判在线，实际 %q", tr.Status)
	}
	if tr.Reason != ingest.ReasonDataReceived {
		t.Fatalf("原因为 %q，期望 DATA_RECEIVED", tr.Reason)
	}
}

// 较早 collectTime 的数据不得回退 lastData。
func TestTermStateIgnoresOlderCollectTime(t *testing.T) {
	st := newState()
	st.ApplyData("ESN-ZX-0001", at(10_000), false, at(10_000))
	st.ApplyData("ESN-ZX-0001", at(9_000), false, at(10_500)) // 迟到的旧数据

	tr := st.Sweep("ESN-ZX-0001", at(10_000+14_000))
	if tr.Status != ingest.StatusOnline {
		t.Fatalf("lastData 不应被旧数据回退，实际 %q", tr.Status)
	}
}

// 状态变化才产生事件：重复上报同一状态不刷事件。
func TestTermStateReportsTransitionsOnly(t *testing.T) {
	st := newState()

	first := st.ApplyStatus("ESN-ZX-0001", "ONLINE", "CONNECTED", at(1000), at(1000))
	if !first.Changed {
		t.Fatal("首次状态应记为变化")
	}
	again := st.ApplyStatus("ESN-ZX-0001", "ONLINE", "CONNECTED", at(2000), at(2000))
	if again.Changed {
		t.Fatal("同一状态重复上报不应再记变化")
	}
	off := st.ApplyStatus("ESN-ZX-0001", "OFFLINE", "GRACEFUL_SHUTDOWN", at(3000), at(3000))
	if !off.Changed {
		t.Fatal("离线应记为变化")
	}
}

// 设备离线：连续 5 次采集失败；第 4 次失败还不能判离线。
func TestDeviceOfflineAfterFiveFailures(t *testing.T) {
	st := newState()

	for i := 1; i <= 4; i++ {
		res := st.DeviceFailure("ESN-ZX-0001", "A", 1, false)
		if res.Offline {
			t.Fatalf("第 %d 次失败不应判设备离线", i)
		}
	}
	res := st.DeviceFailure("ESN-ZX-0001", "A", 1, false)
	if !res.Offline {
		t.Fatal("第 5 次失败应判设备离线")
	}
	if !res.Changed {
		t.Fatal("设备离线应记为变化")
	}
}

// 首次采集成功即恢复，产生恢复事件；未离线的设备不产生多余的恢复事件。
func TestDeviceRecoveredOnFirstSuccess(t *testing.T) {
	st := newState()
	for i := 1; i <= 5; i++ {
		st.DeviceFailure("ESN-ZX-0001", "A", 1, false)
	}

	res := st.DeviceSuccess("ESN-ZX-0001", "A", 1)
	if !res.Recovered {
		t.Fatal("离线后首次成功应产生恢复事件")
	}
	again := st.DeviceSuccess("ESN-ZX-0001", "A", 1)
	if again.Recovered {
		t.Fatal("已在线设备再成功不应重复产生恢复事件")
	}
}

// 恢复后失败计数清零：再失败 4 次不应直接判离线。
func TestDeviceFailureCounterResetsOnRecovery(t *testing.T) {
	st := newState()
	for i := 1; i <= 5; i++ {
		st.DeviceFailure("ESN-ZX-0001", "A", 1, false)
	}
	st.DeviceSuccess("ESN-ZX-0001", "A", 1)

	for i := 1; i <= 4; i++ {
		if st.DeviceFailure("ESN-ZX-0001", "A", 1, false).Offline {
			t.Fatalf("恢复后第 %d 次失败不应判离线", i)
		}
	}
}

// 补传报文里的设备事件不推进设备失败计数，也不触发离线。
func TestDeviceFailureIgnoresBackfill(t *testing.T) {
	st := newState()
	for i := 1; i <= 5; i++ {
		res := st.DeviceFailure("ESN-ZX-0001", "A", 1, true)
		if res.Offline {
			t.Fatal("补传期间的历史失败不应判当前离线")
		}
	}
	if st.DeviceOffline("ESN-ZX-0001", "A", 1) {
		t.Fatal("补传不应让设备进入离线")
	}
}

// 设备之间互不影响：同一总线另一从站的失败不累加到本从站。
func TestDeviceFailureScopedPerSlave(t *testing.T) {
	st := newState()
	for i := 1; i <= 4; i++ {
		st.DeviceFailure("ESN-ZX-0001", "A", 1, false)
	}
	res := st.DeviceFailure("ESN-ZX-0001", "A", 2, false)
	if res.Offline {
		t.Fatal("另一从站的失败不应累加到本从站")
	}
}

// 终端离线与设备离线是两类事件，各自记变化，互不覆盖。
func TestTerminalAndDeviceOfflineAreSeparate(t *testing.T) {
	st := newState()
	st.ApplyData("ESN-ZX-0001", at(1000), false, at(1000))

	dev := st.DeviceFailure("ESN-ZX-0001", "A", 1, false)
	if dev.Offline {
		t.Fatal("设备不应在第 1 次失败时离线")
	}
	term := st.Sweep("ESN-ZX-0001", at(1000+20_000))
	if term.Status != ingest.StatusOffline {
		t.Fatalf("终端应因超时离线，实际 %q", term.Status)
	}
	if st.DeviceOffline("ESN-ZX-0001", "A", 1) {
		t.Fatal("终端离线不应把设备也判离线")
	}
}
