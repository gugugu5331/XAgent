# XAgent 可用性与可靠性路线图 Plan

## 架构概览

本路线图按四个里程碑落地，避免一次性重写过多模块。

- **M1：启动配置与请求可控性**：修复配置默认值语义、支持 LLM 环境变量、Provider 请求超时、TUI 请求取消、Agent/tool 限制配置化。这一层解决 P0 可用性和“卡死不可恢复”。
- **M2：诊断与错误可见性**：统一 diagnostics 采集、事件传递和 TUI 查询入口；补齐 sessionctx、MCP、context、memory、JSONL 恢复等非致命错误展示。
- **M3：工具与权限体验**：增强工具错误展示、Bash stdout/stderr 可见性、权限确认面板、Bash 非沙箱风险提示、永久授权撤销指引。
- **M4：TUI 使用体验与长期流程**：增加 `/help`、未知 slash command 本地处理、多行输入、自适应布局、项目指令兼容、会话摘要/停止原因、plan/do 说明。

核心原则：先把现有能力“可配置、可取消、可诊断、可理解”，不引入真正 Bash 沙箱、云同步或完整插件管理。

## 核心数据结构

### `redact.RuntimeRedactor`

新增运行期脱敏器，用于注册环境变量展开出的 secret，并对用户可见、模型可见、持久化内容统一脱敏：

```go
type RuntimeRedactor struct {
    mu      sync.RWMutex
    secrets []string
}

func (r *RuntimeRedactor) RegisterSecret(value string)
func (r *RuntimeRedactor) Text(value string) string
func (r *RuntimeRedactor) Any(value any) any
```

生命周期与注入策略采用唯一方案：

```go
type LoadOptions struct {
    Redactor *redact.RuntimeRedactor
}

func Load(path string) (*AppConfig, error)
func LoadWithOptions(path string, options LoadOptions) (*AppConfig, error)
```

- `RuntimeRedactor` 是 app 级实例，由启动入口创建，不使用全局 singleton。
- `config.Load(path)` 仅作为兼容 wrapper，内部创建临时 redactor；应用启动必须调用 `LoadWithOptions(path, LoadOptions{Redactor: appRedactor})`。
- config load 阶段展开出的 secret 直接注册到传入的 app redactor；不得通过日志、diagnostic、返回错误或 handoff 暴露展开值。
- provider、MCP、tool、diagnostics、conversation、memory、TUI 渲染都通过 options/deps 注入同一个 redactor；禁止模块内部新建长期 redactor 或使用全局变量。
- provider factory 增加 options，而不是改变调用语义：`NewWithOptions(cfg, ProviderOptions{Redactor: appRedactor})`；旧 `New(cfg)` 作为兼容 wrapper。
- MCP manager、tool executor、diagnostics collector、conversation store、memory manager 使用各自 options/deps 接收 redactor；测试中每个测试创建独立 redactor，避免 secret 串扰。
- `RegisterSecret` 忽略空值；对敏感配置字段展开值即使短或低熵也必须注册，只有来自非敏感自由文本的候选值才允许按长度/熵过滤。

默认规则继续覆盖 api_key、token、Authorization、password、secret、cookie、PEM 私钥；运行期注册值额外精确替换。所有 diagnostics、HTTP body 摘要、stdout/stderr 摘要、permission status、JSONL recovery metadata 都必须经过该层。

### `safedetail.Visibility`

新增内部概念或轻量枚举，约束错误详情能流向哪里：

```go
type Visibility string

const (
    VisibilityUser      Visibility = "user-visible"
    VisibilityModel     Visibility = "model-visible"
    VisibilityPersisted Visibility = "persisted"
    VisibilityMemory    Visibility = "memory-indexable"
)
```

默认策略：

- user-visible：脱敏、限长摘要。
- model-visible：最小必要脱敏摘要，不含完整 stdout/stderr/body/path。
- persisted：脱敏结构化字段，不含 raw body/stderr/坏 JSONL 原文。
- memory-indexable：默认禁止错误详情、diagnostics 和工具输出。

敏感数据流矩阵：

