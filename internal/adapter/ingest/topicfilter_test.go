package ingest_test

import (
	"strings"
	"testing"

	"workshop-monitor/internal/adapter/ingest"
)

// 两种命名方案都要派生出同一种形状的单条通配。
func TestTopicFilterFromTemplates(t *testing.T) {
	cases := []struct {
		name string
		tpl  ingest.TopicTemplates
		want string
	}{
		{"grid", gridTemplates(), "/edge/+/+/up"},
		{"internal", internalTemplates(), "/workshop/+/+/up"},
	}
	for _, c := range cases {
		got, err := ingest.TopicFilterFromTemplates(c.tpl)
		if err != nil {
			t.Fatalf("%s 派生失败: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s 派生为 %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 三类主题形状不一致时不能硬凑一个通配：覆盖不全的现象是某类数据永远不落库。
func TestTopicFilterRejectsMixedShapes(t *testing.T) {
	tpl := gridTemplates()
	tpl.Status = "/edge/{esn}/status"

	if _, err := ingest.TopicFilterFromTemplates(tpl); err == nil {
		t.Fatal("三类主题形状不一致时应报错")
	}
}

// 通配必须真能匹配按模板渲染出的主题——这是它与 Router 之间唯一的耦合点。
func TestTopicFilterMatchesRenderedTopics(t *testing.T) {
	tpl := gridTemplates()
	filter, err := ingest.TopicFilterFromTemplates(tpl)
	if err != nil {
		t.Fatalf("派生失败: %v", err)
	}
	r, err := ingest.NewRouter(tpl)
	if err != nil {
		t.Fatalf("路由构建失败: %v", err)
	}

	// 用 MQTT 通配语义做最小验证：+ 匹配单段，其余段字面相等。
	filterSegs := splitSegs(filter)
	for _, tmpl := range []string{tpl.Data, tpl.Event, tpl.Status} {
		topic := replaceEsn(tmpl, "ESN-ZX-0003")
		if !matches(filterSegs, splitSegs(topic)) {
			t.Fatalf("通配 %q 未覆盖 %q", filter, topic)
		}
		if _, _, ok := r.Match(topic); !ok {
			t.Fatalf("路由未识别 %q", topic)
		}
	}
}

func splitSegs(t string) []string {
	return strings.Split(strings.TrimPrefix(t, "/"), "/")
}

func replaceEsn(tmpl, esn string) string {
	return strings.Replace(tmpl, "{esn}", esn, 1)
}

func matches(filter, topic []string) bool {
	if len(filter) != len(topic) {
		return false
	}
	for i := range filter {
		if filter[i] == "#" {
			return true
		}
		if filter[i] != "+" && filter[i] != topic[i] {
			return false
		}
	}
	return true
}
