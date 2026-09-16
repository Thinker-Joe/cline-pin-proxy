# AGENTS.md — cline-pin-proxy

本文件规定 AI 代理在本仓库中的协作方式、实现约束和验收要求。

## 协作原则

选择完整满足当前需求且总体复杂度最低的方案，同时考虑实现、依赖、运行、排障和维护成本。不得省略权限校验、数据一致性、故障处理或必要验证。

1. 修改前阅读相关代码与文档，明确目标、范围和完成标准。区分需求、设计和实现；涉及正确性、授权或难以撤销的歧义时先澄清，其余采用合理假设并说明。
2. 每次修改保持可验证、可用，优先解决当前问题的根因，不扩大范围。
3. 不为假想需求增加抽象、配置或扩展点。临时方案需说明风险和退出条件。
4. 先检查已有实现和依赖，有明确收益时才自建或引入库，不无故重写通用功能。
5. 删除已确认不再需要的实现，不擅自破坏外部接口或持久数据。
6. 授权范围内的正常、可逆开发步骤可自主完成。销毁数据、生产变更、发布等高影响操作需事先说明并取得授权。
7. 按风险验证，覆盖关键失败路径；修复缺陷时补充回归验证。报告实际结果，不用未经验证的判断代替测试。
8. 如实区分事实、推断和建议。行为、接口或用法改变时同步更新文档。

安全、授权、数据完整性和已确认需求优先于简化实现。约束冲突时明确说明，不擅自降低要求。本文件不授予额外权限。

## 项目概览

面向 Cline API 的上游路由与响应格式兼容代理。源码采用多包布局，构建为单个 Go 可执行文件；模块要求 Go 1.23，CI 和容器构建使用 Go 1.25。仅使用标准库，`go.mod` 不含第三方依赖。

规则匹配 JSON 中的模型 ID，不限制 `cline-pass/` 前缀，也不改写模型 ID。非该前缀的 `deepseek/deepseek-v4-flash` 已有切换上游的实测记录；不能仅凭模型前缀判断是否支持上游选择。

Cline 按模型选择 planner（Vercel AI Gateway）或 direct（OpenRouter）管道。默认同时写入 `providerOptions.gateway` 和顶层 `provider`。部分模型忽略过滤条件，注入成功不代表上游实际采用了指定服务商。

英文入口为 [README.md](README.md)，中文入口为 [README.zh-CN.md](README.zh-CN.md)。修改用户可见行为时同步更新两版。

## 常用命令

```bash
go build -o cline-pin-proxy ./cmd/cline-pin-proxy
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
go test -cover ./...
bash scripts/linux-check.sh
```

每次改动后运行 `go test ./...`；`gofmt -l .` 必须无输出，`go vet` 必须通过。涉及并发路径时运行 race 检测，需要 cgo 和 C 编译器。

开发涉及 Windows，发布目标包含 Linux。修改 errno、文件权限或路径处理时，推送前必须运行 `bash scripts/linux-check.sh`，需要 Docker。Linux 上 `ENOTSUP` 与 `EOPNOTSUPP` 值相同，不能同时作为 switch case；Windows 检查无法发现此类跨平台编译问题。

子命令为 `serve`（默认）、`probe`、`check`、`healthcheck`、`version`。`probe` 通过路由错误解析候选，忽略过滤条件的模型可能产生推理费用。二进制需用 `-config` 显式指定文件；Compose 已指定挂载的 `data/config.json`，默认启用热重载。

## 文件职责

| 路径 | 职责 |
|---|---|
| `cmd/cline-pin-proxy/` | CLI、日志、启动和退出 |
| `internal/proxy/` | HTTP 路由、转发、逐块 Flush、响应格式转换 |
| `internal/pin/` | 解析请求并注入上游字段 |
| `internal/config/` | 默认值、校验、mtime 轮询、配置快照和规则持久化 |
| `internal/admin/` | 管理 API 与认证 |
| `internal/probe/` | 注入 `__probe__` 并解析网关错误中的候选列表 |
| `CONTRIBUTING.md` | 开发检查与发布流程 |
| `docs/VERIFICATION.md` | 上游标识、管道与延迟的实测依据 |
| `docs/CODE_REVIEW.md` | 历史审查、修复和当前实现限制 |

