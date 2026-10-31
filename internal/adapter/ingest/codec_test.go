package ingest_test

import (
	"errors"
	"testing"
	"time"

	"workshop-monitor/internal/adapter/ingest"
	"workshop-monitor/internal/model"
)

const dataPayload = `{
  "header": {
    "schemaVersion": "2.0", "msgType": "DATA",
    "msgId": "ESN-ZX-0001-00010245", "esn": "ESN-ZX-0001", "seq": 10245,
    "collectTime": 1793500200123, "sendTime": 1793500200230,
    "backfill": false, "timeSync": true
  },
  "body": {
    "busId": "A",
    "yc": [
      { "code": "motor_temp", "slaveAddr": 1, "value": 62.5,  "unit": "℃",  "quality": 0 },
      { "code": "motor_vib",  "slaveAddr": 1, "value": 2.35,  "unit": "mm/s","quality": 0 },
      { "code": "uab",        "slaveAddr": 2, "value": 382.4, "unit": "V",   "quality": 0 },
      { "code": "ep_forward", "slaveAddr": 2, "value": 12345.67, "unit": "kWh", "quality": 5 }
    ],
    "yx": [
      { "code": "door",  "slaveAddr": 4, "value": 0, "quality": 0 },
      { "code": "estop", "slaveAddr": 4, "value": null, "quality": 2 }
    ],
    "stat": { "total": 6, "good": 4, "bad": 2 }
  }
}`

func mustDecodeData(t *testing.T) ingest.Decoded {
	t.Helper()
	d, err := ingest.Decode("ESN-ZX-0001", model.MsgTypeData, []byte(dataPayload))
	if err != nil {
		t.Fatalf("合法数据报文不应报错: %v", err)
	}
	return d
}

// 解码后每个测点都要带齐终端、总线、从站、编码与派生的 pointId。
// pointId 由 busId.slaveAddr.pointCode 拼装，顺序不得调整。
func TestDecodeDataDerivesPointID(t *testing.T) {
	d := mustDecodeData(t)

	want := []struct {
		code    string
		slave   int
		siType  string
		pointID string
		value   *float64
		quality int
	}{
		{"motor_temp", 1, "YC", "A.1.motor_temp", f(62.5), 0},
		{"motor_vib", 1, "YC", "A.1.motor_vib", f(2.35), 0},
		{"uab", 2, "YC", "A.2.uab", f(382.4), 0},
		{"ep_forward", 2, "YC", "A.2.ep_forward", f(12345.67), 5},
		{"door", 4, "YX", "A.4.door", f(0), 0},
		{"estop", 4, "YX", "A.4.estop", nil, 2},
	}
	if len(d.Rows) != len(want) {
		t.Fatalf("应解出 %d 行，实际 %d 行", len(want), len(d.Rows))
	}
	for i, w := range want {
		got := d.Rows[i]
		if got.PointCode != w.code || got.SlaveAddr != w.slave || got.SiType != w.siType {
			t.Fatalf("第 %d 行识别为 %+v，期望 code=%s slave=%d siType=%s",
				i, got, w.code, w.slave, w.siType)
		}
		if got.PointID != w.pointID {
			t.Fatalf("%s 的 pointId 为 %q，期望 %q", w.code, got.PointID, w.pointID)
		}
		if got.Quality != w.quality {
			t.Fatalf("%s 的质量码为 %d，期望 %d", w.code, got.Quality, w.quality)
		}
		if !sameValue(got.Value, w.value) {
			t.Fatalf("%s 的值为 %v，期望 %v", w.code, fmtPtr(got.Value), fmtPtr(w.value))
		}
		if got.TerminalID != "ESN-ZX-0001" || got.BusID != "A" {
			t.Fatalf("%s 的终端/总线为 %s/%s", w.code, got.TerminalID, got.BusID)
		}
	}
}

// 时间轴只用 collectTime；receiveTime 由接收方填，不参与业务。
func TestDecodeDataUsesCollectTime(t *testing.T) {
	d := mustDecodeData(t)

	wantMs := int64(1793500200123)
	if got := d.Header.CollectTime; got != wantMs {
		t.Fatalf("collectTime 为 %d，期望 %d", got, wantMs)
	}
	for _, row := range d.Rows {
		if got := row.CollectTime.UnixMilli(); got != wantMs {
			t.Fatalf("行 %s 的 collectTime 为 %d，期望 %d", row.PointCode, got, wantMs)
		}
	}
}

// 质量码 ≤4 而 value 非空，说明边缘把失败值也发了；落库会污染曲线与判定。
func TestDecodeRejectsValueWithBadQuality(t *testing.T) {
	bad := `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":1,"collectTime":1000},
	        "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":1.5,"quality":2}],
	                "yx":[],"stat":{"total":1,"good":0,"bad":1}}}`

	_, err := ingest.Decode("ESN-ZX-0001", model.MsgTypeData, []byte(bad))
	if !errors.Is(err, ingest.ErrBadQuality) {
		t.Fatalf("质量码与值的组合非法时应返回 ErrBadQuality，实际 %v", err)
	}
}

