# 实测验证记录

本文记录本项目对**真实 `api.cline.bot`** 做过哪些验证、结论是什么、以及**真实测试抓出了
哪些单测漏掉的 bug**。所有数据来自 2026-09-16 的实际请求，未做改写。

> 复现前提：一个有效的 Cline Pass `sk_` key。本文件不包含任何凭据。

---

## 一、管道归属与上游 slug（逐模型探测，2026-09-16）

用 `probe` 子命令对每个模型实测，管道归属稳定（连发多次结果一致）：

| 模型 | 管道 | 可用上游数 | 目标厂商的 slug |
|---|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | **planner** | 16 | `deepseek` |
| `cline-pass/deepseek-v4-flash` | 单渠道私有 | — | 见下方说明 |
| `cline-pass/deepseek-v4-flash-vision-exp` | 单渠道私有 | — | — |
| `cline-pass/deepseek-v4-pro` | 单渠道私有 | — | — |
| `cline-pass/glm-5.3` | **planner** | 18 | **`zai`** |
| `cline-pass/glm-5.3-flash` | **direct** | 27 | **`z-ai`** |
| `cline-pass/kimi-k3` | 单渠道私有 | — | 不需要钉 |
| `z-ai/glm-5.3-flash` | **direct** | 27 | `z-ai` |

这解释了为什么**同一套写法不可能同时适配两者**：`providerOptions.gateway.only` 只对
planner 生效，顶层 `provider.only` 只对 direct 生效。本项目默认双写。

### ⚠️ 同一厂商有两种 slug

**`glm-5.3` 的上游叫 `zai`，`glm-5.3-flash` 的叫 `z-ai`**（有连字符与没有连字符之别）。

这不是笔误，是两条管道背后的注册表不同：

- planner（Vercel）侧的清单里是 `... wafer, zai`
- direct（OpenRouter）侧的清单里是 `... cloudflare, z-ai, reka, ...`

**早期版本的默认规则是一条泛化的 `glm` → `z-ai`，实测会把 `glm-5.3` 打成：**

```
400 No available providers match the 'only' filter: z-ai.
    Available providers are: ..., wafer, zai
```

上游 slug 会随 Cline 侧渠道池变动，**上线前务必逐模型 `probe` 复核**。

### 逐模型的钉死支持情况（DeepSeek 系，2026-09-16 判定）

判定方法：注入 `only: ["__probe__"]`（一个绝不存在的上游）。

- 被路由层拦下并报出可用清单 ⇒ **过滤器生效，支持钉死**
- 照常返回正文 ⇒ **过滤器被忽略，钉死无效**

| 模型 | `__probe__` 结果 | 是否支持钉死 |
|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | 500 + 路由层错误 | **✅ 支持** |
| `deepseek/deepseek-v4-flash` | 500 + 路由层错误 | **✅ 支持** |
| `cline-pass/deepseek-v4-flash` | 200 + 正文 `OK` | ❌ 不生效 |
| `cline-pass/deepseek-v4-pro` | 200 + 正文 `OK` | ❌ 不生效 |
| `cline-pass/deepseek-v4-flash-vision-exp` | 404 `model not found` | 模型不存在 |

对**不支持**的两个模型，注入 `only` 会被静默忽略——既不生效也不报错，
所以默认规则里保留 `deepseek` 一条是安全的（前向兼容），但不要指望它改变路由。

**结论：DeepSeek 系里只有两个模型能真正钉死。** 其中
`cline-pass/deepseek-v4-flash` 与 `-v4-pro` 走固定的私有渠道，无从选择。

### ⚠️ `available_providers` 清单不保证穷尽

实测：`deepseek/deepseek-v4-flash` 的路由错误列出了 26 个上游，**里面没有
`deepseek`**；但把请求钉到 `deepseek` 却成功，且响应里 `provider: "DeepSeek"`。

| 注入 | 响应里的 `provider` |
|---|---|
| 不钉 | `Relace` |
| 钉 `deepseek` | **`DeepSeek`** |
| 钉 `novita` | **`Novita`** |

也就是说，**`probe` 给出的清单只是「路由层愿意披露的那部分」**。
要确认某个 slug 是否真的可用，必须发一次钉住它的真实请求，再读响应里的
`finalProvider`（planner）或 `provider`（direct）。

### ⚠️ 部分模型要求调用方身份头

`deepseek/deepseek-v4-flash` 缺少 `x-client-type: cline-cli` 会直接 403：

```
403 deepseek/deepseek-v4-flash is only available via Cline product surfaces.
```

这意味着**探测本身也必须带上这个头**，否则会得出与线上相反的结论。
`probe` 已支持 `probe_headers` 配置与 `-H "name: value"` 覆盖，默认带上
`x-client-type: cline-cli`。

代理侧的 `forward_headers: ["x-client-type"]` 因此是承重配置，不是可选项。

### deepseek-4.1-flash 的真实可用上游（16）

```
alibaba, baseten, boundless, deepinfra, deepseek, fireworks, gmicloud, modal,
morph, novita, parasail, particle, relace, runware, togetherai, wafer
```

### glm-5.3 的真实可用上游（18）

