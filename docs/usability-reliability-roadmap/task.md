# XAgent 可用性与可靠性路线图 Tasks

> 本文经过多 agent review 后修订：原 T1-T22 被提升为 Epic/里程碑切片；真正执行时使用 `Tn.m` 原子任务。每个原子任务应控制在 2-5 分钟，超过则继续拆分。

## 全局执行规则

1. **先合约，后并行。** T0 完成前，不允许并行 agent 修改共享集成文件。
2. **先模块，后集成。** 模块 agent 只做 owner 范围内的 API/helper/测试，主会话负责 `app/update.go`、`events.go`、`orchestrator/chat.go`、`cmd/xagent/main.go` 等集成文件。
3. **先脱敏，后展示。** 任何错误详情、stdout/stderr、MCP/provider body、diagnostics、permission status 进入 TUI/模型/JSONL/memory 前都必须经过统一脱敏与限长。
4. **测试必须可断言。** 单个任务使用具体测试名；`go test ./...` 只作为里程碑 gate。
5. **E2E 不用真实 secret。** tmux、日志、handoff 证据只能使用 fake token，不得包含真实 API key。
6. **每个原子任务结束必须保持仓库可编译。** 涉及 public API 或构造函数变更时，要么提供兼容 shim，要么标记为主会话一次性集成任务，不允许留下不可编译中间态。

## 敏感样本矩阵

所有涉及脱敏的任务至少使用以下样本：

```text
sk-test-secret
Authorization: Bearer abc123
authorization: bearer abc123
api_key=secret-key
apiKey: secret-key
access_token=access-secret
refresh_token=refresh-secret
password=my-password
secret=my-secret
cookie=session-secret
set-cookie=session-secret
-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----
https://example.test/?token=query-secret
x-api-key: x-api-secret
X-Api-Key: x-api-secret
ANTHROPIC_API_KEY=anthropic-secret
OPENAI_API_KEY=openai-secret
GITHUB_TOKEN=ghp_secret
AWS_SECRET_ACCESS_KEY=aws-secret
https://user:pass@example.test/path
Authorization: Basic dXNlcjpwYXNz
{"headers":{"Authorization":"Bearer nested-secret"}}
stdout success contains success-secret
```

禁止这些原文出现在 TUI render、diagnostics list、ToolDisplay、provider/MCP error、conversation JSONL、memory index、permission status、handoff 证据中。

## 共享文件 Owner 表

| 文件/目录 | Owner | 规则 |
|---|---|---|
| `internal/config/*` | Config owner | 同一时间只允许一个 agent 修改；schema/default/env 统一处理 |
| `config.example.yaml` | Config owner 或主会话 | 每个里程碑末尾统一更新，避免并行冲突 |
| `internal/diagnostics/*` | Diagnostics owner | 其他 agent 只调用稳定接口 |
| `internal/events/events.go` | 主会话 | T0 冻结字段后再由主会话统一修改 |
| `internal/app/update.go` | 主会话 | slash command、键盘事件、取消逻辑统一集成 |
| `internal/app/app.go` / `deps.go` | 主会话 | request session、collector 注入、wiring 统一处理 |
| `internal/orchestrator/chat.go` | 主会话 | ctx、diagnostics、permission event 汇合点 |
| `internal/tool/*` | Tool owner | executor 构造签名由 Config owner/主会话统一 |
| `internal/mcpclient/*` | MCP owner | 不直接改 app 命令入口 |
| `internal/conversation/*` | Conversation owner | 不直接改 app/tui list，先做 store/recovery API |
| `internal/instructions/*` | Instructions owner | config schema 稳定后再做候选文件 |
| `internal/tui/*` | TUI owner/主会话 | status/messages/help/confirmation/input 分阶段改 |
| `internal/provider/*` | Provider owner | HTTP timeout/error summary 只用共享 helper |
| `internal/redact/*` | Redaction owner | 运行期 redactor 和样本矩阵先冻结 |
| `internal/contextmgr/*` | Context owner | diagnostics 接入前不改 app/orchestrator |
| `internal/memory/*` | Memory owner | memory index/diagnostics 不直接改 TUI 命令 |
| `internal/sessionctx/*` | Sessionctx owner | 只输出 diagnostics，不直接改 event/app |
| `internal/permission/*` | Permission owner | 只生成 risk/scope/hint，不直接改 TUI 面板 |
| `cmd/xagent/main.go` | 主会话 | 所有 wiring 集中处理 |

## Handoff 模板

每个 agent 返回时必须包含：

```text
Task IDs:
Base commit:
Worktree/branch:
Owner lock acquired/released:
Changed files:
Shared files touched:
Files intentionally not touched:
Public API changes:
Config keys/defaults changed:
Diagnostics codes/severity added:
Redaction-sensitive paths:
Secret scan command/result:
Tests run:
  - command:
    result:
Tests not run and reason:
E2E evidence:
  - scenario:
    setup:
    user input:
    expected:
    observed:
    pass/fail:
AC mapping:
Known risks:
Known conflicts:
Required merge order:
Required follow-up by main session:
```

## T0: 合约冻结与安全策略

### T0.1 冻结敏感数据流分级

**文件：** `docs/usability-reliability-roadmap/plan.md`
**依赖：** 无

**步骤：**
1. 明确 `user-visible`、`model-visible`、`persisted`、`memory-indexable` 四级数据流。
2. 写清默认允许/禁止内容。
3. 写清 diagnostics、stdout/stderr、HTTP body、JSONL recovery、permission status 的默认流向。

**验证：** 文档中存在四级定义，且每级至少有一条禁止项。

### T0.2a 冻结静态 redaction 样本矩阵

**文件：** `internal/redact/redact.go`, `internal/redact/redact_test.go`
**依赖：** 无

**步骤：**
1. 扩展或确认 redactor 支持敏感样本矩阵。
2. 增加大小写 header、URL query、URL userinfo、Basic auth、PEM、嵌套 JSON 测试。

**验证：** `go test -count=1 ./internal/redact -run TestRedactSensitiveMatrix`

### T0.2b 冻结 RuntimeRedactor 生命周期与注入 API

**文件：** `docs/usability-reliability-roadmap/plan.md`, `internal/redact/redact_test.go`
**依赖：** T0.2a

**步骤：**
1. 明确 RuntimeRedactor 是 app 级实例，不使用全局 singleton。
2. 冻结 `config.LoadWithOptions(path, LoadOptions{Redactor})` 与兼容 `Load(path)` 的语义。
3. 冻结 provider/MCP/tool/diagnostics/conversation/memory/app deps 的 redactor options 注入路径。
4. 明确敏感配置字段展开值即使短或低熵也必须注册；非敏感自由文本才允许过滤。

