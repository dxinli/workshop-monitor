package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// 健康检查（详细设计 §5）。GET /healthz → 200（就绪或降级）/ 503（不可用）。
//
// 这里只做「读快照 + 判定 + 编码」，不访问串口、Broker 或磁盘：
// 探测函数全部由调用方注入（rules/edge-agent.md 并发表：健康检查持只读快照）。
// 在本包里自己开连接会让健康检查变成一个会影响被检查对象的调用方。

// 三档结论。down 对应 HTTP 503，其余两档都是 200。
const (
	healthOK       = "ok"
	healthDegraded = "degraded"
	healthDown     = "down"
)

// Check 是每项检查的公共部分。嵌进各分项后 `status` 会平铺到 JSON 顶层。
type Check struct {
	Status string `json:"status"`
}

// ConfigCheck 报告配置状态。配置在启动时已校验，运行时只会是 ok。
type ConfigCheck struct {
	Check
	ConfigVersion string `json:"configVersion,omitempty"`
}

// SerialCheck 报告每条总线是否可用。键是总线标识，值是 open / closed。
type SerialCheck struct {
	Check
	Buses map[string]string `json:"buses,omitempty"`
}

// MQTTCheck 报告连接状态与最近一次成功建连的时刻。
type MQTTCheck struct {
	Check
	Connected   bool  `json:"connected"`
	LastSuccess int64 `json:"lastSuccess,omitempty"`
}

// CacheCheck 报告本地队列占用。字段名与详细设计 §5 的例子一致。
type CacheCheck struct {
	Check
	Depth     int   `json:"depth"`
	Bytes     int64 `json:"bytes"`
	DropCount int64 `json:"dropCount"`
}

// HealthReport 是 /healthz 的响应体。
type HealthReport struct {
	Status    string         `json:"status"`
	Service   string         `json:"service"`
	Instance  string         `json:"instance"`
	Version   string         `json:"version"`
	UptimeSec int64          `json:"uptimeSec"`
	Checks    map[string]any `json:"checks"`
}

// HealthProbes 是四个分项的探测函数。任一为空时该项按 down 处理——
// 缺探测等于不知道该维度是否可用，报 ok 是假的。
type HealthProbes struct {
	Config func() ConfigCheck
	Serial func() SerialCheck
	MQTT   func() MQTTCheck
	Cache  func() CacheCheck
}

// HealthServer 提供只读健康快照。
type HealthServer struct {
	service  string
	instance string
	version  string
	start    time.Time
	now      func() time.Time
	probes   HealthProbes
}

// NewHealthServer 装配健康检查。外部依赖全部由调用方注入。
func NewHealthServer(service, instance, version string, start time.Time,
	now func() time.Time, probes HealthProbes) *HealthServer {
	if now == nil {
		now = time.Now
	}
	return &HealthServer{service: service, instance: instance, version: version,
		start: start, now: now, probes: probes}
}

// Report 取一次快照。判定顺序：任一项 down 即 down；否则任一项 degraded 即 degraded。
func (h *HealthServer) Report() HealthReport {
	checks := map[string]any{}

	cfg := probeOrDown(h.probes.Config, func() ConfigCheck {
		return ConfigCheck{Status: healthDown}
	})
	serial := probeOrDown(h.probes.Serial, func() SerialCheck {
		buses := map[string]string{}
		return SerialCheck{Status: healthDown, Buses: buses}
	})
	mqtt := probeOrDown(h.probes.MQTT, func() MQTTCheck {
		return MQTTCheck{Status: healthDown}
	})
	cache := probeOrDown(h.probes.Cache, func() CacheCheck {
		return CacheCheck{Status: healthDown}
	})

	checks["config"] = cfg
	checks["serial"] = serial
	checks["mqtt"] = mqtt
	checks["cache"] = cache

	return HealthReport{
		Status:    aggregate(cfg.Status, serial.Status, mqtt.Status, cache.Status),
		Service:   h.service,
		Instance:  h.instance,
		Version:   h.version,
		UptimeSec: int64(h.now().Sub(h.start).Seconds()),
		Checks:    checks,
	}
}

