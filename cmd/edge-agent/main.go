// edge-agent：车间终端上的采集进程。
// 本文件只做参数解析、依赖装配与启动调用，业务在 internal/agent。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"workshop-monitor/internal/agent"
	"workshop-monitor/internal/config"
	"workshop-monitor/internal/pkglog"
)

// agentVersion 写进状态报文的 agentVersion 字段，供平台核对终端版本。
const agentVersion = "1.0.0"

// defaultOfflineLimit 是判设备离线的连续失败次数（接口规范 §6.4 固定 5）。
const defaultOfflineLimit = 5

func main() {
	configPath := flag.String("config", "", "终端点表配置路径，如 config/terminals/ESN-ZX-0001.json")
	portsPath := flag.String("ports", "", "串口端点表（仿真用），不填则按点表里的 port 打开")
	topicsPath := flag.String("topics", "config/topics.json", "主题模板文件，主题字符串的唯一来源")
	seqPath := flag.String("seq", "var/seq.state", "序号高水位文件；空串表示不持久化（重启会归零）")
	logLevel := flag.String("log-level", "info", "日志级别 debug / info / warn / error")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "缺少必填参数 -config")
		os.Exit(2)
	}

	level, err := pkglog.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	opts := options{
		configPath: *configPath,
		portsPath:  *portsPath,
		topicsPath: *topicsPath,
		seqPath:    *seqPath,
	}
	if err := run(opts, level); err != nil {
		fmt.Fprintf(os.Stderr, "edge-agent 退出：%v\n", err)
		os.Exit(1)
	}
}

type options struct {
	configPath string
	portsPath  string
	topicsPath string
	seqPath    string
}

// agentRuntime 是装配好的运行期依赖，只在 run 内使用。
type agentRuntime struct {
	term     *config.Terminal
	log      *pkglog.Logger
	settings uplinkSettings
	buses    []io.ReadWriter
	alloc    *agent.SeqAllocator
	link     *agent.MQTTLink
	uplink   *agent.Uplink
}

// run 按 rules/edge-agent.md 的启动顺序装配：
// 读配置 → 校验 → 开串口 → 读序号高水位 → 建 MQTT 并注册遗嘱 → 起轮询与上报 → 等信号。
//
// 任一步失败直接返回错误，不降级启动：带病启动会产出难以解释的错值。
// 本地持久队列（007）与 /healthz（015）不在本任务范围。
func run(opts options, level pkglog.Level) error {
	rt, closeAll, err := setup(opts, level)
	if err != nil {
		return err
	}
	defer closeAll()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 建连先于起采集：遗嘱必须在 Broker 上注册好，否则进程在建连前崩溃就没人知道它离线过。
	if err := rt.link.Connect(ctx); err != nil {
		return err
	}
	rt.logStarted()

	var wg sync.WaitGroup
	rt.startWorkers(ctx, &wg)

	<-ctx.Done()
	wg.Wait()
	rt.shutdown()
	return nil
}

// setup 完成建连之前的所有装配：配置、串口、序号、上报段与连接对象。
func setup(opts options, level pkglog.Level) (*agentRuntime, func(), error) {
	term, err := config.LoadTerminal(opts.configPath)
	if err != nil {
		return nil, nil, err
	}
	if err := checkConfig(term, opts.configPath); err != nil {
		return nil, nil, err
	}

	log := pkglog.New("agent", term.TerminalID, level, os.Stderr)

	settings, err := loadSettings(term, opts.topicsPath)
	if err != nil {
		return nil, nil, err
	}
	ports, err := loadPorts(opts.portsPath)
	if err != nil {
		return nil, nil, err
	}
	buses, closeAll, err := openBuses(term, ports)
	if err != nil {
		return nil, nil, err
	}

	alloc, err := agent.LoadSeqAllocator(opts.seqPath)
	if err != nil {
		closeAll()
		return nil, nil, err
	}

	rt := &agentRuntime{term: term, log: log, settings: settings, buses: buses, alloc: alloc}
	if err := rt.connect(); err != nil {
		closeAll()
		return nil, nil, err
	}
	return rt, closeAll, nil
}

