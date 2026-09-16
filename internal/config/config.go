// Package config 定义 cline-pin-proxy 的配置模型、默认值、校验与环境变量覆盖。
//
// 配置使用 JSON，由标准库解析，无需第三方模块。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
// Cline API 使用两条路由管道，各自读取不同的上游设置：
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
	// Upstream 是 Cline 网关的 OpenAI 兼容基址。
	Upstream string `json:"upstream"`
	// APIKey 非空时用它覆盖客户端传来的 Authorization；为空则透传客户端凭据。
	APIKey string `json:"api_key"`
	// ForwardHeaders 是额外需要透传给上游的客户端请求头（大小写不敏感）。
	ForwardHeaders []string `json:"forward_headers"`
	// ProbeHeaders 是 `probe` 子命令要附带的请求头及其取值。
	//
	// 必须单独配置取值，因为探测时没有"客户端请求"可供转发。实测：
	// `deepseek/deepseek-v4-flash` 这类模型缺少 `x-client-type: cline-cli`
	// 会直接 403，导致探测结论与线上真实行为不一致。
	ProbeHeaders map[string]string `json:"probe_headers"`
	// MaxBodyBytes 是允许读取的最大请求体字节数。
	MaxBodyBytes int64 `json:"max_body_bytes"`
	// WatchSeconds 是配置文件热重载的轮询间隔（秒）。0 表示关闭热重载。
	// 轮询间隔在启动时读取，修改它需要重启。
	WatchSeconds int `json:"watch_seconds"`
	// AdminToken 非空时启用管理 API（/admin/*），并要求携带该令牌。
	AdminToken string `json:"admin_token"`
	// AdminAllowUnauthenticated 在未设置 token 时也启用管理 API。
	// 仅在只绑回环、且确定没有其它本机进程会访问时开启。
	AdminAllowUnauthenticated bool `json:"admin_allow_unauthenticated"`
	// UnwrapDataEnvelope 将符合条件的 JSON 响应中的 data 对象作为响应体，
	// 使只读取顶层 choices 的客户端能够解析 Cline 补全。SSE 不做此转换。
	UnwrapDataEnvelope bool `json:"unwrap_data_envelope"`
	// Rules 是钉死规则表。
	Rules []Rule `json:"rules"`
}

// DefaultListen 只绑回环：代理通常与调用方同机，无需对外暴露。
const DefaultListen = "127.0.0.1:8787"

// DefaultUpstream 是 Cline API 的 OpenAI 兼容入口。
const DefaultUpstream = "https://api.cline.bot/api/v1"

// DefaultMaxBodyBytes 允许多轮长上下文请求。
const DefaultMaxBodyBytes int64 = 64 << 20 // 64 MiB

// DefaultWatchSeconds 是配置文件热重载的默认轮询间隔。
const DefaultWatchSeconds = 5

// Default 返回包含 DeepSeek 与 GLM 规则的默认配置。
// 上游选择依据为 docs/VERIFICATION.md 中 2026-09-16 的实测：
//
//	glm-5.3-flash → relace   首字节延迟 0.72–0.97s，4/4 成功
//	glm-5.3       → friendli 首字节延迟 0.31–0.35s，4/4 成功
//
// flash 规则必须在前，因为 glm-5.3 也能匹配 flash 模型名。
// 两个模型使用不同管道，上游列表和表现不能互相推导，不得合并成宽泛的 glm 规则。
// 修改上游前应对具体模型重新测试；原测试未确认量化方式或输出质量。
func Default() *Config {
	return &Config{
		Listen:       DefaultListen,
		Upstream:     DefaultUpstream,
		MaxBodyBytes: DefaultMaxBodyBytes,
		WatchSeconds: DefaultWatchSeconds,
		// 默认转换 data 包装，可显式关闭以保留原始响应体。
		UnwrapDataEnvelope: true,
		// Cline 网关会用这个头区分调用方，上游侧常见配置依赖它，因此默认透传。
		ForwardHeaders: []string{"x-client-type"},
		// 探测时也要带上同一个头，否则部分模型（如 deepseek/... 规范名）
		// 会 403，得出与线上相反的结论。
		ProbeHeaders: map[string]string{"x-client-type": "cline-cli"},
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
				// 必须排在 glm-5.3 之前：前缀更具体者先匹配。
				Name:      "glm-5.3-flash",
				Model:     "glm-5.3-flash",
				Match:     MatchContains,
				Pipeline:  PipelineAuto,
				Mode:      PinStrict,
				Upstreams: []string{"relace"},
			},
			{
				Name:      "glm-5.3",
				Model:     "glm-5.3",
				Match:     MatchContains,
				Pipeline:  PipelineAuto,
				Mode:      PinStrict,
				Upstreams: []string{"friendli"},
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
		if err := applyFile(cfg, raw); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}

	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyFile 把配置文件里**出现过的**字段覆盖到 cfg 上，其余保持默认值。
//
// 刻意不写成 json.Unmarshal(raw, cfg)：Go 解码 slice 时会复用既有元素，
// 于是 {"rules":[{"model":"x"}]} 里没写的 Name/Upstreams 会继承**同位置默认
// 规则**的值，把自定义模型悄悄发去默认渠道。这里每个字段都解码到独立的新值，
// rules 更是整体替换，不会与默认表混在一起。
func applyFile(cfg *Config, raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	// 顶层 null 会得到 nil map：它既不是"空配置"也不是"没写字段"，
	// 直接拒绝，免得被当成合法配置放行。
	if fields == nil {
		return errors.New("config must be a JSON object")
	}

	// 逐字段解码到零值再赋值：字符串/数字/布尔直接覆盖，切片与 map 整体替换。
	scalars := []struct {
		key string
		dst any
	}{
		{"listen", &cfg.Listen},
		{"upstream", &cfg.Upstream},
		{"api_key", &cfg.APIKey},
		{"admin_token", &cfg.AdminToken},
		{"admin_allow_unauthenticated", &cfg.AdminAllowUnauthenticated},
		{"unwrap_data_envelope", &cfg.UnwrapDataEnvelope},
		{"max_body_bytes", &cfg.MaxBodyBytes},
		{"watch_seconds", &cfg.WatchSeconds},
	}
	for _, f := range scalars {
		raw, ok := fields[f.key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, f.dst); err != nil {
			return fmt.Errorf("field %q: %w", f.key, err)
		}
	}

	if raw, ok := fields["forward_headers"]; ok {
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("field %q: %w", "forward_headers", err)
		}
		cfg.ForwardHeaders = v
	}
	if raw, ok := fields["probe_headers"]; ok {
		var v map[string]string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("field %q: %w", "probe_headers", err)
		}
		cfg.ProbeHeaders = v
	}
	if raw, ok := fields["rules"]; ok {
		var rules []Rule
		if err := json.Unmarshal(raw, &rules); err != nil {
			return fmt.Errorf("field %q: %w", "rules", err)
		}
		if rules == nil {
			return errors.New(`field "rules" must be an array (use [] to disable pinning)`)
		}
		cfg.Rules = rules
	}
	return nil
}

