package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"workshop-monitor/internal/model"
	"workshop-monitor/internal/pkglog"
)

// fakeBroker 是注入的发布通道，只记下「发了什么」，不上 Broker。
// 它实现 Publisher，与 MQTTLink 是同一条缝上的两个实现。
type fakeBroker struct {
	published []recorded
	failNext  int
}

type recorded struct {
	topic   string
	payload []byte
	retain  bool
}

func (f *fakeBroker) Publish(topic string, payload []byte, retain bool) error {
	if f.failNext > 0 {
		f.failNext--
		return errors.New("broker 不可用")
	}
	f.published = append(f.published, recorded{topic: topic, payload: payload, retain: retain})
	return nil
}

// envelopes 解码全部已发布报文，返回按发送顺序排列的 (topic, header, body)。
func (f *fakeBroker) envelopes(t *testing.T) []sentMessage {
	t.Helper()
	out := make([]sentMessage, 0, len(f.published))
	for _, p := range f.published {
		var env struct {
			Header model.Header    `json:"header"`
			Body   json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal(p.payload, &env); err != nil {
			t.Fatalf("解析已发布报文（topic=%s）: %v", p.topic, err)
		}
		out = append(out, sentMessage{topic: p.topic, header: env.Header, body: env.Body, retain: p.retain})
	}
	return out
}

type sentMessage struct {
	topic  string
	header model.Header
	body   json.RawMessage
	retain bool
}

// uplinkFixture 组装一个上报段：序号写到临时文件、时钟固定、日志进缓冲区。
type uplinkFixture struct {
	uplink *Uplink
	broker *fakeBroker
	alloch *SeqAllocator
	clock  *fakeClock
	logbuf *bytes.Buffer
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newUplinkFixture(t *testing.T) *uplinkFixture {
	t.Helper()
	broker := &fakeBroker{}
	alloc, err := LoadSeqAllocator(filepath.Join(t.TempDir(), "seq"))
	if err != nil {
		t.Fatalf("载入序号分配器: %v", err)
	}
	clock := &fakeClock{now: time.UnixMilli(1793500200230)}
	buf := &bytes.Buffer{}
	up, err := NewUplink(UplinkConfig{
		Esn: "ESN-ZX-0001",
		Topics: UplinkTopics{
			Data:   "/edge/ESN-ZX-0001/data/up",
			Event:  "/edge/ESN-ZX-0001/event/up",
			Status: "/edge/ESN-ZX-0001/status/up",
		},
		Alloc:        alloc,
		Link:         broker,
		Log:          pkglog.New("agent", "ESN-ZX-0001", pkglog.LevelDebug, buf),
		TimeSync:     true,
		AgentVersion: "1.0.0",
		Now:          clock.Now,
	})
	if err != nil {
		t.Fatalf("组装上报段: %v", err)
	}
	return &uplinkFixture{uplink: up, broker: broker, alloch: alloc, clock: clock, logbuf: buf}
}

func f64(v float64) *float64 { return &v }

// goodBatch 造一批正常读数：2 个遥测 1 个遥信，采集时间固定。
func goodBatch(collectTime int64) Batch {
	return Batch{
		TerminalID:  "ESN-ZX-0001",
		BusID:       "A",
		CollectTime: collectTime,
		Points: []PointReading{
			{Code: "motor_temp", SlaveAddr: 1, SiType: "YC", Unit: "℃", Value: f64(62.5), Quality: model.QualityGood},
			{Code: "motor_vib", SlaveAddr: 1, SiType: "YC", Unit: "mm/s", Value: f64(2.35), Quality: model.QualityGood},
			{Code: "door", SlaveAddr: 4, SiType: "YX", Value: f64(0), Quality: model.QualityGood},
		},
		Good: 3,
	}
}

// TestSendBatchPublishesDataToConfiguredTopic 验数据报文发到主题模板展开后的地址，
// 且三类主题互不串用。
func TestSendBatchPublishesDataToConfiguredTopic(t *testing.T) {
	fx := newUplinkFixture(t)
	if err := fx.uplink.SendBatch(goodBatch(1793500200123)); err != nil {
		t.Fatalf("上报: %v", err)
	}

	msgs := fx.broker.envelopes(t)
	if len(msgs) != 1 {
		t.Fatalf("发消息数 = %d，期望 1", len(msgs))
	}
	if msgs[0].topic != "/edge/ESN-ZX-0001/data/up" {
		t.Fatalf("主题 = %q，期望 /edge/ESN-ZX-0001/data/up", msgs[0].topic)
	}
	if msgs[0].retain {
		t.Fatal("数据报文不得 Retain（接口规范 §4.1）")
	}
}

// TestHeaderHasEightSpecFields 逐字段核对报文头。
// 期望值是接口规范 §6.2 的字面量，不引用实现里的常量。
func TestHeaderHasEightSpecFields(t *testing.T) {
	fx := newUplinkFixture(t)
	const collectTime = int64(1793500200123)
	if err := fx.uplink.SendBatch(goodBatch(collectTime)); err != nil {
		t.Fatalf("上报: %v", err)
	}

	h := fx.broker.envelopes(t)[0].header
	cases := []struct {
		field string
		got   any
		want  any
	}{
		{"schemaVersion", h.SchemaVersion, "2.0"},
		{"msgType", h.MsgType, "DATA"},
		{"msgId", h.MsgID, "ESN-ZX-0001-00000001"},
		{"esn", h.Esn, "ESN-ZX-0001"},
		{"seq", h.Seq, int64(1)},
		{"collectTime", h.CollectTime, collectTime},
		{"sendTime", h.SendTime, int64(1793500200230)},
		{"backfill", h.Backfill, false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v，期望 %v", c.field, c.got, c.want)
		}
	}
}

// TestSendTimeComesFromPublishMoment 验 sendTime 与 collectTime 不混用：
// collectTime 沿用批次的开始时刻，sendTime 取发布时刻。
func TestSendTimeComesFromPublishMoment(t *testing.T) {
	fx := newUplinkFixture(t)
	const collectTime = int64(1793500200123)
	fx.clock.now = time.UnixMilli(1793500200987)

	if err := fx.uplink.SendBatch(goodBatch(collectTime)); err != nil {
		t.Fatalf("上报: %v", err)
	}
	h := fx.broker.envelopes(t)[0].header
	if h.CollectTime != collectTime {
		t.Fatalf("collectTime = %d，期望沿用批次时刻 %d", h.CollectTime, collectTime)
	}
	if h.SendTime != 1793500200987 {
		t.Fatalf("sendTime = %d，期望发布时刻 1793500200987", h.SendTime)
	}
}

// TestFailedPointsStayInArray 验强制规则 1：失败点必须在数组里且 value=null。
func TestFailedPointsStayInArray(t *testing.T) {
	fx := newUplinkFixture(t)
	batch := Batch{
		TerminalID:  "ESN-ZX-0001",
		BusID:       "A",
		CollectTime: 1793500200123,
		Points: []PointReading{
			{Code: "motor_temp", SlaveAddr: 1, SiType: "YC", Unit: "℃", Value: nil, Quality: model.QualityTimeout},
			{Code: "motor_vib", SlaveAddr: 1, SiType: "YC", Unit: "mm/s", Value: nil, Quality: model.QualityCRCError},
			{Code: "estop", SlaveAddr: 4, SiType: "YX", Value: nil, Quality: model.QualityDeviceException},
		},
		Bad: 3,
	}
	if err := fx.uplink.SendBatch(batch); err != nil {
		t.Fatalf("上报: %v", err)
	}

	var body model.DataBody
	if err := json.Unmarshal(fx.broker.envelopes(t)[0].body, &body); err != nil {
		t.Fatalf("解析报文体: %v", err)
	}
	if len(body.YC) != 2 || len(body.YX) != 1 {
		t.Fatalf("yc=%d yx=%d，期望 2 / 1（失败点不得省略）", len(body.YC), len(body.YX))
	}
	for _, p := range append(append([]model.PointValue{}, body.YC...), body.YX...) {
		if p.Value != nil {
			t.Errorf("%s 的质量码是失败码，value 必须为 null，实际 %v", p.Code, *p.Value)
		}
	}
	if body.Stat.Total != 3 || body.Stat.Good != 0 || body.Stat.Bad != 3 {
		t.Fatalf("stat = %+v，期望 total=3 good=0 bad=3", body.Stat)
	}
}

// TestPointsOfOneBatchShareSeq 验 BR-04：同一次采集的多个测点共享同一 seq。
// 报文只有一条，测点都在 body 里——所以「共享」体现在只有一条报文而不是每点一条。
func TestPointsOfOneBatchShareSeq(t *testing.T) {
	fx := newUplinkFixture(t)
	if err := fx.uplink.SendBatch(goodBatch(1793500200123)); err != nil {
		t.Fatalf("上报: %v", err)
	}
	msgs := fx.broker.envelopes(t)
	if len(msgs) != 1 {
		t.Fatalf("一个采集周期发了 %d 条，期望 1 条", len(msgs))
	}
	var body model.DataBody
	if err := json.Unmarshal(msgs[0].body, &body); err != nil {
		t.Fatalf("解析报文体: %v", err)
	}
	if body.Stat.Total != 3 {
		t.Fatalf("stat.total = %d，期望 3（三点共享同一 seq=%d）", body.Stat.Total, msgs[0].header.Seq)
	}
}

// TestSeqIncreasesAcrossMessages 验序号单调递增，且数据与状态共用同一空间。
func TestSeqIncreasesAcrossMessages(t *testing.T) {
	fx := newUplinkFixture(t)
	if err := fx.uplink.SendBatch(goodBatch(1793500200123)); err != nil {
		t.Fatalf("数据上报: %v", err)
	}
	if err := fx.uplink.SendStatus(StatusOnline, ReasonConnected); err != nil {
		t.Fatalf("状态上报: %v", err)
	}
	if err := fx.uplink.SendBatch(goodBatch(1793500205123)); err != nil {
		t.Fatalf("第二批上报: %v", err)
	}

	msgs := fx.broker.envelopes(t)
	want := []int64{1, 2, 3}
	if len(msgs) != len(want) {
		t.Fatalf("发消息数 = %d，期望 %d", len(msgs), len(want))
	}
	for i, w := range want {
		if msgs[i].header.Seq != w {
			t.Fatalf("第 %d 条 seq = %d，期望 %d", i+1, msgs[i].header.Seq, w)
		}
	}
}

// TestStatusTopicRetainsAndCarriesReason 验 STATE 消息走状态主题、Retain、带 reason。
func TestStatusTopicRetainsAndCarriesReason(t *testing.T) {
	fx := newUplinkFixture(t)
	if err := fx.uplink.SendStatus(StatusOffline, ReasonGracefulShutdown); err != nil {
		t.Fatalf("上报状态: %v", err)
	}
	msg := fx.broker.envelopes(t)[0]
	if msg.topic != "/edge/ESN-ZX-0001/status/up" {
		t.Fatalf("主题 = %q，期望状态主题", msg.topic)
	}
	if !msg.retain {
		t.Fatal("状态报文必须 Retain（接口规范 §4.1）")
	}
	var body model.StatusBody
	if err := json.Unmarshal(msg.body, &body); err != nil {
		t.Fatalf("解析报文体: %v", err)
	}
	if body.Status != "OFFLINE" || body.Reason != "GRACEFUL_SHUTDOWN" {
		t.Fatalf("状态 = %s/%s，期望 OFFLINE/GRACEFUL_SHUTDOWN", body.Status, body.Reason)
	}
}

// TestWillIsOfflineUnexpected 验遗嘱内容（BR-27 的报文侧）。
func TestWillIsOfflineUnexpected(t *testing.T) {
	fx := newUplinkFixture(t)
	topic, payload, err := fx.uplink.WillPayload()
	if err != nil {
		t.Fatalf("组装遗嘱: %v", err)
	}
	if topic != "/edge/ESN-ZX-0001/status/up" {
		t.Fatalf("遗嘱主题 = %q", topic)
	}
	var env struct {
		Header model.Header     `json:"header"`
		Body   model.StatusBody `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("解析遗嘱: %v", err)
	}
	if env.Body.Status != "OFFLINE" || env.Body.Reason != "UNEXPECTED_DISCONNECT" {
		t.Fatalf("遗嘱状态 = %s/%s，期望 OFFLINE/UNEXPECTED_DISCONNECT", env.Body.Status, env.Body.Reason)
	}
	if env.Header.MsgType != "STATUS" {
		t.Fatalf("遗嘱 msgType = %q，期望 STATUS", env.Header.MsgType)
	}
}

// TestWillTakesFreshSeqEachTime 验遗嘱每次都取新序号。
// 固定号会让第二次掉线的遗嘱被消息级幂等当重复丢掉。
func TestWillTakesFreshSeqEachTime(t *testing.T) {
	fx := newUplinkFixture(t)
	_, first, err := fx.uplink.WillPayload()
	if err != nil {
		t.Fatalf("首次遗嘱: %v", err)
	}
	_, second, err := fx.uplink.WillPayload()
	if err != nil {
		t.Fatalf("再次遗嘱: %v", err)
	}
	seqOf := func(b []byte) int64 {
		var env struct {
			Header model.Header `json:"header"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("解析遗嘱: %v", err)
		}
		return env.Header.Seq
	}
	if seqOf(first) == seqOf(second) {
		t.Fatalf("两次遗嘱用了同一序号 %d", seqOf(first))
	}
}

// TestRecoveredDeviceEmitsEvent 验恢复事件：离线后首次成功读取发 SENSOR_RECOVERED。
func TestRecoveredDeviceEmitsEvent(t *testing.T) {
	fx := newUplinkFixture(t)
	const threshold = 5
	for i := 0; i < threshold; i++ {
		failed := Batch{
			TerminalID:  "ESN-ZX-0001",
			BusID:       "A",
			CollectTime: 1793500200123,
			Points: []PointReading{{Code: "motor_temp", SlaveAddr: 1, SiType: "YC",
				Quality: model.QualityTimeout}},
			Bad: 1,
		}
		if err := fx.uplink.SendBatch(failed); err != nil {
			t.Fatalf("第 %d 次失败批上报: %v", i+1, err)
		}
	}
	if err := fx.uplink.SendBatch(goodBatch(1793500205123)); err != nil {
		t.Fatalf("恢复批上报: %v", err)
	}

	var events []model.EventBody
	for _, m := range fx.broker.envelopes(t) {
		if m.header.MsgType != "EVENT" {
			continue
		}
		var ev model.EventBody
		if err := json.Unmarshal(m.body, &ev); err != nil {
			t.Fatalf("解析事件: %v", err)
		}
		events = append(events, ev)
	}
	if len(events) != 2 {
		t.Fatalf("事件数 = %d，期望 2（一次离线 + 一次恢复）", len(events))
	}
	if events[0].EventType != "SENSOR_OFFLINE" || events[0].SlaveAddr != 1 || events[0].FailureCount != 5 {
		t.Fatalf("首个事件 = %+v，期望 SENSOR_OFFLINE 从站 1 连续失败 5 次", events[0])
	}
	if events[1].EventType != "SENSOR_RECOVERED" {
		t.Fatalf("第二个事件 = %s，期望 SENSOR_RECOVERED", events[1].EventType)
	}
	if events[0].Reason == "" || events[1].Reason == "" {
		t.Fatal("事件必须带原因")
	}
}

// TestDeviceOfflineEventOnlyOnce 验离线期间不重复发事件：每周期一条会让平台收到风暴。
func TestDeviceOfflineEventOnlyOnce(t *testing.T) {
	fx := newUplinkFixture(t)
	failed := Batch{
		TerminalID: "ESN-ZX-0001", BusID: "A", CollectTime: 1793500200123,
		Points: []PointReading{{Code: "motor_temp", SlaveAddr: 1, SiType: "YC", Quality: model.QualityTimeout}},
		Bad:    1,
	}
	for i := 0; i < 8; i++ {
		if err := fx.uplink.SendBatch(failed); err != nil {
			t.Fatalf("第 %d 批: %v", i+1, err)
		}
	}

	offline := 0
	for _, m := range fx.broker.envelopes(t) {
		if m.header.MsgType != "EVENT" {
			continue
		}
		var ev model.EventBody
		if err := json.Unmarshal(m.body, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.EventType == EventSensorOffline {
			offline++
		}
	}
	if offline != 1 {
		t.Fatalf("离线事件数 = %d，期望 1（8 次失败只报一次）", offline)
	}
}

// TestOutOfRangeIsNotCommFailure 验超量程不算通信失败：读到了值，只是越界。
func TestOutOfRangeIsNotCommFailure(t *testing.T) {
	fx := newUplinkFixture(t)
	batch := Batch{
		TerminalID: "ESN-ZX-0001", BusID: "A", CollectTime: 1793500200123,
		Points: []PointReading{{Code: "motor_temp", SlaveAddr: 1, SiType: "YC",
			Unit: "℃", Value: f64(250), Quality: model.QualityOutOfRange}},
		Bad: 1,
	}
	for i := 0; i < 8; i++ {
		if err := fx.uplink.SendBatch(batch); err != nil {
			t.Fatalf("第 %d 批: %v", i+1, err)
		}
	}
	for _, m := range fx.broker.envelopes(t) {
		if m.header.MsgType == "EVENT" {
			t.Fatalf("超量程不应产生设备事件，实际收到 %s", string(m.body))
		}
	}
}

// TestPublishFailureIsReturnedNotSwallowed 验上报失败要能被上层看到（007 据此落盘）。
func TestPublishFailureIsReturnedNotSwallowed(t *testing.T) {
	fx := newUplinkFixture(t)
	fx.broker.failNext = 1

	// SendBatch 记日志不返回错误，但 SendStatus 要返回——启动期发 ONLINE 失败必须暴露。
	if err := fx.uplink.SendStatus(StatusOnline, ReasonConnected); err == nil {
		t.Fatal("发布失败时应返回错误")
	}
	if len(fx.broker.published) != 0 {
		t.Fatalf("失败时不应记入已发布列表")
	}
}