```
baseten, blackbox, crusoe, deepinfra, digitalocean, fireworks, friendli,
gmicloud, inceptron, modal, morph, novita, parasail, runware, streamlake,
togetherai, wafer, zai
```

### glm-5.3-flash 的真实可用上游（27，与响应自报的 endpoint_count 一致）

```
deepinfra, relace, morph, wafer, streamlake, gmicloud, novita, makora, crusoe,
coreweave, sail-research, atlas-cloud, fireworks, phala, friendli, siliconflow,
digitalocean, together, parasail, baseten, venice, io-net, cloudflare, z-ai,
reka, nextbit, modal
```

---

## 二、钉死是否真的生效（决定性对照）

请求体统一为 `max_tokens=1024`、prompt 为 `Reply with exactly: OK`，直接打
`https://api.cline.bot/api/v1/chat/completions`。

| 实验 | 注入 | HTTP | `finalProvider` | `fallbacksAvailable` | 尝试次数 | 成本 |
|---|---|---|---|---|---|---|
| **A** | `only: ["deepseek"]` | 200 | **`deepseek`** | **`[]`** | 1 | $0.0000143 |
| **B** 对照 | 无 | 200 | **`novita`** | 15 个 | 4（alibaba 先返回 503） | $0.0000309 |
| **C** | `only: ["novita"]` | 200 | **`novita`** | **`[]`** | 1 | $0.0000297 |

结论：

1. **钉死生效**——注入什么就命中什么（A 与 C 互相印证）。
2. **`fallbacksAvailable` 从 15 项变成空数组**，即严格 `only` 确实关闭了回退，
   这就是 `allow_fallbacks: false` 的等价语义。
3. 不钉时网关会自选（B 落到 `novita`），并且会**先失败再换**（4 次尝试）。
4. 钉到 `deepseek` 比放任自选**便宜约一半**（$0.0000143 vs $0.0000309）。

`fallbacksAvailable` 摘要取自：

```
data.choices[0].message.provider_metadata.gateway.routing
```

注意这个路径——路由元数据挂在 `choices[0].message` 下，**不在响应顶层**。

---

## 三、端到端（透过本项目代理）

启动代理，把 `base_url` 指向 `http://127.0.0.1:<port>/v1`，发真实请求：

| 请求模型 | HTTP | `X-Cline-Pin-Rule` | 实际命中 | 正文 |
|---|---|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | 200 | `deepseek` | `finalProvider=deepseek`，`fallbacks=0` | `OK` |
| `cline-pass/glm-5.3-flash` | 200 | `glm` | `provider=Z.AI`，`fallbacks=0` | `OK` |
| `cline-pass/kimi-k3`（无规则命中） | 200 | `none`（附 `Note: no rule matched`） | 网关自选 `openai-compatible-private` | `OK` |
| `GET /v1/models` | 200 | — | 446 个模型原样透传 | — |

未命中规则的模型确实**完全放行**，由 Cline Pass 自主路由，代理不插手。

---

## 四、发布镜像的实跑验证

在硅谷服务器上从 GHCR 匿名拉取线上镜像，以生产同款加固参数运行，测完即删：

```bash
docker pull ghcr.io/thinker-joe/cline-pin-proxy:latest
docker run -d --name cpp-e2e-test --read-only --tmpfs /tmp \
  -p 127.0.0.1:18787:8787 -e CLINE_PIN_API_KEY=... \
  ghcr.io/thinker-joe/cline-pin-proxy:latest
```

| 检查项 | 结果 |
|---|---|
| 容器状态 | `status=running health=healthy restarts=0`（HEALTHCHECK 生效） |
| `--read-only` + `--tmpfs /tmp` | ✅ 正常运行，说明无写盘依赖 |
| `GET /healthz` | `{"status":"ok"}` |
| `cline-pass/deepseek-v4.1-flash` | HTTP 200，`finalProvider=deepseek`，`fallbacks=0`，正文 `OK` |
| `cline-pass/glm-5.3-flash` | HTTP 200，`provider=Z.AI`，`fallbacks=0`，正文 `OK` |
| 无规则命中的模型 | `X-Cline-Pin-Rule: none` + `Note: no rule matched`，原样放行 |
| 镜像架构 | `linux/amd64`、`linux/arm64` |

---

## 五、sub2api 全链路验收（client → sub2api → 本代理 → Cline Pass）

部署形态：代理容器与 sub2api 同在 `shared-egress-net`，sub2api 的两个 Cline Pass
账号（268 / 301）的 `base_url` 指向 `http://cline-pin-proxy:8787/v1`。

用 sub2api 的**按账号真实上游测试**端点（`POST /api/v1/admin/accounts/:id/test`，
带 `model_id`）逐模型验收，覆盖账号 model_mapping 里的全部条目：