**验证：** `go test -count=1 ./internal/redact -run TestRuntimeRedactorRegistersSecretsPerInstance`

### T0.2c 冻结 SafeHTTPErrorSummary helper

**文件：** `internal/diagnostics/http_summary.go`, `internal/diagnostics/collector_test.go`
**依赖：** T0.2a

**步骤：**
1. 定义 provider/MCP 共享 HTTP 错误摘要 helper。
2. 固定 max bytes、content-type、二进制 body、headers/request body 禁止记录规则。
3. 增加脱敏和截断测试。

**验证：** `go test -count=1 ./internal/diagnostics -run TestSafeHTTPErrorSummaryRedactsAndLimitsBody`

### T0.3 冻结 config schema

**文件：** `internal/config/config.go`
**依赖：** T0.2a

**步骤：**
1. 冻结 `llm.request_timeout_ms`、`agent.*`、`tool.*` 字段名。
2. 冻结 enabled 可选语义表示。
3. 写下注释或测试 fixture，说明旧配置兼容。

**验证：** `go test -count=1 ./internal/config -run TestConfigSchemaCompatibility`

### T0.4 冻结 events payload

**文件：** `internal/events/events.go`
**依赖：** T0.1

**步骤：**
1. 定义 `DiagnosticDisplay` 字段。
2. 扩展 `ToolDisplay` 字段，包含 artifact id/bytes/available，但保持零值兼容。
3. 扩展 `ToolConfirmationRequest` 字段，但保持现有按键语义。

**验证：** `go test -count=1 ./internal/events`

### T0.4a 冻结 diagnostics taxonomy

**文件：** `internal/diagnostics/diagnostic.go`, `docs/usability-reliability-roadmap/plan.md`
**依赖：** T0.4

**步骤：**
1. 明确 severity 枚举和含义。
2. 明确 code/source 命名规范。
3. 明确 hint 格式和同类错误复用 code 的规则。

**验证：** 文档中存在 diagnostics taxonomy 表；`go test -count=1 ./internal/diagnostics`。

### T0.5 冻结本地命令分发策略

**文件：** `docs/usability-reliability-roadmap/plan.md`
**依赖：** 无

**步骤：**
1. 明确 `/help`、`/diagnostics`、`/mcp status`、`/permissions status`、`/memory`、`/compact` 属于本地命令。
2. 明确未知 slash command 不发送给模型。
3. 明确 `app/update.go` 由主会话统一集成。

**验证：** 文档中列出本地命令表和未知命令策略。

### T0.6 冻结普通对话敏感数据例外

**文件：** `docs/usability-reliability-roadmap/plan.md`, `internal/conversation/*_test.go`, `internal/memory/*_test.go`
**依赖：** T0.1, T0.2b

**步骤：**
1. 明确用户普通输入和 provider 正常回复可作为会话恢复内容保存到 JSONL。
2. 明确派生摘要、会话列表、diagnostics、handoff、memory index、测试证据必须脱敏。
3. 测试 memory update/index 不从普通对话中收录敏感样本。

**验证：** `go test -count=1 ./internal/conversation ./internal/memory -run TestOrdinaryConversationSecretFlowPolicy`

### T0.7 冻结 artifact 原始内容例外

**文件：** `docs/usability-reliability-roadmap/plan.md`, `internal/tool/tool_test.go`
**依赖：** T0.1, T0.2b

**步骤：**
1. 明确超限工具输出 artifact 可保存原始完整内容，但只限本机私有目录。
2. 明确 artifact raw 不进入 provider messages、conversation JSONL、memory index、diagnostics、handoff、tmux 证据。
3. 明确默认只显示 artifact id/bytes，不显示绝对路径；模型不能通过 id 自动读取。

**验证：** `go test -count=1 ./internal/tool -run TestArtifactRawContentIsLocalPrivateOnly`

### T0.8 冻结权限持久化脱敏策略

**文件：** `docs/usability-reliability-roadmap/plan.md`, `internal/permission/*_test.go`
**依赖：** T0.2b

**步骤：**
1. 明确 permanent rule 只能保存结构化、最小化、脱敏后的 scope 摘要。
2. 命令/参数包含 runtime secret 或敏感样本时禁用 permanent，只允许 once/session。
3. `/permissions status` 和 handoff 不显示 raw command secret。

**验证：** `go test -count=1 ./internal/permission -run TestPermanentPermissionRulesNeverPersistSecrets`

## Epic M1: 启动配置与请求可控性

### TW.1 主入口注入 RuntimeRedactor 和配置加载 options

**文件：** `cmd/xagent/main.go`, `internal/app/deps.go`, `internal/config/load.go`, `internal/config/load_test.go`
**依赖：** T0.2b, T0.3

**步骤：**
1. 启动入口创建单个 app redactor。
2. 使用 `config.LoadWithOptions` 加载配置并注册展开出的 secret。
3. 将同一个 redactor 放入 app deps，供 provider/MCP/tool/diagnostics/conversation/memory/TUI 使用。
4. 保留 `config.Load(path)` 兼容 wrapper。

**验证：** `go test -count=1 ./cmd/xagent ./internal/app ./internal/config -run TestMainWiresRuntimeRedactor`

### TW.2 主入口注入 diagnostics collector

**文件：** `cmd/xagent/main.go`, `internal/app/deps.go`, `internal/orchestrator/chat.go`
**依赖：** T6.1, T7.2

**步骤：**
1. 启动入口创建单个 diagnostics collector。
2. app deps 和 orchestrator options 使用同一个 collector。
3. config/MCP/sessionctx/context/memory/conversation recovery diagnostics 进入 collector。

**验证：** `go test -count=1 ./internal/app ./internal/orchestrator -run TestMainWiresDiagnosticsCollector`

### TW.3 主入口注入 Agent/Tool/Provider 配置

**文件：** `cmd/xagent/main.go`, `internal/app/deps.go`, `internal/orchestrator/chat.go`, `internal/tool/executor.go`, `internal/provider/factory.go`
**依赖：** T3.1, T3.2, T3.2a, T4.1

**步骤：**
1. Orchestrator 使用 `agent.*` 配置。
2. Tool executor 使用 `tool.*` 配置。
3. Provider 使用 `llm.request_timeout_ms` 配置。
4. app/status 能展示达到限制后的继续建议。

**验证：** `go test -count=1 ./internal/app ./internal/orchestrator ./internal/tool ./internal/provider -run 'ConfiguredLimits|RequestTimeout'`

### T1.1 实现 enabled 可选解析

**文件：** `internal/config/config.go`, `internal/config/load_test.go`
**依赖：** T0.3

**步骤：**
1. 将 instructions/context/memory enabled 改为可区分未配置和显式 false。
2. 新增测试 fixture：未配置、enabled false、enabled true。

