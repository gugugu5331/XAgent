# XAgent 可用性与可靠性路线图 Checklist

> 每一项都必须通过运行代码或观察 TUI 行为验证。不得使用真实 API key、真实 token、真实密码或真实私钥作为验收样本。

## 通用验收规则

- [ ] 每个里程碑结束后运行全量测试（验证：`go test -count=1 ./...` 通过）。
- [ ] 每个涉及 TUI 行为的里程碑完成 tmux 端到端验收（验证：`tmux capture-pane` 中出现预期文本）。
- [ ] 所有验收日志、截图、handoff 证据不包含真实 secret（验证：只使用 fake secret，并对 capture 输出执行敏感字符串检查）。
- [ ] 所有 provider/MCP 外部依赖场景使用 fake/local server（验证：验收记录中包含本地 server 启动命令或测试 fixture，不依赖真实远端服务）。
- [ ] 所有 E2E 使用统一 fake secret 样本，不读取真实 `config.yaml`（验证：临时 config 指向 fake/local provider，env 只设置 `XAGENT_TEST_API_KEY=sk-test-secret` 等 fake 值）。
- [ ] 所有 E2E 证据执行标准 secret scan（验证：扫描命令返回 0，且报告只记录 pass/fail，不粘贴 raw artifact 内容）。

## AC 到验证映射

| AC | 对应任务 | 单元/集成验证 | tmux/E2E 验证 | 脱敏 |
|---|---|---|---|---|
| AC1 配置错误修复建议 | T2.2 | `TestConfigErrorsIncludeActionableHintsAndRedactSecrets` | M1.V1a | 是 |
| AC2 env API key 请求成功 | T2.1, T4.1 | `TestLLMAPIKeyEnvExpansion`, `TestProviderRequestTimeout` | M1.V1b | 是 |
| AC3 enabled=false 不运行/不注入 | T1.1-T1.3 | `Disabled|ExplicitDisabled` | M1.V1c | 否 |
| AC4 timeout/cancel 可恢复 | T4.1, T5.1-T5.3 | `TestProviderRequestTimeout`, `Cancel|ContextCanceled` | M1.V2a, M1.V2b | 是 |
| AC5 `/help` 可发现 | T13.1, T13.2 | `TestHelp...` | M4.V1d | 否 |
| AC6 sessionctx/context/memory diagnostics | T8.1, T8.2 | `TestSessionContextDiagnosticsAreEmittedNotSentToProvider` | M2.V1a-c | 是 |
| AC7 MCP diagnostics/status | T9.1-T9.3 | `TestMCP...` | M2.V1d-e | 是 |
| AC8 工具失败详情给 TUI 和模型 | T10.1, T11.1-T11.3 | `TestProviderReceivesRedactedToolFailureSummaryNextTurn` | M3.V1a | 是 |
| AC9 Bash 权限确认风险 | T12.1-T12.3, T14.1 | `TestPermissionWarningForBashRisk` | M3.V1c | 是 |
| AC10 agent/tool 限制可配置 | T3.1-T3.4, TW.3 | `TestAgentOptionsControlLoopLimits`, `TestExecutorUsesConfiguredLimits` | M1.V1d | 否 |
| AC11 超大输出可追溯 | T10.2-T10.3d, T11.3 | `TestLargeToolOutputWritesPrivateArtifact` | M3.V1b | 是 |
| AC12 坏 JSONL 可恢复保存 | T15.1-T15.3 | `TestRecoveredCorruptJSONLCanSaveAgain` | M4.V1a | 是 |
| AC13 CLAUDE.md/AGENTS.md 兼容 | T16.1-T16.2 | `TestLoaderUsesInstructionFallbackFiles` | M4.V1b | 路径脱敏 |
| AC14 多行输入/布局 | T18.1a-T18.2 | `TestMultiline...`, `TestResponsiveLayoutWidths` | M4.V1d | 否 |
| AC15 会话摘要/停止原因 | T17.1-T17.2 | `TestConversationStoresStopReasonMetadata` | M4.V1c | 否 |
| AC16 plan/do 帮助与误用 | T13.1 | `TestPlanDoUsage` | M4.V1d | 否 |
| AC17 全链路脱敏 | TS.1a-TS.1f | 各 `SecretMatrix` 测试 | 所有 M*.V capture 检查 | 是 |
| AC18 每里程碑测试+tmux | M1.V*, M2.V*, M3.V*, M4.V* | `go test -count=1 ./...` | 每个 M*.V | 是 |
| AC19 工具/MCP/provider secret 不外泄 | TS.1a, TS.1b | `TestHTTPErrorBodySecretMatrix`, `TestToolOutputSecretMatrix` | M2/M3 secret 场景 | 是 |
| AC20 diagnostics 不进模型/memory/JSONL | T7.4, T8.1, T15.1 | `TestDiagnosticEventIsNotConversationMessage` | M2.V1 | 是 |
| AC21 artifact 私有且不暴露模型 | T10.3a-d | `TestArtifactPathIsNotModelVisible` | M3.V1b | 是 |
| AC22 状态命令脱敏 | T7.3, T9.3, T14.1 | `Test...Redacted...` | M2.V1, M3.V1c | 是 |

