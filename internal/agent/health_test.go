package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 健康检查的判据来自 rules/edge-agent.md 与详细设计 §5：
// MQTT 断开是降级（200），串口打不开是不可用（503），缓存达 80% 是降级。
// 期望值是规则表里的字面结论，不是照抄实现里的分支。

func healthOf(probes HealthProbes) HealthReport {
	h := NewHealthServer("edge-agent", "ESN-ZX-0001", "1.0.0",
		time.UnixMilli(0), func() time.Time { return time.UnixMilli(3_600_000) }, probes)
	return h.Report()
}

func okProbes() HealthProbes {
	return HealthProbes{
		Config: func() ConfigCheck { return ConfigCheck{Status: healthOK, ConfigVersion: "1.0.0"} },
		Serial: func() SerialCheck {
			return SerialStatusOf(map[string]bool{"A": true, "B": true})
		},
		MQTT:  func() MQTTCheck { return MQTTStatusOf(true, 0) },
		Cache: func() CacheCheck { return CacheStatusOf(CacheStats{}, DefaultCacheSpec(), 0) },
	}
}

func TestHealthAllOKReturns200(t *testing.T) {
	r := healthOf(okProbes())
	if r.Status != healthOK {
		t.Fatalf("全项正常应为 ok，实际 %q", r.Status)
	}
	if got := (&HealthServer{}).HTTPStatus(r); got != http.StatusOK {
		t.Fatalf("ok 应返回 200，实际 %d", got)
	}
}

// MQTT 断开必须是 200：503 会被编排重启容器，打断采集并丢掉未落盘数据。
func TestHealthMQTTDisconnectedIsDegradedNotDown(t *testing.T) {
	p := okProbes()
	p.MQTT = func() MQTTCheck { return MQTTStatusOf(false, 1_700_000_000_000) }
	r := healthOf(p)

	if r.Status != healthDegraded {
		t.Fatalf("MQTT 断开应为 degraded，实际 %q", r.Status)
	}
	if got := (&HealthServer{}).HTTPStatus(r); got != http.StatusOK {
		t.Fatalf("degraded 应返回 200，实际 %d", got)
	}

	mqtt, ok := r.Checks["mqtt"].(MQTTCheck)
	if !ok {
		t.Fatalf("checks.mqtt 类型不符：%T", r.Checks["mqtt"])
	}
	if mqtt.Connected || mqtt.LastSuccess != 1_700_000_000_000 {
		t.Fatalf("mqtt 分项字段不符：%+v", mqtt)
	}
}

// 串口任一条打不开即不可用：那条总线上的测点全部采不到，报 200 会被当成个别设备故障。
func TestHealthAnyBusClosedIsDown(t *testing.T) {
	p := okProbes()
	p.Serial = func() SerialCheck {
		return SerialStatusOf(map[string]bool{"A": true, "B": false})
	}
	r := healthOf(p)

	if r.Status != healthDown {
		t.Fatalf("任一总线关闭应为 down，实际 %q", r.Status)
	}
	if got := (&HealthServer{}).HTTPStatus(r); got != http.StatusServiceUnavailable {
		t.Fatalf("down 应返回 503，实际 %d", got)
	}

	serial, ok := r.Checks["serial"].(SerialCheck)
	if !ok {
		t.Fatalf("checks.serial 类型不符：%T", r.Checks["serial"])
	}
	if serial.Buses["A"] != "open" || serial.Buses["B"] != "closed" {
		t.Fatalf("总线状态不符：%v", serial.Buses)
	}
}

// 缓存达 80% 是降级。三上限各自独立，任一维度到线即降级。
func TestHealthCacheAtWarnRatioIsDegraded(t *testing.T) {
	spec := CacheSpec{MaxItems: 1000, MaxBytes: 1000, MaxAge: time.Hour, WarnRatio: 0.8}

	cases := []struct {
		name  string
		stats CacheStats
		want  string
	}{
		{"空队列", CacheStats{}, healthOK},
		{"条数 79%", CacheStats{Items: 790}, healthOK},
		{"条数 80%", CacheStats{Items: 800}, healthDegraded},
		{"字节 85%", CacheStats{Bytes: 850}, healthDegraded},
		{"时长 90%", CacheStats{AgeOfOldest: 54 * time.Minute}, healthDegraded},
		{"条数未到但字节到", CacheStats{Items: 100, Bytes: 900}, healthDegraded},
	}
	for _, c := range cases {
		got := CacheStatusOf(c.stats, spec, 7)
		if got.Status != c.want {
			t.Fatalf("%s：应为 %s，实际 %s", c.name, c.want, got.Status)
		}
		if got.DropCount != 7 {
			t.Fatalf("%s：dropCount 应原样透出，实际 %d", c.name, got.DropCount)
		}
	}
}

// 探测函数未注入时按 down 处理：缺探测等于不知道该维度是否可用，报 ok 是假的。
func TestHealthMissingProbeIsDown(t *testing.T) {
	r := healthOf(HealthProbes{})
	if r.Status != healthDown {
		t.Fatalf("探测全缺应为 down，实际 %q", r.Status)
	}
}

// 端点形状：200/503、JSON 字段、只接受 GET。
func TestHealthHandlerShape(t *testing.T) {
	h := NewHealthServer("edge-agent", "ESN-ZX-0001", "1.0.0",
		time.UnixMilli(0), func() time.Time { return time.UnixMilli(3_600_000) }, okProbes())

	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz 应为 200，实际 %d", rec.Code)
	}

	var got HealthReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if got.Service != "edge-agent" || got.Instance != "ESN-ZX-0001" || got.Version != "1.0.0" {
		t.Fatalf("身份字段不符：%+v", got)
	}
	if got.UptimeSec != 3600 {
		t.Fatalf("uptimeSec 应为 3600，实际 %d", got.UptimeSec)
	}
	for _, k := range []string{"config", "serial", "mqtt", "cache"} {
		if _, ok := got.Checks[k]; !ok {
			t.Fatalf("checks 缺 %s", k)
		}
	}

	// 分项状态必须平铺在各自对象里，不能包一层 check: {}
	var raw map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &struct {
		Checks *map[string]map[string]any `json:"checks"`
	}{Checks: &raw}); err != nil {
		t.Fatalf("解析 checks：%v", err)
	}
	if raw["serial"]["status"] != "ok" {
		t.Fatalf("serial.status 未平铺：%v", raw["serial"])
	}

	rec = httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz 应为 405，实际 %d", rec.Code)
	}
}