**验证：** `go test -count=1 ./internal/config -run TestLoadPreservesExplicitEnabledValues`

### T1.2 修复 enabled 默认值应用

**文件：** `internal/config/load.go`, `internal/config/load_test.go`
**依赖：** T1.1

**步骤：**
1. 未配置 enabled 时默认 true。
2. 显式 false 时不覆盖。
3. 保持旧 config 可加载。

**验证：** `go test -count=1 ./internal/config -run TestApplyDefaultsDoesNotOverrideExplicitDisabled`

### T1.3 验证 disabled 子系统不注入

**文件：** `internal/sessionctx/manager_test.go`, `internal/contextmgr/manager_test.go`, `internal/memory/manager_test.go`
**依赖：** T1.2

**步骤：**
1. instructions disabled 时，即使指令文件存在，也不进入 stable sections。
2. context disabled 时，不执行摘要/外置。
3. memory disabled 时，不注入 memory index。

**验证：** `go test -count=1 ./internal/sessionctx ./internal/contextmgr ./internal/memory -run 'Disabled|ExplicitDisabled'`

### T2.1 支持 llm.api_key 环境变量展开

**文件：** `internal/config/load.go`, `internal/config/load_test.go`
**依赖：** T0.2b, T1.2

**步骤：**
1. 仅对 allowlist 字段 `llm.api_key` 做 `${ENV}` 展开。
2. 展开成功后注册运行期 secret。
3. 缺失 env 只显示变量名。

**验证：** `go test -count=1 ./internal/config -run TestLLMAPIKeyEnvExpansion`

### T2.2 启动配置错误给出修复建议

**文件：** `internal/config/validate.go`, `cmd/xagent/main.go`, `internal/config/validate_test.go`
**依赖：** T2.1

**步骤：**
1. 配置文件缺失时提示复制示例配置。
2. LLM 必填字段缺失时提示字段名和建议。
3. `${MISSING_ENV}` 缺失时提示 export 变量名。
4. 错误中不得出现已展开 secret。

**验证：** `go test -count=1 ./internal/config -run TestConfigErrorsIncludeActionableHintsAndRedactSecrets`

### T2.3 更新 config.example.yaml

**文件：** `config.example.yaml`
**依赖：** T2.2, T3.2, T3.2a

**步骤：**
1. 使用 `${ANTHROPIC_API_KEY}` / `${OPENAI_API_KEY}` 示例。
2. 增加“不建议写明文 API key”注释。
3. 增加 request timeout、agent、tool 示例。

**验证：** 手动检查示例不含真实 key；`go test -count=1 ./internal/config`

### T3.1 新增 AgentConfig 默认值和校验

**文件：** `internal/config/config.go`, `internal/config/load.go`, `internal/config/validate.go`, `internal/config/load_test.go`
**依赖：** T0.3

**步骤：**
1. 增加 `agent.max_iterations`、`agent.max_unknown_tool_calls`。
2. 默认值保持 10/2。
3. 校验必须大于 0。

**验证：** `go test -count=1 ./internal/config -run TestAgentConfigDefaultsAndValidation`

### T3.2 新增 ToolConfig 默认值和校验

**文件：** `internal/config/config.go`, `internal/config/load.go`, `internal/config/validate.go`, `internal/config/load_test.go`
**依赖：** T0.3

**步骤：**
1. 增加 `tool.timeout_ms`、`tool.max_output_bytes`。
2. 默认值保持 30000/32768。
3. 校验必须大于 0。

**验证：** `go test -count=1 ./internal/config -run TestToolConfigDefaultsAndValidation`

### T3.2a 新增 LLM request timeout 默认值和校验

**文件：** `internal/config/config.go`, `internal/config/load.go`, `internal/config/validate.go`, `internal/config/load_test.go`
**依赖：** T0.3

**步骤：**
1. 增加 `llm.request_timeout_ms`。
2. 设置合理默认值。
3. 校验必须大于 0。

**验证：** `go test -count=1 ./internal/config -run TestLLMRequestTimeoutDefaultsAndValidation`

### T3.3 Orchestrator 使用 AgentConfig

**文件：** `internal/orchestrator/run_request.go`, `internal/orchestrator/agent_loop.go`, `internal/orchestrator/chat_test.go`
**依赖：** T3.1

**步骤：**
1. OrchestratorOptions 接收 AgentConfig 或 RunOptions。
2. `max_iterations=1` 时一轮后停止。
3. `max_unknown_tool_calls=1` 时首次未知工具后停止。

**验证：** `go test -count=1 ./internal/orchestrator -run TestAgentOptionsControlLoopLimits`

### T3.4 Tool executor 使用 ToolConfig

**文件：** `internal/tool/executor.go`, `internal/tool/tool_test.go`
**依赖：** T3.2

**步骤：**
1. Executor 构造从配置传 timeout/max output，并保留兼容构造或同步更新所有调用方。
2. 小 timeout 触发工具超时。
3. 小 max output 触发截断。

**验证：** `go test -count=1 ./internal/tool -run TestExecutorUsesConfiguredLimits`

### T4.1 Provider HTTP client 使用 request timeout

**文件：** `internal/provider/factory.go`, `internal/provider/*_test.go`
**依赖：** T2.1, T3.2a

**步骤：**
1. 根据 `llm.request_timeout_ms` 创建 HTTP client。
2. 使用阻塞 `httptest.Server` 验证超时。
3. 错误不包含 API key。

**验证：** `go test -count=1 ./internal/provider -run TestProviderRequestTimeout`

### T4.2 Provider HTTP 错误摘要脱敏

**文件：** `internal/provider/openai.go`, `internal/provider/anthropic.go`, `internal/provider/*_test.go`
**依赖：** T0.2c

**步骤：**
1. 非成功响应调用共享 SafeHTTPErrorSummary helper。
2. body preview 经过 RuntimeRedactor。
3. 不展示 request headers/body。

**验证：** `go test -count=1 ./internal/provider -run TestProviderHTTPErrorSummaryRedactsSecrets`

### T5.1 App 保存请求 cancel 状态

**文件：** `internal/app/app.go`, `internal/app/update.go`, `internal/app/update_test.go`
**依赖：** T4.1

**步骤：**
1. Model 增加当前 request session。
2. 发送请求时使用可取消 ctx。
3. Done/Error 时清理 request session。

**验证：** `go test -count=1 ./internal/app -run TestModelTracksRequestSessionLifecycle`

### T5.2 Esc/Ctrl+C 取消 streaming 请求

**文件：** `internal/app/update.go`, `internal/app/update_test.go`
**依赖：** T5.1

**步骤：**
1. streaming 中 Esc 取消请求。
2. streaming 中 ctrl+c 先取消请求，空闲时退出。
3. 取消后输入恢复可用。

