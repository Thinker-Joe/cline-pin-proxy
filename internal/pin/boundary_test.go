package pin

import (
	"encoding/json"
	"testing"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
)

// 代理管理它写入的那几条路由字段，必须清掉互斥的旧值。
//
// 调用方若已经发了 provider.only=["old"]，preferred 规则只追加 order 是无效的：
// 按 only 的过滤语义新候选根本不在允许集合里，配置的回退顺序形同虚设。
func TestPreferredClearsConflictingOnly(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"messages": [],
		"providerOptions": {"gateway": {"only": ["old"]}},
		"provider": {"only": ["old"]}
	}`)

	out, err := Inject(body, Options{
		Upstreams: []string{"new-a", "new-b"},
		Mode:      config.PinPreferred,
		Pipeline:  config.PipelineAuto,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range [][]string{
		{"providerOptions", "gateway"},
		{"provider"},
	} {
		obj := dig(t, out, path...)
		if _, stale := obj["only"]; stale {
			t.Errorf("%v 里仍留着旧的 only=%v，配置的 order 无法生效", path, obj["only"])
		}
		order, _ := obj["order"].([]any)
		if len(order) != 2 || order[0] != "new-a" || order[1] != "new-b" {
			t.Errorf("%v.order = %v, want [new-a new-b]", path, obj["order"])
		}
	}
}

// 反向对称：strict 也必须清掉旧的 order，否则观测到的字段自相矛盾。
func TestStrictClearsStaleOrder(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"messages": [],
		"provider": {"order": ["old-a", "old-b"]}
	}`)

	out, err := Inject(body, Options{
		Upstreams: []string{"deepseek"},
		Mode:      config.PinStrict,
		Pipeline:  config.PipelineAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	obj := dig(t, out, "provider")
	if _, stale := obj["order"]; stale {
		t.Errorf("provider 里仍留着旧的 order=%v", obj["order"])
	}
	only, _ := obj["only"].([]any)
	if len(only) != 1 || only[0] != "deepseek" {
		t.Errorf("provider.only = %v, want [deepseek]", obj["only"])
	}
}

// preferred 的语义是"按序尝试、允许回退"。调用方若带 allow_fallbacks=false，
// 回退会被网关禁掉，配置的多候选退化成"只用第一个"——属于静默失效。
func TestPreferredClearsAllowFallbacksFalse(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"provider":{"allow_fallbacks":false}}`)

	out, err := Inject(body, Options{
		Upstreams: []string{"a", "b"},
		Mode:      config.PinPreferred,
		Pipeline:  config.PipelineDirect,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := dig(t, out, "provider")["allow_fallbacks"]; present {
		t.Error("preferred 下必须清掉 allow_fallbacks，否则回退顺序不生效")
	}
}

// 不相关的字段一个都不能动。
func TestUnrelatedRoutingFieldsArePreserved(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"messages": [],
		"temperature": 0.5,
		"provider": {"require_parameters": true, "data_collection": "deny", "only": ["old"]},
		"providerOptions": {"gateway": {"only": ["old"]}}
	}`)

	out, err := Inject(body, Options{
		Upstreams: []string{"a", "b"},
		Mode:      config.PinPreferred,
		Pipeline:  config.PipelineAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := dig(t, out, "provider"); got["require_parameters"] != true || got["data_collection"] != "deny" {
		t.Errorf("无关字段被改动: %v", got)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root["temperature"] != 0.5 {
		t.Errorf("top-level 字段被改动: %v", root["temperature"])
	}
}

func dig(t *testing.T, body []byte, keys ...string) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("注入结果不是 JSON: %v\n%s", err, body)
	}
	cur := root
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		cur = next
	}
	return cur
}
