package config

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// WriteSeedSQL 生成可导入的 02_seed.sql。
//
// 配置域是投影：脚本先清空 terminal（级联到 bus/device/point）与 threshold，
// 再按配置文件重建，因此重复导入得到同一结果。测量、报文、告警等业务表不动。
//
// 只生成 SQL 文本，不连数据库——导入由 make seed 交给 psql。
func WriteSeedSQL(w io.Writer, terminals []*Terminal, th *ThresholdFile) error {
	ordered := append([]*Terminal(nil), terminals...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TerminalID < ordered[j].TerminalID })

	var b strings.Builder
	b.WriteString("-- 由 cmd/config-tool seed 生成，不要手改。\n")
	b.WriteString("-- 权威源：config/terminals/*.json 与 config/thresholds.json。\n")
	b.WriteString("-- 只重建配置域投影，不触碰测量、报文、告警等业务表。\n\n")
	b.WriteString("SET search_path TO monitor, public;\n\n")
	b.WriteString("BEGIN;\n\n")
	b.WriteString("DELETE FROM threshold;\n")
	b.WriteString("DELETE FROM terminal;\n\n")

	for _, t := range ordered {
		writeTerminal(&b, t)
	}
	writeThresholds(&b, ordered, th)

	b.WriteString("COMMIT;\n\n")
	b.WriteString("-- 初始化断言 D-01~D-04\n")
	b.WriteString("DO $$\nDECLARE\n")
	b.WriteString("  n_point     INT; n_terminal INT; n_device INT; n_threshold INT;\n")
	b.WriteString("BEGIN\n")
	b.WriteString("  SELECT count(*) INTO n_point     FROM point;\n")
	b.WriteString("  SELECT count(*) INTO n_terminal  FROM terminal;\n")
	b.WriteString("  SELECT count(*) INTO n_device    FROM device;\n")
	b.WriteString("  SELECT count(*) INTO n_threshold FROM threshold;\n")
	b.WriteString("  RAISE NOTICE 'D-01 point=% D-02 terminal=% D-03 device=% D-04 threshold=%',\n")
	b.WriteString("    n_point, n_terminal, n_device, n_threshold;\n")
	b.WriteString("END $$;\n")

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("写入种子脚本: %w", err)
	}
	return nil
}

func writeTerminal(b *strings.Builder, t *Terminal) {
	fmt.Fprintf(b, "INSERT INTO terminal (terminal_id, esn, workshop, line, enabled, config_version)\n")
	fmt.Fprintf(b, "  VALUES (%s, %s, %s, %s, TRUE, %s);\n\n",
		q(t.TerminalID), q(t.TerminalID), q(t.Workshop), q(t.Line), q(t.ConfigVersion))

	for _, bus := range t.Buses {
		fmt.Fprintf(b, "INSERT INTO bus (terminal_id, bus_id, port, baud_rate, data_bits, stop_bits, "+
			"parity, timeout_ms, retry_count, inter_frame_ms)\n")
		fmt.Fprintf(b, "  VALUES (%s, %s, %s, %d, %d, %d, %s, %d, %d, %d);\n\n",
			q(t.TerminalID), q(bus.BusID), q(bus.Port), bus.BaudRate, bus.DataBits,
			bus.StopBits, q(bus.Parity), bus.TimeoutMs, bus.RetryCount, bus.InterFrameMs)

		for _, dev := range bus.Devices {
			fmt.Fprintf(b, "INSERT INTO device (terminal_id, bus_id, slave_addr, device_name, device_type)\n")
			fmt.Fprintf(b, "  VALUES (%s, %s, %d, %s, %s);\n\n",
				q(t.TerminalID), q(bus.BusID), dev.SlaveAddr, q(dev.Name), q(dev.Type))
			writePoints(b, t.TerminalID, bus.BusID, dev)
		}
	}
}

func writePoints(b *strings.Builder, terminalID, busID string, dev Device) {
	rows := make([]string, 0, 8)
	for _, blk := range dev.Blocks {
		for _, p := range blk.Points {
			// 库里的 addr 是绝对寄存器地址；配置里的 addr 是块内偏移。
			absolute := *blk.StartAddr + *p.Addr
			rows = append(rows, fmt.Sprintf("  (%s, %s, %d, %s, %s, %s, %s, %d, %d, %s, %s, %s, %s, %s)",
				q(terminalID), q(busID), dev.SlaveAddr, q(p.Code), q(p.Name),
				q(p.SiType), q(p.DataType), absolute, p.Width,
				num(*p.Scale), num(deref(p.Offset)), qi(p.Unit),
				num(deref(p.MinValue)), num(deref(p.MaxValue))))
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("INSERT INTO point (terminal_id, bus_id, slave_addr, point_code, point_name, " +
		"si_type, data_type, addr, width, scale, offset_val, unit, min_value, max_value)\n  VALUES\n")
	b.WriteString(strings.Join(rows, ",\n"))
	b.WriteString(";\n\n")
}

func writeThresholds(b *strings.Builder, terminals []*Terminal, th *ThresholdFile) {
	rows := make([]string, 0, len(terminals)*len(th.Rules))
	for _, t := range terminals {
		for _, r := range th.Rules {
			if r.TerminalID != "" && r.TerminalID != t.TerminalID {
				continue
			}
			for _, code := range r.PointCodes {
				rows = append(rows, fmt.Sprintf("  (%s, %d, 'POINT', %s, %s, %s, %s, %s, %d, TRUE, %s)",
					q(r.AlarmType), r.Level, q(t.TerminalID), qi(stringPtr(r.BusID)),
					q(code), q(r.Operator), thresholdLiteral(r), derefInt(r.DebounceCount),
					q(th.ConfigVersion)))
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("INSERT INTO threshold (alarm_type, level, scope, terminal_id, bus_id, point_code, " +
		"operator, threshold_value, debounce_count, enabled, config_version)\n  VALUES\n")
	b.WriteString(strings.Join(rows, ",\n"))
	b.WriteString(";\n\n")
}

func thresholdLiteral(r ThresholdRule) string {
	if r.ThresholdValue == nil {
		return "NULL"
	}
	return num(*r.ThresholdValue)
}

func q(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func qi(s *string) string {
	if s == nil || *s == "" {
		return "NULL"
	}
	return q(*s)
}

// num 用最短往返表示输出浮点数，避免 %v 把 1e+06 这类值写成科学计数法。
func num(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
