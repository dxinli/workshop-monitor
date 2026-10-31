package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"workshop-monitor/internal/config"
	"workshop-monitor/internal/model"
	"workshop-monitor/internal/pkglog"
)

// 重试上限（`rules/edge-agent.md` 错误与质量码表）：
//   - 超时：点表 retryCount（默认 2），即最多 3 次尝试
//   - CRC：固定 1 次，即最多 2 次尝试
//   - 设备异常应答与帧格式不符：不重试，重试不会让从站改变答复
const crcRetries = 1

// Batch 是一条总线一个采集周期的产出。seq 由 006 分配，本任务不产出。
type Batch struct {
	TerminalID  string         `json:"terminalId"`
	BusID       string         `json:"busId"`
	CollectTime int64          `json:"collectTime"`
	Points      []PointReading `json:"points"`
	Good        int            `json:"good"`
	Bad         int            `json:"bad"`
}

// PointReading 是一个测点的一次读数。失败点同样在列表里，Value 为 nil。
type PointReading struct {
	Code      string   `json:"code"`
	SlaveAddr int      `json:"slaveAddr"`
	SiType    string   `json:"siType"`
	Unit      string   `json:"unit"`
	Value     *float64 `json:"value"`
	Quality   int      `json:"quality"`
}

// BusPoller 是单条总线的轮询协程。持有自己的串口句柄与点表，不与其它总线共享状态。
type BusPoller struct {
	terminal *config.Terminal
	bus      *config.Bus
	port     io.ReadWriter
	sink     func(Batch)
	now      func() time.Time
}

// NewBusPoller 装配一条总线的轮询器。port 由调用方打开并负责关闭——
// 串口是外部资源，不在模块内部创建（AGENTS.md Must Follow）。
func NewBusPoller(term *config.Terminal, bus *config.Bus, port io.ReadWriter, sink func(Batch)) *BusPoller {
	return &BusPoller{
		terminal: term,
		bus:      bus,
		port:     port,
		sink:     sink,
		now:      time.Now,
	}
}

// Run 按采集周期轮询，直到 ctx 取消。采集循环不等待 sink 以外的任何环节；
// 上行断开与否由 sink 自己处理（006）。
func (p *BusPoller) Run(ctx context.Context) {
	interval := time.Duration(p.terminal.CollectIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sink(p.Collect())
		}
	}
}

// Collect 跑一个完整周期：从站地址升序，每从站按寄存器块发一次请求。
//
// collectTime 取本批**开始**时刻。逐块取时间会让同批测点时间不一致，曲线出现锯齿。
func (p *BusPoller) Collect() Batch {
	start := p.now()
	batch := Batch{
		TerminalID:  p.terminal.TerminalID,
		BusID:       p.bus.BusID,
		CollectTime: start.UnixMilli(),
	}

	devices := p.sortedDevices()
	for i := range devices {
		dev := &devices[i]
		for j := range dev.Blocks {
			blk := &dev.Blocks[j]
			words, err := p.readBlock(dev.SlaveAddr, blk)
			batch.Points = append(batch.Points, p.decodeBlock(dev, blk, words, err)...)
		}
	}
	// good 只算质量码 0。超量程(5)是「可读但异常」，质量看板上必须能与正常值区分，
	// 计入 good 会让「这一批全好」掩盖越限。
	for _, pt := range batch.Points {
		if pt.Quality == model.QualityGood {
			batch.Good++
		} else {
			batch.Bad++
		}
	}
	return batch
}

// sortedDevices 复制一份按从站地址升序的设备列表，不改动点表本身（多条总线共享终端结构体）。
func (p *BusPoller) sortedDevices() []config.Device {
	devices := append([]config.Device(nil), p.bus.Devices...)
	sort.Slice(devices, func(i, j int) bool { return devices[i].SlaveAddr < devices[j].SlaveAddr })
	return devices
}

// readBlock 带重试地读一个寄存器块。
func (p *BusPoller) readBlock(slave int, blk *config.Block) ([]uint16, error) {
	startAddr := 0
	if blk.StartAddr != nil {
		startAddr = *blk.StartAddr
	}
	timeout := time.Duration(p.bus.TimeoutMs) * time.Millisecond
	interFrame := time.Duration(p.bus.InterFrameMs) * time.Millisecond

	timeoutLeft := p.bus.RetryCount
	crcLeft := crcRetries
	for {
		words, err := exchange(p.port, slave, startAddr, blk.RegisterCount, timeout, interFrame)
		if err == nil {
			return words, nil
		}
		if retryable(err, &timeoutLeft, &crcLeft) {
			continue
		}
		return nil, err
	}
}

