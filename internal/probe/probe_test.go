package probe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
)

// 以下两个夹具是 2026-09 从 api.cline.bot 抓到的**真实响应原文**，未做任何改写。
//
// 保留原文很重要：早期版本的解析逻辑就是被真实格式打脸的——
// 外层 error 是字符串而非对象、模型名里的点号会截断正则、最后一项与 JSON 尾巴
// 之间没有逗号。合成的夹具发现不了这些。

// realPlannerResponse：cline-pass/deepseek-v4.1-flash，planner 管道。
// 上游 16 个，其中 wafer 是最后一项且紧跟 JSON 尾巴。
const realPlannerResponse = `{"error":"inference request failed: failed to invoke model 'deepseek/deepseek-v4.1-flash' from Vercel: request failed with status 400: {\"error\":{\"message\":\"No available providers match the 'only' filter: __probe__. Available providers are: alibaba, baseten, boundless, deepinfra, deepseek, fireworks, gmicloud, modal, morph, novita, parasail, particle, relace, runware, togetherai, wafer\",\"type\":\"invalid_request_error\",\"param\":{\"modelId\":\"deepseek/deepseek-v4.1-flash\"}}}","success":false}`

// realDirectResponse：cline-pass/glm-5.3-flash，direct 管道。
// 上游 27 个（与响应里的 endpoint_count 一致），且 z-ai 确实在列。
const realDirectResponse = `{"error":"inference request failed: failed to invoke model 'z-ai/glm-5.3-flash' from Openrouter: request failed with status 404: {\"error\":{\"message\":\"No allowed providers are available for the selected model. Providers serving z-ai/glm-5.3-flash-20260826: deepinfra, relace, morph, wafer, streamlake, gmicloud, novita, makora, crusoe, coreweave, sail-research, atlas-cloud, fireworks, phala, friendli, siliconflow, digitalocean, together, parasail, baseten, venice, io-net, cloudflare, z-ai, reka, nextbit, modal, but your request's provider.only preference permits only: __probe__.\",\"code\":404,\"metadata\":{\"available_providers\":[\"deepinfra\",\"relace\",\"morph\",\"wafer\",\"streamlake\",\"gmicloud\",\"novita\",\"makora\",\"crusoe\",\"coreweave\",\"sail-research\",\"atlas-cloud\",\"fireworks\",\"phala\",\"friendli\",\"siliconflow\",\"digitalocean\",\"together\",\"parasail\",\"baseten\",\"venice\",\"io-net\",\"cloudflare\",\"z-ai\",\"reka\",\"nextbit\",\"modal\"],\"requested_providers\":[\"__probe__\"],\"routing_funnel\":[{\"step\":\"Initial Endpoints\",\"endpoint_count\":27}],\"failed_routing_step\":\"Filter by Allowed Providers\"}}}","success":false}`

func TestExtractRealPlannerResponse(t *testing.T) {
	pipeline, upstreams := Extract(realPlannerResponse)

	if pipeline != "planner" {
		t.Errorf("pipeline = %q, want planner (响应里写的是 from Vercel)", pipeline)
	}
	if len(upstreams) != 16 {
		t.Fatalf("got %d upstreams, want 16: %v", len(upstreams), upstreams)
	}
	// 回归：早期版本因为 [^.]+ 被模型名里的 "v4.1" 截断，把最后一项 wafer 丢了。
	if upstreams[len(upstreams)-1] != "wafer" {
		t.Errorf("last upstream = %q, want wafer（最后一项容易被 JSON 尾巴吃掉）", upstreams[len(upstreams)-1])
	}
	for _, want := range []string{"deepseek", "wafer", "alibaba"} {
		if !contains(upstreams, want) {
			t.Errorf("missing %q in %v", want, upstreams)
		}
	}
}

func TestExtractRealDirectResponse(t *testing.T) {
	pipeline, upstreams := Extract(realDirectResponse)

	if pipeline != "direct" {
		t.Errorf("pipeline = %q, want direct (响应里写的是 from Openrouter)", pipeline)
	}
	if len(upstreams) != 27 {
		t.Fatalf("got %d upstreams, want 27: %v", len(upstreams), upstreams)
	}
	// 回归：早期版本按对象解 error，而真实响应里 error 是字符串，直接解析失败。
	if !contains(upstreams, "z-ai") {
		t.Errorf("missing z-ai in %v", upstreams)
	}
	if upstreams[len(upstreams)-1] != "modal" {
		t.Errorf("last upstream = %q, want modal", upstreams[len(upstreams)-1])
	}
}