**验证：** `go test -count=1 ./internal/app -run TestStreamingKeysCancelBeforeQuit`

### T5.3 取消链路覆盖 provider/tool/context

**文件：** `internal/orchestrator/chat_test.go`, `internal/tool/tool_test.go`, `internal/contextmgr/manager_test.go`
**依赖：** T5.2

**步骤：**
1. fake provider 阻塞直到 ctx cancel。
2. fake long-running tool 响应 ctx cancel。
3. context prepare 使用同一 ctx。

**验证：** `go test -count=1 ./internal/orchestrator ./internal/tool ./internal/contextmgr -run 'Cancel|ContextCanceled'`

## Epic M2: Diagnostics 与错误可见性

### T6.1 实现 diagnostics Collector

**文件：** `internal/diagnostics/collector.go`, `internal/diagnostics/collector_test.go`
**依赖：** T0.2a

**步骤：**
1. 实现线程安全 Add/List/Clear/Count。
2. 限制最大条数和单条长度。
3. List 返回副本。

**验证：** `go test -count=1 ./internal/diagnostics -run TestCollectorStoresBoundedCopies`

### T6.2 Collector 脱敏测试

**文件：** `internal/diagnostics/collector_test.go`
**依赖：** T6.1

**步骤：**
1. 将敏感样本写入 diagnostics。
2. 断言 List/format 输出不含原文。
3. 断言字段名可保留但值被替换。

**验证：** `go test -count=1 ./internal/diagnostics -run TestCollectorRedactsSensitiveDiagnostics`

### T7.1 App 本地命令分发骨架

**文件：** `internal/app/update.go`, `internal/app/update_test.go`
**依赖：** T0.5

**步骤：**
1. 抽出本地 slash command 分发函数。
2. 保持 `/compact`、`/memory` 行为不变。
3. 未知 `/xxx` 返回本地错误，不发送给模型。

**验证：** `go test -count=1 ./internal/app -run TestLocalSlashCommandDispatch`

### T7.2 App 注入 diagnostics Collector

**文件：** `internal/app/deps.go`, `internal/app/app.go`, `internal/app/update_test.go`
**依赖：** T6.1

**步骤：**
1. Deps 增加 collector。
2. Model 保存 collector。
3. 空 collector 不影响现有行为。

**验证：** `go test -count=1 ./internal/app -run TestDiagnosticsCollectorInjection`

### T7.3 实现 `/diagnostics`

**文件：** `internal/app/update.go`, `internal/app/update_test.go`, `internal/tui/status.go`
**依赖：** T7.1, T7.2

**步骤：**
1. 空状态显示“暂无诊断”。
2. 非空状态显示 code/severity/source/message/hint。
3. 输出标注“本地诊断，不会发送给模型”。
4. 输出脱敏。

**验证：** `go test -count=1 ./internal/app -run TestDiagnosticsCommandDisplaysRedactedLocalDiagnostics`

### T7.4 Diagnostic event 接入

**文件：** `internal/events/events.go`, `internal/app/update.go`, `internal/app/update_test.go`
**依赖：** T0.4, T7.2

**步骤：**
1. 新增 Diagnostic event 类型。
2. App 收到 event 后写入 collector/status。
3. 不把 diagnostic 追加为 conversation message。

**验证：** `go test -count=1 ./internal/app ./internal/events -run TestDiagnosticEventIsNotConversationMessage`

### T8.1 sessionctx diagnostics 接入 orchestrator

**文件：** `internal/orchestrator/chat.go`, `internal/orchestrator/chat_test.go`
**依赖：** T7.4

**步骤：**
1. `PreparedContext.Diagnostics` 写入 collector。
2. 发 Diagnostic event。
3. 不进入 provider messages。

**验证：** `go test -count=1 ./internal/orchestrator -run TestSessionContextDiagnosticsAreEmittedNotSentToProvider`

### T8.2 context/memory diagnostics 接入

**文件：** `internal/contextmgr/manager.go`, `internal/memory/manager.go`, `internal/orchestrator/chat_test.go`
**依赖：** T8.1

**步骤：**
1. context 摘要失败/熔断进入 collector。
2. memory index 损坏进入 collector。
3. diagnostics 不进入 memory index。

**验证：** `go test -count=1 ./internal/contextmgr ./internal/memory ./internal/orchestrator -run 'Diagnostics|MemoryIndexCorrupt'`

### T9.1 MCP validation diagnostics 接入 collector

**文件：** `internal/config/validate.go`, `internal/mcpclient/manager.go`, `internal/mcpclient/*_test.go`
**依赖：** T2.1, T7.2

**步骤：**
1. env 缺失导致 server disabled 时保留 diagnostic。
2. diagnostic 包含 server 名和变量名。
3. 不包含 env value。

**验证：** `go test -count=1 ./internal/config ./internal/mcpclient -run TestMCPMissingEnvDiagnosticIsVisibleAndRedacted`

### T9.2 MCP HTTP/stdio 错误安全摘要

**文件：** `internal/mcpclient/http.go`, `internal/mcpclient/stdio.go`, `internal/mcpclient/*_test.go`
**依赖：** T0.2c

**步骤：**
1. HTTP 非 2xx 只读取有限 body 前缀。
2. stdio stderr 限长脱敏。
3. 不记录 request headers/body。
4. 二进制 body 只显示 content-type/size。

**验证：** `go test -count=1 ./internal/mcpclient -run TestMCPErrorSummariesAreBoundedAndRedacted`

### T9.3 实现 `/mcp status`

**文件：** `internal/app/update.go`, `internal/app/update_test.go`, `internal/mcpclient/manager.go`
**依赖：** T7.1, T9.1, T9.2

**步骤：**
1. 显示 server 名、状态、错误分类、脱敏摘要。
2. 空状态可读。
3. 标注本地状态，不发送给模型。

**验证：** `go test -count=1 ./internal/app ./internal/mcpclient -run TestMCPStatusCommandDisplaysRedactedDetails`

## Epic M3: 工具与权限体验

### T10.0 定义所有工具结果的数据流策略

**文件：** `internal/tool/tool.go`, `internal/tool/tool_test.go`
**依赖：** T0.1, T0.2b

**步骤：**
1. 明确成功和失败工具结果的 user/model/persisted/memory 可见摘要策略。
2. 成功 stdout 含 secret 时也只生成脱敏摘要。
3. 工具输出默认不进入 memory-indexable 内容。

**验证：** `go test -count=1 ./internal/tool -run TestToolResultVisibilityPolicyAppliesToSuccessAndFailure`

### T10.1 Bash 失败生成模型可见摘要