| 数据来源 | 用户可见 | 模型可见 | JSONL/持久化 | 长期记忆 | Artifact |
|---|---|---|---|---|---|
| 用户普通输入 | 原文显示给本地用户；必要时状态摘要脱敏 | 按对话语义原文发送给 provider | 会话 JSONL 可保存原文，因为这是用户显式对话内容；导出/诊断/证据必须脱敏 | 只有用户明确要求记住的长期事实可进入，写入前脱敏 | 禁止默认生成 |
| Assistant/provider 正常回复 | 原文显示；包含 secret 时渲染层脱敏 | 后续上下文可保留必要回复；不得把 diagnostics/tool raw 混入 | 会话 JSONL 可保存回复原文；恢复诊断和列表摘要必须脱敏 | 默认不把普通回复整段收录；只收录明确长期事实且脱敏 | 禁止默认生成 |
| Stable system / instructions / memory index | 用户通过状态/diagnostics 只看脱敏摘要 | 可按功能进入 provider，但 memory 边界必须标注不可信 | 保存配置/会话元数据时脱敏路径和错误 | memory index 不收录错误详情、工具输出、diagnostics | 禁止默认生成 |
| Bash/stdout 成功输出 | 脱敏限长摘要 | 脱敏最小摘要 | 脱敏结构化摘要 | 禁止默认收录 | 超限时可保存原始完整输出，限本机私有目录，默认不进入模型/证据 |
| Bash/stderr 失败输出 | 脱敏限长摘要 | 脱敏错误摘要 | 脱敏结构化摘要 | 禁止默认收录 | 超限时可保存原始完整输出，限本机私有目录，默认不进入模型/证据 |
| 非 Bash 工具结果 | 脱敏限长摘要 | 脱敏最小摘要 | 脱敏结构化摘要 | 仅允许用户明确表达的长期事实 | 按工具输出策略生成，默认私有 |
| Provider HTTP error body | 脱敏短摘要 | 脱敏错误分类，不含 body 原文 | 脱敏 status/code/preview | 禁止 | 禁止默认生成 |
| MCP HTTP body / stdio stderr | 脱敏短摘要 | 禁止默认进入模型 | 脱敏 status/code/preview | 禁止 | 禁止默认生成 |
| diagnostics detail | 脱敏本地详情 | 禁止 | 默认不保存；如保存只存 code/severity/source/hint | 禁止 | 禁止 |
| JSONL 坏行原文 | 禁止 | 禁止 | 禁止复制原文，只保存行号/错误类型 | 禁止 | 禁止默认生成 |
| Permission rule/status | 脱敏摘要 | 禁止默认进入模型 | 只能持久化脱敏规则摘要；含 secret 的 raw 命令不得永久保存 | 禁止 | 禁止 |
| Artifact path | 默认显示 artifact id；路径需脱敏/缩写 | 默认禁止 | 可保存 artifact id/bytes | 禁止 | 私有目录 0700/文件 0600 |

普通对话原文保存是会话恢复的必要例外，但只限用户主动输入和 provider 正常回复；任何派生摘要、列表、diagnostic、handoff、memory index、测试证据都必须走脱敏层。权限永久授权是高风险持久化路径：如果命令/参数包含 runtime secret 或静态敏感样本，默认禁止 permanent，只允许 once/session；可永久保存的规则必须是结构化、最小化、脱敏后的 scope 摘要。

### `tool.ArtifactRef`

用于 AC11 的完整输出追溯，不把原始输出默认放入模型上下文：

```go
type ArtifactRef struct {
    ID        string
    Path      string
    Bytes     int64
    CreatedAt time.Time
}
```

artifact 保存在用户私有数据目录，目录 0700、文件 0600，不在 repo 工作区。超限工具输出的 artifact 可以保存原始完整内容，这是为了本地用户可追溯性的受控例外；该例外仅限本机私有文件，不得进入 provider messages、conversation JSONL、memory index、diagnostics、handoff 或 tmux 验收证据。`ToolDisplay` 默认显示 artifact id、字节数和“本地私有可追溯”提示，不显示绝对路径；只有本地用户显式执行后续读取动作时才显示脱敏后的内容预览。模型上下文默认只收到“完整输出本地可用”的摘要，不收到路径，也不能通过 artifact id 自动读取完整内容。

### `config.LLMConfig`

新增字段：

```go
type LLMConfig struct {
    Protocol         string         `yaml:"protocol"`
    Model            string         `yaml:"model"`
    BaseURL          string         `yaml:"base_url"`
    APIKey           string         `yaml:"api_key"`
    RequestTimeoutMS int            `yaml:"request_timeout_ms"`
    Thinking         ThinkingConfig `yaml:"thinking"`
}
```

