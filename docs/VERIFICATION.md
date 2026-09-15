# 实测验证记录

本文记录本项目对**真实 `api.cline.bot`** 做过哪些验证、结论是什么、以及**真实测试抓出了
哪些单测漏掉的 bug**。所有数据来自 2026-09-16 的实际请求，未做改写。

> 复现前提：一个有效的 Cline Pass `sk_` key。本文件不包含任何凭据。

---

## 一、管道归属（每条模型固定，不随机）

用 `probe` 子命令对同一个模型连发多次，管道归属稳定：

| 模型 | 管道 | 错误来源措辞 | 可用上游数 |
|---|---|---|---|
| `cline-pass/deepseek-v4.1-flash` | **planner**（Vercel AI Gateway） | `... from Vercel:` | 16 |
| `cline-pass/glm-5.3-flash` | **direct**（OpenRouter） | `... from Openrouter:` | 27 |

这解释了为什么**同一套写法不可能同时适配两者**：`providerOptions.gateway.only` 只对
planner 生效，顶层 `provider.only` 只对 direct 生效。本项目默认双写。

### deepseek 的真实可用上游（16）

```
alibaba, baseten, boundless, deepinfra, deepseek, fireworks, gmicloud, modal,
morph, novita, parasail, particle, relace, runware, togetherai, wafer
```

### glm-5.3-flash 的真实可用上游（27，与响应自报的 endpoint_count 一致）

```
deepinfra, relace, morph, wafer, streamlake, gmicloud, novita, makora, crusoe,
coreweave, sail-research, atlas-cloud, fireworks, phala, friendli, siliconflow,
digitalocean, together, parasail, baseten, venice, io-net, cloudflare, z-ai,
reka, nextbit, modal
```

→ `z-ai` 确实在列，**默认 GLM 规则有效**；`deepseek` 也在 deepseek 的清单里，**默认
DeepSeek 规则有效**。

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

## 四、真实测试抓出、单测漏掉的三个 bug

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

## 五、成本

整个验证过程（约 20 次真实请求，含两次探针与三次完整补全）总计远低于 $0.001。
`probe` 子命令按设计在路由层即失败，不走推理后端。

---

## 六、未验证的部分

诚实记录当前缺口：

- **未在容器里跑过镜像**：镜像是 GitHub Actions 构建并推到 GHCR 的，本地无 Docker，
  只验证了 registry 元数据（多架构、entrypoint、CA 证书、HEALTHCHECK），
  没有实际 `docker run` 过。
- **未做 sub2api → 代理 → Cline Pass 的串联验证**：上面第三节是把 base_url 直接指向
  本机代理做的，没有真的改 sub2api 账号配置。
- **未验证 arm64 运行**：只确认了 arm64 manifest 存在。
- **上游 slug 会变**：Cline 侧渠道池随时可能调整，本文件的清单是 2026-09-16 的快照，
  上线前请自行 `probe` 复核。
