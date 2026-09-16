package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
)

// ---------------------------------------------------------------------------
// Cline 的 {data:{...},success:true} 包封还原。
//
// 背景（2026-09-16 实测）：Cline API 会把整个补全包在 data 里，而几乎全部
// OpenAI 客户端只读顶层 choices，于是表现为"请求成功但没有内容"。
// 只有**非流式**响应会这样，流式 SSE 的事件是标准的。
// ---------------------------------------------------------------------------

// Cline 真实的包封形状（取自真实响应，已截去 provider_metadata 细节）。
const clineEnvelope = `{"data":{"choices":[{"finish_reason":"stop","index":0,` +
	`"logprobs":null,"message":{"content":"ok","role":"assistant"}}],` +
	`"created":1789540261,"generationId":"gen_01M2","id":"gen_01M2",` +
	`"model":"vmc/k3-contributor-fallbacks","object":"chat.completion",` +
	`"system_fingerprint":"fp_x","usage":{"completion_tokens":16,"prompt_tokens":103,` +
	`"total_tokens":119}},"success":true}`

func TestUnwrapDataEnvelope(t *testing.T) {
	out, ok := UnwrapDataEnvelope([]byte(clineEnvelope))
	if !ok {
		t.Fatal("真实包封必须被识别并还原")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("还原结果不是 JSON: %v", err)
	}
	if _, still := doc["data"]; still {
		t.Error("还原后不应再有 data 键")
	}
	if _, still := doc["success"]; still {
		t.Error("还原后不应再有 success 键")
	}
	choices, ok := doc["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %#v", doc["choices"])
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "ok" {
		t.Errorf("content = %v", msg["content"])
	}
	if doc["usage"] == nil {
		t.Error("usage 必须保留（计费依赖它）")
	}
}

// 只有精确的那一个形状才还原，其余一律原样返回。
func TestUnwrapDataEnvelopeLeavesOtherShapesAlone(t *testing.T) {
	cases := map[string]string{
		"标准 OpenAI 响应":         `{"id":"x","object":"chat.completion","choices":[{"message":{"content":"hi"}}],"usage":{}}`,
		"顶层就是 choices":         `{"choices":[{"message":{"content":"hi"}}]}`,
		"/v1/models 的 data 数组": `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`,
		"data 不是对象":            `{"data":"nope","success":true}`,
		"data 里没有 choices":     `{"data":{"id":"x"},"success":true}`,
		"data.choices 是空数组":    `{"data":{"choices":[]},"success":true}`,
		"data.choices 不是数组":    `{"data":{"choices":"x"},"success":true}`,
		"纯数组":                  `[1,2,3]`,
		"空对象":                  `{}`,
		"非 JSON":               `not json at all`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out, ok := UnwrapDataEnvelope([]byte(body))
			if ok {
				t.Errorf("不该还原 %s，却返回了 %s", name, out)
			}
		})
	}
}

// 端到端：非流式响应必须被还原成标准形状。
func TestNonStreamingResponseIsUnwrapped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, clineEnvelope)
	}))
	defer upstream.Close()

	srv := newServer(t, upstream.URL)
	proxySrv := httptest.NewServer(srv.Handler())
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"kimi-k3","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if got := resp.Header.Get("X-Cline-Pin-Unwrapped"); got != "data-envelope" {
		t.Errorf("X-Cline-Pin-Unwrapped = %q", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, raw)
	}
	if _, ok := doc["choices"]; !ok {
		t.Errorf("顶层必须有 choices，实际键: %v\n%s", keysOf(doc), raw)
	}
	// Content-Length 必须与实际正文一致，否则下游会截断或挂起。
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if cl != itoa(len(raw)) {
			t.Errorf("Content-Length = %s, 实际 %d 字节", cl, len(raw))
		}
	}
}

// 流式响应**一个字都不能缓冲**，也不能被还原逻辑碰到。
func TestStreamingResponseIsNotBuffered(t *testing.T) {
	// SSE 正文故意做成"看起来像包封"的样子：即便内容像，也必须原样转发。
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
		"data: [DONE]\n\n"

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse[:len(sse)/2])
		w.(http.Flusher).Flush()
		<-release // 挂住，验证下游已经拿到前半段
		_, _ = io.WriteString(w, sse[len(sse)/2:])
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	defer close(release)

	srv := newServer(t, upstream.URL)
	proxySrv := httptest.NewServer(srv.Handler())
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"kimi-k3","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，SSE 必须原样保留", ct)
	}
	if got := resp.Header.Get("X-Cline-Pin-Unwrapped"); got != "" {
		t.Errorf("流式响应不该被还原，却设置了 X-Cline-Pin-Unwrapped=%q", got)
	}

	// 上游还挂着，下游就必须已经能读到第一批事件。
	buf := make([]byte, len(sse)/2)
	n, err := io.ReadFull(resp.Body, buf)
	if err != nil || n == 0 {
		t.Fatalf("上游仍挂起时下游就该收到数据（n=%d err=%v）——说明被缓冲了", n, err)
	}
	if !strings.HasPrefix(string(buf), "data: {") {
		t.Errorf("首批数据不像 SSE: %q", buf)
	}
}

// 超大响应不还原，但要原样完整转发（不能丢已读走的前缀）。
func TestOversizedResponseIsRelayedIntact(t *testing.T) {
	// 构造一个超过 maxEnvelopeBytes 的合法包封。
	big := strings.Repeat("x", maxEnvelopeBytes+1024)
	body := `{"data":{"choices":[{"message":{"content":"` + big + `"}}]},"success":true}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	srv := newServer(t, upstream.URL)
	proxySrv := httptest.NewServer(srv.Handler())
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"kimi-k3","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if len(raw) != len(body) {
		t.Errorf("超大响应被截断：收到 %d 字节，原样应为 %d", len(raw), len(body))
	}
	if string(raw) != body {
		t.Error("超大响应的正文被改动了")
	}
	// 不还原，但必须**明确告诉调用方**为什么没还原——静默跳过会让人以为
	// 代理坏了，或者以为响应本来就是标准形状。
	if got := resp.Header.Get("X-Cline-Pin-Unwrapped"); got != "skipped-too-large" {
		t.Errorf("X-Cline-Pin-Unwrapped = %q, want skipped-too-large", got)
	}
}

// 关闭开关后必须完全不碰正文（给需要严格原样透传的部署留出口）。
func TestUnwrapCanBeDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, clineEnvelope)
	}))
	defer upstream.Close()

	cfg := &config.Config{Upstream: upstream.URL, MaxBodyBytes: 1 << 20, UnwrapDataEnvelope: false}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	srv := New(config.Static(cfg), discardLogger())
	proxySrv := httptest.NewServer(srv.Handler())
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if string(raw) != clineEnvelope {
		t.Error("关闭开关后响应体必须逐字节原样返回")
	}
	if got := resp.Header.Get("X-Cline-Pin-Unwrapped"); got != "" {
		t.Errorf("关闭开关后不应出现 X-Cline-Pin-Unwrapped=%q", got)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
