package ingest

import (
	"fmt"
	"strings"
)

// TopicFilterFromTemplates 由一套主题模板派生订阅通配。
//
// 规则：三类模板按段对齐，段数必须相同、末段必须是 up；同位置三段取值相同的保留字面，
// 不同的降级为 +。终端标识那一段本来就是占位符，一律降级为 +。
//
// 两套命名方案同形、仅前缀不同，因此都派生出 "/<前缀>/+/+/up"。
// 模板形状超出该假设时直接报错，而不是返回一个覆盖不全的通配——
// 订阅少了主题的现象是「某类数据永远不落库」，比启动失败难查得多。
func TopicFilterFromTemplates(t TopicTemplates) (string, error) {
	tmpls := []struct {
		name string
		tmpl string
	}{
		{"数据", t.Data}, {"事件", t.Event}, {"状态", t.Status},
	}

	parts := make([][]string, 0, len(tmpls))
	for _, m := range tmpls {
		if err := validateTemplate(m.name, m.tmpl); err != nil {
			return "", err
		}
		segs := splitTopic(m.tmpl)
		for i, s := range segs {
			if s == esnPlaceholder {
				segs[i] = "+"
			}
		}
		parts = append(parts, segs)
	}

	width := len(parts[0])
	for i, p := range parts {
		if len(p) != width {
			return "", fmt.Errorf("%s上行主题模板的段数与数据主题不同（%d / %d），无法用单条订阅覆盖",
				tmpls[i].name, len(p), width)
		}
		if p[width-1] != "up" {
			return "", fmt.Errorf("%s上行主题模板 %q 的最后一段应为 up", tmpls[i].name, tmpls[i].tmpl)
		}
	}

	merged := make([]string, width)
	for i := 0; i < width; i++ {
		merged[i] = parts[0][i]
		for _, p := range parts[1:] {
			if p[i] != merged[i] {
				merged[i] = "+"
				break
			}
		}
	}
	return "/" + strings.Join(merged, "/"), nil
}
