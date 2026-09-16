# 代码审查记录

历史审查基线为 `5c475b0`，修复记录指向 `d8e3fc5`，日期为 2026-09-16。审查覆盖 Go 源码、测试、CLI、配置持久化、Docker、CI 和文档，共 18 项：3 项 P1、13 项 P2、2 项 P3。原记录将这 18 项均标记为已修复。

下文描述缺陷在审查基线上的表现，并保留编号、复现依据和修复方式，不表示当前版本仍有这些历史缺陷。本次文档核对发现的实现例外单列在文末。文件位置改用路径和符号，避免旧行号误导。

## 审查方法与限制

基线检查中，`go test -cover ./...`、`go vet ./...`、`go build ./...` 均通过，`gofmt -l .` 无输出。覆盖率为 pin 96.1%、admin 95.2%、probe 92.0%、config 89.7%、proxy 84.1%，CLI 为 0%。这些数字属于历史基线。

审查新增了 15 个边界复现测试，均按预期行为断言，并在基线上失败。测试使用本地 `httptest` 服务和虚构凭据，未请求 Cline 或更改部署。初始测试与输出存放在审查者的临时目录，不是仓库中的可复现附件；后续回归测试位置见下表。

审查环境为 Windows / Go 1.27.0，`CGO_ENABLED=0`，无 GCC，因此当时未运行 `go test -race`。配置一致性问题通过可控切换确定性复现，不等于完成数据竞态检查。修复后的 Linux、race 和部署验证见[实测记录](VERIFICATION.md#8-代码审查后的复验)。

| 回归文件 | 审查项 |
|---|---|
| [proxy/boundary_test.go](../internal/proxy/boundary_test.go) | 1、2、3、6、7、11、13 |
| [config/boundary_test.go](../internal/config/boundary_test.go) | 4、5、9、12、16、17、18 |
| [admin/boundary_test.go](../internal/admin/boundary_test.go) | 10 |
| [pin/boundary_test.go](../internal/pin/boundary_test.go) | 8 |
| [main_test.go](../cmd/cline-pin-proxy/main_test.go) | CLI 帮助、分发、配置校验和健康检查 |

## P1：请求与响应边界

### 1. 编码路径绕过 API 前缀限制

位置：[proxy.go](../internal/proxy/proxy.go)，`safeRoutePath`、`joinUpstream` 和转发入口。

基线上，`GET /v1/%2e%2e/%2e%2e/admin/private` 通过前缀检查，解码后的路径被拼成 `/api/v1/../../admin/private`。模拟上游规范化路径并重定向后，代理继续访问 `/admin/private`，携带配置的上游凭据，客户端收到 200。

此复现证明能够越过配置上游的 API 路径边界，未证明可任意选择主机。Go 1.22+ ServeMux 匹配使用转义路径，而处理器中的 `URL.Path` 已解码，是该问题的关键。

修复：转发前调用 `safeRoutePath`，拒绝编码路径和点段；同时禁用上游重定向跟随，见第 7 项。原复现名：`TestReviewEncodedTraversal`。

### 2. 单个请求混用旧上游与新密钥

位置：[proxy.go](../internal/proxy/proxy.go)，`handle`、`handleChatCompletions`、`forward`、`copyRequestHeaders`。

基线上，请求体限制、规则、上游和凭据分别调用 `Current()`。测试让配置源在选定上游后切换，旧地址实际收到新密钥。原子指针只能保证单次读取安全，不能保证多次读取属于同一版本。

修复：请求入口只读取一次配置快照，再显式传给后续函数，无需锁住完整请求。原复现名：`TestReviewRequestConfigSnapshot`。

### 3. 上游中断被下游视为正常结束

位置：[proxy.go](../internal/proxy/proxy.go)，`flushCopy`、`abortOnBrokenStream`。

模拟上游声明 `Content-Length: 100`，只发送 `data: partial\n\n` 后断开。基线处理器记录读取错误后正常返回，而上游长度未透传，下游 `io.ReadAll` 得到残缺正文且错误为 `nil`，状态仍为 200。

修复：正常流式路径发生读取错误时，通过 `panic(http.ErrAbortHandler)` 中止下游连接；保留客户端取消与正常 EOF 的区别，不在已发送的响应后附加错误 JSON。原复现名：`TestReviewTruncatedResponse`。后续加入的超大 JSON 分支另有例外，见文末。

## P2：配置、路由与发布

### 4. 文件规则继承同位置的默认字段

位置：[config.go](../internal/config/config.go)，`Load`、`applyFile`。

基线直接把 JSON 解码到带默认规则的 `Config`。Go 复用切片元素，使 `{"rules":[{"model":"custom-model"}]}` 继承默认 `Name` 和 `Upstreams`，缺少必填上游仍能加载。

修复：文件出现的字段独立解码，`rules` 使用新切片整体替换；未提供 `rules` 时保留默认表。原复现名：`TestReviewRulesInheritDefaultSliceElements`。

### 5. API 保存规则掩盖未重载的文件修改

位置：[store.go](../internal/config/store.go)，`SetRules`、`reloadLocked`。

先编辑文件中的 `admin_token`，在轮询前调用 `SetRules`。基线保存时读取了新文件，却只更新内存规则，并将新 mtime 标为已处理。结果是磁盘保留新令牌，内存继续使用旧令牌，之后的轮询也不再加载。

修复：写入成功后调用 `reloadLocked(true)`，重读完整文件并应用环境覆盖。原复现名：`TestReviewPersistDoesNotConsumeExternalEdit`。

### 6. `/api/v1/...` 别名重复拼接

位置：[proxy.go](../internal/proxy/proxy.go)，`joinUpstream`。

基线只处理 `/v1` 前缀，默认基址下的 `/api/v1/chat/completions` 被拼为 `/api/v1/api/v1/chat/completions`，通常返回 404；models 等路由同样受影响。

修复：基址已有版本段时，同时识别 `/api/<version>` 和 `/<version>`，增加路径矩阵回归。原复现名：`TestReviewAPIV1Alias`。

### 7. 自动跟随重定向改变请求和响应

位置：[proxy.go](../internal/proxy/proxy.go)，HTTP client 和 `copyResponseHeaders`。

基线 HTTP client 自动跟随 302，将 POST 改为 GET，下游最终收到另一份 200 正文，丢失原始状态、正文和请求语义。

修复：`CheckRedirect` 返回 `http.ErrUseLastResponse`，响应头允许转发 `Location`。原复现名：`TestReviewRedirectPreservation`。

### 8. preferred 保留旧的 only 条件

位置：[pin.go](../internal/pin/pin.go)，`applyChoice`。

调用方已有 `only: ["old"]` 时，基线 preferred 只增加 `order: ["new-a","new-b"]`，两条管道都会保留冲突条件。本轮复现确认生成字段冲突，未对真实网关复测冲突处理。

修复：preferred 删除 `only` 和 `allow_fallbacks`；strict 删除 `order`，保留无关选项。原复现名：`TestReviewPreferredClearsOnly`。

### 9. null 配置在保存时触发 panic

位置：[config.go](../internal/config/config.go)，`applyFile`；[store.go](../internal/config/store.go)，`persistRulesLocked`。

基线接受顶层 `null` 并继续使用默认配置；保存时解码得到 nil map，给 `doc["rules"]` 赋值触发 panic。HTTP 服务器通常恢复处理器 panic 并中止该请求，不能描述为必然导致整个进程退出。

修复：加载和持久化都要求顶层为非 null 对象，失败返回错误。原复现名：`TestReviewNullConfig`。

### 10. PUT null 清空规则

位置：[admin.go](../internal/admin/admin.go)，`decodeRules`、`decodeRuleElements`。

基线将裸 `null` 解码为 nil slice，返回 200 并清空三条规则，而包装形式 `{"rules":null}` 被拒绝。脚本序列化未初始化变量即可意外触发。

修复：拒绝裸 `null` 和 null 元素，只有显式 `[]` 才清空。原复现名：`TestReviewNullAdminRules`。补充澄清：`[null]` 在真实 Store 中原本就会因缺少 model 被规则校验拒绝，不能与裸 `null` 的缺陷等同；现在解码层也拒绝它。

### 11. 透传端点不执行请求体限制

位置：[proxy.go](../internal/proxy/proxy.go)，`forward`。

设置 `max_body_bytes=8` 后，基线仍将 `/v1/responses` 的 1024 字节请求完整发送给模拟上游并返回 200。该路径是流式转发，此复现没有证明全量缓冲或内存溢出。

修复：已知长度超限直接返回 413；未知长度用 `MaxBytesReader` 限制读取，并把超限错误映射为 413。原复现名：`TestReviewPassthroughBodyLimit`。

### 12. 非法规则环境变量被忽略

位置：[config.go](../internal/config/config.go)，`applyEnv`。

基线加载 `CLINE_PIN_RULES='[{invalid'` 不报错，仍使用三条默认规则，实际配置与用户指定不符。

修复：`applyEnv` 返回错误，正常加载流程拒绝非法规则，以及非法数字、布尔环境值。原复现名：`TestReviewInvalidEnvRules`。文件缺失的启动分支未完整传播该错误，见文末。

### 13. 压缩正文丢失编码声明

位置：[proxy.go](../internal/proxy/proxy.go)，请求与响应头复制。

显式把 `accept-encoding` 加入转发列表后，Go Transport 不再自动解压其协商的 gzip 响应，而基线丢弃 `Content-Encoding`。下游收到以 `1f8b0800` 开头的压缩字节，却只有 JSON Content-Type。

修复：响应头转发 `Content-Encoding`。原复现名：`TestReviewForwardedCompression`。测试按编码声明和可解析结果断言，不依赖压缩流中是否出现明文；stored 块可能包含原始文本。

### 14. Compose 默认环境变量覆盖文件

位置：[docker-compose.yml](../docker-compose.yml)。

基线为 `upstream` 和 `forward_headers` 注入非空环境默认值，即使宿主没有设置变量，文件中的自定义值也不会生效。热重载和重启都不能改变环境变量的优先级。

修复：这两项改用 `${VAR:-}`，默认值由应用提供。证据来自 Compose 和配置代码静态对照，原审查未启动 Docker。

### 15. 发布不等待验证

位置：[release.yml](../.github/workflows/release.yml)、[ci.yml](../.github/workflows/ci.yml)。

基线 CI 和 Release 独立触发，Release 没有测试依赖，能编译就可能推送镜像；tag 推送也未由 main 分支 CI 覆盖。

修复：Release 内增加 `verify`，执行格式、vet、race 和模块整理检查。镜像任务依赖 `verify`；二进制任务依赖镜像任务，间接依赖验证。原证据为工作流静态审查，未触发发布。

### 16. 任意原子写入错误都触发原地截断

位置：[store.go](../internal/config/store.go)，`persistRulesLocked`、`isAtomicReplaceUnavailable`。

基线对 `writeAtomic` 的任何失败都尝试 `O_TRUNC` 原地覆盖。如果失败原因是磁盘满或 I/O 错误，可能同时破坏唯一的有效文件。

修复：仅对权限、只读文件系统、跨设备或不支持原子替换等已分类错误启用回退；`ENOSPC`、`EIO` 直接报错。原地写仍不具备原子性。原审查只做静态分析，未实施磁盘写满或硬件故障注入，后续加入错误分类测试。

## P3：数据保真与校验

### 17. 保存规则改变无关大整数

位置：[store.go](../internal/config/store.go)，`persistRulesLocked`。

基线使用 `map[string]any` 解码，数字进入 float64，使 `max_body_bytes: 9007199254740993` 被重写为 `9007199254740992`。默认 64 MiB 不受影响，但违反只修改规则的约定。

修复：使用 `map[string]json.RawMessage`，只替换 `rules`。原复现名：`TestReviewPersistPreservesIntegers`。

### 18. 上游 URL 只校验前缀

位置：[config.go](../internal/config/config.go)，`validateUpstream`。

基线接受 `http://%` 和 `http://localhost?x=1`。前者直到请求阶段才报解析错误，后者把端点追加进 query。

修复：使用 `url.Parse` 校验 HTTP(S)、主机和转义，拒绝 query、fragment；userinfo 仍被允许。原复现名：`TestReviewInvalidUpstreamAccepted`。

## 其他修正与剩余覆盖

历史修复还将 `flag.ErrHelp` 归一为成功，避免 `cline-pin-proxy -h` 以退出码 1 结束。CLI 覆盖率从 0% 增至当时的 22.6%，但 `serve` 启动、启动失败和退出路径仍未纳入该轮测试。

相应文档问题包括：区分单个二进制与多包源码；PUT 返回的 `rules` 是数组；GLM 示例需保留两条规则与顺序；探测不保证零费用；没有环境覆盖时，重启可以重新读取配置。

原审查的 Windows 环境没有运行 race 测试，后续 Linux 验证已补充。arm64 仍只有构建和 manifest 检查，没有实际运行记录。

<a id="current-implementation-limits"></a>

## 当前实现限制

以下两项来自本次文档核对时对当前代码的检查，独立于上面的 18 项历史记录。已使用临时配置和本地模拟上游复现，未请求真实网关；本轮仅更新说明，未修复这些实现分支。

1. **文件缺失时可能忽略环境变量错误。** `config.NewStore` 的 `os.ErrNotExist` 分支调用 `applyEnv(cfg)`，但未检查返回错误。因此，指定不存在的配置文件时，`serve` 可能在环境值非法的情况下继续使用默认值或已部分应用的设置。`config.Load` 的正常路径会返回该错误。应使用有效配置文件并运行 `check`，不能把正常加载器的校验保证推广到这一分支。
2. **超大 JSON 的后续转发错误未处理。** `proxy.bufferForUnwrap` 在读到超过 8 MiB 后，将前缀写出并调用 `flushCopy`，但忽略两者的返回错误。如果上游在剩余正文传输中断开，该分支不会像正常流式路径一样调用 `abortOnBrokenStream`。现有超限完整性测试不能证明它保留了异常结束信号。

本地复现中，`CLINE_PIN_RULES='[{invalid'` 使 `check` 失败，但指定缺失文件的 `serve` 仍启动并返回健康状态。另一测试的上游声明正文比实际发送多 1024 字节，发送超过 8 MiB 后断开；客户端收到 `skipped-too-large` 和残缺正文，读取未报错。

这两项说明了 README 中相关保证的范围。修复时应分别覆盖“缺失配置文件 + 非法环境值”和“超过转换上限后上游中断”的失败路径。
