package ingest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"workshop-monitor/internal/adapter/ingest"
)

// memSink 是 Sink 的内存实现，用于在不连数据库的前提下验证接收段的顺序与判定。
// 每类写入都记一条调用轨迹，测试据此断言「重复消息没写业务表」这类顺序性质。
type memSink struct {
	seen     map[string]bool
	calls    []string
	claimed  map[string]ingest.LogInfo
	data     []ingest.DataRecord
	events   []ingest.EventRecord
	statuses []ingest.StatusRecord
	raws     []ingest.RawRecord
	released []string

	claimErr error
	writeErr error
}

func newMemSink() *memSink {
	return &memSink{seen: map[string]bool{}, claimed: map[string]ingest.LogInfo{}}
}

func key(terminalID string, seq int64) string {
	return terminalID + "|" + itoa(seq)
}

func (m *memSink) Claim(_ context.Context, terminalID string, seq int64, info ingest.LogInfo) (bool, error) {
	m.calls = append(m.calls, "claim:"+key(terminalID, seq))
	if m.claimErr != nil {
		return false, m.claimErr
	}
	if m.seen[key(terminalID, seq)] {
		return false, nil
	}
	m.seen[key(terminalID, seq)] = true
	m.claimed[key(terminalID, seq)] = info
	return true, nil
}

func (m *memSink) Release(_ context.Context, terminalID string, seq int64) error {
	m.calls = append(m.calls, "release:"+key(terminalID, seq))
	delete(m.seen, key(terminalID, seq))
	m.released = append(m.released, key(terminalID, seq))
	return nil
}

func (m *memSink) WriteRaw(_ context.Context, rec ingest.RawRecord) error {
	m.calls = append(m.calls, "raw:"+key(rec.TerminalID, rec.Seq))
	m.raws = append(m.raws, rec)
	return m.writeErr
}

func (m *memSink) WriteData(_ context.Context, rec ingest.DataRecord) error {
	m.calls = append(m.calls, "data:"+key(rec.Raw.TerminalID, rec.Raw.Seq))
	m.data = append(m.data, rec)
	return m.writeErr
}

func (m *memSink) WriteEvent(_ context.Context, rec ingest.EventRecord) error {
	m.calls = append(m.calls, "event:"+key(rec.TerminalID, rec.Raw.Seq))
	m.events = append(m.events, rec)
	return m.writeErr
}

func (m *memSink) WriteStatus(_ context.Context, rec ingest.StatusRecord) error {
	m.calls = append(m.calls, "status:"+key(rec.TerminalID, rec.Raw.Seq))
	m.statuses = append(m.statuses, rec)
	return m.writeErr
}

func newPipeline(t *testing.T, sink ingest.Sink) *ingest.Pipeline {
	t.Helper()
	r, err := ingest.NewRouter(ingest.TopicTemplates{
		Data:   "/edge/{esn}/data/up",
		Event:  "/edge/{esn}/event/up",
		Status: "/edge/{esn}/status/up",
	})
	if err != nil {
		t.Fatalf("模板合法却报错: %v", err)
	}
	return ingest.NewPipeline(r, sink, ingest.RetryPolicy{Delays: []time.Duration{0}})
}

const pipelineData = `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":10245,
  "collectTime":1793500200123,"backfill":false},
  "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":62.5,"quality":0}],
          "yx":[],"stat":{"total":1,"good":1,"bad":0}}}`

// 首条数据报文：登记 → 写原文与历史，且只发生一次业务写入。
func TestPipelineStoresFirstDataMessage(t *testing.T) {
	sink := newMemSink()
	p := newPipeline(t, sink)

	res, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(pipelineData))
	if err != nil {
		t.Fatalf("合法报文不应报错: %v", err)
	}
	if res.Status != ingest.StatusAccepted {
		t.Fatalf("状态为 %q，期望 %q", res.Status, ingest.StatusAccepted)
	}
	if len(sink.data) != 1 {
		t.Fatalf("应写 1 次历史，实际 %d 次", len(sink.data))
	}
	if got := sink.data[0].Rows[0].PointID; got != "A.1.motor_temp" {
		t.Fatalf("pointId 为 %q", got)
	}
	if sink.calls[0] != "claim:ESN-ZX-0001|10245" {
		t.Fatalf("第一步必须是登记幂等键，实际第一调用为 %q", sink.calls[0])
	}
}

