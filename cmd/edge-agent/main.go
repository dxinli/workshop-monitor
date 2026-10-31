// edge-agent：车间终端上的采集进程。
// 本文件只做参数解析、依赖装配与启动调用，业务在 internal/agent。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"workshop-monitor/internal/agent"
	"workshop-monitor/internal/config"
	"workshop-monitor/internal/pkglog"
)

func main() {
	configPath := flag.String("config", "", "终端点表配置路径，如 config/terminals/ESN-ZX-0001.json")
	portsPath := flag.String("ports", "", "串口端点表（仿真用），不填则按点表里的 port 打开")
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

	if err := run(*configPath, *portsPath, level); err != nil {
		fmt.Fprintf(os.Stderr, "edge-agent 退出：%v\n", err)
		os.Exit(1)
	}
}

// run 按 rules/edge-agent.md 的启动顺序装配：读配置 → 校验 → 开串口 → 起轮询协程 → 等信号。
// 005 不含本地队列、MQTT 与 /healthz（007 / 006 / 015）；任一步失败直接返回错误，不降级启动。
func run(configPath, portsPath string, level pkglog.Level) error {
	term, err := config.LoadTerminal(configPath)
	if err != nil {
		return err
	}
	if err := checkConfig(term, configPath); err != nil {
		return err
	}

	log := pkglog.New("agent", term.TerminalID, level, os.Stderr)

	ports, err := loadPorts(portsPath)
	if err != nil {
		return err
	}

	buses, closeAll, err := openBuses(term, ports)
	if err != nil {
		return err
	}
	defer closeAll()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sink := agent.LogSink(log)
	var wg sync.WaitGroup
	for i := range term.Buses {
		poller := agent.NewBusPoller(term, &term.Buses[i], buses[i], sink)
		wg.Add(1)
		go func() {
			defer wg.Done()
			poller.Run(ctx)
		}()
	}

	log.Info("采集已启动", pkglog.F("esn", term.TerminalID),
		pkglog.F("configVersion", term.ConfigVersion),
		pkglog.F("buses", len(term.Buses)),
		pkglog.F("collectIntervalMs", term.CollectIntervalMs))

	<-ctx.Done()
	wg.Wait()
	log.Info("采集已停止", pkglog.F("esn", term.TerminalID))
	return nil
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
