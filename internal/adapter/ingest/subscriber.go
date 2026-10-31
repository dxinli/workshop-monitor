package ingest

import (
	"context"
	"fmt"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// SubscriptionConfig 是订阅端参数。全部由调用方从环境变量或配置传入，本包不读环境。
type SubscriptionConfig struct {
	BrokerURL string
	ClientID  string
	Username  string
	Password  string
	// TopicFilter 是订阅通配，形如 "/edge/+/+/up"。由主题模板派生，不在代码里写死。
	TopicFilter string
	QoS         byte
	KeepAlive   time.Duration
	// ConnectTimeout 与 ReconnectInterval 对应「自动重连，指数退避上限 30 s」。
	ConnectTimeout    time.Duration
	ReconnectInterval time.Duration
	MaxReconnect      time.Duration
}

// Subscriber 是 MQTT 订阅端的唯一实现。
//
// 不做业务判定：收到消息只交给 handler，由 Pipeline 完成解码与落库。
// 持久会话（clean session = false）保证 Broker 侧保留订阅与离线消息。
type Subscriber struct {
	cfg     SubscriptionConfig
	handler func(ctx context.Context, topic string, payload []byte) error
	client  mqtt.Client
}

// NewSubscriber 组装订阅端。handler 为 nil 视为装配错误。
func NewSubscriber(cfg SubscriptionConfig, handler func(context.Context, string, []byte) error) (*Subscriber, error) {
	if cfg.BrokerURL == "" {
		return nil, fmt.Errorf("订阅配置缺少 BrokerURL")
	}
	if cfg.TopicFilter == "" {
		return nil, fmt.Errorf("订阅配置缺少 TopicFilter")
	}
	if handler == nil {
		return nil, fmt.Errorf("订阅配置缺少消息处理器")
	}
	if cfg.QoS > 1 {
		return nil, fmt.Errorf("QoS 只支持 0/1，实际 %d", cfg.QoS)
	}
	return &Subscriber{cfg: cfg, handler: handler}, nil
}

// Connect 建立连接并订阅。连接失败即返回错误，由调用方决定是否退出。
//
// 不在这里重试建连：启动阶段失败就该让进程退出，带病启动会产出难以解释的错值。
// 断线后的重连由 paho 的 AutoReconnect 负责。
func (s *Subscriber) Connect(ctx context.Context) error {
	opts := mqtt.NewClientOptions().
		AddBroker(s.cfg.BrokerURL).
		SetClientID(s.cfg.ClientID).
		SetUsername(s.cfg.Username).
		SetPassword(s.cfg.Password).
		SetCleanSession(false).
		SetKeepAlive(s.cfg.KeepAlive).
		SetConnectTimeout(s.cfg.ConnectTimeout).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(s.cfg.MaxReconnect).
		SetConnectRetryInterval(s.cfg.ReconnectInterval)

	client := mqtt.NewClient(opts)
	token := client.Connect()
	select {
	case <-ctx.Done():
		client.Disconnect(250)
		return fmt.Errorf("建立 MQTT 连接被取消: %w", ctx.Err())
	case <-token.Done():
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("连接 Broker %s: %w", s.cfg.BrokerURL, err)
	}

	sub := client.Subscribe(s.cfg.TopicFilter, s.cfg.QoS, s.dispatch)
	select {
	case <-ctx.Done():
		client.Disconnect(250)
		return fmt.Errorf("订阅 %s 被取消: %w", s.cfg.TopicFilter, ctx.Err())
	case <-sub.Done():
	}
	if err := sub.Error(); err != nil {
		client.Disconnect(250)
		return fmt.Errorf("订阅 %s: %w", s.cfg.TopicFilter, err)
	}

	s.client = client
	return nil
}

// dispatch 在 paho 的回调协程里执行，不阻塞：handler 内部完成落库，失败只返回错误。
// paho 不提供背压，因此 handler 必须是短路径；数据库重试最多 3 次、合计 6 秒。
//
// handler 的错误由调用方自己记日志（cmd 装配时注入），这里不做二次处理。
func (s *Subscriber) dispatch(_ mqtt.Client, msg mqtt.Message) {
	_ = s.handler(context.Background(), msg.Topic(), msg.Payload())
}

// Disconnect 断开连接。优雅退出时调用，断开后 Broker 才会下发 LWT 之外的状态。
func (s *Subscriber) Disconnect(quiesceMs uint) {
	if s.client != nil {
		s.client.Disconnect(quiesceMs)
	}
}