**文件：** `internal/tool/bash.go`, `internal/tool/tool_test.go`
**依赖：** T10.0, T3.4

**步骤：**
1. 非 0 退出时 Content 包含 exit code、脱敏 stdout 摘要、脱敏 stderr 摘要。
2. 模型可见摘要限长。
3. 不包含敏感样本原文。

**验证：** `go test -count=1 ./internal/tool -run TestBashFailureContentIncludesRedactedStdoutStderr`

### T10.2 UTF-8 安全截断

**文件：** `internal/tool/executor.go`, `internal/tool/tool_test.go`
**依赖：** T3.4

**步骤：**
1. 截断不切断 rune。
2. 标记 truncated。
3. 截断摘要仍脱敏。

**验证：** `go test -count=1 ./internal/tool -run TestExecutorTruncatesUTF8Safely`

### T10.3a 定义 artifact 存储 helper

**文件：** `internal/tool/executor.go`, `internal/tool/tool_test.go`
**依赖：** T10.2

**步骤：**
1. 定义 artifact 私有目录解析 helper。
2. 断言目录不在 repo 工作区。
3. 断言目录权限 0700。

**验证：** `go test -count=1 ./internal/tool -run TestToolArtifactDirectoryIsPrivateAndOutsideRepo`

### T10.3b 超大输出写入私有 artifact

**文件：** `internal/tool/executor.go`, `internal/tool/tool_test.go`
**依赖：** T10.3a

**步骤：**
1. 超大输出写入 artifact 文件。
2. 文件权限 0600。
3. ArtifactRef 记录 id/bytes，不记录 raw secret。

**验证：** `go test -count=1 ./internal/tool -run TestLargeToolOutputWritesPrivateArtifact`

### T10.3c 模型摘要不暴露 artifact 路径

**文件：** `internal/tool/executor.go`, `internal/tool/tool_test.go`
**依赖：** T10.3b

**步骤：**
1. 模型可见摘要只说明完整输出本地可用。
2. 模型可见摘要不包含绝对路径。
3. TUI 可见字段使用 artifact id。

**验证：** `go test -count=1 ./internal/tool -run TestArtifactPathIsNotModelVisible`

### T10.3d artifact 清理策略

**文件：** `internal/tool/executor.go`, `internal/tool/tool_test.go`
**依赖：** T10.3b

**步骤：**
1. 定义 artifact TTL 或最大保留数量。
2. 提供启动或显式清理入口的 helper。
3. 测试过期 artifact 可清理，未过期 artifact 保留。

**验证：** `go test -count=1 ./internal/tool -run TestToolArtifactCleanupPolicy`

### T10.4 工具参数错误不伪装成权限拒绝

**文件：** `internal/tool/executor.go`, `internal/tool/tool_test.go`
**依赖：** T10.1

**步骤：**
1. NormalizeCall 参数错误返回 invalid/config error。
2. 不显示“Permission denied”。
3. 错误 recoverable。

**验证：** `go test -count=1 ./internal/tool -run TestInvalidToolArgumentsAreNotPermissionDenied`

### T11.1 ToolDisplay 提取错误详情

**文件：** `internal/orchestrator/tool_batches.go`, `internal/orchestrator/*_test.go`
**依赖：** T0.4, T10.1, T10.3c

**步骤：**
1. 从 tool.Result 提取 error code、stdout、stderr、truncated、artifact ref。
2. 所有字段脱敏。
3. 模型上下文和 TUI display 使用不同摘要。

**验证：** `go test -count=1 ./internal/orchestrator -run TestToolDisplayExtractsRedactedFailureDetails`

### T11.2 模型后续轮次可见失败摘要

**文件：** `internal/orchestrator/chat_test.go`
**依赖：** T11.1

**步骤：**
1. fake provider 第一轮触发失败工具。
2. 第二轮断言收到 tool result 中有 exit code/stdout/stderr 脱敏摘要。
3. 断言不含敏感样本原文。

**验证：** `go test -count=1 ./internal/orchestrator -run TestProviderReceivesRedactedToolFailureSummaryNextTurn`

### T11.3 TUI 展示工具错误详情

**文件：** `internal/tui/messages.go`, `internal/tui/messages_test.go`
**依赖：** T11.1

**步骤：**
1. 工具错误行显示 exit code/error code。
2. 显示 stdout/stderr 摘要。
3. 显示 truncated/artifact 提示。
4. 不显示 secret 原文。

**验证：** `go test -count=1 ./internal/tui -run TestToolErrorDisplayShowsRedactedDetails`

### T12.1 权限确认风险字段生成

**文件：** `internal/permission/*`, `internal/orchestrator/chat_test.go`
**依赖：** T0.4, T0.8

**步骤：**
1. Bash 生成非沙箱 warning。
2. warning 包含文件系统、网络、环境变量、破坏性写入风险。
3. 高风险 Bash 命令不提供 permanent 授权，只允许 once/session，并显示撤销/风险说明。
4. 命令或参数包含 runtime secret 时禁用 permanent，并确保不会持久化 raw command secret。

**验证：** `go test -count=1 ./internal/permission ./internal/orchestrator -run TestPermissionWarningForBashRisk`

### T12.2 权限确认面板渲染

**文件：** `internal/tui/confirmation.go`, `internal/tui/*_test.go`
**依赖：** T12.1

**步骤：**
1. 显示工具名、命令/路径、风险、权限模式、授权范围。
2. 显示 `[Enter/n] 拒绝`。
3. permanent 显示撤销提示。
4. 所有命令参数脱敏。

**验证：** `go test -count=1 ./internal/tui -run TestConfirmationPanelRendersRiskAndRedactsArguments`

### T12.3 App 使用权限确认面板

**文件：** `internal/app/update.go`, `internal/app/update_test.go`
**依赖：** T12.2

**步骤：**
1. 保持 y/s/p/n/esc/enter 语义。
2. footer 或 panel 显示新文案。
3. Enter 仍拒绝但文案明确。

**验证：** `go test -count=1 ./internal/app -run TestConfirmationKeysRemainCompatibleWithPanel`

### T13.1 `/help` 基础版和未知命令

**文件：** `internal/tui/help.go`, `internal/app/update.go`, `internal/app/update_test.go`, `internal/tui/help_test.go`
**依赖：** T7.1

**步骤：**
1. `/help` 显示本地命令和快捷键。
2. 未知 `/foo` 本地报错，不发送给模型。
3. `/plan`、`/do` 空输入提示用法。

**验证：** `go test -count=1 ./internal/app ./internal/tui -run 'TestHelp|TestUnknownSlashCommand|TestPlanDoUsage'`

### T14.1 `/permissions status`

**文件：** `internal/app/update.go`, `internal/app/update_test.go`, `internal/permission/*`
**依赖：** T12.3, T13.1

