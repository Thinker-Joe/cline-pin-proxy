// Package config 定义 cline-pin-proxy 的配置模型、默认值、校验与环境变量覆盖。
//
// 设计原则：零第三方依赖。配置文件用 JSON，避免为 YAML 引入模块依赖，
// 这样 Docker 镜像可以在完全离线的环境里构建。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MatchMode 决定规则中的 Model 如何与请求里的模型 ID 比较。
type MatchMode string

const (
	// MatchContains 大小写不敏感的子串匹配（默认）。
	// 例：Model="deepseek" 可命中 cline-pass/deepseek-v4.1-flash。
	MatchContains MatchMode = "contains"
	// MatchPrefix 大小写不敏感的前缀匹配。
	MatchPrefix MatchMode = "prefix"
	// MatchExact 大小写不敏感的完整相等匹配。
	MatchExact MatchMode = "exact"
)

// PinMode 决定注入语义。
type PinMode string

const (
	// PinStrict 用 only 把上游锁为唯一候选，网关不会回退到其他渠道。
	PinStrict PinMode = "strict"
	// PinPreferred 用 order 按顺序尝试，当前候选失败时允许网关回退。
	PinPreferred PinMode = "preferred"
)

// Pipeline 决定上游偏好写在哪条管道上。
//
// Cline Pass 后面存在两条互不相同的分流管道，钉死写法不同：
//   - planner：Vercel AI Gateway，只认 providerOptions.gateway.*
//   - direct：OpenRouter，只认顶层 provider.*
//
// 管道归属由 Cline 网关侧决定而非客户端，所以默认 auto 同时写两条，
// 每条管道各自忽略不认识的那份，互不干扰。
type Pipeline string

const (
	PipelineAuto    Pipeline = "auto"
	PipelinePlanner Pipeline = "planner"
	PipelineDirect  Pipeline = "direct"
)

// Rule 描述一条「模型 → 上游」钉死规则。规则按数组顺序匹配，首个命中者生效。
type Rule struct {
	// Name 仅用于日志与排查。
	Name string `json:"name"`
	// Model 是用于比较的模式串。
	Model string `json:"model"`
	// Match 是匹配方式，默认 contains。
	Match MatchMode `json:"match"`
	// Pipeline 指定注入管道，默认 auto。
	Pipeline Pipeline `json:"pipeline"`
	// Mode 是钉死强度，默认 strict。
	Mode PinMode `json:"mode"`
	// Upstreams 是目标上游 slug，按优先级排列。
	// strict 只用第一个；preferred 按顺序全部写入 order。
	Upstreams []string `json:"upstreams"`
	// Sort 可选，取值 cost / ttft / tps，要求网关按该指标排序候选。
	Sort string `json:"sort"`
}

// Config 是进程级配置。
type Config struct {
	// Listen 是监听地址。默认只绑定回环，避免把代理暴露到公网。
	Listen string `json:"listen"`
	// Upstream 是 Cline Pass 网关的 OpenAI 兼容基址。
	Upstream string `json:"upstream"`
	// APIKey 非空时用它覆盖客户端传来的 Authorization；为空则透传客户端凭据。
	APIKey string `json:"api_key"`
	// ForwardHeaders 是额外需要透传给上游的客户端请求头（大小写不敏感）。
	ForwardHeaders []string `json:"forward_headers"`
	// MaxBodyBytes 是允许读取的最大请求体字节数。
	MaxBodyBytes int64 `json:"max_body_bytes"`
	// Rules 是钉死规则表。
	Rules []Rule `json:"rules"`
}

// DefaultListen 只绑回环：代理通常与调用方同机，无需对外暴露。
const DefaultListen = "127.0.0.1:8787"

// DefaultUpstream 是 Cline Pass 的官方 OpenAI 兼容入口。
const DefaultUpstream = "https://api.cline.bot/api/v1"

// DefaultMaxBodyBytes 允许多轮长上下文请求。
const DefaultMaxBodyBytes int64 = 64 << 20 // 64 MiB

// Default 返回内置默认配置，其中包含针对 DeepSeek 与 GLM 的常用钉死规则。
//
// 默认规则只是起点：上游 slug 会随 Cline 侧渠道变动，请用 `probe` 子命令
// 探测你账号下真实的可用渠道后再定稿。
func Default() *Config {
	return &Config{
		Listen:       DefaultListen,
		Upstream:     DefaultUpstream,
		MaxBodyBytes: DefaultMaxBodyBytes,
		// Cline 网关会用这个头区分调用方，上游侧常见配置依赖它，因此默认透传。
		ForwardHeaders: []string{"x-client-type"},
		Rules: []Rule{
			{
				Name:      "deepseek",
				Model:     "deepseek",
				Match:     MatchContains,
				Pipeline:  PipelineAuto,
				Mode:      PinStrict,
				Upstreams: []string{"deepseek"},
			},
			{
				Name:      "glm",
				Model:     "glm",
				Match:     MatchContains,
				Pipeline:  PipelineAuto,
				Mode:      PinStrict,
				Upstreams: []string{"z-ai"},
			},
		},
	}
}

