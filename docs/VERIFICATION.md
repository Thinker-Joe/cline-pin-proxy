# 实测记录

本文整理 2026-09-16 与 2026-09-20 对 `api.cline.bot`、发布镜像和 sub2api 集成的测试记录。数据反映当时的账号、网关和配置，**不保证当前仍然适用**。2026-09-16 的记录未在整理时重新请求网关；2026-09-20 的复查（上游过滤时效性、模型 ID 命名与管道归属）见第 11 节，其中包含对前文若干结论的时效性修正。

当前默认配置见 [README](../README.zh-CN.md#默认规则)。下文早期测试使用 GLM 官方上游 `zai` / `z-ai`，后续测速才将默认值改为 `friendli` / `relace`。规则注入成功、请求成功和上游实际采用指定服务商是三种不同的证据，需分别确认。

复现需要有效的 Cline Pass 凭据。本文不含凭据；探测遇到忽略过滤条件的模型时也可能产生费用。

## 1. 模型、管道与上游标识

支持范围包含非 `cline-pass/` 模型 ID。下文记录了 `deepseek/deepseek-v4-flash` 的实际服务商切换，以及 `z-ai/glm-5.3-flash` 的 direct 探测和集成结果；部分带 `cline-pass/` 前缀的模型反而忽略过滤。支持与否需按具体模型确认，不能从前缀推导。

重复探测得到以下结果。单次无路由信息的响应不足以确定管道。

| 模型 | 当时观察到的管道或行为 | 报告的上游数 | 官方上游标识 |
|---|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | planner | 16 | `deepseek` |
| `cline-pass/deepseek-v4-flash` | 私有渠道，忽略过滤 | — | — |
| `cline-pass/deepseek-v4-flash-vision-exp` | 后续请求返回 404，未确认管道 | — | — |
| `cline-pass/deepseek-v4-pro` | 私有渠道，忽略过滤 | — | — |
| `cline-pass/glm-5.3` | planner | 18 | `zai` |
| `cline-pass/glm-5.3-flash` | direct | 27 | `z-ai` |
| `cline-pass/kimi-k3` | 观察到私有渠道 | — | — |
| `z-ai/glm-5.3-flash` | direct | 27 | `z-ai` |

planner 读取 `providerOptions.gateway.only`，direct 读取顶层 `provider.only`。默认 `auto` 同时写入两组字段。

> **2026-09-20 复查**：上表中 `cline-pass/deepseek-v4.1-flash` 的"支持过滤"结论已失效——该模型对 `providerOptions.gateway` 的 `only` / `order` / `sort` 均不再响应；能继续用过滤的 deepseek 模型是 `deepseek/deepseek-v4-flash`。另外带 `cline-pass/` 前缀与不带的同一 slug 默认上游不同。详见第 11 节。
>
> **2026-10-17 复查（可用性）**：上表与后续章节里的 `cline-pass/deepseek-v4-flash` 已返回 404（连同 `cline-pass/deepseek-v4-flash-vision-exp`），`cline-pass/glm-5.3` 改由私有贡献池服务且忽略过滤；当日订阅清单与逐模型矩阵见第 12.6 节，`deepseek-v4.1-flash` 的钉住路径见第 12.7 节。历史记录保留原日期与语境，不代表当前可用。

### 同一服务商在不同管道中的标识

`glm-5.3` 的官方上游为 `zai`，`glm-5.3-flash` 为 `z-ai`。早期宽泛的 `glm → z-ai` 规则会使 `glm-5.3` 返回：

```text
400 No available providers match the 'only' filter: z-ai.
    Available providers are: ..., wafer, zai
```

这两个标识不能互换，修改规则前需对具体模型重新验证。

### DeepSeek 模型的过滤支持

测试注入 `only: ["__probe__"]`。路由层拒绝并返回候选列表，说明该请求采用了过滤条件；仍返回正文则说明过滤未生效。

| 模型 | 结果 | 判断 |
|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | 500，路由错误 | 支持过滤 |
| `deepseek/deepseek-v4-flash` | 500，路由错误 | 支持过滤 |
| `cline-pass/deepseek-v4-flash` | 200，正文 `OK` | 忽略过滤 |
| `cline-pass/deepseek-v4-pro` | 200，正文 `OK` | 忽略过滤 |
| `cline-pass/deepseek-v4-flash-vision-exp` | 404 `model not found` | 当时不可用 |

在这组模型中，前两项确认支持上游选择。默认 `deepseek` 规则也会匹配忽略过滤的模型，但不能改变它们的路由。原始表曾把 vision-exp 归为私有渠道；404 结果不支持该归类，现改为未确认。

> **2026-09-20 复查**：以 `only: ["__probe__"]` 判断"是否支持过滤"的方法本身没有变，但 `cline-pass/deepseek-v4.1-flash` 的结论已反转（当日 01:31 仍返回路由错误，12:30 前后改为返回正文/`empty response content`，即过滤被丢弃）。复查方法与局限见第 11 节。

### 探测列表的局限

`deepseek/deepseek-v4-flash` 的错误响应列出 26 个上游，未包含 `deepseek`，但真实补全确认它可用：

| 请求设置 | 响应 `provider` |
|---|---|
| 不指定上游 | `Relace` |
| 指定 `deepseek` | `DeepSeek` |
| 指定 `novita` | `Novita` |

因此，探测列表不一定完整。确认实际路由需读取 `finalProvider`（planner）或 `provider`（direct）。

该模型缺少 `x-client-type: cline-cli` 时返回：

```text
403 deepseek/deepseek-v4-flash is only available via Cline product surfaces.
```

探测默认通过 `probe_headers` 添加该头，可用 `-H` 覆盖。普通代理请求只通过 `forward_headers` 转发客户端已有的头，不自动添加其取值。

### 当时返回的完整列表

`cline-pass/deepseek-v4.1-flash`，16 项：

```text
alibaba, baseten, boundless, deepinfra, deepseek, fireworks, gmicloud, modal,
morph, novita, parasail, particle, relace, runware, togetherai, wafer
```

`cline-pass/glm-5.3`，18 项：

```text
baseten, blackbox, crusoe, deepinfra, digitalocean, fireworks, friendli,
gmicloud, inceptron, modal, morph, novita, parasail, runware, streamlake,
togetherai, wafer, zai
```

`cline-pass/glm-5.3-flash`，27 项，与响应 `endpoint_count` 一致：

```text
deepinfra, relace, morph, wafer, streamlake, gmicloud, novita, makora, crusoe,
coreweave, sail-research, atlas-cloud, fireworks, phala, friendli, siliconflow,
digitalocean, together, parasail, baseten, venice, io-net, cloudflare, z-ai,
reka, nextbit, modal
```

## 2. 上游选择的对照测试

直接请求 `https://api.cline.bot/api/v1/chat/completions`，使用 `max_tokens=1024`，提示词为 `Reply with exactly: OK`。

| 实验 | 注入 | HTTP | `finalProvider` | `fallbacksAvailable` | 尝试次数 | 成本 |
|---|---|---|---|---|---|---|
| A | `only: ["deepseek"]` | 200 | `deepseek` | `[]` | 1 | $0.0000143 |
| B | 无 | 200 | `novita` | 15 项 | 4，alibaba 曾返回 503 | $0.0000309 |
| C | `only: ["novita"]` | 200 | `novita` | `[]` | 1 | $0.0000297 |

A 与 C 的实际服务商符合指定值，且候选回退列表为空。B 使用网关自动路由，发生了多次尝试。A 的单次成本约为 B 的一半；这三次请求不足以推导长期成本差异。

路由信息读取位置为：

```text
data.choices[0].message.provider_metadata.gateway.routing
```

还原响应包装后，去掉路径开头的 `data`。

## 3. 代理与发布镜像

早期规则使用 GLM 官方上游时，经本代理得到以下结果：

| 请求 | HTTP | `X-Cline-Pin-Rule` | 实际路由 | 正文 |
|---|---|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | 200 | `deepseek` | `finalProvider=deepseek`，无回退 | `OK` |
| `cline-pass/glm-5.3-flash` | 200 | `glm` | `provider=Z.AI`，无回退 | `OK` |
| `cline-pass/kimi-k3` | 200 | `none`，`Note: no rule matched` | `openai-compatible-private` | `OK` |
| `GET /v1/models` | 200 | — | 返回 446 个模型 | — |

硅谷服务器匿名拉取 GHCR 镜像后，使用 `--read-only`、`--tmpfs /tmp` 和回环端口映射运行临时容器，验证结果如下：

| 检查 | 结果 |
|---|---|
| 容器状态 | `status=running health=healthy restarts=0` |
| 只读根文件系统 | 正常运行 |
| `/healthz` | `{"status":"ok"}` |
| DeepSeek v4.1 flash | 200，`finalProvider=deepseek`，无回退，正文 `OK` |
| GLM 5.3 flash | 200，`provider=Z.AI`，无回退，正文 `OK` |
| 无匹配规则 | 返回 `none` 和 `no rule matched`，请求体不注入 |
| 镜像 manifest | 包含 `linux/amd64`、`linux/arm64`；未在 arm64 上运行 |

## 4. sub2api 集成

代理与 sub2api 位于同一 Docker 网络 `shared-egress-net`。测试账号 268、301 的 Base URL 为 `http://cline-pin-proxy:8787/v1`。

通过 `POST /api/v1/admin/accounts/:id/test`，传入 `model_id` 测试账号模型映射。表中的“注入”只表示代理写入的值。

| 账号 | 请求 model_id | 映射后的模型 | 注入 | 结果 |
|---|---|---|---|---|
| 268 | `deepseek-flash-LJ` | `cline-pass/deepseek-v4.1-flash` | `deepseek` | 通过 |
| 268 | `deepseek-v4-flash` | `cline-pass/deepseek-v4-flash` | `deepseek` | 通过 |
| 268 | `deepseek-v4-flash-pass` | `cline-pass/deepseek-v4-flash` | `deepseek` | 通过 |
| 268 | `deepseek-v4-pro` | `cline-pass/deepseek-v4-pro` | `deepseek` | 通过 |
| 268 | `glm-5.3` | `cline-pass/glm-5.3` | `zai` | 通过 |
| 268 | `glm-5.3-flash-solw` | `cline-pass/glm-5.3-flash` | `z-ai` | 通过 |
| 268 | `kimi-k3` | `cline-pass/kimi-k3` | 无 | 通过 |
| 301 | `deepseek-v4-flash` | `deepseek/deepseek-v4-flash` | `deepseek` | 通过 |
| 301 | `glm-5.3-flash` | `z-ai/glm-5.3-flash` | `z-ai` | 通过 |

九项请求均成功。`glm-5.3` 从错误的 `z-ai` 改为 `zai` 后，由 400 变为成功。另行读取路由信息，结果如下：

| 模型 | 注入 | 实际路由 | 回退列表 |
|---|---|---|---|
| `cline-pass/glm-5.3` | `zai` | `finalProvider=zai` | `[]` |
| `cline-pass/glm-5.3-flash` | `z-ai` | `provider=Z.AI` | `[]` |
| `cline-pass/deepseek-v4.1-flash` | `deepseek` | `finalProvider=deepseek` | `[]` |
| `cline-pass/deepseek-v4-flash` | `deepseek` | `openai-compatible-private`，过滤被忽略 | `[]` |
| `cline-pass/kimi-k3` | 无 | `openai-compatible-private` | `[]` |

## 5. 真实请求发现的缺陷

以下缺陷在早期测试中未覆盖，之后已修复并补充回归测试。

| 缺陷 | 真实响应或触发条件 | 修复 |
|---|---|---|
| direct 探测解析失败 | 外层 `error` 是字符串，内嵌 JSON 的引号被转义 | 先还原转义，再提取 `available_providers` 数组 |
| planner 列表缺少最后一项 | 正则在模型名 `v4.1` 的句点处截断，`wafer` 与 JSON 尾部混合 | 按逗号读取合法标识，遇非法项停止；返回数量从 15 修正为 16 |
| 上游路径重复版本段 | 基址已有 `/api/v1`，请求又带 `/v1`，拼成 `/api/v1/v1/chat/completions` | 拼接时去除重复版本前缀 |

早期验证记录的总成本远低于 $0.01，覆盖多次探测和三轮完整补全。该数字不代表后续所有测速与集成测试的总成本，也不构成免费探测承诺。

## 6. GLM 上游测速

逐个指定候选上游，使用 `only: [<上游>]`、`stream: true`、`max_tokens: 128` 串行测量，每项运行 1–4 次。原记录称指标为 TTFT，实际取流式响应首字节到达时间，未区分首个事件与首个内容 token。

### glm-5.3，planner

| 上游 | 首字节延迟 | 结果或备注 |
|---|---|---|
| friendli | 0.31 / 0.33 / 0.34 / 0.35s | 4/4 成功 |
| deepinfra | 0.31 / 0.44 / 0.45 / 0.48s | 4/4 成功 |
| togetherai | 0.36 / 0.36 / 0.40 / 0.54s | 4/4 成功 |
| digitalocean | 0.50 / 0.55 / 0.59 / 0.63s | 4/4 成功 |
| runware | 0.67s | |
| parasail | 0.76 / 0.92s | |
| inceptron | 0.88 / 0.77s | |
| baseten | 1.13 / 1.38s | |
| gmicloud | 1.65 / 1.67s | |
| blackbox | 2.14 / 3.21s | |
| fireworks | 2.34 / 0.67s | 样本波动较大 |
| streamlake | 2.71 / 1.77s | |
| zai（官方） | 3.11 / 2.04s | 官方上游对照 |
| novita | 5.48 / 3.83s | |
| wafer | 5.64 / 0.69s | 样本波动较大 |
| crusoe | 8.63s / 超时 | |
| morph | 1.36 / 7.64s | 样本波动较大 |
| modal | 超时 / 0.81s | |

当时选择 `friendli`：四次均成功，延迟范围较窄。官方上游的两个样本为 2.04–3.11 秒。小样本不能证明长期稳定性。

### glm-5.3-flash，direct

| 上游 | 首字节延迟 | 结果或备注 |
|---|---|---|
| relace | 0.72 / 0.76 / 0.80 / 0.97s | 4/4 成功 |
| cloudflare | 0.62 / 0.77 / 0.96 / 1.14s | 4/4 成功 |
| parasail | 失败 / 0.66 / 0.70 / 1.02s | 3/4 成功 |
| friendli | 0.93 → 1.93 → 2.73 → 3.05s | 4/4 成功，按测试顺序延迟上升 |
| fireworks | 三次失败 / 0.74s | 1/4 成功 |
| streamlake | 1.26 / 1.38s | |
| siliconflow | 0.95 / 1.81s | |
| sail-research | 1.07 / 0.88s | |
| phala | 1.09 / 0.34s | 样本波动较大 |
| wafer | 1.37 / 0.81s | |
| nextbit | 1.76 / 1.38s | |
| z-ai（官方） | 1.77 / 2.16s | 官方上游对照 |
| gmicloud | 2.11 / 1.53s | |
| digitalocean | 2.11 / 0.38s | 样本波动较大 |
| deepinfra | 8.26 / 0.72s | 样本波动较大 |
| reka | 8.05 / 0.24s | 样本波动较大 |
| venice | 失败 / 4.49s | |
| morph / novita / makora / crusoe / coreweave / together / baseten / io-net / modal | `stream_initialization_failed` | 当时严格模式请求均失败 |

当时选择 `relace`：四次均成功，延迟为 0.72–0.97 秒。官方上游的两个样本为 1.77–2.16 秒。探测列表中的 `atlas-cloud` 没有单独的测速记录，因此不能称本表覆盖了全部 27 个候选。

两条管道的列表有重叠，也有不同项；同名上游在两个模型上的表现不能互相推导。`relace`、`cloudflare` 只出现在本次 flash 列表中。测试未比较量化方式或输出质量。

### 排序参数

另行测试了直接发往网关的排序字段：

| 模型 | 注入 | 三次实际路由 | 首字节延迟 |
|---|---|---|---|
| glm-5.3 | `providerOptions.gateway.sort = ttft` | digitalocean ×3 | 0.60 / 0.60 / 0.71s |
| glm-5.3 | `providerOptions.gateway.sort = tps` | friendli ×3 | 0.42 / 0.32 / 0.38s |
| glm-5.3-flash | `provider.sort = latency` | NextBit / Parasail / Relace | 1.34 / 0.65 / 0.70s |
| glm-5.3-flash | `provider.sort = throughput` | Parasail / Parasail / Relace | 0.82 / 1.80 / 0.74s |

这些结果记录了排序请求的路由表现。当前代理规则仍要求 `upstreams` 和 `mode`；给 `strict` 规则添加 `sort` 会同时保留 `only`，不能将其视为只排序、允许任意回退的配置。

## 7. 热重载与管理 API

测试在硅谷 Linux 服务器的 Compose 部署进行，配置目录挂载为 `./data:/etc/cline-pin-proxy`，使用 `watch_seconds=5`，管理 API 要求令牌。历史镜像属于 v0.3，摘要前缀为 `d8f7302d04a4`。

### 重载与错误恢复

进程 PID 为 `3284619`。宿主机文件中增加第四条规则后，约九秒内规则数从 3 变为 4，PID 不变。随后经历非法文件、恢复文件、API 写入和恢复规则，PID 均未变化。

| 场景 | 结果 |
|---|---|
| 写入 `{ this is not json` | 保留原有四条规则 |
| 非法文件期间访问 `/healthz` | 200 |
| 22 秒内的相同 WARN 数量 | 1 |
| 修复文件 | 记录 `config reload recovered`，加载三条规则 |

### 认证、校验与写入

| 请求或检查 | 结果 |
|---|---|
| 已设令牌，无认证读取规则 | 401 |
| 错误令牌 | 401 |
| Bearer 或 `X-Admin-Token` 正确 | 200 |
| `DELETE /admin/rules` | 405，`Allow: GET, PUT` |
| strict 规则缺少 `upstreams` | 400 |
| 未知字段、畸形 JSON、对象缺少 `rules`、尾部多余数据 | 400 |
| 未设令牌且不允许匿名 | 404，此项为单测结果 |
| `GET /admin/config` | 当时响应为 617 字节，不含 `api_key`、`admin_token`，含 `admin_auth_required: true` |
| PUT 四条规则，再恢复三条 | 两次均 `applied=true persisted=true` |
| 文件状态 | 本次文件保持权限 `0600`、属主 `65532:65532` |
| 其他配置 | `listen`、`upstream`、`watch_seconds`、`admin_token`、`api_key` 保留 |

属主结果只描述本次环境。原子替换的实现保留权限位，新文件属主由写入进程决定，不能推导为保留任意原属主。

sub2api 复验中，账号 268 的 GLM 5.3、GLM 5.3 flash、DeepSeek v4.1 flash，以及账号 301 的 GLM 5.3 flash、DeepSeek v4.1 flash 均返回成功。同期代理日志统计如下：

```text
20  cline-pass/deepseek-v4.1-flash -> deepseek        (deepseek)
10  cline-pass/deepseek-v4-flash   -> deepseek        (deepseek)
 4  cline-pass/glm-5.3-flash       -> glm-5.3-flash   (relace)
 2  cline-pass/glm-5.3             -> glm-5.3         (friendli)
```

未记录无匹配或注入失败。日志证明流量经过代理并完成注入，不能单独证明网关采纳了指定上游。

### 本轮修复

- `PUT /admin/rules` 原来只接受 `{"rules":[...]}`，与文档的裸数组示例不符。修复后两种形式均支持，并拒绝尾部多余数据。
- 非法配置原来每五秒告警一次，记录过 22 秒四条、25 秒五条。修复后相同错误只告警一次，恢复时另记日志。
- 原子替换原来固定使用 `0644`，可能放宽已有 `0600` 文件的权限。修复后沿用原权限位。

文件缺失时，服务可以用默认值和环境设置启动；文件存在但非法时启动失败。运行期遇到缺失或非法文件，保留上次有效配置。因此，运行中的服务能继续工作，不代表此时重启一定成功。

## 8. 代码审查后的复验

[代码审查](CODE_REVIEW.md) 基线为 `5c475b0`，记录了 18 项问题。此处保留修复后的测试结果。

### Linux 编译与测试

修复原子替换错误分类时，曾在 switch 中同时列出 `syscall.ENOTSUP` 和 `syscall.EOPNOTSUPP`。Windows 检查通过，但 Linux 上二者同为 95，编译失败：

```text
duplicate case syscall.EOPNOTSUPP (constant 95 of uintptr type syscall.Errno)
```

Release 的验证任务阻止了此次镜像与二进制发布。修复改用 `errors.ErrUnsupported`，并增加 `scripts/linux-check.sh`。

后续在 `go1.25.14 linux/amd64` 上验证：`gofmt -l .` 无输出，`go vet ./...`、`go build ./...`、`go test -race ./...` 均通过，覆盖六个 Go 包。

### 路由与 HTTP 行为

在镜像 `8270f7f9` 的部署上，下列请求均返回 404：

```text
/v1/%2e%2e/%2e%2e/admin/private
/v1/%2e%2e/admin
/api/v1/%2e%2e/%2e%2e/admin
/v1/..%2f..%2fadmin
```

`GET /api/v1/models` 返回 200，不再重复拼接版本前缀。本地模拟上游和临时代理容器验证结果如下：

| 场景 | 结果 |
|---|---|
| `/v1/responses`，1024 字节，已知长度，上限 64 | 413 |
| 同上，chunked 请求 | 413 |
| `/v1/responses`，16 字节 | 200 |
| `/v1/chat/completions` 请求体超限 | 413 |
| 上游返回 302 | 保留 302、`Location: /api/v1/models` 和原始正文 |
| 上游返回 gzip | 保留编码声明，正文为合法 gzip |

该脚本最初报告 14/15 通过。剩余一项的基址不含版本段，脚本却期望代理剥离路径前缀，失败来自错误的测试期望。

管理接口对裸 `null`、`[null]` 返回 400，无认证返回 401，正常读取返回 200，非法请求未修改规则表。文件增加第四条规则后约九秒生效，PID 保持 `3321511`；PUT 恢复三条后，`applied=true persisted=true`，文件权限 `0600`、属主 `65532:65532`。

sub2api 再次测试账号 268 的三个模型和账号 301 的两个模型，五项均通过。同期有 19 条注入日志：DeepSeek v4.1 flash 11 条、GLM 5.3 六条、GLM 5.3 flash 两条，无匹配失败或注入失败。

复验脚本曾把管理令牌用于补全请求，收到上游 401。部署未设置固定上游密钥，管理令牌不能替代 Cline 凭据。之后改用 sub2api 的账号测试接口读取账号中的上游密钥。

## 9. 非流式响应包装

客户端 `vision_analyze` 曾报告无法读取 `data.choices`，因此检查代理接入是否改变了 sub2api 的响应处理。

### sub2api 代码检查

检查对应线上版本 `0.2.5` / `86f93c28e`，本地 checkout 为其直接子提交 `881f32026`，仅增加 VERSION 同步。检索没有发现按 Cline URL 或主机名启用的响应转换。

当时的代码注释说明了用量兼容范围：

> 部分 OpenAI 兼容上游（例如 Cline API）会将标准响应包在 data 字段中：
> `{"data":{"choices": [...], "usage": {...}}, "success":true}`。
> 按优先级先保留原有路径，再尝试兼容层 data 包装，
> **避免同步请求能正常返回但用量被静默记录为 0**。

`openai_gateway_chat_completions_raw.go` 中的 `bufferRawChatCompletions()` 读取用量后，经 `applyOllamaCloudRawChatCompletionsResponse` 处理正文再写出。相关结论限于上述检查版本：

| 处理环节 | 观察 |
|---|---|
| 用量解析 | 支持 `data.usage` |
| 按账号改写请求、非流式响应、SSE | 存在 Ollama Cloud 钩子，按 `ollama.com` / `www.ollama.com` 判断 |
| Cline 正文转换 | 未发现对应钩子 |
| 模型名替换 | `replaceModelInResponseBody()` 只读取顶层 `model`，未处理 `data.model` |
| 其他 `data` 处理 | `grok_observed_models.go`、`openai_images_b64_backfill.go` 对应 models/images，不是补全 |

因此，修改 Cline 账号的 Base URL 不会关闭一段已有的 Cline 正文转换；检查版本中没有这段转换。用量兼容不能使客户端自动读到正文。这里不推测其他项目维护者未实现该功能的原因。

截取的响应如下，省略了路由信息：

```json
{"data":{"choices":[{"finish_reason":"stop","index":0,"logprobs":null,
  "message":{"content":"ok","role":"assistant"}}],
  "created":1789540261,"generationId":"gen_01M2…","id":"gen_01M2…",
  "model":"vmc/k3-contributor-fallbacks","object":"chat.completion",
  "system_fingerprint":"fp_9q1cawe42q",
  "usage":{"completion_tokens":16,"prompt_tokens":103,"total_tokens":119}},
 "success":true}
```

### 直接请求 Cline 的六个模型

早期经网关测试时只看到 `kimi-k3` 返回包装。随后直接请求本代理、固定上游为 Cline，对账号 268 映射的六个模型逐个测试。代理已开启转换，用 `X-Cline-Pin-Unwrapped: data-envelope` 判断原始响应是否有包装。

| 上游模型 | 非流式文本 | 非流式带图 |
|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | `data-envelope` | `data-envelope` |
| `cline-pass/deepseek-v4-flash` | `data-envelope` | Cline 返回 500 |
| `cline-pass/deepseek-v4-pro` | `data-envelope` | `data-envelope` |
| `cline-pass/glm-5.3` | `data-envelope` | Cline 返回 500 |
| `cline-pass/glm-5.3-flash` | `data-envelope` | `data-envelope` |
| `cline-pass/kimi-k3` | `data-envelope` | `data-envelope` |

六个模型的成功非流式文本响应都有包装，因此不能把问题限定为 `kimi-k3`。这组数据也不足以证明所有 Cline 模型、错误响应或未来版本都采用相同格式。带图请求的两次 500 不证明包装存在或不存在。

第一轮使用 `max_tokens: 24`，六个模型中五个返回 500 `empty response content`；提高到 512 后全部成功。原测试将其归因于推理消耗了输出预算，但未单独测量推理 token 分配。排查时需区分预算不足与响应包装。

### 账号路由对结果的影响

网关层的 12 模型矩阵中，只有 `kimi-k3` 返回包装，响应模型为 `vmc/k3-contributor-fallbacks`。其他响应使用各自模型名。随后连续请求四次 `deepseek-flash` 和四次 `glm-5.3-flash`，代理日志没有对应请求，说明这八次请求未经过已接入代理的 Cline 账号。

网关层同时记录了以下格式，不能将它们全部视为直连 Cline 的结果：

| 客户端模型 | 图像 | 流式 | 返回格式 |
|---|---|---|---|
| `kimi-k3` | 无 / 有 | 否 | `data` 包装 |
| `kimi-k3` | 无 / 有 | 是 | 标准 SSE |
| `deepseek-flash` | 无 / 有 | 否 / 是 | 标准 |
| `deepseek-v4.1-flash` | 无 / 有 | 否 / 是 | 标准 |
| `glm-5.3-flash` | 无 / 有 | 否 / 是 | 标准 |

`kimi-k3` 在账号 268 映射为 `cline-pass/kimi-k3`，确认经过 Cline。流式结果支持“这次包装问题发生在非流式路径”，但不能仅凭格式推导所有上游的 SSE 处理机制。

### 开关对照与实现约束

在同一部署中，切换 `unwrap_data_envelope` 并请求 `kimi-k3` 非流式补全：

| 设置 | 顶层 `choices` | `data` 包装 | 客户端 |
|---|---|---|---|
| 开启 | 有 | 无 | 正常 |
| 关闭 | 无 | 有 | 无法读取正文 |
| 恢复开启 | 有 | 无 | 正常 |

此对照确认了代理转换对该请求的作用。当前实现只转换“顶层无 `choices`、`data` 是对象、`data.choices` 为非空数组”的 JSON 响应，转换后重新计算 `Content-Length`。SSE 逐块转发；JSON 缓冲上限为 8 MiB，超限原样转发并标记 `skipped-too-large`。

反例测试覆盖标准响应、模型列表、错误体和数组等输入；流式测试验证上游尚未结束时下游已收到首块。超大 JSON 的后续读取错误存在实现例外，见[当前实现限制](CODE_REVIEW.md#current-implementation-limits)。

### 测试环境中的账号变更

对照当日 04:03 的账号备份，账号 268 的映射从八项改为七项：移除 `deepseek-v4-flash-pass`、`deepseek-v4-flash-vision-exp-pass`，新增 `deepseek-v4.1-flash`。账号 248 被设为 `schedulable=false`。原记录确认这两项均为人工变更。

账号 248 使用 `platform=openai` 和 Base URL `https://opencode.ai/zen/go`，未启用 sub2api 的 `opencode_go` 专用平台。该平台控制协议选择、模型列表和额度窗口，不包含 Cline 响应转换。停用该账号后，相关模型更可能被调度至 Cline，因此暴露包装问题。`base_url` 与 `model_mapping` 是独立字段，修改前者不会修改后者。

## 10. 验证边界

- 未在 arm64 设备上运行，只检查了镜像 manifest 和拉取。
- GLM 测速已覆盖真实流式请求；原文“没有真实流式测试”的说明已过时。未完成所有模型、客户端和长时间流式响应的完整矩阵。
- 未在 `vision_analyze` 所在客户端确认其实际模型名与完整路由，不能把该客户端的所有错误归因于 Cline。
- 未在线上用超过 8 MiB 的真实响应验证转换回退，现有完整性证据来自单测。
- 探测依赖 `from Vercel`、`from Openrouter`、`available_providers`、`Available providers are` 等错误文本。格式变化时可能无法解析，命令会返回原始片段供排查。
- 上游列表、延迟和模型可用性都可能变化。新结论应附测试日期、请求设置和实际路由信息，不以旧记录替代复测。

## 11. 2026-09-20 复查：上游过滤时效性、模型 ID 命名与管道归属

本节为当日复查，起因是外部报告称 `providerOptions.gateway.only` 已失效（linux.do t/topic/2925265）。复测确认该报告**只对部分模型成立**，并纠正了若干容易误用的命名与管道假设。**本节结论只对当日的账号、网关与请求设置成立。**

### 测试设置

- 环境：硅谷服务器直连 `https://api.cline.bot/api/v1`；账号 268（当日更换 API key），请求头 `x-client-type: cline-cli`，`max_tokens: 512`，`stream: false`。
  `max_tokens: 64` 在推理模型上会返回 `{"error":"empty response content"}`（见第 9 节），该现象与"过滤被丢弃"极易混淆，不能用来判断过滤是否生效。
- 判据：planner 管道读响应 `choices[0].message.provider_metadata.gateway.routing` 的 `finalProvider` / `fallbacksAvailable` / `planningReasoning`；direct 管道读顶层 `provider`。
- 方法：**不能用"指定 A 得到 A"下结论**——若网关忽略过滤并恒定返回 A，结果完全相同。本节一律做多点反证（指定 A/B/C 应分别得到 A/B/C），并补一个否定用例（指定不服务该模型的上游应当报错）。过滤被采纳时，`planningReasoning` 会出现 `Provider set restricted to: <slug>` 且 `fallbacksAvailable` 收窄；被丢弃时这两个特征都消失。

### 11.1 planner 管道：整块字段被丢弃（仅部分模型）

模型 `deepseek/deepseek-v4.1-flash`（`cline-pass/` 前缀形式结果相同），同一 key、同一分钟，仅改注入字段：

| 注入写法 | finalProvider | fallbacksAvailable | planningReasoning |
|---|---|---|---|
| 不注入（基线） | `deepseek` | 15 | `System credentials planned for: deepseek, alibaba, baseten, …` |
| `gateway.only=["togetherai"]` | `deepseek` | 15 | 同上，**无** `Provider set restricted to:` |
| `gateway.only=["deepseek"]` | `deepseek` | 15 | 同上 |
| 双管道 `only=["togetherai"]` | `deepseek` | 15 | 同上 |
| `gateway.order=["togetherai"]` | `deepseek` | 15 | 同上 |
| `gateway.sort="tps"` | `deepseek` | 15 | 同上 |

即对该模型 **`only` / `order` / `sort` 全部无效**，`providerOptions.gateway` 整块未被采用。同一模型在当日 01:31 的测试中还能看到 `Provider set restricted to: deepseek. System credentials planned for: deepseek.`（fallbacks 收窄），12:30 前后已无此特征——**这类能力由 Cline 决定，随时可能开关。**

### 11.2 仍然生效的对照模型

| 模型 | 管道 | 反证结果 |
|---|---|---|
| `z-ai/glm-5.3`（`cline-pass/glm-5.3` 同结果） | planner | `only=["zai"]` → finalProvider `zai`，fallbacks 0，`Provider set restricted to: zai`；经代理注入 `friendli` → `friendli`，fallbacks 0 |
| `z-ai/glm-5.3-flash`（`cline-pass/` 同结果） | direct | 基线 Venice；`only=["z-ai"]` → `Z.AI`；`only=["relace"]` → `Relace`；`only=["venice"]` → `Venice` |
| `deepseek/deepseek-v4-flash` | direct | 基线 Novita；`only=["deepseek"]` → `DeepSeek`；`only=["novita"]` → `Novita`；`only=["baseten"]` → `BaseTen`；`only=["togetherai"]` → 500 `No allowed providers`（否定用例：过滤确实到达路由层，且 strict 不静默回退） |
| `~deepseek/deepseek-v4-flash-latest` | direct | 基线 CoreWeave；`only=["deepseek"]` → `DeepSeek` |

结论：**"能否指定上游"没有全局失效。**同一条 planner 管道里 `z-ai/glm-5.3` 仍然听 `providerOptions.gateway.only`，而 `deepseek/deepseek-v4.1-flash` 不听；direct 管道本次抽查的模型全部仍然生效。

### 11.3 `cline-pass/` 是路由前缀，不是模型名

- 当日 `GET /models` 返回 446 个模型，**不含任何 `cline-pass` 条目**，但 `cline-pass/<已存在的 slug>` 仍可用：`cline-pass/deepseek-v4.1-flash`、`cline-pass/glm-5.3`、`cline-pass/kimi-k3` 均返回 200。
- 编造 slug（`cline-pass/nonexistent-xyz`）与不存在的名字（`cline-pass/deepseek-flash`、`deepseek/deepseek-flash`）返回 404 `model not found`。
- `deepseek-flash` 是 **sub2api 侧的客户端模型名**，不是 Cline 模型；账号 268 的 `model_mapping` 把它映射为 `cline-pass/deepseek-v4.1-flash`。
- **前缀会改变默认上游**：同一模型、同一 key、同一分钟：

| 客户端模型 ID | finalProvider | planning 列表首位 |
|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | `deepseek` | deepseek |
| `deepseek/deepseek-v4.1-flash`（非前缀） | `alibaba` | alibaba |

  由于过滤对这两个 ID 都无效，把映射从前缀形式改成非前缀形式会**静默换掉默认上游且无法用注入拉回**。
- 前缀形式也可能落到私有渠道并忽略过滤：`cline-pass/deepseek-v4-flash` → `vmc/deepseek-v4-flash-contributor-fallbacks`（`openai-compatible-private`）；`cline-pass/kimi-k3` → `vmc/k3-contributor-fallbacks`。

### 11.4 管道归属不能从模型名推断

| 模型 ID | 管道 | 能否指定上游 |
|---|---|---|
| `deepseek/deepseek-v4-flash` | direct | 能 |
| `~deepseek/deepseek-v4-flash-latest` | direct | 能 |
| `deepseek/deepseek-v4-flash-0731` | planner | 不能（基线 baseten；注入 `novita` 后仍是 baseten，fallbacks 17 不变） |
| `deepseek/deepseek-v4.1-flash` | planner | 不能（默认 deepseek） |
| `cline-pass/deepseek-v4-flash` | 私有渠道 | 不能 |

名称相近的 `deepseek-v4-flash` 与 `deepseek-v4-flash-0731` 分属两条管道，**必须逐 ID 实测**，不能按名字或前缀推断。

### 11.5 延迟对照

同机、同分钟、流式首字节，各 3 次：

| 路线 | 首字节 |
|---|---|
| `cline-pass/deepseek-v4.1-flash`（默认 deepseek，注入无效） | 0.55 / 0.55 / 0.59 s |
| `deepseek/deepseek-v4-flash` 钉 `deepseek`（注入有效） | 0.88 / 1.04 / 0.95 s |
| `deepseek/deepseek-v4-flash` 钉 `novita` | 1.32 / 1.57 / 2.26 s |

"能强制"的路线比"默认即最优"的路线慢约 0.3–0.5 秒。单机 3 次采样，不构成 SLA。

### 11.6 同日的权益故障（与过滤无关）

当日 11:45 前后，账号 268/301 的旧 key 对全部 `cline-pass/*` 返回 `403 ENTITLEMENT_ERROR: the user is not subscribed to required model plan`，而同一 key 请求非前缀 ID 仍为 200；更换账号 268 的 key 后 `cline-pass/*` 恢复。这类 403 属账号/权益问题，不能据此推断过滤行为，也不代表该命名空间被全局移除（外部报告者当日仍可用 `cline-pass/deepseek-v4.1-flash`）。

### 11.7 局限

- 结论仅对 2026-09-20 的该账号、该上游与上述请求设置成立；过滤是否生效由 Cline 决定，可随时变化（同一模型当日 01:31 生效、12:30 前后失效）。
- 未验证 `x-client-type` 之外的身份条件、其他账号、其他模型族（非 deepseek / GLM）以及全部 `~…-latest` 别名。
- 前缀与非前缀 ID 走不同路由，选哪个属于产品取舍；本文只记录事实，不给出"应当用哪个"的结论。
- `probe` 的行为随过滤是否生效而变：过滤被丢弃时 `only=["__probe__"]` 不再触发路由错误，而是**真实执行一次推理并计费**。当日 `probe -model deepseek/deepseek-v4.1-flash` 只报"管道未识别 / 未能解析"，而 `probe -model z-ai/glm-5.3` 仍能解析出候选。
- 延迟仅测首字节，未测吞吐（TPS）与长流式响应；外部报告提到的 TPS 下降未复现也未否定。
- 代理侧只记录注入决策，不记录 Cline 实际采用的 `finalProvider`。本节所有"是否生效"的判断都来自手工读取响应元数据，不能从代理日志得出。

## 12. 2026-10-17 外部核对：三个上游参考项目的近期改动

本节核对 [NOTICE](../NOTICE) 列出的三个上游参考项目的改动，找出值得吸收的行为与协议事实。
**除 12.1 明确标注"本次实测"的匿名接口请求外，本节都是转述**：路由与清单结论来自对方的公开记录或
其自己的实测，不是本项目的实测，按第 11 节多点反证复测之前不能当作 Cline 的行为。

| 项目 | 核对到的 HEAD |
|---|---|
| `munmunjaklin458-afk/cline-pass-switcher` | `02538a3`（2026-09-11） |
| `Stabilize7440/cpagw-gateway` | `38aba73`（2026-09-08） |
| `yhshzh/dsh-cline-pass` | `91fbd7e`（0.2.8，2026-10-04） |

### 12.1 官方推荐模型接口（本次实测，可匿名访问）

`GET https://api.cline.bot/api/v1/ai/cline/recommended-models` 在 2026-10-17 无需鉴权即可读取：
不带 `Authorization`、带错误 Bearer、使用默认 UA 均返回 200 与 `application/json`（约 3.3 KB）。
响应顶层是 `recommended` / `free` / `clinePass` / `clineCloud` 四个数组，元素形如
`{"id":…,"name":…,"description":…,"tags":[…]}`。`clinePass` 当日 14 条，全部带 `cline-pass/` 前缀：

```text
deepseek-v4.1-flash, mimo-v2.6-flash, mimo-v2.6-pro, glm-5.3, deepseek-v4-pro,
qwen3.8-max, muse-spark-1.3-contributor, kimi-k3, glm-5.3-flash, qwen3.7-plus,
qwen3.7-max, minimax-m3, mimo-v2.5-pro, mimo-v2.5
```

用途与边界：

- 这是官方口径的**订阅可用清单**，不要外推：清单外再加前缀的 slug 当日返回 404（`cline-pass/claude-sonnet-5.5`、
  `cline-pass/deepseek-v4-flash` 等，见 12.6），"能否用"以清单加逐 ID 复测为准；反过来，在清单里也不代表
  账号可用（`muse-spark-1.3-contributor` 403）或过滤会被采纳（见第 11 节）。
- 当日默认三条规则涉及的 `glm-5.3`、`glm-5.3-flash` 与 deepseek 系模型仍在清单内，但 `cline-pass/glm-5.3`
  已改走私有池并忽略过滤（12.6），默认规则只对裸 ID 生效。
- 上游项目 `cline-pass-switcher` 的 14 项内置清单（`bb7eea0`）与今日接口已不一致：`glm-5.2`、
  `kimi-k2.6`、`kimi-k2.7-code`、`deepseek-v4-flash` 已不在接口返回中，只能当快照，不能当可用性依据。

### 12.2 上游清单的第三方快照（未复测）

`cpagw-gateway` 在 2026-08-26 记录了两个模型的路由表：`glm-5.2` 18 项、`kimi-k3` 14 项。与第 1 节
2026-09-16 的记录相比，出现本文此前没有的 slug：`moonshotai`、`nebius`，以及 `together`
（与 `togetherai` 并存）、`zai`（与 `z-ai` 并存）。这些只是**线索**：来源是对方项目的自测、日期更早，
没有按第 11 节做多点反证，不得据此新增或修改默认规则。

### 12.3 探测结局的分类（据 `dsh-cline-pass` 0.2.8 改进 `probe`）

上游把"探测成功"收紧为"HTTP 成功且 `choices` 非空"，并区分几种非路由结局。本项目据此改进了 `probe`：
代码是本仓库实现，判定依据仍取本项目的实测记录。

| 结局 | 判定依据 |
|---|---|
| `filter-ignored`（2xx 真实执行） | 第 11.7 节：过滤被丢弃时 `only: ["__probe__"]` 不触发路由错误，而是真实推理并计费 |
| `empty-content` | 第 9 节：`max_tokens` 过低时返回 `empty response content`，说明请求已到模型 |
| `auth`（401/403） | 第 1 节（缺 `x-client-type` 的 403）、第 11.6 节（`ENTITLEMENT_ERROR`），两者都只能判身份/权益 |
| `model-unavailable`（404） | 第 11.3 节：不存在的 slug 返回 404 `model not found` |

同时确认三点：

- `200` 或"响应里未命名渠道"都不是"采纳"证据；能读到的实际上游只能记为**本次观测**，与第 11.4 节
  "逐 ID 实测"一致。
- `sort: ""` 被网关拒绝（上游 `de1feaf` 记录为 400）。本项目配置会把空值与 `none` 归一为空、非法值
  直接报错，不会发出空串 `sort`，因此无需改码。
- 上游报告过 `200` 的 SSE 流内出现 `data: {"error": …}` 帧。本项目未复现，流式验收不能只看状态码。

### 12.4 候选改动的处置

| 候选 | 上游依据 | 处置 |
|---|---|---|
| 按管道翻译渠道拼写（planner 写 `zai`/`togetherai`，direct 写 `z-ai`/`together`） | `dsh-cline-pass` 自 2026-09-21 起归一化，称 OpenRouter 对 `provider.only` 里的未知服务商"拒绝而非忽略" | **已采纳**：12.5 的实测确认写错拼写会直接 400/404，不是被忽略；翻译表只收录实测过的两对，其余 slug 原样写入 |
| `provider.ignore` 排除列表 | 来自 OpenRouter 文档；`cpagw-gateway` 与 `cline-pass-switcher` 都不用该字段（它们在应用层把排除项换算成 `only` 白名单） | **已采纳**：12.5 实测 direct 管道生效、planner 无对应字段；落成规则的 `mode: "exclude"`，只写 direct 的 `provider.ignore`，`pipeline: "planner"` 校验拒绝 |
| 应用层排除项（把 `only` 算成白名单） | 同上两个项目 | 不采纳：需要维护"每模型已知上游清单"的运行时状态，白名单过期会静默失效；收益不抵复杂度 |

三个项目的其余改动（账号池、额度、面板、catalog、并发上限、响应标准化）与本项目"零依赖透传代理、
保留上游状态码、只做 data 信封还原"的边界不符，明确不吸收。响应标准化尤其冲突：它会把状态码改写为
200/502/404 并替换错误体，破坏第 8 节验证过的状态码保留行为。

### 12.5 2026-10-17 实测：拼写矩阵、排除字段与候选清单变化

本节是**本项目自己的实测**，起因是核对 12.4 的候选改动。设置：直连 `https://api.cline.bot/api/v1`，
请求头 `x-client-type: cline-cli`，`stream: false`，`max_tokens: 512`（路由对照）或 1（`probe` 子命令）；
模型为 `z-ai/glm-5.3`（planner）与 `z-ai/glm-5.3-flash`（direct）。判据同第 11 节：读 `finalProvider` /
`fallbacksAvailable`（planner）或顶层 `provider`（direct），并做否定用例。

#### 拼写矩阵（同一渠道，两条管道写法不同）

| 管道 | 注入 | HTTP | 实际路由 | 结论 |
|---|---|---|---|---|
| planner | `gateway.only=[zai]` | 200 | `finalProvider=zai`，fallbacks 0 | 接受 |
| planner | `gateway.only=[z-ai]` | 500 | 400 `No available providers match the 'only' filter: z-ai` | 拒绝，不是忽略 |
| planner | `gateway.only=[friendli]` | 200 | `finalProvider=friendli`，fallbacks 0 | 对照：过滤确实生效 |
| planner | `gateway.only=[together]` | 500 | 400 同上，列出了 `togetherai` | 拒绝 |
| direct | `provider.only=[z-ai]` | 200 | `provider=Z.AI` | 接受 |
| direct | `provider.only=[zai]` | 500 | 404 `No allowed providers are available …` | 拒绝 |
| direct | `provider.only=[relace]` | 200 | `provider=Relace` | 对照 |
| direct | `provider.only=[togetherai]` | 500 | 404 同上 | 拒绝 |

因此拼写翻译是双向的、逐管道成立的：`zai`↔`z-ai`、`togetherai`↔`together`。这只证明这两个渠道；
其它上游标识没有实测，不得据此外推。实现只收录这两对（`internal/pin/pin.go` 的 `pipelineSpelling`），
未知 slug 原样写入。

#### 排除字段

| 管道 | 注入 | HTTP | 结果 |
|---|---|---|---|
| direct | `provider.only=[relace]` + `provider.ignore=[relace]` | 500 | 404 `All providers have been ignored …` —— `ignore` 确实到达 OpenRouter |
| direct | `provider.ignore=[relace]`（两次） | 200 | 两次均为 `Venice`；无过滤基线为 `Relace` / `Venice`，只能算弱证据 |
| planner | `gateway.only=[relace]` + `gateway.exclude=[relace]` | 200 | `finalProvider=relace`：`exclude` 被丢弃，planner 无对应排除字段 |

结论：`provider.ignore` 只在 direct 管道可用。planner 侧没有找到任何排除字段，不能把 `ignore` 写进
`providerOptions.gateway` 当作等价能力。

#### 候选清单与可用性变化（与第 1 节 2026-09-16 记录对比）

| 模型 | 当日上游数 | 与第 1 节相比 |
|---|---|---|
| `z-ai/glm-5.3`（planner，`cline-pass/glm-5.3` 同） | 23 | 新增 `alibaba`、`bedrock`、`boundless`、`inference-net`、`relace`、`token-pass`；`blackbox` 不再出现 |
| `z-ai/glm-5.3-flash`（direct，`cline-pass/` 同） | 30 | 新增 `open-inference`、`near-ai`、`decart`、`inceptron`、`inference-net`；`io-net`、`cloudflare` 不再出现 |
| `deepseek/deepseek-v4-flash`（direct） | 24 | 仍可过滤；列表不再逐项对比 |

其它当日观察：

- `probe -model deepseek/deepseek-v4.1-flash` 返回 `500 empty response content`，即**过滤字段仍被丢弃**、
  请求已真实到达模型并计费；新版 `probe` 报 `empty-content` 并说明"过滤未采纳"（12.3）。
- `cline-pass/deepseek-v4-flash` 当日返回 `404 model not found`，与 2026-09-20 的"私有渠道、忽略过滤"
  不同：该 ID 已不可用。`deepseek/deepseek-v4-flash` 仍可用。
- 官方推荐模型接口当日仍可匿名读取，内容与 12.1 一致（`clinePass` 14 条）。
- 代理端到端（默认规则）：`cline-pass/glm-5.3-flash` 非流式返回 200，顶层 `choices` 存在、
  `provider=Relace`，响应头带 `X-Cline-Pin-Unwrapped: data-envelope`；流式请求 200、SSE 正常分帧。
- 拼写翻译的整链验证：规则故意用"错误拼写"——planner 模型 `z-ai/glm-5.3` 配 `upstreams: ["z-ai"]`、
  direct 模型 `z-ai/glm-5.3-flash` 配 `upstreams: ["zai"]`，`pipeline: "auto"`。两条请求均 200：
  前者 `finalProvider=zai`，后者 `provider=Z.AI`；响应头 `X-Cline-Pin-Upstreams` 显示配置原值
  （`z-ai` / `zai`），不是翻译后的值。
- `exclude` 模式的整链验证（direct 模型 `z-ai/glm-5.3-flash`）：规则 `mode: "exclude"` +
  `exclude: ["z-ai"]`，请求同时带调用方 `provider.only=["z-ai"]`。经代理返回 500
  `All providers have been ignored`——证明规则确实写入了 `provider.ignore`、OpenRouter 采纳了它，
  且调用方原有的 `only` 被保留。另用 `exclude: ["venice","relace"]` 连续两次，实际路由均为 `Z.AI`，
  未出现被排除的两个渠道。
- `exclude` 的配置校验：`pipeline: "planner"` + `mode: "exclude"` 被 `check` 拒绝
  （`mode=exclude has no planner equivalent`），不会静默变成无效规则。

局限同第 10、11 节：结论仅对当日账号与网关成立，候选清单与拼写都可能变化；只测了两个渠道和两个模型。

### 12.6 2026-10-17 实测：ClinePass 订阅清单、路由与过滤支持

同一账号、同一日。方法分两步，都不产生生成费用：

- **存在性与路由**：发送 `messages: []`。不存在的模型返回 404 `model not found`；存在的模型
  会被上游以 400 `At least one message is required` 拒绝，**未进入推理**，错误里带
  `failed to invoke model '<canonical>' from <Vercel|Openrouter>`，可读出管道与规范模型 ID。
- **过滤是否被采纳**：`probe`（注入 `only:["__probe__"]`）。返回路由错误=采纳；返回
  `empty-content`/正文=丢弃。丢弃时该次探测已计费。

#### 订阅目录（`clinePass`，14 条）

| 模型 ID | 路由管道 | 规范模型 | 过滤 |
|---|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | planner | `deepseek/deepseek-v4.1-flash` | ❌ 丢弃 |
| `cline-pass/mimo-v2.6-flash` | direct | `xiaomi/mimo-v2.6-flash` | ✅ 8 候选 |
| `cline-pass/mimo-v2.6-pro` | direct | `xiaomi/mimo-v2.6-pro` | ✅ 4 候选 |
| `cline-pass/glm-5.3` | planner → 私有池 | `vmc/glm-5p3-contributor-fallbacks` | ❌ 丢弃 |
| `cline-pass/deepseek-v4-pro` | planner | `deepseek/deepseek-v4-pro` | ✅ 6 候选 |
| `cline-pass/qwen3.8-max` | planner → 私有池 | `vmc/qwen3p8-max-contributor-fallbacks` | ❌ 丢弃 |
| `cline-pass/muse-spark-1.3-contributor` | — | — | 403 不在本账号计划内 |
| `cline-pass/kimi-k3` | planner → 私有池 | `vmc/k3-contributor-fallbacks` | ❌ 丢弃 |
| `cline-pass/glm-5.3-flash` | direct | `z-ai/glm-5.3-flash` | ✅ 30 候选 |
| `cline-pass/qwen3.7-plus` | planner | `qwen/qwen3.7-plus` | ✅ 1 候选 |
| `cline-pass/qwen3.7-max` | planner | `qwen/qwen3.7-max` | ✅ 1 候选 |
| `cline-pass/minimax-m3` | planner | `minimax/minimax-m3` | ✅ 4 候选 |
| `cline-pass/mimo-v2.5-pro` | direct | `xiaomi/mimo-v2.5-pro` | ✅ 6 候选 |
| `cline-pass/mimo-v2.5` | direct | `xiaomi/mimo-v2.5` | ✅ 5 候选 |

要点：

- **带前缀会改路由，且可能改掉过滤能力**：`z-ai/glm-5.3`（planner，采纳过滤）与
  `cline-pass/glm-5.3`（私有贡献池，丢弃过滤）已不同。默认规则 `glm-5.3 → friendli`
  只对裸 ID 生效；对 `cline-pass/glm-5.3` 注入成功但不改变路由。`cline-pass/glm-5.3-flash`
  仍是 direct，默认 `relace` 规则有效。
- **订阅清单只覆盖 `cline-pass/*` 命名空间**：`cline-pass/claude-sonnet-5.5` 返回 404，
  `cline-pass/nonexistent-xyz` 同样 404——前缀不是"任意目录模型都能加"。
- 已下架的旧 ID（均 404）：`cline-pass/deepseek-v4-flash`、`cline-pass/deepseek-v4-pro-0813`、
  `cline-pass/glm-5.2`、`cline-pass/kimi-k2.7-code`。12.5 里 `cline-pass/deepseek-v4-flash`
  的"私有渠道"结论已不适用。
- `muse-spark-1.3-contributor` 在目录里但本账号 403（`is not available in your plan`），
  说明"目录里有"不等于"账号可用"。

#### 同一 key 下的其它目录（非 cline-pass）

- `recommended` 6 条均可路由：`anthropic/claude-sonnet-5.5`（direct）、`google/gemini-4-argon`
  （planner）、`anthropic/claude-opus-5.5`（direct）、`openai/gpt-6-astra`（planner）、
  `openai/gpt-6.1-sol`（planner）、`moonshotai/kimi-k3`（planner）。
- `free` 4 条：`cline-free/nemotron-3.5-super-vl`（planner → `private/nvidia-nemotron-3.5-super-vl-120b-a12b`）、
  `cline-free/mimo-v2.6-flash`（direct）、`cline-free/solar-mini4`（direct）、
  `cline-free/muse-spark-1.3-contributor`（403，同计划限制）。
- `cline-cloud/deepseek-v4.1-flash` 返回 403 `This request is not supported`，未确认可用。
- `GET /models` 当日 470 个条目（2026-09-20 为 446），不含任何 `cline-pass/`、`cline-free/`、
  `cline-cloud/` 条目；它们是独立命名空间，不能从 `/models` 推断订阅可用性。

局限：只测了该账号；订阅清单与路由当天即可能变化；过滤结论只覆盖上表，未做真实补全验证
（`probe` 的计费口径见第 11.7 节）。

### 12.7 2026-10-17 实测：`deepseek-v4.1-flash` 如何钉住官方 DeepSeek

背景：12.6 确认 `cline-pass/deepseek-v4.1-flash` 与裸 ID `deepseek/deepseek-v4.1-flash` 都丢弃过滤，
默认落到 `alibaba`。本节找出可行的钉住路径。

#### 主 ID 无法钉：注入与排除都无效

同一账号、`max_tokens: 512`、`stream: false`，读 `finalProvider` / `fallbacksAvailable` / `planningReasoning`：

| 请求 | finalProvider | fallbacks | 结论 |
|---|---|---|---|
| 裸 `deepseek/deepseek-v4.1-flash` ×3 | `alibaba` ×3 | 17 | 默认 alibaba |
| `cline-pass/deepseek-v4.1-flash` ×3 | `alibaba` ×3 | 17 | 前缀只把 planning 首位改成 deepseek，实际仍是 alibaba |
| 前缀 + `providerOptions.gateway.only=["deepseek"]` | `alibaba` | 17 | 过滤被丢弃 |
| 前缀 + 顶层 `provider.only=["deepseek"]` | `alibaba` | 17 | planner 不读顶层字段 |
| 前缀 + 顶层 `provider.ignore=["alibaba"]` | `alibaba` | 17 | **排除法无效**：`provider.ignore` 只属于 direct 管道 |
| 裸 + 顶层 `provider.ignore=["alibaba"]` | `alibaba` | 17 | 同上 |

即：这个模型走 planner，planner 对它的整块 `providerOptions.gateway` 被丢弃，而 planner 没有
`ignore`/`exclude` 字段（12.5 已用 glm-5.3 验证 `gateway.exclude` 被丢弃）。**在这个 ID 上没有任何可用的钉法。**

#### 实际落点分布（2026-10-17，同一账号）

连续 12 次普通补全采样，前缀形式 `cline-pass/deepseek-v4.1-flash`：`finalProvider=alibaba` **12/12**，
单次成本 $0.00001185–0.00002325。加上本节此前的各种注入尝试，当日对主 ID 的 **23 次请求全部落在
`alibaba`**（裸形式 4 次、前缀形式 19 次）。

关键在完整 `planningReasoning`：凭据规划顺序与执行顺序不同——

```text
System credentials planned for: deepseek, fireworks, baseten, alibaba, runware, particle, novita, …
Total execution order: alibaba(system) → deepseek(system) → fireworks(system) → baseten(system) → …
```

即首个**计划**凭据是 deepseek，但网关把**实际执行顺序**重排为 alibaba 在首位。由于过滤被丢弃，
规则无法干预这个重排。注意 `finalProvider` 表示"由哪家节点服务"，不是模型归属：
模型仍是 DeepSeek 的 v4.1-flash，只是今天由阿里云节点执行。

历史对照：2026-09-20 同一前缀形式默认落到 `deepseek`（第 11.3 节），说明该默认落点会变，
不能当成稳定路由；需要官方节点时用本节后面的 `-1017` + strict 方案。

#### 可行路径：同日快照别名 `deepseek/deepseek-v4.1-flash-1017`

`/models` 里存在 `deepseek/deepseek-v4.1-flash-1017`（planner，候选仅 4 家）。它**采纳 `only`**，
且 `order` 无效——多点反证如下（判据同第 11 节）：

| 注入 | finalProvider | fallbacks | planningReasoning |
|---|---|---|---|
| 不注入（基线） | `deepinfra` | 3 | `System credentials planned for: deepinfra, deepseek, runinfra, inference-net` |
| `only=["deepseek"]` | **`deepseek`** | 0 | `Provider set restricted to: deepseek` |
| `only=["deepinfra"]` | `deepinfra` | 0 | `restricted to: deepinfra` |
| `only=["inference-net"]` | `inference-net` | 0 | `restricted to: inference-net` |
| `only=["runinfra"]` | `runinfra` | 0 | `restricted to: runinfra` |
| `only=["novita"]`（否定用例） | — | — | 500 `No available providers match the 'only' filter: novita` |
| `order=["deepseek","deepinfra"]` | `runinfra` | 3 | order 不生效，实际执行顺序被网关重排 |

因此钉官方 DeepSeek 的正确写法是：

```json
{"model":"deepseek/deepseek-v4.1-flash-1017","mode":"strict","upstreams":["deepseek"]}
```

（默认的 `deepseek` 子串规则本身就会命中该 ID 并强制 `deepseek`，只要客户端改用这个模型名。）

代理整链验证（规则 `deepseek-v4.1-flash-1017` → strict `deepseek`）：

- `deepseek/deepseek-v4.1-flash-1017` → 200，`finalProvider=deepseek`。
- 同一规则匹配不到 `cline-pass/deepseek-v4.1-flash`；故意给它配 strict `deepseek` 时，响应头
  显示 `upstreams=deepseek`，但实际 `finalProvider=alibaba`——再次说明"注入成功"不等于"被采纳"。

#### 边界与风险

- `-1017` 是**裸 ID**（没有 `cline-pass/` 前缀），不在订阅目录里；它是否计入 ClinePass 套餐额度
  未确认，可能与订阅计费口径不同。`cline-pass/deepseek-v4.1-flash-1017` 不存在（404）。
- 日期快照别名可能随目录更新消失或指向新快照；引用前重新 `probe`（本例 `probe` 会返回 4 个候选，
  若变成 `empty-content` 说明过滤又被丢弃）。
- `preferred`（order）在该变体上不生效，规则必须用 `strict`；`sort` 未测。
- 价格差异明显：基线 `deepinfra` 约 $0.0000021/次，官方 `deepseek` 约 $0.0000149/次（同一次短补全，
  单次观测，不构成计价结论）。
- 只测了一个账号与当天网关；这是"当前可用路径"，不是 Cline 的稳定接口。
