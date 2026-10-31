package agent

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"workshop-monitor/internal/model"
	"workshop-monitor/internal/pkglog"
)

// 状态报文的固定取值（接口规范 §6.5）。
const (
	StatusOnline  = "ONLINE"
	StatusOffline = "OFFLINE"

	ReasonConnected        = "CONNECTED"
	ReasonUnexpectedLoss   = "UNEXPECTED_DISCONNECT"
	ReasonGracefulShutdown = "GRACEFUL_SHUTDOWN"
)

// defaultEgressCapacity 是上报队列容量。
//
// 采集与上报之间必须隔一个有界队列：队列满时丢最旧的一批并计数，采集协程绝不等上报。
// 让采集等上报会把「上行慢」传染成「采集慢」，两条总线随之漂移，现场看到的现象
// 只是「周期不稳」，与真正的原因隔得很远（rules/edge-agent.md「采集调度」）。
const defaultEgressCapacity = 256

// Publisher 是上报段对 Broker 的唯一依赖面。生产实现是 MQTTLink；
// 测试用内存实现注入，这样「发了几条、报文头填了什么」能在不上 Broker 时验证。
// 接口只有一个方法：需要的是发布能力，不是连接对象的全部行为。
type Publisher interface {
	Publish(topic string, payload []byte, retain bool) error
}

// UplinkConfig 是上报段参数。全部由调用方注入，本包不读配置与环境变量。
type UplinkConfig struct {
	Esn          string
	Topics       UplinkTopics
	Alloc        *SeqAllocator
	Link         Publisher
	Log          *pkglog.Logger
	Capacity     int
	TimeSync     bool
	AgentVersion string
	OfflineLimit int
	Now          func() time.Time
}

// Uplink 组装三类上行报文：
//   - DATA 来自一个采集批
//   - EVENT 来自设备连续失败 / 恢复
//   - STATUS 来自自身连接状态（含遗嘱）
type Uplink struct {
	cfg   UplinkConfig
	watch *deviceWatch
	queue chan Batch
	drops atomic.Int64
	start time.Time
}

// NewUplink 校验装配参数。
func NewUplink(cfg UplinkConfig) (*Uplink, error) {
	switch {
	case cfg.Esn == "":
		return nil, fmt.Errorf("上报段缺少终端标识")
	case cfg.Alloc == nil:
		return nil, fmt.Errorf("上报段缺少序号分配器")
	case cfg.Link == nil:
		return nil, fmt.Errorf("上报段缺少 MQTT 连接")
	case cfg.Log == nil:
		return nil, fmt.Errorf("上报段缺少日志器")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Capacity <= 0 {
		cfg.Capacity = defaultEgressCapacity
	}
	return &Uplink{
		cfg:   cfg,
		watch: newDeviceWatch(cfg.OfflineLimit),
		queue: make(chan Batch, cfg.Capacity),
		start: time.Now(),
	}, nil
}

// Push 是采集协程的上行出口，非阻塞。队列满时丢最旧的一批并计数，绝不等待。
func (u *Uplink) Push(b Batch) {
	select {
	case u.queue <- b:
		return
	default:
	}
	select {
	case old := <-u.queue:
		u.noteDrop(old, "上报队列已满，丢弃最早一批")
	default:
	}
	select {
	case u.queue <- b:
	default:
		u.noteDrop(b, "上报队列已满，丢弃本批")
	}
}

// noteDrop 记一条丢弃日志。丢了多少必须能在日志里数出来，不许静默。
func (u *Uplink) noteDrop(b Batch, reason string) {
	u.drops.Add(1)
	u.cfg.Log.Error(reason,
		pkglog.F("esn", b.TerminalID), pkglog.F("busId", b.BusID),
		pkglog.F("collectTime", b.CollectTime),
		pkglog.F("droppedTotal", u.drops.Load()))
}

// Run 消费上报队列，直到 ctx 取消。留在队列里的批次不再补发：
// 持久化补传是 007 的职责，内存队列只负责把采集与上报解耦。
func (u *Uplink) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-u.queue:
			if err := u.SendBatch(b); err != nil {
				u.cfg.Log.Warn("数据上报失败",
					pkglog.F("esn", b.TerminalID), pkglog.F("busId", b.BusID),
					pkglog.F("collectTime", b.CollectTime), pkglog.F("err", err.Error()))
			}
		}
	}
}

