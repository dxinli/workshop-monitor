// Package ingest 实现接收段：主题解析 → 解码 → 归一化 → 幂等 → 原文留存 → 投影。
//
// 本包不 import internal/adapter/evaluate。与判定段唯一的连接是有界 channel，
// 由 cmd/proto-adapter 装配。
package ingest

import (
	"context"
	"fmt"
	"time"
)

// 解析结论，写入 message_log.parse_status。
const (
	// StatusAccepted 表示首次收到，业务表已写。
	StatusAccepted = "OK"
	// StatusDuplicate 表示 (terminalId, seq) 已登记过，未写任何业务表。
	StatusDuplicate = "DUPLICATE"
	// StatusParseError 表示报文体无法解码，只留原文。
	StatusParseError = "PARSE_ERROR"
	// StatusIdentityMismatch 表示报文头 esn 与主题不一致，只留原文。
	StatusIdentityMismatch = "IDENTITY_MISMATCH"
)

// LogInfo 是写入 message_log 的一行所需的信息（不含幂等键本身）。
type LogInfo struct {
	MsgType     string
	CollectTime time.Time
	ReceiveTime time.Time
	Backfill    bool
	PointCount  int
	GoodCount   int
	BadCount    int
	ParseStatus string
	FailReason  string
}

// Claimer 在 message_log 上登记 (terminalID, seq)。
//
// 返回 true 表示本次是首次登记，调用方应继续写业务表；
// 返回 false 表示已存在，调用方必须直接返回，不写历史、实时值、事件。
// 实现必须是单条 INSERT ... ON CONFLICT DO NOTHING：先查后插在并发下必然失效。
type Claimer interface {
	Claim(ctx context.Context, terminalID string, seq int64, info LogInfo) (bool, error)
}

// Idem 是幂等判定的唯一落点。
//
// 顺序固定：登记 → 写业务表。反过来先写后去重会留下无法清理的脏数据，
// 而删除脏数据又会污染已聚合的分钟视图。
type Idem struct {
	claim Claimer
}

// NewIdem 组装幂等判定。claim 由调用方注入，单测用内存实现，生产用 DB 实现。
func NewIdem(claim Claimer) *Idem { return &Idem{claim: claim} }

// Guard 先登记再执行 write。
//
// 重复消息返回 StatusDuplicate 且不调用 write；登记本身失败时返回错误且不调用 write——
// 「没记上幂等键却已入库」比「这条没入库」更难排查。
func (i *Idem) Guard(ctx context.Context, terminalID string, seq int64, info LogInfo, write func(context.Context) error) (string, error) {
	fresh, err := i.claim.Claim(ctx, terminalID, seq, info)
	if err != nil {
		return "", fmt.Errorf("登记幂等键 %s|%d: %w", terminalID, seq, err)
	}
	if !fresh {
		return StatusDuplicate, nil
	}

	if err := write(ctx); err != nil {
		return "", fmt.Errorf("写入 %s|%d 的业务表: %w", terminalID, seq, err)
	}
	return StatusAccepted, nil
}
