# Changelog

本文件记录每个版本的用户可见变更。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)，日期为维护者本地日期（UTC+8）。

**发布约定**：打 tag 前，把 `[Unreleased]` 下的内容整理进新的 `## [x.y.z] - YYYY-MM-DD` 小节。
Release workflow 取该小节作为 GitHub Release 正文；若缺少对应小节，verify 阶段会直接失败，
不会推送镜像、也不会创建 Release。条目写"对用户意味着什么"，而不是复述提交。

## [Unreleased]

### 文档

- `docs/VERIFICATION.md` 新增第 11 节（2026-09-20 复查）：上游过滤字段的时效性（`deepseek/deepseek-v4.1-flash`
  的整个 `providerOptions.gateway` 失效）、`cline-pass/` 的前缀语义及其对默认上游的影响、逐 ID 的管道归属、
  多点反证的判断方法，并在第 1 节标注旧结论的适用边界。
- 两版 README 的「上游路由」「默认规则」「排查问题」补充时效性警告：过滤是否被采纳由 Cline 决定、逐模型不同
  且可能随时变化，必须读响应元数据确认；默认 `deepseek` 规则对 `deepseek/deepseek-v4.1-flash` 当前无法强制。
- `AGENTS.md` 补充 `cline-pass/` 前缀语义、过滤结论的复测要求，以及"指定 A 得到 A 不能作为依据"的判断标准。

## [1.0.0] - 2026-09-20

首个稳定版本。

### 新增

- 零依赖 Go 透传代理，把 Cline API 调用的上游渠道钉死：同时注入 planner 管道（Vercel AI Gateway）
  的 `providerOptions.gateway.only` 与 direct 管道（OpenRouter）的顶层 `provider.only`，
  两条管道各取所需。规则匹配请求里的 `model` 值，不要求 `cline-pass/` 前缀，也不改写模型 ID。
- 规则支持 `strict` / `preferred` 两种强度与 `cost` / `ttft` / `tps` 排序；默认同时写两条管道。
- 配置热重载与管理 API（`/admin/config`、`/admin/rules`、`/admin/probe`、`/admin/reload`）：
  Docker 部署下改规则或渠道不再需要重建容器；非法配置保留上一份有效值并只告警一次。
- `probe` 子命令：从网关路由错误里解析候选上游并给出建议规则。探测携带 `x-client-type: cline-cli`，
  缺少该头时部分模型返回 403。
- 还原 Cline 非流式响应的 `{data:{choices},success:true}` 包封，修复客户端"补全成功但没有内容"；
  SSE 逐块转发并 Flush，不缓冲、不解析。
- 默认规则逐模型锁定 GLM 渠道（`glm-5.3-flash → relace`、`glm-5.3 → friendli`）：
  实测官方 `zai` / `z-ai` 首字节 1.8–3.1 秒，比所选第三方渠道慢数倍，依据见
  [docs/VERIFICATION.md](docs/VERIFICATION.md)。
- `scripts/linux-check.sh`：在 `golang:1.25-alpine` 中执行格式化、vet、构建与 race 测试，
  用于捕获只在 Linux 暴露的编译问题。
- 中英双语 README 与最小 Compose 部署：镜像、端口加可选环境变量即可试用，无需先写配置文件。

### 变更

- `PUT /admin/rules` 同时接受裸数组与 `{"rules":[...]}`；重复的配置错误不再刷屏。
- 仓库定位从"Cline Pass 钉死代理"扩展为"Cline API 上游路由与响应格式兼容代理"。

### 修复

- 默认 GLM 规则曾用同一个上游标识覆盖两个模型，导致 `glm-5.3` 被"打死"；现按模型分别锁定正确 slug。
- 写回规则时沿用原文件权限，不再把 `0600` 改成 `0644`。
- Linux 上 `ENOTSUP` 与 `EOPNOTSUPP` 取值相同，不能同时作为 switch case（Windows 上的检查发现不了）。
- 外部代码审查确认的 18 项缺陷（3 P1 / 13 P2 / 2 P3），以及真实网关测试抓出的 3 个缺陷。

### 文档

- 新增 `AGENTS.md` 与 `CONTRIBUTING.md`，记录协作约束、检查命令与发布流程。
- `docs/VERIFICATION.md` 记录上游标识、管道归属与延迟实测；`docs/CODE_REVIEW.md` 记录审查结论与
  当前实现限制。两份材料均标注日期与适用范围，历史结论不作为当前服务保证。
- README 以快速上手为先，并明确"把 Cline API 地址 `https://api.cline.bot/api/v1` 换成代理地址
  即可试用"。

[Unreleased]: https://github.com/Thinker-Joe/cline-pin-proxy/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/Thinker-Joe/cline-pin-proxy/releases/tag/v1.0.0
