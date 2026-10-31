package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"workshop-monitor/internal/config"
	"workshop-monitor/internal/model"
)

// TestExpandTopicsFollowsScheme 验两套命名方案展开出不同前缀、同一形态。
// 主题模板来自 config/topics.json，期望值是接口规范 §5.1 的字面量。
func TestExpandTopicsFollowsScheme(t *testing.T) {
	cases := []struct {
		scheme config.TopicSchemes
		want   UplinkTopics
	}{
		{
			scheme: config.TopicSchemes{
				DataUp:   "/edge/{esn}/data/up",
				EventUp:  "/edge/{esn}/event/up",
				StatusUp: "/edge/{esn}/status/up",
			},
			want: UplinkTopics{
				Data:   "/edge/ESN-ZX-0001/data/up",
				Event:  "/edge/ESN-ZX-0001/event/up",
				Status: "/edge/ESN-ZX-0001/status/up",
			},
		},
		{
			scheme: config.TopicSchemes{
				DataUp:   "/workshop/{esn}/data/up",
				EventUp:  "/workshop/{esn}/event/up",
				StatusUp: "/workshop/{esn}/status/up",
			},
			want: UplinkTopics{
				Data:   "/workshop/ESN-ZX-0001/data/up",
				Event:  "/workshop/ESN-ZX-0001/event/up",
				Status: "/workshop/ESN-ZX-0001/status/up",
			},
		},
	}
	for _, c := range cases {
		got, err := ExpandTopics(c.scheme, "ESN-ZX-0001")
		if err != nil {
			t.Fatalf("展开 %q: %v", c.scheme.DataUp, err)
		}
		if got != c.want {
			t.Errorf("展开结果 = %+v，期望 %+v", got, c.want)
		}
	}
}

// TestExpandTopicsRejectsBadTemplate 验占位符缺失或重复时报错：
// 展开不出终端标识的主题会让 5 台终端发到同一个地方。
func TestExpandTopicsRejectsBadTemplate(t *testing.T) {
	cases := []struct {
		name string
		s    config.TopicSchemes
	}{
		{"缺占位符", config.TopicSchemes{
			DataUp: "/edge/data/up", EventUp: "/edge/{esn}/event/up", StatusUp: "/edge/{esn}/status/up"}},
		{"占位符重复", config.TopicSchemes{
			DataUp: "/edge/{esn}/{esn}/data/up", EventUp: "/edge/{esn}/event/up",
			StatusUp: "/edge/{esn}/status/up"}},
		{"无前导斜杠", config.TopicSchemes{
			DataUp: "edge/{esn}/data/up", EventUp: "/edge/{esn}/event/up",
			StatusUp: "/edge/{esn}/status/up"}},
	}
	for _, c := range cases {
		if _, err := ExpandTopics(c.s, "ESN-ZX-0001"); err == nil {
			t.Errorf("%s：应报错", c.name)
		}
	}
}

// TestDataBodySplitsYCAndYX 验遥测与遥信按 siType 分组，且空数组序列化成 [] 而非 null。
func TestDataBodySplitsYCAndYX(t *testing.T) {
	b := Batch{
		BusID: "A",
		Points: []PointReading{
			{Code: "motor_temp", SlaveAddr: 1, SiType: "YC", Unit: "℃", Value: f64(62.5)},
			{Code: "door", SlaveAddr: 4, SiType: "YX", Value: f64(1)},
			{Code: "ia", SlaveAddr: 2, SiType: "YC", Unit: "A", Value: f64(35.2)},
		},
		Good: 3,
	}
	body := DataBodyOf(b)
	if len(body.YC) != 2 || len(body.YX) != 1 {
		t.Fatalf("yc=%d yx=%d，期望 2 / 1", len(body.YC), len(body.YX))
	}
	if body.YC[0].Code != "motor_temp" || body.YC[1].Code != "ia" {
		t.Fatalf("遥测顺序 = %s,%s，期望保持采集顺序", body.YC[0].Code, body.YC[1].Code)
	}
	if body.YX[0].Code != "door" || body.YX[0].SlaveAddr != 4 {
		t.Fatalf("遥信 = %+v", body.YX[0])
	}
	if body.Stat.Total != 3 || body.Stat.Good != 3 || body.Stat.Bad != 0 {
		t.Fatalf("stat = %+v", body.Stat)
	}

	// 只有遥信的总线：yc 必须是 []，不是 null。
	onlyYX := DataBodyOf(Batch{BusID: "B", Points: []PointReading{
		{Code: "door", SlaveAddr: 4, SiType: "YX", Value: f64(0)},
	}})
	raw, err := json.Marshal(onlyYX)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if strings.Contains(string(raw), `"yc":null`) {
		t.Fatalf("yc 序列化成了 null：%s", raw)
	}
}

// TestEncodeUplinkKeepsChineseAndNoTrailingNewline 验中文直出、报文末无多余换行。
// 多一个字节会让报文长度与容量测算对不上；转义会让现场核对变成读\u 转义。
func TestEncodeUplinkKeepsChineseAndNoTrailingNewline(t *testing.T) {
	h := HeaderFor("ESN-ZX-0001", model.MsgTypeEvent, 7, 1793500200123, 1793500200180, true)
	body := model.EventBody{
		EventType: "SENSOR_OFFLINE", BusID: "A", SlaveAddr: 1,
		EventTime: 1793500200123, Reason: "CONTINUOUS_TIMEOUT",
		FailureCount: 5, Detail: "连续 5 次采集失败",
	}
	payload, err := EncodeUplink(h, body)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if strings.HasSuffix(string(payload), "\n") {
		t.Fatal("报文末尾不应带换行")
	}
	if !strings.Contains(string(payload), "连续 5 次采集失败") {
		t.Fatalf("中文被转义了：%s", payload)
	}
	if strings.Contains(string(payload), `\u`) {
		t.Fatalf("报文里出现 unicode 转义：%s", payload)
	}

	var env struct {
		Header model.Header    `json:"header"`
		Body   model.EventBody `json:"body"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("回解: %v", err)
	}
	if env.Header.MsgID != "ESN-ZX-0001-00000007" {
		t.Fatalf("msgId = %q，期望 ESN-ZX-0001-00000007", env.Header.MsgID)
	}
}

// TestHeaderForZeroPadsMsgID 验 msgId 的 8 位零填充在位数变化时仍然成立。
func TestHeaderForZeroPadsMsgID(t *testing.T) {
	cases := map[int64]string{
		1:         "ESN-ZX-0001-00000001",
		10245:     "ESN-ZX-0001-00010245",
		123456789: "ESN-ZX-0001-123456789",
	}
	for seq, want := range cases {
		if got := HeaderFor("ESN-ZX-0001", model.MsgTypeData, seq, 0, 0, true).MsgID; got != want {
			t.Errorf("seq=%d 的 msgId = %q，期望 %q", seq, got, want)
		}
	}
}
