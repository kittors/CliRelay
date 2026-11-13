# 上游请求结构修复（2026-11-13）

## 需求与根因

来源：用户两张截图及本会话的只读排查；2026-11-13 用户授权“赶紧修复吧”。截图来自其他用户的服务，不作为本项目线上故障证据。

- 图 1：`input[140].content` 最大长度 0、实际长度 1。已在 df9651cd6 的真实 Codex 执行器确认，非空 reasoning.content 会被保留；现有密文清理不能处理此字段。
- 图 2：`cache_control` 最多 4 块、实际 5 块。已确认身份指纹会给客户端合法的 4 块再加 1 块，且顶层自动缓存未被纳入处理。
- 本轮只改 CliRelay 请求准备/发送边界及回归测试，不改前端、数据库和部署配置。

## 可追溯 TODO

| 项目 | 来源 | 实现 | 工程验证 | 人工验收 |
| --- | --- | --- | --- | --- |
| 正式回归用例在旧代码失败，修复后通过 | 两图、用户回归规则 | 已完成 | 已通过（见下文） | 待用户 |
| 面向 OpenAI/Codex 的非空 reasoning.content 规范化，保留 summary/密文/正常消息/工具结果 | 图 1 | 已完成 | 已通过（见下文） | 待用户 |
| 长历史一次处理全部不兼容项，兼容空/缺失 content，不修改原始输入 | 图 1 | 已完成 | 已通过（见下文） | 待用户 |
| 覆盖 HTTP 流式、非流式、compact、WebSocket；兼容上游维持自有字段 | 图 1、前轮建议 | 已完成 | 已通过（见下文） | 待用户 |
| 身份指纹加断点后总数不超 4，优先撤销网关自己的标记 | 图 2 | 已完成 | 已通过（见下文） | 待用户 |
| 顶层自动缓存、客户端本身超限、无断点默认注入 | 图 2、前轮相邻边界 | 已完成 | 已通过（见下文） | 待用户 |
| Claude 流式、非流式、count_tokens 与跳过加工入口统一出口检查 | 图 2、出口排查 | 已完成 | 已通过（见下文） | 待用户 |
| 相关旧行为回归、完整本地 CI、结构门禁、交付前逐项复核 | 项目规则 | 已完成 | 已通过（见下文） | 待用户 |

## 验证记录

- 基线版本：`df9651cd6abbc04aa7ed8aee161879f5a5624e8c`，独立 worktree / 分支 `fix/upstream-request-schema`。
- `go test ./internal/runtime/executor -run 'Test(CodexReasoningContent|PrepareCodexReasoningContent|ClaudeCacheBudgetRegression)' -count=1`：先仅新增正式测试，旧版运行失败。HTTP 非流式/流式/compact 返回截图原始 400；Claude 客户端 4/5 断点、顶层自动缓存、count_tokens、跳过加工入口出现超限。
- 修复后上述测试通过；新增官方 API Key 流式/非流式目标回归和精确目标域名边界。用 `go test -overlay /tmp/cliproxy-schema-redgreen-20261113/old-code-overlay.json ./internal/runtime/executor -run 'Test(CodexReasoningContentHTTPRegression|CodexOfficialAPIKeyReasoningContentHTTPRegression|ClaudeCacheBudgetRegression)' -count=1` 再次加载 HEAD 旧实现，仍复现对应 400（返回测试保留调用旧入口；新 helper 专用测试从 overlay 中剔除以避免编译错误冒充回归失败）。
- `go test ./internal/runtime/executor ./internal/translator/claude/openai/responses ./internal/translator/codex/openai/responses -count=1`：三个相关包完整测试通过。
- 最终代码 `./scripts/ci-pr.sh`：退出码 0；结构门禁、agent 配置检查、gofmt、secret scan、全量 go vet / go test、updater vet/test/build、golangci-lint v1.64.5、后端构建全部通过。
- 最终 `go test -race ./internal/runtime/executor -run 'Test(CodexReasoningContent|CodexOfficialAPIKeyReasoningContent|PrepareCodexReasoningContent|ClaudeCacheBudget)' -count=1`：7 组测试、39 个子场景通过，竞态检测通过。
- 清理数量写入现有 Codex 诊断 `reasoning_contents_removed`，覆盖准确计数和幂等不重复计数。
- 长历史样本有 150 个不兼容 reasoning 项，涵盖报错中的 input[140]；验证一次发出即可成功、summary/合法密文/工具调用及结果/超过 float64 精度的大整数不丢失，输入未原地修改，重复规范化幂等。
- Claude 覆盖 HTTP 流式、非流式、count_tokens、skip_anthropic_processing，独立遍历出站 JSON 统计所有标记；合法 4 个客户端断点保留，优先撤掉身份标记。混合 tools/system/messages 用例确认只删除身份标记；默认无客户端断点的已有缓存测试通过。
- 出口复核：Codex HTTP Execute、ExecuteStream、compact 与两个 WebSocket 入口共 5 处统一调用 `prepareCodexResponsesForUpstream`。官方 OpenAI/ChatGPT 主机才清理非空 reasoning.content；密文过滤继续只限 ChatGPT OAuth。兼容主机、相似域名、空 content/空数组/普通消息不被误处理。
- Claude Execute、ExecuteStream、CountTokens 共用 postClaudeMessages，每次实际发送（含模型规则重试）都限额；不在 ensureCacheControl 中分散裁剪，避免漏过 count_tokens 与跳过加工入口。
- 未覆盖：官方付费接口和线上部署未调用；浏览器页面无改动；Bedrock 未注入身份指纹且不共用本次发送器，未扩展其缓存策略；外部中转自定义域名维持兼容上游语义。不同 TTL 顺序等其他上游限制不属于本次两类错误。

## 交付状态

实现、回归和完整本地 CI 已完成；人工验收与线上验证待用户。用户随后明确授权合入 dev、合入 main 并发版；按 required checks 和发布流程继续，合并后清理 worktree。

## 交付前逐项复核

已重新核对两张原始截图及用户“先排查，随后授权修复”的全部要求。上表 8 项均有实现路径和实际测试证据；根治理仓和 codeProxy 没有修改。工程验证完成不等同于人工验收或上线。

## 发布 TODO（用户补充授权）

| 项目 | 状态 | 证据 |
| --- | --- | --- |
| 本轮修复 PR 合入 CliRelay dev | 进行中 | 本轮完整本地 `./scripts/ci-pr.sh` 已重新通过 |
| dev 部署及公网探测 | 待执行 | 只在 mode 非 built-only、节点 success、Rollout 公网通过后报告 live |
| 两个子仓 dev 合入 main | 待执行 | 独立 release worktree，通过 PR |
| main 同步回 dev、本地引用同步 | 待执行 | main→dev PR、ls-remote 三方核验 |
| 正式发布 | 待执行 | 后端候选 v0.6.3；前端 v0.7.2 代码树与 main/dev 相同，不重复发空版本 |
| 发布产物下载验证及本会话 worktree 清理 | 待执行 | checksums、归档条目、原生版本烟测 |

本轮实读工作流：后端 `release.yaml` + `docker-publish.yml` 使用 GitHub Release / GHCR，不再存在旧 `docker-image.yml` 的 DockerHub 凭据依赖。后端另有两个冲突旧 PR #1289/#1185，未包含于已验证 dev，本次不纳入。
