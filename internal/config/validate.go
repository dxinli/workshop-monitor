package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Severity 决定校验失败时是否拒绝启动。
type Severity int

const (
	// Fatal 对应 V-01~V-09、V-11：拒绝启动。
	Fatal Severity = iota
	// Warning 对应 V-10：只告警，不拒绝。
	Warning
)

// Issue 是一条校验失败。Field 为 JSON 路径，Value 为原始值，两者缺一不可。
type Issue struct {
	Rule   string
	File   string
	Field  string
	Value  string
	Detail string
	Sev    Severity
}

func (i Issue) String() string {
	return fmt.Sprintf("%s %s %s = %s：%s", i.File, i.Rule, i.Field, i.Value, i.Detail)
}

// Report 是一次校验的全量结果与统计。
type Report struct {
	Issues    []Issue
	Terminals int
	Buses     int
	Slaves    int
	Points    int
	Rules     int
	Versions  map[string]string
}

// Failed 表示存在 Fatal 级问题。
func (r Report) Failed() bool {
	for _, i := range r.Issues {
		if i.Sev == Fatal {
			return true
		}
	}
	return false
}

var pointCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

var (
	dataTypeWidth = map[string]int{"int16": 1, "uint16": 1, "int32": 2, "uint32": 2}
	siTypes       = map[string]bool{"YC": true, "YX": true}
	alarmTypes    = map[string]bool{
		"HIGH_LIMIT": true, "HIGH_HIGH_LIMIT": true, "LOW_LIMIT": true,
		"STATE_ALARM": true, "SENSOR_OFFLINE": true, "TERMINAL_OFFLINE": true,
		"CACHE_WARNING": true, "DATA_QUALITY_ERROR": true,
	}
	operators = map[string]bool{">": true, ">=": true, "<": true, "<=": true}
)

type builder struct{ issues []Issue }

func (b *builder) add(rule, file, field, value, detail string, sev Severity) {
	b.issues = append(b.issues, Issue{
		Rule: rule, File: file, Field: field, Value: value, Detail: detail, Sev: sev,
	})
}

func (b *builder) fatal(rule, file, field, value, detail string) {
	b.add(rule, file, field, value, detail, Fatal)
}

// need 是 V-01 的单一落点：空串即视为未填。
func (b *builder) need(rule, file, field, value string) bool {
	if strings.TrimSpace(value) == "" {
		b.fatal(rule, file, field, rawString(value), "必填字段缺失")
		return false
	}
	return true
}

// needPtr 处理「0 是合法值」的整数字段：指针为 nil 说明键不存在。
func (b *builder) needPtr(rule, file, field string, v *int) bool {
	if v == nil {
		b.fatal(rule, file, field, "<缺失>", "必填字段缺失")
		return false
	}
	return true
}

func (b *builder) needFloat(rule, file, field string, v *float64) bool {
	if v == nil {
		b.fatal(rule, file, field, "<缺失>", "必填字段缺失")
		return false
	}
	return true
}

func rawString(s string) string {
	if strings.TrimSpace(s) == "" {
		return "<缺失>"
	}
	return fmt.Sprintf("%q", s)
}

func rawValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "<缺失>"
	case string:
		return rawString(x)
	default:
		return fmt.Sprint(x)
	}
}

// Validate 跑全部规则。每条规则独立执行，不因前一条失败而跳过。
// topics 为 nil 时跳过主题模板校验（只校验点表与阈值的调用方）。
func Validate(terminals []*Terminal, th *ThresholdFile, topics *TopicFile) Report {
	b := &builder{}
	rep := Report{Versions: map[string]string{}}

	for _, t := range terminals {
		validateTerminal(b, t, t.fileName())
		rep.Terminals++
		rep.Versions[t.TerminalID] = t.ConfigVersion
		for _, bus := range t.Buses {
			rep.Buses++
			rep.Slaves += len(bus.Devices)
			for _, dev := range bus.Devices {
				for _, blk := range dev.Blocks {
					rep.Points += len(blk.Points)
				}
			}
		}
	}

	validateV10(b, rep)
	validateThresholds(b, th, "thresholds.json", terminals)
	rep.Rules = len(th.Rules)
	if topics != nil {
		validateTopics(b, topics)
	}
	rep.Issues = b.issues
	sort.SliceStable(rep.Issues, func(i, j int) bool {
		if rep.Issues[i].File != rep.Issues[j].File {
			return rep.Issues[i].File < rep.Issues[j].File
		}
		return rep.Issues[i].Field < rep.Issues[j].Field
	})
	return rep
}