// connect 组装上报段与 MQTT 连接。
//
// 两者互相引用：遗嘱由上报段组装，发布又要走连接。用闭包接上——
// 连接真正调用这两个回调是在 Connect 之后，那时 uplift 已装配完毕。
func (rt *agentRuntime) connect() error {
	var u *agent.Uplink
	link, err := agent.NewMQTTLink(rt.settings.mqtt,
		func() (string, []byte, error) { return u.WillPayload() },
		func() error { return u.OnConnected() }, rt.log)
	if err != nil {
		return err
	}
	if u, err = newUplink(rt.term, rt.settings, rt.alloc, link, rt.log); err != nil {
		return err
	}
	rt.link = link
	rt.uplink = u
	return nil
}

// startWorkers 起连接守护、上报消费与每总线一个轮询协程。
//
// 采集出口先把批次记成周期聚合日志再入上行队列：两条都是非阻塞的，
// 日志留在本地、数据走上行，现场排查与平台取证各取所需（005 的偏差 5）。
func (rt *agentRuntime) startWorkers(ctx context.Context, wg *sync.WaitGroup) {
	for _, fn := range []func(context.Context){
		rt.link.Run,
		rt.uplink.Run,
	} {
		wg.Add(1)
		go func(f func(context.Context)) {
			defer wg.Done()
			f(ctx)
		}(fn)
	}

	sink := func(b agent.Batch) {
		agent.LogSink(rt.log)(b)
		rt.uplink.Push(b)
	}
	for i := range rt.term.Buses {
		poller := agent.NewBusPoller(rt.term, &rt.term.Buses[i], rt.buses[i], sink)
		wg.Add(1)
		go func() {
			defer wg.Done()
			poller.Run(ctx)
		}()
	}
}

// shutdown 先发优雅退出状态再断开连接：顺序反了，Broker 会下发遗嘱，
// 退出原因就变成 UNEXPECTED_DISCONNECT。
func (rt *agentRuntime) shutdown() {
	if err := rt.uplink.Shutdown(); err != nil {
		rt.log.Warn("优雅退出状态发布失败", pkglog.F("err", err.Error()))
	}
	rt.link.Disconnect(250)
	rt.log.Info("采集已停止", pkglog.F("esn", rt.term.TerminalID),
		pkglog.F("lastSeq", rt.alloc.Current()))
}

func (rt *agentRuntime) logStarted() {
	rt.log.Info("采集已启动", pkglog.F("esn", rt.term.TerminalID),
		pkglog.F("configVersion", rt.term.ConfigVersion),
		pkglog.F("buses", len(rt.term.Buses)),
		pkglog.F("collectIntervalMs", rt.term.CollectIntervalMs),
		pkglog.F("topicScheme", rt.settings.scheme),
		pkglog.F("dataTopic", rt.settings.topics.Data),
		pkglog.F("lastSeq", rt.alloc.Current()))
}

// newUplink 组装上报段。
//
// TimeSync 固定为 true：是否已同步时钟由平台按 collectTime 与接收时间判定，
// 终端侧没有可信参照时钟时无法自证。置 false 的开关不在本任务范围（见偏差）。
func newUplink(term *config.Terminal, s uplinkSettings, alloc *agent.SeqAllocator,
	link agent.Publisher, log *pkglog.Logger) (*agent.Uplink, error) {
	return agent.NewUplink(agent.UplinkConfig{
		Esn:          term.TerminalID,
		Topics:       s.topics,
		Alloc:        alloc,
		Link:         link,
		Log:          log,
		TimeSync:     true,
		AgentVersion: agentVersion,
		OfflineLimit: defaultOfflineLimit,
	})
}

// checkConfig 跑 V-01~V-09 并把每条问题打到 stderr，含字段与原始值（FR-E10）。
func checkConfig(term *config.Terminal, path string) error {
	report := config.ValidateTerminal(term)
	for _, issue := range report.Issues {
		if issue.Sev == config.Fatal {
			fmt.Fprintf(os.Stderr, "%s\n", issue)
		}
	}
	if report.Failed() {
		return fmt.Errorf("配置校验未通过（%s），拒绝启动", path)
	}
	return nil
}
