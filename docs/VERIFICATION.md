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

### 单渠道私有模型

`deepseek-v4-flash`、`deepseek-v4-pro`、`kimi-k3` 这类模型**没有可枚举的多上游**：

- `probe` 无法枚举（`__probe__` 过滤被忽略，不产生路由错误）
- 真实请求的 `finalProvider` 恒为 `openai-compatible-private`，`fallbacksAvailable` 为空
- 对它们注入 `only` **会被忽略，既不生效也不报错**

结论：这类模型**不需要钉死**。默认规则里保留 `deepseek` 一条对它们是前向兼容
（将来若变为多上游即自动生效），但不要指望它当下改变路由。

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

## 九、未验证的部分

诚实记录当前缺口：

- **未验证 arm64 实际运行**：只确认了 arm64 manifest 存在并能被拉取，没有在
  arm64 机器上跑过。
- **未覆盖流式下的钉死**：真实请求都是 `stream: false`。流式透传的「逐块 Flush」
  有单测覆盖（用真实 socket 断言首块在上游仍挂起时就已到达），但没有对着真实
  网关跑过流式钉死。
- **`sort` 参数未实测**：`cost` / `ttft` / `tps` 三种排序只在单测里验证了编码
  正确，没有对真实网关验证其效果。
- **上游 slug 会变**：Cline 侧渠道池随时可能调整，本文清单是 2026-09-16 的快照，
  上线前请自行 `probe` 复核。
- **依赖 Cline 网关的错误措辞**：`probe` 的解析建立在 `from Vercel` / `from
  Openrouter` 与 `available_providers` / `Available providers are` 这些措辞之上。
  措辞变化时 `probe` 会返回空清单并打印原始片段（已验证该降级路径），
  但不会自动适配。