func validateTerminal(b *builder, t *Terminal, file string) {
	b.need("V-01", file, "terminalId", t.TerminalID)
	b.need("V-01", file, "configVersion", t.ConfigVersion)
	b.need("V-01", file, "workshop", t.Workshop)
	b.need("V-01", file, "line", t.Line)
	if t.CollectIntervalMs <= 0 {
		b.fatal("V-01", file, "collectIntervalMs", rawValue(t.CollectIntervalMs), "必填且必须为正整数")
	}
	if len(t.Buses) == 0 {
		b.fatal("V-01", file, "buses", "<空数组>", "至少一条总线")
	}
	for i := range t.Buses {
		validateBus(b, file, fmt.Sprintf("buses[%d]", i), &t.Buses[i])
	}
}

func validateBus(b *builder, file, path string, bus *Bus) {
	b.need("V-01", file, path+".busId", bus.BusID)
	b.need("V-01", file, path+".port", bus.Port)
	b.need("V-01", file, path+".parity", bus.Parity)
	for _, f := range []struct {
		name string
		val  int
	}{
		{"baudRate", bus.BaudRate}, {"dataBits", bus.DataBits}, {"stopBits", bus.StopBits},
		{"timeoutMs", bus.TimeoutMs}, {"retryCount", bus.RetryCount},
	} {
		if f.val <= 0 {
			b.fatal("V-01", file, path+"."+f.name, rawValue(f.val), "必填且必须为正整数")
		}
	}
	if len(bus.Devices) == 0 {
		b.fatal("V-01", file, path+".devices", "<空数组>", "至少一个从站")
	}

	seen := map[int]int{}
	for i := range bus.Devices {
		dev := &bus.Devices[i]
		devPath := fmt.Sprintf("%s.devices[%d]", path, i)
		validateV02(b, file, devPath, i, bus, seen)
		validateDevice(b, file, devPath, dev)
	}
}

// validateV02：从站地址 1~247，且同一总线内唯一。
func validateV02(b *builder, file, path string, idx int, bus *Bus, seen map[int]int) {
	dev := &bus.Devices[idx]
	if dev.SlaveAddr < 1 || dev.SlaveAddr > 247 {
		b.fatal("V-02", file, path+".slaveAddr", rawValue(dev.SlaveAddr), "从站地址必须在 1~247")
		return
	}
	if prev, ok := seen[dev.SlaveAddr]; ok {
		b.fatal("V-02", file, path+".slaveAddr", rawValue(dev.SlaveAddr),
			fmt.Sprintf("与 devices[%d] 重复，同一总线内从站地址必须唯一", prev))
		return
	}
	seen[dev.SlaveAddr] = idx
}

func validateDevice(b *builder, file, path string, dev *Device) {
	b.need("V-01", file, path+".name", dev.Name)
	b.need("V-01", file, path+".type", dev.Type)
	if len(dev.Blocks) == 0 {
		b.fatal("V-01", file, path+".blocks", "<空数组>", "至少一个寄存器块")
	}

	codes := map[string]int{}
	total := 0
	for i := range dev.Blocks {
		blkPath := fmt.Sprintf("%s.blocks[%d]", path, i)
		total += validateBlock(b, file, blkPath, &dev.Blocks[i], codes)
	}
	// V-09：单从站寄存器数不超过 125。
	if total > 125 {
		b.fatal("V-09", file, path, rawValue(total), "单从站寄存器总数超过 125")
	}
}

// validateBlock 跑 V-01/V-06/V-07 与 V-03，返回本块声明的寄存器数。
func validateBlock(b *builder, file, path string, blk *Block, codes map[string]int) int {
	if blk.FunctionCode != 3 {
		b.fatal("V-01", file, path+".functionCode", rawValue(blk.FunctionCode), "本期只支持 03 读保持寄存器")
	}
	if blk.RegisterCount <= 0 || blk.RegisterCount > 125 {
		b.fatal("V-09", file, path+".registerCount", rawValue(blk.RegisterCount),
			"块寄存器数必须在 1~125")
	}
	if blk.StartAddr == nil {
		b.fatal("V-01", file, path+".startAddr", "<缺失>", "必填字段缺失")
	} else if *blk.StartAddr < 0 {
		b.fatal("V-06", file, path+".startAddr", rawValue(*blk.StartAddr), "起始地址不得为负")
	}
	if len(blk.Points) == 0 {
		b.fatal("V-01", file, path+".points", "<空数组>", "寄存器块至少包含一个测点")
	}

	spans := make([]span, 0, len(blk.Points))
	for i := range blk.Points {
		pPath := fmt.Sprintf("%s.points[%d]", path, i)
		spans = append(spans, validatePoint(b, file, pPath, &blk.Points[i], codes))
	}
	validateV06(b, file, path, blk, spans)
	validateV07(b, file, path, blk, spans)
	return blk.RegisterCount
}

