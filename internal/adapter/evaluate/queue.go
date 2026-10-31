package evaluate

import (
	"context"
	"sync/atomic"

	"workshop-monitor/internal/model"
)

// Queue 是接收段与判定段之间唯一的有界 channel。
//
// 两边互不 import：接收段只调用 Push，判定段只调用 Run。
// 满时丢弃并计数，绝不阻塞入库——入库比判定重要，判定慢了记下来再去查。
type Queue struct {
	ch       chan Sample
	dropped  atomic.Int64
	received atomic.Int64
}

// NewQueue 按容量建队列。容量非正时取 4096，与详细设计 A9 的取值一致。
func NewQueue(capacity int) *Queue {
	if capacity <= 0 {
		capacity = 4096
	}
	return &Queue{ch: make(chan Sample, capacity)}
}

// Push 投递一条读数。非阻塞：满则丢弃并计数。
//
// 质量非 GOOD 的读数在入口就被丢掉，不占队列——它注定不参与求值，
// 让它挤掉一条可用读数只是让告警更容易漏。
func (q *Queue) Push(s Sample) {
	if s.Quality != model.QualityGood || s.Value == nil {
		return
	}
	select {
	case q.ch <- s:
		q.received.Add(1)
	default:
		q.dropped.Add(1)
	}
}

// Dropped 返回被丢弃的读数条数。持续增长表示判定跟不上入库，告警会漏。
func (q *Queue) Dropped() int64 { return q.dropped.Load() }

// Depth 返回当前积压条数。
func (q *Queue) Depth() int { return len(q.ch) }

// Capacity 返回队列容量。
func (q *Queue) Capacity() int { return cap(q.ch) }

// Run 单协程串行消费，直到 ctx 取消。判定协程必须唯一，这是不加锁的前提。
//
// 单条判定失败只上报，不中断消费：一条写不进去不该让整条判定链停摆，
// 队列会继续排空，后续读数照常判定。
func (q *Queue) Run(ctx context.Context, ev *Evaluator, onErr func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-q.ch:
			if err := ev.Evaluate(ctx, s); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
