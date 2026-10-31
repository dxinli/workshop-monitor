package ingest_test

import (
	"context"
	"testing"
	"time"

	"workshop-monitor/internal/adapter/ingest"
)

// fakeStateSink 包住 memSink，把状态判定相关的写入单独记下来。
type fakeStateSink struct {
	*memSink
	termWrites []ingest.TermSnapshot
	deviceEvts []ingest.EventRecord
}

func (f *fakeStateSink) WriteTermState(_ context.Context, snap ingest.TermSnapshot) error {
	f.termWrites = append(f.termWrites, snap)
	return nil
}

func (f *fakeStateSink) WriteDeviceEvent(_ context.Context, rec ingest.EventRecord) error {
	f.deviceEvts = append(f.deviceEvts, rec)
	return nil
}

func newStatePipeline(t *testing.T, sink ingest.Sink, state *ingest.TermState) *ingest.Pipeline {
	t.Helper()
	r, err := ingest.NewRouter(ingest.TopicTemplates{
		Data:   "/edge/{esn}/data/up",
		Event:  "/edge/{esn}/event/up",
		Status: "/edge/{esn}/status/up",
	})
	if err != nil {
		t.Fatalf("模板合法却报错: %v", err)
	}
	p := ingest.NewPipeline(r, sink, ingest.RetryPolicy{Delays: []time.Duration{0}})
	p.WithTermState(state, 3)
	return p
}

const statusOnline = `{"header":{"msgType":"STATUS","esn":"ESN-ZX-0001","seq":101,
  "collectTime":1793500380123,"backfill":false},
  "body":{"status":"ONLINE","statusTime":1793500380123,"reason":"CONNECTED",
          "uptimeSec":1,"cacheDepth":0,"cacheBytes":0,"agentVersion":"1.0.0"}}`

const statusOffline = `{"header":{"msgType":"STATUS","esn":"ESN-ZX-0001","seq":102,
  "collectTime":1793500381123,"backfill":false},
  "body":{"status":"OFFLINE","statusTime":1793500381123,"reason":"UNEXPECTED_DISCONNECT",
          "uptimeSec":1,"cacheDepth":0,"cacheBytes":0,"agentVersion":"1.0.0"}}`

// 状态报文入库后，状态判定结果要传到 sink，且带着 lastSeen。
func TestPipelineFeedsTermStateOnStatus(t *testing.T) {
	base := newMemSink()
	sink := &fakeStateSink{memSink: base}
	state := ingest.NewTermState(ingest.TermStateConfig{OfflinePeriods: 3, CollectInterval: 5 * time.Second})
	p := newStatePipeline(t, sink, state)

	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/status/up", []byte(statusOnline)); err != nil {
		t.Fatalf("状态报文不应报错: %v", err)
	}
	if len(base.statuses) != 1 {
		t.Fatalf("应写 1 次状态记录，实际 %d 次", len(base.statuses))
	}
	got := base.statuses[0]
	if got.Status != ingest.StatusOnline || got.Reason != ingest.ReasonConnected {
		t.Fatalf("状态投影为 %+v", got)
	}
	if !got.Changed {
		t.Fatal("首次 ONLINE 应标记为变化，才会更新 terminal_state 并记事件")
	}
	if got.LastSeen.IsZero() {
		t.Fatal("lastSeen 必须带上，否则巡检无法判断超时")
	}
}

// 重复上报同一状态：只留原文，不反复写状态表。
func TestPipelineSkipsUnchangedStatus(t *testing.T) {
	base := newMemSink()
	sink := &fakeStateSink{memSink: base}
	state := ingest.NewTermState(ingest.TermStateConfig{CollectInterval: 5 * time.Second})
	p := newStatePipeline(t, sink, state)
	ctx := context.Background()

	if _, err := p.Handle(ctx, "/edge/ESN-ZX-0001/status/up", []byte(statusOnline)); err != nil {
		t.Fatalf("首次状态不应报错: %v", err)
	}
	again := `{"header":{"msgType":"STATUS","esn":"ESN-ZX-0001","seq":103,
	  "collectTime":1793500382123,"backfill":false},
	  "body":{"status":"ONLINE","statusTime":1793500382123,"reason":"CONNECTED",
	          "uptimeSec":2,"cacheDepth":0,"cacheBytes":0,"agentVersion":"1.0.0"}}`
	if _, err := p.Handle(ctx, "/edge/ESN-ZX-0001/status/up", []byte(again)); err != nil {
		t.Fatalf("重复状态不应报错: %v", err)
	}
	if len(base.statuses) != 2 {
		t.Fatalf("两条状态报文都应留记录，实际 %d 条", len(base.statuses))
	}
	if base.statuses[1].Changed {
		t.Fatal("状态未变化时不应标记变化，否则会重复写 terminal_state 与事件")
	}
}

// 数据报文推进 lastData 并判在线。
func TestPipelineFeedsTermStateOnData(t *testing.T) {
	base := newMemSink()
	sink := &fakeStateSink{memSink: base}
	state := ingest.NewTermState(ingest.TermStateConfig{CollectInterval: 5 * time.Second})
	p := newStatePipeline(t, sink, state)

	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(pipelineData)); err != nil {
		t.Fatalf("数据报文不应报错: %v", err)
	}
	if len(sink.termWrites) != 1 {
		t.Fatalf("首次数据应写 1 次状态，实际 %d 次", len(sink.termWrites))
	}
	if sink.termWrites[0].Reason != ingest.ReasonDataReceived {
		t.Fatalf("原因为 %q", sink.termWrites[0].Reason)
	}
	if !sink.termWrites[0].LastData.Equal(time.UnixMilli(1793500200123)) {
		t.Fatalf("lastData 取 collectTime，实际 %v", sink.termWrites[0].LastData)
	}
}

