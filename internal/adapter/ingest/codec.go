package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"workshop-monitor/internal/model"
)

// 解析错误分类。调用方据此决定 message_log.parse_status 与 raw_message.fail_reason。
var (
	// ErrParse 表示报文结构不合法（非 JSON、缺必填字段、统计不符、质量码越界等）。
	ErrParse = errors.New("报文解析失败")
	// ErrIdentityMismatch 表示报文头 esn 与主题中的终端不一致。
	ErrIdentityMismatch = errors.New("报文头 esn 与主题不一致")
	// ErrBadQuality 表示质量码与 value 的组合违反接口规范 §8 规则 1。
	ErrBadQuality = errors.New("质量码与值不匹配")
)

// QualityGood 是「正常」质量码。质量码取值固定，见接口规范 §8。
const QualityGood = model.QualityGood

// Row 是一条待落库的测点读数。pointId 由 busId.slaveAddr.pointCode 派生，不在报文里。
type Row struct {
	TerminalID  string
	BusID       string
	SlaveAddr   int
	PointCode   string
	PointID     string
	SiType      string
	Value       *float64
	Quality     int
	CollectTime time.Time
	Seq         int64
	Backfill    bool
}

// Decoded 是一条报文解码后的结果。
//
// Rows 只对 DATA 报文非空；Event 与 Status 各自填对应字段。
// Header 在 Partial 时只填 esn / seq / collectTime / msgType，供原文留存使用。
type Decoded struct {
	Header  model.Header
	Rows    []Row
	Event   *model.EventBody
	Status  *model.StatusBody
	Partial bool
}

// Decode 按主题给出的 msgType 解码报文体。
//
// topicEsn 来自主题（可信来源），报文头 esn 必须与它一致——不一致时整条判 IDENTITY_MISMATCH，
// 但报文头仍会被解出来，调用方要用它留原文。
func Decode(topicEsn, msgType string, payload []byte) (Decoded, error) {
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		// 原文不是 JSON 时也要能判出 parse_status，因此错误里只挂 ErrParse。
		return Decoded{}, fmt.Errorf("%w: 非法 JSON（%d 字节）", ErrParse, len(payload))
	}
	if env.Header == nil {
		return Decoded{}, fmt.Errorf("%w: 缺少 header", ErrParse)
	}

	d := Decoded{Header: *env.Header}
	if err := checkHeader(d.Header, topicEsn, msgType); err != nil {
		return d, err
	}
	return decodeBodyOf(msgType, env.Body, d)
}

type envelope struct {
	Header *model.Header   `json:"header"`
	Body   json.RawMessage `json:"body"`
}

// checkHeader 校验报文头里能单独判定的四件事：身份、序号、采集时间、消息类型。
func checkHeader(h model.Header, topicEsn, msgType string) error {
	if h.Esn == "" {
		return fmt.Errorf("%w: 报文头缺少 esn", ErrParse)
	}
	if h.Esn != topicEsn {
		return fmt.Errorf("%w: 报文头 esn=%s，主题 esn=%s", ErrIdentityMismatch, h.Esn, topicEsn)
	}
	if h.Seq <= 0 {
		return fmt.Errorf("%w: seq 必须为正整数，实际 %d", ErrParse, h.Seq)
	}
	if h.CollectTime <= 0 {
		return fmt.Errorf("%w: collectTime 必须为正整数，实际 %d", ErrParse, h.CollectTime)
	}
	if h.MsgType != msgType {
		return fmt.Errorf("%w: 报文头 msgType=%s，主题判定 %s", ErrParse, h.MsgType, msgType)
	}
	return nil
}

func decodeBodyOf(msgType string, raw json.RawMessage, d Decoded) (Decoded, error) {
	switch msgType {
	case model.MsgTypeData:
		rows, err := decodeDataBody(d.Header.Esn, d.Header, raw)
		d.Rows = rows
		return d, err
	case model.MsgTypeEvent:
		var body model.EventBody
		if err := decodeBody(raw, &body); err != nil {
			return d, err
		}
		if body.EventType == "" {
			return d, fmt.Errorf("%w: 事件缺少 eventType", ErrParse)
		}
		d.Event = &body
		return d, nil
	case model.MsgTypeStatus:
		var body model.StatusBody
		if err := decodeBody(raw, &body); err != nil {
			return d, err
		}
		if body.Status == "" {
			return d, fmt.Errorf("%w: 状态缺少 status", ErrParse)
		}
		d.Status = &body
		return d, nil
	default:
		return d, fmt.Errorf("%w: 未知 msgType %q", ErrParse, msgType)
	}
}