用途：配置 Provider HTTP 请求超时。`APIKey` 允许 `${ENV}` 展开。

### `config.AgentConfig`

新增配置组：

```go
type AgentConfig struct {
    MaxIterations       int `yaml:"max_iterations"`
    MaxUnknownToolCalls int `yaml:"max_unknown_tool_calls"`
}
```

用途：替代 hard-coded Agent Loop 默认值。

### `config.ToolConfig`

新增配置组：

```go
type ToolConfig struct {
    TimeoutMS      int `yaml:"timeout_ms"`
    MaxOutputBytes int `yaml:"max_output_bytes"`
}
```

用途：替代 `cmd/xagent/main.go` 中工具执行 30s / 32KB 硬编码。

### 可选 bool 默认值模型

为需要“区分未配置和配置 false”的字段使用内部默认跟踪。推荐实现：

```go
type OptionalBool struct {
    Set   bool
    Value bool
}
```

或直接将以下字段改为 `*bool`：

```go
ContextConfig.Enabled      *bool
InstructionsConfig.Enabled *bool
MemoryConfig.Enabled       *bool
```

对外行为：YAML 未设置时默认 true；显式 false 时保持 false。为了减少迁移风险，plan 推荐使用指针 bool，并提供 helper `enabledOrDefault(value *bool, defaultValue bool)`。

### `diagnostics.SafeHTTPErrorSummary`

Provider 和 MCP 共享同一 HTTP 错误摘要规则：

```go
type HTTPErrorSummary struct {
    StatusCode  int
    ContentType string
    BodyPreview string
    Truncated   bool
}
```

规则：只读取有限前缀；不记录 request headers、Authorization、Cookie、request body；未知或二进制 content-type 只显示大小和类型；body preview 必须脱敏、限长。

### `diagnostics.Collector`

新增轻量诊断收集器：

```go
type Collector struct {
    mu    sync.Mutex
    items []Diagnostic
    limit int
}

func (c *Collector) Add(items ...Diagnostic)
func (c *Collector) List() []Diagnostic
func (c *Collector) Clear()
func (c *Collector) CountBySeverity() map[Severity]int
```

用途：让 app/orchestrator/sessionctx/MCP/memory/context 产生的非致命诊断进入统一查询入口。

### Diagnostics taxonomy

| 字段 | 规则 |
|---|---|
| Severity | 仅使用 info/warning/error；warning 表示可恢复但需用户注意，error 表示阻塞当前操作 |
| Code | 使用小写 snake_case，前缀为模块名，如 `mcp_missing_env`、`context_summary_failed` |
| Source | 使用稳定模块名或 server 名，不包含 raw secret |
| Hint | 一句话下一步建议；不得包含 raw body、stderr、坏 JSONL 原文或展开后的 env 值 |
| Path | 优先相对路径或 `~` 缩写；进入模型或导出证据前必须脱敏 |

### `events.DiagnosticDisplay`

新增事件 payload：

```go
type DiagnosticDisplay struct {
    Code     string
    Severity string
    Source   string
    Path     string
    Message  string
    Hint     string
}
```

`events.Type` 新增：

```go
DiagnosticEmitted Type = "diagnostic_emitted"
```

用途：streaming 过程中把 diagnostics 推送到 TUI。

### `events.ToolDisplay`

扩展工具展示：

```go
type ToolDisplay struct {
    CallID            string
    Name              string
    Arguments         string
    Summary           string
    Status            ToolDisplayStatus
    ErrorCode         string
    Stdout            string
    Stderr            string
    Truncated         bool
    Recoverable       bool
    ArtifactID        string
    ArtifactBytes     int64
    ArtifactAvailable bool
}
```

用途：TUI 展示工具失败的具体原因。

### `events.ToolConfirmationRequest`

扩展权限确认：

```go
type ToolConfirmationRequest struct {
    CallID         string
    Name           string
    Arguments      string
    Prompt         string
    Risk           string
    PermissionMode string
    ScopePreview   string
    Warning        string
    RevokeHint     string
    AllowPermanent bool
    Decision       chan ToolConfirmationDecision
}
```

用途：权限确认面板展示风险、范围、永久规则撤销提示。

### `app.RequestSession`

在 TUI Model 内新增当前请求控制：

```go
type RequestSession struct {
    Cancel context.CancelFunc
    StartedAt time.Time
    Timeout time.Duration
}
```