// Load 读取配置文件并应用默认值与环境变量覆盖。
// path 为空时直接使用默认配置，仍然会应用环境变量覆盖。
func Load(path string) (*Config, error) {
	cfg := Default()

	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		// 从默认值出发整体覆盖，让配置文件只需写关心到的字段。
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(cfg)

	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv 应用环境变量覆盖。环境变量优先级高于配置文件。
func applyEnv(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_LISTEN")); v != "" {
		cfg.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_UPSTREAM")); v != "" {
		cfg.Upstream = v
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_API_KEY")); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_MAX_BODY_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			cfg.MaxBodyBytes = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_FORWARD_HEADERS")); v != "" {
		cfg.ForwardHeaders = splitList(v)
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_RULES")); v != "" {
		var rules []Rule
		if err := json.Unmarshal([]byte(v), &rules); err == nil {
			cfg.Rules = rules
		}
	}
}

func splitList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Normalize 补齐默认值并做严格校验，就地修改 cfg。
func (c *Config) Normalize() error {
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	c.Upstream = strings.TrimRight(strings.TrimSpace(c.Upstream), "/")
	if c.Upstream == "" {
		c.Upstream = DefaultUpstream
	}
	if !strings.HasPrefix(c.Upstream, "http://") && !strings.HasPrefix(c.Upstream, "https://") {
		return fmt.Errorf("upstream must start with http:// or https://, got %q", c.Upstream)
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	c.APIKey = strings.TrimSpace(c.APIKey)

	seen := make(map[string]bool, len(c.ForwardHeaders))
	headers := make([]string, 0, len(c.ForwardHeaders))
	for _, h := range c.ForwardHeaders {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		lower := strings.ToLower(h)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		headers = append(headers, lower)
	}
	c.ForwardHeaders = headers

	for i := range c.Rules {
		if err := c.Rules[i].Normalize(i); err != nil {
			return err
		}
	}
	return nil
}

// Normalize 校验单条规则并补齐默认值。idx 用于生成可定位的错误信息。
func (r *Rule) Normalize(idx int) error {
	where := fmt.Sprintf("rules[%d]", idx)
	if strings.TrimSpace(r.Name) == "" {
		r.Name = fmt.Sprintf("rule-%d", idx)
	}
	r.Model = strings.TrimSpace(r.Model)
	if r.Model == "" {
		return fmt.Errorf("%s (%s): model must not be empty", where, r.Name)
	}

	switch r.Match {
	case "":
		r.Match = MatchContains
	case MatchContains, MatchPrefix, MatchExact:
	default:
		return fmt.Errorf("%s (%s): match must be one of contains|prefix|exact, got %q", where, r.Name, r.Match)
	}

	switch r.Pipeline {
	case "":
		r.Pipeline = PipelineAuto
	case PipelineAuto, PipelinePlanner, PipelineDirect:
	default:
		return fmt.Errorf("%s (%s): pipeline must be one of auto|planner|direct, got %q", where, r.Name, r.Pipeline)
	}

	switch r.Mode {
	case "":
		r.Mode = PinStrict
	case PinStrict, PinPreferred:
	default:
		return fmt.Errorf("%s (%s): mode must be one of strict|preferred, got %q", where, r.Name, r.Mode)
	}

	ups := make([]string, 0, len(r.Upstreams))
	for _, u := range r.Upstreams {
		if u = strings.TrimSpace(u); u != "" {
			ups = append(ups, u)
		}
	}
	if len(ups) == 0 {
		return fmt.Errorf("%s (%s): upstreams must not be empty", where, r.Name)
	}
	if r.Mode == PinPreferred && len(ups) < 2 {
		// preferred 只给一个候选等价于 strict 但没有回退，通常是配置笔误。
		return fmt.Errorf("%s (%s): mode=preferred needs at least 2 upstreams, got %d", where, r.Name, len(ups))
	}
	r.Upstreams = ups

	switch s := strings.ToLower(strings.TrimSpace(r.Sort)); s {
	case "", "none":
		r.Sort = ""
	case "cost", "ttft", "tps":
		r.Sort = s
	default:
		return fmt.Errorf("%s (%s): sort must be one of cost|ttft|tps, got %q", where, r.Name, r.Sort)
	}
	return nil
}

// Match 返回首个命中给定模型的规则。
// 传 nil 接收者或空规则表时返回 false，调用方据此走纯净透传。
func (c *Config) Match(model string) (Rule, bool) {
	if c == nil {
		return Rule{}, false
	}
	for _, r := range c.Rules {
		if r.Matches(model) {
			return r, true
		}
	}
	return Rule{}, false
}

// Matches 报告规则是否命中给定模型 ID。
func (r Rule) Matches(model string) bool {
	key := strings.ToLower(strings.TrimSpace(r.Model))
	target := strings.ToLower(strings.TrimSpace(model))
	if key == "" || target == "" {
		return false
	}
	switch r.Match {
	case MatchExact:
		return target == key
	case MatchPrefix:
		return strings.HasPrefix(target, key)
	default:
		return strings.Contains(target, key)
	}
}
