// Package probe 用「假上游」技巧枚举某个模型背后真实可用的上游渠道。
//
// 原理：给请求注入一个不存在的上游名（__probe__），网关会在路由层直接失败。
// 因为不存在任何可用候选，这次请求不会走到推理后端，所以基本不消耗 token，
// 而错误信息里会带上它当前可用的完整渠道清单。
//
// 真实响应结构（2026-09 实测，api.cline.bot）：
//
//	{"error":"inference request failed: failed to invoke model '<slug>' from
//	         <Vercel|Openrouter>: request failed with status <code>:
//	         {\"error\":{\"message\":\"...\",\"metadata\":{\"available_providers\":[...]}}}",
//	 "success":false}
//
// 注意外层 error 是**字符串**而不是对象，内层 JSON 被转义了一层。这个结构决定了
// 解析不能依赖简单的 json.Unmarshal，也不能依赖引号形状敏感的正则。
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
	"github.com/Thinker-Joe/cline-pin-proxy/internal/pin"
)

// impossibleUpstream 是绝不会存在的上游名，用来逼迫网关在路由层报错。
const impossibleUpstream = "__probe__"

// maxErrorBytes 限制读取的错误体大小，避免异常上游把内存打满。
const maxErrorBytes = 1 << 20 // 1 MiB

// upstreamSlugRe 过滤出合法的上游 slug。
var upstreamSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Result 是一次上游探测的结果。
type Result struct {
	Model     string   `json:"model"`
	Pipeline  string   `json:"pipeline"`
	Upstreams []string `json:"upstreams"`
	Status    int      `json:"status"`
	Detail    string   `json:"detail,omitempty"`
}

// Probe 探测一个模型背后真实可用的上游渠道。
//
// pipeline 传空或 auto 时同时注入两条管道，由网关侧决定实际生效的那条；
// 也可以强制 planner / direct 以排查管道归属。
func Probe(ctx context.Context, client *http.Client, cfg *config.Config, model, pipeline string) (Result, error) {
	res := Result{Model: model}

	payload := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return res, fmt.Errorf("build probe payload: %w", err)
	}

	mode := config.PipelineAuto
	switch config.Pipeline(strings.ToLower(strings.TrimSpace(pipeline))) {
	case config.PipelinePlanner:
		mode = config.PipelinePlanner
	case config.PipelineDirect:
		mode = config.PipelineDirect
	}

	raw, err = pin.Inject(raw, pin.Options{
		Upstreams: []string{impossibleUpstream},
		Mode:      config.PinStrict,
		Pipeline:  mode,
	})
	if err != nil {
		return res, fmt.Errorf("inject probe pin: %w", err)
	}

	endpoint := strings.TrimRight(cfg.Upstream, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return res, fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(cfg.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// 上游可能要求调用方身份头。实测 `deepseek/deepseek-v4-flash` 缺少
	// `x-client-type: cline-cli` 会直接 403，此时探测会得出与线上相反的结论，
	// 所以探测必须带上与代理转发同一套请求头。
	for name, value := range cfg.ProbeHeaders {
		req.Header.Set(name, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return res, fmt.Errorf("probe request failed: %w", err)
	}
	defer resp.Body.Close()

	text, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if err != nil {
		return res, fmt.Errorf("read probe response: %w", err)
	}
	res.Status = resp.StatusCode

	detected, upstreams := Extract(string(text))
	res.Pipeline = detected
	res.Upstreams = upstreams
	if len(upstreams) == 0 {
		res.Detail = truncate(string(text), 400)
	}
	return res, nil
}

// Extract 从网关的错误响应里还原出管道类型与真实可用的上游清单。
//
// 返回的 pipeline 为空表示两种格式都没匹配上（网关措辞变了，或请求根本没被
// 路由层拦下）。upstreams 已去重并保持网关给出的顺序。
func Extract(bodyText string) (pipeline string, upstreams []string) {
	// 网关把内层 JSON 整体转义了一层（`\"` 而非 `"`）。先还原再解析，
	// 否则下面的 JSON 切片会因为字面反斜杠而解析失败，标记式提取也会把
	// 最后一项与尾巴粘成的 token 判为非法。
	text := strings.ReplaceAll(bodyText, `\"`, `"`)

	// available_providers 是结构化字段，最完整且无歧义，优先用它。
	if list := extractAvailableProvidersArray(text); len(list) > 0 {
		return pipelineOr(text, string(config.PipelineDirect)), list
	}
	// planner 管道只给自然语言清单，没有结构化字段。
	for _, marker := range []string{"available providers are", "providers serving"} {
		if list := extractListAfterMarker(text, marker); len(list) > 0 {
			return pipelineOr(text, string(config.PipelinePlanner)), list
		}
	}
	return "", nil
}

// pipelineOr 优先用网关自己的措辞判断管道，拿不到再退回调用方给的兜底值。
func pipelineOr(text, fallback string) string {
	if p := detectPipeline(text); p != "" {
		return p
	}
	return fallback
}

// detectPipeline 用网关错误里的来源措辞判断管道归属。
//
// 实测：`... from Vercel:` 对应 planner，`... from Openrouter:` 对应 direct。
// 这比根据「哪种注入生效了」去反推更直接可靠。
func detectPipeline(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "from vercel"):
		return string(config.PipelinePlanner)
	case strings.Contains(lower, "from openrouter"):
		return string(config.PipelineDirect)
	}
	return ""
}

