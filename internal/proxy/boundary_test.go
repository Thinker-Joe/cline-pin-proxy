package proxy

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// 边界回归测试。这些场景跨多个步骤，靠提高行覆盖率发现不了，
// 都是对真实缺陷定向补齐的（来源：docs/CODE_REVIEW.md 的独立审查）。
// ---------------------------------------------------------------------------

// 客户端可用百分号编码绕过 /v1/ 前缀检查。
//
// Go 1.22+ 的 ServeMux 用 EscapedPath() 做匹配与 cleanPath：字面量 "%2e%2e"
// 不是 path.Clean 眼里的 ".."，所以不会被规范化掉、也不会触发 301；
// 而处理器里读到的 r.URL.Path 是**已解码**的 "/v1/../../admin/private"。
// 拿它去拼上游 URL，上游一规范化就跳出了 API 前缀。
func TestEncodedPathTraversalIsRejected(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	srv := newServer(t, upstream.URL)

	for _, target := range []string{
		"/v1/%2e%2e/%2e%2e/admin/private",
		"/v1/%2e%2e/admin",
		"/api/v1/%2e%2e/%2e%2e/admin",
		"/v1/..%2f..%2fadmin",
		"/v1/a/%2e%2e/%2e%2e/%2e%2e/admin",
	} {
		t.Run(target, func(t *testing.T) {
			got.path = ""
			req := httptest.NewRequest(http.MethodGet, target, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if got.path != "" {
				t.Errorf("上游被访问到 %q（应当拒绝该路径）", got.path)
			}
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}

// 一次请求必须只用一份配置快照。
//
// 逐个字段调 Current() 会在热重载正好插进中间时，把请求发往旧上游、
// 却带上新配置的密钥。原子指针只保证单次读取安全，不保证整条链一致。
func TestRequestUsesSingleConfigSnapshot(t *testing.T) {
	upstream, got := newUpstream(t, nil)

	oldCfg := &config.Config{
		Upstream: upstream.URL, MaxBodyBytes: 1 << 20, APIKey: "old-key",
		Rules: []config.Rule{{Name: "r", Model: "deepseek", Match: config.MatchContains,
			Pipeline: config.PipelineAuto, Mode: config.PinStrict, Upstreams: []string{"deepseek"}}},
	}
	newCfg := *oldCfg
	newCfg.APIKey = "new-key"
	if err := oldCfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := newCfg.Normalize(); err != nil {
		t.Fatal(err)
	}

	src := &countingSource{cfgs: []*config.Config{oldCfg, &newCfg}}
	srv := New(src, discardLogger())

	body := `{"model":"cline-pass/deepseek-v4.1-flash","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := src.Calls(); n != 1 {
		t.Errorf("一次请求读了 %d 次配置，必须是 1（否则可能新旧混用）", n)
	}
	if auth := got.headers.Get("Authorization"); auth != "Bearer old-key" {
		t.Errorf("Authorization = %q, want Bearer old-key", auth)
	}
}

// 透传路径同样只读一次配置。
func TestPassthroughUsesSingleConfigSnapshot(t *testing.T) {
	upstream, _ := newUpstream(t, nil)
	cfg := &config.Config{Upstream: upstream.URL, MaxBodyBytes: 1 << 20}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	src := &countingSource{cfgs: []*config.Config{cfg}}
	srv := New(src, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	if n := src.Calls(); n != 1 {
		t.Errorf("透传请求读了 %d 次配置，必须是 1", n)
	}
}

// 上游中途断开不能伪装成正常结束。
//
// 上游声明 Content-Length: 100 却只发 15 字节就断；因为响应头已经写出去了，
// 不主动中止连接的话，下游读到的是一段被"正常结束"的残缺 SSE，HTTP 层
// 丢失了失败信号，调用方无法据此重试。
func TestTruncatedUpstreamIsNotReportedAsSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: partial\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler) // 上游异常断开
	}))
	defer upstream.Close()

	srv := newServer(t, upstream.URL)
	proxySrv := httptest.NewServer(srv.Handler())
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/v1/models")
	if err != nil {
		return // 连响应头都没拿到也算正确暴露了失败
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("上游截断必须以错误暴露给客户端，实际拿到 %d 字节且 err==nil: %q",
			len(body), body)
	}
}

// README 与 chatCompletionsPaths 都声明支持 /api/v1/... 别名。
//
// 上游基址本身以 /api/v1 结尾，客户端再带 /api/v1 时若只剥离 "/v1"，
// 会拼成 /api/v1/api/v1/chat/completions，上游一律 404。
func TestAPIV1AliasIsNotDuplicated(t *testing.T) {
	for _, tc := range []struct{ base, path, want string }{
		{"https://h/api/v1", "/v1/chat/completions", "https://h/api/v1/chat/completions"},
		{"https://h/api/v1", "/api/v1/chat/completions", "https://h/api/v1/chat/completions"},
		{"https://h/api/v1", "/chat/completions", "https://h/api/v1/chat/completions"},
		{"https://h/api/v1", "/v1/models", "https://h/api/v1/models"},
		{"https://h/api/v1", "/api/v1/models", "https://h/api/v1/models"},
		{"https://h/api/v1", "/v1", "https://h/api/v1/"},
		{"https://h", "/v1/chat/completions", "https://h/v1/chat/completions"},
		{"https://h", "/api/v1/chat/completions", "https://h/api/v1/chat/completions"},
		{"https://h/v1", "/v1/chat/completions", "https://h/v1/chat/completions"},
		{"https://h/v1", "/api/v1/chat/completions", "https://h/v1/chat/completions"},
	} {
		if got := joinUpstream(tc.base, tc.path); got != tc.want {
			t.Errorf("joinUpstream(%q, %q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

// 代理不能替调用方跟随重定向：那会改掉方法（POST→GET）、丢掉原始状态码
// 与响应体，违反"上游状态码与响应体原样回传"的约定。
func TestRedirectIsPassedThroughNotFollowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"followed":true}`)
			return
		}
		w.Header().Set("Location", "/redirected")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, `{"original":true}`)
	}))
	defer upstream.Close()

	srv := newServer(t, upstream.URL)
	proxySrv := httptest.NewServer(srv.Handler())
	defer proxySrv.Close()

	// 必须禁用自动重定向，否则 http.Client 会把 302 跟掉。
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := noRedirect.Post(proxySrv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302（状态码必须原样回传）", resp.StatusCode)
	}
	if !strings.Contains(string(body), "original") {
		t.Errorf("body = %q, want 原始响应体", body)
	}
	if loc := resp.Header.Get("Location"); loc != "/redirected" {
		t.Errorf("Location = %q, 必须回传，否则下游拿到不可用的重定向响应", loc)
	}
}

