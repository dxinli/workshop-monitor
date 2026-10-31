package config

import "fmt"

// Adapter 是接入服务的运行配置，对应 config/adapter.json。
//
// 只放进程级参数：Broker 地址、账号、数据库连接、队列容量。
// 业务参数（点表、阈值、主题模板）各有独立文件，不塞进这里。
type Adapter struct {
	BrokerURL            string `json:"brokerUrl"`
	TopicScheme          string `json:"topicScheme"`
	QoS                  byte   `json:"qos"`
	KeepAliveSec         int    `json:"keepAliveSec"`
	ConnectTimeoutSec    int    `json:"connectTimeoutSec"`
	ReconnectIntervalSec int    `json:"reconnectIntervalSec"`
	MaxReconnectSec      int    `json:"maxReconnectSec"`
	ClientID             string `json:"clientId"`
	Username             string `json:"username"`
	TopicsFile           string `json:"topicsFile"`
	Database             struct {
		URL               string `json:"url"`
		MaxConns          int32  `json:"maxConns"`
		ConnectTimeoutSec int    `json:"connectTimeoutSec"`
	} `json:"database"`
	Queue struct {
		Capacity int `json:"capacity"`
	} `json:"queue"`
}

// LoadAdapter 读取接入服务配置并校验必填项。
//
// 口令不写进配置文件：配置会进仓库，口令只从环境变量取。
func LoadAdapter(path string) (*Adapter, error) {
	var a Adapter
	if err := decodeStrict(path, &a); err != nil {
		return nil, err
	}
	if err := a.validate(); err != nil {
		return nil, fmt.Errorf("接入服务配置 %s: %w", path, err)
	}
	return &a, nil
}

func (a *Adapter) validate() error {
	switch {
	case a.BrokerURL == "":
		return fmt.Errorf("brokerUrl 必填")
	case a.ClientID == "":
		return fmt.Errorf("clientId 必填")
	case a.Username == "":
		return fmt.Errorf("username 必填")
	case a.Database.URL == "":
		return fmt.Errorf("database.url 必填")
	case a.TopicScheme != "grid" && a.TopicScheme != "internal":
		return fmt.Errorf("topicScheme 只能是 grid 或 internal，实际 %q", a.TopicScheme)
	case a.QoS > 1:
		return fmt.Errorf("qos 只支持 0 或 1，实际 %d", a.QoS)
	case a.Queue.Capacity <= 0:
		return fmt.Errorf("queue.capacity 必须为正整数，实际 %d", a.Queue.Capacity)
	case a.Database.MaxConns <= 0:
		return fmt.Errorf("database.maxConns 必须为正整数，实际 %d", a.Database.MaxConns)
	}
	return nil
}