用途：请求中按 Esc/Ctrl+C 可取消，而不是只能等待或退出。

## 模块设计

### 配置模块 `internal/config`

**职责：**
- 解析新增 `llm.request_timeout_ms`、`agent.*`、`tool.*`。
- 支持 `llm.api_key`、必要 URL/header 中的环境变量展开。
- 修复 `enabled: false` 被默认值覆盖的问题。
- 校验新增字段并给出可操作错误。

**对外接口：**
- `Load(path string) (*AppConfig, error)` 保持不变。
- 新增 helper：
  - `Enabled(value *bool, defaultValue bool) bool`
  - `ExpandConfigValue(value string, sensitive bool) (string, error)` 或复用现有 expand 逻辑。

**依赖：** 无业务依赖，仅依赖 yaml/os/path。

### Provider 模块 `internal/provider`

**职责：**
- 使用 `LLMConfig.RequestTimeoutMS` 创建带 timeout 的 HTTP client。
- Provider 请求使用调用方传入 ctx，配合 TUI 取消。
- 统一常见 provider 错误分类，至少保留 HTTP status 和脱敏 body 摘要。

**对外接口：**
- `New(cfg config.LLMConfig) (Provider, error)` 保持不变。
- 内部 `http.Client{Timeout: ...}`。

**依赖：** config、redact。

### Orchestrator 模块 `internal/orchestrator`

**职责：**
- 接收 AgentOptions，替代 `defaultRunOptions()` 硬编码。
- 将 `sessionctx.Prepare` 返回 diagnostics 发到 event channel 和 collector。
- 工具事件附带错误码、stdout/stderr、truncated。
- Provider/context/tool 错误提供更可操作的 stop message。

**对外接口：**
- `OrchestratorOptions` 新增：
  - `Agent config.AgentConfig`
  - `Diagnostics *diagnostics.Collector`
- `Send(ctx, conv, text)` 继续接收 ctx；调用方负责超时/取消。
- `CompactContext(ctx, conv)` 保持不变，但错误进入 diagnostics。

**依赖：** provider、tool、conversation、diagnostics、events。

### App/TUI 模块 `internal/app`、`internal/tui`

**职责：**
- 新增 `/help`、`/diagnostics`、`/mcp status`、`/permissions status` 的本地命令入口。
- 未知 slash command 本地报错。
- 请求中 Esc/Ctrl+C 优先取消当前请求；空闲时退出。
- 权限确认从 footer 文本升级为结构化面板。
- 展示工具错误详情，支持后续展开/折叠。
- 状态栏展示 diagnostics 数量、权限模式、请求耗时/timeout。

**对外接口：**
- `Model` 增加：
  - `request *RequestSession`
  - `diagnostics *diagnostics.Collector`
- TUI 增加渲染函数：
  - `RenderHelp(...) string`
  - `RenderDiagnostics(...) string`
  - `RenderConfirmationPanel(...) string`

**依赖：** events、diagnostics、redact、config。

### Tool 模块 `internal/tool`

**职责：**
- Bash 失败时 Content 包含 stdout + stderr 摘要，不只 stderr。
- 截断按 rune 或安全字节边界，避免破坏 UTF-8。
- 工具结果保留 stdout/stderr 前缀、错误码、recoverable、truncated。
- 权限归一化失败不再伪装成 permission denied，应返回 invalid arguments 或 permission config error 的明确错误。

**对外接口：**
- `Executor` 保持构造函数，但由 config 传 timeout/max output。
- `Result` 结构不必强制变更；事件转换层读取已有字段，必要时增加 helper：
  - `DisplayFields(result Result) ToolDisplayFields`

**依赖：** permission。

### MCP 模块 `internal/mcpclient`

**职责：**
- 配置 diagnostics、启动失败、stdio stderr/protocol diagnostics、HTTP 非 2xx body 进入统一 diagnostics。
- `/mcp status` 可查询 server 名称、source、状态、错误、建议。

**对外接口：**
- `Manager.Diagnostics() []diagnostics.Diagnostic` 或扩展现有 status。
- HTTP transport 非 2xx 返回脱敏 body 摘要。

**依赖：** diagnostics、redact。

### Conversation 模块 `internal/conversation`