// 重复报文：只有登记被调用，业务写入次数不增加（BR-03）。
func TestPipelineDuplicateWritesNothing(t *testing.T) {
	sink := newMemSink()
	p := newPipeline(t, sink)
	ctx := context.Background()

	if _, err := p.Handle(ctx, "/edge/ESN-ZX-0001/data/up", []byte(pipelineData)); err != nil {
		t.Fatalf("首次不应报错: %v", err)
	}
	before := len(sink.calls)

	res, err := p.Handle(ctx, "/edge/ESN-ZX-0001/data/up", []byte(pipelineData))
	if err != nil {
		t.Fatalf("重复报文不应报错: %v", err)
	}
	if res.Status != ingest.StatusDuplicate {
		t.Fatalf("重复报文状态为 %q，期望 DUPLICATE", res.Status)
	}
	if got := sink.calls[before:]; len(got) != 1 || got[0] != "claim:ESN-ZX-0001|10245" {
		t.Fatalf("重复报文只应有一次登记调用，实际 %v", got)
	}
	if len(sink.data) != 1 {
		t.Fatalf("重复报文不得写历史，历史累计 %d 次", len(sink.data))
	}
}

// 解析失败：留原文、不写历史，且 message_log 记 PARSE_ERROR。
func TestPipelineParseErrorKeepsRawOnly(t *testing.T) {
	sink := newMemSink()
	p := newPipeline(t, sink)

	bad := `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":9001,"collectTime":1000},
	  "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":1,"quality":2}],
	          "yx":[],"stat":{"total":1,"good":0,"bad":1}}}`

	res, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(bad))
	if err != nil {
		t.Fatalf("解析失败不应返回错误，应作为结果落库: %v", err)
	}
	if res.Status != ingest.StatusParseError {
		t.Fatalf("状态为 %q，期望 PARSE_ERROR", res.Status)
	}
	if len(sink.data) != 0 {
		t.Fatalf("解析失败不得写历史，实际 %d 次", len(sink.data))
	}
	if len(sink.raws) != 1 {
		t.Fatalf("解析失败必须留原文，实际留了 %d 条", len(sink.raws))
	}
	if sink.raws[0].FailReason == "" {
		t.Fatal("原文必须带失败原因")
	}
	if got := sink.claimed["ESN-ZX-0001|9001"]; got.ParseStatus != ingest.StatusParseError {
		t.Fatalf("message_log 的 parse_status 为 %q", got.ParseStatus)
	}
}

// esn 与主题不一致：判 IDENTITY_MISMATCH，仍留原文；原文的 terminal_id 取主题值。
func TestPipelineIdentityMismatchKeepsRaw(t *testing.T) {
	sink := newMemSink()
	p := newPipeline(t, sink)

	res, err := p.Handle(context.Background(), "/edge/ESN-ZX-0002/data/up", []byte(pipelineData))
	if err != nil {
		t.Fatalf("身份不一致不应返回错误: %v", err)
	}
	if res.Status != ingest.StatusIdentityMismatch {
		t.Fatalf("状态为 %q，期望 IDENTITY_MISMATCH", res.Status)
	}
	if len(sink.data) != 0 {
		t.Fatalf("身份不一致不得写历史")
	}
	if len(sink.raws) != 1 {
		t.Fatalf("身份不一致必须留原文")
	}
	if got := sink.raws[0].TerminalID; got != "ESN-ZX-0002" {
		t.Fatalf("原文 terminal_id 应为主题值 ESN-ZX-0002，实际 %q", got)
	}
}

// 整个载荷不是 JSON 时没有序号可用：只留原文，不登记 message_log。
func TestPipelineUnparsablePayloadSkipsClaim(t *testing.T) {
	sink := newMemSink()
	p := newPipeline(t, sink)

	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte("<<garbage>>")); err != nil {
		t.Fatalf("非 JSON 不应返回错误: %v", err)
	}
	if len(sink.claimed) != 0 {
		t.Fatalf("没有序号时不应登记 message_log，实际登记 %d 条", len(sink.claimed))
	}
	if len(sink.raws) != 1 {
		t.Fatalf("非 JSON 必须留原文，实际 %d 条", len(sink.raws))
	}
}