各包的 `boundary_test.go` 保留代码审查回归；响应转换测试位于 `internal/proxy/envelope_test.go`。

## 行为约束

改变以下约定前，应向用户说明影响，并更新英文 README 的“Behavior and security”及中文 README 的“行为约定与安全”。文档记录的实现例外不能当作预期行为；修复时补充失败路径验证。

- 只对 `POST /v1/chat/completions` 及 `/chat/completions`、`/api/v1/chat/completions` 别名注入上游字段。其他受支持端点不改写请求体，保留调用方探测上游能力的结果。
- 未命中规则时保留原始请求体。注入失败时同样原样转发，并设置 `X-Cline-Pin-Note`。
- 保留上游状态码，不跟随重定向，30x 与 `Location` 返回调用方。上游读取异常时应中止下游响应，不能把残缺正文当作正常结束；使用 `http.ErrAbortHandler`，不要在已开始的正文后附加错误 JSON。
- 响应体唯一允许的内容转换是 `unwrap_data_envelope`，默认开启。仅转换 JSON Content-Type、顶层无 `choices`、`data` 为对象且 `data.choices` 为非空数组的响应。SSE 不进入整包缓冲。JSON 超过 8 MiB 后原样转发，并设置 `X-Cline-Pin-Unwrapped: skipped-too-large`。
- 转发路径必须先经过 `safeRoutePath`。拒绝编码路径与点段，避免把 ServeMux 匹配的转义路径解码后拼成越过 API 前缀的上游地址。
- 每个转发请求只读取一份配置快照，并传给后续步骤，防止热重载导致上游地址和密钥来自不同配置。
- 流式响应使用固定大小缓冲区，逐块 Flush，不解析 JSON，不等待完整响应。不设置 `http.Client.Timeout`；分别控制连接、TLS 和响应头超时。
- 请求注入用 `json.Number` 保留数字精度，关闭 HTML 转义，保持其他字段的 JSON 语义及 `< > &`。修改注入逻辑时运行 pin 包测试。
- 在指定管道中，preferred 删除调用方的 `only` 与 `allow_fallbacks`，strict 删除 `order`。配置了 `sort` 才覆盖原排序值，不修改无关选项。`sort` 不会取消 strict 的 `only`。
- 默认 GLM 规则保留 `glm-5.3-flash → relace` 在前、`glm-5.3 → friendli` 在后。不得合并为宽泛的 `glm`。修改标识前，必须对对应模型 probe 并发送真实请求确认路由，记录到 `docs/VERIFICATION.md`。
- 运行期配置非法或文件删除时保留上一份有效配置。启动时已有非法文件应失败；仅文件不存在时可使用默认值和环境设置启动。
- 文件一旦包含 `rules` 就整体替换默认表。显式非法环境值应导致加载失败；文件缺失启动分支的现有例外见代码审查文末。
- 持久化只替换 `rules`，使用 `map[string]json.RawMessage` 保留其他值和数字精度，不把环境密钥写入文件。只有确认无法原子替换时才考虑原地写入；`ENOSPC`、`EIO` 直接报错，不能为重试截断原文件。
- 未配置 `admin_token` 且未显式允许匿名时，管理 API 返回 404。配置令牌后必须验证凭据。`GET /admin/config` 不返回 `api_key` 和 `admin_token`。`PUT /admin/rules` 接受数组或 `{"rules":[...]}`，拒绝 `null`，清空需显式 `[]`。

## 验收与提交

- 零第三方依赖是硬约束。新增依赖前说明理由并取得确认。
- 验收要求：`go test ./...`、`go vet ./...` 通过，`gofmt -l .` 无输出；并发与跨平台改动按上文补充验证。
- 新增上游标识和管道结论必须有 `docs/VERIFICATION.md` 中的实测依据，不使用网关文档或推测替代。历史数据应保留日期与适用范围。
- 不提交密钥，保留 `config.json`、`data/` 的 git 忽略规则。
- 未经用户明确要求，不执行 commit、push 或创建 tag。
