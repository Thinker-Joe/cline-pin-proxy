// Package proxy 实现带上游钉死的透传代理。
//
// 行为约定：
//
//   - 只对 POST 的 chat/completions 端点做上游注入；
//   - 其余端点（/v1/models、/v1/responses 等）纯净透传，不替上游做决定——
//     这很重要，因为调用方常用这些端点探测上游能力，代理不该干扰结论；
//   - 上游状态码与响应体原样回传，调用方的故障转移逻辑才不会失灵；
//   - 流式响应逐块 Flush，全程 O(1) 内存，不做任何 JSON 解析。
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
	"github.com/Thinker-Joe/cline-pin-proxy/internal/pin"
)

// chatCompletionsPaths 是唯一会做上游注入的端点集合。
var chatCompletionsPaths = map[string]bool{
	"/v1/chat/completions":     true,
	"/chat/completions":        true,
	"/api/v1/chat/completions": true,
}

// allowedPassthroughPrefixes 限定可透传的路径前缀，
// 避免代理被当成访问上游任意路径的跳板。
var allowedPassthroughPrefixes = []string{"/v1/", "/api/v1/"}

// baseForwardHeaders 是始终尝试透传给上游的客户端请求头（小写）。
//
// 不含 accept-encoding：交给 Go 的 transport 自行协商与解压，避免把压缩流
// 原样转发出去时与 Content-Length 语义打架。
var baseForwardHeaders = []string{"content-type", "accept", "authorization", "user-agent"}

// hopByHopHeaders 是逐跳头，按 RFC 7230 不得跨代理转发。
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"proxy-connection":    true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// Server 是透传代理。
type Server struct {
	cfg    *config.Config
	client *http.Client
	log    *slog.Logger
}

// New 构造代理。
//
// 客户端刻意不设 http.Client.Timeout：流式生成可能持续数分钟，整体超时会把
// 长回答硬砍断。超时控制改由 dial / TLS / 响应头三段分别设置。
func New(cfg *config.Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Server{
		cfg:    cfg,
		client: &http.Client{Transport: transport},
		log:    log,
	}
}

// Handler 返回代理的 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handle)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method == http.MethodPost && chatCompletionsPaths[r.URL.Path] {
		s.handleChatCompletions(w, r)
		return
	}

	if !isPassthroughPath(r.URL.Path) {
		s.writeError(w, http.StatusNotFound, "no route: "+r.Method+" "+r.URL.Path)
		return
	}
	s.forward(w, r, nil, nil)
}

func isPassthroughPath(path string) bool {
	for _, prefix := range allowedPassthroughPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}

	model, err := pin.ModelOf(body)
	if err != nil {
		s.log.Warn("model field unreadable, passing through unpinned", "error", err)
		s.forward(w, r, body, pinHeaders("none", "", "", "model field unreadable"))
		return
	}

	rule, matched := s.cfg.Match(model)
	if !matched {
		// 无规则命中时完全原样放行，由 Cline Pass 自主路由。
		s.forward(w, r, body, pinHeaders("none", "", "", "no rule matched"))
		return
	}

	injected, err := pin.Inject(body, pin.Options{
		Upstreams: rule.Upstreams,
		Mode:      rule.Mode,
		Pipeline:  rule.Pipeline,
		Sort:      rule.Sort,
	})
	if err != nil {
		// 注入失败时降级为未钉死透传，并在响应头里明确标注，避免"以为钉住了"。
		s.log.Warn("injection failed, passing through unpinned",
			"model", model, "rule", rule.Name, "error", err)
		s.forward(w, r, body, pinHeaders("none", "", "", "injection failed"))
		return
	}

	joined := strings.Join(rule.Upstreams, ">")
	s.log.Info("pinned request",
		"model", model,
		"rule", rule.Name,
		"upstreams", joined,
		"mode", string(rule.Mode),
		"pipeline", string(rule.Pipeline),
	)

	s.forward(w, r, injected, pinHeaders(rule.Name, joined, string(rule.Mode), ""))
}

// pinHeaders 组装用于观测的响应头。note 非空表示这次没有真正钉死。
func pinHeaders(rule, upstreams, mode, note string) map[string]string {
	h := map[string]string{"X-Cline-Pin-Rule": rule}
	if upstreams != "" {
		h["X-Cline-Pin-Upstreams"] = upstreams
	}
	if mode != "" {
		h["X-Cline-Pin-Mode"] = mode
	}
	if note != "" {
		h["X-Cline-Pin-Note"] = note
	}
	return h
}