// 业务写入失败时撤销登记，让 QoS1 重投能重新入库；
// 否则重投会被判成重复，数据静默丢失。
func TestPipelineReleasesClaimOnWriteFailure(t *testing.T) {
	sink := newMemSink()
	sink.writeErr = errors.New("数据库连接被重置")
	p := newPipeline(t, sink)

	_, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(pipelineData))
	if err == nil {
		t.Fatal("写入失败应返回错误")
	}
	if len(sink.released) != 1 || sink.released[0] != "ESN-ZX-0001|10245" {
		t.Fatalf("应撤销 1 次登记，实际 %v", sink.released)
	}

	// 撤销后同一序号重投必须能重新写业务表。
	sink.writeErr = nil
	res, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(pipelineData))
	if err != nil {
		t.Fatalf("撤销后重投不应报错: %v", err)
	}
	if res.Status != ingest.StatusAccepted {
		t.Fatalf("撤销后重投状态为 %q，期望 OK", res.Status)
	}
}

// 登记本身失败时不能写业务表。
func TestPipelineClaimFailureStopsWrites(t *testing.T) {
	sink := newMemSink()
	sink.claimErr = errors.New("连接池耗尽")
	p := newPipeline(t, sink)

	_, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(pipelineData))
	if err == nil {
		t.Fatal("登记失败应返回错误")
	}
	if len(sink.data) != 0 {
		t.Fatalf("登记失败不得写业务表，实际 %d 次", len(sink.data))
	}
}

// 事件与状态报文走各自的写入分支，不产生历史。
func TestPipelineRoutesEventAndStatus(t *testing.T) {
	sink := newMemSink()
	p := newPipeline(t, sink)
	ctx := context.Background()

	eventPayload := `{"header":{"msgType":"EVENT","esn":"ESN-ZX-0001","seq":10246,"collectTime":1793500320123},
	  "body":{"eventType":"SENSOR_OFFLINE","busId":"A","slaveAddr":1,"eventTime":1793500320123,
	          "reason":"CONTINUOUS_TIMEOUT","failureCount":5,"detail":"timeout"}}`
	if _, err := p.Handle(ctx, "/edge/ESN-ZX-0001/event/up", []byte(eventPayload)); err != nil {
		t.Fatalf("事件报文不应报错: %v", err)
	}

	statusPayload := `{"header":{"msgType":"STATUS","esn":"ESN-ZX-0001","seq":10247,"collectTime":1793500380123},
	  "body":{"status":"OFFLINE","statusTime":1793500380123,"reason":"UNEXPECTED_DISCONNECT",
	          "uptimeSec":3600,"cacheDepth":0,"cacheBytes":0,"agentVersion":"1.0.0"}}`
	if _, err := p.Handle(ctx, "/edge/ESN-ZX-0001/status/up", []byte(statusPayload)); err != nil {
		t.Fatalf("状态报文不应报错: %v", err)
	}

	if len(sink.events) != 1 || sink.events[0].EventType != "SENSOR_OFFLINE" {
		t.Fatalf("应写 1 条设备事件，实际 %+v", sink.events)
	}
	if sink.events[0].EventTime.UnixMilli() != 1793500320123 {
		t.Fatalf("事件时间应取 eventTime")
	}
	if len(sink.statuses) != 1 || sink.statuses[0].Status != "OFFLINE" {
		t.Fatalf("应写 1 条状态记录，实际 %+v", sink.statuses)
	}
	if len(sink.data) != 0 {
		t.Fatalf("事件与状态报文不得产生历史行")
	}
}

// 补传标记随报文头进入落库结构，判定侧据此抑制通知（BR-36）。
func TestPipelinePropagatesBackfill(t *testing.T) {
	sink := newMemSink()
	p := newPipeline(t, sink)

	backfill := `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":7001,"collectTime":1000,"backfill":true},
	  "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":1,"quality":0}],
	          "yx":[],"stat":{"total":1,"good":1,"bad":0}}}`

	if _, err := p.Handle(context.Background(), "/edge/ESN-ZX-0001/data/up", []byte(backfill)); err != nil {
		t.Fatalf("补传报文不应报错: %v", err)
	}
	if len(sink.data) != 1 {
		t.Fatalf("应写 1 次历史")
	}
	if !sink.data[0].Header.Backfill || !sink.data[0].Rows[0].Backfill {
		t.Fatal("backfill 应随报文头传到落库结构，供实时值通道过滤")
	}
	if !sink.claimed["ESN-ZX-0001|7001"].Backfill {
		t.Fatal("message_log 的 backfill 列应为 true")
	}
}