// HTTPStatus 把结论映射成状态码。DOWN 用 503 让编排把它当故障；
// 降级必须是 200，否则重启会打断采集并丢掉内存里没落盘的数据。
func (h *HealthServer) HTTPStatus(r HealthReport) int {
	if r.Status == healthDown {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// Handler 返回只读的 HTTP 处理器。任何写方法一律 405。
func (h *HealthServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
			return
		}
		report := h.Report()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(h.HTTPStatus(report))
		_ = json.NewEncoder(w).Encode(report)
	})
	return mux
}

// Serve 在 addr 上起 HTTP 服务，ctx 取消后优雅关闭。
//
// 监听失败返回错误：健康端口被占用时静默降级，运维会以为服务健康。
func (h *HealthServer) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听健康检查端口 %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second}

	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("健康检查服务: %w", err)
	}
	return <-done
}

// aggregate 汇总四项结论。
func aggregate(statuses ...string) string {
	result := healthOK
	for _, s := range statuses {
		switch s {
		case healthDown:
			return healthDown
		case healthDegraded:
			result = healthDegraded
		}
	}
	return result
}

// probeOrDown 调用探测函数；未注入或返回空状态时按 down 处理。
func probeOrDown[T any](fn func() T, fallback func() T) T {
	if fn == nil {
		return fallback()
	}
	out := fn()
	if statusOf(out) == "" {
		return fallback()
	}
	return out
}

// statusOf 从分项里取 status。四项都内嵌 Check，用类型分支取，避免反射。
func statusOf(v any) string {
	switch c := v.(type) {
	case ConfigCheck:
		return c.Status
	case SerialCheck:
		return c.Status
	case MQTTCheck:
		return c.Status
	case CacheCheck:
		return c.Status
	}
	return ""
}

// CacheStatusOf 按三上限的使用率判定队列是否进入降级。
//
// 任一维度达预警线即降级：三上限是「哪个先到算哪个」，健康检查必须与预警事件同源
// （rules/edge-agent.md 健康检查：缓存达 80% → 降级）。
func CacheStatusOf(st CacheStats, spec CacheSpec, dropCount int64) CacheCheck {
	status := healthOK
	ratios := []float64{}
	if spec.MaxItems > 0 {
		ratios = append(ratios, float64(st.Items)/float64(spec.MaxItems))
	}
	if spec.MaxBytes > 0 {
		ratios = append(ratios, float64(st.Bytes)/float64(spec.MaxBytes))
	}
	if spec.MaxAge > 0 {
		ratios = append(ratios, st.AgeOfOldest.Seconds()/spec.MaxAge.Seconds())
	}
	warn := spec.WarnRatio
	if warn <= 0 {
		warn = DefaultCacheWarnRatio
	}
	for _, r := range ratios {
		if r >= warn {
			status = healthDegraded
			break
		}
	}
	return CacheCheck{
		Status:    status,
		Depth:     st.Items,
		Bytes:     st.Bytes,
		DropCount: dropCount,
	}
}

// SerialStatusOf 把总线可用性映射成分项。任一条 closed 即 down：
// 一条总线打不开时该总线上的测点全部采不到，报 200 会让运维以为只是个别设备故障。
func SerialStatusOf(buses map[string]bool) SerialCheck {
	status := healthOK
	out := make(map[string]string, len(buses))
	for id, open := range buses {
		if open {
			out[id] = "open"
			continue
		}
		out[id] = "closed"
		status = healthDown
	}
	return SerialCheck{Status: status, Buses: out}
}

// MQTTStatusOf 映射连接状态。断连是设计内状态：降级而不是不可用，
// 返回 503 会被编排重启容器，打断采集并丢掉还没落盘的数据。
func MQTTStatusOf(connected bool, lastSuccess int64) MQTTCheck {
	status := healthOK
	if !connected {
		status = healthDegraded
	}
	return MQTTCheck{Status: status, Connected: connected, LastSuccess: lastSuccess}
}
