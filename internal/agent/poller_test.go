package agent

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"workshop-monitor/internal/config"
	"workshop-monitor/internal/model"
)

// fakePort 是注入的「串口」。它按 RTU 帧作答，让采集链路能在不连硬件的前提下被验证。
// 串口是 AGENTS.md 指定的外部资源，从这里注入而不是在内部 new 一个——否则这条链路测不了。
type fakePort struct {
	mu      sync.Mutex
	slave   map[int][]uint16
	faults  map[int]faultAction
	pending []byte
	writes  [][]byte
	// dropNext 表示接下来 n 个请求不回应答（模拟丢帧导致的超时）。
	dropNext int
}

type faultAction struct {
	timeout   bool
	exception int // 非 0 时返回 0x83 异常应答
	crcBad    bool
}

func newFakePort() *fakePort {
	return &fakePort{slave: map[int][]uint16{}, faults: map[int]faultAction{}}
}

func (f *fakePort) set(slave int, registers ...uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slave[slave] = registers
}

func (f *fakePort) setFault(slave int, a faultAction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[slave] = a
}

func (f *fakePort) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	if len(p) < 6 {
		return len(p), nil
	}
	if f.dropNext > 0 {
		f.dropNext--
		return len(p), nil
	}
	slave := int(p[0])
	start := int(p[2])<<8 | int(p[3])
	count := int(p[4])<<8 | int(p[5])
	regs, ok := f.slave[slave]
	if !ok || start+count > len(regs) {
		f.pending = append(f.pending, f.exceptionFrame(slave, 2)...)
		return len(p), nil
	}
	f.pending = append(f.pending, f.reply(slave, regs[start:start+count])...)
	return len(p), nil
}

func (f *fakePort) reply(slave int, regs []uint16) []byte {
	switch a := f.faults[slave]; {
	case a.timeout:
		return nil
	case a.exception != 0:
		return f.exceptionFrame(slave, a.exception)
	}
	frame := []byte{byte(slave), fcReadHolding, byte(len(regs) * 2)}
	for _, r := range regs {
		frame = append(frame, byte(r>>8), byte(r))
	}
	frame = withCRC(frame)
	if f.faults[slave].crcBad {
		frame[len(frame)-1] ^= 0xFF
	}
	return frame
}

func (f *fakePort) exceptionFrame(slave, code int) []byte {
	return withCRC([]byte{byte(slave), fcReadHolding | exceptionFlag, byte(code)})
}

// Read 吐出已备好的应答字节；没有应答时返回 io.EOF，由 readFrame 按截止时间判超时。
func (f *fakePort) Read(p []byte) (int, error) {
	f.mu.Lock()
	if len(f.pending) > 0 {
		n := copy(p, f.pending)
		f.pending = f.pending[n:]
		f.mu.Unlock()
		return n, nil
	}
	f.mu.Unlock()
	time.Sleep(time.Millisecond)
	return 0, io.EOF
}

func testTerminal() *config.Terminal {
	scale, scaleVib, scaleEnergy := 0.1, 0.01, 0.01
	zero := 0.0
	minTemp, maxTemp := -20.0, 200.0
	minVib, maxVib := 0.0, 50.0
	minEnergy, maxEnergy := 0.0, 42949672.95
	unitC, unitV, unitKWh := "℃", "mm/s", "kWh"
	addr0, addr1, addr5 := 0, 1, 5
	startAddr := 0

	return &config.Terminal{
		TerminalID:        "ESN-TEST-0001",
		ConfigVersion:     "1.0.0",
		CollectIntervalMs: 5000,
		Buses: []config.Bus{{
			BusID:      "A",
			TimeoutMs:  20,
			RetryCount: 2,
			Devices: []config.Device{{
				SlaveAddr: 2,
				Name:      "三相电量仪表",
				Type:      "power_meter",
				Blocks: []config.Block{{
					FunctionCode: 3, StartAddr: &startAddr, RegisterCount: 7,
					Points: []config.Point{
						{Code: "motor_temp", SiType: "YC", DataType: "int16", ByteOrder: "AB",
							Addr: &addr0, Width: 1, Scale: &scale, Offset: &zero,
							Unit: &unitC, MinValue: &minTemp, MaxValue: &maxTemp},
						{Code: "motor_vib", SiType: "YC", DataType: "uint16", ByteOrder: "AB",
							Addr: &addr1, Width: 1, Scale: &scaleVib, Offset: &zero,
							Unit: &unitV, MinValue: &minVib, MaxValue: &maxVib},
						{Code: "ep_forward", SiType: "YC", DataType: "uint32", ByteOrder: "ABCD",
							Addr: &addr5, Width: 2, Scale: &scaleEnergy, Offset: &zero,
							Unit: &unitKWh, MinValue: &minEnergy, MaxValue: &maxEnergy},
					},
				}},
			}},
		}},
	}
}

