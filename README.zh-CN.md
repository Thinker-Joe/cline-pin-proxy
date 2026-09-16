# cline-pin-proxy

[English](README.md)

一个 Go 代理，用于为通过 Cline API 调用的模型指定推理服务商，并将 Cline 包装在 `data` 中的 JSON 补全转换为标准 OpenAI 响应格式。模型 ID 可以带或不带 `cline-pass/` 前缀，上游选择是否生效取决于该模型是否支持 Cline 的路由字段。

项目仅使用 Go 标准库，构建产物为单个可执行文件，支持流式转发、配置热重载和可选的管理 API。

```text
OpenAI 兼容客户端 → cline-pin-proxy → Cline API → 推理服务商
```

## 快速开始

### Docker Compose

需要安装 Docker 和 Compose v2 插件，可用 `docker compose version` 确认。将下方文件复制到部署目录即可，无需下载项目源码或安装 Go。镜像支持 `linux/amd64` 和 `linux/arm64`。以下命令使用 Bash。

**1. 创建部署目录和 Compose 文件。**

```bash
mkdir -p cline-pin-proxy/data
cd cline-pin-proxy
```

将以下内容保存为 `docker-compose.yaml`，配置与仓库中的 [docker-compose.yml](docker-compose.yml) 一致：

```yaml
services:
  cline-pin-proxy:
    image: ghcr.io/thinker-joe/cline-pin-proxy:latest
    container_name: cline-pin-proxy
    restart: unless-stopped
    ports:
      - "127.0.0.1:8787:8787"
    environment:
      CLINE_PIN_UPSTREAM: ${CLINE_PIN_UPSTREAM:-}
      CLINE_PIN_API_KEY: ${CLINE_PIN_API_KEY:-}
      CLINE_PIN_FORWARD_HEADERS: ${CLINE_PIN_FORWARD_HEADERS:-}
      CLINE_PIN_LOG_LEVEL: ${CLINE_PIN_LOG_LEVEL:-info}
    volumes:
      - ./data:/etc/cline-pin-proxy
    command: ["serve", "-config", "/etc/cline-pin-proxy/config.json"]
    healthcheck:
      test: ["CMD", "/usr/local/bin/cline-pin-proxy", "healthcheck", "-url", "http://127.0.0.1:8787/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
```

**2. 创建配置文件。**

将以下内容保存为 `data/config.json`。更新已有部署时，保留原有配置。

```json
{
  "api_key": "",
  "admin_token": ""
}
```