## M1：启动配置与请求可控性

### 配置与 env

- [ ] 缺少配置文件时输出修复建议（验证：使用临时目录启动，期望输出包含复制示例配置的命令或等价提示）。
- [ ] 缺少 LLM 必填字段时输出字段名和建议（验证：临时 config 缺 `protocol/model/base_url/api_key`，期望显示缺失字段）。
- [ ] `${MISSING_ENV}` 缺失时只显示变量名，不显示任何 secret value（验证：运行配置加载测试和 CLI 启动错误）。
- [ ] `${XAGENT_TEST_API_KEY}` 存在时可被展开并用于 fake provider 请求（验证：fake provider 收到授权信息但 TUI/capture 不显示 key）。
- [ ] `instructions.enabled=false` 不加载项目指令（验证：fake provider 收到的 stable system 不含指令内容）。
- [ ] `context.enabled=false` 不执行上下文压缩/摘要（验证：context manager 相关测试无 Changed/summary）。
- [ ] `memory.enabled=false` 不读取/注入 memory index（验证：sessionctx stable sections 不含 memory index）。

### Provider timeout 与取消

- [ ] provider request timeout 可配置（验证：`llm.request_timeout_ms` 设置很小，本地阻塞 server 超时）。
- [ ] provider timeout 在 TUI 中显示用户可理解的错误和建议（验证：M1.V2a capture 中包含 timeout 分类和建议动作）。
- [ ] streaming 时按 Esc 取消请求（验证：M1.V2b capture 中包含已取消/可继续输入文案）。
- [ ] streaming 时按 Ctrl+C 优先取消请求，空闲时退出（验证：app key handling test）。
- [ ] 取消后可以发送第二条消息（验证：M1.V2b 第二条请求进入发送或收到回复）。

### Agent/tool 配置

- [ ] `agent.max_iterations` 控制最大轮数（验证：`max_iterations=1` 时一轮后停止）。
- [ ] `agent.max_unknown_tool_calls` 控制未知工具阈值（验证：未知工具达到阈值后停止）。
- [ ] `tool.timeout_ms` 控制工具超时（验证：长运行 fake tool 超时）。
- [ ] `tool.max_output_bytes` 控制输出截断或 artifact（验证：小输出上限触发截断提示）。
- [ ] 达到限制时提示继续方式或配置调整方式（验证：TUI/status/stop message 包含“继续/调大配置/缩小任务”或等价建议）。
- [ ] AC10 有独立 tmux E2E 记录（验证：M1.V1d 覆盖 max iterations、unknown tool、tool timeout、max output 四个限制）。

## M2：Diagnostics 与 MCP 可见性

### Diagnostics 基础