// retryable 消耗一次对应的重试额度。额度用尽后返回 false，错误交给调用方标记质量码。
func retryable(err error, timeoutLeft, crcLeft *int) bool {
	if errors.Is(err, ErrCRC) && *crcLeft > 0 {
		*crcLeft--
		return true
	}
	if errors.Is(err, ErrTimeout) && *timeoutLeft > 0 {
		*timeoutLeft--
		return true
	}
	return false
}

// decodeBlock 把寄存器块转成测点读数。整块失败时块内每个点都进 batch、Value 为 nil——
// 省略失败点会让「没采」和「没这个点」无法区分（接口规范 §6.2 强制规则 1）。
func (p *BusPoller) decodeBlock(dev *config.Device, blk *config.Block, words []uint16, readErr error) []PointReading {
	out := make([]PointReading, 0, len(blk.Points))
	for i := range blk.Points {
		pt := &blk.Points[i]
		reading := PointReading{Code: pt.Code, SlaveAddr: dev.SlaveAddr, SiType: pt.SiType}
		if pt.Unit != nil {
			reading.Unit = *pt.Unit
		}
		if readErr != nil {
			reading.Quality = qualityFromError(readErr)
			out = append(out, reading)
			continue
		}
		addr := 0
		if pt.Addr != nil {
			addr = *pt.Addr
		}
		if addr+pt.Width > len(words) {
			reading.Quality = model.QualityParseError
			out = append(out, reading)
			continue
		}
		reading.Value, reading.Quality = toEngineering(pt, words[addr:addr+pt.Width])
		out = append(out, reading)
	}
	return out
}

// qualityFromError 把通信错误映射成质量码（接口规范 §3.6）。边缘只产生 0~5。
func qualityFromError(err error) int {
	var ex *ExceptionError
	switch {
	case err == nil:
		return model.QualityGood
	case errors.Is(err, ErrTimeout):
		return model.QualityTimeout
	case errors.As(err, &ex):
		return model.QualityDeviceException
	case errors.Is(err, ErrCRC):
		return model.QualityCRCError
	default:
		return model.QualityParseError
	}
}

// LogSink 把批次打成周期聚合日志并返回给上报段。005 还没有上报段，它作为唯一出口；
// 006 起可以串在它前面（先记日志再上报），不需要改采集循环。
//
// 不逐点打印成功：140 点 × 5 秒会淹没日志（详细设计 §7.3）。失败点逐条打，否则现场
// 只知道「这一批有坏点」不知道是哪个。
func LogSink(log *pkglog.Logger) func(Batch) {
	return func(batch Batch) {
		log.Info("采集周期完成",
			pkglog.F("esn", batch.TerminalID),
			pkglog.F("busId", batch.BusID),
			pkglog.F("collectTime", batch.CollectTime),
			pkglog.F("total", len(batch.Points)),
			pkglog.F("good", batch.Good),
			pkglog.F("bad", batch.Bad),
		)
		// debug 级别才逐点打印：它是验收用的取证手段，日常运行会淹没日志。
		// info 级别下这几行被 pkglog 直接过滤，连字符串都不拼。
		for _, pt := range batch.Points {
			log.Debug("测点读数",
				pkglog.F("esn", batch.TerminalID),
				pkglog.F("busId", batch.BusID),
				pkglog.F("collectTime", batch.CollectTime),
				pkglog.F("pointId", fmt.Sprintf("%s.%d.%s", batch.BusID, pt.SlaveAddr, pt.Code)),
				pkglog.F("value", pt.Value),
				pkglog.F("quality", pt.Quality),
			)
		}
		for _, pt := range batch.Points {
			if pt.Quality == model.QualityGood {
				continue
			}
			log.Warn("测点采集失败",
				pkglog.F("esn", batch.TerminalID),
				pkglog.F("busId", batch.BusID),
				pkglog.F("collectTime", batch.CollectTime),
				pkglog.F("pointId", fmt.Sprintf("%s.%d.%s", batch.BusID, pt.SlaveAddr, pt.Code)),
				pkglog.F("quality", pt.Quality),
			)
		}
	}
}
