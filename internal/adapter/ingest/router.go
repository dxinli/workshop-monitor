package ingest

import (
	"fmt"
	"strings"

	"workshop-monitor/internal/model"
)

// esnPlaceholder 是主题模板里代表终端标识的占位符。
const esnPlaceholder = "{esn}"

// TopicTemplates 是一套命名方案的三类上行主题模板，形如 "/edge/{esn}/data/up"。
//
// 主题字符串一律由调用方从 config/topics.json 读入后传入，本包不出现任何字面量主题。
type TopicTemplates struct {
	Data   string
	Event  string
	Status string
}

// Router 按模板解析主题，提取终端标识与消息类型。
type Router struct {
	patterns []topicPattern
}

type topicPattern struct {
	segments []string
	msgType  string
}

// NewRouter 编译模板。模板缺 {esn}、占位符重复、三类模板相同时直接报错：
// 这些情况都会让解析结果不确定，而"解析出错的终端"比"启动失败"难查得多。
func NewRouter(t TopicTemplates) (*Router, error) {
	set := []struct {
		tmpl    string
		msgType string
		name    string
	}{
		{t.Data, model.MsgTypeData, "数据"},
		{t.Event, model.MsgTypeEvent, "事件"},
		{t.Status, model.MsgTypeStatus, "状态"},
	}

	r := &Router{}
	seen := make(map[string]string, len(set))
	for _, s := range set {
		if err := validateTemplate(s.name, s.tmpl); err != nil {
			return nil, err
		}
		if prev, ok := seen[s.tmpl]; ok {
			return nil, fmt.Errorf("%s与%s的上行主题模板相同：%q", prev, s.name, s.tmpl)
		}
		seen[s.tmpl] = s.name

		r.patterns = append(r.patterns, topicPattern{
			segments: splitTopic(s.tmpl),
			msgType:  s.msgType,
		})
	}
	return r, nil
}

func validateTemplate(name, tmpl string) error {
	if tmpl == "" {
		return fmt.Errorf("%s上行主题模板为空", name)
	}
	if !strings.HasPrefix(tmpl, "/") {
		return fmt.Errorf("%s上行主题模板 %q 必须以 / 开头", name, tmpl)
	}
	if n := strings.Count(tmpl, esnPlaceholder); n != 1 {
		return fmt.Errorf("%s上行主题模板 %q 必须且只能含一个 %s，实际 %d 个", name, tmpl, esnPlaceholder, n)
	}
	return nil
}

// Match 解析主题。第二个返回值是消息类型（DATA / EVENT / STATUS）。
// 不匹配任何模板时返回 ok=false，调用方应丢弃该消息并计数，不落库。
func (r *Router) Match(topic string) (terminalID, msgType string, ok bool) {
	segs := splitTopic(topic)
	for _, p := range r.patterns {
		if esn, hit := p.match(segs); hit {
			return esn, p.msgType, true
		}
	}
	return "", "", false
}

func splitTopic(t string) []string {
	if t == "" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(t, "/"), "/")
}

// match 逐段比较：占位符段捕获终端标识，其余段必须字面相等。
// 段数必须完全相同，否则 /edge/{esn}/data/up/extra 会被误判为数据主题。
func (p topicPattern) match(segs []string) (string, bool) {
	if len(segs) != len(p.segments) {
		return "", false
	}
	var esn string
	for i, want := range p.segments {
		if want == esnPlaceholder {
			if segs[i] == "" {
				return "", false
			}
			esn = segs[i]
			continue
		}
		if segs[i] != want {
			return "", false
		}
	}
	return esn, esn != ""
}
