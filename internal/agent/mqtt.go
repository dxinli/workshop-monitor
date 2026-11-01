package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"workshop-monitor/internal/pkglog"
)

// MQTTConfig 是连接参数（接口规范 §4.1）。全部由调用方传入，本包不读环境变量。
type MQTTConfig struct {
	BrokerURL string
	ClientID  string
	Username  string
	Password  string
	QoS       byte
	// KeepAlive 取 30 秒：3 个上报周期内可识别掉线。
	KeepAlive      time.Duration
	ConnectTimeout time.Duration
	PubAckTimeout  time.Duration
	// ReconnectMin / ReconnectMax 是指数退避的起止值：1s → 2s → 4s … 上限 30s。
	ReconnectMin time.Duration
	ReconnectMax time.Duration
}

// WillFunc 在每次建连时组装一份新的遗嘱载荷。
//
// 必须是函数而不是固定字节：遗嘱占用一个序号，同一条遗嘱在两次掉线里重放会撞上
// 消息级幂等被判成重复，平台就永远看不到第二次离线。
type WillFunc func() (topic string, payload []byte, err error)

// MQTTLink 维持一条到 Broker 的连接：建连、注册遗嘱、断线后指数退避重连。
//
// 不用 paho 的 AutoReconnect：它复用同一份 options，遗嘱载荷与序号会被固定住。
// 自管重连还让「重连后补发 ONLINE」与「退避上限 30 秒」有明确的落点。
type MQTTLink struct {
	cfg         MQTTConfig
	will        WillFunc
	onConnected func() error
	log         *pkglog.Logger

	mu     sync.RWMutex
	client mqtt.Client
	lost   chan struct{}
}

// NewMQTTLink 组装连接。onConnected 在每次（重）连成功后调用，用来发布 ONLINE。
func NewMQTTLink(cfg MQTTConfig, will WillFunc, onConnected func() error, log *pkglog.Logger) (*MQTTLink, error) {
	if cfg.BrokerURL == "" {
		return nil, fmt.Errorf("MQTT 配置缺少 BrokerURL")
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, fmt.Errorf("MQTT 配置缺少账号或口令")
	}
	if will == nil || onConnected == nil {
		return nil, fmt.Errorf("MQTT 配置缺少遗嘱或连接成功回调")
	}
	if cfg.QoS > 1 {
		return nil, fmt.Errorf("QoS 只支持 0/1，实际 %d", cfg.QoS)
	}
	return &MQTTLink{cfg: cfg, will: will, onConnected: onConnected, log: log}, nil
}

// Connect 建立首个会话。失败即返回错误，由调用方终止进程——
// 带病启动会产出难以解释的错值（rules/edge-agent.md 启动顺序）。
func (l *MQTTLink) Connect(ctx context.Context) error {
	client, err := l.open(ctx)
	if err != nil {
		return err
	}
	l.setClient(client)
	l.log.Info("MQTT 已连接", pkglog.F("broker", redactURL(l.cfg.BrokerURL)))
	return l.onConnected()
}

// Run 守住连接：断线后按 1s / 2s / 4s … 30s 退避重连，直到 ctx 取消。
func (l *MQTTLink) Run(ctx context.Context) {
	backoff := l.cfg.ReconnectMin
	for {
		lost := l.lostCh()
		select {
		case <-ctx.Done():
			return
		case <-lost:
		}

		l.clearClient()
		for !l.tryReconnect(ctx, backoff) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff, l.cfg.ReconnectMax)
		}
		backoff = l.cfg.ReconnectMin
	}
}

// tryReconnect 尝试建一次会话。
func (l *MQTTLink) tryReconnect(ctx context.Context, wait time.Duration) bool {
	attemptCtx, cancel := context.WithTimeout(ctx, l.cfg.ConnectTimeout)
	defer cancel()

	client, err := l.open(attemptCtx)
	if err != nil {
		l.log.Warn("MQTT 重连失败，继续退避", pkglog.F("retryAfterMs", wait.Milliseconds()),
			pkglog.F("err", err.Error()))
		return false
	}
	l.setClient(client)
	l.log.Info("MQTT 已恢复", pkglog.F("broker", redactURL(l.cfg.BrokerURL)))
	if err := l.onConnected(); err != nil {
		l.log.Warn("重连后发布状态失败", pkglog.F("err", err.Error()))
	}
	return true
}