type span struct {
	start, end int
	valid      bool
}

func validatePoint(b *builder, file, path string, p *Point, codes map[string]int) span {
	b.need("V-01", file, path+".code", p.Code)
	b.need("V-01", file, path+".name", p.Name)
	b.need("V-01", file, path+".siType", p.SiType)
	b.need("V-01", file, path+".dataType", p.DataType)
	b.need("V-01", file, path+".byteOrder", p.ByteOrder)

	// V-03：测点编码设备内唯一，且匹配 ^[a-z][a-z0-9_]{1,31}$。
	if p.Code != "" {
		if !pointCodePattern.MatchString(p.Code) {
			b.fatal("V-03", file, path+".code", rawString(p.Code),
				"测点编码须匹配 ^[a-z][a-z0-9_]{1,31}$")
		}
		if prev, ok := codes[p.Code]; ok {
			b.fatal("V-03", file, path+".code", rawString(p.Code),
				fmt.Sprintf("与 points[%d] 重复，测点编码设备内唯一", prev))
		} else {
			codes[p.Code] = countIndex(path)
		}
	}

	// V-04：数据类型与寄存器宽度匹配。
	if want, ok := dataTypeWidth[p.DataType]; !ok && p.DataType != "" {
		b.fatal("V-04", file, path+".dataType", rawString(p.DataType), "未知数据类型")
	} else if ok && p.Width != want {
		b.fatal("V-04", file, path+".width", rawValue(p.Width),
			fmt.Sprintf("%s 的寄存器宽度必须为 %d", p.DataType, want))
	}

	validateV05(b, file, path, p)
	validateV08(b, file, path, p)

	s := span{valid: false}
	if b.needPtr("V-01", file, path+".addr", p.Addr) {
		if *p.Addr < 0 {
			b.fatal("V-06", file, path+".addr", rawValue(*p.Addr), "块内偏移不得为负")
			return s
		}
		s.start = *p.Addr
		s.end = *p.Addr + max(p.Width, 0)
		s.valid = p.Width > 0
	}
	if p.Width <= 0 {
		b.fatal("V-04", file, path+".width", rawValue(p.Width), "寄存器宽度必须为正")
	}
	return s
}

// validateV05：缩放系数非零，且量程下限小于上限（含 offset 后的可取值范围）。
func validateV05(b *builder, file, path string, p *Point) {
	if b.needFloat("V-01", file, path+".scale", p.Scale) && *p.Scale == 0 {
		b.fatal("V-05", file, path+".scale", rawValue(*p.Scale), "缩放系数不得为 0")
	}
	if !b.needFloat("V-01", file, path+".minValue", p.MinValue) {
		return
	}
	if !b.needFloat("V-01", file, path+".maxValue", p.MaxValue) {
		return
	}
	if *p.MinValue >= *p.MaxValue {
		b.fatal("V-05", file, path+".minValue", rawValue(*p.MinValue),
			fmt.Sprintf("量程下限必须小于上限 %v", *p.MaxValue))
	}
}

func validateV08(b *builder, file, path string, p *Point) {
	if p.SiType == "" {
		return // V-01 已报
	}
	if !siTypes[p.SiType] {
		b.fatal("V-08", file, path+".siType", rawString(p.SiType), "四遥类型只能是 YC 或 YX")
	}
}