func decodeBody(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: 缺少 body", ErrParse)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%w: body 结构不符（%d 字节）", ErrParse, len(raw))
	}
	return nil
}

func decodeDataBody(terminalID string, h model.Header, raw json.RawMessage) ([]Row, error) {
	var body model.DataBody
	if err := decodeBody(raw, &body); err != nil {
		return nil, err
	}
	if body.BusID == "" {
		return nil, fmt.Errorf("%w: 数据报文缺少 busId", ErrParse)
	}

	rows := make([]Row, 0, len(body.YC)+len(body.YX))
	for _, group := range []struct {
		values []model.PointValue
		siType string
	}{{body.YC, "YC"}, {body.YX, "YX"}} {
		for i, p := range group.values {
			row, err := toRow(terminalID, body.BusID, group.siType, i, p, h)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
	}

	if want := len(body.YC) + len(body.YX); body.Stat.Total != want {
		return nil, fmt.Errorf("%w: stat.total=%d，实际 yc+yx=%d", ErrParse, body.Stat.Total, want)
	}
	return rows, nil
}

func toRow(terminalID, busID, siType string, idx int, p model.PointValue, h model.Header) (Row, error) {
	path := fmt.Sprintf("%s[%d]", siType, idx)
	if p.Code == "" {
		return Row{}, fmt.Errorf("%w: %s 缺少 code", ErrParse, path)
	}
	if p.Quality < model.QualityGood || p.Quality > model.QualityManual {
		return Row{}, fmt.Errorf("%w: %s(%s) 质量码 %d 越界", ErrParse, path, p.Code, p.Quality)
	}
	if err := checkValueForQuality(path, p); err != nil {
		return Row{}, err
	}

	return Row{
		TerminalID:  terminalID,
		BusID:       busID,
		SlaveAddr:   p.SlaveAddr,
		PointCode:   p.Code,
		PointID:     busID + "." + itoa(p.SlaveAddr) + "." + p.Code,
		SiType:      siType,
		Value:       p.Value,
		Quality:     p.Quality,
		CollectTime: time.UnixMilli(h.CollectTime),
		Seq:         h.Seq,
		Backfill:    h.Backfill,
	}, nil
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }

// checkValueForQuality 落实接口规范 §8 规则 1：
// quality 0~4 时 value 必须为 null（1~4 是失败码，0 是正常码必须有值）；
// quality 5~8 保留数值。0 号位单独处理，是因为它既不是失败码、也不允许空值。
func checkValueForQuality(path string, p model.PointValue) error {
	switch {
	case p.Quality == model.QualityGood && p.Value == nil:
		return fmt.Errorf("%w: %s(%s) 质量码 0 是正常读数，value 不得为 null",
			ErrBadQuality, path, p.Code)
	case p.Quality > model.QualityGood && p.Quality <= model.QualityParseError && p.Value != nil:
		return fmt.Errorf("%w: %s(%s) 失败质量码 %d 时 value 必须为 null，实际 %v",
			ErrBadQuality, path, p.Code, p.Quality, *p.Value)
	case p.Quality > model.QualityParseError && p.Value == nil:
		return fmt.Errorf("%w: %s(%s) 质量码 %d 时必须保留数值", ErrBadQuality, path, p.Code, p.Quality)
	}
	return nil
}

// DecodeHeaderOnly 只解报文头，用于解析失败时仍要留存原文的场景。
// 解不出时返回零值，调用方以 NULL 落 raw_message。
func DecodeHeaderOnly(payload []byte) (model.Header, error) {
	var env struct {
		Header *model.Header `json:"header"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return model.Header{}, fmt.Errorf("解析报文头: %w", err)
	}
	if env.Header == nil {
		return model.Header{}, errors.New("报文缺少 header")
	}
	return *env.Header, nil
}

// IsValidJSON 判断载荷是否可存入 jsonb 列。原文留存要求列非空且是合法 JSON。
func IsValidJSON(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	return len(trimmed) > 0 && json.Valid(trimmed)
}
