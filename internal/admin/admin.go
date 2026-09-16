// Package admin 提供运行期的配置读取、规则替换与上游探测接口。
//
// 存在的理由：Docker 部署下改配置原本有两个硬摩擦——config.json 是挂载文件，
// compose 察觉不到内容变化因而不会重建容器；而 distroless 镜像里没有 shell，
// 探测上游只能靠 `docker run --rm --entrypoint ...` 绕一圈。
//
// 本包把这两件事都变成 HTTP 调用，可脚本化、可远程执行。
package admin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
	"github.com/Thinker-Joe/cline-pin-proxy/internal/probe"
)

// maxAdminBodyBytes 限制管理接口的请求体，防止误传大文件把内存打满。
const maxAdminBodyBytes = 1 << 20 // 1 MiB

// RuleStore 是管理接口需要的配置能力。
type RuleStore interface {
	Current() *config.Config
	SetRules(rules []config.Rule) (persisted bool, persistErr error, err error)
	Reload() (bool, error)
}

// Prober 执行一次上游探测。由调用方注入，便于测试与解耦。
type Prober interface {
	Probe(ctx context.Context, model, pipeline string) (probe.Result, error)
}

// Handler 是 /admin/* 的处理器。
type Handler struct {
	store  RuleStore
	prober Prober
	log    *slog.Logger
}

// NewHandler 构造管理接口处理器。
func NewHandler(store RuleStore, prober Prober, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: store, prober: prober, log: log}
}

// Register 把管理端点挂到 mux 上。
//
// 端点始终注册，由 guard 在每个请求上按当前配置决定放行、鉴权还是返回 404。
// 这样 token 改动可以随配置热重载立即生效，无需重启。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/config", h.guard(h.handleConfig))
	mux.HandleFunc("/admin/rules", h.guard(h.handleRules))
	mux.HandleFunc("/admin/probe", h.guard(h.handleProbe))
	mux.HandleFunc("/admin/reload", h.guard(h.handleReload))
}

// guard 实现管理接口的启用开关与令牌鉴权。
//
// 未配置令牌且未显式允许匿名时返回 404 而非 401：不向未授权者暴露
// 「此处存在管理接口」这一事实。
func (h *Handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.store.Current()
		token := strings.TrimSpace(cfg.AdminToken)

		if token == "" && !cfg.AdminAllowUnauthenticated {
			notFound(w)
			return
		}
		if token != "" && !tokenMatches(r, token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cline-pin-proxy"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]string{
					"message": "invalid or missing admin token",
					"type":    "auth_error",
				},
			})
			return
		}
		next(w, r)
	}
}

// tokenMatches 从 Authorization 或 X-Admin-Token 取令牌并做常量时间比较。
func tokenMatches(r *http.Request, want string) bool {
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	got = strings.TrimSpace(got)
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-Admin-Token"))
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleConfig 返回当前生效配置。api_key 与 admin_token 一律不下发。
func (h *Handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	cfg := h.store.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"listen":               cfg.Listen,
		"upstream":             cfg.Upstream,
		"forward_headers":      cfg.ForwardHeaders,
		"probe_headers":        cfg.ProbeHeaders,
		"max_body_bytes":       cfg.MaxBodyBytes,
		"watch_seconds":        cfg.WatchSeconds,
		"unwrap_data_envelope": cfg.UnwrapDataEnvelope,
		"admin_auth_required":  strings.TrimSpace(cfg.AdminToken) != "",
		"rules":                cfg.Rules,
	})
}

