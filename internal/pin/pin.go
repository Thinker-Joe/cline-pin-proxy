// Package pin 实现 Cline Pass 的「上游钉死」注入。
//
// Cline Pass 网关后面有两条分流管道，钉死写法完全不同，且管道归属由网关侧决定：
//
//	planner（Vercel AI Gateway）：只认 providerOptions.gateway.{only,order,sort}
//	direct （OpenRouter）        ：只认顶层 provider.{only,order,sort}
//
// 实测结论（来源：cline-pass-switcher、cpagw-gateway 两个独立项目的公开记录）：
// 对 planner 管道，顶层 provider.only 会被 Cline 网关丢弃——这正是「在官方 API 上
// 写 provider.only 换上游不生效」的原因；反之 direct 管道会忽略 providerOptions。
//
// 因此默认策略是两条管道同时写：每条管道各自取用它认识的那份，互不干扰。
package pin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
)

// openRouterSort 把统一的排序语义映射为 OpenRouter 的取值。
// planner 管道直接吃 cost/ttft/tps，direct 管道需要这三个别名。
var openRouterSort = map[string]string{
	"cost": "price",
	"ttft": "latency",
	"tps":  "throughput",
}

// ErrNotObject 表示请求体不是一个 JSON 对象，无法注入。
var ErrNotObject = fmt.Errorf("request body is not a JSON object")

// Options 描述一次注入。
type Options struct {
	// Upstreams 是目标上游 slug，按优先级排列。
	Upstreams []string
	// Mode 决定用 only（strict）还是 order（preferred）。
	Mode config.PinMode
	// Pipeline 决定写在哪条管道上。
	Pipeline config.Pipeline
	// Sort 可选，要求网关按 cost/ttft/tps 排序候选。
	Sort string
}

// ModelOf 只解析请求体里的 model 字段，用于选规则。
// 不做完整解码，因此对超大请求体也很廉价。
func ModelOf(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var probe struct {
		Model string `json:"model"`
	}
	if err := dec.Decode(&probe); err != nil {
		return "", fmt.Errorf("decode model field: %w", err)
	}
	return strings.TrimSpace(probe.Model), nil
}

// Inject 把上游偏好按双管道语义写入请求体，返回新的 JSON 字节。
//
// 数字一律用 json.Number 承载，且编码时关闭 HTML 转义，保证除注入字段外
// 请求体的语义完全不变（含大整数精度与 < > & 等字符的原样保留）。
func Inject(body []byte, opts Options) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotObject, err)
	}

	ups := normalizeUpstreams(opts.Upstreams)
	sort := strings.ToLower(strings.TrimSpace(opts.Sort))
	preferred := opts.Mode == config.PinPreferred

	writePlanner, writeDirect := pipelines(opts.Pipeline)

	// 直接在被注入的那层对象上改，而不是先造一个临时 map 再合并：
	// 代理必须能**删掉**自己管辖字段里的旧值（见 applyChoice）。
	if writePlanner {
		gw := nestedMap(root, "providerOptions", "gateway")
		applyChoice(gw, ups, preferred)
		if sort != "" {
			gw["sort"] = sort
		}
	}

	if writeDirect {
		p := nestedMap(root, "provider")
		applyChoice(p, ups, preferred)
		if sort != "" {
			if mapped, ok := openRouterSort[sort]; ok {
				sort = mapped
			}
			p["sort"] = sort
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// 关闭 HTML 转义，避免把 < > & 改写成 \u003c 之类，保持请求体原貌。
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("encode request body: %w", err)
	}
	// json.Encoder 会补一个换行，去掉它。
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// pipelines 把配置里的管道选择翻译成两个布尔开关。
func pipelines(p config.Pipeline) (planner, direct bool) {
	switch p {
	case config.PipelinePlanner:
		return true, false
	case config.PipelineDirect:
		return false, true
	default:
		// auto：两条都写。未知管道下这是唯一安全的选择。
		return true, true
	}
}

// applyChoice 按钉死强度写入 only 或 order，并清掉互斥的旧值。
//
// strict   → only: [上游]         网关没有其他候选，等价于禁止回退
// preferred→ order: [上游, ...]   按序尝试，失败才回退
//
// 为什么必须删：这两组字段是**互斥**的，代理才是决定路由的一方（规则由
// 部署者配置，不是调用方）。若调用方自己发了 only，只追加 order 是无效的
// ——新候选根本不在 only 的允许集合里，配置的回退顺序形同虚设；
// 反过来 preferred 遇到调用方的 allow_fallbacks=false 也会被禁掉回退，
// 多候选悄悄退化成"只用第一个"。这类静默失效正是本项目最想避免的。
func applyChoice(dst map[string]any, upstreams []string, preferred bool) {
	if len(upstreams) == 0 {
		return
	}
	if preferred {
		delete(dst, "only")
		delete(dst, "allow_fallbacks")
		dst["order"] = toAnySlice(upstreams)
		return
	}
	delete(dst, "order")
	dst["only"] = []any{upstreams[0]}
}

// mergeInto 把 src 的键合并进 dst，同键以 src 为准。
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

// nestedMap 取出（必要时创建）root 下指定路径的嵌套对象。
// 中间节点若已存在但不是对象，会被替换为对象——注入字段需要它可写。
func nestedMap(root map[string]any, keys ...string) map[string]any {
	cur := root
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	return cur
}

func normalizeUpstreams(in []string) []string {
	out := make([]string, 0, len(in))
	for _, u := range in {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// ExtractUpstreams 读回响应里的路由事实，供日志与验收使用。
//
// 真实响应（2026-09 api.cline.bot 实测）里这两个字段的位置是：
//
//	planner：data.choices[0].message.provider_metadata.gateway.routing.finalProvider
//	direct ：data.provider（显示名，需要 slug 化，例如 "Z.AI" -> "z-ai"）
//
// 注意路由元数据挂在 choices[0].message 下，不在响应顶层；同时也兼容去掉
// data 信封、以及 provider_metadata 直接挂在 choice 上的变体。
//
// 返回空串表示该响应没有携带路由信息（例如错误响应）。
func ExtractUpstreams(payload map[string]any) (finalProvider, canonicalSlug string) {
	root := payload
	if data, ok := payload["data"].(map[string]any); ok {
		root = data
	}

	if routing, ok := routingFrom(root); ok {
		if s, ok := routing["finalProvider"].(string); ok {
			finalProvider = s
		}
		if s, ok := routing["canonicalSlug"].(string); ok {
			canonicalSlug = s
		}
	}
	if finalProvider == "" {
		if s, ok := root["provider"].(string); ok {
			finalProvider = slugify(s)
		}
	}
	return finalProvider, canonicalSlug
}

// routingFrom 依次尝试几处已知的放置位置，返回 provider_metadata.gateway.routing。
func routingFrom(root map[string]any) (map[string]any, bool) {
	if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				if r, ok := routingAt(msg); ok {
					return r, true
				}
			}
			if r, ok := routingAt(choice); ok {
				return r, true
			}
		}
	}
	return routingAt(root)
}

func routingAt(node map[string]any) (map[string]any, bool) {
	meta, ok := node["provider_metadata"].(map[string]any)
	if !ok {
		return nil, false
	}
	gateway, ok := meta["gateway"].(map[string]any)
	if !ok {
		return nil, false
	}
	routing, ok := gateway["routing"].(map[string]any)
	return routing, ok
}

// slugify 把上游显示名规范成 slug，例如 "GMICloud" -> "gmicloud"、"Z.AI" -> "z-ai"。
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
