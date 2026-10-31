package config

import (
	"fmt"
	"sort"
	"strings"
)

// validateTopics 校验 config/topics.json。
//
// 主题模板是冻结线 A 的一部分，但取值不写进代码：这里只保证两套方案都齐备、
// 模板里带 {esn} 占位符、且同一方案内三类主题互不相同。
func validateTopics(b *builder, tf *TopicFile) {
	const file = "topics.json"
	b.need("V-01", file, "configVersion", tf.ConfigVersion)
	if len(tf.Schemes) == 0 {
		b.fatal("V-01", file, "schemes", "<空对象>", "至少一套命名方案")
		return
	}

	for _, name := range sortedKeys(tf.Schemes) {
		s := tf.Schemes[name]
		path := "schemes." + name
		seen := map[string]string{}
		for _, f := range []struct{ key, val string }{
			{"dataUp", s.DataUp}, {"eventUp", s.EventUp}, {"statusUp", s.StatusUp},
		} {
			field := path + "." + f.key
			if !b.need("V-01", file, field, f.val) {
				continue
			}
			if !strings.Contains(f.val, "{esn}") {
				b.fatal("V-01", file, field, rawString(f.val), "主题模板必须包含 {esn} 占位符")
				continue
			}
			if prev, ok := seen[f.val]; ok {
				b.fatal("V-01", file, field, rawString(f.val),
					fmt.Sprintf("与 %s.%s 相同，三类上行主题不得重名", path, prev))
				continue
			}
			seen[f.val] = f.key
		}
	}
}

func sortedKeys(m map[string]TopicSchemes) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