// extractAvailableProvidersArray 抠出 available_providers 的 JSON 数组。
//
// 刻意不用正则：真实响应里这个字段被包在「JSON 字符串套 JSON」的双层转义中，
// 引号前带反斜杠（形如 \"available_providers\"），引号敏感的正则很容易漏掉。
// 逐字符定位 [ 与 ] 更稳，且上游 slug 里不含 ]。
//
// 输入允许是未还原转义的原文：本函数自带一次还原，便于单独调用。
func extractAvailableProvidersArray(text string) []string {
	text = strings.ReplaceAll(text, `\"`, `"`)

	const key = "available_providers"
	idx := strings.Index(text, key)
	if idx < 0 {
		return nil
	}
	open := strings.IndexByte(text[idx:], '[')
	if open < 0 {
		return nil
	}
	open += idx
	closing := strings.IndexByte(text[open:], ']')
	if closing < 0 {
		return nil
	}
	closing += open

	var raw []string
	if err := json.Unmarshal([]byte(text[open:closing+1]), &raw); err != nil {
		return nil
	}
	return normalizeList(raw)
}

// extractListAfterMarker 解析网关的自然语言清单，例如：
//
//	Available providers are: alibaba, baseten, wafer
//	Providers serving z-ai/glm-5.3-flash-20260826: deepinfra, relace, modal, but your request's ...
//
// 做法：定位标记 → 跳到其后的第一个冒号 → 逐逗号读 token。
//
// **遇到第一个非法 slug 必须停，而不是跳过。** 实测 planner 响应的最后一项
// `wafer` 紧跟着 `","type":"invalid_request_error"`，两者之间没有逗号，若按
// 「跳过非法项继续扫」处理，wafer 会和整段 JSON 尾巴一起被丢掉。
func extractListAfterMarker(text, marker string) []string {
	idx := strings.Index(strings.ToLower(text), strings.ToLower(marker))
	if idx < 0 {
		return nil
	}
	rest := text[idx+len(marker):]
	if colon := strings.IndexByte(rest, ':'); colon >= 0 {
		rest = rest[colon+1:]
	}

	out := make([]string, 0, 32)
	for _, token := range strings.Split(rest, ",") {
		token = strings.ToLower(strings.Trim(token, " \t\r\n\"'"))
		if !upstreamSlugRe.MatchString(token) {
			break
		}
		out = append(out, token)
	}
	return dedupe(out)
}

// normalizeList 小写化、去空、去重，并保持原顺序。
func normalizeList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		out = append(out, item)
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// truncate 按字节截断长文本，仅供日志与诊断展示使用。
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