| 账号 | 请求 model_id | 结果 | 映射后的模型 | 代理钉到 |
|---|---|---|---|---|
| 268 | `deepseek-flash-LJ` | ✅ | `cline-pass/deepseek-v4.1-flash` | `deepseek` |
| 268 | `deepseek-v4-flash` | ✅ | `cline-pass/deepseek-v4-flash` | `deepseek` |
| 268 | `deepseek-v4-flash-pass` | ✅ | `cline-pass/deepseek-v4-flash` | `deepseek` |
| 268 | `deepseek-v4-pro` | ✅ | `cline-pass/deepseek-v4-pro` | `deepseek` |
| 268 | `glm-5.3` | ✅ | `cline-pass/glm-5.3` | **`zai`** |
| 268 | `glm-5.3-flash-solw` | ✅ | `cline-pass/glm-5.3-flash` | **`z-ai`** |
| 268 | `kimi-k3` | ✅ | `cline-pass/kimi-k3` | 不钉（单渠道） |
| 301 | `deepseek-v4-flash` | ✅ | `deepseek/deepseek-v4-flash` | `deepseek` |
| 301 | `glm-5.3-flash` | ✅ | `z-ai/glm-5.3-flash` | `z-ai` |

**9/9 通过。** 其中 `glm-5.3` 是最有说服力的一条：修正 slug 之前它是 **400 报错**，
改成 `zai` 后变成 ✅ 成功——证明 `only` 过滤器确实被上游采纳。

再透过部署的代理直取路由元数据，确认钉死**真的落地**（不只是请求成功）：

| 模型 | 规则 | 注入 | `finalProvider` | `fallbacksAvailable` |
|---|---|---|---|---|
| `cline-pass/glm-5.3` | `glm-5.3` | `zai` | **`zai`** | `[]` |
| `cline-pass/glm-5.3-flash` | `glm-5.3-flash` | `z-ai` | `provider=Z.AI` | `[]` |
| `cline-pass/deepseek-v4.1-flash` | `deepseek` | `deepseek` | **`deepseek`** | `[]` |
| `cline-pass/deepseek-v4-flash` | `deepseek` | `deepseek` | `openai-compatible-private`（钉被忽略） | `[]` |
| `cline-pass/kimi-k3` | 无 | — | `openai-compatible-private` | `[]` |

---

## 六、真实测试抓出、单测漏掉的 bug

这三个都是**只有打真实流量才会暴露**的问题，共同点是：单测用的假上游太宽容。

### 1. direct 管道解析完全失效

真实响应里 `error` 字段是**字符串**而非对象（JSON 套 JSON，内层被转义一层）：

```json
{"error":"... from Openrouter: request failed with status 404: {\"error\":{...\"metadata\":{\"available_providers\":[...]}}}","success":false}
```

早期实现按对象解 `error.metadata.available_providers`，直接解析失败。
→ 修复：先统一还原 `\"` 转义，再逐字符定位 `available_providers` 后的 `[...]`。

### 2. planner 清单丢掉最后一项

planner 的清单是自然语言，且**模型名里的点号**会截断正则：

```
Available providers are: alibaba, ..., togetherai, wafer","type":"invalid_request_error","param":{"modelId":"deepseek/deepseek-v4.1-flash"}
```

早期用 `([^.]+)` 提取，被 `v4.1` 的句点截断，最后一项 `wafer` 与 JSON 尾巴粘成
一个非法 token 被过滤掉 → **返回 15 项而不是 16 项，静默丢数据**。
→ 修复：改为逐逗号读 token、**遇非法即停**，并先还原转义。

### 3. 路径拼接出重复版本段，全部 404

调用方 base_url 指向 `http://proxy:8787/v1` 时请求路径是 `/v1/chat/completions`，
而上游基址 `https://api.cline.bot/api/v1` 末尾**已经**有版本段，直接相加得到：

```
https://api.cline.bot/api/v1/v1/chat/completions   →  404 Not Found
```

这是最严重的一个——**所有请求都失败**。单测的假上游接受任意路径，所以没发现。
→ 修复：`joinUpstream` 在上游 base 末尾版本段与路径首段相同时剥离后者。

---

## 八、成本

整个验证过程（含多次探针、逐模型探测与三轮完整补全）总计**远低于 $0.01**。
`probe` 子命令按设计在路由层即失败，不走推理后端。

---

## 九、上游测速（GLM 提速取舍，2026-09-16）

背景：官方渠道偏慢。对两个 GLM 模型的**全部候选渠道**逐个实测首字延迟
（TTFT，取流式响应首字节到达时间）与可用性。

方法：`only: [<上游>]` + `stream: true`，`max_tokens` 128，逐条串行测量；
每个候选跑 1–4 次，记录 HTTP、TTFT、completion_tokens 与是否报错。

### glm-5.3（planner 管道）

| 上游 | TTFT | 备注 |
|---|---|---|
| **friendli** | **0.31 / 0.33 / 0.34 / 0.35s** | 4/4 成功，离散度最小 |
| deepinfra | 0.31 / 0.44 / 0.45 / 0.48s | 4/4 |
| togetherai | 0.36 / 0.36 / 0.40 / 0.54s | 4/4 |
| digitalocean | 0.50 / 0.55 / 0.59 / 0.63s | 4/4 |
| runware | 0.67s | |
| parasail | 0.76 / 0.92s | |
| inceptron | 0.88 / 0.77s | |
| baseten | 1.13 / 1.38s | |
| gmicloud | 1.65 / 1.67s | |
| blackbox | 2.14 / 3.21s | |
| fireworks | 2.34 / 0.67s | 不稳定 |
| streamlake | 2.71 / 1.77s | |
| **`zai`（官方）** | **3.11 / 2.04s** | 基准 |
| novita | 5.48 / 3.83s | 比官方还慢 |
| wafer | 5.64 / 0.69s | 极不稳定 |
| crusoe | 8.63s / 超时 | 避免 |
| morph | 1.36 / 7.64s | 不稳定 |
| modal | 超时 / 0.81s | 不稳定 |