// joinUpstream 把客户端路径拼到上游基址上，并消掉重复的版本段。
//
// 调用方的 base_url 有两种常见写法，而请求路径都会是 /v1/...：
//
//	http://proxy:8787     客户端自己拼 -> /v1/chat/completions
//	http://proxy:8787/v1  客户端拼端点 -> /v1/chat/completions（同样带 /v1）
//
// 而上游基址 https://api.cline.bot/api/v1 末尾**已经**含有版本段。
// 若直接相加会得到 /api/v1/v1/chat/completions，上游一律回 404——
// 这是本项目早期版本的真实故障，单测用假上游接任意路径，所以漏掉了。
func joinUpstream(base, path string) string {
	base = strings.TrimRight(base, "/")
	if seg := versionSegment(base); seg != "" && strings.HasPrefix(path, "/"+seg+"/") {
		path = strings.TrimPrefix(path, "/"+seg)
	}
	return base + path
}

// versionSegment 返回 base 末尾形如 v1 / v2 / v1beta 的版本段，不是则返回空。
func versionSegment(base string) string {
	idx := strings.LastIndexByte(base, '/')
	if idx < 0 {
		return ""
	}
	seg := base[idx+1:]
	if len(seg) < 2 || seg[0] != 'v' {
		return ""
	}
	for _, r := range seg[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return ""
		}
	}
	return seg
}

// readBody 读取请求体；失败时已经写好响应，返回 ok=false。
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	limited := http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds configured limit")
			return nil, false
		}
		s.writeError(w, http.StatusBadRequest, "cannot read request body")
		return nil, false
	}
	return body, true
}

// forward 把请求转发到上游并把响应原样回传。
// body 为 nil 时直接流式转发原始请求体（用于纯净透传路径）。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, body []byte, extraHeaders map[string]string) {
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}

	target := joinUpstream(s.cfg.Upstream, r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	var reader io.Reader = r.Body
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, reader)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "cannot build upstream request")
		return
	}
	s.copyRequestHeaders(r, req)

	resp, err := s.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			// 客户端主动断开：请求上下文已取消，上游请求也随之终止。不是错误。
			s.log.Debug("client canceled request", "path", r.URL.Path)
			return
		}
		s.log.Error("upstream request failed", "target", target, "error", err)
		s.writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if err := flushCopy(w, resp.Body); err != nil && r.Context().Err() == nil {
		s.log.Debug("response stream ended early", "error", err)
	}
}

// copyRequestHeaders 只透传白名单内的请求头。
func (s *Server) copyRequestHeaders(from *http.Request, to *http.Request) {
	allow := make(map[string]bool, len(baseForwardHeaders)+len(s.cfg.ForwardHeaders))
	for _, h := range baseForwardHeaders {
		allow[h] = true
	}
	for _, h := range s.cfg.ForwardHeaders {
		allow[strings.ToLower(strings.TrimSpace(h))] = true
	}

	for name, values := range from.Header {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || !allow[lower] {
			continue
		}
		for _, v := range values {
			to.Header.Add(name, v)
		}
	}

	if to.Header.Get("Content-Type") == "" && from.ContentLength != 0 {
		to.Header.Set("Content-Type", "application/json")
	}

	// 配置了固定 key 时覆盖客户端凭据；否则沿用透传过来的 Authorization。
	if key := strings.TrimSpace(s.cfg.APIKey); key != "" {
		to.Header.Set("Authorization", "Bearer "+key)
	}
}

// copyResponseHeaders 透传响应头。
//
// 采用白名单而非黑名单：既避开逐跳头，也刻意不转发 content-length——
// 注入会改变请求体长度，响应长度也可能因解压而变，交给 Go 自行计算更安全。
// x-* 全量放行，这样调用方能读到上游的路由元数据头。
func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] {
			continue
		}
		if lower != "content-type" && lower != "cache-control" && lower != "retry-after" && !strings.HasPrefix(lower, "x-") {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// flushCopy 边读边写并逐块 Flush。
//
// 这是流式正确性的关键：任何"先读完整响应再返回"的写法都会让首字延迟退化成
// 整段生成时间。这里全程 O(1) 内存、不做 JSON 解析、不缓冲。
func flushCopy(w http.ResponseWriter, src io.Reader) error {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			// 个别 ResponseWriter 不支持即时 Flush（如部分中间件包装），
			// 此时退化为普通缓冲，而不是让请求失败。
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "proxy_error"},
	})
	if err != nil {
		payload = []byte(`{"error":{"message":"proxy error","type":"proxy_error"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
