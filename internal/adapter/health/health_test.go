package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 判据来自 rules/adapter-evaluate.md 的健康表：
// 丢弃持续增长 → 降级；活动告警数 ≠ 库中未恢复数 → 降级；判定段缺失 → 503。

func newServer(deps Deps) *Server {
	return New("proto-adapter", "adapter", "1.0.0",
		time.UnixMilli(0), func() time.Time { return time.UnixMilli(3_600_000) }, deps)
}

func statsOf(depth, active, debounce int, dropped int64) EvaluateStats {
	return EvaluateStats{QueueDepth: depth, QueueCapacity: 4096, DropCount: dropped,
		ActiveAlarmCount: active, DebounceKeys: debounce}
}

func healthyDeps() Deps {
	return Deps{
		Evaluate:      func() EvaluateStats { return statsOf(0, 2, 5, 0) },
		ActiveInDB:    func(context.Context) (int, error) { return 2, nil },
		MQTTConnected: func() bool { return true },
		LastReceive:   func() int64 { return 1_700_000_000_000 },
	}
}

func TestAllOKReturns200(t *testing.T) {
	r := newServer(healthyDeps()).Report(context.Background())
	if r.Status != statusOK {
		t.Fatalf("全项正常应为 ok，实际 %q", r.Status)
	}
	if HTTPStatus(r) != http.StatusOK {
		t.Fatalf("ok 应返回 200，实际 %d", HTTPStatus(r))
	}
}

// 判定段字段整体缺失 = 判定协程没起来。报文照常入库但告警永不响，必须 503。
func TestMissingEvaluateIsDown(t *testing.T) {
	d := healthyDeps()
	d.Evaluate = nil
	r := newServer(d).Report(context.Background())

	if r.Status != statusDown {
		t.Fatalf("判定段缺失应为 down，实际 %q", r.Status)
	}
	if HTTPStatus(r) != http.StatusServiceUnavailable {
		t.Fatalf("down 应返回 503，实际 %d", HTTPStatus(r))
	}
}

// 内存活动告警数与库不一致：启动重建失败或中途写库异常，需重启。
func TestActiveCountMismatchIsDegraded(t *testing.T) {
	d := healthyDeps()
	d.ActiveInDB = func(context.Context) (int, error) { return 5, nil }
	r := newServer(d).Report(context.Background())

	if r.Status != statusDegraded {
		t.Fatalf("活动告警数不一致应为 degraded，实际 %q", r.Status)
	}
	if HTTPStatus(r) != http.StatusOK {
		t.Fatalf("degraded 应返回 200，实际 %d", HTTPStatus(r))
	}
	eval, ok := r.Checks["evaluate"].(EvaluateCheck)
	if !ok {
		t.Fatalf("checks.evaluate 类型不符：%T", r.Checks["evaluate"])
	}
	if eval.ActiveInDB != 5 || eval.ActiveAlarmCount != 2 {
		t.Fatalf("对账字段不符：内存 %d 库 %d", eval.ActiveAlarmCount, eval.ActiveInDB)
	}
	if eval.Reason == "" {
		t.Fatal("降级必须给出原因")
	}
}

// 丢弃「持续增长」才降级：单次增长可能只是启动突刺。
func TestDropCountGrowingIsDegraded(t *testing.T) {
	drop := int64(0)
	d := healthyDeps()
	d.Evaluate = func() EvaluateStats { return statsOf(100, 2, 5, drop) }

	s := newServer(d)
	if got := s.Report(context.Background()).Status; got != statusOK {
		t.Fatalf("基线采样不应降级，实际 %q", got)
	}

	// 「持续」按两次连续采样都观察到增长判：基线 → 第一次增长还不算持续。
	drop = 1200
	if got := s.Report(context.Background()).Status; got != statusOK {
		t.Fatalf("单次增长不应降级，实际 %q", got)
	}

	drop = 1500
	if got := s.Report(context.Background()).Status; got != statusDegraded {
		t.Fatalf("连续两次增长应降级，实际 %q", got)
	}

	// 计数持平即不再算「持续增长」，活动数一致时整体回到 ok。
	if got := s.Report(context.Background()).Status; got != statusOK {
		t.Fatalf("计数持平后应恢复 ok，实际 %q", got)
	}
}

// 订阅断开是降级不是不可用：重启会打断接收，且丢掉未入库的消息。
func TestMQTTDisconnectedIsDegraded(t *testing.T) {
	d := healthyDeps()
	d.MQTTConnected = func() bool { return false }
	r := newServer(d).Report(context.Background())

	if r.Status != statusDegraded {
		t.Fatalf("订阅断开应为 degraded，实际 %q", r.Status)
	}
	if HTTPStatus(r) != http.StatusOK {
		t.Fatalf("degraded 应返回 200，实际 %d", HTTPStatus(r))
	}
}

// 端点形状：字段齐全、只接受 GET。
func TestHandlerShape(t *testing.T) {
	s := newServer(healthyDeps())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz 应为 200，实际 %d", rec.Code)
	}

	var got Report
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if got.Service != "proto-adapter" || got.Instance != "adapter" || got.Version != "1.0.0" {
		t.Fatalf("身份字段不符：%+v", got)
	}
	if got.UptimeSec != 3600 {
		t.Fatalf("uptimeSec 应为 3600，实际 %d", got.UptimeSec)
	}

	eval, ok := got.Checks["evaluate"].(map[string]any)
	if !ok {
		t.Fatalf("checks.evaluate 类型不符：%T", got.Checks["evaluate"])
	}
	for _, k := range []string{"queueDepth", "queueCapacity", "dropCount",
		"activeAlarmCount", "debounceKeys"} {
		if _, ok := eval[k]; !ok {
			t.Fatalf("evaluate 缺字段 %s：%v", k, eval)
		}
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /healthz 应为 405，实际 %d", rec.Code)
	}
}

// 库查询失败也是降级：健康检查不能因为查不到库就报 ok。
func TestActiveInDBErrorIsDegraded(t *testing.T) {
	d := healthyDeps()
	d.ActiveInDB = func(context.Context) (int, error) { return 0, errors.New("连接池耗尽") }
	r := newServer(d).Report(context.Background())

	if r.Status != statusDegraded {
		t.Fatalf("库查询失败应为 degraded，实际 %q", r.Status)
	}
}
