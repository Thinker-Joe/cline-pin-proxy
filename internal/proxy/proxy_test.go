package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
)

// capture 是一个假的 Cline Pass 上游，记录收到的请求体并返回可编排的响应。
type capture struct {
	path    string
	headers http.Header
	body    []byte
}

// newUpstream 启动假上游。handler 为 nil 时回一个最小合法响应。
func newUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.path = r.URL.Path
		got.headers = r.Header.Clone()
		got.body = body
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func newServer(t *testing.T, upstream string, rules ...config.Rule) *Server {
	t.Helper()
	cfg := &config.Config{
		Upstream:     upstream,
		MaxBodyBytes: 1 << 20,
		Rules:        rules,
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	return New(config.Static(cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func post(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-client")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	return out
}

// 命中规则时，上游收到的必须是注入过的请求体，且两条管道都写了。
func TestPinsMatchingModelOnBothPipelines(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	srv := newServer(t, upstream.URL, config.Rule{
		Name: "ds", Model: "deepseek", Match: config.MatchContains,
		Pipeline: config.PipelineAuto, Mode: config.PinStrict, Upstreams: []string{"deepseek"},
	})

	rec := post(t, srv, "/v1/chat/completions", `{"model":"cline-pass/deepseek-v4.1-flash","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", got.path)
	}

	root := decodeJSON(t, got.body)
	gw, ok := root["providerOptions"].(map[string]any)
	if !ok {
		t.Fatalf("providerOptions missing: %s", got.body)
	}
	gateway := gw["gateway"].(map[string]any)
	only := gateway["only"].([]any)
	if len(only) != 1 || only[0] != "deepseek" {
		t.Errorf("providerOptions.gateway.only = %v", only)
	}
	p := root["provider"].(map[string]any)
	if only := p["only"].([]any); len(only) != 1 || only[0] != "deepseek" {
		t.Errorf("provider.only = %v", only)
	}

	// 观测头必须如实反映这次钉死。
	if got := rec.Header().Get("X-Cline-Pin-Rule"); got != "ds" {
		t.Errorf("X-Cline-Pin-Rule = %q", got)
	}
	if got := rec.Header().Get("X-Cline-Pin-Upstreams"); got != "deepseek" {
		t.Errorf("X-Cline-Pin-Upstreams = %q", got)
	}
	if got := rec.Header().Get("X-Cline-Pin-Note"); got != "" {
		t.Errorf("X-Cline-Pin-Note should be empty on success, got %q", got)
	}
}

// 未命中规则时请求体必须逐字节原样转发，由 Cline Pass 自主路由。
func TestUnmatchedModelIsPassedThroughUntouched(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	srv := newServer(t, upstream.URL, config.Rule{
		Name: "ds", Model: "deepseek", Match: config.MatchContains,
		Pipeline: config.PipelineAuto, Mode: config.PinStrict, Upstreams: []string{"deepseek"},
	})

	const body = `{"model":"cline-pass/kimi-k3","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, srv, "/v1/chat/completions", body)

	if string(got.body) != body {
		t.Errorf("body was modified:\n got %s\nwant %s", got.body, body)
	}
	if root := decodeJSON(t, got.body); root["provider"] != nil || root["providerOptions"] != nil {
		t.Errorf("no pin expected for unmatched model: %s", got.body)
	}
	if rec.Header().Get("X-Cline-Pin-Rule") != "none" {
		t.Errorf("X-Cline-Pin-Rule = %q, want none", rec.Header().Get("X-Cline-Pin-Rule"))
	}
	if rec.Header().Get("X-Cline-Pin-Note") == "" {
		t.Error("note should explain why nothing was pinned")
	}
}

// GLM 与 DeepSeek 是两条不同规则，必须各自命中。
func TestRoutesDeepseekAndGlmSeparately(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	srv := newServer(t, upstream.URL,
		config.Rule{Name: "ds", Model: "deepseek", Match: config.MatchContains,
			Pipeline: config.PipelineAuto, Mode: config.PinStrict, Upstreams: []string{"deepseek"}},
		config.Rule{Name: "glm", Model: "glm", Match: config.MatchContains,
			Pipeline: config.PipelineDirect, Mode: config.PinStrict, Upstreams: []string{"z-ai"}},
	)

	post(t, srv, "/v1/chat/completions", `{"model":"cline-pass/glm-5.3-flash"}`)
	root := decodeJSON(t, got.body)
	if _, ok := root["providerOptions"]; ok {
		t.Errorf("glm rule is direct-only, providerOptions should be absent: %s", got.body)
	}
	p, ok := root["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider missing: %s", got.body)
	}
	if only := p["only"].([]any); len(only) != 1 || only[0] != "z-ai" {
		t.Errorf("provider.only = %v, want [z-ai]", only)
	}
}

// 上游状态码与响应体必须保真，否则调用方的故障转移会失灵。
func TestUpstreamStatusAndErrorBodyArePreserved(t *testing.T) {
	upstream, _ := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	})
	srv := newServer(t, upstream.URL)

	rec := post(t, srv, "/v1/chat/completions", `{"model":"cline-pass/kimi-k3"}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate limited") {
		t.Errorf("error body not preserved: %s", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "17" {
		t.Errorf("Retry-After not forwarded: %q", rec.Header().Get("Retry-After"))
	}
}

// SSE 必须逐块透传，不能被缓冲到流结束。
//
// 用真实 socket 而不是 ResponseRecorder：Recorder 非并发安全，无法用来观察
// "上游还挂着时客户端是否已经收到第一块"。
func TestStreamingIsFlushedIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream, _ := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test upstream cannot flush")
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		flusher.Flush()
		<-release // 第二块保持挂起，用来证明第一块已经到达客户端
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	})

	srv := newServer(t, upstream.URL)
	front := httptest.NewServer(srv.Handler())
	defer front.Close()

	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		close(release)
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		close(release)
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		close(release)
		t.Errorf("Content-Type = %q", ct)
	}

	type chunk struct {
		text string
		err  error
	}
	first := make(chan chunk, 1)
	go func() {
		buf := make([]byte, 512)
		n, rerr := resp.Body.Read(buf)
		first <- chunk{string(buf[:n]), rerr}
	}()

	select {
	case got := <-first:
		close(release)
		if !strings.Contains(got.text, `"he"`) {
			t.Fatalf("first chunk = %q, want the content delta", got.text)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first SSE chunk did not arrive while the upstream was still open: response was buffered")
	}

	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("stream tail missing [DONE]: %q", rest)
	}
}

// 非 chat/completions 的端点必须纯净透传：调用方靠它们探测上游能力。
func TestProbePathsArePassedThroughVerbatim(t *testing.T) {
	upstream, got := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no route"}}`))
	})
	srv := newServer(t, upstream.URL, config.Rule{
		Name: "ds", Model: "deepseek", Match: config.MatchContains,
		Pipeline: config.PipelineAuto, Mode: config.PinStrict, Upstreams: []string{"deepseek"},
	})

	const body = `{"model":"deepseek-v4-flash","input":"hi"}`
	rec := post(t, srv, "/v1/responses", body)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 passthrough", rec.Code)
	}
	if string(got.body) != body {
		t.Errorf("/v1/responses body must be untouched:\n got %s", got.body)
	}
	if strings.Contains(string(got.body), "provider") {
		t.Errorf("responses path must never be pinned: %s", got.body)
	}
}

