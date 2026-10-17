// Package pin 实现 Cline API 的上游字段注入，不限制模型 ID 的命名空间。
//
// Cline 网关使用两条路由管道，各自读取不同字段，管道归属由网关决定：
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

// pipelineSpelling 记录同一渠道在两条管道里的不同写法，注入前按目标管道翻译。
//
// 依据：2026-10-17 对 api.cline.bot 的实测（docs/VERIFICATION.md 第 12.5 节）。
// 把 slug 写到错误的管道不是"被忽略"，而是直接失败：
//
//	planner 对 z-ai / together 返回 400 No available providers match the 'only' filter
//	direct  对 zai / togetherai 返回 404 No allowed providers
//
// 与 sort 的 cost→price 同性质：这是网关侧的命名差异，由代理在写入时翻译，
// 而不是让每条规则自己区分管道。不在表里的 slug 原样保留——不猜测未实测的别名。
var pipelineSpelling = map[config.Pipeline]map[string]string{
	config.PipelinePlanner: {"z-ai": "zai", "together": "togetherai"},
	config.PipelineDirect:  {"zai": "z-ai", "togetherai": "together"},
}

// canonicalUpstreams 返回该管道应写入的 slug 列表；不修改入参，无映射时原样返回。
func canonicalUpstreams(ups []string, pipeline config.Pipeline) []string {
	table := pipelineSpelling[pipeline]
	if len(table) == 0 || len(ups) == 0 {
		return ups
	}
	out := make([]string, len(ups))
	for i, u := range ups {
		if canonical, ok := table[u]; ok {
			out[i] = canonical
			continue
		}
		out[i] = u
	}
	return out
}

// ErrNotObject 表示请求体不是一个 JSON 对象，无法注入。
var ErrNotObject = fmt.Errorf("request body is not a JSON object")

// Options 描述一次注入。
type Options struct {
	// Upstreams 是目标上游 slug，按优先级排列。
	Upstreams []string
	// Exclude 是 mode=exclude 时要排除的上游 slug，只写 direct 管道的
	// provider.ignore：planner 管道没有对应的排除字段（2026-10-17 实测）。
	Exclude []string
	// Mode 决定用 only（strict）、order（preferred）还是 ignore（exclude）。
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
	excl := normalizeUpstreams(opts.Exclude)
	sort := strings.ToLower(strings.TrimSpace(opts.Sort))
	preferred := opts.Mode == config.PinPreferred

	writePlanner, writeDirect := pipelines(opts.Pipeline)

	// 直接在被注入的那层对象上改，而不是先造一个临时 map 再合并：
	// 代理必须能**删掉**自己管辖字段里的旧值（见 applyChoice）。
	//
	// exclude 模式没有 upstreams，planner 也没有 ignore 对应物，所以两条分支
	// 都按"实际要写的内容"判断，而不是只看管道开关：否则 exclude 规则会在
	// planner 侧凭空写出一个 sort。
	if writePlanner && len(ups) > 0 {
		gw := nestedMap(root, "providerOptions", "gateway")
		applyChoice(gw, canonicalUpstreams(ups, config.PipelinePlanner), preferred)
		if sort != "" {
			gw["sort"] = sort
		}
	}

	if writeDirect && (len(ups) > 0 || len(excl) > 0) {
		p := nestedMap(root, "provider")
		if len(ups) > 0 {
			applyChoice(p, canonicalUpstreams(ups, config.PipelineDirect), preferred)
		}
		if len(excl) > 0 {
			// ignore 是排除法：调用方原有的 only/order 保留不动，与"不修改
			// 无关选项"的约定一致。
			p["ignore"] = toAnySlice(canonicalUpstreams(excl, config.PipelineDirect))
		}
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

// Routing 是一次补全响应里能读到的路由事实。
//
// 真实响应（2026-09 api.cline.bot 实测）里这些字段的位置是：
//
//	planner：data.choices[0].message.provider_metadata.gateway.routing.{finalProvider,canonicalSlug}
//	direct ：data.provider（显示名，需要 slug 化，例如 "Z.AI" -> "z-ai"）
//
// 注意路由元数据挂在 choices[0].message 下，不在响应顶层；同时也兼容去掉
// data 信封、以及 provider_metadata 直接挂在 choice 上的变体。
//
// 管道由**字段出现的位置**判定，不能按模型名推断。零值表示响应没有携带
// 路由信息（例如路由层错误），不能当成"没有服务过"。
type Routing struct {
	// Pipeline 是元数据所在管道；读不到路由信息时为空。
	Pipeline config.Pipeline
	// Provider 是实际服务的上游 slug：planner 读 finalProvider（本就是 slug），
	// direct 读顶层 provider 显示名再 slug 化。
	Provider string
	// CanonicalSlug 是 planner 元数据里的模型规范名；direct 不提供。
	CanonicalSlug string
}

// ExtractRouting 读回响应里的路由事实，供日志、探测与验收使用。
func ExtractRouting(payload map[string]any) Routing {
	root := payload
	if data, ok := payload["data"].(map[string]any); ok {
		root = data
	}

	if routing, ok := routingFrom(root); ok {
		out := Routing{Pipeline: config.PipelinePlanner}
		if s, ok := routing["finalProvider"].(string); ok {
			out.Provider = s
		}
		if s, ok := routing["canonicalSlug"].(string); ok {
			out.CanonicalSlug = s
		}
		if out.Provider != "" || out.CanonicalSlug != "" {
			return out
		}
	}
	if s, ok := root["provider"].(string); ok && strings.TrimSpace(s) != "" {
		return Routing{Pipeline: config.PipelineDirect, Provider: slugify(s)}
	}
	return Routing{}
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
