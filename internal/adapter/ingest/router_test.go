package ingest_test

import (
	"testing"

	"workshop-monitor/internal/adapter/ingest"
)

func gridTemplates() ingest.TopicTemplates {
	return ingest.TopicTemplates{
		Data:   "/edge/{esn}/data/up",
		Event:  "/edge/{esn}/event/up",
		Status: "/edge/{esn}/status/up",
	}
}

func internalTemplates() ingest.TopicTemplates {
	return ingest.TopicTemplates{
		Data:   "/workshop/{esn}/data/up",
		Event:  "/workshop/{esn}/event/up",
		Status: "/workshop/{esn}/status/up",
	}
}

// 三类上行主题都要能解析出终端标识与消息类型。
func TestRouterMatchesAllThreeTypes(t *testing.T) {
	r, err := ingest.NewRouter(gridTemplates())
	if err != nil {
		t.Fatalf("模板合法却报错: %v", err)
	}

	cases := []struct {
		topic  string
		esn    string
		msgTyp string
	}{
		{"/edge/ESN-ZX-0001/data/up", "ESN-ZX-0001", "DATA"},
		{"/edge/ESN-ZX-0005/event/up", "ESN-ZX-0005", "EVENT"},
		{"/edge/ESN-ZX-0002/status/up", "ESN-ZX-0002", "STATUS"},
	}
	for _, c := range cases {
		gotEsn, gotType, ok := r.Match(c.topic)
		if !ok {
			t.Fatalf("%s 应能匹配", c.topic)
		}
		if gotEsn != c.esn || gotType != c.msgTyp {
			t.Fatalf("%s 解析为 (%q, %q)，期望 (%q, %q)", c.topic, gotEsn, gotType, c.esn, c.msgTyp)
		}
	}
}

// 换一套命名方案后解析结果不变，说明主题字符串确实只来自模板。
func TestRouterIsSchemeAgnostic(t *testing.T) {
	grid, err := ingest.NewRouter(gridTemplates())
	if err != nil {
		t.Fatalf("grid 模板报错: %v", err)
	}
	internal, err := ingest.NewRouter(internalTemplates())
	if err != nil {
		t.Fatalf("internal 模板报错: %v", err)
	}

	gotEsn, gotType, ok := internal.Match("/workshop/ESN-ZX-0004/data/up")
	if !ok || gotEsn != "ESN-ZX-0004" || gotType != "DATA" {
		t.Fatalf("internal 方案解析为 (%q, %q, %v)", gotEsn, gotType, ok)
	}

	// grid 模板不得匹配 internal 主题，否则切换方案等于没切换。
	if _, _, ok := grid.Match("/workshop/ESN-ZX-0004/data/up"); ok {
		t.Fatal("grid 模板不应匹配 internal 主题")
	}
}

// 非上行主题（预留的下行、通配、空主题）一律不匹配，调用方据此丢弃。
func TestRouterRejectsNonUplinkTopics(t *testing.T) {
	r, err := ingest.NewRouter(gridTemplates())
	if err != nil {
		t.Fatalf("模板合法却报错: %v", err)
	}

	for _, topic := range []string{
		"/edge/ESN-ZX-0001/cmd/down",
		"/edge/ESN-ZX-0001/param/down",
		"/edge/+/+/up",
		"/edge/ESN-ZX-0001/data",
		"/edge/ESN-ZX-0001/data/up/extra",
		"/other/ESN-ZX-0001/data/up",
		"",
	} {
		if _, _, ok := r.Match(topic); ok {
			t.Fatalf("%q 不应匹配上行主题", topic)
		}
	}
}

// 模板缺 {esn} 时构造即失败：没有占位符就无法提取终端标识，
// 而静默接受会让所有终端的数据落到同一个空标识上。
func TestNewRouterRejectsTemplateWithoutPlaceholder(t *testing.T) {
	bad := gridTemplates()
	bad.Event = "/edge/static/event/up"

	if _, err := ingest.NewRouter(bad); err == nil {
		t.Fatal("模板缺 {esn} 应在构造时失败")
	}
}

// 三类模板重复时也应在构造时失败：同一主题对应两种消息类型，解析结果不确定。
func TestNewRouterRejectsDuplicateTemplates(t *testing.T) {
	bad := gridTemplates()
	bad.Status = bad.Data

	if _, err := ingest.NewRouter(bad); err == nil {
		t.Fatal("三类模板重复应在构造时失败")
	}
}