→ 选中 **friendli**（最快且 4/4 稳定，比官方快约 6–9 倍）。

### glm-5.3-flash（direct 管道）

| 上游 | TTFT | 备注 |
|---|---|---|
| **relace** | **0.72 / 0.76 / 0.80 / 0.97s** | 4/4 成功，最稳定 |
| cloudflare | 0.62 / 0.77 / 0.96 / 1.14s | 4/4 |
| parasail | (失败) / 0.66 / 0.70 / 1.02s | **3/4** |
| friendli | 0.93 → 1.93 → 2.73 → 3.05s | 4/4 但**持续劣化** |
| fireworks | 三次失败 / 0.74s | **1/4** |
| streamlake | 1.26 / 1.38s | |
| siliconflow | 0.95 / 1.81s | |
| sail-research | 1.07 / 0.88s | |
| phala | 1.09 / 0.34s | 不稳定 |
| wafer | 1.37 / 0.81s | |
| nextbit | 1.76 / 1.38s | |
| **`z-ai`（官方）** | **1.77 / 2.16s** | 基准 |
| gmicloud | 2.11 / 1.53s | |
| digitalocean | 2.11 / 0.38s | 不稳定 |
| deepinfra | 8.26 / 0.72s | 不稳定 |
| reka | 8.05 / 0.24s | 不稳定 |
| venice | 失败 / 4.49s | |
| **morph / novita / makora / crusoe / coreweave / together / baseten / io-net / modal** | 全部 `stream_initialization_failed` | **strict 下不可用** |

→ 选中 **relace**（4/4 稳定，比官方快约 2.2–2.7 倍）。

### 渠道池不通用

两条管道的候选池**完全不同**：`friendli`、`togetherai` 在 glm-5.3 上很快，
而 `glm-5.3-flash` 的 `friendli` 会从 0.93s 劣化到 3.05s；反过来
`relace`、`cloudflare` 只出现在 flash 侧。**换 slug 必须在对应模型上重测。**

### 附：`sort` 参数实测有效

顺带验证了此前一直标为「未实测」的排序参数，两条管道都生效且稳定：

| 模型 | 注入 | 3 次命中 | TTFT |
|---|---|---|---|
| glm-5.3 | `providerOptions.gateway.sort = ttft` | digitalocean ×3 | 0.60 / 0.60 / 0.71s |
| glm-5.3 | `providerOptions.gateway.sort = tps` | friendli ×3 | 0.42 / 0.32 / 0.38s |
| glm-5.3-flash | `provider.sort = latency` | NextBit / Parasail / Relace | 1.34 / 0.65 / 0.70s |
| glm-5.3-flash | `provider.sort = throughput` | Parasail / Parasail / Relace | 0.82 / 1.80 / 0.74s |

注意：`sort` **保留回退**，而 `strict` 的 `only` 没有。对 glm-5.3 而言
`sort: tps` 的效果与直接钉 friendli 相当，但多了一层自动切换——
如果 friendli 以后变慢，把该条规则改成 `sort` 是一行改动。

---

## 十、未验证的部分

诚实记录当前缺口：

- **未验证 arm64 实际运行**：只确认了 arm64 manifest 存在并能被拉取，没有在
  arm64 机器上跑过。
- **未覆盖流式下的钉死**：真实请求都是 `stream: false`。流式透传的「逐块 Flush」
  有单测覆盖（用真实 socket 断言首块在上游仍挂起时就已到达），但没有对着真实
  网关跑过流式钉死。
- **上游 slug 会变**：Cline 侧渠道池随时可能调整，本文清单是 2026-09-16 的快照，
  上线前请自行 `probe` 复核。
- **依赖 Cline 网关的错误措辞**：`probe` 的解析建立在 `from Vercel` / `from
  Openrouter` 与 `available_providers` / `Available providers are` 这些措辞之上。
  措辞变化时 `probe` 会返回空清单并打印原始片段（已验证该降级路径），
  但不会自动适配。

---

## 十一、配置热重载与管理 API（2026-09-16，真实服务器实测）

**为什么做这次迭代**：此前改规则只能「编辑 `config.json` + 重启进程」，而 Docker
部署下这条路是断的——`docker compose up -d` 察觉不到挂载文件的内容变化，不会重建
容器，改完毫无反应，很容易误判成「配置没写对」。同时 distroless 镜像里没有 shell，
`probe` 只能靠 `docker run --rm --entrypoint` 绕一圈。两者都变成可脚本化的能力。