func newPoller(port *fakePort) *BusPoller {
	term := testTerminal()
	return NewBusPoller(term, &term.Buses[0], port, func(Batch) {})
}

func valueOf(batch Batch, code string) *PointReading {
	for i := range batch.Points {
		if batch.Points[i].Code == code {
			return &batch.Points[i]
		}
	}
	return nil
}

func TestCollectReadsAllPoints(t *testing.T) {
	port := newFakePort()
	// 从站 2：温度 62.5 ℃（625）、振动 2.35 mm/s（235）、电能 12345.67 kWh
	// （1234567 = 0x0012D687，高字 0x0012 在前）。
	port.set(2, 625, 235, 0, 0, 0, 0x0012, 0xD687)

	batch := newPoller(port).Collect()

	if batch.BusID != "A" || batch.TerminalID != "ESN-TEST-0001" {
		t.Fatalf("批次标识 = %s/%s", batch.TerminalID, batch.BusID)
	}
	if len(batch.Points) != 3 {
		t.Fatalf("测点数 = %d，期望 3", len(batch.Points))
	}
	want := map[string]float64{"motor_temp": 62.5, "motor_vib": 2.35, "ep_forward": 12345.67}
	for _, pt := range batch.Points {
		if pt.Value == nil {
			t.Fatalf("%s 应有值", pt.Code)
		}
		if *pt.Value != want[pt.Code] {
			t.Fatalf("%s = %v，期望 %v", pt.Code, *pt.Value, want[pt.Code])
		}
		if pt.Quality != model.QualityGood {
			t.Fatalf("%s 质量码 = %d，期望 0", pt.Code, pt.Quality)
		}
	}
	if batch.Good != 3 || batch.Bad != 0 {
		t.Fatalf("统计 good=%d bad=%d，期望 3/0", batch.Good, batch.Bad)
	}
	if port.requestCount() != 1 {
		t.Fatalf("一个周期每从站只发一次请求，实发 %d 次", port.requestCount())
	}
}

func TestCollectMapsFailuresToQuality(t *testing.T) {
	cases := []struct {
		name   string
		action faultAction
		wantQ  int
	}{
		{name: "超时", action: faultAction{timeout: true}, wantQ: model.QualityTimeout},
		{name: "异常码 02", action: faultAction{exception: 2}, wantQ: model.QualityDeviceException},
		{name: "CRC 错", action: faultAction{crcBad: true}, wantQ: model.QualityCRCError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port := newFakePort()
			port.set(2, 625, 235, 0, 0, 0, 0, 0)
			port.setFault(2, tc.action)

			batch := newPoller(port).Collect()

			if len(batch.Points) != 3 {
				t.Fatalf("失败时失败点也必须留在批次里，实得 %d 个", len(batch.Points))
			}
			for _, pt := range batch.Points {
				if pt.Quality != tc.wantQ {
					t.Fatalf("%s 质量码 = %d，期望 %d", pt.Code, pt.Quality, tc.wantQ)
				}
				if pt.Value != nil {
					t.Fatalf("%s 失败时必须为空值，实得 %v", pt.Code, *pt.Value)
				}
			}
			if batch.Bad != 3 {
				t.Fatalf("bad = %d，期望 3", batch.Bad)
			}
		})
	}
}

func TestCollectRetriesTimeoutThenSucceeds(t *testing.T) {
	port := newFakePort()
	port.set(2, 625, 235, 0, 0, 0, 0, 0)
	port.dropNext = 1 // 首帧丢失，重试应恢复

	batch := newPoller(port).Collect()

	if got := valueOf(batch, "motor_temp"); got == nil || got.Quality != model.QualityGood {
		t.Fatalf("重试后应恢复 GOOD，实得 %+v", got)
	}
	if got := port.requestCount(); got != 2 {
		t.Fatalf("请求帧数 = %d，期望 2（首次超时 + 一次重试）", got)
	}
}

