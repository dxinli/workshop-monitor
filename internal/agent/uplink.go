package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"workshop-monitor/internal/config"
	"workshop-monitor/internal/model"
)

// SchemaVersion 是上行报文头的契约版本（接口规范 §6.2）。
const SchemaVersion = "2.0"

// esnPlaceholder 是主题模板里代表终端标识的占位符。
const esnPlaceholder = "{esn}"

// UplinkTopics 是一套命名方案展开后的三个上行主题，形如 "/edge/ESN-ZX-0001/data/up"。
//
// 模板来自 config/topics.json，本包不出现任何主题字面量：切换方案只改环境变量
// MQTT_TOPIC_SCHEME，代码不动。
type UplinkTopics struct {
	Data   string
	Event  string
	Status string
}

// ExpandTopics 把一套模板按终端标识展开成实际主题。
//
// 模板缺 {esn} 时直接报错：展开不出终端标识的主题会让所有终端发到同一个地方，
// 而这种错误在 Broker 上看起来只是「主题少了一段」。
func ExpandTopics(s config.TopicSchemes, esn string) (UplinkTopics, error) {
	if esn == "" {
		return UplinkTopics{}, fmt.Errorf("终端标识为空，无法展开上行主题")
	}
	out := UplinkTopics{}
	for _, f := range []struct {
		name, tmpl string
		dst        *string
	}{
		{"数据", s.DataUp, &out.Data},
		{"事件", s.EventUp, &out.Event},
		{"状态", s.StatusUp, &out.Status},
	} {
		expanded, err := expandTopic(f.name, f.tmpl, esn)
		if err != nil {
			return UplinkTopics{}, err
		}
		*f.dst = expanded
	}
	return out, nil
}

func expandTopic(name, tmpl, esn string) (string, error) {
	if tmpl == "" {
		return "", fmt.Errorf("%s上行主题模板为空", name)
	}
	if n := strings.Count(tmpl, esnPlaceholder); n != 1 {
		return "", fmt.Errorf("%s上行主题模板 %q 必须且只能含一个 %s，实际 %d 个",
			name, tmpl, esnPlaceholder, n)
	}
	if !strings.HasPrefix(tmpl, "/") {
		return "", fmt.Errorf("%s上行主题模板 %q 必须以 / 开头", name, tmpl)
	}
	return strings.Replace(tmpl, esnPlaceholder, esn, 1), nil
}

// Envelope 是一条上行报文的线上形状。报文头与报文体分开拼装，
// 三类报文共用同一个头，避免每处各写一遍 9 个字段。
type Envelope struct {
	Header model.Header `json:"header"`
	Body   any          `json:"body"`
}

// HeaderFor 组装报文头。msgId 为 {esn}-{seq:08d}（接口规范 §6.2）。
func HeaderFor(esn, msgType string, seq, collectTime, sendTime int64, timeSync bool) model.Header {
	return model.Header{
		SchemaVersion: SchemaVersion,
		MsgType:       msgType,
		MsgID:         fmt.Sprintf("%s-%08d", esn, seq),
		Esn:           esn,
		Seq:           seq,
		CollectTime:   collectTime,
		SendTime:      sendTime,
		Backfill:      false,
		TimeSync:      timeSync,
	}
}

// EncodeUplink 序列化一条上行报文。中文直出（不转义），便于人工逐字段核对。
func EncodeUplink(h model.Header, body any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Envelope{Header: h, Body: body}); err != nil {
		return nil, fmt.Errorf("序列化 %s 报文 seq=%d: %w", h.MsgType, h.Seq, err)
	}
	// Encoder 末尾补换行，报文体里不要带——它会让报文长度比预期多 1 字节。
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

// DataBodyOf 把一批采集结果转成 DATA 报文体。
//
// 按 siType 分成遥测与遥信两组；失败点照样出现在数组里（Value 为 nil），
// 省略会让「没采到」与「没这个点」无法区分（接口规范 §6.2 强制规则 1）。
func DataBodyOf(b Batch) model.DataBody {
	body := model.DataBody{BusID: b.BusID}
	for _, pt := range b.Points {
		pv := model.PointValue{
			Code:      pt.Code,
			SlaveAddr: pt.SlaveAddr,
			Value:     pt.Value,
			Unit:      pt.Unit,
			Quality:   pt.Quality,
		}
		if pt.SiType == "YX" {
			body.YX = append(body.YX, pv)
			continue
		}
		body.YC = append(body.YC, pv)
	}
	// 空数组要序列化成 []，不能是 null：订阅方按数组遍历，null 会让解析直接失败。
	if body.YC == nil {
		body.YC = []model.PointValue{}
	}
	if body.YX == nil {
		body.YX = []model.PointValue{}
	}
	body.Stat = model.Stat{Total: len(b.Points), Good: b.Good, Bad: b.Bad}
	return body
}