**环境**：腾讯云-硅谷 `170.106.176.16`，`/opt/cline-pin-proxy`，compose 挂载
`./data:/etc/cline-pin-proxy`。镜像 `sha256:d8f7302d04a4…`（`v0.3` 一代），
运行参数 `watch_seconds=5`，`admin_api="enabled (token required)"`。

### 1. 热重载确实不需要重启进程（决定性证据）

```
PID 基线: 3284619   规则数: 3

# 直接在宿主机上改挂载出来的文件，加第 4 条规则，全程不碰容器
规则数: 4           ← 9 秒后（1~2 个轮询周期）
PID:    3284619     ← 与基线相同
```

日志：`msg="config reloaded" rules=4 watch_seconds=5`

**PID 全程未变**，这是「热重载」区别于「重启后重新读配置」的关键证据。
本次验证从部署到结束共经历：加规则 → 写坏 → 恢复 → PUT 落盘 → PUT 复原，
`PID` 始终是 `3284619`。

### 2. 坏配置不会中断服务，且告警不刷屏

写入 `{ this is not json` 后：

| 观察项 | 结果 |
|---|---|
| 生效规则数 | **4**（上一份好配置继续生效） |
| `/healthz` | **200** |
| 22 秒（约 4 个轮询周期）内的 WARN 条数 | **1** |
| 文件改回合法后 | 自动接管，打出 `config reload recovered` + `config reloaded rules=3` |

### 3. 管理 API 鉴权与密钥卫生

| 请求 | 结果 |
|---|---|
| 无认证 `GET /admin/rules`（已设令牌） | **401** |
| 错误令牌 | **401** |
| `Authorization: Bearer <token>` | **200** |
| `X-Admin-Token: <token>` | **200** |
| `DELETE /admin/rules` | **405** + `Allow: GET, PUT` |
| 非法规则（`strict` 但无 `upstreams`） | **400** |
| 未知字段 / 畸形 JSON / 对象形式缺 `rules` / 尾部拖数据 | **400** |
| 未设令牌且未开 `admin_allow_unauthenticated` | **404**（单测覆盖，非 403） |

`GET /admin/config` 返回 **617 字节**，断言通过：

- 不包含 `admin_token` 的值；
- 连 `api_key` **字段本身都不返回**，只回 `admin_auth_required: true`。

### 4. `PUT /admin/rules` 真的会落盘，且不破坏其余配置

```
PUT 裸数组 4 条 → applied=True persisted=True rules=4
  文件规则名     : ['deepseek', 'glm-5.3-flash', 'glm-5.3', 'put-persist-check']
  文件权限/属主  : 600 65532:65532        ← 权限位与属主均被保留
  进程级字段保留 : listen upstream watch_seconds admin_token api_key 全部仍在

PUT 裸数组 3 条 → applied=True persisted=True
  文件规则名     : ['deepseek', 'glm-5.3-flash', 'glm-5.3']
```

### 5. 全链路仍然通（sub2api → 本代理 → Cline Pass）

用 `POST /api/v1/admin/accounts/:id/test` 对两个 Cline Pass 账号逐个模型发真实请求：

| 账号 | 模型 | 结果 |
|---|---|---|
| 268 `Cline Pass - fdvvcc` | `cline-pass/glm-5.3` | ✅ 返回 `ok` |
| 268 | `cline-pass/glm-5.3-flash` | ✅ 返回 `ok` |
| 268 | `cline-pass/deepseek-v4.1-flash` | ✅ 返回 `ok` |
| 301 `Cline Pass - fdvvcc - free` | `cline-pass/glm-5.3-flash` | ✅ 返回 `ok` |
| 301 | `cline-pass/deepseek-v4.1-flash` | ✅ 返回 `ok` |

代理侧同期日志（证明流量确实经过代理且被钉死，而不是绕过了它）：

```
20  cline-pass/deepseek-v4.1-flash -> deepseek        (deepseek)
10  cline-pass/deepseek-v4-flash   -> deepseek        (deepseek)
 4  cline-pass/glm-5.3-flash       -> glm-5.3-flash   (relace)
 2  cline-pass/glm-5.3             -> glm-5.3         (friendli)
```

**无一条未命中规则，无一条注入失败。**

### 6. 本轮真实测试抓出的 bug（单测均未覆盖）

#### (1) `PUT /admin/rules` 只认包装形式，而文档写的是裸数组

实现要求 `{"rules":[...]}`，README 与 curl 示例写的却是 `[...]`：

```
{"error":{"message":"invalid JSON body: json: cannot unmarshal array into Go value
 of type struct { Rules []config.Rule \"json:\\\"rules\\\"\" }","type":"invalid_request_error"}}
```

调用方要先吃一个 400 才知道该用哪种写法。**修复**：两种都接受，并补测试锁定；
同时拒绝 `[...] garbage` 这种尾部拖数据的输入。

#### (2) 坏配置每 5 秒重复告警

`watch_seconds=5` 下，坏文件每个轮询周期都打一条一模一样的 WARN，
实测 22 秒 4 条、此前 25 秒 5 条，几分钟就能把日志淹掉。**修复**：同一个错误
只报一次，恢复时补一条 `config reload recovered`。
（修复前/后的对照已在 §2 用真实计数器确认。）

#### (3) 写回会把文件权限降级

