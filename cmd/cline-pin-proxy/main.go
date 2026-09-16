// Command cline-pin-proxy 是一个把 Cline Pass 的上游渠道钉死的透传代理。
//
// 它解决一个很具体的问题：Cline Pass 订阅模型背后有多个推理上游（deepseek、
// z-ai、baseten、novita……），由 Cline 网关自行调度，客户端无法控制实际走哪家。
// 而这个网关内部有两条互不相同的分流管道，钉死写法完全不同：
//
//	planner（Vercel AI Gateway）：只认 providerOptions.gateway.{only,order,sort}
//	direct （OpenRouter）        ：只认顶层 provider.{only,order,sort}
//
// 本代理在请求进入 Cline Pass 之前把两份写法都注入进去，让每条管道各取所需。
//
// 用法：
//
//	cline-pin-proxy serve   -config config.json
//	cline-pin-proxy probe   -model cline-pass/deepseek-v4.1-flash
//	cline-pin-proxy check   -config config.json -model deepseek-v4-flash
//	cline-pin-proxy version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Thinker-Joe/cline-pin-proxy/internal/admin"
	"github.com/Thinker-Joe/cline-pin-proxy/internal/config"
	"github.com/Thinker-Joe/cline-pin-proxy/internal/probe"
	"github.com/Thinker-Joe/cline-pin-proxy/internal/proxy"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	sub := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}

	var err error
	switch sub {
	case "serve":
		err = runServe(args)
	case "probe":
		err = runProbe(args)
	case "check":
		err = runCheck(args)
	case "healthcheck":
		err = runHealthcheck(args)
	case "version":
		fmt.Println("cline-pin-proxy", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (want serve|probe|check|version)", sub)
	}

	// `cline-pin-proxy -h` 会被当成 serve 的 flag，flag 包打完用法后返回
	// ErrHelp。那是一次正常的求助手势，不该以非零码退出。
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func usage() {
	fmt.Print(`cline-pin-proxy - 钉死 Cline Pass 的上游渠道

子命令:
  serve        启动透传代理（默认）
  probe        探测某个模型背后真实可用的上游渠道（零 token 开销）
  check        校验配置并预览规则匹配结果
  healthcheck  探测本地 /healthz，供容器 HEALTHCHECK 使用
  version      打印版本

示例:
  cline-pin-proxy serve -config config.json
  cline-pin-proxy probe -model cline-pass/deepseek-v4.1-flash
  cline-pin-proxy probe -model deepseek/deepseek-v4-flash -H "x-client-type: cline-cli"
  cline-pin-proxy check -config config.json -model deepseek-v4-flash
  cline-pin-proxy healthcheck -url http://127.0.0.1:8787/healthz

环境变量:
  CLINE_PIN_LISTEN             监听地址
  CLINE_PIN_UPSTREAM           Cline Pass 基址
  CLINE_PIN_API_KEY            固定上游 API Key（为空则透传客户端凭据）
  CLINE_PIN_FORWARD_HEADERS    额外透传的请求头，逗号分隔
  CLINE_PIN_PROBE_HEADERS      probe 附带的请求头，"name: value" 逗号分隔
  CLINE_PIN_MAX_BODY_BYTES     请求体上限
  CLINE_PIN_WATCH_SECONDS      配置文件热重载间隔（0 关闭）
  CLINE_PIN_ADMIN_TOKEN        管理 API 令牌（设置后启用 /admin/*）
  CLINE_PIN_ADMIN_ALLOW_UNAUTHENTICATED  未设令牌时也启用管理 API
  CLINE_PIN_RULES              规则表 JSON，覆盖配置文件
  CLINE_PIN_LOG_LEVEL          日志级别 debug|info|warn|error

管理 API（需配置 admin_token，否则返回 404）:
  GET  /admin/config   查看当前生效配置（密钥不下发）
  GET  /admin/rules    查看规则
  PUT  /admin/rules    整体替换规则，立即生效并尽力写回配置文件
  POST /admin/probe    探测模型可用上游   {"model":"cline-pass/glm-5.3"}
  POST /admin/reload   强制从文件重载配置

  鉴权：Authorization: Bearer <token> 或 X-Admin-Token: <token>
  示例：curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8787/admin/rules
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径（可选）")
	listen := fs.String("listen", "", "覆盖监听地址")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := newLogger()

	store, err := config.NewStore(*configPath, logger)
	if err != nil {
		return err
	}
	cfg := store.Current()

	// listen 只在启动时读一次：进程无法在不中断连接的前提下重新绑定端口，
	// 因此它不参与热重载（其余配置都可以）。
	addr := cfg.Listen
	if v := strings.TrimSpace(*listen); v != "" {
		addr = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 热重载：改配置文件即生效，Docker 下无需重建容器。
	// 这一点很关键——config.json 是挂载文件，compose 察觉不到内容变化。
	if secs := cfg.WatchSeconds; secs > 0 && store.Path() != "" {
		go store.Watch(ctx, time.Duration(secs)*time.Second)
	}

	mux := http.NewServeMux()
	proxy.New(store, logger).Register(mux)

	adminHandler := admin.NewHandler(store, &configProber{
		store:  store,
		client: &http.Client{Timeout: 90 * time.Second},
	}, logger)
	adminHandler.Register(mux)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// 刻意不设 WriteTimeout：流式生成可能持续数分钟，会被硬砍断。
		IdleTimeout: 120 * time.Second,
	}

	adminState := "disabled"
	switch {
	case strings.TrimSpace(cfg.AdminToken) != "":
		adminState = "enabled (token required)"
	case cfg.AdminAllowUnauthenticated:
		adminState = "enabled (no auth)"
	}
	logger.Info("cline-pin-proxy starting",
		"version", version,
		"listen", addr,
		"upstream", cfg.Upstream,
		"rules", len(cfg.Rules),
		"fixed_api_key", cfg.APIKey != "",
		"config_file", store.Path(),
		"watch_seconds", cfg.WatchSeconds,
		"admin_api", adminState,
	)
	for _, r := range cfg.Rules {
		logger.Info("rule loaded",
			"name", r.Name, "model", r.Model, "match", string(r.Match),
			"upstreams", strings.Join(r.Upstreams, ">"),
			"mode", string(r.Mode), "pipeline", string(r.Pipeline))
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径（可选）")
	model := fs.String("model", "", "要探测的模型 ID，例如 cline-pass/deepseek-v4.1-flash")
	pipeline := fs.String("pipeline", "auto", "强制管道：auto|planner|direct")
	apiKey := fs.String("api-key", "", "覆盖 API Key")
	var headerFlags multiFlag
	fs.Var(&headerFlags, "H", `附加上游请求头，格式 "name: value"，可重复。`+
		`部分模型（如 deepseek/... 规范名）缺少 x-client-type 会 403`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*model) == "" {
		return errors.New("probe: -model is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(*apiKey); v != "" {
		cfg.APIKey = v
	}
	if cfg.APIKey == "" {
		return errors.New("probe: 缺少 API Key，请设置 CLINE_PIN_API_KEY 或传 -api-key")
	}
	if len(headerFlags) > 0 {
		if cfg.ProbeHeaders == nil {
			cfg.ProbeHeaders = map[string]string{}
		}
		for _, item := range headerFlags {
			name, value, ok := strings.Cut(item, ":")
			if !ok {
				return fmt.Errorf("probe: -H 需要 \"name: value\" 格式，收到 %q", item)
			}
			cfg.ProbeHeaders[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}

	client := &http.Client{Timeout: 90 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := probe.Probe(ctx, client, cfg, strings.TrimSpace(*model), *pipeline)
	if err != nil {
		return err
	}

	fmt.Printf("模型      : %s\n", res.Model)
	fmt.Printf("上游状态码: %d\n", res.Status)
	if len(cfg.ProbeHeaders) > 0 {
		names := make([]string, 0, len(cfg.ProbeHeaders))
		for k := range cfg.ProbeHeaders {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Printf("附带请求头: %s\n", strings.Join(names, ", "))
	}
	if res.Pipeline == "" {
		fmt.Printf("管道      : 未识别（网关措辞可能已变化）\n")
	} else {
		fmt.Printf("管道      : %s\n", res.Pipeline)
	}
	if len(res.Upstreams) == 0 {
		fmt.Printf("可用上游  : 未能解析\n")
		if res.Detail != "" {
			fmt.Printf("原始片段  : %s\n", res.Detail)
		}
		fmt.Printf("\n提示：若上游返回 4xx 而非路由层报错，可能是缺少调用方身份头；\n" +
			"      试加 -H \"x-client-type: cline-cli\" 或配置 probe_headers。\n")
		return nil
	}

	fmt.Printf("可用上游  : %d 个\n", len(res.Upstreams))
	for i, u := range res.Upstreams {
		fmt.Printf("  %2d. %s\n", i+1, u)
	}
	fmt.Printf("\n注意：该清单来自路由层的错误信息，**不保证穷尽**——实测有好用的上游\n" +
		"      并不出现在这份清单里。要确认某个 slug 是否真的可用，直接发一次\n" +
		"      钉住它的真实请求、再看响应里的 finalProvider / provider 字段。\n")

	rule := config.Rule{
		Name:      "pin-" + sanitizeName(res.Model),
		Model:     res.Model,
		Match:     config.MatchExact,
		Pipeline:  config.PipelineAuto,
		Mode:      config.PinStrict,
		Upstreams: []string{res.Upstreams[0]},
	}
	suggested, err := json.MarshalIndent([]config.Rule{rule}, "", "  ")
	if err == nil {
		fmt.Printf("\n可直接粘贴进 config.json 的 rules（默认钉第一个上游，可按需改）：\n%s\n", suggested)
	}
	return nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '/' || r == '.':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径（可选）")
	model := fs.String("model", "", "可选：预览该模型会命中哪条规则")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("配置无效: %w", err)
	}

	fmt.Println("配置校验通过")
	fmt.Printf("  监听地址  : %s\n", cfg.Listen)
	fmt.Printf("  上游基址  : %s\n", cfg.Upstream)
	fmt.Printf("  固定 Key  : %v\n", cfg.APIKey != "")
	fmt.Printf("  请求体上限: %d 字节\n", cfg.MaxBodyBytes)
	fmt.Printf("  透传请求头: %s\n", strings.Join(cfg.ForwardHeaders, ", "))
	fmt.Printf("  规则数    : %d\n", len(cfg.Rules))
	for i, r := range cfg.Rules {
		fmt.Printf("    [%d] %-16s model=%-24s match=%-8s upstreams=%-28s mode=%-9s pipeline=%s\n",
			i, r.Name, r.Model, r.Match, strings.Join(r.Upstreams, ">"), r.Mode, r.Pipeline)
	}

	if m := strings.TrimSpace(*model); m != "" {
		if r, ok := cfg.Match(m); ok {
			fmt.Printf("\n模型 %q 命中规则 %q → 钉到 %s（%s）\n",
				m, r.Name, strings.Join(r.Upstreams, ">"), r.Mode)
		} else {
			fmt.Printf("\n模型 %q 未命中任何规则 → 将纯净透传，由 Cline Pass 自主路由\n", m)
		}
	}
	return nil
}

// configProber 用**当前生效**的配置执行探测。
//
// 走配置源而不是启动时的快照，这样 probe_headers 之类的改动热重载后
// 立即对 /admin/probe 生效。
type configProber struct {
	store  config.Source
	client *http.Client
}

func (p *configProber) Probe(ctx context.Context, model, pipeline string) (probe.Result, error) {
	return probe.Probe(ctx, p.client, p.store.Current(), model, pipeline)
}

// multiFlag 允许同一个 flag 重复出现并累积取值。
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// runHealthcheck 探测本进程的 /healthz，供容器 HEALTHCHECK 使用。
//
// 单独做成子命令而不是依赖 curl/wget：镜像基于 distroless，里面没有任何
// shell 或 HTTP 客户端，只能靠二进制自己探自己。
func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8787/healthz", "健康检查地址")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(*url)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLINE_PIN_LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