// 结构化数组优先于自然语言清单：两者都在时要用数组（更完整、无歧义）。
func TestStructuredArrayWinsOverNaturalLanguage(t *testing.T) {
	list := extractAvailableProvidersArray(realDirectResponse)
	if len(list) != 27 {
		t.Fatalf("array extraction got %d, want 27", len(list))
	}
	// 自然语言那句以 "but your request's ..." 结尾，不能把英文词混进来。
	for _, item := range list {
		if !upstreamSlugRe.MatchString(item) {
			t.Errorf("invalid slug leaked in: %q", item)
		}
	}
}

func TestDetectPipeline(t *testing.T) {
	cases := map[string]string{
		"failed to invoke model 'x' from Vercel: oops":     "planner",
		"failed to invoke model 'x' from Openrouter: oops": "direct",
		"FROM VERCEL":    "planner",
		"unrelated text": "",
	}
	for input, want := range cases {
		if got := detectPipeline(input); got != want {
			t.Errorf("detectPipeline(%q) = %q, want %q", input, got, want)
		}
	}
}

// 标记式提取必须「遇非法即停」，否则会把 JSON 尾巴并进最后一项。
func TestExtractListAfterMarkerStopsAtTrailingJunk(t *testing.T) {
	got := extractListAfterMarker(
		`Providers serving z-ai/glm-5.3-flash-20260826: deepinfra, novita, modal, but your request's provider.only permits only: __probe__.`,
		"providers serving",
	)
	if !reflect.DeepEqual(got, []string{"deepinfra", "novita", "modal"}) {
		t.Errorf("got %v, want [deepinfra novita modal]", got)
	}
}

// 标记后紧跟模型名与冒号时要能跳过前缀。
func TestExtractListAfterMarkerSkipsModelPrefix(t *testing.T) {
	got := extractListAfterMarker("Available providers are: alibaba, z-ai, wafer", "available providers are")
	if !reflect.DeepEqual(got, []string{"alibaba", "z-ai", "wafer"}) {
		t.Errorf("got %v", got)
	}
}

func TestExtractReturnsNothingWhenUnrecognized(t *testing.T) {
	for _, body := range []string{
		``,
		`{"error":{"message":"model not found"}}`,
		`totally unrelated text`,
		`{"error":{"metadata":{"available_providers":[]}}}`,
	} {
		pipeline, upstreams := Extract(body)
		if pipeline != "" || len(upstreams) != 0 {
			t.Errorf("Extract(%q) = %q/%v, want empty", body, pipeline, upstreams)
		}
	}
}