省略 `rules` 会使用[默认规则](#默认规则)，其他未填写字段使用程序默认值。默认由各客户端提供自己的 Cline API 密钥；要统一使用固定上游密钥，填写 `api_key`。需要[管理 API](#管理-api) 时，再填写 `admin_token`。

**3. 启动并检查服务。**

```bash
docker compose pull
docker compose up -d --wait
docker compose ps
curl -fsS http://127.0.0.1:8787/healthz
```

`--wait` 会等待容器健康检查通过。健康接口返回 `{"status":"ok"}`，只检查代理进程，不检查上游可用性。启动失败时，用 `docker compose logs --tail=100 cline-pin-proxy` 查看日志。

**4. 接入客户端。**

Base URL 填写 `http://127.0.0.1:8787/v1`，API Key 填写 Cline API 密钥，可以使用下方的[测试请求](#客户端配置)。端口 8787 只发布到 Docker 宿主机的回环地址。客户端在另一个容器中时，将两者加入同一 Docker 网络，使用 `http://cline-pin-proxy:8787/v1`，见[接入 sub2api](#接入-sub2api)。

**日常管理。** 在 Compose 文件所在目录执行：

| 操作 | 命令 |
|---|---|
| 查看状态 | `docker compose ps` |
| 持续查看日志 | `docker compose logs -f --tail=100 cline-pin-proxy` |
| 临时停止 | `docker compose stop` |
| 再次启动 | `docker compose start` |
| 重启进程 | `docker compose restart cline-pin-proxy` |
| 删除容器及 Compose 网络 | `docker compose down` |

宿主机的 `data/` 挂载到容器内的 `/etc/cline-pin-proxy`，执行 `docker compose down` 后仍然保留。修改规则时编辑 `data/config.json`，大部分设置默认在五秒内自动重载。仅启动时生效的设置和环境变量覆盖规则见[热重载](#热重载)。

也可以通过运行中的容器校验配置、预览规则匹配：

```bash
docker compose exec cline-pin-proxy /usr/local/bin/cline-pin-proxy \
  check -config /etc/cline-pin-proxy/config.json -model deepseek/deepseek-v4-flash
```

更新镜像时，先拉取再重建服务。此操作会短暂中断服务，保留 `data/`：

```bash
docker compose pull
docker compose up -d --wait
```

`docker compose restart` 不会切换到刚拉取的新镜像。如需从本地源码构建，先获取项目源码，再取消仓库中 `docker-compose.yml` 的 `build: .` 注释，运行 `docker compose up -d --build --wait`。

### 二进制

从 [GitHub Releases](https://github.com/Thinker-Joe/cline-pin-proxy/releases) 下载，或使用 Go 1.23 及以上版本构建：

```bash
go build -o cline-pin-proxy ./cmd/cline-pin-proxy
cp config.example.json config.json
./cline-pin-proxy serve -config config.json
```

必须用 `-config` 指定文件，程序不会自动读取工作目录中的 `config.json`。省略该参数时，只使用内置默认值和环境变量。

### 客户端配置

| 配置项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:8787/v1` |
| API Key | 有权访问所请求模型的 Cline API 密钥 |
| 模型示例 | `cline-pass/deepseek-v4.1-flash` |

默认转发客户端的 `Authorization` 请求头。设置 `api_key` 或 `CLINE_PIN_API_KEY` 后，代理会用固定密钥覆盖客户端凭据。固定密钥只用于访问上游，**不会为代理的普通 API 路由启用客户端认证**。

在 shell 中将 `CLINE_KEY` 设置为 Cline API 密钥后，可以发送测试请求：

```bash
curl -i http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $CLINE_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"cline-pass/deepseek-v4.1-flash","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":512}'
```

## 上游路由

规则匹配请求中的 `model` 值，不要求特定命名空间。默认 `deepseek` 规则同时匹配 `cline-pass/deepseek-v4.1-flash` 和 `deepseek/deepseek-v4-flash`；`glm-5.3-flash` 规则也匹配 `z-ai/glm-5.3-flash`。代理保留原模型 ID。这些前缀属于 JSON 字段，不是 HTTP 路径，调用端点都是 `POST /v1/chat/completions`。

历史实测已确认 `deepseek/deepseek-v4-flash` 可切换上游：指定 `deepseek` 或 `novita` 后，响应分别返回 `provider: "DeepSeek"` 或 `"Novita"`。`z-ai/glm-5.3-flash` 也通过了 direct 管道探测及注入 `z-ai` 的集成请求。部分 `cline-pass/` 模型则会忽略上游过滤。依据见[实测记录](docs/VERIFICATION.md#1-模型管道与上游标识)。

其他模型 ID 可以配置自定义规则。上游选择要生效，账号必须有权访问该模型，网关也必须支持它的路由字段；模型出现在 `/v1/models` 中不能单独证明它支持上游选择。

Cline 使用两条路由管道，分别从不同的请求字段读取上游设置：

| 管道 | 后端 | 路由字段 | 响应中的路由信息 |
|---|---|---|---|
| `planner` | Vercel AI Gateway | `providerOptions.gateway` | `choices[0].message.provider_metadata.gateway.routing` |
| `direct` | OpenRouter | 顶层 `provider` | `provider` |

每个模型使用哪条管道由 Cline 决定。`pipeline: "auto"` 会同时写入两组字段。例如，DeepSeek 的严格模式规则会添加：

```json
{
  "providerOptions": {"gateway": {"only": ["deepseek"]}},
  "provider": {"only": ["deepseek"]}
}
```

部分模型会忽略这些字段。代理成功注入并不代表 Cline 实际采用了指定上游，验证规则时需要检查上游返回的路由信息。未还原包装的响应会把这些信息放在 `data` 下。

### 默认规则

规则按大小写不敏感的子串匹配，首条匹配规则生效。

| 模型子串，按匹配顺序排列 | 上游 | 模式 |
|---|---|---|
| `deepseek` | `deepseek` | `strict` |
| `glm-5.3-flash` | `relace` | `strict` |
| `glm-5.3` | `friendli` | `strict` |

`glm-5.3` 也能匹配 flash 模型名，因此 flash 规则必须放在前面。两个 GLM 模型的可用上游列表不同，合并为宽泛的 `glm` 规则可能选中不支持的上游。

GLM 默认值依据 2026-09-16 的测量结果选择：`glm-5.3` 使用 `friendli` 时，首个流式字节在 0.31–0.35 秒到达；`glm-5.3-flash` 使用 `relace` 时为 0.72–0.97 秒，各成功四次。这些是历史观测值，不构成延迟保证，也不代表质量比较。第三方服务商可能使用量化模型，本次测试未确认数值精度或输出质量。

上游标识（slug）、忽略过滤条件的已知模型和完整测量数据见[实测记录](docs/VERIFICATION.md)。

### 规则字段

| 字段 | 取值与行为 |
|---|---|
| `name` | 日志和响应头中的规则名，默认 `rule-<index>`。 |
| `model` | 必填，用于匹配的模型 ID 或子串。 |
| `match` | `contains`（默认）、`prefix` 或 `exact`，均不区分大小写。 |
| `pipeline` | `auto`（默认）同时写入两条管道；`planner` 或 `direct` 只写指定管道。 |
| `mode` | `strict`（默认）将首个上游写入 `only`；`preferred` 将候选写入 `order`，允许回退。 |
| `upstreams` | 必填的上游标识数组。`strict` 只使用第一项，`preferred` 至少需要两项。 |
| `sort` | 可选，支持 `cost`、`ttft`、`tps`；在 direct 管道分别映射为 `price`、`latency`、`throughput`。 |

在所选管道中，`strict` 会删除已有的 `order`；`preferred` 会删除已有的 `only` 和 `allow_fallbacks`。其余上游选项保留。只有配置了 `sort` 才会覆盖调用方的排序值。给严格模式添加 `sort` 不会启用回退或移除 `only`；当前规则不支持只排序而不指定候选。

配置文件一旦包含 `rules`，就会整体替换默认规则表。`"rules": []` 表示关闭注入。与内置默认值一致的完整配置见 [config.example.json](config.example.json)。

## 响应格式兼容

实测中，Cline 的非流式补全采用以下包装格式：

```json
{"data":{"choices":[{"message":{"role":"assistant","content":"OK"}}]},"success":true}
```

只读取顶层 `choices` 的客户端无法取得正文。代理默认返回内部的 `data` 对象，并添加 `X-Cline-Pin-Unwrapped: data-envelope`。

转换仅适用于 JSON Content-Type，且必须同时满足：

- 顶层没有 `choices` 字段。
- `data` 是对象。
- `data.choices` 是非空数组。

其余响应体保持原样。JSON 响应的缓冲上限为 8 MiB，额外读取一个字节用于判断是否超限。超过上限时原样转发，并添加 `X-Cline-Pin-Unwrapped: skipped-too-large`。SSE（`text/event-stream`）逐块转发并 Flush，不解析 JSON，也不等待完整响应。

设置 `"unwrap_data_envelope": false` 可关闭转换。此设置作用于所有转发路由的 JSON 响应，与是否匹配上游规则无关。

## 配置

优先级为 **非空环境变量 > 配置文件 > 内置默认值**。空字符串或纯空白环境变量不覆盖文件。`serve -listen`、`probe -api-key` 等命令行参数对相应命令具有更高优先级。

| JSON 字段 | 默认值 | 环境变量 |
|---|---|---|
| `listen` | `127.0.0.1:8787` | `CLINE_PIN_LISTEN` |
| `upstream` | `https://api.cline.bot/api/v1` | `CLINE_PIN_UPSTREAM` |
| `api_key` | 空，转发客户端凭据 | `CLINE_PIN_API_KEY` |
| `forward_headers` | `["x-client-type"]` | `CLINE_PIN_FORWARD_HEADERS` |
| `probe_headers` | `{"x-client-type":"cline-cli"}` | `CLINE_PIN_PROBE_HEADERS` |
| `max_body_bytes` | `67108864`（64 MiB） | `CLINE_PIN_MAX_BODY_BYTES` |
| `watch_seconds` | `5`，`0` 关闭轮询 | `CLINE_PIN_WATCH_SECONDS` |
| `admin_token` | 空，默认关闭管理 API | `CLINE_PIN_ADMIN_TOKEN` |
| `admin_allow_unauthenticated` | `false` | `CLINE_PIN_ADMIN_ALLOW_UNAUTHENTICATED` |
| `unwrap_data_envelope` | `true` | `CLINE_PIN_UNWRAP_DATA_ENVELOPE` |
| `rules` | [默认规则](#默认规则) | `CLINE_PIN_RULES` |

`CLINE_PIN_FORWARD_HEADERS` 使用逗号分隔的请求头名称；`CLINE_PIN_PROBE_HEADERS` 使用逗号分隔的 `name: value`；`CLINE_PIN_RULES` 使用 JSON 数组。`CLINE_PIN_LOG_LEVEL` 支持 `debug`、`info`、`warn`、`error`，没有对应的 JSON 字段。

`upstream` 必须是包含主机名的 HTTP(S) URL，不允许 query 或 fragment。配置必须是 JSON 对象，不接受 `null`。正常配置加载流程会拒绝非法规则 JSON，以及非法的数字、布尔环境变量。文件缺失时的启动例外见[当前实现限制](docs/CODE_REVIEW.md#current-implementation-limits)。

### 热重载

服务按指定间隔检查文件修改时间。合法配置替换当前配置；文件非法或暂时不存在时，继续使用上一份有效配置。相同错误只告警一次，恢复后记录恢复日志。

`listen`、`watch_seconds` 和日志级别只在启动时生效，其余配置字段可以重载，包括管理令牌。Docker 镜像设置了 `CLINE_PIN_LISTEN=0.0.0.0:8787`，以支持端口映射；它会覆盖示例文件中的容器内回环监听地址。

重载后仍以环境变量为准。要通过文件管理某字段，应先移除它的非空环境变量覆盖。修改容器环境变量需要重建容器。重启进程会重新读取文件，但仅执行 `docker compose up -d` 不会因挂载文件内容变化而重启容器。

`serve -config` 指定的文件不存在时，服务使用默认值和环境设置启动，并等待文件出现；已有文件内容非法则启动失败。`check` 和 `probe` 显式指定的文件必须存在。

### 管理 API

设置 `admin_token` 后启用以下接口，使用 `Authorization: Bearer <token>` 或 `X-Admin-Token: <token>` 认证。

| 方法 | 路径 | 行为 |
|---|---|---|
| `GET` | `/admin/config` | 读取已加载配置，不返回 `api_key` 和 `admin_token`。 |
| `GET` | `/admin/rules` | 返回 `{"rules":[...]}`。 |
| `PUT` | `/admin/rules` | 替换内存规则，并尝试写入文件。 |
| `POST` | `/admin/probe` | 使用 `{"model":"...","pipeline":"auto"}` 探测上游。 |
| `POST` | `/admin/reload` | 立即重读配置文件。 |

未设置令牌时，整组接口返回 404，除非显式启用 `admin_allow_unauthenticated`。只要设置了令牌，缺少或错误的凭据就会返回 401，即使该选项为 `true`。仅在可信环境中允许无认证访问。

以下示例假定 shell 中的 `ADMIN_TOKEN` 已设置为管理令牌：

```bash
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8787/admin/rules

# 保存、编辑并提交完整规则表。
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8787/admin/rules > rules.json
# 编辑 rules.json 后再执行下一条命令。
curl -fsS -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' --data-binary @rules.json \
  http://127.0.0.1:8787/admin/rules
```

`PUT` 接受裸数组和 `{"rules":[...]}`，拒绝 `null`、null 元素、未知规则字段和 JSON 后的多余数据。清空规则必须显式提交 `[]`。

成功响应包含 `applied`、`persisted` 和当前 `rules` 数组。写入失败时还会返回 `persist_error` 和 `hint`。未保存的规则可能被后续重载替换，也会在重启后丢失。重新加载时，`CLINE_PIN_RULES` 仍会覆盖文件中的规则；通过 API 管理规则前应移除该覆盖，并检查响应中的规则数组。

保存只修改 `rules` 的值，保留其他 JSON 值及已有文件权限位，不会把环境变量中的密钥写入文件。原子替换需要目录可写。Docker 中的进程使用 UID/GID 65532；Linux 上可用 `sudo chown -R 65532:65532 data` 将挂载目录交给该用户。无法原子替换时，代理可能原地覆盖并记录警告。原地覆盖不保证原子性；磁盘满或 I/O 错误不会触发这一回退。

`POST /admin/probe` 使用配置中的上游密钥和探测请求头，管理令牌不能充当上游凭据。

## 探测与校验

设置 `CLINE_PIN_API_KEY` 后，查看网关报告的上游列表：

```bash
./cline-pin-proxy probe -model cline-pass/deepseek-v4.1-flash
./cline-pin-proxy probe -model deepseek/deepseek-v4-flash -H 'x-client-type: cline-cli'
```

探测会注入 `only: ["__probe__"]`，再解析路由错误。支持过滤的模型会在推理前拒绝请求；忽略过滤的模型可能生成响应并产生费用。返回的列表可能不完整，列出的上游也可能在真实请求中失败。

用 `-pipeline planner` 或 `-pipeline direct` 限定注入管道。探测默认携带 `x-client-type: cline-cli`，部分模型缺少该头会返回 403。命令会根据列表第一项生成一条精确匹配的建议规则，使用前应确认该选择；替换 `rules` 会同时替换所有已有规则。

以下命令只校验配置并预览匹配，不访问 Cline：

```bash
./cline-pin-proxy check -config config.json -model cline-pass/glm-5.3-flash
```

用 `./cline-pin-proxy help` 查看命令，用 `./cline-pin-proxy <command> -h` 查看参数。CLI 提示目前使用中文。

## 接入 sub2api

对于 sub2api 中按 OpenAI 兼容 API-key 账号配置的 Cline Pass，将账号 Base URL 从 `https://api.cline.bot/api/v1` 改为代理的 `/v1` 地址。

- 两个服务都在容器中时，加入同一 Docker 网络，使用 `http://cline-pin-proxy:8787/v1`。容器内的 `127.0.0.1` 指向容器自身。
- 代理未配置固定上游密钥时，账号必须保留 Cline Pass 密钥。
- 账号设置的 `x-client-type: cline-cli` 会被默认代理配置转发。普通代理请求不会主动添加该头。
- `/v1/responses` 等受支持路径不注入上游字段，sub2api 可据此检测上游协议支持情况。

## 排查问题

| 响应头 | 含义 |
|---|---|
| `X-Cline-Pin-Rule` | 补全请求应用的规则，`none` 表示未应用规则。 |
| `X-Cline-Pin-Upstreams` | 配置的上游列表，以 `>` 连接；严格模式仍只使用第一项。 |
| `X-Cline-Pin-Mode` | `strict` 或 `preferred`。 |
| `X-Cline-Pin-Note` | 未注入的原因：无匹配、无法读取模型字段或注入失败。 |
| `X-Cline-Pin-Unwrapped` | `data-envelope` 表示已转换，`skipped-too-large` 表示超出缓冲上限。 |

注入响应头只记录代理决策，不代表 Cline 实际使用的上游；普通透传路由不会添加这些头。上游返回的 `x-*` 头会被转发，但代理自身不生成“实际服务商”响应头。

日志通过 Go 的 `slog` 以文本格式写入 stderr。规则注入日志包含 `model`、`rule`、`upstreams`、`mode` 和 `pipeline`：

```text
level=INFO msg="pinned request" model=cline-pass/deepseek-v4.1-flash rule=deepseek upstreams=deepseek mode=strict pipeline=auto
```

非流式补全正文为空时，检查 `X-Cline-Pin-Unwrapped` 和 `choices` 所在位置。同时确认输出 token 预算足够：一次实测使用 `max_tokens: 24` 时，六个推理模型中有五个返回 `empty response content`；改为 512 后全部成功。这与响应包装是不同的问题。

## 行为约定与安全

- 只对 `POST /v1/chat/completions`、`/chat/completions`、`/api/v1/chat/completions` 注入上游字段。`/v1/` 和 `/api/v1/` 下的其他请求不改写请求体，`OPTIONS` 在本地处理。
- 未匹配或无法解析的补全请求保留原始请求体；注入失败也使用原始请求体转发，并在响应头注明。
- 保留上游状态码，不跟随重定向，返回原始 `Location`。除前述 JSON 包装转换外，响应体保持原样；HTTP 分帧与压缩可能由 Go Transport 处理。
- SSE 使用固定大小缓冲区逐块复制，每次读取后 Flush。代理不设 HTTP 客户端整体超时；连接和 TLS 握手各限 10 秒，等待响应头限 120 秒。
- 正常流式路径和缓冲 JSON 路径遇到上游读取错误时会中止下游连接。超大 JSON 转发路径存在单独的[已知限制](docs/CODE_REVIEW.md#current-implementation-limits)。
- 每个转发请求使用同一份配置快照。注入保留 JSON 数字精度，不对 `<`、`>`、`&` 做 HTML 转义。
- 默认请求体上限为 64 MiB，透传端点同样受限。读取超限返回 413；长度未知的请求此时可能已部分发往上游。
- 转发前校验 API 路径，路径校验器拒绝编码路径和路径穿越段。不支持的路由返回 404。
- 请求头仅转发 `Content-Type`、`Accept`、`Authorization`、`User-Agent` 及 `forward_headers`。响应头仅转发 `Content-Type`、`Content-Encoding`、`Cache-Control`、`Retry-After`、`Location` 和 `x-*`，不复制上游 `Content-Length`；缓冲 JSON 响应会重新计算长度。
- 默认监听地址和 Compose 宿主机端口仅绑定回环。普通 API 路由没有独立的客户端认证；向可信客户端之外开放时，应通过网络控制或带认证的反向代理限制访问。
- 配置文件中的密钥为明文，`config.json` 和 `data/` 已被 git 忽略。`/admin/config` 不返回两个密钥字段，但会返回 `probe_headers`，不能把它视为通用的敏感信息过滤接口。

## 开发与项目资料

源码结构、检查命令和发布流程见 [CONTRIBUTING.md](CONTRIBUTING.md)。历史网关测试见[实测记录](docs/VERIFICATION.md)，审查基线、修复结果和当前限制见[代码审查记录](docs/CODE_REVIEW.md)。这两份详细记录使用中文。

路由知识来自 [cline-pass-switcher](https://github.com/munmunjaklin458-afk/cline-pass-switcher)、[cpagw-gateway](https://github.com/Stabilize7440/cpagw-gateway) 和 [dsh-cline-pass](https://github.com/yhshzh/dsh-cline-pass) 的公开记录，本项目未复制其源代码。详见 [NOTICE](NOTICE)。

本项目专注上游路由和响应格式兼容，不提供账号池或 Web UI。

## 许可证

[MIT](LICENSE)。
