package agent

import (
	"testing"

	"workshop-monitor/internal/config"
	"workshop-monitor/internal/model"
)

// 总线可用性判据：连续 busDownCycles 个周期整条总线一个字节都没读回才算断开。
// 设备异常应答（1）与超量程（5）说明从站答复了，总线是通的。

func newTestPoller() *BusPoller {
	return NewBusPoller(&config.Terminal{TerminalID: "ESN-ZX-0001"},
		&config.Bus{BusID: "A"}, nil, func(Batch) {})
}

func reading(quality int) PointReading {
	return PointReading{Code: "p", Quality: quality}
}

func TestBusStaysUpOnGoodReadings(t *testing.T) {
	p := newTestPoller()
	for i := 0; i < 10; i++ {
		p.trackBusHealth([]PointReading{reading(model.QualityGood)})
	}
	if !p.BusUp() {
		t.Fatal("全部正常读数时总线应可用")
	}
}

// 设备异常应答与超量程都说明从站答复了，不能算链路断开。
func TestBusStaysUpOnDeviceLevelFailures(t *testing.T) {
	p := newTestPoller()
	for i := 0; i < 10; i++ {
		p.trackBusHealth([]PointReading{
			reading(model.QualityDeviceException),
			reading(model.QualityOutOfRange),
		})
	}
	if !p.BusUp() {
		t.Fatal("设备级失败不应判总线断开")
	}
}

// 连续 5 个周期全是通信层失败才判断开；第 4 个周期还不到。
func TestBusDownAfterFiveConsecutiveCommFailures(t *testing.T) {
	p := newTestPoller()
	for i := 0; i < busDownCycles-1; i++ {
		p.trackBusHealth([]PointReading{reading(model.QualityTimeout)})
	}
	if !p.BusUp() {
		t.Fatalf("连续 %d 个周期失败时总线仍应可用", busDownCycles-1)
	}

	p.trackBusHealth([]PointReading{reading(model.QualityTimeout)})
	if p.BusUp() {
		t.Fatalf("连续 %d 个周期无响应应判总线断开", busDownCycles)
	}
}

// 中途采到一条正常读数即复位，计数不会跨中断累计。
func TestBusRecoversOnSingleGoodReading(t *testing.T) {
	p := newTestPoller()
	for i := 0; i < 4; i++ {
		p.trackBusHealth([]PointReading{reading(model.QualityTimeout)})
	}
	p.trackBusHealth([]PointReading{reading(model.QualityGood)})
	for i := 0; i < 4; i++ {
		p.trackBusHealth([]PointReading{reading(model.QualityTimeout)})
	}
	if !p.BusUp() {
		t.Fatal("中间有一次正常读数后，重新计数不应立即判断开")
	}
}

// CRC 与解析失败同属通信层：一个字节都没读回来。
func TestBusCountsCRCAndParseErrorsAsCommFailure(t *testing.T) {
	for _, q := range []int{model.QualityCRCError, model.QualityParseError} {
		p := newTestPoller()
		for i := 0; i < busDownCycles; i++ {
			p.trackBusHealth([]PointReading{reading(q)})
		}
		if p.BusUp() {
			t.Fatalf("质量码 %d 连续 %d 个周期应判总线断开", q, busDownCycles)
		}
	}
}
