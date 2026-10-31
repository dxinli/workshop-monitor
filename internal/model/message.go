// Package model 存放与接口规范一一对应的结构体。
//
// 本包只有结构体与常量，不含方法：解析、校验、换算一律放在各自的适配器里，
// 这样 agent 与 adapter 共享的只有数据形状，不会共享行为。
package model

// 上行报文的 msgType 取值（接口规范 §6.2）。
const (
	MsgTypeData   = "DATA"
	MsgTypeEvent  = "EVENT"
	MsgTypeStatus = "STATUS"
)

// 数据质量码（接口规范 §8，冻结线 A）。固定 9 个值，不得增删改名。
const (
	QualityGood            = 0
	QualityDeviceException = 1
	QualityTimeout         = 2
	QualityCRCError        = 3
	QualityParseError      = 4
	QualityOutOfRange      = 5
	QualityStale           = 6
	QualitySimulated       = 7
	QualityManual          = 8
)

// Header 是三类上行报文共用的报文头，共 9 个字段（8 规范字段 + 1 本地扩展）。
type Header struct {
	SchemaVersion string `json:"schemaVersion"`
	MsgType       string `json:"msgType"`
	MsgID         string `json:"msgId"`
	Esn           string `json:"esn"`
	Seq           int64  `json:"seq"`
	CollectTime   int64  `json:"collectTime"`
	SendTime      int64  `json:"sendTime"`
	Backfill      bool   `json:"backfill"`
	TimeSync      bool   `json:"timeSync"`
}

// PointValue 是一个测点的一次读数。失败点也必须出现在数组里，Value 为 nil。
type PointValue struct {
	Code      string   `json:"code"`
	SlaveAddr int      `json:"slaveAddr"`
	Value     *float64 `json:"value"`
	Unit      string   `json:"unit"`
	Quality   int      `json:"quality"`
}

// Stat 是本批测点的计数，用于质量看板与取证。
type Stat struct {
	Total int `json:"total"`
	Good  int `json:"good"`
	Bad   int `json:"bad"`
}

// DataBody 是 DATA 报文的报文体。
type DataBody struct {
	BusID string       `json:"busId"`
	YC    []PointValue `json:"yc"`
	YX    []PointValue `json:"yx"`
	Stat  Stat         `json:"stat"`
}

// EventBody 是 EVENT 报文的报文体。
type EventBody struct {
	EventType    string `json:"eventType"`
	BusID        string `json:"busId"`
	SlaveAddr    int    `json:"slaveAddr"`
	EventTime    int64  `json:"eventTime"`
	Reason       string `json:"reason"`
	FailureCount int    `json:"failureCount"`
	Detail       string `json:"detail"`
}

// StatusBody 是 STATUS 报文的报文体。
type StatusBody struct {
	Status       string `json:"status"`
	StatusTime   int64  `json:"statusTime"`
	Reason       string `json:"reason"`
	UptimeSec    int64  `json:"uptimeSec"`
	CacheDepth   int    `json:"cacheDepth"`
	CacheBytes   int64  `json:"cacheBytes"`
	AgentVersion string `json:"agentVersion"`
}

// Uplink 是一条上行报文。Body 保留原始 JSON，由调用方按 MsgType 二次解码——
// 三类报文体结构不同，统一塞进一个字段会让解码分支散落在各处。
type Uplink struct {
	Header Header
	Body   []byte
}