**职责：**
- `List()` 不应静默跳过损坏但可恢复的 JSONL；应返回可显示的 degraded item 或 recovery metadata。
- Recover 后保存应修复或绕过旧坏行，避免继续保存失败。
- 会话 metadata 记录 stop reason、最近摘要或恢复诊断。

**对外接口：**
- 可选新增：
  - `ConversationListItem` 带 `Recoverable bool`、`Diagnostics []Diagnostic`、`LastStopReason string`。
  - 或保持 `Conversation`，在 `List()` 中对坏文件生成最小 Conversation 占位。

**依赖：** diagnostics、redact。

### Instructions 模块 `internal/instructions`

**职责：**
- 兼容多个默认指令文件候选。
- 如果发现常见候选文件但未加载，产生 diagnostics。
- 保持 include 防环、防越界和缓存失效。

**对外接口：**
- `InstructionsConfig` 新增可选：
  - `ProjectFiles []string` 或 `FallbackFiles []string`
- `Loader.Load` 输出 diagnostics 保持不变。

**依赖：** config、diagnostics。

## 模块交互

### 请求发送与取消

1. 用户在 TUI 输入文本。
2. `app.Model` 创建 `context.WithCancel`，保存到 `RequestSession`。
3. 调用 `orchestrator.Send(ctx, conv, text)`。
4. Provider、context manager、tool executor 全部使用该 ctx。
5. 用户按 Esc/Ctrl+C：
   - 若 streaming，调用 cancel，状态显示“正在取消”。
   - orchestrator 收到 ctx cancelled，保存当前可保存状态，发 Done/Error。
   - TUI 重新启用输入。

### diagnostics 流动

1. config load、MCP validation、MCP start、sessionctx prepare、memory/context/conversation recovery 产生 diagnostics。
2. diagnostics 先转换为脱敏、限长的 SafeDiagnostic，再写入 `diagnostics.Collector`。
3. 请求中新增 diagnostics 同时发 `events.DiagnosticEmitted`。
4. TUI 状态栏显示数量和最高严重级别。
5. 用户输入 `/diagnostics` 查看脱敏详情和建议。
6. diagnostics 不作为 conversation message 保存，不进入模型上下文，不进入长期 memory。

### 工具错误展示

1. Tool 执行返回 `tool.Result`。
2. Tool 层生成两种摘要：模型可见的最小脱敏错误摘要、用户可见的脱敏限长详情。
3. orchestrator 转换为 `events.ToolDisplay` 时填充错误码、stdout/stderr 摘要、truncated、artifact ref。
4. TUI 默认显示 summary + 关键错误一行 + 截断/完整输出追溯提示。
5. 模型上下文默认只收到模型可见摘要，不收到原始 stdout/stderr，也不默认收到完整 artifact 路径。
6. 完整输出 artifact 默认只保存在用户私有数据目录，除非用户显式请求，否则不会被模型读取。

### 权限确认

1. orchestrator 生成 `ToolConfirmationRequest`。
2. 权限模块提供风险/规则/授权范围说明。
3. TUI 渲染结构化确认面板。
4. 用户选择 once/session/permanent/deny/cancel。
5. permanent 显示撤销提示，未来 `/permissions status` 可列出规则。

## 文件组织