func TestCollectOutOfRangeKeepsValue(t *testing.T) {
	port := newFakePort()
	port.set(2, 625, 6000, 0, 0, 0, 0, 0) // 振动 60.00 mm/s，超过 0~50

	batch := newPoller(port).Collect()

	vib := valueOf(batch, "motor_vib")
	if vib == nil || vib.Value == nil {
		t.Fatal("超量程必须保留数值")
	}
	if *vib.Value != 60 || vib.Quality != model.QualityOutOfRange {
		t.Fatalf("值=%v 质量=%d，期望 60 / 5", *vib.Value, vib.Quality)
	}
	if batch.Bad != 1 || batch.Good != 2 {
		t.Fatalf("超量程计入 bad：good=%d bad=%d，期望 2/1", batch.Good, batch.Bad)
	}
}

func TestCollectDoesNotReusePreviousValue(t *testing.T) {
	port := newFakePort()
	port.set(2, 625, 235, 0, 0, 0, 0, 0)
	p := newPoller(port)
	if first := p.Collect(); first.Points[0].Value == nil {
		t.Fatal("首轮应有值")
	}

	port.setFault(2, faultAction{timeout: true})
	for _, pt := range p.Collect().Points {
		if pt.Value != nil {
			t.Fatalf("失败后不得沿用上一次值，%s 仍有 %v", pt.Code, *pt.Value)
		}
	}
}

func TestCollectTimeStampedOncePerBatch(t *testing.T) {
	port := newFakePort()
	port.set(2, 625, 235, 0, 0, 0, 0, 0)
	p := newPoller(port)

	// 让时钟在采集过程中前进：若逐块取时间，collectTime 会落在中途。
	var clockCalls int
	base := time.UnixMilli(1793500200000)
	p.now = func() time.Time {
		clockCalls++
		return base.Add(time.Duration(clockCalls) * time.Second)
	}
	batch := p.Collect()

	if clockCalls != 1 {
		t.Fatalf("采集周期内取时钟 %d 次，期望 1 次（本批开始时刻）", clockCalls)
	}
	if batch.CollectTime != base.Add(time.Second).UnixMilli() {
		t.Fatalf("collectTime = %d，期望本批开始时刻", batch.CollectTime)
	}
}

func TestCollectReadsDevicesInAddressOrder(t *testing.T) {
	term := testTerminal()
	// 第二个从站地址更小，排到前面。
	two := term.Buses[0].Devices[0]
	two.SlaveAddr = 5
	one := two
	one.SlaveAddr = 1
	term.Buses[0].Devices = []config.Device{two, one}

	port := newFakePort()
	port.set(1, 1, 2, 0, 0, 0, 0, 0)
	port.set(5, 3, 4, 0, 0, 0, 0, 0)
	p := NewBusPoller(term, &term.Buses[0], port, func(Batch) {})
	p.Collect()

	if port.requestCount() != 2 {
		t.Fatalf("请求数 = %d，期望 2", port.requestCount())
	}
	first := port.writes[0][0]
	if int(first) != 1 {
		t.Fatalf("首个请求发往从站 %d，期望按地址升序从 1 开始", first)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	term := testTerminal()
	term.CollectIntervalMs = 10
	port := newFakePort()
	port.set(2, 625, 235, 0, 0, 0, 0, 0)

	done := make(chan Batch, 8)
	p := NewBusPoller(term, &term.Buses[0], port, func(b Batch) { done <- b })

	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("10 ms 周期内应有批次产出")
	}
	cancel()

	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("ctx 取消后不应再有批次")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBuildReadRequestFrame(t *testing.T) {
	frame, err := buildReadRequest(2, 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) != 8 {
		t.Fatalf("帧长 = %d，期望 8", len(frame))
	}
	want := []byte{0x02, 0x03, 0x00, 0x00, 0x00, 0x07}
	if !bytes.Equal(frame[:6], want) {
		t.Fatalf("帧头 = % X，期望 % X", frame[:6], want)
	}
	if crc16(frame[:6]) != uint16(frame[6])|uint16(frame[7])<<8 {
		t.Fatalf("CRC 未按低字节在前追加：% X", frame[6:])
	}
}

// TestCRC16Reference 用 Modbus 规范 v1.1b3 的示例帧做独立基准：
// 请求 11 03 00 6B 00 03 的 CRC 线上字节为 76 87，应答为 49 AD。
func TestCRC16Reference(t *testing.T) {
	req := []byte{0x11, 0x03, 0x00, 0x6B, 0x00, 0x03}
	if got := crc16(req); got != 0x8776 {
		t.Fatalf("CRC = 0x%04X，期望 0x8776（线上 76 87）", got)
	}
	resp := []byte{0x11, 0x03, 0x06, 0xAE, 0x41, 0x56, 0x52, 0x43, 0x40}
	if got := crc16(resp); got != 0xAD49 {
		t.Fatalf("CRC = 0x%04X，期望 0xAD49（线上 49 AD）", got)
	}
}