// SendBatch 上报一个采集周期：先 DATA，再本批产生的 EVENT。
//
// 事件在数据之后取号：两类报文共用同一个序号空间（接口规范 §9.1 第 3 条）。
// 数据发不出去就返回错误（007 据此落盘）；事件失败只记日志——重试一条事件
// 不该挡住后续周期的数据，而且事件本身会在下一个周期重新判定。
func (u *Uplink) SendBatch(b Batch) error {
	if err := u.sendData(b); err != nil {
		return err
	}
	for _, ev := range u.watch.Observe(b) {
		if err := u.sendEvent(ev); err != nil {
			u.cfg.Log.Warn("事件上报失败",
				pkglog.F("esn", u.cfg.Esn), pkglog.F("eventType", ev.EventType),
				pkglog.F("busId", ev.BusID), pkglog.F("slaveAddr", ev.SlaveAddr),
				pkglog.F("err", err.Error()))
			continue
		}
		u.cfg.Log.Info("设备事件已上报",
			pkglog.F("esn", u.cfg.Esn), pkglog.F("eventType", ev.EventType),
			pkglog.F("busId", ev.BusID), pkglog.F("slaveAddr", ev.SlaveAddr),
			pkglog.F("reason", ev.Reason), pkglog.F("failureCount", ev.FailureCount))
	}
	return nil
}

// OnConnected 是连接（重连）成功后的回调：发布 ONLINE，Retain。
func (u *Uplink) OnConnected() error {
	if err := u.SendStatus(StatusOnline, ReasonConnected); err != nil {
		return fmt.Errorf("发布 ONLINE: %w", err)
	}
	return nil
}

// Shutdown 发布优雅退出状态。必须在断开连接之前调用——先断开会由 Broker 下发遗嘱，
// 退出原因就变成 UNEXPECTED_DISCONNECT。
func (u *Uplink) Shutdown() error {
	if err := u.SendStatus(StatusOffline, ReasonGracefulShutdown); err != nil {
		return fmt.Errorf("发布 OFFLINE: %w", err)
	}
	return nil
}

// SendStatus 发布状态报文。Retain：新订阅者要能立刻拿到终端最后状态。
func (u *Uplink) SendStatus(status, reason string) error {
	topic, payload, err := u.statusPayload(status, reason)
	if err != nil {
		return err
	}
	return u.cfg.Link.Publish(topic, payload, true)
}

// WillPayload 组装遗嘱：OFFLINE + UNEXPECTED_DISCONNECT。
//
// 每次建连都重新取号：同一份遗嘱在两次掉线里重放会撞上消息级幂等，
// 平台就只看得到第一次离线。
func (u *Uplink) WillPayload() (string, []byte, error) {
	return u.statusPayload(StatusOffline, ReasonUnexpectedLoss)
}

func (u *Uplink) sendData(b Batch) error {
	header, err := u.header(model.MsgTypeData, b.CollectTime)
	if err != nil {
		return err
	}
	body := DataBodyOf(b)
	payload, err := EncodeUplink(header, body)
	if err != nil {
		return err
	}
	if err := u.cfg.Link.Publish(u.cfg.Topics.Data, payload, false); err != nil {
		return fmt.Errorf("上报数据 seq=%d bus=%s: %w", header.Seq, b.BusID, err)
	}
	u.cfg.Log.Debug("数据已上报",
		pkglog.F("esn", u.cfg.Esn), pkglog.F("busId", b.BusID), pkglog.F("seq", header.Seq),
		pkglog.F("collectTime", b.CollectTime), pkglog.F("total", body.Stat.Total))
	return nil
}

func (u *Uplink) sendEvent(ev model.EventBody) error {
	header, err := u.header(model.MsgTypeEvent, ev.EventTime)
	if err != nil {
		return err
	}
	payload, err := EncodeUplink(header, ev)
	if err != nil {
		return err
	}
	if err := u.cfg.Link.Publish(u.cfg.Topics.Event, payload, false); err != nil {
		return fmt.Errorf("上报事件 %s: %w", ev.EventType, err)
	}
	return nil
}

func (u *Uplink) statusPayload(status, reason string) (string, []byte, error) {
	now := u.cfg.Now()
	header, err := u.header(model.MsgTypeStatus, now.UnixMilli())
	if err != nil {
		return "", nil, err
	}
	body := model.StatusBody{
		Status:     status,
		StatusTime: now.UnixMilli(),
		Reason:     reason,
		// 缓存深度与占用归 007；本期没有持久队列，如实上报 0。
		UptimeSec:    int64(now.Sub(u.start).Seconds()),
		AgentVersion: u.cfg.AgentVersion,
	}
	payload, err := EncodeUplink(header, body)
	if err != nil {
		return "", nil, err
	}
	return u.cfg.Topics.Status, payload, nil
}

// header 分配序号并组装报文头。
//
// sendTime 在这里取，collectTime 由调用方传入：补传时保留原 collectTime、
// 刷新 sendTime 才成立（接口规范 §10）。
func (u *Uplink) header(msgType string, collectTime int64) (model.Header, error) {
	seq, err := u.cfg.Alloc.Next()
	if err != nil {
		return model.Header{}, fmt.Errorf("分配序号: %w", err)
	}
	return HeaderFor(u.cfg.Esn, msgType, seq, collectTime, u.cfg.Now().UnixMilli(), u.cfg.TimeSync), nil
}
