package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"workshop-monitor/internal/agent"
	"workshop-monitor/internal/config"
)

// 上行参数的默认值。Broker 地址与口令没有默认值——缺了就拒绝启动，
// 猜一个 localhost 会让现场以为「连不上是网络问题」。
const (
	defaultTopicScheme = "grid"
	defaultQoS         = 1
	defaultKeepAliveS  = 30
	defaultConnectSec  = 10
	defaultPubAckMs    = 2000
)

// uplinkSettings 是从环境变量与主题文件读出的全部上行参数。
type uplinkSettings struct {
	mqtt   agent.MQTTConfig
	topics agent.UplinkTopics
	scheme string
}

// loadSettings 装配上行参数。
//
// 按接口规范附录 B：地址、方案、QoS、心跳都走环境变量；账号就是终端标识，
// 口令按 `MQTT_<终端>_PASSWORD` 推导——与 scripts/gen-passwd.sh 的规则一致，
// 也保证「ACL、口令文件、主题」三处用的是同一个账号名。
func loadSettings(term *config.Terminal, topicsPath string) (uplinkSettings, error) {
	scheme, topics, err := resolveTopics(term.TerminalID, topicsPath)
	if err != nil {
		return uplinkSettings{}, err
	}
	mqtt, err := resolveMQTT(term.TerminalID)
	if err != nil {
		return uplinkSettings{}, err
	}
	return uplinkSettings{scheme: scheme, topics: topics, mqtt: mqtt}, nil
}

// resolveTopics 按 MQTT_TOPIC_SCHEME 选一套模板并按终端标识展开。
func resolveTopics(esn, topicsPath string) (string, agent.UplinkTopics, error) {
	scheme := envOr("MQTT_TOPIC_SCHEME", defaultTopicScheme)
	if scheme != "grid" && scheme != "internal" {
		return "", agent.UplinkTopics{},
			fmt.Errorf("MQTT_TOPIC_SCHEME=%q 不支持，只能是 grid 或 internal", scheme)
	}

	tf, err := config.LoadTopics(topicsPath)
	if err != nil {
		return "", agent.UplinkTopics{}, err
	}
	schemes, ok := tf.Schemes[scheme]
	if !ok {
		return "", agent.UplinkTopics{}, fmt.Errorf("%s 中没有命名方案 %q", topicsPath, scheme)
	}
	topics, err := agent.ExpandTopics(schemes, esn)
	if err != nil {
		return "", agent.UplinkTopics{}, fmt.Errorf("展开上行主题: %w", err)
	}
	return scheme, topics, nil
}

// resolveMQTT 读连接参数。地址与口令没有默认值：猜一个 localhost 会让现场
// 把「配置缺失」误判成「网络不通」。
func resolveMQTT(esn string) (agent.MQTTConfig, error) {
	broker := os.Getenv("MQTT_BROKER_URL")
	if broker == "" {
		return agent.MQTTConfig{}, fmt.Errorf("缺少环境变量 MQTT_BROKER_URL")
	}
	passwordVar := passwordVarOf(esn)
	password := os.Getenv(passwordVar)
	if password == "" {
		return agent.MQTTConfig{}, fmt.Errorf("缺少环境变量 %s", passwordVar)
	}
	qos, err := intEnv("MQTT_QOS", defaultQoS)
	if err != nil {
		return agent.MQTTConfig{}, err
	}
	if qos < 0 || qos > 1 {
		return agent.MQTTConfig{}, fmt.Errorf("MQTT_QOS=%d 只支持 0 或 1", qos)
	}
	keepAlive, err := intEnv("MQTT_KEEPALIVE", defaultKeepAliveS)
	if err != nil {
		return agent.MQTTConfig{}, err
	}
	return agent.MQTTConfig{
		BrokerURL:      broker,
		ClientID:       "edge-" + esn,
		Username:       esn,
		Password:       password,
		QoS:            byte(qos),
		KeepAlive:      time.Duration(keepAlive) * time.Second,
		ConnectTimeout: defaultConnectSec * time.Second,
		PubAckTimeout:  defaultPubAckMs * time.Millisecond,
		ReconnectMin:   time.Second,
		ReconnectMax:   30 * time.Second,
	}, nil
}

// passwordVarOf 与 scripts/gen-passwd.sh 的 var_of 逐字一致：
// ESN-ZX-0001 → MQTT_ESN_ZX_0001_PASSWORD。两边必须同源，否则容器里连不上。
func passwordVarOf(terminalID string) string {
	replacer := strings.NewReplacer("-", "_", ".", "_")
	return "MQTT_" + strings.ToUpper(replacer.Replace(terminalID)) + "_PASSWORD"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("环境变量 %s=%q 不是整数: %w", key, raw, err)
	}
	return v, nil
}