**步骤：**
1. 显示当前 permission mode。
2. 显示 session/permanent grants 脱敏摘要或空状态。
3. 标注本地状态，不发送给模型。

**验证：** `go test -count=1 ./internal/app ./internal/permission -run TestPermissionsStatusCommandRedactsRules`

### T13.2 `/help` 补充权限/MCP/diagnostics 细节

**文件：** `internal/tui/help.go`, `internal/tui/help_test.go`
**依赖：** T9.3, T14.1

**步骤：**
1. help 包含 `/diagnostics`、`/mcp status`、`/permissions status`、`/memory`、`/compact`、`/plan`、`/do`。
2. help 包含 Bash 非沙箱简短说明和 permission modes 简述。
3. help 不包含任何 runtime secret。

**验证：** `go test -count=1 ./internal/tui -run TestHelpIncludesStatusCommandsAndSecurityNotes`

## Epic M4: 会话恢复、指令兼容与 TUI 长输入

### T15.1 JSONL 坏行列表占位

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T0.2a

**步骤：**
1. List 遇到坏 JSONL 返回可恢复占位。
2. diagnostics 只包含行号、错误类型、可恢复状态。
3. 不包含坏行原文。

**验证：** `go test -count=1 ./internal/conversation -run TestListIncludesRecoverableCorruptJSONLWithoutLeakingLine`

### T15.2 恢复后保存 repair

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/recovery.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T15.1

**步骤：**
1. Recover 跳过坏行后继续对话。
2. Save 不再被旧坏行阻断。
3. 如写 repair/snapshot，权限和脱敏符合规则。

**验证：** `go test -count=1 ./internal/conversation -run TestRecoveredCorruptJSONLCanSaveAgain`

### T15.3 TUI 列表显示 recovery 状态

**文件：** `internal/app/app.go`, `internal/tui/list.go`, `internal/tui/*_test.go`
**依赖：** T7.2, T15.2

**步骤：**
1. 列表显示“需要恢复”或等价标记。
2. 恢复诊断进入 collector。
3. 不显示坏行原文。

**验证：** `go test -count=1 ./internal/app ./internal/tui -run TestConversationListShowsRecoverableSession`

### T16.1 指令候选文件兼容

**文件：** `internal/instructions/loader.go`, `internal/instructions/loader_test.go`
**依赖：** T1.2

**步骤：**
1. 支持当前默认文件、`CLAUDE.md`、`AGENTS.md`。
2. 显式 project_file 优先。
3. include 安全保持不变。

**验证：** `go test -count=1 ./internal/instructions -run TestLoaderUsesInstructionFallbackFiles`

### T16.2 指令候选 diagnostics

**文件：** `internal/instructions/loader.go`, `internal/instructions/loader_test.go`
**依赖：** T7.2, T16.1

**步骤：**
1. 候选存在但未加载时产生 diagnostic。
2. 路径显示使用相对路径或 home 缩写。
3. diagnostic 脱敏。

**验证：** `go test -count=1 ./internal/instructions -run TestLoaderReportsUnusedInstructionCandidatesSafely`

### T17.1 stop reason metadata

**文件：** `internal/conversation/conversation.go`, `internal/orchestrator/agent_loop.go`, `internal/conversation/*_test.go`
**依赖：** T3.3, T5.2, T15.2

**步骤：**
1. Conversation metadata 记录 stop reason/message。
2. max iterations、provider timeout、user cancel 均有 stop reason。
3. 旧记录兼容。

**验证：** `go test -count=1 ./internal/conversation ./internal/orchestrator -run TestConversationStoresStopReasonMetadata`

### T17.2 会话列表显示摘要或停止原因

**文件：** `internal/tui/list.go`, `internal/tui/*_test.go`
**依赖：** T17.1

**步骤：**
1. 列表显示 stop reason 或最近摘要。
2. 空值时回退原标题。
3. 长文本截断安全。

**验证：** `go test -count=1 ./internal/tui -run TestConversationListShowsStopReasonOrSummary`

### T18.1a 输入状态支持多行文本

**文件：** `internal/tui/input.go`, `internal/tui/*_test.go`
**依赖：** T5.2, T12.3, T13.1

**步骤：**
1. 输入缓冲支持多行文本。
2. 现有单行输入行为保持兼容。
3. Clear/Value 对多行文本工作正常。

**验证：** `go test -count=1 ./internal/tui -run TestInputStoresMultilineText`

### T18.1b 多行粘贴不破坏输入缓冲

**文件：** `internal/tui/input.go`, `internal/tui/*_test.go`
**依赖：** T18.1a

**步骤：**
1. 粘贴包含换行的文本。
2. 断言换行被保留。
3. 断言光标/缓冲状态可继续编辑。

**验证：** `go test -count=1 ./internal/tui -run TestInputPasteMultilineText`

### T18.1c 定义提交与换行按键

**文件：** `internal/app/update.go`, `internal/tui/input.go`, `internal/app/update_test.go`
**依赖：** T18.1b

**步骤：**
1. 明确 Enter/组合键的提交与换行规则。
2. 空输入不提交。
3. 多行输入提交后清空。

**验证：** `go test -count=1 ./internal/app ./internal/tui -run TestMultilineSubmitAndNewlineKeys`

### T18.1d streaming/confirmation/list 焦点优先级不变

**文件：** `internal/app/update.go`, `internal/app/update_test.go`
**依赖：** T18.1c

**步骤：**
1. streaming 时输入不可提交且 Esc/Ctrl+C 取消优先。
2. confirmation 时 y/s/p/n/enter/esc 仍由确认处理。
3. list screen Enter 仍选择会话。

**验证：** `go test -count=1 ./internal/app -run TestMultilineInputDoesNotBreakFocusPriority`

### T18.2 自适应布局渲染

**文件：** `internal/tui/messages.go`, `internal/tui/input.go`, `internal/tui/*_test.go`
**依赖：** T18.1d

**步骤：**
1. 40/80/120 列宽渲染不重叠。
2. 长消息换行。
3. 多行输入高度限制。

**验证：** `go test -count=1 ./internal/tui -run TestResponsiveLayoutWidths`

## 安全与验收横向任务

### TS.1a Provider/MCP HTTP body 脱敏矩阵

**文件：** `internal/provider/*_test.go`, `internal/mcpclient/*_test.go`
**依赖：** T4.2, T9.2

**步骤：**
1. 将敏感样本注入 provider HTTP error body 和 MCP HTTP body。
2. 断言错误摘要、diagnostics、status 输出不含原文。
3. 断言 request headers/body 未被记录。

**验证：** `go test -count=1 ./internal/provider ./internal/mcpclient -run TestHTTPErrorBodySecretMatrix`

