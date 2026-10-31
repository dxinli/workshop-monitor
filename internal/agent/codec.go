// Package agent 是终端上的采集核心：按点表轮询、寄存器值换算、质量标记、产出批次。
//
// 上行（006）、本地缓存（007）、健康检查（015）不在本包：它们各自需要 MQTT、
// 磁盘队列与 HTTP 服务，混进来会让采集循环依赖一堆不相干的外部资源。
package agent

import (
	"math"

	"workshop-monitor/internal/config"
	"workshop-monitor/internal/model"
)

// toEngineering 把一次读到的寄存器字换算成工程值并给出质量码（接口规范 §3.5 §3.6）。
//
// 顺序固定：拼字（高字在前）→ 符号扩展 → ×scale + offset → 按 scale 的小数位舍入
// → 量程比较。超量程保留数值、只改质量码：置空会丢掉「值异常但可读」的信息。
//
// 返回 nil 值只出现在形状不符时（PARSE_ERROR）：字数与宽度对不上、字节序不是本期
// 支持的 AB / AB CD、数据类型不是本期支持的四种。这些本应由 V-04 在启动时拦住，
// 运行期再判一次是为了绝不产出错值——错值比空值难排查得多。
func toEngineering(p *config.Point, words []uint16) (*float64, int) {
	raw, ok := rawValue(p, words)
	if !ok {
		return nil, model.QualityParseError
	}
	if p.Scale == nil {
		return nil, model.QualityParseError
	}
	offset := 0.0
	if p.Offset != nil {
		offset = *p.Offset
	}

	eng := roundTo(raw**p.Scale+offset, decimalsOf(*p.Scale))
	if (p.MinValue != nil && eng < *p.MinValue) || (p.MaxValue != nil && eng > *p.MaxValue) {
		return &eng, model.QualityOutOfRange
	}
	return &eng, model.QualityGood
}

// rawValue 按数据类型与字节序把寄存器字还原成带符号的整数原值。
func rawValue(p *config.Point, words []uint16) (float64, bool) {
	if !byteOrderSupported(p.Width, p.ByteOrder) || len(words) < p.Width {
		return 0, false
	}
	var v int64
	if p.Width == 2 {
		v = int64(words[0])<<16 | int64(words[1])
	} else {
		v = int64(words[0])
	}
	switch p.DataType {
	case "int16", "int32":
		if p.Width != widthOf(p.DataType) {
			return 0, false
		}
		if p.DataType == "int16" {
			v = int64(int16(v))
		} else {
			v = int64(int32(v))
		}
	case "uint16", "uint32":
		if p.Width != widthOf(p.DataType) {
			return 0, false
		}
	default:
		return 0, false
	}
	return float64(v), true
}

func widthOf(dataType string) int {
	if dataType == "int32" || dataType == "uint32" {
		return 2
	}
	return 1
}

// byteOrderSupported：本期只认大端。两字测点的「AB CD」与单字测点的「AB」
// 都是高字在前，按同一处理；CDAB 等变体见接口规范附录 A，本期不生效。
func byteOrderSupported(width int, order string) bool {
	switch width {
	case 1:
		return order == "AB"
	case 2:
		return order == "AB" || order == "ABCD"
	}
	return false
}

// decimalsOf 从缩放系数推小数位：0.1 → 1，0.01 → 2，1 → 0。
// 不用 log10——log10(0.1) 在浮点下是 1.0000000000000002，向上取整会多一位。
func decimalsOf(scale float64) int {
	if scale <= 0 {
		return 0
	}
	for d := range 10 {
		if math.Abs(scale-math.Round(scale)) < 1e-9 {
			return d
		}
		scale *= 10
	}
	return 9
}

// roundTo 消掉浮点尾差：3 × 0.1 在 float64 里是 0.30000000000000004。
func roundTo(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}
