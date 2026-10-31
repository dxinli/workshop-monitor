package evaluate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// NotifyRequest 是一条通知请求，字段与详细设计 §4.1 一一对应。
type NotifyRequest struct {
	NotifyID     string  `json:"notifyId"`
	TerminalID   string  `json:"terminalId"`
	PointID      string  `json:"pointId"`
	AlarmType    string  `json:"alarmType"`
	Level        int     `json:"level"`
	TriggerValue float64 `json:"triggerValue"`
	OccurTime    int64   `json:"occurTime"`
	Scene        string  `json:"scene,omitempty"`
	Reason       string  `json:"reason,omitempty"`
}

// Sender 把通知交给编排层。唯一实现是 HTTPSender，单测用内存实现。
//
// 这里刻意不返回「是否应该重试」：重试责任在编排层，adapter 内不做重试（BR-41）。
type Sender interface {
	Send(ctx context.Context, req NotifyRequest) error
}

// NotifyQueue 是有界队列 + 单消费者。
//
// Deliver 非阻塞：队列满就丢弃并计数，绝不让判定协程等网络。
// 判定跟不上入库时告警会漏，这个计数是唯一能看出来这件事的地方。
type NotifyQueue struct {
	ch      chan NotifyRequest
	sender  Sender
	dropped atomic.Int64
	sent    atomic.Int64
	failed  atomic.Int64
}

// NewNotifyQueue 按容量建队列。容量非正时取 1024，避免无缓冲 channel 把 Deliver 变成阻塞调用。
func NewNotifyQueue(sender Sender, capacity int) *NotifyQueue {
	if capacity <= 0 {
		capacity = 1024
	}
	return &NotifyQueue{ch: make(chan NotifyRequest, capacity), sender: sender}
}

// Deliver 投递一条通知请求。不阻塞、不返回错误：调用方是判定协程，不能在这里等。
func (q *NotifyQueue) Deliver(req NotifyRequest) {
	select {
	case q.ch <- req:
	default:
		q.dropped.Add(1)
	}
}

// Dropped 返回被丢弃的通知请求数。
func (q *NotifyQueue) Dropped() int64 { return q.dropped.Load() }

// Sent 返回投递成功的请求数。
func (q *NotifyQueue) Sent() int64 { return q.sent.Load() }

// Failed 返回投递失败的请求数。
func (q *NotifyQueue) Failed() int64 { return q.failed.Load() }

// Run 单协程消费队列，直到 ctx 取消。
//
// 失败只计数，不重试、不回滚告警：编排层不可达时告警照常入库（BR-41）。
func (q *NotifyQueue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-q.ch:
			q.dispatch(ctx, req)
		}
	}
}

func (q *NotifyQueue) dispatch(ctx context.Context, req NotifyRequest) {
	// 短超时：投递慢不该让队列积压到判定侧。
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := q.sender.Send(sendCtx, req); err != nil {
		q.failed.Add(1)
		return
	}
	q.sent.Add(1)
}

// HTTPSender 把通知 POST 给编排层。只发不等业务结果，超时短，不重试。
type HTTPSender struct {
	client   *http.Client
	endpoint string
}

// NewHTTPSender 组装投递器。关闭通知时不建投递器，而不是建一个空转的——
// 空转的投递器会把「跳过」统计成「已发送」，健康检查看到的是假成功。
func NewHTTPSender(endpoint string, timeout time.Duration) *HTTPSender {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPSender{client: &http.Client{Timeout: timeout}, endpoint: endpoint}
}

// Send 投递一条通知。请求体是 §4.1 的扁平结构，编排层据此组装卡片。
func (s *HTTPSender) Send(ctx context.Context, req NotifyRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("序列化通知 %s: %w", req.NotifyID, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造通知请求 %s: %w", req.NotifyID, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("投递通知 %s: %w", req.NotifyID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 响应体丢掉也要读干净，否则连接无法复用。
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("投递通知 %s 返回 %d", req.NotifyID, resp.StatusCode)
	}
	return nil
}
