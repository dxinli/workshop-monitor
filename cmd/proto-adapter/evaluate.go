package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"workshop-monitor/internal/adapter/evaluate"
	"workshop-monitor/internal/adapter/ingest"
	"workshop-monitor/internal/model"
)

// notifySettings 是通知投递的三项参数。走环境变量，与编排层地址一致：
// 地址会随部署变化，写进配置文件就得随仓库一起改。
type notifySettings struct {
	Endpoint string
	Enabled  bool
	Timeout  time.Duration
}

func loadNotifySettings() notifySettings {
	return notifySettings{
		Endpoint: os.Getenv("NOTIFY_ENDPOINT"),
		Enabled:  os.Getenv("NOTIFY_ENABLED") != "false",
		Timeout:  5 * time.Second,
	}
}

// evaluateDeps 是判定段的装配结果。接收段只通过 Queue 往里投递。
type evaluateDeps struct {
	queue  *evaluate.Queue
	notify *evaluate.NotifyQueue
	eval   *evaluate.Evaluator
}

// buildEvaluate 装配判定段：阈值快照 → 活动集合重建 → 队列与通知出口。
//
// 重建必须在订阅之前完成（见 run），否则订阅后立即到达的数据会对已告警测点重复拉响。
func buildEvaluate(ctx context.Context, capacity int, store evaluate.Store) (*evaluateDeps, error) {
	ns := loadNotifySettings()

	// 通知关闭时判定段拿不到出口：告警照常入库，不发也不计数，
	// 否则「跳过」会被统计成「已发送」，健康检查看到的是假的成功数。
	var notifyQ *evaluate.NotifyQueue
	var notifier evaluate.Notifier
	if ns.Enabled && ns.Endpoint != "" {
		// 通知队列与判定队列同容量：两者都是「满则丢弃并计数」，
		// 拆成两个配置项只会让现场多一处需要调的数。
		notifyQ = evaluate.NewNotifyQueue(evaluate.NewHTTPSender(ns.Endpoint, ns.Timeout), capacity)
		notifier = notifyQ
	}

	queue := evaluate.NewQueue(capacity)
	ev := evaluate.NewEvaluator(store, notifier)

	if err := ev.Reload(ctx); err != nil {
		return nil, err
	}
	if err := ev.Bootstrap(ctx); err != nil {
		return nil, err
	}
	return &evaluateDeps{queue: queue, notify: notifyQ, eval: ev}, nil
}

// dispatch 把一条报文的读数投递进判定队列。
//
// 只投递 OK 的 DATA：重复消息的 Rows 仍带着解码结果，
// 投进去会让同一批读数被数两次，去抖计数提前达标而误报。
func dispatch(deps *evaluateDeps, res ingest.Result) {
	if deps == nil || res.Status != ingest.StatusAccepted ||
		res.MsgType != model.MsgTypeData || len(res.Rows) == 0 {
		return
	}
	for _, r := range res.Rows {
		deps.queue.Push(evaluate.Sample{
			TerminalID:  r.TerminalID,
			BusID:       r.BusID,
			PointID:     r.PointID,
			PointCode:   r.PointCode,
			Value:       r.Value,
			Quality:     r.Quality,
			CollectTime: r.CollectTime,
			Seq:         r.Seq,
			Backfill:    r.Backfill,
		})
	}
}

// describe 汇总判定段指标，写在启动与退出日志里。
func describe(deps *evaluateDeps) string {
	st := deps.eval.Stats()
	if deps.notify == nil {
		return fmt.Sprintf("active=%d debounce=%d notify=disabled", st.ActiveAlarmCount, st.DebounceKeys)
	}
	return fmt.Sprintf("active=%d debounce=%d notifySent=%d notifyFailed=%d notifyDropped=%d",
		st.ActiveAlarmCount, st.DebounceKeys,
		deps.notify.Sent(), deps.notify.Failed(), deps.notify.Dropped())
}

// maxDropLog 是丢弃计数的上报间隔：每满这么多条打一行，不是每条都打。
const maxDropLog int64 = 1000

// dropWatcher 定期检查丢弃计数，每满 1000 条上报一次。
//
// 判定跟不上入库是「告警会漏」的唯一信号，静默丢弃不可接受；
// 但每条都打日志会在积压时刷屏，所以按千条聚合。
func dropWatcher(ctx context.Context, deps *evaluateDeps) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var reported int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dropped := deps.queue.Dropped()
			if dropped-reported < maxDropLog {
				continue
			}
			reported = dropped
			fmt.Fprintf(os.Stderr, "level=warn svc=adapter inst=adapter msg=%q dropped=%d depth=%d capacity=%d\n",
				"判定队列丢弃，告警会漏", dropped, deps.queue.Depth(), deps.queue.Capacity())
		}
	}
}