```text
cmd/xagent/main.go
  - 注入新增 Agent/Tool/Diagnostics 配置
  - 启动后收集 MCP/config diagnostics

internal/config/
  config.go      - 新增 LLM timeout、AgentConfig、ToolConfig、可选 enabled 字段
  load.go        - 修复默认值语义、LLM env 展开、用户配置合并策略
  validate.go    - 新增字段校验和更可操作错误
  mcp.go         - 保持 MCP merge，补 diagnostics 传递

internal/diagnostics/
  diagnostic.go  - 保持 Diagnostic
  collector.go   - 新增 Collector 和查询/截断/脱敏 helper

internal/provider/
  factory.go     - HTTP timeout
  openai.go      - 错误 body 脱敏摘要、ctx 传递
  anthropic.go   - 同上

internal/orchestrator/
  chat.go        - sessionctx diagnostics event、ctx 取消、tool display 扩展
  agent_loop.go  - 使用配置化 RunOptions
  run_request.go - 未知 slash command 可由 app 层先处理；保留 plan/do 解析
  tool_batches.go - 工具结果 display 字段提取

internal/app/
  app.go         - Model 增加 diagnostics/request 状态
  update.go      - /help、/diagnostics、/mcp status、/permissions status、取消请求、未知 slash command
  deps.go        - 注入 diagnostics collector、config 派生状态

internal/tui/
  status.go      - diagnostics 数量、权限模式、timeout/elapsed
  messages.go    - 工具错误详情显示、自适应宽度入口
  input.go       - 后续 textarea/multiline
  confirmation.go - 新增权限确认面板渲染
  help.go        - 新增帮助文本渲染

internal/tool/
  bash.go        - 失败 content 包含 stdout/stderr
  executor.go    - config timeout/max output、UTF-8 安全截断、参数错误分类
  tool.go        - 如有必要扩展 Result helper

internal/mcpclient/
  manager.go     - diagnostics 暴露和状态详情
  http.go        - 非 2xx body 摘要
  stdio.go       - stderr/protocol diagnostics 并入 manager

internal/conversation/
  jsonl_store.go - 损坏会话列表占位、恢复后保存修复
  recovery.go    - recovery diagnostics 继续完善
  conversation.go - stop reason/summary metadata（可后续里程碑）

internal/instructions/
  loader.go      - 多候选指令文件兼容和 diagnostics

config.example.yaml
  - 新增 llm.request_timeout_ms、agent、tool 示例

docs/usability-reliability-roadmap/
  spec.md
  plan.md
  task.md
  checklist.md
```

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 实现顺序 | P0 到 P1 再 P2，按里程碑拆 | 全量路线图范围大，分阶段可测试、可提交 |
| enabled 默认值 | 用指针 bool 或等价 presence tracking | 必须区分未配置与显式 false |
| 请求取消 | TUI 持有 context cancel，传入 orchestrator | 最小侵入，provider/tool/context 都已接受 ctx |
| diagnostics | 新增本地 Collector + event 推送 | 统一非致命错误入口，不阻塞正常对话 |
| Provider timeout | HTTP client timeout + ctx cancel 双保险 | 解决网络挂死和用户主动取消两个场景 |
| 工具错误 | 结构化数据保留，TUI 展示摘要 | 不破坏现有 tool.Result，同时改善可见性 |
| Bash 沙箱 | 不做 OS 沙箱，只加强风险提示 | 符合 spec 不做事项，避免虚假安全承诺 |
| MCP 错误 body | 保存脱敏短摘要 | 提升可操作性，同时控制敏感信息风险 |
| JSONL 损坏 | 列表可见 + 恢复后 repair | 用户能找回会话，避免坏行反复阻断保存 |
| 多行输入 | 放在后续里程碑 | 体验价值高但涉及 TUI 交互较多，避免阻塞 P0 |

## Spec 覆盖映射

- F1/F2/F3 → 配置模块、config.example、启动错误。
- F4 → provider、app request session、orchestrator ctx。
- F5/F6/F19 → app 本地命令、tui help/status。
- F7/F8/F20 → diagnostics collector、MCP/sessionctx/context/memory 集成。
- F9/F13 → tool executor、Bash、TUI messages。
- F10/F11 → events confirmation、TUI confirmation panel、permission hints。
- F12 → AgentConfig、ToolConfig、orchestrator/tool executor。
- F14/F17 → conversation JSONL list/recovery metadata。
- F15 → instructions loader。
- F16 → TUI input/messages。
- F18 → help 文档和 plan/do 误用提示。
- F21 → 敏感数据流分级、`RuntimeRedactor`、TS.1。
- F22 → SafeHTTPErrorSummary、Tool 输出摘要、diagnostics/JSONL/memory 禁止 raw 规则、TS.1。
- F23 → `ArtifactRef`、工具 artifact 私有存储、ToolDisplay artifact 字段、M3 artifact 验收。

## 多角度审查修订

> 并行审查 agent 因上游 API/网关错误未能产出有效结论，本节由主会话按同样五个角度补审，作为 task.md 拆分约束。

### 架构拆分审查

- **P0：M1 必须再拆成两个可独立提交的切片。** 配置语义修复和 provider 取消/超时都很关键，但同时改 config、provider、app、orchestrator 容易扩大回归面。task.md 应先做配置兼容切片，再做请求取消切片。
- **P0：diagnostics Collector 不应成为全局单例。** 应由 `app.Deps` 注入，避免测试和并发会话共享状态；collector 只保存最近有限条目。
- **P1：events 扩展要保持向后兼容。** `ToolDisplay` 增字段可以零值兼容；新增事件类型必须让旧 switch 默认忽略，不应破坏现有流。
- **P1：JSONL 会话列表修复应独立于 TUI 摘要增强。** 先保证损坏会话可见和可保存，再做摘要/停止原因。