- [ ] Collector 限制最大条数和单条长度（验证：`TestCollectorStoresBoundedCopies`）。
- [ ] Collector 输出脱敏（验证：`TestCollectorRedactsSensitiveDiagnostics`）。
- [ ] `/diagnostics` 空状态显示“暂无诊断”或等价文案（验证：app command test）。
- [ ] `/diagnostics` 非空状态显示 code/severity/source/message/hint（验证：app command test）。
- [ ] `/diagnostics` 标注本地诊断，不发送给模型（验证：TUI 文案和 conversation messages 均不包含 diagnostic message）。
- [ ] Diagnostic event 不追加为 conversation message（验证：`TestDiagnosticEventIsNotConversationMessage`）。

### sessionctx/context/memory

- [ ] instructions include 越界 diagnostic 可见（验证：M2.V1a）。
- [ ] memory index 损坏 diagnostic 可见且可恢复（验证：M2.V1b）。
- [ ] context 摘要失败 diagnostic 可见，应用不中断（验证：M2.V1c）。
- [ ] diagnostics 不进入 provider messages（验证：orchestrator fake provider 收到的 messages 不含 diagnostic body）。
- [ ] diagnostics 不进入 memory index（验证：TS.1e）。

### MCP

- [ ] MCP env 缺失时 `/mcp status` 显示 server 名和缺失变量名（验证：M2.V1d）。
- [ ] MCP env 缺失不显示 env value（验证：敏感样本检查）。
- [ ] MCP stdio stderr 错误显示脱敏限长摘要（验证：M2.V1e）。
- [ ] MCP HTTP 401/403 显示 status 和脱敏 body preview（验证：M2.V1e）。
- [ ] MCP HTTP 二进制/未知 content-type 只显示类型/大小（验证：mcpclient test）。
- [ ] MCP request headers/body 不被记录（验证：TS.1a）。

## M3：工具错误、Artifact 与权限

### 工具输出与 artifact

- [ ] Bash 失败 Content 包含 exit code/stdout/stderr 脱敏摘要（验证：`TestBashFailureContentIncludesRedactedStdoutStderr`）。
- [ ] Bash 成功 stdout 含 secret 时也只生成脱敏摘要（验证：TS.1b）。
- [ ] UTF-8 截断不切断 rune（验证：`TestExecutorTruncatesUTF8Safely`）。
- [ ] 超大输出写入私有 artifact（验证：M3.V1b 文件系统权限检查）。
- [ ] artifact 目录不在仓库工作区（验证：路径不以项目根开头）。
- [ ] artifact 目录权限 0700、文件权限 0600（验证：stat 权限）。
- [ ] 模型可见摘要不包含 artifact 绝对路径（验证：fake provider 第二轮 messages 检查）。
- [ ] TUI 显示 artifact id 或追溯提示（验证：M3.V1b capture）。
- [ ] artifact 清理策略可执行（验证：过期 artifact 被清理，未过期保留）。

### 模型后续轮次

- [ ] 工具失败后，下一轮 provider 能看到脱敏失败摘要（验证：`TestProviderReceivesRedactedToolFailureSummaryNextTurn`）。
- [ ] 下一轮 provider 不看到 raw stdout/stderr secret（验证：TS.1b）。
- [ ] ToolDisplay 展示 error code/stdout/stderr/truncated/artifact 字段（验证：`TestToolDisplayExtractsRedactedFailureDetails`）。
- [ ] AC8 有 fake provider E2E 证据（验证：M3.V1a 记录第二轮 provider 输入，包含 exit code 和脱敏摘要，不含敏感样本原文）。

### 权限

- [ ] Bash 权限确认明确提示非沙箱（验证：M3.V1c capture）。
- [ ] Bash warning 包含文件系统、网络、环境变量、破坏性写入风险（验证：`TestPermissionWarningForBashRisk`）。
- [ ] 高风险 Bash 不提供 permanent 授权（验证：TS.1f 和 M3.V1c 高风险路径）。
- [ ] 含 runtime secret 或敏感样本的命令/参数不会写入 permanent rule（验证：`TestPermanentPermissionRulesNeverPersistSecrets`）。
- [ ] 低风险 permanent 授权显示脱敏规则摘要、落点和撤销提示（验证：M3.V1c 低风险路径）。
- [ ] 权限确认面板显示工具名、命令/路径、风险、权限模式、授权范围（验证：`TestConfirmationPanelRendersRiskAndRedactsArguments`）。
- [ ] `[Enter/n] 拒绝` 文案可见且按键行为保持兼容（验证：app key handling test）。
- [ ] `/permissions status` 显示当前模式和脱敏规则摘要（验证：M3.V1c）。

