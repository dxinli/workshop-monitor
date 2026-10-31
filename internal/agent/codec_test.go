package agent

import (
	"testing"

	"workshop-monitor/internal/config"
	"workshop-monitor/internal/model"
)

// 换算 seam 的表驱动测试。
//
// 期望值是手算的：寄存器原值 → 按 AB/CD 拼字 → 符号扩展 → raw×scale+offset
// → 四舍五入到 scale 的小数位 → 与量程比较。断言里出现的一切数字都能在纸上复算，
// 不调用实现里的任何辅助函数，避免「照抄实现」的假通过。
func TestToEngineering(t *testing.T) {
	// point 按点表字段造，只填换算用得到的项。
	point := func(dataType string, scale float64, offset float64, min, max float64) *config.Point {
		return &config.Point{
			DataType:  dataType,
			ByteOrder: "AB",
			Width:     1,
			Scale:     &scale,
			Offset:    &offset,
			MinValue:  &min,
			MaxValue:  &max,
		}
	}
	wide := func(p *config.Point, byteOrder string) *config.Point {
		p.Width = 2
		p.ByteOrder = byteOrder
		return p
	}

	cases := []struct {
		name        string
		point       *config.Point
		words       []uint16
		wantValue   float64
		wantQuality int
	}{
		// motor_temp：int16、×0.1、量程 -20~200
		{
			name: "int16 正数", point: point("int16", 0.1, 0, -20, 200),
			words: []uint16{601}, wantValue: 60.1, wantQuality: model.QualityGood,
		},
		{
			name: "int16 负数按补码扩展", point: point("int16", 0.1, 0, -20, 200),
			words: []uint16{0xFF9C}, wantValue: -10, wantQuality: model.QualityGood,
		},
		{
			name: "int16 量程下限恰好命中", point: point("int16", 0.1, 0, -20, 200),
			words: []uint16{0xFF38}, wantValue: -20, wantQuality: model.QualityGood,
		},
		{
			name: "int16 低于下限保留数值", point: point("int16", 0.1, 0, -20, 200),
			words: []uint16{0xFF37}, wantValue: -20.1, wantQuality: model.QualityOutOfRange,
		},
		{
			name: "int16 全负码仍是有效负值", point: point("int16", 0.1, 0, -4000, 200),
			words: []uint16{0x8000}, wantValue: -3276.8, wantQuality: model.QualityGood,
		},

		// motor_vib：uint16、×0.01、量程 0~50
		{
			name: "uint16 乘 0.01", point: point("uint16", 0.01, 0, 0, 50),
			words: []uint16{235}, wantValue: 2.35, wantQuality: model.QualityGood,
		},
		{
			name: "uint16 上限恰好命中", point: point("uint16", 0.01, 0, 0, 50),
			words: []uint16{5000}, wantValue: 50, wantQuality: model.QualityGood,
		},
		{
			name: "uint16 超过上限保留数值", point: point("uint16", 0.01, 0, 0, 50),
			words: []uint16{5001}, wantValue: 50.01, wantQuality: model.QualityOutOfRange,
		},
		{
			name: "uint16 无符号不得扩展成负数", point: point("uint16", 1, 0, 0, 65535),
			words: []uint16{0xFFFF}, wantValue: 65535, wantQuality: model.QualityGood,
		},

		// ep_forward：uint32、AB CD 高字在前、×0.01
		{
			name: "uint32 高字在前拼装", point: wide(point("uint32", 0.01, 0, 0, 42949672.95), "ABCD"),
			words: []uint16{0x0001, 0xE240}, wantValue: 1234.56, wantQuality: model.QualityGood,
		},
		{
			name: "uint32 字序颠倒会算出 65536 倍", point: wide(point("uint32", 0.01, 0, 0, 42949672.95), "ABCD"),
			words: []uint16{0x0000, 0x0001}, wantValue: 0.01, wantQuality: model.QualityGood,
		},
		{
			name: "uint32 上限恰好命中", point: wide(point("uint32", 0.01, 0, 0, 42949672.95), "ABCD"),
			words: []uint16{0xFFFF, 0xFFFF}, wantValue: 42949672.95, wantQuality: model.QualityGood,
		},
		{
			name: "int32 负数按补码扩展", point: wide(point("int32", 0.1, 0, -1000, 1000), "ABCD"),
			words: []uint16{0xFFFF, 0xFFFF}, wantValue: -0.1, wantQuality: model.QualityGood,
		},
		{
			name: "int32 最小值", point: wide(point("int32", 1, 0, -3000000000, 0), "ABCD"),
			words: []uint16{0x8000, 0x0000}, wantValue: -2147483648, wantQuality: model.QualityGood,
		},

		// 偏移与舍入
		{
			name: "offset 参与换算", point: point("uint16", 0.1, 5, 0, 100),
			words: []uint16{100}, wantValue: 15, wantQuality: model.QualityGood,
		},
		{
			name: "按 scale 小数位舍入", point: point("uint16", 0.1, 0, 0, 100),
			words: []uint16{3}, wantValue: 0.3, wantQuality: model.QualityGood,
		},
		{
			name: "scale 为 1 时不带小数", point: point("uint16", 1, 0, 0, 1),
			words: []uint16{1}, wantValue: 1, wantQuality: model.QualityGood,
		},
		{
			name: "scale 0.001 保留三位", point: point("uint16", 0.001, 0, 0, 1.6),
			words: []uint16{620}, wantValue: 0.62, wantQuality: model.QualityGood,
		},
		{
			name: "压力超过量程保留数值", point: point("uint16", 0.001, 0, 0, 1.6),
			words: []uint16{1601}, wantValue: 1.601, wantQuality: model.QualityOutOfRange,
		},

		// 形状不符：V-04 在启动时已拦，这里只保证运行期不 panic、不产出错值
		{
			name: "宽度 2 却只给一个字", point: wide(point("uint32", 0.01, 0, 0, 100), "ABCD"),
			words: []uint16{1}, wantValue: 0, wantQuality: model.QualityParseError,
		},
		{
			name: "字节序 CDAB 本期不开放", point: wide(point("uint32", 0.01, 0, 0, 100), "CDAB"),
			words: []uint16{1, 2}, wantValue: 0, wantQuality: model.QualityParseError,
		},
		{
			name: "float32 本期不实现", point: wide(point("float32", 1, 0, 0, 100), "ABCD"),
			words: []uint16{0x4248, 0x0000}, wantValue: 0, wantQuality: model.QualityParseError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, quality := toEngineering(tc.point, tc.words)
			if quality != tc.wantQuality {
				t.Fatalf("质量码 = %d，期望 %d", quality, tc.wantQuality)
			}
			if tc.wantQuality == model.QualityParseError {
				if got != nil {
					t.Fatalf("解析失败时应无值，得到 %v", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("质量码 %d 必须有值，得到空", quality)
			}
			if *got != tc.wantValue {
				t.Fatalf("工程值 = %v，期望 %v", *got, tc.wantValue)
			}
		})
	}
}
