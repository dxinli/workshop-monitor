// proto-adapter：平台侧接入服务。本文件只做参数解析、依赖装配与启动调用。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"workshop-monitor/internal/adapter/evaluate"
	"workshop-monitor/internal/adapter/ingest"
	"workshop-monitor/internal/config"
)

func main() {
	configPath := flag.String("config", "", "接入服务配置路径，如 config/adapter.json")
	flag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "缺少必填参数 -config")
		os.Exit(2)
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "proto-adapter 退出：%v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.LoadAdapter(configPath)
	if err != nil {
		return err
	}

	router, filter, err := buildRouter(cfg)
	if err != nil {
		return err
	}

	password, err := requireEnv("MQTT_WORKSHOP_ADAPTER_PASSWORD")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// 判定段必须在订阅之前重建活动集合并载入阈值：顺序反了，
	// 订阅后立刻到达的数据会对已告警测点重复拉响，制造假告警风暴。
	evalDeps, err := buildEvaluate(ctx, cfg.Queue.Capacity, evaluate.NewPGStore(pool))
	if err != nil {
		return err
	}
	startEvaluate(ctx, evalDeps)

	pipeline := newPipeline(ctx, cfg, router, pool)
	sub, err := ingest.NewSubscriber(subscriptionOf(cfg, filter, password), newHandler(pipeline, evalDeps))
	if err != nil {
		return err
	}

	if err := sub.Connect(ctx); err != nil {
		return err
	}
	fmt.Printf("已订阅 %s（方案 %s），等待上行报文\n", filter, cfg.TopicScheme)

	<-ctx.Done()
	sub.Disconnect(250)
	fmt.Printf("收到退出信号，已断开订阅；判定段 %s\n", describe(evalDeps))
	return nil
}

// startEvaluate 启动判定段的三个后台协程：队列消费、通知投递、阈值热加载。
//
// 每个协程只报错不退出：告警漏判的代价远大于错误日志刷屏，
// 而进程重启会丢掉去抖计数与活动集合，比带病运行更糟。
func startEvaluate(ctx context.Context, deps *evaluateDeps) {
	go deps.queue.Run(ctx, deps.eval, func(err error) {
		logErr("warn", "告警判定失败", err)
	})
	if deps.notify != nil {
		go deps.notify.Run(ctx)
	}
	go deps.eval.ReloadLoop(ctx, 60*time.Second, func(err error) {
		logErr("warn", "阈值重载失败，沿用上一份快照", err)
	})
	go dropWatcher(ctx, deps)
}

func logErr(level, msg string, err error) {
	fmt.Fprintf(os.Stderr, "level=%s svc=adapter inst=adapter msg=%q err=%q\n", level, msg, err.Error())
}

// newPipeline 组装接收段。巡检周期取采集周期：
// 判离线的窗口是 3 个周期，巡检再快也只是重复判定。
func newPipeline(ctx context.Context, cfg *config.Adapter, router *ingest.Router, pool *pgxpool.Pool) *ingest.Pipeline {
	p := ingest.NewPipeline(router, ingest.NewPGSink(pool), ingest.DefaultRetryPolicy())
	p.WithTermState(ingest.NewTermState(ingest.TermStateConfig{
		OfflinePeriods:  3,
		CollectInterval: time.Duration(cfg.CollectIntervalMs) * time.Millisecond,
	}), cfg.CollectIntervalMs/1000)
	go sweepLoop(ctx, p, cfg.CollectIntervalMs)
	return p
}

// buildRouter 由主题模板构造路由与订阅通配。两者必须来自同一份模板，否则会订阅到解析不了的主题。
func buildRouter(cfg *config.Adapter) (*ingest.Router, string, error) {
	templates, err := loadTopics(cfg)
	if err != nil {
		return nil, "", err
	}
	router, err := ingest.NewRouter(templates)
	if err != nil {
		return nil, "", err
	}
	filter, err := ingest.TopicFilterFromTemplates(templates)
	if err != nil {
		return nil, "", err
	}
	return router, filter, nil
}