// validateV06：块内 addr+width 不重叠、不越界。
func validateV06(b *builder, file, path string, blk *Block, spans []span) {
	ordered := make([]span, 0, len(spans))
	for _, s := range spans {
		if s.valid {
			ordered = append(ordered, s)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].start < ordered[j].start })

	for _, s := range ordered {
		if s.end > blk.RegisterCount {
			b.fatal("V-06", file, path, rawValue(fmt.Sprintf("%d..%d", s.start, s.end)),
				fmt.Sprintf("越界：块寄存器数仅 %d", blk.RegisterCount))
		}
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i].start < ordered[i-1].end {
			b.fatal("V-06", file, path, rawValue(fmt.Sprintf("%d..%d", ordered[i].start, ordered[i].end)),
				fmt.Sprintf("与 %d..%d 重叠", ordered[i-1].start, ordered[i-1].end))
		}
	}
}

// validateV07：块内点必须从 0 起连续铺满 registerCount，无空洞、无悬空尾部。
// 嵌套结构下「孤立点」在语法上不存在，这里校验的是它的等价形式：
// 每个被读取的寄存器都必须归属于某个测点。
func validateV07(b *builder, file, path string, blk *Block, spans []span) {
	ordered := make([]span, 0, len(spans))
	for _, s := range spans {
		if s.valid {
			ordered = append(ordered, s)
		}
	}
	if len(ordered) == 0 {
		return
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].start < ordered[j].start })

	if ordered[0].start != 0 {
		b.fatal("V-07", file, path, rawValue(ordered[0].start), "块首寄存器未映射到测点")
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i].start > ordered[i-1].end {
			b.fatal("V-07", file, path,
				rawValue(fmt.Sprintf("%d..%d", ordered[i-1].end, ordered[i].start)),
				"寄存器空洞未映射到测点")
		}
	}
	if last := ordered[len(ordered)-1].end; last != blk.RegisterCount {
		b.fatal("V-07", file, path, rawValue(fmt.Sprintf("%d..%d", last, blk.RegisterCount)),
			fmt.Sprintf("块尾寄存器未映射到测点（registerCount=%d）", blk.RegisterCount))
	}
}

// validateV10：5 × 2 × 5 从站 × 14 = 140。只告警，不拒绝。
func validateV10(b *builder, rep Report) {
	const wantTerminals, wantBuses, wantSlaves, wantPoints = 5, 10, 50, 140
	got := map[string][2]int{
		"终端数": {rep.Terminals, wantTerminals},
		"总线数": {rep.Buses, wantBuses},
		"从站数": {rep.Slaves, wantSlaves},
		"测点数": {rep.Points, wantPoints},
	}
	for _, name := range []string{"终端数", "总线数", "从站数", "测点数"} {
		v := got[name]
		if v[0] != v[1] {
			b.add("V-10", "config/terminals", name, fmt.Sprint(v[0]),
				fmt.Sprintf("与约定规模不一致：期望 %d，实际 %d（仅告警）", v[1], v[0]), Warning)
		}
	}
}

// countIndex 从 JSON 路径里取最后一个下标，用于「与第几个重复」的提示。
func countIndex(path string) int {
	open := strings.LastIndex(path, "[")
	closeIdx := strings.LastIndex(path, "]")
	if open < 0 || closeIdx <= open {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(path[open+1:closeIdx], "%d", &n); err != nil {
		return -1
	}
	return n
}

// ValidateTerminal 只跑单台终端的校验规则（V-01~V-09），供 edge-agent 启动时使用。
//
// 不跑 V-10：那是整份配置的规模核对（5 终端 / 10 总线 / 140 点），单台终端启动时
// 必然不满足，报警只会变成每个终端的噪声。不跑 V-11 与主题模板：阈值与主题属于平台侧，
// edge-agent 不读这两份文件，拿不到就不该判它。
func ValidateTerminal(t *Terminal) Report {
	b := &builder{}
	validateTerminal(b, t, t.fileName())
	sort.SliceStable(b.issues, func(i, j int) bool { return b.issues[i].Field < b.issues[j].Field })
	return Report{
		Issues:    b.issues,
		Terminals: 1,
		Buses:     len(t.Buses),
		Slaves:    countSlaves(t),
		Points:    countPoints(t),
		Versions:  map[string]string{t.TerminalID: t.ConfigVersion},
	}
}

func countSlaves(t *Terminal) int {
	n := 0
	for _, bus := range t.Buses {
		n += len(bus.Devices)
	}
	return n
}

func countPoints(t *Terminal) int {
	n := 0
	for _, bus := range t.Buses {
		for _, dev := range bus.Devices {
			for _, blk := range dev.Blocks {
				n += len(blk.Points)
			}
		}
	}
	return n
}