### TS.1b Tool stdout/stderr 与 ToolDisplay 脱敏矩阵

**文件：** `internal/tool/tool_test.go`, `internal/orchestrator/*_test.go`, `internal/tui/messages_test.go`
**依赖：** T10.1, T11.3

**步骤：**
1. 将敏感样本注入成功 stdout、失败 stdout/stderr。
2. 断言模型可见摘要、ToolDisplay、TUI render 不含原文。
3. 断言成功工具输出也遵守数据流矩阵。

**验证：** `go test -count=1 ./internal/tool ./internal/orchestrator ./internal/tui -run TestToolOutputSecretMatrix`

### TS.1c Diagnostics/App/TUI 脱敏矩阵

**文件：** `internal/diagnostics/collector_test.go`, `internal/app/update_test.go`, `internal/tui/*_test.go`
**依赖：** T6.2, T7.3

**步骤：**
1. 将敏感样本注入 diagnostics message/hint/source/path。
2. 断言 collector、`/diagnostics`、status render 不含原文。
3. 断言 diagnostics 不作为 conversation message。

**验证：** `go test -count=1 ./internal/diagnostics ./internal/app ./internal/tui -run TestDiagnosticsSecretMatrix`

### TS.1d Conversation JSONL/recovery 持久化脱敏矩阵

**文件：** `internal/conversation/*_test.go`
**依赖：** T15.2

**步骤：**
1. 将敏感样本注入坏 JSONL 行和 recovery diagnostics。
2. 断言新 JSONL/snapshot/diagnostics 不复制坏行原文。
3. 断言只保存行号、错误类型、可恢复状态。

**验证：** `go test -count=1 ./internal/conversation -run TestConversationRecoverySecretMatrix`

### TS.1e Memory index 禁止收录错误详情和工具输出

**文件：** `internal/memory/*_test.go`
**依赖：** T8.2

**步骤：**
1. 将敏感样本注入 diagnostics、工具输出和错误详情。
2. 触发 memory update/index rebuild。
3. 断言 memory index 不包含错误详情、工具输出和敏感原文。

**验证：** `go test -count=1 ./internal/memory -run TestMemoryIndexRejectsDiagnosticAndToolOutputSecrets`

### TS.1f Permission status/confirmation 脱敏矩阵

**文件：** `internal/permission/*_test.go`, `internal/tui/confirmation_test.go`, `internal/app/update_test.go`
**依赖：** T12.2, T14.1

**步骤：**
1. 将敏感样本注入命令参数、URL query、headers。
2. 断言 confirmation panel 和 `/permissions status` 不含原文。
3. 断言高风险 Bash 不提供 permanent。

**验证：** `go test -count=1 ./internal/permission ./internal/tui ./internal/app -run TestPermissionSecretMatrix`

### TS.2 fake/local provider 与 MCP fixture

**文件：** `internal/testutil/*`, `docs/usability-reliability-roadmap/checklist.md`
**依赖：** T4.1, T9.2

**步骤：**
1. 提供 fake streaming provider，可配置成功回复、阻塞、timeout、工具调用、HTTP 401/403 body。
2. 提供 fake MCP stdio/HTTP server，可配置 missing env、stderr、protocol error、HTTP body。
3. 所有 fixture 只使用敏感样本矩阵中的 fake secret。
4. checklist 写明启动命令、临时 config 模板、tmux 命令和 capture 断言。

**验证：** `go test -count=1 ./internal/testutil ./internal/provider ./internal/mcpclient -run 'FakeProvider|FakeMCP'`

### TS.3 标准 secret scan 命令

**文件：** `docs/usability-reliability-roadmap/checklist.md`
**依赖：** TS.2

**步骤：**
1. 定义对 tmux capture、测试日志、conversation JSONL、memory index、artifact metadata、handoff 文本执行的统一扫描命令。
2. 扫描敏感样本矩阵中的每个 fake secret 原文。
3. 规定 artifact raw 文件不直接贴入证据，只检查权限和元数据；如需内容检查，在本地执行 scan 后只记录 pass/fail。

**验证：** checklist 中存在标准 secret scan 命令模板。

### TS.4 AC 到测试映射表

**文件：** `docs/usability-reliability-roadmap/checklist.md`
**依赖：** checklist 阶段

**步骤：**
1. 为 AC1-AC22 建立任务、单元测试、集成测试、tmux E2E 映射。
2. 标注是否涉及脱敏。
3. 标注 fake/local server 要求。

**验证：** checklist 中每个 AC 至少有一条可观测验证。

## Milestone 验收节点

### M1.V1a 配置错误启动验收

**依赖：** T2.2

**步骤：**
1. 使用缺失 config 启动。
2. 使用缺少 LLM 必填字段的临时 config 启动。
3. 使用缺失 env 的 config 启动。

**期望：** 输出包含修复建议和变量名；不含 secret 原文；执行 `go test -count=1 ./...` 通过。

### M1.V1b env key 成功请求验收

**依赖：** T2.1, T4.1

**步骤：**
1. 使用 fake/local provider 和 `${XAGENT_TEST_API_KEY}`。
2. 启动 TUI 并发送简单请求。
3. capture-pane 检查成功回复。

**期望：** 请求成功；capture-pane 不含 env value。

### M1.V1c enabled=false 子系统验收

**依赖：** T1.3

**步骤：**
1. 用临时 config 分别设置 instructions/context/memory disabled。
2. 发送请求到 fake provider。
3. 检查 provider 收到的系统上下文。

**期望：** 对应子系统内容不注入。

### M1.V1d agent/tool 限制 E2E 验收

**依赖：** T3.3, T3.4, TW.3

**步骤：**
1. 使用 fake provider 连续触发工具调用，配置 `agent.max_iterations=1`。
2. 使用 fake provider 触发未知工具，配置 `agent.max_unknown_tool_calls=1`。
3. 使用长运行 fake tool，配置很小 `tool.timeout_ms`。
4. 使用大输出 fake tool，配置很小 `tool.max_output_bytes`。
5. capture-pane 检查达到限制后的继续/调大配置/缩小任务提示。

**期望：** 所有限制均按配置生效；TUI/status/stop message 给出可操作建议；capture-pane 不含 secret。

### M1.V2a provider timeout 可见验收

**依赖：** T4.1, T5.1

**步骤：**
1. fake/local provider 延迟超过 `llm.request_timeout_ms`。
2. 启动 TUI 并发送请求。
3. capture-pane 检查 timeout 文案。

**期望：** TUI 显示超时分类和建议，输入恢复可用，`go test -count=1 ./...` 通过。

### M1.V2b 请求取消后继续输入验收

**依赖：** T5.2