// handleRules 读取或整体替换规则表。
//
// PUT 会立即生效；若配置文件可写则同时写回，否则返回 persist_error 与提示。
func (h *Handler) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"rules": h.store.Current().Rules})

	case http.MethodPut:
		rules, err := decodeRules(r)
		if err != nil {
			badRequest(w, err.Error())
			return
		}

		persisted, persistErr, err := h.store.SetRules(rules)
		if err != nil {
			// 校验失败时不改动任何状态，调用方可以修正后重试。
			badRequest(w, err.Error())
			return
		}

		h.log.Info("rules updated via admin api",
			"count", len(rules), "persisted", persisted)

		resp := map[string]any{
			"ok":        true,
			"applied":   true,
			"persisted": persisted,
			"rules":     h.store.Current().Rules,
		}
		if persistErr != nil {
			resp["persist_error"] = persistErr.Error()
			resp["hint"] = "规则已生效但未写回文件；若配置以只读方式挂载，请改为可写，" +
				"或直接编辑配置文件（会自动热重载）"
		}
		writeJSON(w, http.StatusOK, resp)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// handleProbe 探测某个模型背后可用的上游渠道。
//
// 让容器部署无需 shell 就能探测——此前只能靠
// `docker run --rm --entrypoint /usr/local/bin/cline-pin-proxy`。
func (h *Handler) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var body struct {
		Model    string `json:"model"`
		Pipeline string `json:"pipeline"`
	}
	if err := decodeJSON(r, &body); err != nil {
		badRequest(w, err.Error())
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		badRequest(w, "model is required")
		return
	}

	res, err := h.prober.Probe(r.Context(), model, body.Pipeline)
	if err != nil {
		h.log.Warn("admin probe failed", "model", model, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "upstream_error"},
		})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleReload 强制从文件重载配置，忽略修改时间。
func (h *Handler) handleReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	changed, err := h.store.Reload()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "config_error"},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"changed": changed,
		"rules":   h.store.Current().Rules,
	})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// decodeRules 解析 PUT /admin/rules 的请求体，接受两种写法：
//
//	[ {...}, {...} ]              // 裸数组——这个端点名下最自然的写法
//	{ "rules": [ {...} ] }        // 包装形式，与 GET 的响应结构对称
//
// 两种都收：只认其中一种的话，调用方要先吃一个 400 才知道该用哪种，
// 而实测中这确实发生了（README 写裸数组、实现只认包装形式）。
//
// 但 null 一律拒绝：裸 `null` 会解码成 nil slice 并被当成"清空规则表"接受，
// 一个把未初始化变量直接序列化的脚本就能关掉全部钉死规则。清空只能显式写 []。
func decodeRules(r *http.Request) ([]config.Rule, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty request body")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, errors.New(`invalid JSON body: rules must be an array (use [] to clear all rules)`)
	}

	if trimmed[0] == '{' {
		var wrapper struct {
			Rules []json.RawMessage `json:"rules"`
		}
		if err := unmarshalStrict(trimmed, &wrapper); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		if wrapper.Rules == nil {
			return nil, errors.New(`invalid JSON body: object form requires a "rules" array (use [] to clear all rules)`)
		}
		return decodeRuleElements(wrapper.Rules)
	}

	var elements []json.RawMessage
	if err := unmarshalStrict(trimmed, &elements); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return decodeRuleElements(elements)
}

// decodeRuleElements 逐个解码规则，并拒绝 null 元素。
func decodeRuleElements(elements []json.RawMessage) ([]config.Rule, error) {
	rules := make([]config.Rule, 0, len(elements))
	for i, raw := range elements {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("invalid JSON body: rules[%d] must be an object, got null", i)
		}
		var rule config.Rule
		if err := unmarshalStrict(raw, &rule); err != nil {
			return nil, fmt.Errorf("invalid JSON body: rules[%d]: %w", i, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// decodeJSON 严格解析请求体：限制体积并拒绝未知字段，避免拼错的参数被静默忽略。
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAdminBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// unmarshalStrict 与 decodeJSON 同样的严格语义，但作用于已在内存里的字节。
func unmarshalStrict(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// 拒绝 `[...] garbage` 这种「合法 JSON 后面还拖着东西」的输入。
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("unexpected trailing data after JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":{"message":"failed to encode response","type":"internal_error"}}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]string{"message": msg, "type": "invalid_request_error"},
	})
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]string{"message": "not found", "type": "not_found"},
	})
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
		"error": map[string]string{
			"message": "method not allowed, use " + strings.Join(allowed, " or "),
			"type":    "method_not_allowed",
		},
	})
}