### Help 与 slash command

- [ ] `/help` 显示本地命令和快捷键（验证：`TestHelpCommandDisplaysLocalCommandsAndShortcuts`）。
- [ ] 未知 `/foo` 本地报错，不发送给模型（验证：`TestUnknownSlashCommandIsLocalError`）。
- [ ] `/plan` 和 `/do` 空输入显示用法（验证：`TestPlanDoCommandsShowUsageWhenEmpty`）。
- [ ] `/help` 包含 `/diagnostics`、`/mcp status`、`/permissions status` 和 Bash 非沙箱说明（验证：help render test）。

## M4：会话恢复、指令兼容与 TUI 长输入

### JSONL 会话恢复

- [ ] 坏 JSONL 会话仍出现在列表中（验证：`TestListIncludesRecoverableCorruptJSONLWithoutLeakingLine`）。
- [ ] 坏行 diagnostic 不包含坏行原文（验证：TS.1d）。
- [ ] Recover 后继续 Save 不被旧坏行阻断（验证：`TestRecoveredCorruptJSONLCanSaveAgain`）。
- [ ] 列表显示“需要恢复”或等价标记（验证：M4.V1a capture）。

### 指令兼容

- [ ] 只存在 `CLAUDE.md` 时可以加载或给出明确提示（验证：M4.V1b）。
- [ ] `AGENTS.md` 候选兼容（验证：instructions loader test）。
- [ ] 显式 `project_file` 优先于 fallback（验证：instructions loader test）。
- [ ] include 越界仍被拒绝（验证：instructions security test）。
- [ ] 候选未加载 diagnostic 路径脱敏或缩写（验证：T16.2 test）。

### 会话摘要/停止原因

- [ ] max iterations stop reason 被记录（验证：conversation metadata test）。
- [ ] user cancel stop reason 被记录（验证：conversation metadata test）。
- [ ] provider timeout stop reason 被记录（验证：conversation metadata test）。
- [ ] 会话列表显示 stop reason 或摘要（验证：M4.V1c capture）。
- [ ] 旧记录没有 metadata 时仍可显示原标题（验证：list render test）。

### 多行输入与布局

- [ ] 输入缓冲支持多行文本（验证：`TestInputStoresMultilineText`）。
- [ ] 粘贴多行不破坏缓冲（验证：`TestInputPasteMultilineText`）。
- [ ] 提交/换行按键规则明确且可测试（验证：`TestMultilineSubmitAndNewlineKeys`）。
- [ ] streaming 时取消优先级不变（验证：`TestMultilineInputDoesNotBreakFocusPriority`）。
- [ ] confirmation 和 list 焦点不被多行输入破坏（验证：app focus test）。
- [ ] 40/80/120 列宽渲染不重叠（验证：`TestResponsiveLayoutWidths`）。
- [ ] tmux resize 到 40/120 后 capture-pane 中消息和输入提示仍可读（验证：M4.V1d）。

## 全链路脱敏验收

- [ ] Provider/MCP HTTP body 脱敏矩阵通过（验证：TS.1a）。
- [ ] Tool stdout/stderr 与 ToolDisplay 脱敏矩阵通过（验证：TS.1b）。
- [ ] Diagnostics/App/TUI 脱敏矩阵通过（验证：TS.1c）。
- [ ] Conversation JSONL/recovery 脱敏矩阵通过（验证：TS.1d）。
- [ ] Memory index 不收录错误详情和工具输出（验证：TS.1e）。
- [ ] Permission status/confirmation 脱敏矩阵通过（验证：TS.1f）。
- [ ] tmux capture-pane 中不含敏感样本原文（验证：对每个 M*.V capture 输出执行字符串检查）。

