# cline-pin-proxy

把 [Cline Pass](https://cline.bot/cline-pass) 订阅模型背后的**上游渠道钉死**的一个轻量透传代理。

单文件 Go 程序，**零第三方依赖**，约 1.5k 行（含测试），distroless 镜像约 5 MB。

```
你的调用方  ──►  cline-pin-proxy  ──►  Cline Pass  ──►  指定上游
 (sub2api        注入上游偏好            api.cline.bot      deepseek / z-ai / …
  等任何
  OpenAI 客户端)
```

---

## 它解决什么问题

Cline Pass 的订阅模型背后不是单一上游，而是一串第三方推理服务（`deepseek`、`z-ai`、
`baseten`、`novita`、`gmicloud`、`fireworks`……），由 Cline 网关自己调度。
不同渠道的成本、速度、量化格式不一样，但**客户端无法控制实际走哪家**。

原因在于 Cline 网关后面有**两条互不相同的分流管道**，钉死写法完全不同：

| 管道 | 实际后端 | 识别特征 | 钉死写法 |
|---|---|---|---|
| **planner** | Vercel AI Gateway | 响应带 `provider_metadata.gateway.routing` | `providerOptions.gateway.only` |
| **direct** | OpenRouter | 响应顶层带 `provider` 字段 | 顶层 `provider.only` |

**关键**：对 planner 管道，顶层 `provider.only` 会被 Cline 网关**直接丢弃**——
这正是「在官方 API 上写 `provider.only` 换上游不生效」的原因。反过来 direct 管道
会忽略 `providerOptions`。

管道归属由 Cline 侧决定而非客户端，**所以你无法提前知道该写哪一种**。

本代理的做法是**两条管道都写**：

```json
{
  "model": "cline-pass/deepseek-v4.1-flash",
  "messages": [ ... ],
  "providerOptions": { "gateway": { "only": ["deepseek"] } },
  "provider":        { "only": ["deepseek"] }
}
```

每条管道各取所需，互不干扰。实测确认这说明来自
[cline-pass-switcher](https://github.com/munmunjaklin458-afk/cline-pass-switcher) 与
[cpagw-gateway](https://github.com/Stabilize7440/cpagw-gateway) 两个独立项目的公开记录。

> **实测结论（2026-09-16，真实网关 + sub2api 全链路）**：
> `cline-pass/deepseek-v4.1-flash` 走 **planner**、`cline-pass/glm-5.3-flash` 走
> **direct**、`cline-pass/glm-5.3` 又是 **planner**，管道归属稳定但**逐模型不同**
> ——这正是必须双写的原因。
> 完整证据、成本数据与真实测试抓出的 bug 见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。

---

## 快速开始

### Docker（推荐）

```bash
# 内置默认规则已覆盖 DeepSeek 与 GLM，给个 key 就能跑
CLINE_PIN_API_KEY=sk_xxx docker compose up -d

# 确认活着
curl -s http://127.0.0.1:8787/healthz
```

镜像也已发布到 GHCR：

```bash
docker pull ghcr.io/thinker-joe/cline-pin-proxy:latest
```

### 单文件二进制

```bash
go build -o cline-pin-proxy ./cmd/cline-pin-proxy
CLINE_PIN_API_KEY=sk_xxx ./cline-pin-proxy serve
```

### 客户端接入

把任何 OpenAI 兼容客户端的 Base URL 指过来即可：

```
Base URL: http://127.0.0.1:8787/v1
API Key:  sk_xxx（Cline Pass 的 key）
Model:    cline-pass/deepseek-v4.1-flash
```

---

## 接入 sub2api

典型场景：sub2api 把 Cline Pass 当成一个 **openai 平台的 api-key 账号**，
你希望这个账号转出去的请求被钉到 DeepSeek / GLM 官方渠道。

把该账号的 `base_url` 从 `https://api.cline.bot/api/v1` 改成代理地址即可：

```
https://api.cline.bot/api/v1   →   http://127.0.0.1:8787/v1
```

sub2api 会拼成 `http://127.0.0.1:8787/v1/chat/completions`，正好命中代理的注入端点。

三个衔接细节：

1. **凭据**。若代理配了 `CLINE_PIN_API_KEY`，sub2api 账号里的 key 填什么都行（会被覆盖）；
   若留空，则 sub2api 账号里的 key 必须是真实的 Cline Pass key。
2. **`x-client-type` 请求头**。sub2api 常用 `header_override` 注入
   `x-client-type: cline-cli`，代理默认已把它透传（见 `CLINE_PIN_FORWARD_HEADERS`）。
3. **`/v1/responses` 探测**。sub2api 会探测上游是否支持 Responses API。
   代理对非 `chat/completions` 路径**纯净透传**，不做任何注入，让 sub2api
   拿到 Cline Pass 的真实答复自行判断——不会因为代理的存在而误判协议。

> 如果 sub2api 与代理都在容器里，把两者放进同一 Docker 网络，
> 并把 `base_url` 指向 `http://cline-pin-proxy:8787/v1`。

---

## 配置

配置优先级：**环境变量 > 配置文件 > 内置默认值**。

### 内置默认规则

开箱即用（`contains` 匹配、`strict` 模式、双管道）。**规则按序匹配，具体在前**：

| 命中 | 钉到 | 备注 |
|---|---|---|
| `*deepseek*` | `deepseek` | 实测在该模型 16 个可用上游之列 |
| `*glm-5.3-flash*` | `z-ai` | **必须排在 `glm-5.3` 之前** |
| `*glm-5.3*` | **`zai`** | 注意：与上面不是同一个 slug |

⚠️ **同一家厂商在不同模型上用了两种 slug**：`glm-5.3-flash` 的上游叫 `z-ai`，
而 `glm-5.3` 的叫 `zai`。用一条泛化的 `glm` → `z-ai` 规则会把 `glm-5.3`
打成 400（`No available providers match the 'only' filter: z-ai`）。
这两条规则的顺序与 slug 都是承重的，改动前请先 `probe`。

三条规则都已在真实网关上逐模型验证生效，且经 sub2api 全链路验收 9/9 通过。
详见 **[docs/VERIFICATION.md](docs/VERIFICATION.md)**。

⚠️ 上游 slug 会随 Cline 侧渠道池变动，清单只是快照。
**上线前请用 `probe` 子命令在你自己的账号上复核。**

### 配置文件

```bash
cp config.example.json config.json
```

```json
{
  "listen": "127.0.0.1:8787",
  "upstream": "https://api.cline.bot/api/v1",
  "api_key": "",
  "forward_headers": ["x-client-type"],
  "max_body_bytes": 67108864,
  "rules": [
    {
      "name": "deepseek-official",
      "model": "deepseek-v4.1-flash",
      "match": "contains",
      "pipeline": "auto",
      "mode": "strict",
      "upstreams": ["deepseek"]
    },
    {
      "name": "glm-prefer-zai",
      "model": "glm",
      "match": "contains",
      "pipeline": "auto",
      "mode": "preferred",
      "upstreams": ["z-ai", "gmicloud"]
    }
  ]
}
```

### 字段说明

| 字段 | 取值 | 说明 |
|---|---|---|
| `name` | 字符串 | 仅用于日志与排查 |
| `model` | 字符串 | 用于比较的模式串 |
| `match` | `contains` / `prefix` / `exact` | 匹配方式，默认 `contains`（大小写不敏感） |
| `pipeline` | `auto` / `planner` / `direct` | 注入写在哪条管道，默认 `auto`（两条都写） |
| `mode` | `strict` / `preferred` | `strict` 用 `only` 锁死唯一候选；`preferred` 用 `order` 按序尝试、允许回退 |
| `upstreams` | 字符串数组 | 目标上游 slug。`strict` 只用第一个；`preferred` 需 ≥2 个 |
| `sort` | `cost` / `ttft` / `tps` | 可选，要求网关按该指标排序候选 |

**规则按数组顺序匹配，首个命中者生效。** 放具体的规则在前面，宽泛的放后面。

### 环境变量

| 变量 | 说明 |
|---|---|
| `CLINE_PIN_LISTEN` | 监听地址，默认 `127.0.0.1:8787` |
| `CLINE_PIN_UPSTREAM` | Cline Pass 基址，默认 `https://api.cline.bot/api/v1` |
| `CLINE_PIN_API_KEY` | 固定上游密钥；留空则透传调用方的 `Authorization` |
| `CLINE_PIN_FORWARD_HEADERS` | 额外透传的请求头，逗号分隔 |
| `CLINE_PIN_MAX_BODY_BYTES` | 请求体上限，默认 64 MiB |
| `CLINE_PIN_RULES` | 规则表 JSON，整体覆盖配置文件 |
| `CLINE_PIN_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |

---

## 探测上游

不知道某个模型背后有哪些渠道可用？不需要抓包，也不需要真的消耗 token：

```bash
cline-pin-proxy probe -model cline-pass/deepseek-v4.1-flash
```

输出：

```
模型      : cline-pass/deepseek-v4.1-flash
上游状态码: 404
管道      : direct
可用上游  : 4 个
   1. deepseek
   2. novita
   3. baseten
   4. gmicloud

可直接粘贴进 config.json 的 rules（默认钉第一个上游，可按需改）：
[ { "name": "pin-cline-pass-deepseek-v4-1-flash", ... } ]
```

**原理**：给请求注入一个绝不存在的上游名（`__probe__`），网关会在路由层直接失败。
因为不存在任何可用候选，这次请求走不到推理后端，**基本不消耗 token**，
而错误信息里会带上它当前可用的完整渠道清单。

两条管道的错误格式不同，代理会分别解析；`-pipeline` 可强制指定以排查管道归属。

### 校验配置

```bash
cline-pin-proxy check -config config.json -model cline-pass/glm-5.3-flash
```

---

## 观测

### 响应头

每个响应都会带上这次决策的结果：

| 响应头 | 含义 |
|---|---|
| `X-Cline-Pin-Rule` | 命中的规则名；`none` 表示未钉死 |
| `X-Cline-Pin-Upstreams` | 本次钉住的目标，多个用 `>` 连接 |
| `X-Cline-Pin-Mode` | `strict` / `preferred` |
| `X-Cline-Pin-Note` | **仅在未钉死时出现**，说明原因（无规则命中 / 注入失败） |

上游自己的路由元数据头（`x-*`）也会原样透传，包括 `X-Cline-Actual-Upstream`
之类的字段——**这就是「抓包确认 finalProvider」的替代品**。

### 日志

```json
{"level":"INFO","msg":"pinned request","model":"cline-pass/deepseek-v4.1-flash",
 "rule":"deepseek","upstreams":"deepseek","mode":"strict","pipeline":"auto"}
```

---

## 行为约定

这些是刻意设计的，改动前请先想清楚：

- **只注入 `POST /v1/chat/completions`**（含 `/chat/completions`、`/api/v1/chat/completions`）。
  其余端点纯净透传——调用方靠它们探测上游能力，代理不该干扰结论。
- **未命中规则时完全原样转发**，由 Cline Pass 自主路由。不会偷偷钉任何东西。
- **注入失败时降级为未钉死透传**，并在 `X-Cline-Pin-Note` 里注明，避免"以为钉住了"。
- **上游状态码与响应体原样回传**，调用方的故障转移逻辑才不会失灵。
- **流式响应逐块 Flush**，全程 O(1) 内存，不做任何 JSON 解析。
  这是首字延迟不退化的前提。
- **不设 `http.Client.Timeout`**：流式生成可能持续数分钟，整体超时会把长回答砍断。
  超时改由 dial / TLS / 响应头三段分别控制。
- 注入采用「解析 → 深度合并 → 重新编码」，数字用 `json.Number` 承载、
  且关闭 HTML 转义，保证除注入字段外请求体语义完全不变（含大整数精度与 `< > &` 原样保留）。

---

## 安全

- 默认**只监听 `127.0.0.1`**。代理通常与调用方同机，不需要对外暴露。
  容器内需监听 `0.0.0.0`，但 `docker-compose.yml` 把宿主机端口绑定限制在回环。
- 请求头**白名单透传**，白名单外的一律不外泄到上游。
- 响应头同样白名单，且刻意不转发 `Content-Length`（注入会改变长度）。
- 路径白名单限制在 `/v1/` 与 `/api/v1/`，代理不会变成访问上游任意路径的跳板。
- 请求体有大小上限，超限返回 413 而不是把内存读满。
- 配置里的 `api_key` 是明文。别把 `config.json` 提交进 git（`.gitignore` 已排除）。

---

## 开发

```bash
go test ./...              # 全部单测
go test -race ./...        # 需要 cgo（例如 Linux CI）
go vet ./...
gofmt -l .                 # 应为空
go test -cover ./...
```

覆盖率：`config` 94.8% / `pin` 97.5% / `probe` 92.0% / `proxy` 83.8%。

CI 在每次 push 与 PR 上跑 `gofmt` + `vet` + `test -race`；
打 tag 时构建 `linux/amd64`、`linux/arm64` 多架构镜像推到 GHCR，
并附带 5 个平台的裸二进制。

---

## 与同类项目的关系

| 项目 | 形态 | 与本项目的关系 |
|---|---|---|
| [cline-pass-switcher](https://github.com/munmunjaklin458-afk/cline-pass-switcher) | Node 代理 + 控制台 | 功能全（账号池、测速、校验），零依赖。**想开箱即用、要 UI 就选它** |
| [cpagw-gateway](https://github.com/Stabilize7440/cpagw-gateway) | CLIProxyAPI 插件（Go） | 需要 CPA 进程；双管道注入设计一致 |
| **本项目** | 单文件 Go 代理 | **只要钉死这一件事**：无账号池、无 UI、无状态，约 5 MB 镜像 |

本项目的双管道知识来自上述项目的公开实测记录，**未复制其源代码**。
详见 [NOTICE](NOTICE)。

---

## License

[MIT](LICENSE) —— 可自由使用、修改、商用，仅需保留版权声明。