### 测试验收审查

- **P0：每个配置默认值变化都要有回归测试。** 特别是 `enabled` 显式 false、未配置默认 true、旧 config 仍可加载。
- **P0：请求取消必须用 fake provider 做确定性测试。** 不依赖真实网络；fake provider 阻塞直到 ctx cancelled，断言 TUI/Orchestrator 能恢复输入。
- **P1：diagnostics 要同时测 collector 和 TUI 命令。** 单元测试验证 collector 脱敏/限长，app 测试验证 `/diagnostics` 输出。
- **P1：MCP 错误 body/stderr 只测脱敏摘要。** 不把真实 token 写入 fixture。
- **P1：tmux E2E 分两类。** P0 每次做真实 TUI smoke；provider 网络超时用 fake/local server，不依赖真实 API 长等待。

### 安全隐私审查

- **P0：新增错误详情必须先过脱敏层。** MCP body、stdout/stderr、diagnostics、permission prompt、session JSONL 都不能直接输出原始 secret。
- **P0：环境变量展开错误不能泄露变量值。** 缺失时只显示变量名；存在时不得在 diagnostics 中显示展开后的值。
- **P1：Bash 非沙箱提示不能暗示已有保护。** 文案要明确“未隔离文件系统”，不要写成“受限于项目根”。
- **P1：永久授权展示只显示规则摘要，不显示敏感参数。** 如果命令里含 token，应展示脱敏命令。
- **P2：工具 stdout/stderr 展示需有长度上限。** 防止 TUI 卡顿和敏感内容扩大传播。

### TUI 体验审查

- **P0：取消请求的按键优先级必须明确。** streaming 中 `Esc` 取消请求；`ctrl+c` 第一次取消、空闲时退出，避免误杀进程。
- **P1：`/help` 应先以 notice/message 文本实现。** 不要第一阶段就做复杂页面路由，降低 TUI 改造风险。
- **P1：权限确认面板应先替代 footer 但不改变快捷键语义。** Enter 仍拒绝，但文案明确 `[Enter/n] 拒绝`。
- **P2：多行输入最后做。** textarea 会影响 Enter 提交、粘贴、历史列表焦点，适合 M4 独立切片。

### 实施风险审查

- **P0：不要在同一任务里同时改配置 schema 和所有调用方行为。** task.md 应按 config → wiring → behavior → tests 排序。
- **P0：每个里程碑结束都必须 `go test ./...`。** 涉及 TUI 行为的里程碑还要 tmux 验收。
- **P1：先利用现有命令体系。** `/diagnostics`、`/help`、`/mcp status` 可复用 `app/update.go` 本地命令模式，不先引入 slash command 框架。
- **P1：多 agent 并行适合按模块切。** config/provider、diagnostics/MCP、tool/permission、TUI/help、conversation/recovery 可并行；同一文件如 `app/update.go` 不适合多人同时改。

## 推荐 task.md 拆分方向

1. **M1a 配置语义与新增配置**：修 `enabled` false、LLM env、AgentConfig、ToolConfig、config.example、config tests。
2. **M1b Provider 超时与请求取消**：HTTP timeout、app request session、ctx cancel、fake provider tests、tmux smoke。
3. **M2a diagnostics 基础设施**：Collector、events diagnostic、app deps、`/diagnostics`。
4. **M2b MCP/sessionctx/context diagnostics 接入**：MCP validation/start/http body/stderr、sessionctx prepare diagnostics、context failure diagnostics。
5. **M3a 工具错误详情**：Bash stdout/stderr content、UTF-8 safe truncation、ToolDisplay fields、TUI display tests。
6. **M3b 权限确认体验**：confirmation request fields、Bash 非沙箱 warning、永久授权撤销提示、TUI panel tests。
7. **M4a help 与 slash command**：`/help`、未知 slash command、plan/do 说明、memory/compact/mcp/diagnostics/permissions 帮助。
8. **M4b 会话恢复可见性**：损坏 JSONL 列表占位、恢复后保存 repair、recovery diagnostics。
9. **M4c 指令兼容与 TUI 长输入**：CLAUDE.md/AGENTS.md 兼容提示、多行输入、自适应布局。
