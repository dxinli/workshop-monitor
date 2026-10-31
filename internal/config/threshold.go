package config

import (
	"fmt"
	"sort"
	"strings"
)

// validateThresholds 跑 V-11，并校验模板里的 pointCodes 能落到点上。
func validateThresholds(b *builder, tf *ThresholdFile, file string, terminals []*Terminal) {
	b.need("V-01", file, "configVersion", tf.ConfigVersion)
	if len(tf.Rules) == 0 {
		b.fatal("V-01", file, "rules", "<空数组>", "至少一条告警规则")
		return
	}

	known := knownPointCodes(terminals)
	ids := map[string]int{}
	scopes := map[string]int{}

	for i, r := range tf.Rules {
		path := fmt.Sprintf("rules[%d]", i)
		if b.need("V-11", file, path+".id", r.ID) {
			if prev, ok := ids[r.ID]; ok {
				b.fatal("V-11", file, path+".id", rawString(r.ID),
					fmt.Sprintf("与 rules[%d] 重复", prev))
			}
			ids[r.ID] = i
		}
		validateRuleFields(b, file, path, r)
		validateRulePoints(b, file, path, r, known)
		validateRuleScope(b, file, path, r, scopes)
	}
}

func validateRuleFields(b *builder, file, path string, r ThresholdRule) {
	if len(r.PointCodes) == 0 {
		b.fatal("V-11", file, path+".pointCodes", "<空数组>", "必须至少指定一个测点编码")
	}
	if r.AlarmType == "" {
		b.fatal("V-11", file, path+".alarmType", "<缺失>", "告警类型必填")
	} else if !alarmTypes[r.AlarmType] {
		b.fatal("V-11", file, path+".alarmType", rawString(r.AlarmType), "不在告警类型枚举内")
	}
	if r.Operator == "" || !operators[r.Operator] {
		b.fatal("V-11", file, path+".operator", rawString(r.Operator), "运算符只能是 > >= < <=")
	}
	if r.Level < 1 || r.Level > 4 {
		b.fatal("V-11", file, path+".level", rawValue(r.Level), "级别必须在 1~4")
	}
	if r.DebounceCount == nil {
		b.fatal("V-11", file, path+".debounceCount", "<缺失>", "去抖次数必填，开关量写 0")
	} else if *r.DebounceCount < 0 {
		b.fatal("V-11", file, path+".debounceCount", rawValue(*r.DebounceCount), "去抖次数不得为负")
	} else if r.AlarmType == "STATE_ALARM" && *r.DebounceCount != 0 {
		b.fatal("V-11", file, path+".debounceCount", rawValue(*r.DebounceCount),
			"状态报警不去抖，debounceCount 必须为 0")
	}
	// 状态报警是等值判定，没有阈值；阈值类必须给阈值。
	if r.AlarmType == "STATE_ALARM" {
		return
	}
	if r.ThresholdValue == nil {
		b.fatal("V-11", file, path+".thresholdValue", "<缺失>", "阈值类规则必须给阈值")
	}
}

func validateRulePoints(b *builder, file, path string, r ThresholdRule, known map[string]bool) {
	for j, code := range r.PointCodes {
		field := fmt.Sprintf("%s.pointCodes[%d]", path, j)
		if !pointCodePattern.MatchString(code) {
			b.fatal("V-11", file, field, rawString(code),
				"测点编码须匹配 ^[a-z][a-z0-9_]{1,31}$")
			continue
		}
		if !known[code] {
			b.fatal("V-11", file, field, rawString(code), "点表里没有该测点编码")
		}
	}
}

// validateRuleScope：模板按「终端 + 总线」展开成库里的 threshold 行，
// 同一作用域内同一告警类型只能有一条，否则 ux_threshold_scope 会拒绝导入。
func validateRuleScope(b *builder, file, path string, r ThresholdRule, seen map[string]int) {
	if r.BusID != "" && r.BusID != "A" && r.BusID != "B" {
		b.fatal("V-11", file, path+".busId", rawString(r.BusID), "总线标识只能是 A 或 B")
	}
	key := strings.Join([]string{
		r.TerminalID, r.BusID,
		strings.Join(sortedCopy(r.PointCodes), ","),
		r.AlarmType,
	}, "|")
	if prev, ok := seen[key]; ok {
		b.fatal("V-11", file, path, rawString(r.ID),
			fmt.Sprintf("与 rules[%d] 展开后作用域相同，导入时会被唯一索引拒绝", prev))
		return
	}
	seen[key] = countIndex(path)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func knownPointCodes(terminals []*Terminal) map[string]bool {
	out := map[string]bool{}
	for _, t := range terminals {
		for _, bus := range t.Buses {
			for _, dev := range bus.Devices {
				for _, blk := range dev.Blocks {
					for _, p := range blk.Points {
						out[p.Code] = true
					}
				}
			}
		}
	}
	return out
}