func TestModelsEndpointPassesThrough(t *testing.T) {
	upstream, got := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"cline-pass/kimi-k3"}]}`))
	})
	srv := newServer(t, upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kimi-k3") {
		t.Errorf("models passthrough failed: %d %s", rec.Code, rec.Body.String())
	}
	if got.path != "/v1/models" {
		t.Errorf("upstream path = %q", got.path)
	}
}

// 白名单外的路径要直接拒绝，不能让代理变成访问上游任意路径的跳板。
func TestPathOutsideAllowlistIsRejected(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	srv := newServer(t, upstream.URL)

	for _, path := range []string{"/admin/config", "/internal/debug", "/v2/chat/completions"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
	if got.path != "" {
		t.Errorf("rejected path still reached upstream: %q", got.path)
	}
}

// 路径穿越由 ServeMux 先行清理，无论最终是重定向还是拒绝，都不得打到上游。
func TestPathTraversalNeverReachesUpstream(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	srv := newServer(t, upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/../../etc/passwd", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("traversal returned 200: %s", rec.Body.String())
	}
	if got.path != "" {
		t.Errorf("traversal reached upstream: %q", got.path)
	}
}

func TestHealthEndpoint(t *testing.T) {
	upstream, _ := newUpstream(t, nil)
	srv := newServer(t, upstream.URL)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz = %d %s", rec.Code, rec.Body.String())
	}
}

// 上游基址已含版本段时，不得拼出 /v1/v1/...。
//
// 这是端到端测试才暴露的真实故障：调用方（如 sub2api）把 base_url 指向
// http://proxy:8787/v1，请求路径本身就是 /v1/chat/completions，
// 直接相加会得到 https://api.cline.bot/api/v1/v1/chat/completions → 全部 404。
func TestPathJoiningAvoidsDuplicateVersionSegment(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		path     string
		wantPath string
	}{
		{"base 带 /v1 且路径也带 /v1", "https://up.example/api/v1", "/v1/chat/completions", "/api/v1/chat/completions"},
		{"base 带 /v1 而路径不带", "https://up.example/api/v1", "/chat/completions", "/api/v1/chat/completions"},
		{"base 带 /v1 且路径是 models", "https://up.example/api/v1", "/v1/models", "/api/v1/models"},
		{"base 无版本段则原样拼", "https://up.example", "/v1/chat/completions", "/v1/chat/completions"},
		{"base 版本段与路径不同则不剥离", "https://up.example/v4", "/v1/chat/completions", "/v4/v1/chat/completions"},
		{"尾斜杠被规整", "https://up.example/api/v1/", "/v1/models", "/api/v1/models"},
		{"非版本段不剥离", "https://up.example/gateway", "/v1/models", "/gateway/v1/models"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinUpstream(tc.base, tc.path); got != "https://up.example"+tc.wantPath {
				t.Errorf("joinUpstream(%q, %q) = %q, want %q",
					tc.base, tc.path, got, "https://up.example"+tc.wantPath)
			}
		})
	}
}

// 端到端确认：base 带版本段时，假上游收到的路径不含重复段。
func TestRequestReachesVersionedUpstreamBase(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	cfg := &config.Config{
		Upstream:     upstream.URL + "/api/v1",
		MaxBodyBytes: 1 << 20,
		Rules: []config.Rule{{
			Name: "ds", Model: "deepseek", Match: config.MatchContains,
			Pipeline: config.PipelineAuto, Mode: config.PinStrict, Upstreams: []string{"deepseek"},
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(config.Static(cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := post(t, srv, "/v1/chat/completions", `{"model":"cline-pass/deepseek-v4.1-flash"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got.path != "/api/v1/chat/completions" {
		t.Errorf("upstream saw %q, want /api/v1/chat/completions（不得出现重复版本段）", got.path)
	}
}

