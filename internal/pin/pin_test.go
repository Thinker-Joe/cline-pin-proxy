package pin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
)

// decode 把注入结果解析回 map，方便断言。
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, body)
	}
	return out
}

func nested(t *testing.T, root map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := root
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("missing nested object at %q in %v", k, root)
		}
		cur = next
	}
	return cur
}

func stringsOf(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("value is not an array: %#v", v)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("array item is not a string: %#v", item)
		}
		out = append(out, s)
	}
	return out
}

// auto 管道下 strict 必须把两条管道都写成 only，这是本项目的核心行为。
func TestInjectStrictWritesBothPipelines(t *testing.T) {
	body := []byte(`{"model":"cline-pass/deepseek-v4.1-flash","messages":[]}`)

	got, err := Inject(body, Options{
		Upstreams: []string{"deepseek"},
		Mode:      config.PinStrict,
		Pipeline:  config.PipelineAuto,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	root := decode(t, got)

	gw := nested(t, root, "providerOptions", "gateway")
	if only := stringsOf(t, gw["only"]); len(only) != 1 || only[0] != "deepseek" {
		t.Errorf("providerOptions.gateway.only = %v, want [deepseek]", only)
	}
	if _, hasOrder := gw["order"]; hasOrder {
		t.Errorf("strict mode must not write order, got %v", gw["order"])
	}

	p := nested(t, root, "provider")
	if only := stringsOf(t, p["only"]); len(only) != 1 || only[0] != "deepseek" {
		t.Errorf("provider.only = %v, want [deepseek]", only)
	}
}

func TestInjectPreferredWritesOrder(t *testing.T) {
	body := []byte(`{"model":"m"}`)

	got, err := Inject(body, Options{
		Upstreams: []string{"deepseek", "novita"},
		Mode:      config.PinPreferred,
		Pipeline:  config.PipelineAuto,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	root := decode(t, got)

	gw := nested(t, root, "providerOptions", "gateway")
	if order := stringsOf(t, gw["order"]); len(order) != 2 || order[0] != "deepseek" || order[1] != "novita" {
		t.Errorf("providerOptions.gateway.order = %v", order)
	}
	if _, hasOnly := gw["only"]; hasOnly {
		t.Errorf("preferred mode must not write only, got %v", gw["only"])
	}
}

// 强制单管道时不得污染另一条，否则会改变对端行为。
func TestInjectPipelineScoping(t *testing.T) {
	body := []byte(`{"model":"m"}`)

	plannerOnly, err := Inject(body, Options{
		Upstreams: []string{"z-ai"}, Mode: config.PinStrict, Pipeline: config.PipelinePlanner,
	})
	if err != nil {
		t.Fatalf("Inject planner: %v", err)
	}
	root := decode(t, plannerOnly)
	if _, ok := root["provider"]; ok {
		t.Errorf("pipeline=planner must not write top-level provider, got %v", root["provider"])
	}
	nested(t, root, "providerOptions", "gateway")

	directOnly, err := Inject(body, Options{
		Upstreams: []string{"z-ai"}, Mode: config.PinStrict, Pipeline: config.PipelineDirect,
	})
	if err != nil {
		t.Fatalf("Inject direct: %v", err)
	}
	root = decode(t, directOnly)
	if _, ok := root["providerOptions"]; ok {
		t.Errorf("pipeline=direct must not write providerOptions, got %v", root["providerOptions"])
	}
	nested(t, root, "provider")
}

// 注入只能增加字段，不能动调用方原有的任何内容。
func TestInjectPreservesUnrelatedFields(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.7,"tools":[{"type":"function"}],"messages":[{"role":"user","content":"hi"}]}`)

	got, err := Inject(body, Options{
		Upstreams: []string{"deepseek"}, Mode: config.PinStrict, Pipeline: config.PipelineAuto,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	root := decode(t, got)

	if root["model"] != "m" {
		t.Errorf("model changed: %v", root["model"])
	}
	if root["temperature"] != 0.7 {
		t.Errorf("temperature changed: %v", root["temperature"])
	}
	if _, ok := root["tools"]; !ok {
		t.Errorf("tools field lost")
	}
	if _, ok := root["messages"]; !ok {
		t.Errorf("messages field lost")
	}
}

// 大整数必须逐位保留：map[string]any 默认会把数字变 float64 并静默改写。
func TestInjectPreservesLargeIntegers(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1，float64 无法精确表示
	body := []byte(`{"model":"m","max_tokens":` + big + `}`)

	got, err := Inject(body, Options{
		Upstreams: []string{"deepseek"}, Mode: config.PinStrict, Pipeline: config.PipelineAuto,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if !strings.Contains(string(got), big) {
		t.Errorf("large integer was rewritten:\n%s", got)
	}
}

// 关闭 HTML 转义，避免把提示词里的 < > & 改写成 \u003c 之类。
func TestInjectDoesNotHTMLEscape(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"content":"a < b & c > d"}]}`)

	got, err := Inject(body, Options{
		Upstreams: []string{"deepseek"}, Mode: config.PinStrict, Pipeline: config.PipelineAuto,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if strings.Contains(string(got), `\u003c`) || strings.Contains(string(got), `\u0026`) {
		t.Errorf("output was HTML escaped:\n%s", got)
	}
	if !strings.Contains(string(got), "a < b & c > d") {
		t.Errorf("content not preserved verbatim:\n%s", got)
	}
}

// 调用方已写的同级字段要保留，只覆盖我们负责的键。
func TestInjectMergesIntoExistingGatewayObject(t *testing.T) {
	body := []byte(`{"model":"m","providerOptions":{"gateway":{"sort":"cost","foo":"bar"}}}`)

	got, err := Inject(body, Options{
		Upstreams: []string{"deepseek"}, Mode: config.PinStrict, Pipeline: config.PipelinePlanner,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	gw := nested(t, decode(t, got), "providerOptions", "gateway")

	if gw["sort"] != "cost" {
		t.Errorf("existing sort was clobbered: %v", gw["sort"])
	}
	if gw["foo"] != "bar" {
		t.Errorf("existing unrelated key was dropped: %v", gw["foo"])
	}
	if only := stringsOf(t, gw["only"]); len(only) != 1 || only[0] != "deepseek" {
		t.Errorf("only = %v", only)
	}
}

// sort 在两条管道上的取值不同：direct 需要 OpenRouter 的别名。
func TestInjectSortMapping(t *testing.T) {
	got, err := Inject([]byte(`{"model":"m"}`), Options{
		Upstreams: []string{"deepseek"},
		Mode:      config.PinStrict,
		Pipeline:  config.PipelineAuto,
		Sort:      "cost",
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	root := decode(t, got)

	if s := nested(t, root, "providerOptions", "gateway")["sort"]; s != "cost" {
		t.Errorf("planner sort = %v, want cost", s)
	}
	if s := nested(t, root, "provider")["sort"]; s != "price" {
		t.Errorf("direct sort = %v, want price (OpenRouter 别名)", s)
	}
}

func TestInjectRejectsNonObjectBody(t *testing.T) {
	for _, body := range []string{`[]`, `"str"`, `{`, ``} {
		if _, err := Inject([]byte(body), Options{
			Upstreams: []string{"deepseek"}, Mode: config.PinStrict, Pipeline: config.PipelineAuto,
		}); err == nil {
			t.Errorf("Inject(%q) should fail", body)
		}
	}
}

func TestModelOf(t *testing.T) {
	got, err := ModelOf([]byte(`{"model":"  cline-pass/glm-5.3  ","messages":[]}`))
	if err != nil {
		t.Fatalf("ModelOf: %v", err)
	}
	if got != "cline-pass/glm-5.3" {
		t.Errorf("ModelOf = %q", got)
	}
	if _, err := ModelOf([]byte(`not json`)); err == nil {
		t.Errorf("ModelOf should fail on invalid JSON")
	}
}

func TestExtractUpstreams(t *testing.T) {
	// 真实 planner 响应结构：路由元数据在 data.choices[0].message 下，不在顶层。
	planner := map[string]any{
		"data": map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"content": "OK",
						"provider_metadata": map[string]any{
							"gateway": map[string]any{
								"routing": map[string]any{
									"finalProvider":      "deepseek",
									"canonicalSlug":      "deepseek/deepseek-v4.1-flash",
									"fallbacksAvailable": []any{},
								},
							},
						},
					},
				},
			},
		},
	}
	fp, slug := ExtractUpstreams(planner)
	if fp != "deepseek" {
		t.Errorf("planner finalProvider = %q, want deepseek", fp)
	}
	if slug != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("planner canonicalSlug = %q", slug)
	}

	// 真实 direct 响应结构：顶层 provider 是显示名，需要 slug 化。
	direct := map[string]any{
		"data": map[string]any{
			"provider": "Z.AI",
			"model":    "z-ai/glm-5.3-flash",
		},
	}
	if fp, _ := ExtractUpstreams(direct); fp != "z-ai" {
		t.Errorf("direct provider = %q, want z-ai", fp)
	}

	// 去掉 data 信封也要能用。
	unwrapped := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"provider_metadata": map[string]any{
						"gateway": map[string]any{"routing": map[string]any{"finalProvider": "novita"}},
					},
				},
			},
		},
	}
	if fp, _ := ExtractUpstreams(unwrapped); fp != "novita" {
		t.Errorf("unwrapped finalProvider = %q, want novita", fp)
	}

	// 没有路由信息的响应必须返回空串，不能瞎猜。
	if fp, slug := ExtractUpstreams(map[string]any{"choices": []any{}}); fp != "" || slug != "" {
		t.Errorf("expected empty for a routing-less payload, got %q/%q", fp, slug)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"GMICloud":  "gmicloud",
		"Z.AI":      "z-ai",
		"DeepSeek":  "deepseek",
		"  Alibaba": "alibaba",
		"a  b":      "a-b",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