原子替换时硬编码 `0644`。配置里现在可能有 `admin_token`，这等于在运维者
不知情的情况下把 `0600` 的私密配置降成全局可读。**修复**：沿用原文件的权限位。

### 7. 一个刻意的语义不对称（易踩，记在这里）

| 场景 | 启动时 | 运行期 |
|---|---|---|
| 配置文件**不存在** | 以默认值 + 环境变量启动，保留路径等文件出现 | 文件出现后自动接管 |
| 配置文件**存在但非法** | **启动失败** | 保留上一份好配置 + WARN，不中断 |

理由：文件缺失是「还没配」，可以合理降级；文件非法是「配错了」，
启动时静默降级会让人以为规则生效了。**代价**：如果文件被写坏且此刻重启容器，
容器起不来——这是有意的失败快速，但运维上要知道。

---

## 十二、外部代码审查的处置与复验（2026-09-16）

外部 agent 出具了 [CODE_REVIEW.md](CODE_REVIEW.md)，基线 `5c475b0`，列了
**18 项问题（3 P1 / 13 P2 / 2 P3）**。逐项处置结果见该文档末尾，这里只记录
**修复后的真实复验**。

### 1. 编译期才发现的一项：Linux 上 ENOTSUP 与 EOPNOTSUPP 是同一常量

修 P2-16（原子写失败的分类）时写了：

```go
switch errno {
case syscall.EACCES, syscall.EPERM, syscall.EROFS, syscall.EXDEV,
    syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOSYS:   // Linux：重复 case
```

Windows 上 `gofmt` / `go vet` / `go test` 全绿，**推到 CI 才炸**：

```
duplicate case syscall.EOPNOTSUPP (constant 95 of uintptr type syscall.Errno)
```

这正是本次新增的 `release.yml` 验证门禁的价值——它拦住了这次发布
（`Build & push image` 与 `Attach binaries` 都处于 `skipped`，没有推 latest）。
改用 `errors.ErrUnsupported` 表达"不支持"（`syscall.Errno.Is` 已处理这两个别名）。

**据此新增 `scripts/linux-check.sh`**：用 `golang:1.25-alpine` 起容器跑
`gofmt` + `vet` + `build` + `test -race`。本项目在 Windows 上开发、Linux 上发布，
凡是碰 errno 常量、文件权限语义、路径分隔符的改动，推之前都要过一遍。

### 2. 真实 Linux 工具链上的验收

```
go: go version go1.25.14 linux/amd64
--- gofmt -l . ---   clean
--- go vet ./... ---   （无输出）
--- go build ./... --- （无输出）
--- go test -race ./... ---
ok  cmd/cline-pin-proxy        ok  internal/admin    ok  internal/config
ok  internal/pin               ok  internal/probe    ok  internal/proxy
```

### 3. 生产代理上的行为复验（`170.106.176.16:8787`，镜像 `8270f7f9`）

**编码路径穿越被拒绝**（修复前会穿到上游的 `/admin/...`）：

```
/v1/%2e%2e/%2e%2e/admin/private   → 404
/v1/%2e%2e/admin                  → 404
/api/v1/%2e%2e/%2e%2e/admin       → 404
/v1/..%2f..%2fadmin               → 404
```

**`/api/v1` 别名不再被重复拼接**（修复前拼成 `/api/v1/api/v1/models` → 404）：

```
GET /api/v1/models → 200
{"object":"list","data":[{"id":"~deepseek/deepseek-pro-latest",...}]}
```

**其余行为**（用本地 mock 上游 + 临时代理容器，确定性验证 14/15，
唯一一条 FAIL 是验证脚本自己的期望值写错——该 mock 基址没有版本段，
不剥离才是正确行为）：

| 场景 | 结果 |
|---|---|
| `POST /v1/responses` 1024B（有 Content-Length，上限 64） | **413** |
| 同上但 `Transfer-Encoding: chunked` | **413** |
| `POST /v1/responses` 16B（未超限） | **200** |
| `POST /v1/chat/completions` 超限 | **413** |
| 上游回 302 | 下游收到 **302** + `Location: /api/v1/models` + **原始正文** |
| 上游回 `Content-Encoding: gzip` | 编码声明被保留，正文是合法 gzip |

**管理 API**：裸 `null` → **400**、`[null]` → **400**、无认证 → **401**、
正常读取 → **200**，规则表未被非法请求改动。

**热重载**：改文件加第 4 条规则 → 9 秒生效，容器 PID 全程 `3321511` 未变；
`PUT` 裸数组复原 3 条 → `applied=true persisted=true`，文件权限 `600`、属主
`65532:65532` 均保留。

### 4. 全链路复验（sub2api → 本代理 → Cline Pass）

| 账号 | 模型 | 结果 |
|---|---|---|
| 268 | `cline-pass/glm-5.3` | ✅ `success=True content='ok'` |
| 268 | `cline-pass/glm-5.3-flash` | ✅ |
| 268 | `cline-pass/deepseek-v4.1-flash` | ✅ |
| 301 | `cline-pass/glm-5.3-flash` | ✅ |
| 301 | `cline-pass/deepseek-v4.1-flash` | ✅ |

