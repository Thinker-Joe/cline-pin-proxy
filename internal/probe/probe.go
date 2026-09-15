// Package probe 用「假上游」技巧枚举某个模型背后真实可用的上游渠道。
//
// 原理：给请求注入一个不存在的上游名（__probe__），网关会在路由层直接失败。
// 因为不存在任何可用候选，这次请求不会走到推理后端，所以基本不消耗 token，
// 而错误信息里会带上它当前可用的完整渠道清单。
//
// 两条管道吐出的错误格式不同，需要分别解析：
//
//	planner：错误文本里的 "Available providers are: a, b, c"
//	direct ：错误 JSON 里的 error.metadata.available_providers
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

var (
	// availableProvidersRe 匹配 planner 管道的错误措辞。
	availableProvidersRe = regexp.MustCompile(`(?i)available providers are:\s*([^.]+)`)
	// upstreamSlugRe 过滤出合法的上游 slug，挡掉错误句子里混进来的普通单词。
	upstreamSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

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

// truncate 按字节截断长文本，仅供日志与诊断展示使用。
func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Extract 从网关的错误响应里还原出管道类型与真实可用的上游清单。
//
// 返回的 pipeline 为空表示两种格式都没匹配上（网关措辞变了，或请求根本没被
// 路由层拦下）。upstreams 已去重并保持网关给出的顺序。
func Extract(bodyText string) (pipeline string, upstreams []string) {
	if match := availableProvidersRe.FindStringSubmatch(bodyText); match != nil {
		list := parseProviderList(match[1])
		if len(list) > 0 {
			return string(config.PipelinePlanner), list
		}
	}

	if list := extractDirectProviders(bodyText); len(list) > 0 {
		return string(config.PipelineDirect), list
	}
	return "", nil
}

// parseProviderList 把 "a, b, c" 拆成合法的上游 slug 列表。
func parseProviderList(segment string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, 8)
	for _, token := range strings.Split(segment, ",") {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" || seen[token] || !upstreamSlugRe.MatchString(token) {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

// extractDirectProviders 从 direct 管道的错误 JSON 里取出可用上游。
// 错误文本前面可能带 HTTP 状态之类的前缀，所以从第一个 '{' 开始解析。
func extractDirectProviders(bodyText string) []string {
	start := strings.IndexByte(bodyText, '{')
	if start < 0 {
		return nil
	}
	var parsed struct {
		Error struct {
			Metadata struct {
				AvailableProviders []string `json:"available_providers"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(bodyText[start:]), &parsed); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(parsed.Error.Metadata.AvailableProviders))
	for _, p := range parsed.Error.Metadata.AvailableProviders {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
