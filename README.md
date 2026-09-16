# cline-pin-proxy

把 [Cline Pass](https://cline.bot/cline-pass) 订阅模型背后的**上游渠道钉死**的一个轻量透传代理。

单二进制 Go 程序（源码是多包布局，产物只有一个静态链接的可执行文件），
**零第三方依赖**，约 2.4k 行源码 + 2.5k 行测试，distroless 镜像约 5 MB。

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
# 1) 准备配置（内置默认规则已覆盖 DeepSeek 与 GLM，可以先不改）
mkdir -p data && cp config.example.json data/config.json

# 2) 起服务
CLINE_PIN_API_KEY=sk_xxx docker compose up -d

# 3) 确认活着
curl -s http://127.0.0.1:8787/healthz
```

配置文件挂在 `./data/config.json`，**改它不用重启容器** —— 见[热重载](#热重载)。

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

| 命中 | 钉到 | 实测 TTFT |
|---|---|---|
| `*deepseek*` | `deepseek`（官方） | — |
| `*glm-5.3-flash*` | `relace` | 0.72–0.97s（4/4 成功） |
| `*glm-5.3*` | `friendli` | **0.31–0.35s（4/4 成功）** |

⚠️ **GLM 刻意不钉官方渠道**：官方 `zai` / `z-ai` 实测首字延迟
**2.0–3.1s / 1.8–2.2s**，而选中的两个第三方渠道快 **3–6 倍**。
代价是可能落到量化（fp8/fp4）版本——这是知情的速度/质量取舍。

⚠️ **两条 GLM 规则必须保持这个顺序，且不能合并成泛化的 `glm`**：
两条管道的渠道池**不通用**（`glm-5.3` 走 planner、`glm-5.3-flash` 走 direct）。
把一侧测通的 slug 搬到另一侧可能直接失败——实测 `glm-5.3-flash` 在 strict 下的
`morph`/`novita`/`makora`/`baseten`/`modal` 等会报 `stream_initialization_failed`。
**换 slug 前必须在对应模型上重新测速。**

所有 slug 与延迟均来自真实网关实测，经 sub2api 全链路验收。
完整数据见 **[docs/VERIFICATION.md](docs/VERIFICATION.md)**。

### 配置文件

```bash
cp config.example.json config.json      # Docker 部署则是 data/config.json
```

```json
{
  "listen": "127.0.0.1:8787",
  "upstream": "https://api.cline.bot/api/v1",
  "api_key": "",
  "forward_headers": ["x-client-type"],
  "probe_headers": { "x-client-type": "cline-cli" },
  "max_body_bytes": 67108864,

  "watch_seconds": 5,
  "admin_token": "",
  "admin_allow_unauthenticated": false,

  "rules": [
    {
      "name": "deepseek-official",
      "model": "deepseek",
      "match": "contains",
      "pipeline": "auto",
      "mode": "strict",
      "upstreams": ["deepseek"]
    },
    {
      "name": "glm-5.3-flash",
      "model": "glm-5.3-flash",
      "match": "contains",
      "pipeline": "auto",
      "mode": "strict",
      "upstreams": ["relace"]
    },
    {
      "name": "glm-prefer-fast",
      "model": "glm-5.3",
      "match": "contains",
      "pipeline": "auto",
      "mode": "preferred",
      "upstreams": ["friendli", "z-ai", "gmicloud"]
    }
  ]
}
```

⚠️ **一旦文件里出现 `rules`，内置默认规则表就被整体替换**（而不是合并）。
所以示例里把三条都写全了。**不要把 `glm-5.3-flash` 和 `glm-5.3` 合并成一条
泛化的 `glm`**：前者走 direct 管道、后者走 planner，两条管道的渠道池不通用，
一条规则同时匹配两者时，总有一侧会被钉到它没有的渠道上。
`rules: []` 是合法的——表示显式关闭钉死，全部交给 Cline Pass 自主路由。

### 字段说明

**规则（`rules[]`）**

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

**进程级**

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8787` | 监听地址。**只在启动时读取一次**，改它需要重启 |
| `upstream` | `https://api.cline.bot/api/v1` | Cline Pass 基址 |
| `api_key` | 空 | 固定上游密钥；留空则透传调用方的 `Authorization` |
| `forward_headers` | `["x-client-type"]` | 白名单外额外透传给上游的请求头 |
| `probe_headers` | `{"x-client-type":"cline-cli"}` | `probe` 子命令附带的请求头 |
| `max_body_bytes` | 67108864 | 请求体上限 |
| `watch_seconds` | `5` | 配置热重载轮询间隔；`0` 关闭热重载。**启动时读取一次** |
| `admin_token` | 空 | 管理 API 令牌；留空且未开 `admin_allow_unauthenticated` 时管理 API 整体不可见 |
| `admin_allow_unauthenticated` | `false` | 显式允许无令牌访问管理 API（仅限完全可信的本机环境） |

`listen` 与 `watch_seconds` **只在启动时读取一次**（前者无法在不中断连接的前提下重新绑定，
后者要改的是轮询循环本身）；**其余字段全部参与热重载**，包括 `admin_token` ——
管理接口在每个请求上按当前配置判定，所以换令牌不需要重启。

### 热重载

改配置文件即生效，**不需要重启进程，也不需要重建容器**：

```bash
# 直接编辑挂载出来的文件（文件属主是容器用户时用 sudo tee，别用会改属主的编辑器）
sudo vim data/config.json
```

进程按 `watch_seconds` 轮询文件修改时间，发现变化就重新解析并原子替换生效配置。
日志会打出 `config reloaded`：

```json
{"level":"INFO","msg":"config reloaded","path":"/etc/cline-pin-proxy/config.json","rules":4}
```

两个安全约定：

- **解析失败不会导致中断**。坏配置被拒绝，**上一份好配置继续生效**，同时打 WARN 日志。
  同一个错误只告警一次（否则每 5 秒一条会在几分钟内刷满日志），文件修好后打一条
  `config reload recovered` 并自动接管。
- **文件被删除也不会中断**。配置回落到上次成功的值，等文件重新出现后再接管。

> 为什么值得做这个：`docker compose up -d` 察觉不到挂载文件的内容变化，不会重建容器，
> 改完配置毫无反应，是个很容易误判成「配置没写对」的坑。`restart` 其实能生效
> （重新读一次文件），但要中断一次服务；对「换个 slug 试试速度」这种高频操作，
> 热重载几乎是无成本的。真正**只有热重载能救**的场景，是下面这种环境变量遮蔽 ——
> 见[环境变量与配置文件的优先级](#环境变量与配置文件的优先级)。

### 管理 API

用 `admin_token` 打开（或显式 `admin_allow_unauthenticated: true`）。
**未配置令牌时整组路由返回 404**，不是 403 —— 不向扫描者暴露「这里有个管理面」。

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/admin/config` | 查看当前生效配置（**自动隐去 `api_key` 与 `admin_token`**） |
| `GET` | `/admin/rules` | 查看规则 |
| `PUT` | `/admin/rules` | 替换规则：立即生效，并尽力写回配置文件 |
| `POST` | `/admin/probe` | 探测某模型可用上游：`{"model":"...","pipeline":"auto"}` |
| `POST` | `/admin/reload` | 强制重新读取配置文件 |

认证方式二选一：`Authorization: Bearer <token>` 或 `X-Admin-Token: <token>`。

```bash
TOKEN=$(python3 -c 'import json;print(json.load(open("data/config.json"))["admin_token"])')

# 看看现在钉的是什么
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8787/admin/rules | jq

# 热更新规则（不需要重启）。裸数组和 {"rules":[...]} 两种写法都收。
curl -s -X PUT -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '[{"name":"glm","model":"glm-5.3","match":"contains","mode":"strict","upstreams":["friendli"]}]' \
     http://127.0.0.1:8787/admin/rules
# {"ok":true,"applied":true,"persisted":true,"rules":[{"name":"glm",...}]}
# 「规则」字段是生效后的完整规则数组，不是数量。

# 加个新模型前先看看有哪些渠道
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"model":"cline-pass/glm-5.3"}' http://127.0.0.1:8787/admin/probe | jq
```

`PUT /admin/rules` 的响应字段：

- `applied` —— 是否已在本进程生效（只要规则合法就是 `true`，与落盘无关）。
- `persisted` —— 是否成功写回配置文件。**写回失败不影响生效**，
  响应会同时给出 `persist_error` 与 `hint`，规则最迟在下一次重启后消失。

> **Docker 下写回失败是正常现象，不是 bug**。容器以 nonroot（uid 65532）运行，
> 而 compose 建出来的 `./data` 属于 root，原子替换需要**目录**可写。
> 代理会自动退化为原地覆盖并打 WARN；若宿主目录同样不可写，就只生效不落盘。
> 想让它落盘：`sudo chown -R 65532:65532 data`。
> 也可以干脆不用 API —— 直接编辑 `data/config.json`，热重载等价且更可审计。

### 环境变量

| 变量 | 说明 |
|---|---|
| `CLINE_PIN_LISTEN` | 监听地址，默认 `127.0.0.1:8787` |
| `CLINE_PIN_UPSTREAM` | Cline Pass 基址，默认 `https://api.cline.bot/api/v1` |
| `CLINE_PIN_API_KEY` | 固定上游密钥；留空则透传调用方的 `Authorization` |
| `CLINE_PIN_FORWARD_HEADERS` | 额外透传的请求头，逗号分隔 |
| `CLINE_PIN_PROBE_HEADERS` | `probe` 附带的请求头，`name: value` 逗号分隔 |
| `CLINE_PIN_MAX_BODY_BYTES` | 请求体上限，默认 64 MiB |
| `CLINE_PIN_RULES` | 规则表 JSON，整体覆盖配置文件 |
| `CLINE_PIN_WATCH_SECONDS` | 热重载轮询间隔秒数；`0` 关闭 |
| `CLINE_PIN_ADMIN_TOKEN` | 管理 API 令牌 |
| `CLINE_PIN_ADMIN_ALLOW_UNAUTHENTICATED` | `true` 时允许无令牌访问管理 API |
| `CLINE_PIN_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |

> 配置里的 `api_key`、`admin_token` 属敏感字段，**环境变量的值不会被写回配置文件** ——
> 通过 `PUT /admin/rules` 落盘时只替换 `rules` 键，其余内容原样保留。

### 环境变量与配置文件的优先级

**环境变量 > 配置文件 > 内置默认值**。这条规则的副作用值得单独说清楚：

> 只要环境变量**非空**，它就赢，配置文件里同名字段改什么都不会生效 ——
> 热重载也一样，因为热重载只重新读文件，然后又被环境变量盖回去。

空字符串（或纯空白）等同于"没设置"，不参与覆盖。所以想让配置文件说了算，
就不要给那个变量赋值。

Docker 部署尤其要注意这点：compose 里写 `${VAR:-某个默认值}` 会**无条件**把
默认值注入容器，于是配置文件里的 `upstream` / `forward_headers` 永远被遮蔽。
本项目的 `docker-compose.yml` 因此统一用空默认值 `${VAR:-}`，把默认值留给应用本身。

**显式给出的非法值会让启动失败**，不会被静默忽略：把 `CLINE_PIN_RULES='[{bad'`
当成"没配置"，会让运维者以为规则表已经覆盖了，实际继续走默认渠道。

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

严格说这**不是无条件免费**：它依赖"网关在路由层就拦下"。对会执行该过滤器的模型，
请求根本到不了推理后端（不产生 token）；但实测也发现过个别模型忽略过滤器、
照常返回正文的情况（见 [docs/VERIFICATION.md](docs/VERIFICATION.md) 第二节），
那种情况会正常计费。想完全确定，先看返回里有没有可用清单。

两条管道的错误格式不同，代理会分别解析；`-pipeline` 可强制指定以排查管道归属。

### 两个必须知道的局限

**① 清单不保证穷尽。** 实测 `deepseek/deepseek-v4-flash` 的清单列了 26 个上游、
**不含 `deepseek`**，但钉到 `deepseek` 却成功。要确认某个 slug 真的可用，
必须发一次钉住它的真实请求，再读响应里的 `finalProvider` / `provider`。

**② 部分模型要求调用方身份头。** `deepseek/...` 这类规范名缺少
`x-client-type: cline-cli` 会直接 403 —— 这会让探测得出与线上相反的结论。
`probe` 默认已带上该头（可用 `probe_headers` 配置或 `-H` 覆盖）：

```bash
cline-pin-proxy probe -model deepseek/deepseek-v4-flash -H "x-client-type: cline-cli"
```

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

- **只注入 `POST /v1/chat/completions`**（含 `/chat/completions`、`/api/v1/chat/completions`
  三种写法，都会归一化成上游基址下的同一路径）。
  其余端点纯净透传——调用方靠它们探测上游能力，代理不该干扰结论。
- **未命中规则时完全原样转发**，由 Cline Pass 自主路由。不会偷偷钉任何东西。
- **注入失败时降级为未钉死透传**，并在 `X-Cline-Pin-Note` 里注明，避免"以为钉住了"。
- **上游状态码与响应体原样回传**，调用方的故障转移逻辑才不会失灵。
  因此**不跟随重定向**：30x 连同 `Location` 原样返回，而不是替调用方把 302 跟成 200。
- **上游中途断开时主动断连**。响应头一旦发出就无法再改成 5xx，如果只是安静返回，
  下游会把残缺内容当成"正常结束"（`Content-Length` 不透传，HTTP 层没有失败信号）。
  代理会以 `http.ErrAbortHandler` 断开连接，客户端因此拿到 `unexpected EOF`。
- **流式响应逐块 Flush**，全程 O(1) 内存，不做任何 JSON 解析。
  这是首字延迟不退化的前提。
- **不设 `http.Client.Timeout`**：流式生成可能持续数分钟，整体超时会把长回答砍断。
  超时改由 dial / TLS / 响应头三段分别控制。
- 注入采用「解析 → 深度合并 → 重新编码」，数字用 `json.Number` 承载、
  且关闭 HTML 转义，保证除注入字段外请求体语义完全不变（含大整数精度与 `< > &` 原样保留）。
- **代理是路由字段的唯一决定者**。在它写入的那条管道上，`only` / `order` /
  `allow_fallbacks` / `sort` 以配置为准：`preferred` 会清掉调用方自带的 `only`
  与 `allow_fallbacks=false`，否则多候选会静默退化成"只用第一个"。
  其它字段（包括 `require_parameters`、`data_collection` 等）一律不动。
- **一次请求只用一份配置快照**。热重载在请求处理中途生效也不会让这次请求
  混用新旧配置（旧上游 + 新密钥）。

---

## 安全

- 默认**只监听 `127.0.0.1`**。代理通常与调用方同机，不需要对外暴露。
  容器内需监听 `0.0.0.0`，但 `docker-compose.yml` 把宿主机端口绑定限制在回环。
- 请求头**白名单透传**，白名单外的一律不外泄到上游。
- 响应头同样白名单，且刻意不转发 `Content-Length`（注入会改变长度）。
- 路径白名单限制在 `/v1/` 与 `/api/v1/`，**并拒绝任何百分号转义与点段**：
  `/v1/%2e%2e/%2e%2e/admin` 这类编码穿越在 Go 1.22+ 上不会被 ServeMux 规范化，
  拿去拼上游 URL 就能跳出 API 前缀。支持的 OpenAI 端点路径不含需要转义的字符，
  所以直接拒绝比猜测安全。
- 请求体有大小上限，超限返回 413 而不是把内存读满；**透传端点同样执行**
  （已知长度直接拒，chunked 靠 `MaxBytesReader` 在读取中拦截）。
- **管理 API 默认整体关闭**（未设 `admin_token` 时返回 404 而非 403），
  令牌比较用 `crypto/subtle` 常量时间实现；`GET /admin/config` 会隐去
  `api_key` 与 `admin_token`，避免把密钥回显给调用方。
- 写回配置**只在确认"原子替换做不到"**（目录不可写、只读挂载）时才退化为
  原地覆盖。磁盘满、I/O 错误这类内容写失败会直接报错，不会去截断唯一的配置文件。
- 配置里的 `api_key` 是明文。别把 `config.json` / `data/` 提交进 git（`.gitignore` 已排除）。

---

## 开发

```bash
go test ./...              # 全部单测
go test -race ./...        # 需要 cgo（例如 Linux CI）
go vet ./...
gofmt -l .                 # 应为空
go test -cover ./...
```

覆盖率：`pin` 94.5% / `admin` 94.9% / `probe` 92.0% / `config` 88.6% / `proxy` 84.7% / `cmd` 22.6%
（`config` 未覆盖的主要是 `Sync`/`Close` 失败这类需要故障注入才走得到的分支；
`cmd` 只覆盖了子命令分发与 `check`/`healthcheck`，`serve` 的启动路径要真实起服务，未纳入）。

CI 在每次 push 与 PR 上跑 `gofmt` + `vet` + `test -race`；
`Release` 工作流**自己也带一道同样的验证门禁**（单测没过就不会推镜像），
通过后构建 `linux/amd64`、`linux/arm64` 多架构镜像推到 GHCR，
打 tag 时额外附带 5 个平台的裸二进制。

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