// applyEnv 应用环境变量覆盖。环境变量优先级高于配置文件。
//
// 显式给出但内容非法时**返回错误**而不是静默忽略：把 `CLINE_PIN_RULES='[{bad'`
// 当成"没配置"，会让运维者以为规则表已被覆盖，实际继续走默认渠道。
func applyEnv(cfg *Config) error {
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
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("CLINE_PIN_MAX_BODY_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxBodyBytes = n
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_WATCH_SECONDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("CLINE_PIN_WATCH_SECONDS must be a non-negative integer, got %q", v)
		}
		cfg.WatchSeconds = n
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_ADMIN_TOKEN")); v != "" {
		cfg.AdminToken = v
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_ADMIN_ALLOW_UNAUTHENTICATED")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("CLINE_PIN_ADMIN_ALLOW_UNAUTHENTICATED must be a boolean, got %q", v)
		}
		cfg.AdminAllowUnauthenticated = b
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_UNWRAP_DATA_ENVELOPE")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("CLINE_PIN_UNWRAP_DATA_ENVELOPE must be a boolean, got %q", v)
		}
		cfg.UnwrapDataEnvelope = b
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_FORWARD_HEADERS")); v != "" {
		cfg.ForwardHeaders = splitList(v)
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_PROBE_HEADERS")); v != "" {
		cfg.ProbeHeaders = parseHeaderPairs(v)
	}
	if v := strings.TrimSpace(os.Getenv("CLINE_PIN_RULES")); v != "" {
		var rules []Rule
		if err := json.Unmarshal([]byte(v), &rules); err != nil {
			return fmt.Errorf("CLINE_PIN_RULES is not a valid JSON rule array: %w", err)
		}
		if rules == nil {
			return errors.New(`CLINE_PIN_RULES must be a JSON array (use [] to disable pinning)`)
		}
		cfg.Rules = rules
	}
	return nil
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

// parseHeaderPairs 解析 "name: value, name2: value2" 形式的请求头配置。
// 找不到冒号的条目会被忽略，避免把写错的配置当成合法头名。
func parseHeaderPairs(s string) map[string]string {
	out := make(map[string]string)
	for _, item := range splitList(s) {
		name, value, ok := strings.Cut(item, ":")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		out[name] = value
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
	if err := validateUpstream(c.Upstream); err != nil {
		return err
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.AdminToken = strings.TrimSpace(c.AdminToken)
	if c.WatchSeconds < 0 {
		c.WatchSeconds = 0
	}

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

	probeHeaders := make(map[string]string, len(c.ProbeHeaders))
	for name, value := range c.ProbeHeaders {
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		probeHeaders[name] = value
	}
	c.ProbeHeaders = probeHeaders

	for i := range c.Rules {
		if err := c.Rules[i].Normalize(i); err != nil {
			return err
		}
	}
	return nil
}

// validateUpstream 校验基址真的可以被 url 解析并用于路径拼接。
//
// 只看 "http://" 前缀不够：`http://%` 要通过真实请求才炸，而带 query 的
// `http://host?x=1` 会被后续的字符串拼接毁掉（端点被追加进 query）。
func validateUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstream is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("upstream scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("upstream must include a host, got %q", raw)
	}
	// query / fragment 会与"追加端点路径"的拼接方式冲突，userinfo 允许保留。
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("upstream must not contain a query or fragment, got %q", raw)
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

// Matches 报告规则是否命中给定模型 ID，不要求 cline-pass/ 或其他命名空间前缀。
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