**5/5 通过。** 代理侧同期日志 19 条 `pinned request`，分布
`11×deepseek-v4.1-flash→deepseek`、`6×glm-5.3→friendli`、`2×glm-5.3-flash→relace`，
**未命中规则 0 条、注入失败 0 条**。

### 5. 一处修正

复验脚本最初把 **admin token** 当成 Cline Pass key 发给了 `/v1/chat/completions`
（生产代理是透传模式，`CLINE_PIN_API_KEY` 为空），拿到 401。
**这是验证脚本的错误，不是代理的**：响应头里的 `X-Cline-Pin-Rule: glm-5.3`
恰恰证明代理的规则匹配与注入都正常，只是上游拒绝了那个凭据。
真实补全改用 sub2api 的账号测试接口，它从账号配置里取真实 key。

---

## 十三、Cline 的 `{data:{...}}` 包封与非流式补全（2026-09-16）

**起因**：有报告称客户端 `vision_analyze` 读图报错，现象是"走网关时返回
`{data:{choices}}` 包封，适配器只读顶层 `choices`"，并怀疑与把 Cline 账号的
`base_url` 改成代理地址有关。

### 1. 「sub2api 对 Cline 的 URL 有特殊处理」这个假设不成立

对 `sub2api` 全仓检索：

```
$ rg -ni 'cline\.bot' .          # 零命中
$ rg -ni 'cline' -g '!node_modules' .
backend/internal/service/openai_gateway_response_handling.go:1330:  // 部分 OpenAI 兼容上游（例如 Cline API）…
backend/internal/service/openai_gateway_service_test.go:526:        func TestExtractOpenAIUsage_ReadsClineDataEnvelope
```

**没有任何基于 URL / 主机名的 Cline 识别**。把 `base_url` 换成代理地址，
不可能"让 sub2api 认不出来"而关掉某段兼容逻辑——那段逻辑根本不存在。

### 2. 真实机制

sub2api 自己的注释就是最权威的说明：

> 部分 OpenAI 兼容上游（例如 Cline API）会将标准响应包在 data 字段中：
> `{"data":{"choices": [...], "usage": {...}}, "success":true}`。
> 按优先级先保留原有路径，再尝试兼容层 data 包装，
> **避免同步请求能正常返回但用量被静默记录为 0**。

也就是说：sub2api **知道**这个包封，但只为**用量统计**加了 `data.usage` 兼容，
**正文仍然原样透传给客户端**（注释里的"同步请求能正常返回"正是此意）。
所以只要模型被路由到 Cline 账号、且客户端用非流式，就会踩到。

真实包封（本次抓取，已截去 `provider_metadata` 细节）：

```json
{"data":{"choices":[{"finish_reason":"stop","index":0,"logprobs":null,
  "message":{"content":"ok","role":"assistant"}}],
  "created":1789540261,"generationId":"gen_01M2…","id":"gen_01M2…",
  "model":"vmc/k3-contributor-fallbacks","object":"chat.completion",
  "system_fingerprint":"fp_9q1cawe42q",
  "usage":{"completion_tokens":16,"prompt_tokens":103,"total_tokens":119}},
 "success":true}
```

### 3. 范围：**不是只有 kimi-k3 —— Cline 的非流式响应一律带包封**

> ⚠️ **本节结论经过一次修正。** 初版写的是"只有 `kimi-k3` 会出现包封"，
> 那是**在网关层测的、把路由混进来了**：其他模型当时没被路由到 Cline 账号，
> 所以看不到包封。下面的直连实验推翻了它。

**直连实验（决定性）**：从服务器直接请求本代理（`127.0.0.1:8787`），
上游恒为 Cline，逐个测账号 268 映射的全部 6 个上游模型名。

判断依据是响应头 `X-Cline-Pin-Unwrapped` —— 代理已开启还原，包封不会留在
正文里，但这个头会如实记录"还原发生过"，因此**不需要关掉开关就能测**。

| 上游模型名 | 非流式 | 带图非流式 |
|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | **data-envelope** | **data-envelope** |
| `cline-pass/deepseek-v4-flash` | **data-envelope** | Cline 侧 500（与包封无关） |
| `cline-pass/deepseek-v4-pro` | **data-envelope** | **data-envelope** |
| `cline-pass/glm-5.3` | **data-envelope** | Cline 侧 500（与包封无关） |
| `cline-pass/glm-5.3-flash` | **data-envelope** | **data-envelope** |
| `cline-pass/kimi-k3` | **data-envelope** | **data-envelope** |

**结论：Cline 对非流式响应**一律**包 `data`，与具体模型无关。**

这解释了为什么 sub2api 的注释把它写成"Cline API 的行为"而不是"某个模型的问题"。

### 3.1 那为什么之前只看到 `kimi-k3` 出问题

因为**只有 kimi-k3 当时被路由到了 Cline 账号**。网关层矩阵（12 个模型）的结果：

- 12 个模型里只有 `kimi-k3` 返回包封
- 它的落点 `model` 是 `vmc/k3-contributor-fallbacks`（Cline 虚拟模型）
- 其余模型返回的 `model` 是它们自己的名字（`deepseek-flash`、`glm-5.3-flash`…），
  说明它们**根本没走 Cline**