// 配置了固定 key 时必须覆盖客户端凭据，而不是叠加。
func TestFixedAPIKeyOverridesClientAuthorization(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	cfg := &config.Config{
		Upstream:     upstream.URL,
		APIKey:       "sk-fixed",
		MaxBodyBytes: 1 << 20,
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(config.Static(cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))

	post(t, srv, "/v1/chat/completions", `{"model":"m"}`)

	if auth := got.headers.Get("Authorization"); auth != "Bearer sk-fixed" {
		t.Errorf("Authorization = %q, want the configured key", auth)
	}
}

// 未配置固定 key 时沿用客户端凭据。
func TestClientAuthorizationIsForwardedWhenNoFixedKey(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	srv := newServer(t, upstream.URL)

	post(t, srv, "/v1/chat/completions", `{"model":"m"}`)

	if auth := got.headers.Get("Authorization"); auth != "Bearer sk-client" {
		t.Errorf("Authorization = %q, want the client's", auth)
	}
}

// 白名单外的请求头不得外泄到上游；白名单内的要带上。
func TestHeaderForwardingIsWhitelisted(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	cfg := &config.Config{
		Upstream:       upstream.URL,
		MaxBodyBytes:   1 << 20,
		ForwardHeaders: []string{"x-client-type"},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(config.Static(cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Type", "cline-cli")
	req.Header.Set("X-Secret-Internal", "must-not-leak")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if got.headers.Get("X-Client-Type") != "cline-cli" {
		t.Errorf("whitelisted header was dropped")
	}
	if got.headers.Get("X-Secret-Internal") != "" {
		t.Errorf("non-whitelisted header leaked upstream: %q", got.headers.Get("X-Secret-Internal"))
	}
}

// 上游路由元数据头要能回传给调用方，用于核对最终命中的上游。
func TestUpstreamRoutingHeadersAreRelayed(t *testing.T) {
	upstream, _ := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cline-Actual-Upstream", "deepseek")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	srv := newServer(t, upstream.URL)

	rec := post(t, srv, "/v1/chat/completions", `{"model":"m"}`)

	if got := rec.Header().Get("X-Cline-Actual-Upstream"); got != "deepseek" {
		t.Errorf("routing metadata header not relayed: %q", got)
	}
}

// 超过体积上限要明确拒绝，而不是把内存读满。
func TestBodyOverLimitIsRejected(t *testing.T) {
	upstream, _ := newUpstream(t, nil)
	cfg := &config.Config{Upstream: upstream.URL, MaxBodyBytes: 64}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(config.Static(cfg), slog.New(slog.NewTextHandler(io.Discard, nil)))

	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 500) + `"}]}`
	rec := post(t, srv, "/v1/chat/completions", big)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// 上游不可达时要回 502 而不是挂死或 panic。
func TestUpstreamUnreachableYieldsBadGateway(t *testing.T) {
	srv := newServer(t, "http://127.0.0.1:1")

	rec := post(t, srv, "/v1/chat/completions", `{"model":"m"}`)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "proxy_error") {
		t.Errorf("error shape = %s", rec.Body.String())
	}
}