// max_body_bytes 的文档约定是进程级的，透传端点也必须执行。
func TestPassthroughHonoursBodyLimit(t *testing.T) {
	upstream, got := newUpstream(t, nil)
	cfg := &config.Config{Upstream: upstream.URL, MaxBodyBytes: 64}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(config.Static(cfg), discardLogger())

	payload := strings.Repeat("x", 1024)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413（透传路径也必须执行体积上限）", rec.Code)
	}
	if len(got.body) > 64 {
		t.Errorf("上游收到 %d 字节，超出上限 64", len(got.body))
	}
}

// 显式透传 accept-encoding 时，上游会回压缩响应。
//
// 但 Go Transport 只有在**自己**加协商头时才会透明解压；调用方显式带了
// accept-encoding，它就原样保留压缩响应。此时若代理再丢掉 Content-Encoding，
// 下游会把 gzip 字节当成 JSON/SSE 解析。
//
// 断言写成"要么保留编码声明，要么给出可解析明文"，不依赖具体压缩字节
// （deflate 对小输入可能用 stored 块，明文会字面出现，用子串判断会假阴性）。
func TestForwardedCompressionIsDecodable(t *testing.T) {
	const plain = `{"choices":[{"message":{"content":"ok"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, plain)
		_ = gz.Close()
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Upstream: upstream.URL, MaxBodyBytes: 1 << 20,
		ForwardHeaders: []string{"accept-encoding"},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(config.Static(cfg), discardLogger())
	proxySrv := httptest.NewServer(srv.Handler())
	defer proxySrv.Close()

	// 客户端自己禁用自动解压，才能看清代理到底转发了什么。
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/v1/models", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.Header.Get("Content-Encoding") == "gzip" {
		if _, err := io.ReadAll(mustGunzip(t, raw)); err != nil {
			t.Fatalf("声明的 gzip 正文解不开: %v", err)
		}
		return
	}

	// 没有编码声明 -> 正文必须已经是明文。
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Errorf("既没保留 Content-Encoding 也没解压，正文不是合法 JSON（前 8 字节 %x）", raw[:min(8, len(raw))])
	}
}

func mustGunzip(t *testing.T, raw []byte) io.Reader {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	return zr
}

// countingSource 记录 Current() 被调用了多少次。
type countingSource struct {
	mu    sync.Mutex
	calls int
	cfgs  []*config.Config
}

func (s *countingSource) Current() *config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i >= len(s.cfgs) {
		i = len(s.cfgs) - 1
	}
	return s.cfgs[i]
}

func (s *countingSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