// 补传数据只留原文与历史，不推进状态。
func TestPipelineBackfillDoesNotAdvanceState(t *testing.T) {
	base := newMemSink()
	sink := &fakeStateSink{memSink: base}
	state := ingest.NewTermState(ingest.TermStateConfig{CollectInterval: 5 * time.Second})
	p := newStatePipeline(t, sink, state)

	backfill := `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":7001,"collectTime":1000,"backfill":true},
	  "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":1,"quality":0}],
	          "yx":[],"stat":{"total":1,"good":1,"bad":0}}}`
	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(backfill)); err != nil {
		t.Fatalf("补传报文不应报错: %v", err)
	}
	if len(sink.termWrites) != 0 {
		t.Fatalf("补传不应推进终端状态，实际写了 %d 次", len(sink.termWrites))
	}
}

// 设备事件：SENSOR_OFFLINE / SENSOR_RECOVERED 各落一条 device_event。
func TestPipelineDeviceEvents(t *testing.T) {
	base := newMemSink()
	sink := &fakeStateSink{memSink: base}
	state := ingest.NewTermState(ingest.TermStateConfig{CollectInterval: 5 * time.Second})
	p := newStatePipeline(t, sink, state)

	off := `{"header":{"msgType":"EVENT","esn":"ESN-ZX-0001","seq":201,"collectTime":1793500320000},
	  "body":{"eventType":"SENSOR_OFFLINE","busId":"A","slaveAddr":1,"eventTime":1793500320000,
	          "reason":"CONTINUOUS_TIMEOUT","failureCount":5,"detail":"timeout"}}`
	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/event/up", []byte(off)); err != nil {
		t.Fatalf("设备离线事件不应报错: %v", err)
	}
	if !state.DeviceOffline("ESN-ZX-0001", "A", 1) {
		t.Fatal("收到 SENSOR_OFFLINE 后设备应处于离线")
	}

	rec := `{"header":{"msgType":"EVENT","esn":"ESN-ZX-0001","seq":202,"collectTime":1793500330000},
	  "body":{"eventType":"SENSOR_RECOVERED","busId":"A","slaveAddr":1,"eventTime":1793500330000,
	          "reason":"FIRST_SUCCESS","failureCount":0,"detail":""}}`
	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/event/up", []byte(rec)); err != nil {
		t.Fatalf("设备恢复事件不应报错: %v", err)
	}
	if state.DeviceOffline("ESN-ZX-0001", "A", 1) {
		t.Fatal("收到 SENSOR_RECOVERED 后设备应恢复在线")
	}
}

// 没有注入状态判定器时，行为与 008 完全一致：只落库，不写状态表。
func TestPipelineWithoutTermStateKeepsOldBehaviour(t *testing.T) {
	base := newMemSink()
	sink := &fakeStateSink{memSink: base}
	p := newPipeline(t, sink)

	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/status/up", []byte(statusOnline)); err != nil {
		t.Fatalf("状态报文不应报错: %v", err)
	}
	if len(sink.termWrites) != 0 {
		t.Fatalf("未注入判定器时不应写状态表，实际 %d 次", len(sink.termWrites))
	}
	if len(base.statuses) != 1 {
		t.Fatalf("仍应写 1 次状态记录，实际 %d 次", len(base.statuses))
	}
}

// SweepAll 把超时判定结果交给 sink，产生 TERMINAL_OFFLINE 事件。
func TestPipelineSweepWritesOffline(t *testing.T) {
	base := newMemSink()
	sink := &fakeStateSink{memSink: base}
	state := ingest.NewTermState(ingest.TermStateConfig{OfflinePeriods: 3, CollectInterval: 5 * time.Second})
	p := newStatePipeline(t, sink, state)
	ctx := context.Background()

	if _, err := p.Handle(ctx, "/edge/ESN-ZX-0001/data/up", []byte(pipelineData)); err != nil {
		t.Fatalf("数据报文不应报错: %v", err)
	}
	before := len(sink.termWrites)

	// 采集时间 + 20 秒，超过 3 × 5 秒
	if err := p.Sweep(ctx, time.UnixMilli(1793500200123+20_000)); err != nil {
		t.Fatalf("巡检不应报错: %v", err)
	}
	if len(sink.termWrites) != before+1 {
		t.Fatalf("超时后应写 1 次状态，实际新增 %d 次", len(sink.termWrites)-before)
	}
	last := sink.termWrites[len(sink.termWrites)-1]
	if last.Status != ingest.StatusOffline || last.Reason != ingest.ReasonDataTimeout {
		t.Fatalf("超时状态为 %+v", last)
	}

	// 再次巡检不应重复产生离线事件
	if err := p.Sweep(ctx, time.UnixMilli(1793500200123+40_000)); err != nil {
		t.Fatalf("第二次巡检不应报错: %v", err)
	}
	if got := len(sink.termWrites); got != before+1 {
		t.Fatalf("已离线的终端不应重复写状态，累计 %d 次", got)
	}
}