用「连发 4 次 `deepseek-flash` + 4 次 `glm-5.3-flash`，再看代理日志」验证：
**代理零请求** —— 这些请求确实没经过 Cline（只有 base_url 指向代理的账号
268/301 才会出现在代理日志里）。

所以"哪些模型会中招"完全取决于**当时的账号调度**，而不是模型本身。

### 3.2 一个测量陷阱：max_tokens 太小会伪装成 Cline 故障

直连实验第一轮用了 `max_tokens: 24`，6 个模型里有 5 个返回
500 `empty response content`。原因不是包封，而是**推理模型把预算全花在思考上、
正文为空**，Cline 就此报错。把预算提到 512 后全部成功。

排查这类问题时要先排除这个干扰，否则会误判成"上游坏了"。

### 4. 关键约束：**只有非流式会这样**

流式之所以正常，是因为 sub2api 必须解析 SSE 才能计费，重发时就已是标准事件；
非流式则是原样转发，包封原封不动地漏给客户端。

矩阵探测（模型 × 是否带图 × 是否流式，2026-09-16）：

| 模型 | 图 | 流式 | 形状 |
|---|---|---|---|
| `kimi-k3` | 无 | **否** | **`data` 包封** |
| `kimi-k3` | 有 | **否** | **`data` 包封** |
| `kimi-k3` | 无 / 有 | 是 | 标准 SSE |
| `deepseek-flash` | 无 / 有 | 否 / 是 | 标准 |
| `deepseek-v4.1-flash` | 无 / 有 | 否 / 是 | 标准 |
| `glm-5.3-flash` | 无 / 有 | 否 / 是 | 标准 |

`kimi-k3` 在账号 268（Cline Pass）映射为 `cline-pass/kimi-k3`——确认走 Cline。

### 4. 修复与 A/B 对照（决定性证据）

在代理侧还原包封（`unwrap_data_envelope`，默认开启）。用**同一台生产环境**
做开关对照，探测 `kimi-k3` 非流式：

| `unwrap_data_envelope` | 顶层 `choices` | `data` 包封 | 客户端结果 |
|---|---|---|---|
| **开启**（默认） | ✅ True | False | 正常 |
| **关闭** | False | ✅ True | **失败（复现原始症状）** |
| 恢复开启 | ✅ True | False | 正常 |

这一对照同时证明了三件事：该请求**确实经过本代理**、包封**来自 Cline**、
以及形状变化**确实是代理造成的**（而不是路由漂移）。

### 5. 实现取舍

- **只认精确形状**：顶层无 `choices`、`data` 是对象、`data.choices` 是非空数组。
  其余（标准响应、`/v1/models` 的 `data` 数组、错误体、纯数组）一律原样转发，
  已有 10 组反例测试锁定。
- **流式一个字节都不缓冲**：`isJSONContentType` 把 `text/event-stream` 排除在外；
  测试断言"上游仍挂起时下游必须已收到首字节"，防止有人日后为了省事把 SSE 也缓冲了。
- **缓冲上限 8 MiB**：超过则原样流式转发，并打
  `X-Cline-Pin-Unwrapped: skipped-too-large`。宁可还原不了也不 OOM，
  且**不静默跳过**——静默会让人以为代理坏了。
- **读到一半断开**时按既有约定主动断连，不把半截 JSON 当完整响应返回。
- 还原后补 `Content-Length`，避免下游按错误长度截断或挂起。
- 可用 `unwrap_data_envelope: false` 完全关闭，给需要严格原样透传的部署留出口。

### 6. 顺带观察到的两处账号配置差异（已知为人工变更，非异常）
对照 2026-09-16 04:03 的账号备份 `cline_pass_accounts_backup_20260916040328.json`，
有两处差异。**经确认这是有意的人工配置变更**，不是漂移，记录在此只为避免
后来者误判：

- 账号 268 的 `model_mapping` 由 8 项调整为 7 项（`deepseek-v4-flash-pass` 与
  `deepseek-v4-flash-vision-exp-pass` 移除，`deepseek-v4.1-flash` 新增）。
- 账号 248 `OpenCode GO - joecoffee`（`kimi-k3 → kimi-k3`）被停用
  （`schedulable=false`）。

需要注意的**副作用**（也是本次问题的发现路径）：248 停用后，`kimi-k3` 等模型会
回退到 Cline 账号，因而更容易碰到上面的 `data` 包封。包封还原上线后，
无论路由怎么变都不再受影响。

顺带澄清一点：`base_url` 与 `model_mapping` 是**独立字段**，改前者不会影响后者。

### 7. 仍未验证

- 未在客户端侧（`vision_analyze` 所在的 agent）确认其使用的具体模型名，
  因此"它是否经由 Cline"是**推断**而非直证；直证的是
  "`kimi-k3` 非流式经本代理会得到包封，开启还原后得到标准形状"。
- 未验证 `unwrap_data_envelope` 在超过 8 MiB 的大响应上的线上表现
  （单测覆盖了逐字节完整性，但没有真实的大响应流量）。