func TestExtractHandlesErrorAsNestedObject(t *testing.T) {
	// 有些部署会把 error 直接给成对象，这条路径也要能走通。
	body := `{"error":{"message":"no allowed providers","metadata":{"available_providers":["DeepSeek","novita"]}}}`
	pipeline, upstreams := Extract(body)
	if !reflect.DeepEqual(upstreams, []string{"deepseek", "novita"}) {
		t.Errorf("upstreams = %v", upstreams)
	}
	if pipeline != "direct" {
		t.Errorf("pipeline = %q, want direct as fallback", pipeline)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("  short  ", 40); got != "short" {
		t.Errorf("truncate trimmed = %q", got)
	}
	if got := truncate("abcdefghij", 4); got != "abcd..." {
		t.Errorf("truncate long = %q", got)
	}
}

// ---------------------------------------------------------------------------
// HTTP 层测试
// ---------------------------------------------------------------------------

// probeUpstream 是一个假网关：它断言探测请求确实用了不存在的上游名，
// 然后按指定格式回一个路由层错误。
//
// wantPipeline 为空表示期望双管道都注入（auto），否则只期望对应那条。
func probeUpstream(t *testing.T, wantModel, wantAuth, wantPipeline string, reply func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("probe hit %q, want /chat/completions", r.URL.Path)
		}
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), wantAuth)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("probe body is not JSON: %v", err)
		}
		if body["model"] != wantModel {
			t.Errorf("model = %v, want %q", body["model"], wantModel)
		}

		if wantPipeline == "" || wantPipeline == "planner" {
			gw, _ := body["providerOptions"].(map[string]any)
			gateway, _ := gw["gateway"].(map[string]any)
			if only, _ := gateway["only"].([]any); len(only) != 1 || only[0] != impossibleUpstream {
				t.Errorf("providerOptions.gateway.only = %v, want [%s]", only, impossibleUpstream)
			}
		} else if _, ok := body["providerOptions"]; ok {
			t.Errorf("planner pipeline should not be written when forced to %q", wantPipeline)
		}

		if wantPipeline == "" || wantPipeline == "direct" {
			p, _ := body["provider"].(map[string]any)
			if only, _ := p["only"].([]any); len(only) != 1 || only[0] != impossibleUpstream {
				t.Errorf("provider.only = %v, want [%s]", only, impossibleUpstream)
			}
		} else if _, ok := body["provider"]; ok {
			t.Errorf("direct pipeline should not be written when forced to %q", wantPipeline)
		}

		reply(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeAgainstRealPlannerResponse(t *testing.T) {
	upstream := probeUpstream(t, "cline-pass/deepseek-v4.1-flash", "Bearer sk-test", "", func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(realPlannerResponse))
	})

	cfg := &config.Config{Upstream: upstream.URL, APIKey: "sk-test"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	res, err := Probe(context.Background(), upstream.Client(), cfg, "cline-pass/deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Pipeline != "planner" || len(res.Upstreams) != 16 {
		t.Errorf("pipeline=%q upstreams=%d, want planner/16", res.Pipeline, len(res.Upstreams))
	}
	if res.Detail != "" {
		t.Errorf("Detail should be empty on success, got %q", res.Detail)
	}
}

func TestProbeAgainstRealDirectResponse(t *testing.T) {
	upstream := probeUpstream(t, "cline-pass/glm-5.3-flash", "", "direct", func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(realDirectResponse))
	})

	cfg := &config.Config{Upstream: upstream.URL}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	res, err := Probe(context.Background(), upstream.Client(), cfg, "cline-pass/glm-5.3-flash", "direct")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Pipeline != "direct" || len(res.Upstreams) != 27 {
		t.Errorf("pipeline=%q upstreams=%d, want direct/27", res.Pipeline, len(res.Upstreams))
	}
}

// 网关措辞变化导致解析不出渠道时，要保留原始片段便于排查，而不是静默失败。
func TestProbeUnparsableKeepsDetail(t *testing.T) {
	upstream := probeUpstream(t, "cline-pass/kimi-k3", "", "", func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"something entirely new"}}`))
	})

	cfg := &config.Config{Upstream: upstream.URL}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	res, err := Probe(context.Background(), upstream.Client(), cfg, "cline-pass/kimi-k3", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(res.Upstreams) != 0 {
		t.Errorf("upstreams = %v, want empty", res.Upstreams)
	}
	if res.Detail == "" {
		t.Error("detail should carry the raw error snippet for diagnosis")
	}
}

func TestProbeUpstreamUnreachable(t *testing.T) {
	cfg := &config.Config{Upstream: "http://127.0.0.1:1"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(context.Background(), http.DefaultClient, cfg, "m", ""); err == nil {
		t.Fatal("expected an error for an unreachable upstream")
	}
}

// 探测必须带上 probe_headers。
//
// 实测：`deepseek/deepseek-v4-flash` 缺少 `x-client-type: cline-cli` 会直接 403，
// 此时探测会得出「不支持钉死」的错误结论，与线上真实行为相反。
func TestProbeSendsConfiguredHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(realDirectResponse))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Upstream:     srv.URL,
		ProbeHeaders: map[string]string{"x-client-type": "cline-cli", "x-extra": "1"},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	if _, err := Probe(context.Background(), srv.Client(), cfg, "m", ""); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if v := got.Get("x-client-type"); v != "cline-cli" {
		t.Errorf("x-client-type = %q, want cline-cli", v)
	}
	if v := got.Get("x-extra"); v != "1" {
		t.Errorf("x-extra = %q, want 1", v)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
