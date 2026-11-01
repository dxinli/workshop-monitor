package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// 接入服务的健康检查（详细设计 §5 的 "接入服务追加判定段字段"）。
//
// 与采集侧同形：200 表示就绪或降级，503 表示不可用。判定段字段整体缺失时 503——
// 判定协程没起来，报文照样入库但告警永远不会响，这比进程崩溃更难发现。

// 三档结论。down 对应 503。
const (
	statusOK       = "ok"
	statusDegraded = "degraded"
	statusDown     = "down"
)

// EvaluateStats 是判定段暴露给健康检查的只读指标。
//
// 队列指标与判定指标来自两个对象（Queue 与 Evaluator），这里合成一份快照，
// 健康检查不直接持有它们的指针，方便在 seam 上替换。
type EvaluateStats struct {
	QueueDepth       int   `json:"queueDepth"`
	QueueCapacity    int   `json:"queueCapacity"`
	DropCount        int64 `json:"dropCount"`
	ActiveAlarmCount int   `json:"activeAlarmCount"`
	DebounceKeys     int   `json:"debounceKeys"`
	LastEvalTime     int64 `json:"lastEvalTime,omitempty"`
}

// Deps 是健康检查要读的外部状态。任一项为 nil 时按「判定段缺失」处理。
type Deps struct {
	// Evaluate 取判定段指标。nil 表示判定协程未装配。
	Evaluate func() EvaluateStats
	// ActiveInDB 读库里的未恢复告警数，用来与内存活动集合对账。
	ActiveInDB func(ctx context.Context) (int, error)
	// MQTTConnected 报告订阅连接是否可用。
	MQTTConnected func() bool
	// LastReceive 是最近一次成功处理报文的接收时刻（毫秒），0 表示还没收到过。
	LastReceive func() int64
}

// Report 是 /healthz 的响应体。
type Report struct {
	Status    string         `json:"status"`
	Service   string         `json:"service"`
	Instance  string         `json:"instance"`
	Version   string         `json:"version"`
	UptimeSec int64          `json:"uptimeSec"`
	Checks    map[string]any `json:"checks"`
}

// Check 是每项检查的公共部分。嵌进分项后 status 平铺到各自对象顶层。
type Check struct {
	Status string `json:"status"`
}

// EvaluateCheck 是判定段分项。
type EvaluateCheck struct {
	Check
	EvaluateStats
	// ActiveInDB 是库里未恢复的告警数。与 activeAlarmCount 不等即降级。
	ActiveInDB int `json:"activeAlarmCountInDB"`
	// Reason 说明降级原因，没有降级时为空。
	Reason string `json:"reason,omitempty"`
}

// MQTTCheck 是订阅连接分项。
type MQTTCheck struct {
	Check
	Connected   bool  `json:"connected"`
	LastReceive int64 `json:"lastReceive,omitempty"`
}

// Server 提供只读健康快照。
type Server struct {
	service  string
	instance string
	version  string
	start    time.Time
	now      func() time.Time
	deps     Deps

	mu           sync.Mutex
	lastDrop     int64
	growingTicks int
}

// New 装配健康检查。外部依赖全部由调用方注入。
func New(service, instance, version string, start time.Time, now func() time.Time, deps Deps) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{service: service, instance: instance, version: version,
		start: start, now: now, deps: deps}
}

// Report 取一次快照并给出结论。
func (s *Server) Report(ctx context.Context) Report {
	checks := map[string]any{}

	eval := s.evaluate(ctx)
	checks["evaluate"] = eval

	if s.deps.MQTTConnected == nil {
		checks["mqtt"] = MQTTCheck{Status: statusDown}
	} else {
		connected := s.deps.MQTTConnected()
		status := statusOK
		if !connected {
			status = statusDegraded
		}
		var last int64
		if s.deps.LastReceive != nil {
			last = s.deps.LastReceive()
		}
		checks["mqtt"] = MQTTCheck{Status: status, Connected: connected, LastReceive: last}
	}

	return Report{
		Status:    aggregate(eval.Status, mqttStatus(checks["mqtt"])),
		Service:   s.service,
		Instance:  s.instance,
		Version:   s.version,
		UptimeSec: int64(s.now().Sub(s.start).Seconds()),
		Checks:    checks,
	}
}

// evaluate 组装判定段分项。判定段缺失直接 down。
func (s *Server) evaluate(ctx context.Context) EvaluateCheck {
	if s.deps.Evaluate == nil {
		return EvaluateCheck{Status: statusDown, Reason: "判定段未装配"}
	}
	st := s.deps.Evaluate()

	out := EvaluateCheck{Status: statusOK}
	out.EvaluateStats = st
	if s.deps.ActiveInDB != nil {
		n, err := s.deps.ActiveInDB(ctx)
		switch {
		case err != nil:
			out.Status = statusDegraded
			out.Reason = "查询库中未恢复告警失败：" + err.Error()
		default:
			out.ActiveInDB = n
			if n != st.ActiveAlarmCount {
				// 内存集合与库不一致：可能是启动重建失败，也可能是中途写库异常。
				out.Status = statusDegraded
				out.Reason = fmt.Sprintf("活动告警数不一致：内存 %d，库 %d", st.ActiveAlarmCount, n)
			}
		}
	}

	// 「持续增长」按连续两次采样都增长判定：累计值只增不减，单次增长不足以说明跟不上，
	// 一次性的丢弃（例如启动瞬间的突刺）不该让健康检查一直报降级。
	growing := s.observeDrop(st.DropCount)
	if growing && out.Status == statusOK {
		out.Status = statusDegraded
		out.Reason = fmt.Sprintf("判定队列丢弃连续增长，累计 %d 条，告警会漏", st.DropCount)
	}
	return out
}

// growingTicksNeeded 是判「持续增长」需要的连续增长次数。
const growingTicksNeeded = 2

// observeDrop 记录一次丢弃计数采样，返回是否处于持续增长状态。
//
// 采样间隔由抓取方决定，因此这里不按时间窗算速率，只数连续次数：
// 抓到两次都在涨即为持续，任何一次持平都清零。
func (s *Server) observeDrop(count int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if count > s.lastDrop {
		s.growingTicks++
	} else {
		s.growingTicks = 0
	}
	s.lastDrop = count
	return s.growingTicks >= growingTicksNeeded
}

// mqttStatus 取分项里的 status，供 aggregate 用。
func mqttStatus(v any) string {
	if c, ok := v.(MQTTCheck); ok {
		return c.Status
	}
	return statusDown
}

// aggregate 汇总结论：任一项 down 即 down，否则任一项 degraded 即 degraded。
func aggregate(statuses ...string) string {
	out := statusOK
	for _, s := range statuses {
		switch s {
		case statusDown:
			return statusDown
		case statusDegraded:
			out = statusDegraded
		}
	}
	return out
}

// HTTPStatus 把结论映射成状态码。
func HTTPStatus(r Report) int {
	if r.Status == statusDown {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

// Handler 返回只读处理器。任何写方法一律 405。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "只支持 GET", http.StatusMethodNotAllowed)
			return
		}
		report := s.Report(r.Context())
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(HTTPStatus(report))
		_ = json.NewEncoder(w).Encode(report)
	})
	return mux
}

// Serve 在 addr 上起 HTTP 服务，ctx 取消后优雅关闭。监听失败返回错误：
// 端口被占用时静默降级，运维会以为服务健康。
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听健康检查端口 %s: %w", addr, err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}

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