// open 建一个 paho 会话并注册遗嘱。每次调用都重新取遗嘱载荷（新序号）。
func (l *MQTTLink) open(ctx context.Context) (mqtt.Client, error) {
	willTopic, willPayload, err := l.will()
	if err != nil {
		return nil, err
	}

	opts := mqtt.NewClientOptions().
		AddBroker(l.cfg.BrokerURL).
		SetClientID(l.cfg.ClientID).
		SetUsername(l.cfg.Username).
		SetPassword(l.cfg.Password).
		// 持久会话：Broker 侧保留订阅与离线消息（接口规范 §4.1）。
		SetCleanSession(false).
		SetKeepAlive(l.cfg.KeepAlive).
		SetConnectTimeout(l.cfg.ConnectTimeout).
		// 重连自己管：paho 的自动重连会复用同一份遗嘱载荷。
		SetAutoReconnect(false).
		SetWill(willTopic, string(willPayload), l.cfg.QoS, true).
		SetConnectionLostHandler(func(_ mqtt.Client, lost error) { l.handleLost(lost) })

	client := mqtt.NewClient(opts)
	token := client.Connect()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("连接 Broker 被取消: %w", ctx.Err())
	case <-token.Done():
	}
	if err := token.Error(); err != nil {
		return nil, fmt.Errorf("连接 Broker %s: %w", redactURL(l.cfg.BrokerURL), err)
	}
	return client, nil
}

// Publish 发布一条报文并等待 PUBACK。未连接时立即返回错误，不挂起等待。
func (l *MQTTLink) Publish(topic string, payload []byte, retain bool) error {
	l.mu.RLock()
	client := l.client
	l.mu.RUnlock()
	if client == nil || !client.IsConnectionOpen() {
		return fmt.Errorf("MQTT 未连接")
	}

	token := client.Publish(topic, l.cfg.QoS, retain, payload)
	if !token.WaitTimeout(l.cfg.PubAckTimeout) {
		return fmt.Errorf("发布 %s 等待 PUBACK 超时（%s）", topic, l.cfg.PubAckTimeout)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("发布 %s: %w", topic, err)
	}
	return nil
}

// Disconnect 断开连接。调用方先发完 OFFLINE 再调这里：先断开会让 Broker 直接下发遗嘱，
// 优雅退出的原因就丢成了 UNEXPECTED_DISCONNECT。
func (l *MQTTLink) Disconnect(quiesceMs uint) {
	l.mu.Lock()
	client := l.client
	l.client = nil
	l.mu.Unlock()
	if client != nil {
		client.Disconnect(quiesceMs)
	}
}

func (l *MQTTLink) setClient(client mqtt.Client) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.client = client
	l.lost = make(chan struct{})
}

func (l *MQTTLink) clearClient() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.client = nil
}

func (l *MQTTLink) lostCh() chan struct{} {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lost
}

// handleLost 记录断线并叫醒守护协程。关闭 lost 通道只做一次，重复回调不会 panic。
func (l *MQTTLink) handleLost(err error) {
	l.mu.Lock()
	lost := l.lost
	l.lost = nil
	l.mu.Unlock()

	l.log.Warn("MQTT 断开", pkglog.F("reason", err.Error()))
	if lost != nil {
		close(lost)
	}
}

// nextBackoff 翻倍到上限为止。
func nextBackoff(d, max time.Duration) time.Duration {
	if d <= 0 {
		d = time.Second
	}
	d *= 2
	if max > 0 && d > max {
		return max
	}
	return d
}

// redactURL 去掉连接串里的账号口令再进日志。
func redactURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	if _, host, ok := strings.Cut(rest, "@"); ok {
		return scheme + "://***@" + host
	}
	return raw
}