**步骤：**
1. 使用 fake/local provider 启动 XAgent。
2. 发送会阻塞的请求。
3. 按 Esc 取消。
4. 再发送第二条请求。

**期望：** 出现“已取消/可继续输入”等可见文本；第二条请求可发送；capture-pane 不含 secret。

### M2.V1a include 越界 diagnostic 验收

**依赖：** T8.1

**步骤：**
1. 构造 include 越界项目指令。
2. 启动 TUI 后输入 `/diagnostics`。

**期望：** 显示 include/path escape 类 code/source/hint；不显示敏感路径原文。

### M2.V1b memory index 损坏 diagnostic 验收

**依赖：** T8.2

**步骤：**
1. 写入损坏 memory index。
2. 启动 TUI 后触发请求或 `/diagnostics`。

**期望：** 显示 memory index rebuilt/corrupt 诊断；不显示 note 原始敏感内容。

### M2.V1c context 摘要失败 diagnostic 验收

**依赖：** T8.2

**步骤：**
1. 使用 fake provider 让 context summary 失败。
2. 输入 `/diagnostics`。

**期望：** 显示 context summary failure 或 circuit breaker 诊断，应用不中断。

### M2.V1d MCP env 缺失 status 验收

**依赖：** T9.3

**步骤：**
1. 配置 MCP server 使用缺失 env。
2. 输入 `/mcp status`。

**期望：** 显示 server 名、缺失变量名和建议；不显示变量值。

### M2.V1e MCP stdio/HTTP 错误摘要验收

**依赖：** T9.2, T9.3

**步骤：**
1. 构造 stdio stderr 含 fake secret。
2. 构造 HTTP 401/403 body 含 fake secret。
3. 输入 `/mcp status` 和 `/diagnostics`。

**期望：** 显示 status/code/脱敏摘要；不显示 raw body/stderr。

### M3.V1a Bash 失败详情和模型后续轮次验收

**依赖：** T10.1, T11.2, T11.3

**步骤：**
1. fake provider 第一轮请求 Bash 失败：stdout=`out success contains success-secret`、stderr=`err sk-test-secret`、exit=7。
2. capture-pane 检查工具错误展示包含 exit code/stdout/stderr 脱敏摘要。
3. fake provider 第二轮记录收到的 tool result/message。
4. 检查第二轮 provider 输入含失败分类和脱敏 stdout/stderr 摘要，不含敏感样本原文。

**期望：** TUI 和模型后续轮次都能看到足够失败详情；raw secret 不进入 TUI、provider messages、JSONL 或 handoff。

### M3.V1b 超大输出 artifact 验收

**依赖：** T10.3a-T10.3d, T11.3

**步骤：**
1. 触发超大 stdout。
2. 检查 TUI 截断和 artifact id 提示。
3. 检查 artifact 目录不在 repo、权限 0700/0600。

**期望：** 用户可追溯完整输出；模型上下文不含 artifact 绝对路径。

### M3.V1c 权限确认和 permissions status 验收

**依赖：** T0.8, T12.1-T14.1

**步骤：**
1. 触发高风险 Bash 权限确认，例如包含网络/环境变量/破坏性写入或 fake secret 参数的命令。
2. 检查确认面板显示非沙箱 warning，且不提供 permanent 授权。
3. 触发低风险可永久授权的命令或路径规则，选择 permanent。
4. 输入 `/permissions status`，检查显示脱敏规则摘要、权限模式和撤销提示。
5. 执行标准 secret scan。

**期望：** 高风险路径不能 permanent；低风险 permanent 有落点/撤销/status；任何状态或证据不显示 fake secret 原文。

### M4.V1a 坏 JSONL 恢复验收

**依赖：** T15.1-T15.3

**步骤：**
1. 构造坏 JSONL。
2. 启动 TUI 查看会话列表。
3. 恢复后继续发送消息并保存。

**期望：** 列表显示可恢复，保存成功，不显示坏行原文。

### M4.V1b 指令兼容验收

**依赖：** T16.1-T16.2

**步骤：**
1. 项目只放 `CLAUDE.md`。
2. 启动 TUI 并发送请求。
3. 输入 `/diagnostics`。

**期望：** 指令被加载或有安全提示；不读取越界 include。

### M4.V1c 会话停止原因列表验收

**依赖：** T17.1-T17.2

**步骤：**
1. 制造 max iterations 或用户取消停止原因。
2. 回到会话列表。

**期望：** 列表显示 stop reason 或摘要。

### M4.V1d 多行输入和布局验收

**依赖：** T18.1a-T18.2

**步骤：**
1. 粘贴多行输入。
2. 调整 tmux 宽度到 40 和 120。
3. 输入 `/help`、未知 `/foo`、`/plan`、`/do`。

**期望：** 多行输入、布局、help、未知命令提示均可见且不重叠。

## Wave-based 多 Agent 协作

### Wave 0：主会话合约冻结

执行 T0.1-T0.8（包含 T0.2a/T0.2b/T0.2c/T0.4a）。完成前，agent 不修改 `app/update.go`、`events.go`、`orchestrator/chat.go`、`cmd/xagent/main.go`。

### Wave 1：低冲突模块并行

- Agent A：T1.1-T2.2，仅限 config 模块；不改 main wiring。
- Agent B：T6.1-T6.2，仅限 diagnostics 模块。
- Agent C：T15.1-T15.2，仅限 conversation 模块。
- Agent D：T16.1，等待 Agent A 完成 T1.2 后启动，仅限 instructions 模块。

### Wave 2：主会话与条件并行集成

- 主会话：T3.1-T5.3，完成 M1 验收。
- Tool owner：T10.1-T10.4 在 T3.4 完成后启动，仅限 tool 模块。
- 主会话：T7.1-T9.3，完成 M2 验收。
- 主会话或单一 integrator：T11.1-T14.1，完成 M3 验收。
- 主会话或单一 integrator：T15.3、T16.2、T17.1-T18.2，完成 M4 验收。

### Wave 3：横向安全回归

执行 TS.1a-TS.1f、TS.2-TS.4 和所有里程碑 secret capture 检查。任何泄露原文的路径都阻塞进入开发完成状态。

## 执行顺序摘要

```text
T0.1 → T0.2a → T0.2b → T0.2c → T0.3 → T0.4 → T0.4a → T0.5 → T0.6 → T0.7 → T0.8
TW.1 → TW.2 → TW.3

M1: T1.* → T2.* → T3.* → T4.* → T5.* → M1.V*
M2: T6.* → T7.* → T8.* → T9.* → M2.V*
M3: T10.* → T11.* → T12.* → T13.* → T14.* → TS.1a/TS.1b/TS.1c/TS.1f → M3.V*
M4: T15.* → T16.* → T17.* → T18.* → TS.1d/TS.1e → M4.V*
```