func subscriptionOf(cfg *config.Adapter, filter, password string) ingest.SubscriptionConfig {
	return ingest.SubscriptionConfig{
		BrokerURL:         cfg.BrokerURL,
		ClientID:          cfg.ClientID,
		Username:          cfg.Username,
		Password:          password,
		TopicFilter:       filter,
		QoS:               cfg.QoS,
		KeepAlive:         time.Duration(cfg.KeepAliveSec) * time.Second,
		ConnectTimeout:    time.Duration(cfg.ConnectTimeoutSec) * time.Second,
		ReconnectInterval: time.Duration(cfg.ReconnectIntervalSec) * time.Second,
		MaxReconnect:      time.Duration(cfg.MaxReconnectSec) * time.Second,
	}
}

// sweepLoop 周期性巡检终端状态。数据超时不靠报文触发，只能靠定时器发现。
//
// 巡检与订阅回调并发访问判定器，TermState 内部加锁；写库走同一连接池。
func sweepLoop(ctx context.Context, p *ingest.Pipeline, intervalMs int) {
	ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := p.Sweep(ctx, now); err != nil {
				fmt.Fprintf(os.Stderr, "level=error svc=adapter inst=adapter msg=%q err=%q\n",
					"状态巡检失败", err.Error())
			}
		}
	}
}

// newHandler 把 Pipeline 包成订阅回调。失败只记日志，不中断订阅——
// 单条报文出错不该让整个接收段停摆。
//
// 投递判定只在入库成功后发生：入库失败时上层会把整条消息当失败处理，
// 此时把读数送进判定队列会让「没落库的数据参与告警」，重投时还会再判一次。
func newHandler(p *ingest.Pipeline, deps *evaluateDeps) func(context.Context, string, []byte) error {
	return func(ctx context.Context, topic string, payload []byte) error {
		res, err := p.Handle(ctx, topic, payload)
		logResult(res, err)
		if err == nil {
			dispatch(deps, res)
		}
		return err
	}
}

// loadTopics 按 topicScheme 选一套模板。主题字符串只来自 config/topics.json。
func loadTopics(cfg *config.Adapter) (ingest.TopicTemplates, error) {
	topics, err := config.LoadTopics(cfg.TopicsFile)
	if err != nil {
		return ingest.TopicTemplates{}, err
	}
	scheme, ok := topics.Schemes[cfg.TopicScheme]
	if !ok {
		return ingest.TopicTemplates{}, fmt.Errorf("topicsFile 中没有方案 %q", cfg.TopicScheme)
	}
	return ingest.TopicTemplates{
		Data:   scheme.DataUp,
		Event:  scheme.EventUp,
		Status: scheme.StatusUp,
	}, nil
}

func openPool(ctx context.Context, cfg *config.Adapter) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("解析 database.url: %w", err)
	}
	poolCfg.MaxConns = cfg.Database.MaxConns
	poolCfg.ConnConfig.ConnectTimeout = time.Duration(cfg.Database.ConnectTimeoutSec) * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("建立数据库连接池: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	return pool, nil
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("缺少环境变量 %s", key)
	}
	return v, nil
}

// logResult 按 AGENTS.md 的固定字段输出。采集循环不在此列，消息级日志是允许的。
func logResult(res ingest.Result, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "level=error svc=adapter inst=%s esn=%s seq=%d msg=%q err=%q status=%s\n",
			"adapter", res.TerminalID, res.Seq, "接收失败", err.Error(), res.Status)
		return
	}
	fmt.Printf("level=info svc=adapter inst=adapter esn=%s seq=%d msg_type=%s points=%d status=%s\n",
		res.TerminalID, res.Seq, res.MsgType, res.PointCount, res.Status)
}
