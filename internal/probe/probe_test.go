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

// planner 管道的错误是自然语言，需要从 "Available providers are: ..." 里提取。
func TestExtractPlannerStyle(t *testing.T) {
	body := `{"error":{"message":"No allowed providers are available. Available providers are: baseten, novita, deepseek, gmicloud."}}`

	pipeline, upstreams := Extract(body)
	if pipeline != "planner" {
		t.Errorf("pipeline = %q, want planner", pipeline)
	}
	want := []string{"baseten", "novita", "deepseek", "gmicloud"}
	if !reflect.DeepEqual(upstreams, want) {
		t.Errorf("upstreams = %v, want %v", upstreams, want)
	}
}

// direct 管道的错误是结构化 JSON，取 error.metadata.available_providers。
func TestExtractDirectStyle(t *testing.T) {
	body := `{"error":{"message":"no allowed providers","metadata":{"available_providers":["DeepSeek","novita"]}}}`

	pipeline, upstreams := Extract(body)
	if pipeline != "direct" {
		t.Errorf("pipeline = %q, want direct", pipeline)
	}
	want := []string{"deepseek", "novita"}
	if !reflect.DeepEqual(upstreams, want) {
		t.Errorf("upstreams = %v, want %v (应该小写化)", upstreams, want)
	}
}

// 真实网关有时在 JSON 前带状态前缀，解析要能跳过。
func TestExtractDirectStyleWithPrefix(t *testing.T) {
	body := "HTTP 400: " + `{"error":{"metadata":{"available_providers":["z-ai"]}}}`

	pipeline, upstreams := Extract(body)
	if pipeline != "direct" || len(upstreams) != 1 || upstreams[0] != "z-ai" {
		t.Errorf("got pipeline=%q upstreams=%v", pipeline, upstreams)
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

// 多词短语含空格会被剔除；含下划线或前导连字符的非法 slug 也会被剔除。
func TestParseProviderListFiltersNoise(t *testing.T) {
	got := parseProviderList(" the following, deepseek, are available, z-ai, bad_slug, -lead")
	want := []string{"deepseek", "z-ai"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseProviderList = %v, want %v", got, want)
	}
}

// 单个普通英文单词与真实 slug 无法区分，只能保留——这是该技巧的固有局限，
// 好在真实网关的清单里不会出现孤立的功能词。
func TestParseProviderListKeepsSingleWordTokens(t *testing.T) {
	got := parseProviderList("available, deepseek, gmicloud")
	want := []string{"available", "deepseek", "gmicloud"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseProviderList = %v, want %v", got, want)
	}
}

func TestParseProviderListDedupes(t *testing.T) {
	got := parseProviderList("deepseek, DeepSeek, deepseek")
	if len(got) != 1 || got[0] != "deepseek" {
		t.Errorf("got %v, want a single deepseek", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("  short  ", 40); got != "short" {
		t.Errorf("truncate trimmed = %q", got)
	}
	long := truncate("abcdefghij", 4)
	if long != "abcd..." {
		t.Errorf("truncate long = %q", long)
	}
}

// probeUpstream 是一个假网关：它断言收到的探测请求确实用了不存在的上游名，
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

		// 探测必须在目标管道上注入不可能存在的上游，否则不会在路由层失败。
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

func TestProbePlannerPipeline(t *testing.T) {
	upstream := probeUpstream(t, "cline-pass/deepseek-v4.1-flash", "Bearer sk-test", "", func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Available providers are: deepseek, novita, baseten."}}`))
	})

	cfg := &config.Config{Upstream: upstream.URL, APIKey: "sk-test"}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	res, err := Probe(context.Background(), upstream.Client(), cfg, "cline-pass/deepseek-v4.1-flash", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Pipeline != "planner" {
		t.Errorf("pipeline = %q, want planner", res.Pipeline)
	}
	want := []string{"deepseek", "novita", "baseten"}
	if !reflect.DeepEqual(res.Upstreams, want) {
		t.Errorf("upstreams = %v, want %v", res.Upstreams, want)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d", res.Status)
	}
}

func TestProbeDirectPipeline(t *testing.T) {
	upstream := probeUpstream(t, "cline-pass/glm-5.3-flash", "", "direct", func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"metadata":{"available_providers":["z-ai","gmicloud"]}}}`))
	})

	cfg := &config.Config{Upstream: upstream.URL}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	res, err := Probe(context.Background(), upstream.Client(), cfg, "cline-pass/glm-5.3-flash", "direct")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if res.Pipeline != "direct" {
		t.Errorf("pipeline = %q, want direct", res.Pipeline)
	}
	if !reflect.DeepEqual(res.Upstreams, []string{"z-ai", "gmicloud"}) {
		t.Errorf("upstreams = %v", res.Upstreams)
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