## E2E fake fixture 与 secret scan 模板

### Fake/local server 要求

每个 M*.V 场景必须使用临时 config 和 fake/local server：

```text
Fake provider setup:
- 成功回复：返回固定文本，例如 "OK from fake provider"。
- 阻塞/timeout：保持连接或延迟超过 llm.request_timeout_ms。
- 工具调用：返回预设 tool call，用于 Bash 失败、unknown tool、max iterations。
- HTTP 错误：返回 401/403，body 含敏感样本矩阵中的 fake secret。

Fake MCP setup:
- stdio：输出可控 stderr，支持 protocol error。
- HTTP：返回 401/403 或二进制 content-type。
- env：只使用缺失变量名或 fake env value，不使用真实远端服务。
```

临时 config 必须满足：

```text
llm.base_url = fake/local provider URL
llm.api_key = ${XAGENT_TEST_API_KEY}
XAGENT_TEST_API_KEY = sk-test-secret
mcp servers 指向 fake/local fixture
```

禁止在 E2E 中读取真实 `/Users/luoxinxin/Desktop/XAgent/config.yaml` 或任何真实 API key。

### 标准 secret scan 命令模板

对每个 capture、测试日志、JSONL、memory index、artifact metadata、handoff 文本执行：

```sh
python3 - <<'PY' "$FILE_TO_SCAN"
import pathlib, sys
needles = [
    'sk-test-secret', 'abc123', 'secret-key', 'access-secret', 'refresh-secret',
    'my-password', 'my-secret', 'session-secret', 'query-secret', 'x-api-secret',
    'anthropic-secret', 'openai-secret', 'ghp_secret', 'aws-secret',
    'success-secret', 'nested-secret', '-----BEGIN PRIVATE KEY-----'
]
text = pathlib.Path(sys.argv[1]).read_text(errors='ignore')
found = [s for s in needles if s in text]
if found:
    print('SECRET_SCAN_FAIL ' + ','.join(found))
    raise SystemExit(1)
print('SECRET_SCAN_PASS')
PY
```

artifact raw 文件不粘贴进报告；只记录本地 scan pass/fail、路径是否在 repo 外、目录/文件权限是否符合 0700/0600。

## Handoff 与多 Agent 验收

- [ ] 每个 agent handoff 包含 task IDs、base commit、changed files、shared files touched（验证：handoff 文本完整）。
- [ ] 每个 agent handoff 包含 public API/config/diagnostic code 变化（验证：handoff 文本完整）。
- [ ] 每个 agent handoff 包含 tests run 和 tests not run reason（验证：handoff 文本完整）。
- [ ] 每个 agent handoff 包含 secret scan command/result（验证：handoff 文本完整）。
- [ ] 主会话按 Wave 顺序合并（验证：合并记录或报告按 Wave 分组）。
- [ ] 每次合并共享 owner 文件后运行相关包测试（验证：报告列出命令和结果）。
- [ ] 每个里程碑完成前运行 `go test -count=1 ./...`（验证：报告记录输出）。

## 端到端验收报告模板

每个 M*.V 场景验收时记录：

```text
Scenario:
Task/AC IDs:
Config path:
Fake/local server setup:
Command to start XAgent:
User input / key sequence:
Expected visible text:
Expected absent sensitive strings:
Observed capture-pane excerpt:
Secret scan command/result:
Pass/fail:
Follow-up:
```

## 最终通过条件

- [ ] AC1-AC22 均至少有一个通过的单元/集成验证。
- [ ] 每个里程碑至少有一个通过的 tmux E2E 场景，且 M1-M4 的所有 M*.V 子场景均记录结果。
- [ ] 全链路脱敏验收全部通过。
- [ ] `go test -count=1 ./...` 通过。
- [ ] 无真实 secret 出现在 TUI capture、测试日志、JSONL、memory index、handoff 证据中。
