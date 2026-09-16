# 实测记录

本文整理 2026-09-16 对 `api.cline.bot`、发布镜像和 sub2api 集成的测试记录。数据反映当时的账号、网关和配置，不保证当前仍然适用。本次文档整理未重新请求网关。

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