// stat.total 必须等于 yc+yx 的实际条数，否则说明报文被截断或伪造。
func TestDecodeRejectsStatMismatch(t *testing.T) {
	bad := `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":1,"collectTime":1000},
	        "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":1.5,"quality":0}],
	                "yx":[],"stat":{"total":14,"good":14,"bad":0}}}`

	_, err := ingest.Decode("ESN-ZX-0001", model.MsgTypeData, []byte(bad))
	if !errors.Is(err, ingest.ErrParse) {
		t.Fatalf("统计与实际条数不符时应返回 ErrParse，实际 %v", err)
	}
}

// 报文头 esn 与主题不一致时整条判 IDENTITY_MISMATCH，且必须能取到 esn 供原文留存。
func TestDecodeReportsIdentityMismatch(t *testing.T) {
	_, err := ingest.Decode("ESN-ZX-0002", model.MsgTypeData, []byte(dataPayload))
	if !errors.Is(err, ingest.ErrIdentityMismatch) {
		t.Fatalf("esn 与主题不一致时应返回 ErrIdentityMismatch，实际 %v", err)
	}
}

// 未知质量码、未知 msgType、非 JSON 一律是解析失败，不能当成正常数据处理。
func TestDecodeRejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"非 JSON":     `not json`,
		"未知 msgType": `{"header":{"msgType":"UNKNOWN","esn":"ESN-ZX-0001","seq":1,"collectTime":1000},"body":{}}`,
		"质量码越界": `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":1,"collectTime":1000},
		             "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":1,"quality":9}],
		                     "yx":[],"stat":{"total":1,"good":0,"bad":1}}}`,
		"缺 seq": `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","collectTime":1000},
		          "body":{"busId":"A","yc":[],"yx":[],"stat":{"total":0,"good":0,"bad":0}}}`,
	}
	for name, payload := range cases {
		if _, err := ingest.Decode("ESN-ZX-0001", model.MsgTypeData, []byte(payload)); !errors.Is(err, ingest.ErrParse) {
			t.Fatalf("%s 应返回 ErrParse，实际 %v", name, err)
		}
	}
}

// 事件与状态报文只解析出结构，不产生历史行。
func TestDecodeEventAndStatus(t *testing.T) {
	eventPayload := `{"header":{"msgType":"EVENT","esn":"ESN-ZX-0001","seq":10246,"collectTime":1793500320123},
	  "body":{"eventType":"SENSOR_OFFLINE","busId":"A","slaveAddr":1,"eventTime":1793500320123,
	          "reason":"CONTINUOUS_TIMEOUT","failureCount":5,"detail":"timeout"}}`
	ev, err := ingest.Decode("ESN-ZX-0001", model.MsgTypeEvent, []byte(eventPayload))
	if err != nil {
		t.Fatalf("事件报文不应报错: %v", err)
	}
	if ev.Event == nil || ev.Event.EventType != "SENSOR_OFFLINE" || ev.Event.SlaveAddr != 1 {
		t.Fatalf("事件体解析错误: %+v", ev.Event)
	}
	if len(ev.Rows) != 0 {
		t.Fatalf("事件报文不应产生历史行，实际 %d 行", len(ev.Rows))
	}

	statusPayload := `{"header":{"msgType":"STATUS","esn":"ESN-ZX-0001","seq":10247,"collectTime":1793500380123},
	  "body":{"status":"OFFLINE","statusTime":1793500380123,"reason":"UNEXPECTED_DISCONNECT",
	          "uptimeSec":3600,"cacheDepth":0,"cacheBytes":0,"agentVersion":"1.0.0"}}`
	st, err := ingest.Decode("ESN-ZX-0001", model.MsgTypeStatus, []byte(statusPayload))
	if err != nil {
		t.Fatalf("状态报文不应报错: %v", err)
	}
	if st.Status == nil || st.Status.Status != "OFFLINE" || st.Status.Reason != "UNEXPECTED_DISCONNECT" {
		t.Fatalf("状态体解析错误: %+v", st.Status)
	}
	if len(st.Rows) != 0 {
		t.Fatalf("状态报文不应产生历史行，实际 %d 行", len(st.Rows))
	}
}

// 补传标记来自报文头 backfill，不用时间差推断。
func TestDecodeKeepsBackfillFlag(t *testing.T) {
	payload := `{"header":{"msgType":"DATA","esn":"ESN-ZX-0001","seq":5,"collectTime":1000,"backfill":true},
	  "body":{"busId":"A","yc":[{"code":"motor_temp","slaveAddr":1,"value":1,"quality":0}],
	          "yx":[],"stat":{"total":1,"good":1,"bad":0}}}`
	d, err := ingest.Decode("ESN-ZX-0001", model.MsgTypeData, []byte(payload))
	if err != nil {
		t.Fatalf("补传报文不应报错: %v", err)
	}
	if !d.Header.Backfill {
		t.Fatal("header.backfill 应为 true")
	}
	if !d.Rows[0].Backfill {
		t.Fatal("行上的 backfill 应随报文头")
	}
}

func f(v float64) *float64 { return &v }

func sameValue(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtPtr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

var _ = time.Now
