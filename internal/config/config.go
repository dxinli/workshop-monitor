// Package config 载入并校验终端点表与告警阈值。
//
// 配置文件是唯一权威源，数据库只是供界面读取的投影：本包只读文件，
// 不碰数据库，投影由 cmd/config-tool seed 生成 SQL 后交给 psql。
//
// 结构体字段与接口规范 §3.7 / §11.1 一一对应，校验规则见 V-01~V-12。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Terminal 是一份终端点表，对应 config/terminals/<esn>.json。
type Terminal struct {
	TerminalID        string `json:"terminalId"`
	ConfigVersion     string `json:"configVersion"`
	Workshop          string `json:"workshop"`
	Line              string `json:"line"`
	CollectIntervalMs int    `json:"collectIntervalMs"`
	PublishBatch      bool   `json:"publishBatch"`
	Buses             []Bus  `json:"buses"`

	// path 是载入来源，校验报错时用它定位文件。不参与 JSON。
	path string
}

// Bus 是一条 RS485 总线。
type Bus struct {
	BusID        string   `json:"busId"`
	Mode         string   `json:"mode"`
	Port         string   `json:"port"`
	BaudRate     int      `json:"baudRate"`
	DataBits     int      `json:"dataBits"`
	StopBits     int      `json:"stopBits"`
	Parity       string   `json:"parity"`
	TimeoutMs    int      `json:"timeoutMs"`
	RetryCount   int      `json:"retryCount"`
	InterFrameMs int      `json:"interFrameMs"`
	Devices      []Device `json:"devices"`
}

// Device 是总线上的一个从站。
type Device struct {
	SlaveAddr int     `json:"slaveAddr"`
	Name      string  `json:"name"`
	Type      string  `json:"type"`
	Blocks    []Block `json:"blocks"`
}

// Block 是一次读取的连续寄存器窗口。Points[].Addr 是块内偏移，不是绝对地址。
type Block struct {
	FunctionCode  int     `json:"functionCode"`
	StartAddr     *int    `json:"startAddr"`
	RegisterCount int     `json:"registerCount"`
	Points        []Point `json:"points"`
}

// Point 是设备上的一个测点。
// Addr、Scale、StartAddr 用指针，是因为 0 在结构里是合法取值：
// 缺字段与显式 0 必须能区分，否则 V-01 会把缺失误报成 V-05/V-06。
type Point struct {
	Code      string   `json:"code"`
	Name      string   `json:"name"`
	SiType    string   `json:"siType"`
	DataType  string   `json:"dataType"`
	ByteOrder string   `json:"byteOrder"`
	Addr      *int     `json:"addr"`
	Width     int      `json:"width"`
	Scale     *float64 `json:"scale"`
	Offset    *float64 `json:"offset"`
	Unit      *string  `json:"unit"`
	MinValue  *float64 `json:"minValue"`
	MaxValue  *float64 `json:"maxValue"`
}

// ThresholdFile 是 config/thresholds.json：规则模板，seed 时按终端展开。
type ThresholdFile struct {
	ConfigVersion string          `json:"configVersion"`
	Rules         []ThresholdRule `json:"rules"`
}

// ThresholdRule 是一条告警规则模板。
// 不填 TerminalID 表示适用全部终端；不填 BusID 表示适用该终端全部总线。
type ThresholdRule struct {
	ID             string   `json:"id"`
	PointCodes     []string `json:"pointCodes"`
	AlarmType      string   `json:"alarmType"`
	Operator       string   `json:"operator"`
	ThresholdValue *float64 `json:"thresholdValue"`
	Level          int      `json:"level"`
	DebounceCount  *int     `json:"debounceCount"`
	TerminalID     string   `json:"terminalId"`
	BusID          string   `json:"busId"`
}

// LoadTerminal 读取一份终端点表。未知字段直接报错，避免拼错的键被静默忽略。
func LoadTerminal(path string) (*Terminal, error) {
	var t Terminal
	if err := decodeStrict(path, &t); err != nil {
		return nil, err
	}
	t.path = filepath.Base(path)
	return &t, nil
}

// LoadTerminalDir 读取目录下全部 *.json，按文件名排序，保证输出稳定。
func LoadTerminalDir(dir string) ([]*Terminal, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("枚举终端点表 %s: %w", dir, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("终端点表目录 %s 下没有 *.json", dir)
	}
	sort.Strings(matches)

	out := make([]*Terminal, 0, len(matches))
	for _, p := range matches {
		t, err := LoadTerminal(p)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// LoadThresholds 读取告警阈值文件。
func LoadThresholds(path string) (*ThresholdFile, error) {
	var tf ThresholdFile
	if err := decodeStrict(path, &tf); err != nil {
		return nil, err
	}
	return &tf, nil
}

// LoadTopics 读取主题模板文件。
func LoadTopics(path string) (*TopicFile, error) {
	var tf TopicFile
	if err := decodeStrict(path, &tf); err != nil {
		return nil, err
	}
	return &tf, nil
}

// TopicFile 是 config/topics.json：两套命名方案的主题模板。
// 主题字符串不写在代码里，切换方案只改这里与 MQTT_TOPIC_SCHEME。
type TopicFile struct {
	ConfigVersion string                  `json:"configVersion"`
	Schemes       map[string]TopicSchemes `json:"schemes"`
}

// TopicSchemes 是一套命名方案下的三类上行主题模板，{esn} 为占位符。
type TopicSchemes struct {
	DataUp   string `json:"dataUp"`
	EventUp  string `json:"eventUp"`
	StatusUp string `json:"statusUp"`
}

// fileName 是报错时显示的文件名。
func (t *Terminal) fileName() string {
	if t.path == "" {
		return t.TerminalID + ".json"
	}
	return t.path
}

func decodeStrict(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置 %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("解析配置 %s: %w", path, err)
	}
	return nil
}
