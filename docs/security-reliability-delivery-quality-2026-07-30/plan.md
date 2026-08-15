# XAgent 安全、可靠性与交付质量提升 Plan（2026-07-30）

> 状态：原设计及 C1–C18c 已批准（2026-07-30 至 2026-08-03）
>
> 前置文档：`spec.md`（已批准）
>
> 本文只定义技术设计，不授权实施。`task.md` 与 `checklist.md` 均批准后才能修改实现代码。

## 架构概览

### 分层结构

```text
cmd/xagent（唯一组装根与进程生命周期所有者）
    │
    ├── app / command / tui
    │       用户意图、导航、状态边界与纯展示
    │
    ├── orchestrator / events
    │       Agent Loop、Provider 流消费、权限与工具调度、有序提交
    │
    ├── provider / mcpclient / conversation / tool / permission / hook
    │       领域能力、协议、持久化与执行
    │
    └── redact / budget / diagnostics / safefs / proctree / artifact / netpolicy
            无业务反向依赖的安全基础能力
```

依赖只向下。`cmd/xagent` 负责把最终配置转换成各模块的 Options，并按最小能力注入；叶子模块不读取全局配置。TUI 不直接访问 Config、Store、Provider、MCP 或 Orchestrator，只消费 ViewModel 并产生用户 intent。

### 组装与生命周期

- `cmd/xagent` 是唯一 composition root：创建进程级 redactor、诊断、受保护文件根、artifact store、网络策略、进程树 runner、权限系统、Provider、MCP、Tool Registry、Conversation Store、Orchestrator、App 和 TUI。
- 创建者拥有关闭责任。关闭按所有权反序执行，所有 Close 幂等；调用者 context 只限制等待，已经开始的清理不被放弃。
- Provider 的单次 `ChatStream` 由 Orchestrator 关闭；MCP 使用 `Manager → ServerSession → Connection → Transport` 所有权链；物理资源由 Transport 释放。
- 任一部分初始化失败时，只回滚已经成功创建的对象，不暴露半初始化 Provider 流、MCP 工具或 Conversation 状态。

### 安全边界

- Bash 授权身份由实际 Shell、工作目录稳定身份、环境摘要和原始命令字节生成；展示或脱敏文本不参与匹配。
- Rule/session/permanent 只影响是否再次提示；每次实际执行仍需要一次性、不可伪造且不可重放的 `ExecutionTicket`。
- Write/Edit 通过 `safefs` 强制保护权限文件；Shell 与解释器通过平台受保护执行层约束。无法证明受保护路径不可写时，Bash fail closed。
- 文件、命令、Instruction、Provider 和 MCP 从读取第一字节起执行累计预算；内存只保留有界预览。
- 完整原始工具输出只允许进入工作区外的私有 artifact。模型、会话、memory、诊断和日志只获得脱敏安全视图及 opaque artifact Ref。

### 运行时与数据边界

- Provider 返回显式可关闭的流对象；扫描错误、截断 EOF、取消和消费者提前退出都进入同一关闭路径。
- MCP 的 Manager、Session、Connection 和 Transport 都有显式状态机、唯一资源所有者和重复关闭语义。
- Conversation v2 使用规范化状态摘要判断 dirty：纯尾部消息追加写 batch，既有消息或其他持久化状态变化写 snapshot。
- 工具 Risk、ReadOnly、ConcurrentSafe 相互独立；只有 `ReadOnly && ConcurrentSafe` 的调用可并发，结果仍按原始调用顺序回灌。
- 配置使用 presence-aware 分层解析：`默认 < 用户 < 项目 < 运行期/CLI`，显式 `false` 和 `0` 不会被默认值覆盖。

### 交付里程碑

| 里程碑 | 内容 | 完成门槛 |
|---|---|---|
| M0 仓库止血 | 私有产物退出 Git index、补忽略规则、建立 repoaudit | 本地数据保留；secret、gitlink、私有目录、超限二进制门禁可复现 |
| M1 安全基座 | redact、budget、diagnostics、safefs、proctree、artifact、netpolicy | 三平台基础安全测试与 canary 零泄漏通过 |
| M2 运行时安全 | Permission Ticket、Tool 流式采集、Provider 流、MCP 生命周期、Hook 适配 | 授权碰撞、受保护路径、预算、关闭和进程树场景通过 |
| M3 数据与编排 | Conversation v2、配置合并、并发调度、取消状态 | 崩溃恢复、状态持久化、有序并发及旧格式迁移通过 |
| M4 UI 与 CLI | 导航事务、三层状态、响应式布局、帮助与版本 | 80×24、动态缩放、会话切换、help/version 场景通过 |
| M5 跨平台与交付 | macOS/Linux/Windows、CI、E2E、README 与规格治理 | AC1–AC38 全部取得当前可复现证据 |

里程碑按顺序推进。M1 的安全能力可以并行实现，但权限票据与 Executor、MCP ownership、Conversation v2 等边界必须原子切换，不能长期保留新旧双入口。

## 核心数据结构

### 安全文本与预算

```go
package redact

type SafeText struct {
	value string
}

type RuntimeRedactor interface {
	RegisterSecret(secret []byte) error
	Redact(raw string) SafeText
}
```

`SafeText` 字段不导出，只能由运行期 redactor 创建。所有进入模型、TUI、会话、诊断、日志及 memory 的文本必须先转换；原始内容不能通过 struct literal 绕过。

```go
package budget

type Dimension uint8

const (
	Bytes Dimension = iota
	Files
	Directories
	Lines
	Items
	ExpandedBytes
	ProtocolErrors
)

type Counter struct {
	// 限额和已使用量不导出；并发安全。
}

type LimitError struct {
	Scope     string
	Dimension Dimension
	Limit     int64
	Observed  int64
}

func NewCounter(effective, hard Limits) (*Counter, error)
func (c *Counter) Consume(d Dimension, amount int64) error
func (c *Counter) Remaining(d Dimension) int64
func (c *Counter) Snapshot() Snapshot
```

预算在读取、分配或写入前消费；负数、整数溢出和超过硬上限均直接报错。`LimitError` 不携带载荷。

### Artifact 与安全文件访问

```go
package artifact

type Ref struct {
	ID        string
	Bytes     int64
	CreatedAt time.Time
	Available bool
	Complete  bool
	// 不包含真实路径。
}

type Writer interface {
	io.Writer
	Commit(ctx context.Context) (Ref, error)
	Abort() error
}

type Store interface {
	Begin(ctx context.Context, metadata Metadata) (Writer, error)
	OpenForUser(ctx context.Context, id string) (io.ReadCloser, Ref, error)
	Cleanup(ctx context.Context) (CleanupResult, error)
	Close() error
}
```

`OpenForUser` 只注入本地用户动作，不注册成模型工具。每个 Writer 必须恰好 Commit 或 Abort 一次。

```go
package safefs

type Identity struct {
	volume [16]byte
	object [16]byte
}

type Policy struct {
	// 允许根、受保护槽位与平台安全规则。
}

type Capability struct {
	seal *capabilitySeal
}

type Root struct {
	// 已打开目录句柄、稳定身份和 Policy。
}

func OpenRoot(path string, policy Policy) (*Root, error)
func (r *Root) OpenRead(ctx context.Context, relative string) (*File, error)
func (r *Root) Walk(
	ctx context.Context,
	relative string,
	counter *budget.Counter,
	visit func(Entry) error,
) error
func (r *Root) AtomicWrite(
	ctx context.Context,
	capability Capability,
	relative string,
	perm fs.FileMode,
	write func(io.Writer) error,
) error
func (r *Root) Close() error
```

路径验证与访问使用同一打开句柄链。普通工具拿不到修改权限配置所需的专用 Capability。

### 调用身份与执行票据

```go
package permission

type CallIdentity struct {
	version uint16
	digest  [32]byte
}

type BashIdentityInput struct {
	Shell             string
	WorkingDirectory  safefs.Identity
	RawCommand        []byte
	EnvironmentDigest [32]byte
}

func NewBashIdentity(input BashIdentityInput) (CallIdentity, error)

type ExecutionTicket struct {
	// nonce、callID、identity、有效期和 MAC 均不导出。
}

type TicketVerifier interface {
	VerifyAndConsume(
		ticket ExecutionTicket,
		callID string,
		identity CallIdentity,
	) error
}
```

摘要输入采用版本化、长度前缀编码。票据零值无效、不可序列化且只能消费一次；安全状态变化可以使尚未消费的危险票据失效。

### 进程树与工具结果

```go
package proctree

type Request struct {
	Executable string
	Args       []string
	WorkingDir *safefs.Root
	Env        []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

type Result struct {
	ExitCode  int
	Cancelled bool
	TimedOut  bool
}

type Process interface {
	Wait(ctx context.Context) (Result, error)
	Terminate(ctx context.Context) error
	Close() error
}

type Runner interface {
	Start(ctx context.Context, request Request) (Process, error)
}
```

`Start` 返回前必须完成进程树 containment 和受保护执行验证，不暴露裸 `exec.Cmd`。

```go
package tool

type ExecutionPolicy struct {
	ReadOnly       bool
	ConcurrentSafe bool
}

type ExecutionState string

const (
	Prepared             ExecutionState = "prepared"
	Rejected             ExecutionState = "rejected"
	CancelledBeforeStart ExecutionState = "cancelled_before_start"
	Running              ExecutionState = "running"
	Completed            ExecutionState = "completed"
	CancelledAfterStart  ExecutionState = "cancelled_after_start"
)

type Result struct {
	// 状态、三个安全视图、OutputMeta、SafeError 和时间均不导出。
}

func (r Result) ModelContent() redact.SafeText
func (r Result) UserView() UserView
func (r Result) PersistedContent() redact.SafeText
func (r Result) OutputMeta() OutputMeta
```

`CancelledBeforeStart` 的 `Result=nil`；Rejected、Completed 和 CancelledAfterStart 可产生反映真实状态的安全结果。Result 不保存 raw stdout/stderr。

### 网络与 Provider

```go
package netpolicy

type Endpoint struct {
	URL          *url.URL
	Origin       string
	AddressClass AddressClass
	Purpose      Purpose
}

type HTTPPolicy interface {
	ValidateInitial(ctx context.Context, rawURL string, purpose Purpose) (Endpoint, error)
	ValidateRequest(ctx context.Context, endpoint Endpoint, target *url.URL) error
	ValidateRedirect(ctx context.Context, endpoint Endpoint, next *url.URL) error
}

type ClientFactory interface {
	New(endpoint Endpoint, options ClientOptions) (Client, error)
}
```

```go
package provider

type ChatStream interface {
	Events() <-chan StreamEvent
	Close(ctx context.Context) error
}

type Provider interface {
	StreamChat(ctx context.Context, request ChatRequest) (ChatStream, error)
	Name() string
}
```

每个流分别限制原始响应、单事件、事件数、正文、thinking 和工具参数累计大小。生产者是唯一可以关闭事件通道的对象。

### MCP 生命周期

```go
type ManagerState uint8
type SessionState uint8
type ConnectionState uint8
type TransportState uint8

type Manager interface {
	Start(ctx context.Context) error
	CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error)
	Tools() []tool.Tool
	Snapshot() ManagerSnapshot
	Close(ctx context.Context) error
}

type TransportEvent struct {
	Frame json.RawMessage
}

type Transport interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, frame json.RawMessage) error
	Receive(ctx context.Context) (TransportEvent, error)
	Close(ctx context.Context) error
}
```

Manager 在同一锁区检查运行状态并取得调用 lease；进入 Closing 后拒绝新 lease。Connection 是 Transport 的唯一所有者，只有一个 receive loop；pending response channel 容量为 1 且不关闭。

### Conversation v2

```go
package conversation

type StateDigest string

type PersistedState struct {
	Revision       uint64
	MessageCount   int
	Digest         StateDigest
	MessagesDigest StateDigest
}

type RecordKind string

const (
	RecordBatch    RecordKind = "batch"
	RecordSnapshot RecordKind = "snapshot"
)

type JSONLRecord struct {
	Version        int
	Kind           RecordKind
	SessionID      string
	Revision       uint64
	PreviousDigest StateDigest
	Digest         StateDigest
	Batch          *MessageBatch
	Snapshot       *Conversation
}

type Store interface {
	Create(ctx context.Context) (*Conversation, error)
	List(ctx context.Context) (ListResult, error)
	Load(ctx context.Context, id string) (LoadResult, error)
	Save(ctx context.Context, conversation *Conversation) (SaveResult, error)
	Maintain(ctx context.Context) (MaintenanceResult, error)
}
```

状态摘要覆盖消息、工具状态、外置状态、摘要、上下文和元数据。纯追加写 batch；其他变化写 snapshot；完全未变不写入。

### Presence-aware 配置

```go
package config

type Optional[T any] struct {
	Set   bool
	Value T
}

type ConfigSource string

const (
	SourceDefault ConfigSource = "default"
	SourceUser    ConfigSource = "user"
	SourceProject ConfigSource = "project"
	SourceRuntime ConfigSource = "runtime"
)

type ConfigLayer struct {
	Source ConfigSource
	Path   string
	Value  PartialAppConfig
}

type LoadedConfig struct {
	Config     AppConfig
	Provenance map[string]ConfigSource
}

func DecodePartial(path string) (PartialAppConfig, error)
func MergeLayers(layers ...ConfigLayer) (MergeResult, error)
func ResolveConfig(result MergeResult, options LoadOptions) (LoadedConfig, error)
```

只有 `Set=false` 才继承。显式 false、0 和负数保留到最终验证；显式 null 和未知字段失败。MCP 不同名合并，同名由高优先级层整项替换。

### App、导航与布局

```go
type RuntimeState struct {
	RequestSequence uint64
	// Provider、MCP、配置和长期诊断；RequestSequence 跨 reset 单调递增。
}

type ConversationState struct {
	ActiveID string
	Mode     string
	Skills   []string
}

type RequestState struct {
	Generation   uint64
	Duration     time.Duration
	Tokens       Usage
	Cache        CacheUsage
	StopReason   string
	LastError    *diagnostics.SafeError
	Confirmation *ConfirmationState
}

type NavigationRequest struct {
	Generation uint64
	Kind       NavigationKind
	SessionID  string
}

type LayoutInput struct {
	Terminal         Size
	InputLines       int
	ShowConfirmation bool
	ShowCommandMenu  bool
	Screen           string
}

func ComputeLayout(input LayoutInput) Layout
```

请求、会话和进程状态分别保存并通过统一 reset 边界清理。TUI 只负责布局、渲染和 intent。

### 仓库审计

```go
package repoaudit

type Finding struct {
	RuleID   string
	Path     string
	Line     int
	Severity string
	Message  string
}

type Report struct {
	Checked  int
	Findings []Finding
}

type Auditor struct {
	Policy Policy
}

func (a Auditor) Audit(ctx context.Context, source Source) (Report, error)
func (r Report) Passed() bool
```

Finding 不保存匹配原文。Gitlink 必须同时存在显式声明和一致的 `.gitmodules` 映射；测试 secret 仅允许按“路径＋规则 ID＋内容摘要”精确豁免。

## 模块设计

### 安全基础模块

| 模块 | 职责与接口 | 依赖与边界 | 覆盖 |
|---|---|---|---|
| `internal/redact` | 维护进程级秘密清单，通过 `RegisterSecret`、`Redact` 生成 `SafeText`；支持结构化值脱敏 | 仅依赖标准库；不得返回已注册秘密或保存 artifact；由组装根创建唯一实例 | F7 |
| `internal/budget` | 提供 `Counter`、`Snapshot`、`LimitError`；在读取、分配和写入前执行多维预算与硬上限 | 不读取 Config、不决定截断文案、不保存载荷 | F9、F12–F14 |
| `internal/diagnostics` | 统一脱敏、终端字符清理、UTF-8 截断、重复聚合和容量控制 | 仅依赖 redact；不保存原始请求、响应或持久日志 | F2、F5、F7、F12–F21 |
| `internal/safefs` | 使用根句柄和稳定身份完成读取、遍历、原子写入及受保护路径检查 | 依赖 budget 和平台系统调用；不执行 Shell、不处理用户授权 | F4、F12、F13、F31 |
| `internal/proctree` | 建立进程树 containment，负责终止、强杀和 reap | 依赖 safefs；不解析命令、不决定权限、不缓存输出 | F4、F11、F16、F31 |
| `internal/artifact` | 管理私有 staging、原子提交、用户读取和清理 | 依赖 budget、safefs；模型、memory、诊断和普通工具不能打开原文 | F9、F10 |
| `internal/netpolicy` | 验证初始 URL、DNS、请求、重定向和实际拨号；创建安全 HTTP Client | 不解析业务协议、不管理凭据值 | F7、F8 |

现有 `tool/path.go`、`permission/sandbox.go` 和 `instructions/sandbox.go` 的重复路径能力下沉到 safefs；Hook、MCP 和 Provider 的网络安全能力下沉到 netpolicy。

### 配置、权限与执行

| 模块 | 职责与接口 | 依赖与边界 | 覆盖 |
|---|---|---|---|
| `internal/config` | 严格解析 PartialConfig，按固定优先级合并，应用默认值、展开环境变量、注册秘密并返回 provenance | 依赖 YAML 和 redact；不启动服务，业务模块不能自行补默认值 | F7、F22、F28 |
| `internal/permission` | 加载规则、执行硬约束、生成确认信息和最小永久规则；构造身份并签发 Ticket | 依赖 safefs、redact、matcher；不执行工具、不渲染 UI、不再靠字符串扫描保护文件 | F3–F6 |
| `internal/tool` | Tool、Registry、schema、参数校验、唯一 Executor、内置工具和三种结果视图 | 依赖 permission、budget、artifact、safefs、proctree、redact；不决定授权、调度或持久化 | F4、F9–F12、F23、F24 |
| `internal/instructions` | 发现固定指令源、维护优先级与缓存，安全读取 include 图并共享累计预算 | 依赖 safefs、budget、redact、diagnostics；不执行指令、不写文件、不联网 | F7、F13 |
| `internal/hook` | 保留规则、事件、Prompt lease 和生命周期；命令接入 proctree，HTTP 接入 netpolicy | Hook 只能增加限制，不能批准工具或接收 raw 工具输出 | F4、F7–F9、F11 |

迁移约束：

- 可构造、可复用的 `permission.Grant` 被一次性 `ExecutionTicket` 取代。
- `tool.Executor.truncate` 的事后截断改为流式预算、有界预览和 artifact 分流。
- Read/Grep/Glob 统一使用 safefs 与 budget；Write/Edit 只能获得普通写 capability；Bash 只能通过 proctree。
- Hook、Bash 和 MCP stdio 共用进程树能力，每棵进程树只有一个 Wait 所有者。

### Provider 与 MCP

#### `internal/provider`

职责：

- 保留统一请求、流事件和 usage 契约，由 OpenAI、Anthropic adapter 完成协议映射。
- 从响应第一字节开始限制响应、事件、正文、thinking 和工具参数累计大小。
- 返回显式可关闭的 ChatStream；所有退出路径可靠释放底层流。
- OpenAI 在 `finish_reason` 后继续读取 usage，只有协议终止标记到达才正常结束。

依赖 netpolicy、budget、redact 及现有请求 DTO；不组装 Prompt、不执行工具、不保存会话、不渲染状态。Provider 构造时接收组装根提供的客户端，不能自行创建绕过策略的 Client。覆盖 F7、F8、F17、F18。

#### `internal/mcpclient`

职责：

- 按 Manager、ServerSession、Connection、Transport 管理初始化、发现、调用和关闭。
- Manager 使用调用 lease；Connection 独占 Transport 和唯一 receive loop。
- HTTP Transport 跟踪全部活动 body；stdio Transport 持续排空 stderr 并通过 proctree 管理进程。
- JSON、SSE、frame、分页、工具数量、协议错误和诊断均使用累计预算。
- 远程工具默认危险、非只读、不可并发，除非本地可信配置明确收紧描述。

依赖 budget、netpolicy、proctree、redact、diagnostics 和 tool 契约；不批准权限、不显示确认界面、不决定全局批次排序，也不回滚服务端副作用。覆盖 F5、F7、F8、F14–F16。

### 数据与上下文

| 模块 | 职责与接口 | 依赖与边界 | 覆盖 |
|---|---|---|---|
| `internal/conversation` | v2 batch/snapshot、摘要链、崩溃安全保存、有界列表、恢复、迁移和清理 | 不读取 artifact 正文、不负责上下文压缩或 UI | F10、F19–F22、F24 |
| `internal/contextmgr` | 决定上下文压缩和工具结果外置时机，只接收安全视图、usage 和 artifact 元数据 | 不读取完整 artifact，不直接写会话文件 | F7、F10、F18、F20 |
| `internal/sessionctx`、`prompt`、`resources` | 组装稳定与动态 Prompt 区块 | 只允许安全文本进入模型；不加载全局配置、不执行工具 | F7、F13 |
| `internal/memory` | 保存和更新经过脱敏、预算控制的记忆 | 不接收 raw 工具输出、真实 artifact 路径或凭据 | F7、F10 |
| `internal/skill` | 保留发现、激活和 Prompt 贡献，状态按边界清理 | 不新增 Skill 类型，不绕过工具权限与预算 | F7、F26、O1 |

旧 JSON 和 v1 JSONL 通过同一入口非破坏读取；只有后续成功保存才升级为 v2 snapshot。

### 编排、应用与界面

| 模块 | 职责与接口 | 依赖与边界 | 覆盖 |
|---|---|---|---|
| `internal/orchestrator` | 准备上下文、消费并关闭 Provider 流、累计 usage、权限决策、工具调度、有序回灌和保存 | 不读取 Config、不渲染 TUI、不负责进程级关闭 | F17、F18、F20、F23、F24 |
| `internal/events` | 传递有限、脱敏、不可变的进度、usage、确认和结果事件 | 不携带 raw 输出、真实路径或业务服务引用 | F7、F10、F18、F24 |
| `internal/app` | UI 与业务服务的唯一控制器；维护三层状态并执行导航事务 | 仅通过窄接口访问 Store、Orchestrator 和 artifact 用户读取器 | F21、F25、F26、F29 |
| `internal/command` | 维护正式命令、快捷键、隐藏兼容入口、补全和帮助的单一元数据 | 命令只产生 App intent，不直接操作领域服务 | F25、F29 |
| `internal/tui` | 纯布局、输入和安全 ViewModel 渲染 | 不导入 Config、Store、Orchestrator，不读取 artifact | F6、F10、F21、F25–F29 |
| `cmd/xagent` | 唯一组装根和生命周期所有者；help/version 前置，退出时反向关闭 | 可依赖全部模块，但不承载业务算法 | F15、F17、F30、F31 |

### 仓库与交付

| 模块 | 职责与接口 | 边界 | 覆盖 |
|---|---|---|---|
| `internal/repoaudit` | 审计私有目录、疑似凭据、超限二进制、gitlink 和规格追溯关系 | 只读；不自动 unstage、删除、改写历史或撤销凭据 | F1、F2、F33 |
| `cmd/xagent-repo-check` | 为本地 hook、CI 和发布前检查提供统一入口 | 不复制规则、不输出 secret 原文 | F2、F32、F33 |
| `internal/testutil` | 确定性 fake Provider、MCP、进程、文件故障和时钟 | 默认不访问公网、不读取真实凭据、不依赖短 sleep | F31、F32 |
| CI 门禁 | fmt、静态检查、测试、race、覆盖率、三平台构建、repo audit、重复稳定性和 E2E | 不自动发布、不上传私有 artifact | F2、F31–F33 |
| 文档治理 | README、帮助、配置 schema 与 F→AC→task→checklist→证据保持一致 | 不删除历史规格，不把计划当完成事实 | F29、F33 |

最终依赖方向：

```text
cmd/xagent
    ↓
app → orchestrator → provider / tool / permission / conversation / hook
 ↓          ↓
tui       events
             ↓
redact / budget / diagnostics / safefs / proctree / artifact / netpolicy
```

底层模块不反向依赖 Config、App 或 TUI；所有实例和最小能力由 `cmd/xagent` 注入。

## 模块交互

### 启动、配置与秘密注册

```text
解析 CLI
  ├─ help/version → 输出后成功退出，不读取配置
  └─ 正常启动
       → 创建 RuntimeRedactor
       → 严格解析并合并配置层
       → 展开最终生效的环境变量
       → 立即注册全部秘密
       → 验证最终配置与硬上限
       → 用最终 diagnostics limits 创建唯一有界 Diagnostics
       → 构造安全基础模块
       → 构造 Store、Provider、Hook、MCP、Tool
       → 构造 Orchestrator
       → 构造 App 与 TUI
```

普通应用配置错误在联网或启动子进程前终止；错误不回显标量或凭据。权限规则损坏则进入可观测 degraded 状态，危险工具全部拒绝，低风险只读能力重新按保守策略判断。任一构造失败都按反向顺序关闭已创建资源。

### Provider 流

```text
App 用户请求
  → Orchestrator 组装安全上下文
  → Provider.StreamChat
  → 取得 ChatStream 后立即登记 Close
  → 逐字节消费响应预算
  → framing / 协议解析
  → 文本、thinking、tool call、usage 事件
  → Orchestrator 消费终态
  → ChatStream.Close
```

```text
Orchestrator
└── ChatStream
    ├── 派生 context
    ├── HTTP body 或 SDK stream
    ├── 唯一生产协程
    ├── 有界事件通道
    └── closeDone 与首次清理结果
```

- 正常、网络错误、解析错误、扫描错误、预算超限、取消和提前退出均进入幂等 Close。
- 生产者是唯一能关闭事件通道的对象；事件发送必须监听关闭信号。
- 建流失败时，Provider 在返回错误前关闭已获得的 body。
- `Close(ctx)` 的 context 只限制等待，不撤销清理。
- OpenAI 状态固定为 `Reading → FinishReasonSeen → UsageSeen → [DONE] → Completed`；缺少终止标记的 EOF 是截断错误。

### 工具授权与启动

```text
Provider ToolCall
  → Registry 校验名称、schema 和参数
  → 分配稳定 callID 与原始顺序号
  → Permission 检查硬约束与健康状态
  → Hook BeforeTool 只能拒绝
  → Permission 规则决策或用户确认
  → 签发一次性 ExecutionTicket
  → Executor 重算实际调用身份并原子消费票据
  → 最终取消检查
  → 建立 safefs 句柄或 proctree containment
  → 跨过真正启动边界
  → 执行并采集输出
  → ResultFactory 生成三个安全视图
  → Hook AfterTool 接收安全结果
  → Scheduler 按原始顺序回灌与持久化
```

Hook 不能签发票据、放宽权限或修改已绑定身份的参数。若未来需要修改参数，原票据作废，调用从 schema 校验重新开始。

真正启动边界：

| 工具类型 | 启动边界 |
|---|---|
| Read/Grep/Glob | 通过固定句柄开始第一次读取或遍历 |
| Write/Edit | 受保护目录项复验完成，开始 staging 写入 |
| Bash、stdio Hook/MCP | 子进程已经进入完整 containment 和受保护执行层 |
| 远程 MCP | 获得调用 lease 后首次发送请求字节 |

取消与失败：

| 发生位置 | 状态 | 结果行为 |
|---|---|---|
| 未知工具、schema 非法、硬约束、用户或 Hook 拒绝 | Rejected | 生成真实、结构化的安全拒绝结果 |
| 权限配置损坏 | 危险工具 Rejected | 生成安全诊断；低风险只读能力按保守策略判断 |
| 票据过期、重放、身份不符或安全状态变化 | Rejected | 启动前拒绝，不产生外部效果 |
| Hook 期间或启动门前取消 | CancelledBeforeStart | `Result=nil`，不伪造 tool-role 结果 |
| 真正启动后取消或超时 | CancelledAfterStart | 停止操作或回收进程树，并记录真实状态 |
| 预算超限 | Completed 或取消终态 | 停止生产者，返回 LimitError 和 artifact 元数据 |
| 事件发送或保存失败 | 保留真实执行事实 | 不重复执行，记录可恢复诊断 |

准备阶段顺序执行；只有 ReadOnly 且 ConcurrentSafe 的批次通过 semaphore 并发。Worker 只写自己的序号槽位；实时事件可乱序，Conversation 与 Provider 输入按原始调用顺序。

### 输出与 Artifact

```text
工具输出第一字节
  ├─→ 私有 staging artifact
  └─→ 有界 UTF-8 预览
          → RuntimeRedactor
          → UserView
          → ModelContent
          → PersistedContent
```

- 内联阈值以内，完成后 Abort staging；超过阈值则 Commit artifact，并在操作硬预算内继续采集。
- 达到 artifact 或操作硬上限时停止生产者，保存所有已接受字节并标记不完整。
- UserView 显示脱敏预览、ID、字节数和截断原因；ModelContent 不能读取原文；PersistedContent 只保存安全摘要、状态和 Ref。
- Hook、Conversation、Memory、Diagnostics 和日志不能接收 raw 缓冲或真实路径。

### HTTP 请求与重定向

```text
ValidateInitial
  → 解析 DNS 与地址类别
  → 创建受策略约束的请求
  → 仅向已验证 origin 注入凭据
  → RoundTrip
  → 每次 redirect 移除敏感头
  → 重新验证协议、origin、DNS 和地址类别
  → dial 阶段复核实际目标
```

非回环明文 HTTP、HTTPS 降级、跨源、外网转私网/loopback、URL userinfo 及 DNS 安全类别变化均在发送下一跳前拒绝。ClientFactory 自行构造每个 owner 的私有标准 transport/client，不接受共享或调用方提供的网络实现。

### MCP 启动、调用与关闭

```text
Manager
└── ServerSession
    └── Connection
        └── Transport
            ├── HTTP response bodies
            └── stdio process、pipes 与 I/O 协程
```

启动：

```text
Manager.Start
  → ServerSession.Start
  → Transport.Start
  → Connection 启动唯一 receive loop
  → initialize
  → capabilities / tools 分页发现
  → 校验累计预算
  → 原子发布工具快照
```

只有 Running Session 发布工具。任一步失败都由当前所有者关闭已创建资源。

调用：

```text
Manager 同一锁区检查 Running 并取得 lease
  → Session 获取并发额度
  → Connection 分配 request ID
  → 注册容量为 1、永不关闭的 pending
  → Transport.Send
  → 唯一 receive loop 接收并校验响应
  → 原子 takePending(ID)
  → 返回安全结果
  → release lease
```

响应、取消、发送失败和连接终止只有一个路径能移除 pending。连接失败时一次性失败全部 pending；未知或迟到 ID 只进入有界诊断。

独立 HTTP 响应超限只失败当前调用；共享 stdio/SSE 无法恢复 framing 时，Connection 失败并关闭 Transport。stdio 只有 supervisor 调用 Wait，关闭时依次停止写入、终止树、reap、等待 I/O 协程、释放句柄。

关闭：

```text
Manager 进入 Closing 并拒绝新 lease
  → 取消 Manager 与 Session context
  → 有界尝试清理远端会话
  → Connection 拒绝发送并失败 pending
  → Transport.Close 解除阻塞 I/O
  → 等待 receive loop 与已有 lease
  → 发布 Closed 快照
```

### Conversation 保存与恢复

```text
计算规范化状态摘要
  ├─ 与 PersistedState 相同      → SaveNoop
  ├─ 仅尾部追加消息且其他状态未变 → 写一个 batch
  └─ 既有消息或其他状态变化      → 写一个 snapshot
```

```text
生成 UpdatedAt、revision 和 digest
  → 有界编码完整 record
  → batch：追加完整 JSONL 行并 fsync
  → snapshot/compact：同目录 tmp + fsync + atomic rename
  → 成功后才更新内存 PersistedState
```

加载器验证版本、revision、PreviousDigest、payload 种类、重算摘要和累计预算。无换行尾记录作为 torn tail 忽略；损坏或超限记录返回 RecoveryReport，不能让会话消失。旧 JSON/v1 非破坏读取，后续成功保存才升级。

List 语义：

- 单文件错误返回 recoverable placeholder 和诊断。
- 达到扫描预算返回部分列表与 `Truncated=true`。
- 根目录无法读取返回顶层错误；App 保留当前页面，不显示伪造空历史。

### 导航、重置与 TUI

```text
TUI key/command
  → Navigation intent
  → App 检查请求状态并增加 generation
  → WaitIdle
  → Save 当前会话
  → List/Create/Load 候选
  → 校验异步结果 generation
  → 原子提交 App 状态
  → 生成 ViewModel
  → TUI 渲染
```

streaming 或等待确认时，导航提示用户先显式取消。WaitIdle、Save、List、Create 或 Load 任一步失败都保留原 screen、Conversation 和 Hook 生命周期；迟到 generation 被丢弃。成功时才触发 SessionEnd、切换并触发 SessionStart。

| 边界 | 清理 | 保留 |
|---|---|---|
| 新请求 | duration、usage、cache、stop reason、error、confirmation、临时缓冲 | 当前会话、mode、skills、Provider/MCP |
| 切换会话 | 新请求状态以及 mode、skills、输入、消息视图、会话通知 | Provider、模型、MCP、全局诊断、运行配置 |

`tea.WindowSizeMsg` 在 screen/key 分发前处理。App 传递 LayoutInput，TUI 只返回 intent。

### 退出与交付门禁

```text
App 停止接收新 intent
  → 取消当前请求
  → Orchestrator.WaitIdle
  → 关闭 ChatStream 和工具资源
  → 保存当前 Conversation
  → 结束 App 会话
  → MCP Manager.Close
  → Hook Shutdown
  → 关闭受控网络客户端
  → Artifact Store 清理并关闭
  → 关闭 safefs 根句柄
  → Diagnostics 最后关闭
```

每层 Close 幂等。调用者超时只停止等待，内部清理继续在 2 秒硬预算内进行；不能完成时产生有界诊断。

```text
Git index / worktree / 指定提交
  → repoaudit
  → 私有目录、secret、gitlink、二进制、文档追溯规则
  → Report
      无 Finding → exit 0
      规则命中   → exit 1
      Git/I/O/策略错误 → 独立失败状态
```

本地 hook、CI 和发布前检查调用同一入口。审计只读，不自动 unstage、删除或改写历史。

## 文件组织

标记：`[新建]` 新增文件；`[修改]` 保留路径并调整；`[迁移]` 从现有文件拆出；`[删除候选]` 仅在替代实现、调用方迁移和测试全部通过后删除。未列出的现有文件保持不动。

### 根目录、CLI 与组装

```text
XAgent/
├── .gitignore                              [修改] 本地运行产物和附件
├── .repoaudit.yaml                         [新建] 审计策略、空 gitlink 声明、精确 fixture 豁免
├── README.md                               [修改] 平台、命令、配置与真实状态
├── config.example.yaml                     [修改] 完整 schema 和安全示例
├── go.mod / go.sum                         [修改] 平台依赖及 tidy 结果
└── cmd/
    ├── xagent/
    │   ├── main.go                         [修改] 调用 RunCLI 并转换退出码
    │   ├── cli.go                          [新建] help/version/config
    │   ├── version.go                      [新建] 构建注入与 VCS 后备
    │   ├── assembly.go                     [迁移] 唯一组装根
    │   ├── lifecycle.go                    [迁移] 反向关闭
    │   ├── cli_test.go                     [新建]
    │   ├── assembly_test.go                [迁移]
    │   ├── lifecycle_test.go               [迁移]
    │   ├── e2e_security_test.go            [新建] 授权、受保护路径与 canary 场景
    │   ├── e2e_output_process_test.go      [新建] Artifact 与进程树场景
    │   ├── e2e_conversation_test.go        [新建] 恢复、外置与导航场景
    │   ├── e2e_mcp_provider_test.go        [新建] 网络、预算与关闭场景
    │   ├── e2e_platform_{darwin,linux,windows}_test.go [新建] 三平台原生安全场景
    │   └── e2e_performance*_test.go        [新建] 当前 adapter 与性能比较
    ├── xagent-repo-check/
    │   ├── main.go                         [新建]
    │   └── main_test.go                    [新建]
    └── xagent-check/
        ├── main.go                         [新建] 本地/CI 聚合入口
        ├── run.go                          [新建] 质量 profile
        └── run_test.go                     [新建]
```

### 安全基础包

```text
internal/
├── redact/
│   ├── redact.go                           [修改]
│   ├── safe_text.go                        [新建]
│   ├── runtime.go                          [迁移]
│   ├── safe_text_test.go                   [新建]
│   └── runtime_test.go                     [迁移]
├── budget/
│   ├── limits.go                           [新建]
│   ├── counter.go                          [新建]
│   ├── error.go                            [新建]
│   ├── counter_test.go                     [新建]
│   └── overflow_test.go                    [新建]
├── diagnostics/
│   ├── diagnostic.go                       [修改]
│   ├── collector.go                        [修改]
│   ├── sink.go                             [新建]
│   ├── snapshot.go                         [新建]
│   ├── sanitize.go                         [迁移]
│   ├── http_summary.go                     [修改]
│   ├── collector_test.go                   [新建]
│   ├── sanitize_test.go                    [新建]
│   ├── http_summary_test.go                [新建]
│   └── secret_canary_test.go               [新建]
├── safefs/
│   ├── api.go / policy.go / capability.go  [新建]
│   ├── path.go / walk.go                   [新建]
│   ├── root_posix.go                       [新建] darwin || linux
│   ├── atomic_posix.go                     [新建] darwin || linux
│   ├── root_windows.go                     [新建]
│   ├── atomic_windows.go                   [新建]
│   ├── root_unsupported.go                 [新建]
│   ├── root_test.go                        [新建]
│   ├── root_posix_test.go                  [新建]
│   └── root_windows_test.go                [新建]
├── proctree/
│   ├── api.go / lifecycle.go               [新建]
│   ├── protected_exec.go                   [新建]
│   ├── runner_factory_darwin.go            [新建]
│   ├── runner_factory_linux.go             [新建]
│   ├── runner_factory_windows.go           [新建]
│   ├── runner_factory_unsupported.go       [新建]
│   ├── runner_posix.go                     [新建] darwin || linux
│   ├── protected_darwin.go                 [新建]
│   ├── protected_linux.go                  [新建]
│   ├── runner_windows.go                   [新建]
│   ├── protected_windows.go                [新建]
│   ├── runner_unsupported.go               [新建]
│   ├── lifecycle_test.go                   [新建]
│   ├── assembly_contract_test.go           [新建]
│   ├── runner_posix_test.go                [新建]
│   └── runner_windows_test.go              [新建]
├── artifact/
│   ├── api.go / file_store.go              [新建]
│   ├── writer.go / cleanup.go              [新建]
│   ├── private_posix.go                    [新建]
│   ├── private_windows.go                  [新建]
│   ├── private_unsupported.go              [新建]
│   └── file_store_test.go                  [新建]
└── netpolicy/
    ├── policy.go / endpoint.go             [新建]
    ├── resolver.go / client.go             [新建]
    ├── redirect.go / dial.go / error.go     [新建]
    ├── policy_test.go                      [新建]
    ├── redirect_test.go                    [新建]
    └── dial_test.go                        [新建]
```

所有 POSIX 文件显式使用 `//go:build darwin || linux`。Windows 和 unsupported 路径不能落入弱化实现。

### 权限、工具、Instruction 与 Hook

```text
internal/
├── permission/
│   ├── authorizer.go                       [修改]
│   ├── identity.go / bash_identity.go      [新建]
│   ├── ticket.go / health.go               [新建]
│   ├── decision.go / loader.go             [修改]
│   ├── writer.go / matcher.go              [修改]
│   ├── session.go / blacklist.go           [修改]
│   ├── rule.go / result.go                 [修改]
│   ├── sandbox.go                          [迁移→删除候选]
│   ├── identity_test.go                    [新建]
│   ├── ticket_test.go                      [新建]
│   ├── health_test.go                      [新建]
│   └── writer_test.go                      [新建]
├── tool/
│   ├── tool.go                             [修改]
│   ├── execution_policy.go                 [新建]
│   ├── execution_state.go                  [新建]
│   ├── result.go / result_factory.go       [新建]
│   ├── capture.go                          [新建]
│   ├── executor.go / registry.go           [修改]
│   ├── validation.go / read_scope.go       [修改]
│   ├── bash.go                             [修改]
│   ├── shell_posix.go                      [新建]
│   ├── shell_windows.go                    [新建]
│   ├── shell_unsupported.go                [新建]
│   ├── read.go / grep.go / glob.go         [修改]
│   ├── write.go / edit.go                  [修改]
│   ├── path.go                             [迁移→删除候选]
│   ├── executor_test.go / capture_test.go  [新建]
│   ├── file_tools_test.go                  [新建/迁移]
│   ├── bash_runtime_test.go                [新建]
│   ├── bash_posix_test.go                  [新建]
│   └── bash_windows_test.go                [新建]
├── instructions/
│   ├── loader.go / include.go              [修改]
│   ├── cache.go / types.go                 [新建]
│   ├── sandbox.go                          [迁移→删除候选]
│   ├── loader_test.go                      [修改]
│   ├── include_test.go                     [新建]
│   ├── include_posix_test.go               [新建]
│   └── include_windows_test.go             [新建]
└── hook/
    ├── api.go / engine.go                  [修改]
    ├── command.go                          [修改]
    ├── command_shell_posix.go              [新建]
    ├── command_shell_windows.go            [新建]
    ├── command_shell_unsupported.go        [新建]
    ├── command_unix.go                     [迁移→删除候选]
    ├── command_other.go                    [删除候选]
    ├── http.go / limits.go / loader.go     [修改]
    ├── diagnostic.go / async.go            [修改]
    ├── command_test.go / http_test.go      [修改]
    └── lifecycle_test.go                   [新建]
```

`internal/skill/discovery.go` 改为使用 safefs，移除 Unix 包直接导入。

### Provider、MCP 与 Transport

```text
internal/
├── provider/
│   ├── provider.go                         [修改] 公共契约
│   ├── request.go / event.go               [迁移]
│   ├── stream.go / stream_limits.go        [新建]
│   ├── observer.go                         [迁移]
│   ├── factory.go                          [修改]
│   ├── sse_decoder.go                      [迁移]
│   ├── openai.go                           [修改]
│   ├── openai_request.go                   [迁移]
│   ├── openai_stream.go                    [迁移]
│   ├── anthropic.go                        [修改]
│   ├── anthropic_request.go                [迁移]
│   ├── anthropic_stream.go                 [迁移]
│   └── *_stream_test.go                    [新建]
└── mcpclient/
    ├── manager.go                          [修改]
    ├── lease.go                            [新建]
    ├── session.go / connection.go          [迁移]
    ├── pending.go / factory.go             [迁移]
    ├── adapter.go / names.go               [修改]
    ├── protocol/
    │   ├── types.go / jsonrpc.go           [迁移]
    │   ├── codec.go                        [新建]
    │   └── codec_test.go                   [新建]
    ├── transport/
    │   ├── transport.go / state.go         [新建]
    │   ├── limits.go                       [新建]
    │   ├── http/
    │   │   ├── config.go / transport.go    [迁移]
    │   │   ├── sse.go / json.go            [迁移]
    │   │   ├── body_registry.go            [新建]
    │   │   ├── session.go                  [迁移]
    │   │   └── *_test.go                   [新建/迁移]
    │   └── stdio/
    │       ├── config.go / transport.go    [迁移]
    │       ├── framing.go / stderr.go      [迁移]
    │       ├── writer.go / supervisor.go   [新建]
    │       └── *_test.go                   [新建/迁移]
    ├── manager_lifecycle_test.go           [新建]
    ├── lease_test.go                       [新建]
    ├── session_test.go                     [新建]
    └── connection_test.go                  [新建]
```

Provider 保持单包，避免 adapter 反向导入公共契约。MCP Transport 拆为子包，使 Manager、Connection 和物理资源所有权也在 import 层清晰。

### 配置、会话、编排与界面

```text
internal/
├── config/
│   ├── config.go / load.go / validate.go   [修改]
│   ├── optional.go / partial.go            [新建]
│   ├── decode.go                           [迁移]
│   ├── merge.go                            [新建]
│   ├── resolve.go                          [迁移]
│   ├── mcp.go                              [修改]
│   └── {decode,merge,resolve}_test.go       [新建]
├── conversation/
│   ├── conversation.go / message.go        [修改]
│   ├── store.go                            [修改]
│   ├── state_digest.go                     [新建]
│   ├── jsonl_record.go / jsonl_store.go    [修改]
│   ├── jsonl_save.go / jsonl_load.go       [迁移]
│   ├── recovery.go / cleanup.go            [修改]
│   ├── migration.go                        [新建]
│   ├── file_store.go                       [迁移→删除候选]
│   └── {state_digest,recovery,migration}_test.go [新建]
├── orchestrator/
│   ├── chat.go / agent_loop.go             [修改]
│   ├── execution_state.go                  [修改]
│   ├── tool_scheduler.go                   [迁移]
│   ├── tool_batches.go                     [迁移→删除候选]
│   ├── stream_collector.go                 [修改]
│   ├── run_tracker.go / independent.go     [修改]
│   ├── tool_scheduler_test.go              [新建]
│   └── cancellation_test.go                [修改]
├── app/
│   ├── app.go / deps.go                    [修改]
│   ├── state.go / navigation.go            [新建]
│   ├── lifecycle.go / update.go            [修改]
│   ├── events.go / commands.go             [修改]
│   ├── navigation_test.go                  [新建]
│   └── state_test.go                       [新建]
└── tui/
    ├── view_model.go / layout.go           [新建]
    ├── input.go / messages.go / list.go    [修改]
    ├── status.go / command_menu.go         [修改]
    ├── confirmation.go                     [新建]
    ├── artifact.go / help.go               [新建]
    ├── program.go                          [修改]
    ├── layout_test.go                      [新建]
    ├── confirmation_test.go                [新建]
    └── help_test.go                        [新建]
```

适配文件：

| 文件 | 调整 |
|---|---|
| `internal/events/events.go` | 事件只携带安全、有界 DTO |
| `internal/contextmgr/manager.go` | 只处理安全工具视图和 artifact 元数据 |
| `internal/sessionctx/manager.go` | 只向 Prompt 注入安全内容 |
| `internal/prompt/dynamic.go` | 禁止 raw 工具输出进入模型 |
| `internal/memory/{manager,update,write}.go` | 写入前统一脱敏和预算 |
| `internal/skill/{discovery,activity,prompt}.go` | safefs 发现及请求/会话状态边界 |
| `internal/command/{definition,registry,builtins}.go` | 命令、快捷键与帮助共享元数据 |

### 审计、测试、CI 与文档

```text
internal/
├── repoaudit/
│   ├── types.go / auditor.go               [新建]
│   ├── git_source.go                       [新建]
│   ├── private_paths.go / secrets.go       [新建]
│   ├── gitlinks.go / binaries.go           [新建]
│   ├── docs.go                             [新建]
│   └── *_test.go                           [新建]
├── testutil/
│   ├── fake_provider.go                    [修改]
│   ├── conversation_store.go               [新建]
│   ├── fake_mcp.go / safefs.go             [新建]
│   ├── proctree.go / artifact.go           [新建]
│   └── clock.go                            [新建]
└── e2e/
    ├── harness.go / assertions.go          [新建] e2e tag；fixture 与断言辅助
    ├── performance.go                      [新建] e2e tag；archive、构建与比较
    ├── perfprotocol/protocol.go             [新建] adapter NDJSON 协议
    ├── perfworkload/workload.go             [新建] 共享 workload 与规范摘要
    └── testdata/
        ├── processhelper/                  [新建] 父、子、孙进程握手 helper
        └── performance/
            └── adapter_607b_test.go        [新建] 固定 baseline revision adapter

.github/
├── workflows/
│   └── ci.yml                              [新建] fmt、static、test×3、race、coverage、
│                                               三平台、敏感场景×20、repoaudit、E2E
└── xagent-actions.lock.json                [新建] 人工审核并冻结的远端 workflow/action 依赖闭包

docs/
├── index.md                                [新建] 当前与历史规格状态
├── security-reliability-delivery-quality-2026-07-30/
│   ├── spec.md                             [现有]
│   ├── plan.md                             [本流程生成]
│   ├── task.md                             [Plan 批准后生成]
│   └── checklist.md                        [Task 批准后生成]
└── <既有主题>/{spec,plan,task,checklist}.md
                                                [按需修改] 状态、superseded_by、有效证据
```

### 删除候选

以下路径仅在替代实现完成、`rg` 无引用、旧格式兼容测试和三平台安全测试通过后删除：

```text
internal/provider/sse.go
internal/mcpclient/http.go
internal/mcpclient/http_closer.go
internal/mcpclient/stdio.go
internal/mcpclient/stdio_unix.go
internal/mcpclient/stdio_other.go
internal/mcpclient/jsonrpc.go
internal/mcpclient/protocol.go
internal/mcpclient/limits.go
internal/mcpclient/redact.go
internal/permission/sandbox.go
internal/tool/path.go
internal/instructions/sandbox.go
internal/hook/command_unix.go
internal/hook/command_other.go
internal/orchestrator/tool_batches.go
internal/conversation/file_store.go
```

### 当前仓库数据边界

实施阶段仅将已识别的非产品条目移出 Git index，本地数据保持原位：

```text
.claude/worktrees/**
.mewcode/memory/**
.mewcode/sessions/**
fakeprovider
*.webarchive
photo_*.jpg / photo_*.jpeg / photo_*.png
其他已确认的本地二进制或个人附件
```

不会创建 `.gitmodules` 来合法化 worktree gitlink，也不会删除、移动或覆盖这些文件。本节不授权任何 unstage 或数据清理。

## 技术决策

### 安全、文件与执行

| ADR | 决策 | 理由、否决方案与代价 | 追溯 |
|---|---|---|---|
| SEC-01 | 唯一 RuntimeRedactor；跨边界文本使用 SafeText | 否决模块独立正则脱敏；代价是所有输出边界需改造 | F7、N2 |
| FS-01 | 句柄相对文件访问，校验和 I/O 使用同一 Root 链 | 否决字符串前缀及 `EvalSymlinks → Open`；代价是平台实现 | F4、F12、F13、F31 |
| FS-02 | 普通与权限专用 capability 分离，并保护尚不存在的槽位 | 否决路径黑名单及 Ticket 兼任文件能力 | F4、F6 |
| FS-03 | 安全文件同目录 staging、flush、原子 replace、必要时同步父目录 | 否决直接 truncate/write 和跨卷临时文件 | F4、F5、N9 |
| PROC-01 | Linux 使用 capability-probed Landlock/`no_new_privs`，macOS 使用 capability-probed Seatbelt，Windows 使用受限 token/访问策略；Job Object 管生命周期 | 否决命令扫描、只读位、事后恢复和弱化 fallback；能力不足时 Bash 不可用 | F4、F11、F31、O4 |
| PROC-02 | 保护策略验证和 containment 成功后才 spawn/resume | 否决 warning 后继续；旧内核可能保守拒绝 | F4、F5、N1 |
| PROC-03 | proctree.Process 是唯一 Wait 所有者 | 否决裸 `exec.CommandContext`、只杀父进程和双 Wait | F11、F16、N7、N8 |
| AUTH-01 | Bash 身份使用 Shell、工作目录身份、环境摘要和原始字节 | 否决 trim、展示文本和简单拼接摘要 | F3、F6 |
| AUTH-02 | Rule 与一次性 Ticket 分离；每次执行都签新 Ticket | 否决可复制 Grant 作为执行凭证 | F3、F5、F24 |
| AUTH-03 | 权限损坏时危险工具 fail closed，只有内建低风险只读 allowlist 可受限继续 | 否决空配置回退、危险 last-known-good 和确认覆盖 | F5、N1 |
| RES-01 | 预算在采集前消费，同一操作共享累计 Counter | 否决 ReadAll 后截断、逐文件重置和只设超时 | F9、F12–F14、N5 |
| ART-01 | 私有 artifact 是唯一 raw 原文例外，从第一字节写 staging | 否决 raw 进入 JSONL/memory 或把路径交给模型 | F7、F9、F10、N3 |
| ART-02 | Artifact 也有单文件、总容量和保留期硬上限 | 否决无限写入和静默丢弃；极端输出只保留到停止点 | F9、F10、N5 |
| RES-02 | ResultFactory 一次生成三个安全视图，Result 不保留 raw | 否决调用方自行脱敏同一个字符串 | F7、F10、F24 |
| DIAG-01 | 统一有界聚合诊断，先脱敏清理再保存 | 否决各模块无界错误数组；必须报告聚合和 dropped | F2、F7、F14、N14 |

平台 API 返回成功不是充分验收条件；必须运行真实绕过样本。能力探测失败的环境必须证明 Bash 未启动。

### Provider、网络与 MCP

| ADR | 决策 | 理由、否决方案与代价 | 追溯 |
|---|---|---|---|
| NET-01 | Provider、远程 MCP、HTTP Hook 共用 ClientFactory；初始、redirect、dial 都复验 | 否决 adapter 自检和默认 redirect；跨源重定向不再兼容 | F7、F8 |
| PROV-01 | Provider 返回显式 ChatStream，调用者立即登记 Close | 否决只返回 channel 和 GC/finalizer | F17、N8 |
| PROV-02 | 单生产者拥有 body、SDK stream 和事件通道 | 否决多读取者和调用者关通道；增加 closeOnce/closeDone | F17、N7 |
| PROV-03 | `finish_reason` 不终止 OpenAI 流；`[DONE]` 才正常完成 | 否决停止原因即返回和任意 EOF 成功 | F17、F18 |
| MCP-01 | 固定 Manager→Session→Connection→Transport 所有权 | 否决 Manager 平铺持有全部资源 | F15、F16 |
| MCP-02 | 同锁完成 Running 检查、lease 获取和 Closing | 否决单 closed bool 与锁外 WaitGroup.Add | F15、N8 |
| MCP-03 | 唯一 receive loop；pending 容量 1 且不关闭；原子 takePending | 否决每调用读取 Transport 和取消时关通道 | F15、F16 |
| MCP-04 | Transport 仅负责字节、framing、预算和物理资源 | 否决 Transport 直接返回业务 ToolResult | F14–F16 |
| MCP-05 | HTTP 单响应超限只失败该调用；不可重同步的共享流关闭 Connection | 否决超限后盲目继续解析 | F14、N5 |
| MCP-06 | 远端 session 清理有界、尽力而为，本地关闭不依赖远端成功 | 否决空 Close 或远端成功后才退出 | F15、N7 |
| MCP-07 | stdio 只能通过 proctree；唯一 supervisor Wait，stderr 持续排空 | 否决裸 Cmd、只杀父进程和无主写 goroutine | F16、F31 |

### 配置、会话与编排

| ADR | 决策 | 理由、否决方案与代价 | 追溯 |
|---|---|---|---|
| CFG-01 | decode/merge 使用 Optional，最终 Config 使用确定值；固定四层优先级 | 否决零值推断 presence 和业务模块补默认值 | F22、F28 |
| CFG-02 | false、0、负数参与合并，合并后验证；null 和未知字段失败 | 否决把所有 `<=0` 当未设置 | F28、N17 |
| CFG-03 | 旧 `tool.max_output_bytes` 映射为内联预览上限；新旧冲突时报错 | 否决静默改义或立即删除；保留一版兼容入口 | N16、N17 |
| DATA-01 | v2 使用规范化摘要；纯追加 batch，其他变化 snapshot | 否决消息数量判断和每次全量重写 | F19、F20、N11 |
| DATA-02 | 每次 Save 一个可见 record；fsync/rename 后才更新 PersistedState | 否决先改内存再落盘 | F19、F20、N9 |
| DATA-03 | torn tail 忽略；旧 JSON/v1 非破坏加载，成功保存后升级 | 否决加载时原地覆盖或删除 | F19、N9、N10 |
| DATA-04 | ListResult 表达部分结果、预算与诊断；顶层 error 只代表列表不可信 | 否决单文件坏即空列表或静默忽略 | F21、F22 |
| ORCH-01 | Risk、ReadOnly、ConcurrentSafe 分离 | 否决 RiskSafe 等同可并发 | F23 |
| ORCH-02 | Worker 写预分配槽，join 后按原顺序更新 | 否决 goroutine 直接并发修改 Conversation | F23、N11 |
| ORCH-03 | 启动前取消无 tool-role Result；启动后取消记录真实 Result | 否决空结果填补或全部当超时 | F24 |
| APP-01 | Runtime、Conversation、Request 状态分离并统一 reset | 否决 Model 零散字段复位 | F26 |
| APP-02 | 导航是 WaitIdle→Save→候选→原子提交；运行中先显式取消 | 否决先切 UI 再保存和隐式取消 | F21、F25 |
| UI-01 | TUI 只消费 ViewModel/产生 intent；80×24 完整，小于基线 compact | 否决 TUI 直连领域服务和伪造终端尺寸 | F25–F29 |
| CLI-01 | help/version 在配置前；命令、快捷键、帮助共享元数据 | 否决把 help 当错误及多份帮助文案 | F29、F30 |

旧有效配置继续解析。旧权限规则增加版本：能无损表达的自动迁移；无法证明等价的旧 Bash 自动允许规则进入 `legacy_untrusted`，重新确认后才能生成新规则，原文件在迁移成功前保留。

### 跨平台与交付

| ADR | 决策 | 理由、否决方案与代价 | 追溯 |
|---|---|---|---|
| PLAT-01 | 共用 POSIX 文件显式 `darwin || linux`；平台机制分别拆文件；unsupported 只返回错误 | 否决公共文件导入 Unix 包、runtime.GOOS 和 `!unix` no-op | F31 |
| REPO-01 | 默认审计 Git index，CI 审计提交树，可显式审计 worktree；Git 数据 NUL-safe | 否决只扫工作目录和解析人类可读 Git 输出 | F1、F2 |
| REPO-02 | Fixture 精确豁免；gitlink 声明默认空 | 否决目录排除、通用假密钥白名单和自动建 .gitmodules | F2 |
| REPO-03 | 仓库止血只移出 index并忽略，本地数据保留 | 否决删除本地数据、改写历史或自动轮换凭据 | F1、O5 |
| CI-01 | 跨平台 Go 聚合命令是本地和 CI 共同入口 | 否决只提供 POSIX shell 脚本 | F32 |
| CI-02 | 默认测试使用 fake、确定性时钟和故障注入 | 否决公网、真实凭据和固定短 sleep | F32、N18–N21 |
| DOC-01 | 文档记录状态和 superseded_by；追溯关系可机器检查 | 否决删除旧规格和用旧勾选作为当前证据 | F33、N22 |
| MIG-01 | M0–M5 分段，但安全边界原子切换，不长期保留双入口 | 否决一次性大爆炸和长期不安全兼容旁路 | F3–F33 |

### 初始默认预算与硬上限

硬上限编译进安全模块，配置只能在有效范围内调整，不能关闭。

| 范围 | 默认值 | 不可提高的硬上限 |
|---|---:|---:|
| 模型/会话内联工具预览 | 32 KiB | 1 MiB |
| 单次工具原始捕获与 artifact | 64 MiB | 512 MiB |
| Artifact 总容量 | 1 GiB | 8 GiB |
| Artifact 保留期 | 7 天 | 365 天 |
| 工具执行超时 | 30 秒 | 24 小时 |
| Read 单文件读取 | 16 MiB | 256 MiB |
| Grep/Glob 累计扫描 | 256 MiB、10 万文件、2.5 万目录、100 万行 | 2 GiB、100 万文件、25 万目录、1000 万行 |
| Instruction | 单文件 64 KiB、累计 1 MiB、64 文件、展开 2 MiB、深度 5 | 单文件 1 MiB、累计 16 MiB、1024 文件、展开 32 MiB、深度 32 |
| MCP 单次响应 | 1 MiB | 16 MiB |
| MCP 工具/分页/协议错误 | 128 / 32 / 32 | 1024 / 128 / 256 |
| Provider 原始响应/单事件/事件数 | 16 MiB / 1 MiB / 10 万 | 64 MiB / 4 MiB / 100 万 |
| Provider 正文/thinking/工具参数 | 8 MiB / 8 MiB / 1 MiB | 32 MiB / 32 MiB / 8 MiB |
| Conversation 单记录/单会话 | 16 MiB / 256 MiB | 64 MiB / 1 GiB |
| Conversation 列表扫描 | 1000 文件 / 10 MiB | 10 万文件 / 1 GiB |
| Conversation 保留期 | 30 天 | 3650 天 |
| Conversation 时间跨度提醒 | 7 天 | 3650 天 |
| Diagnostics | 100 项、单项 2 KiB、总计 2 MiB | 1000 项、单项 64 KiB、总计 16 MiB |
| 取消与关闭清理 | 2 秒 | 2 秒 |

Hook、Memory、Agent、模型窗口及其他既有公开数值键的默认值、最小值和硬上限由已批准的 C14 补齐；与本表一起由同一配置 schema 明示并由 ResolveConfig 统一验证。

## 实施前契约补遗（C1–C17 已批准）

任务拆解复核确认总体架构与 Spec 没有冲突，但原 Plan 有若干类型、所有权和用户入口尚不足以让模块独立实现。以下条款补齐契约；与前文简化签名不一致时，以本节为准。

### C1：SafeFS bootstrap 与能力签发

```go
package safefs

type CapabilityClass uint8

const (
	OrdinaryWrite CapabilityClass = iota
	ProtectedWrite
)

type Capabilities struct {
	// 两种带私有 seal 的能力；本对象只返回给组装根。
}

type OpenResult struct {
	Root         *Root
	Capabilities Capabilities
}

type Binding struct {
	// Root 身份、对象身份或父目录身份＋目标名的版本化摘要；不可伪造。
}

func Bootstrap(path string, policy Policy) (OpenResult, error)
func (c Capabilities) Ordinary() Capability
func (c Capabilities) Protected() Capability
func (r *Root) Bind(relative string) (Binding, error)
```

- `Root` 本身不能签发、升级或复制 capability；零值和跨 Root capability 无效。
- `cmd/xagent` 保留 `Capabilities` 容器，只把 Ordinary capability 注入 Write/Edit，把 Protected capability 注入 permission Writer；任何领域模块都拿不到容器。
- Protected capability 只能写 Policy 中明确列出的受保护槽位，Ordinary capability 明确排除这些槽位；现存对象和“父目录身份＋尚不存在名称”采用相同语义。

### C2：通用调用身份与 Ticket 签发

```go
package permission

type CallIdentityInput struct {
	ToolName           string
	CanonicalArguments []byte
	WorkingDirectory   *safefs.Identity
	ResourceBindings   []safefs.Binding
	EnvironmentDigest  *[32]byte
	TargetDigest       *[32]byte
}

type TicketIssuer interface {
	Issue(callID string, identity CallIdentity) (ExecutionTicket, error)
}

func NewCallIdentity(input CallIdentityInput) (CallIdentity, error)
func NewBashIdentity(input BashIdentityInput) (CallIdentity, error)
```

- schema 校验后先把 JSON 规范化为确定字节；map 遍历顺序、展示文本和脱敏文案不参与身份。
- 文件资源由 safefs 生成不可伪造的 `Binding`：已存在对象绑定句柄身份，未存在对象绑定父目录身份和目标名。
- MCP 身份绑定注册名、服务器最终配置摘要和规范参数；Bash 仍使用专用构造器绑定原始命令字节。
- callID 不进入 identity，但同时写入 Ticket；每次规则命中或确认都通过 Issuer 签发新 Ticket。

### C3：受保护进程、管道与清理所有权

```go
package proctree

type ProtectionMode uint8

const ProtectionRequired ProtectionMode = 1

type ProtectionPlan struct {
	// 由已打开 Root、受保护槽位和每次运行的私有 scratch 构造；不可序列化。
}

func NewProtectionPlan(roots []*safefs.Root, scratch *safefs.Root) (ProtectionPlan, error)

type ProtectionPlanFactory interface {
	Create(ctx context.Context) (ProtectionPlan, error)
}

func NewProtectionPlanFactory(roots []*safefs.Root, scratchParent string) (ProtectionPlanFactory, error)

type Pipes struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

type StartError struct {
	Code          string
	TargetStarted bool
}

type Request struct {
	Executable string
	Args       []string
	WorkingDir *safefs.Root
	Env        []string
	Mode       ProtectionMode
	Protection ProtectionPlan
}

type Process interface {
	Pipes() Pipes
	CloseStdin() error
	Wait(ctx context.Context) (Result, error)
	Terminate(ctx context.Context) error
	Close(ctx context.Context) error
}

type Runner interface {
	Start(ctx context.Context, request Request) (Process, error)
}

func NewRunner(options Options) (Runner, error)
```

- `NewRunner` 通过互斥 build tag 只选择当前平台的受保护实现；unsupported 平台返回只会在目标启动前拒绝的 Runner，不能回退到裸 `exec`。`ProtectionPlanFactory` 固定已打开 roots 与私有 scratch parent，每次 `Create` 生成独立、不可序列化的 plan；plan 交付 `Start` 后即由 Runner 接管，任何启动失败或启动前取消均清理 scratch，成功启动则由 Process 终态清理；业务模块不能自行拼装或放宽 plan。
- Bash、命令 Hook 和 stdio MCP 均必须传 `ProtectionRequired`；缺失、零值、能力不足或策略验证失败均在目标程序 exec 前 fail closed。
- Runner 只有在保护层安装成功并收到 exec 成功握手后才返回 `Process`。返回 `StartError` 时 `TargetStarted` 必须为 false；已成功 exec 后的失败只通过 Process 终态报告。
- `Process` 内部的 supervisor 是唯一 OS `Wait`/reap 所有者。外层 stdio supervisor 只能调用可重复等待的 `Process.Wait`，不得接触裸 `exec.Cmd` 或 OS handle。
- Process 拥有三个 pipe；调用方只借用 `Pipes` 读写，借用句柄的 `Close` 仍 fail closed。无输入或写入完成后只能调用幂等 `CloseStdin`，由 Process 原子阻止新写并关闭 owner stdin 以向目标发送 EOF；Close 再按“阻止新写、终止进程树、reap、关闭其余 pipe、等待 I/O”收敛，任何层都不得关闭事件或 pending channel 来解除竞争。
- Linux 通过同一 XAgent 可执行文件的隐藏 self-reexec launcher，在目标 exec 前安装 `no_new_privs` 与 Landlock；macOS 使用身份校验后的系统 Seatbelt launcher；Windows 在 suspended process 上安装 Job Object 与低完整性/受限 token 后才 resume。
- 三个平台默认只给每次运行的私有 scratch 写能力；项目根和权限配置根对不受信子进程只读。该策略可能保守拒绝 Bash/Hook/MCP 对项目的写入，但不能放宽受保护文件约束。缺少所需平台能力时，AC31 的等价安全结果是“对应不受信进程没有启动”，而不是跳过测试或使用弱化 fallback。
- `cmd/xagent/internal_mode.go` 与 `internal/proctree/launcher_linux.go` 纳入文件组织；内部 launcher 在普通 help 中隐藏，只接受父进程通过继承句柄传入的版本化 plan，拒绝从普通 CLI 参数构造或放宽 ProtectionPlan。

### C4：网络客户端的不可绕过边界

```go
package netpolicy

type ClientOptions struct {
	Timeout          time.Duration
	SensitiveHeaders []string
	TrustedRoots     *x509.CertPool
}

type Client interface {
	Do(req *http.Request) (*http.Response, error)
	SDKHTTPClient() *http.Client
	CloseIdleConnections()
}

type ClientFactory interface {
	New(endpoint Endpoint, options ClientOptions) (Client, error)
}
```

- `ClientFactory` 自行从标准库默认安全值构造每个 owner 的私有 client/transport，不接收 `http.Client`、RoundTripper、Proxy、Dial/DialTLS、CheckRedirect、CookieJar 或协议 handler。Provider、MCP、Hook 和 Assembly 均无注入这些对象的入口。
- `TrustedRoots` 是唯一允许的传输自定义项；Factory 在构造时深拷贝证书池并只用于标准 TLS 验证。nil 使用系统信任根，调用方后续修改原池不能影响已创建 Client。
- `Client` 是持有私有标准 client 与受控 RoundTripper 的 concrete wrapper；Factory 覆盖 Proxy、拨号、TLS 拨号和 redirect 行为，Provider、MCP 和 Hook 的业务代码只使用 `Do`。
- `SDKHTTPClient` 只供必须接收 `*http.Client` 的 SDK adapter 使用，返回的 client 必须原样传入 SDK，不得替换 Transport 或 redirect policy。
- 安全约束同时位于 RoundTripper/dial 层；即使 SDK 自行构造 redirect 请求，每次 RoundTrip 仍复验 endpoint，跨源敏感头在传输前被移除并拒绝请求。
- 默认敏感头至少覆盖 Authorization、Proxy-Authorization、Cookie、API key 类头；配置可增加、不可移除默认集合。创建者负责 `CloseIdleConnections`。

### C5：Provider 与跨边界安全 DTO

```go
package diagnostics

type SafeError struct {
	Code        string
	Source      string
	Message     redact.SafeText
	Recoverable bool
}

package provider

type SafeToolCall struct {
	ID             string
	Name           string
	ArgumentsJSON  redact.SafeText
}

type StreamEvent struct {
	Type     StreamEventType
	Delta    redact.SafeText
	Error    *diagnostics.SafeError
	Usage    *Usage
	ToolCall *SafeToolCall
}
```

- `ChatRequest` 使用只包含 `SafeText` 的 `ModelMessage`/`SystemBlock`，不直接接收 conversation.Message 或普通 string 内容。
- Provider parser 可在内部短暂持有受预算约束的 raw frame，但事件发布前必须脱敏；工具参数随后只从安全 JSON 解析。若脱敏导致参数不再是合法 JSON，则返回 `unsafe_tool_arguments`，不执行工具。
- MCP adapter、Hook、Events、Conversation、Memory 和 TUI 同样只能跨边界传 `SafeText`、`SafeError`、安全 Result 或 opaque artifact Ref；协议 Transport 的 raw frame 不得越过 Connection/adapter。

### C6：MCP 远端 session 清理

```go
package mcpclient

type RemoteSessionCloser interface {
	CloseRemote(ctx context.Context) error
}
```

- HTTP Transport 保存最近一次有效 session ID 并实现 `RemoteSessionCloser`；stdio Transport 不实现。
- `ServerSession.Close` 在 Connection 仍可用时先类型断言并用独立有界 context 调用 `CloseRemote`，然后无条件关闭 Connection/Transport；远端失败只产生安全诊断，不能阻塞本地回收。
- Transport 的普通 `Close` 只负责本地 body、pipe、连接和 worker，不重复发送远端清理请求。

### C7：异步生命周期 Close 的双 context 语义

- ChatStream、MCP Manager/Session/Connection/Transport、proctree Process、Hook Engine 和 App runtime 各自的 Options 都包含 `CleanupTimeout time.Duration` 与 `Diagnostics diagnostics.BoundedSink`；只关闭本地文件句柄且不会阻塞的 Root/Store 不强制套用异步状态机。
- 第一次 Close 通过 `sync.Once` 启动独立清理；内部 context 不继承调用者取消，只受 2 秒硬上限约束。
- 调用者 context 只限制本次等待。调用者先超时则返回其 context error，后台清理继续；这类等待错误不缓存为清理结果。
- 清理完成后缓存最终结果，后续 Close 返回同一最终结果；超出内部硬上限时执行最后的强制关闭并向统一 Sink 只写一次诊断。

### C8：公开配置键与 Artifact 默认位置

新增配置键固定如下；旧 `tool.max_output_bytes` 保留一版并迁移到 `tool.inline_output_bytes`，两者冲突时报错。

| 范围 | 配置键 |
|---|---|
| 工具输出 | `tool.inline_output_bytes`, `tool.capture_bytes`, `tool.timeout_ms` |
| Artifact | `artifact.root`, `artifact.max_file_bytes`, `artifact.max_total_bytes`, `artifact.retention_days` |
| 文件 | `files.read_max_bytes`, `files.scan_max_bytes`, `files.scan_max_files`, `files.scan_max_directories`, `files.scan_max_lines` |
| Instruction | `instructions.max_file_bytes`, `instructions.max_total_bytes`, `instructions.max_files`, `instructions.max_expanded_bytes`, `instructions.max_include_depth` |
| MCP | `mcp.max_response_bytes`, `mcp.max_tools`, `mcp.max_pages`, `mcp.max_protocol_errors` |
| Provider 流 | `llm.stream.max_response_bytes`, `llm.stream.max_event_bytes`, `llm.stream.max_events`, `llm.stream.max_text_bytes`, `llm.stream.max_thinking_bytes`, `llm.stream.max_tool_arguments_bytes` |
| Conversation | `session.max_record_bytes`, `session.max_session_bytes`, `session.max_scan_files`, `session.max_scan_bytes`, `session.retention_days`, `session.gap_reminder_days` |
| Diagnostics/关闭 | `diagnostics.max_items`, `diagnostics.max_item_bytes`, `diagnostics.max_total_bytes`, `lifecycle.cleanup_timeout_ms` |

- 未设置 `artifact.root` 时，使用 OS 用户缓存目录下的 `xagent/artifacts/<project-id>`；`project-id` 是项目 Root 稳定身份的 SHA-256，不含明文路径。
- 显式 artifact root 必须位于工作区外、通过私有权限检查且不能经链接回到工作区；默认缓存目录不可用时启动失败，不回退到项目目录。
- `lifecycle.cleanup_timeout_ms` 可降低但不能高于 2000；其他默认值和硬上限沿用前表。

### C9：会话导航的公开操作

- `/new` 创建并切换到新会话；`/sessions` 打开会话列表，公开别名为 `/list`。
- 空闲聊天页按 `Esc` 打开会话列表；请求流式执行或等待确认时，第一次 `Esc` 只执行显式取消，清理完成后再次触发导航。
- 会话列表中 `Enter` 恢复选中会话、`n` 新建、`q` 维持退出行为；所有绑定来自 command metadata 并由 `/help` 展示。
- 上述操作只产生 App intent；仍按 `WaitIdle → Save → List/Create/Load → generation 校验 → 原子提交` 执行。

### C10：旧 `ExternalPath` 非破坏迁移

```go
package conversation

type LegacyArtifactImporter interface {
	Import(ctx context.Context, legacyPath string, metadata artifact.Metadata) (artifact.Ref, error)
}
```

- 组装根只向 migration 注入该接口；Importer 通过旧数据根的 safefs 句柄验证并流式复制到私有 Artifact Store。
- 只有 Commit 成功后，首次 v2 snapshot 才写 opaque Ref；旧文件和旧记录永不自动删除、移动或覆盖。
- 导入失败时保留原记录供恢复，返回 `Available=false` 的安全占位和诊断；legacyPath 不进入模型、TUI、memory 或新 JSONL。

### C11：本地提交门禁的启用方式

- 新增受版本控制的 `.githooks/pre-commit`，内容只调用 `go run ./cmd/xagent-repo-check --source=index`，不复制审计规则。
- README 提供显式安装命令 `git config core.hooksPath .githooks` 与撤销命令；程序不得静默修改用户 Git 配置。
- CI 和发布 profile 无条件直接调用同一 Go 审计入口，因此本地 hook 未安装也不能绕过远端门禁。

### C12：治理前性能基线与可复现比较

- AC36 的治理前基线固定为 revision `607b3dd3cb9d5f01bcba9a047b816187b4512b4a`；不得使用实施后的代码、首次 M5 运行结果或人工填写数值反向充当基线。
- CI performance job 必须以 `fetch-depth: 0` checkout，使固定历史对象本地可用。`internal/e2e/performance.go` 是唯一 coordinator；它先用完整 SHA 和 commit object 类型校验 baseline，缺失、类型错误或解析到其他对象时直接失败，不得联网 fetch、回退当前代码或生成临时基线。
- coordinator 通过 `git archive --format=tar <revision>` 的 stdout 在测试临时目录安全解包，拒绝绝对路径、`..` 和逃逸链接；不得 checkout、reset、创建 worktree、修改当前工作区或接触 M0 保护的本地数据。
- coordinator 把共享的 `internal/e2e/perfprotocol/protocol.go`、`internal/e2e/perfworkload/workload.go` 复制到临时 baseline 中的相同 import 路径，并把固定的 `internal/e2e/testdata/performance/adapter_607b_test.go` 精确复制为临时源树的 `cmd/xagent/e2e_performance_adapter_607b_test.go`。这样 baseline adapter 与旧入口处于同一个 `package main`，只能桥接该 revision 的未导出构造 API；current adapter 位于 `cmd/xagent/e2e_performance_adapter_test.go` 并调用 C13 的生产 Assembly。
- 两个 `cmd/xagent` 测试 adapter 使用同一个 Go toolchain 编译。依赖由 CI 预取到同一只读 module cache，adapter 构建时固定 `GOPROXY=off`，缺依赖直接失败而不访问公网。adapter 只能实现共享 `perfworkload.Driver` 的版本映射，不能各自定义 workload、fixture、完成条件、规范摘要或减少生产组件。

```go
package perfprotocol

const Version = 1

type Workload string
type Phase string

const (
	StartupReady         Workload = "startup_ready"
	OrdinaryConversation Workload = "ordinary_conversation"
	SessionList          Workload = "session_list"
	ReadOnlyTool         Workload = "read_only_tool"
	PhaseReady           Phase    = "ready"
	PhaseRun             Phase    = "run"
)

type Ready struct {
	Version       int         `json:"version"`
	ID            uint64      `json:"id"`
	Workload      Workload    `json:"workload"`
	Phase         Phase       `json:"phase"`
	Revision      string      `json:"revision"`
	Observation   Observation `json:"observation"`
	StateDigest   string      `json:"state_digest"`
	SafeErrorCode string `json:"safe_error_code,omitempty"`
}

type Bootstrap struct {
	Version     int      `json:"version"`
	ID          uint64   `json:"id"`
	Workload    Workload `json:"workload"`
	FixtureRoot string   `json:"fixture_root"`
}

type Begin struct {
	Version  int      `json:"version"`
	ID       uint64   `json:"id"`
	Workload Workload `json:"workload"`
	Phase    Phase    `json:"phase"`
}

type Command struct {
	Version int    `json:"version"`
	ID      uint64 `json:"id"`
	Action  string `json:"action"` // build 或 run
}

type Observation struct {
	Ready           bool     `json:"ready"`
	Screen          string   `json:"screen,omitempty"`
	MessageCount    int      `json:"message_count,omitempty"`
	AssistantSHA256 string   `json:"assistant_sha256,omitempty"`
	SessionIDs      []string `json:"session_ids,omitempty"`
	ReadBytes       int64    `json:"read_bytes,omitempty"`
	ReadSHA256      string   `json:"read_sha256,omitempty"`
}

type Result struct {
	Version       int         `json:"version"`
	ID            uint64      `json:"id"`
	Workload      Workload    `json:"workload"`
	Phase         Phase       `json:"phase"`
	Observation   Observation `json:"observation"`
	StateDigest   string      `json:"state_digest"`
	SafeErrorCode string      `json:"safe_error_code,omitempty"`
}
```

```go
package perfworkload

type Manifest struct {
	Version                  int
	ReadyScreen              string
	Prompt                   string
	ConversationMessageCount int
	AssistantSHA256          string
	OrderedSessionIDs        []string
	ReadRelativePath         string
	ReadBytes                int64
	ReadSHA256               string
}

type MessageView struct {
	Role    string
	Content string
}

type SessionView struct {
	ID string
}

type ReadView struct {
	Bytes   int64
	Content []byte
}

type Driver interface {
	Ready(ctx context.Context) (screen string, err error)
	Submit(ctx context.Context, prompt string) error
	WaitIdle(ctx context.Context) error
	Messages(ctx context.Context) ([]MessageView, error)
	OpenSessions(ctx context.Context) error
	Sessions(ctx context.Context) ([]SessionView, error)
	Read(ctx context.Context, relative string) (ReadView, error)
	Close(ctx context.Context) error
}

type BuildDriver func(context.Context) (Driver, error)

func LoadManifest(fixtureRoot string) (Manifest, error)
func BuildReady(ctx context.Context, workload perfprotocol.Workload, manifest Manifest, build BuildDriver) (perfprotocol.Observation, string, Driver, error)
func Run(ctx context.Context, workload perfprotocol.Workload, phase perfprotocol.Phase, manifest Manifest, driver Driver) (perfprotocol.Observation, error)
func ValidateAndDigest(workload perfprotocol.Workload, phase perfprotocol.Phase, manifest Manifest, observation perfprotocol.Observation) (string, error)
```

- `perfworkload` 固定 manifest、规范化视图、`Driver` 窄接口、四个用户操作脚本、phase 后置条件校验和 `StateDigest` 编码；coordinator 与两个 adapter 都调用同一份代码。adapter 只能把各 revision 的真实 App/Executor 结果映射为 `MessageView`、`SessionView` 和 `ReadView`，不能直接指定 Observation 或摘要。`PhaseReady` 只允许 ready/screen 字段，`PhaseRun` 按 workload 要求 conversation/session/read 字段且拒绝 `StartupReady`；摘要编码必须纳入 workload 与 phase。coordinator 对收到的 Observation 再独立执行 `ValidateAndDigest(workload, phase, ...)`，并要求结果与 manifest、外部 Provider 观测和 adapter 摘要一致。
- 四个脚本固定为：正常组装到 App ready；提交 manifest 中的单轮提示、等待 idle 并观察固定 assistant 回复；打开会话列表并观察 manifest 中的完整有序 ID；通过生产只读 Executor 读取 manifest 指定文件并观察字节数与 SHA-256。普通对话的 Provider 是 coordinator 管理的同一本机 loopback 服务；baseline/current 都必须通过各 revision 的生产 Provider adapter 连接它，coordinator 独立断言请求次数和提示摘要。不得以接口 fake 替换任一版本的 Provider；其他三个 workload 同样使用真实 Store、Permission 和 Executor。
- 每个样本使用一个新进程和一个在启动前完成的全新隔离 fixture。coordinator 启动 adapter 后立即通过 stdin 发送含 fixture/workload 的 `Bootstrap`；adapter 必须先读取并校验 Bootstrap、加载 manifest、准备纯 Options，但此时不得构造任何生产 owner。准备完成后 adapter 发送同 ID、同 workload、`phase=ready` 的 `Begin` 并等待；只有收到唯一 `build` Command 后，才能把“调用该 revision 的唯一生产组装入口并返回仍存活 Driver”作为 closure 交给共享 `BuildReady`。共享函数调用 closure，再执行 `PhaseReady` 脚本并形成 Observation/摘要，adapter 随后发送 Ready。
- baseline 从旧 `defaultStartupFactories` 开始，只把 getwd、userHomeDir、lookupEnv 和 runTUI 四个环境接点绑定到隔离 fixture；newStore、newProvider、newRegistry、newMCP 及其余 owner 构造器保持生产默认。runTUI bridge 只暂停 callback 并暴露存活 App Driver，直到 Driver.Close 后才继续旧入口的正常关闭。`StartupReady` 收到 Ready 后立即 Close/退出，不再接收 `run` 或发送 Result；其余 workload 在 Ready 后发送新的 `phase=run` Begin，接收唯一 `run` Command，执行共享 `PhaseRun` 脚本、发送唯一 Result，再 Close/退出。
- adapter 的 stdout 只传一行一个对象的 NDJSON，诊断只能以安全 code 写 stderr。coordinator 用 Begin/Ready/Result 握手作为完成 gate，不用 `sleep` 推断完成；每个 phase 必须严格是 `Begin → 同 action Command → Ready/Result`，协议版本、ID、workload、phase、错误、外部后置条件或 digest 不一致立即失败，不进入性能比较。
- coordinator 是唯一计时者并使用单调时钟：启动从写入完整 `build` Command 前计到完整、校验通过的 Ready，因 adapter 在 Begin 后不得构造 owner，该区间覆盖生产组装入口到 App ready，同时排除 `go test` harness、测试二进制装载和 Bootstrap/fixture 准备；其他 workload 从写入完整 `run` Command 前计到完整、校验通过的 Result。协议不接受任何 adapter 自报持续时间；固定的 Command/NDJSON 开销在两个 revision 中完全相同。
- 每个 revision/workload 按预先确定的顺序恰好运行 5 个不计分预热样本和 30 个计分样本；单个 Ready 或 Result 的 watchdog 为 30 秒。任何预热或计分尝试发生超时、协议/后置条件错误、进程异常或非正持续时间时立即令整个性能门禁失败，不得重试、替换、剔除或只保留较快样本。baseline/current 按样本交错且轮换先后顺序，在同一原生 runner、同一 Go toolchain 上执行。
- 每项分别比较 30 个有效样本的中位数。每组时长按无符号纳秒值非降序排序为 `s[0]..s[29]`，中位数固定为 `s[14] + (s[15]-s[14])/2`，即中间两项算术平均值向下取整到整纳秒；不得改取下中位数、上中位数或浮点平均值。通过门禁按精确有理数比较 `CurrentMedianNanos*5 <= BaselineMedianNanos*6`，使用不溢出的双字乘法比较乘积，不能用浮点或取整后的展示比值决定通过。证据记录 baseline/current revision、OS/arch、Go 版本、样本数、两个中位数和仅供展示的 ppm 比值，但不得记录 fixture 真实路径、凭据或原始会话内容。

### C13：E2E 复用唯一生产组装根

- E2E 的场景测试放在 `cmd/xagent/e2e_*_test.go`，使用 `package main` 在测试进程内直接调用 T4 建立的唯一 `Assembly.Build`；不得从 `internal/e2e` 重建 Store、Executor、Provider、MCP、App 或 TUI 依赖图，也不得恢复 T4.29a 删除的旧组装入口。
- `internal/e2e` 的 `harness.go`、`assertions.go` 与 `performance.go` 是带 `e2e` build tag 的可导入非测试文件，只提供 hermetic fixture、外部 workload 驱动和断言辅助；`*_test.go` 场景留在 `cmd/xagent`。生产构建不导入该包。

```go
package main

type RuntimePaths struct {
	ProjectRoot    string
	UserConfigRoot string
	UserDataRoot   string
	UserCacheRoot  string
}

type ConfigInputs struct {
	UserPath    string
	ProjectPath string
	Runtime     config.PartialAppConfig
}

type AssemblyOptions struct {
	Paths        RuntimePaths
	Config       ConfigInputs
	LookupEnv    func(string) (string, bool)
	TrustedRoots *x509.CertPool
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
}

type Assembly struct {
	// factories 不导出；defaultAssembly 填入唯一生产构造器集合。
	factories assemblyFactories
}

func defaultAssembly() Assembly
func (a Assembly) Build(ctx context.Context, options AssemblyOptions) (*Runtime, error)
```

- 正常 `RunCLI` 在 help/version 前置返回后解析 OS 目录并显式填满 `AssemblyOptions`，再调用 `defaultAssembly().Build`。`Build` 拒绝缺失或相对 Root，以及 nil `LookupEnv`/IO；它在内部执行真实的配置 decode/merge/resolve、秘密注册、owner 构造和 registry 登记。
- Assembly 不接受调用方提供的 `http.Client`、RoundTripper、Proxy、Dial/DialTLS、CheckRedirect、CookieJar 或协议 handler。`TrustedRoots` 是唯一网络测试输入：Build 只把它传入 C4 `ClientOptions`，由 netpolicy Factory 深拷贝并用于标准 TLS 校验，同时自行创建并完全控制 transport、拨号、代理、redirect 和 header 策略；生产 `RunCLI` 固定传 nil 使用系统信任根。恶意自定义 client/transport 没有注入入口，证书池后续变更也不能影响已创建 Client。
- `assemblyFactories` 仅用于 T4.28 的同包初始化回滚单元测试逐点制造构造失败；所有字段均由 `defaultAssembly` 设为生产构造器，不能从 CLI、配置或其他包设置。E2E 和性能 current adapter 必须调用 `defaultAssembly`，不得覆盖 factories；它们只可用临时绝对 Roots、显式配置层、固定环境查找、可选测试 CA 信任根和受 netpolicy 控制的本机 HTTP/TLS fixture。
- `Runtime` 是 Build 成功后唯一发布对象，持有真实 App/TUI 控制器与同一 ownership/close registry；同包 E2E 通过既有 App intent、事件和 ViewModel 驱动用户路径，并最终调用 Runtime 的正常关闭入口。不得为了测试导出领域 owner、增加隐藏 E2E CLI 模式或复制关闭顺序。
- 场景必须使用真实 Conversation Store、Permission、Executor、safefs、artifact、netpolicy、Orchestrator、App 状态机及关闭 registry。Provider/MCP 客户端也使用生产 adapter，只把其外部服务配置到确定性的本机 TLS/loopback fixture；进程树、文件保护和平台路径场景必须使用当前平台真实实现，不能用 fake 代替。
- 用户路径通过真实 App intent、事件和 ViewModel 驱动；CLI help/version 通过真实 `RunCLI` 驱动。E2E 不要求引入跨平台 PTY，也不能因无 PTY 而绕过导航、保存、关闭或安全边界。
- 五组 AC38 场景均以 `e2e` tag 独立命名并可单独运行；macOS、Linux 和 Windows 三个原生 runner 都必须在测试开始确认运行时为 amd64，macOS 不能在 arm64 runner 仅设置交叉编译目标后冒充原生结果。能力不足时只接受“目标未启动”的 fail-closed 证据，不得 skip、降级为 fake 或用交叉编译替代原生运行。

### C14：既有公开数值配置的明确边界（已批准）

下表补齐 C8 之外仍公开、但前文尚未给出完整数值契约的键。数值单位与键名一致；所有键都由唯一 Resolve 阶段产生确定值。

| 配置键 | 默认值 | 有效最小值 | 不可提高的硬上限 | 组合规则 |
|---|---:|---:|---:|---|
| `llm.request_timeout_ms` | 120000 | 1 | 86400000 | 触发后仍按 C7 在 2 秒内回收 |
| `llm.thinking.budget_tokens` | 4096 | 1 | 1000000 | thinking 关闭时也解析验证，但不消费 |
| `agent.max_iterations` | 10 | 1 | 1000 | 每次请求累计，不按工具调用重置 |
| `agent.max_unknown_tool_calls` | 2 | 1 | 100 | 每次请求累计 |
| `tool.timeout_ms` | 30000 | 1 | 86400000 | 触发后仍按 C7 在 2 秒内回收 |
| `context.tool_result_threshold_chars` | 32768 | 1 | 1048576 | 不得高于多结果阈值 |
| `context.tool_results_threshold_chars` | 65536 | 1 | 4194304 | 不得低于单结果阈值 |
| `context.model_window_tokens` | 200000 | 1 | 10000000 | 必须分别大于 auto/manual margin |
| `context.auto_margin_tokens` | 13000 | 1 | 1000000 | 严格小于 model window |
| `context.manual_margin_tokens` | 3000 | 1 | 1000000 | 严格小于 model window |
| `context.recent_keep_tokens` | 10000 | 1 | 1000000 | 严格小于 model window |
| `context.recent_keep_messages` | 5 | 1 | 10000 | 与 token 上限同时生效 |
| `context.summary_failure_limit` | 3 | 1 | 100 | 每次压缩流程累计 |
| `context.preview_chars` | 2000 | 1 | 1048576 | 只生成安全预览 |
| `mcp.default_timeout_ms` | 30000 | 1 | 86400000 | server 未设置时继承 |
| `mcp.servers.<name>.timeout_ms` | 继承 30000 | 1 | 86400000 | 未设置与显式 0 必须区分 |
| `session.retention_days` | 30 | 1 | 3650 | 仍受扫描文件/字节上限约束 |
| `session.gap_reminder_days` | 7 | 1 | 3650 | 只控制恢复提醒 |
| `memory.max_index_lines` | 200 | 1 | 100000 | 行数与字节数同时生效 |
| `memory.max_index_bytes` | 25600 | 1 | 16777216 | 行数与字节数同时生效 |
| `memory.update_queue_size` | 8 | 1 | 1024 | 不得低于 update concurrency |
| `memory.update_concurrency` | 1 | 1 | 64 | 不得高于 queue size |
| `memory.update_timeout_ms` | 30000 | 1 | 86400000 | 超时后不得遗留 Provider 流 |
| `memory.max_candidate_bytes` | 65536 | 1 | 16777216 | 在分配/发送前消费 |
| `lifecycle.cleanup_timeout_ms` | 2000 | 1 | 2000 | 只能降低，不能关闭或提高 |

Hook rule 的 `timeout` 使用 duration 字符串而非毫秒整数：command 默认 30 秒、HTTP 默认 10 秒、subagent 默认 30 秒，三者有效最小值均为 1 毫秒、硬上限均为 10 分钟；prompt action 禁止配置 timeout。目标运行超时与 C7 的关闭等待是两个独立边界。

- 表中“未设置”与“显式 0”由 `Optional` presence 区分：未设置应用默认或继承值；显式 0、负数、超过硬上限、整数溢出及违反组合规则都在启动阶段失败，不能用于关闭安全限额，也不能静默截断。
- 超过新硬上限的旧配置采用非破坏迁移：原文件保持不变，启动错误只报告字段路径、允许范围和降低该值的迁移说明；不自动改写配置，不回显其他配置值。
- 本表与前文“初始默认预算与硬上限”共同构成完整数值契约，并同时进入 C8 schema↔example、Resolve 的 default/min/cap/cap+1/zero/negative/overflow 表驱动测试及 Config→owner 单一映射测试。任何后续调整都必须修改 Plan 并重新审批，不能只改常量或示例。
- 文件组织的 closed-world 约束针对生产、接口和交付路径：未列出的现有生产文件保持不动。为验证已批准行为，可在对应包补充或调整 `_test.go` 与 `testdata`，但不得借测试扩大生产能力、增加第二入口或触碰候选外数据。

### C15：公开数值输入闭集与验证所有权（已批准，2026-07-31）

C14 复审后发现三类契约必须分开：`config.yaml` 的资源旋钮由 Config Resolve 负责；`hooks.yaml` 由 Hook Loader/compile 负责；Skill frontmatter 与 permission rule 文件由各自严格解析器负责。C15 只封闭既有公开输入，不新增业务能力，并按以下条款纠正 C14 中“全部由 ResolveConfig 验证”和“数值契约已经完整”的概括；未被本节改写的 C14 条款继续有效。

C14 开头的“C8 之外”按“C8 所列及 C8 之外尚未完整定义的既有键”理解；C8/C14/C15 三表的并集才是公开数值闭集。`config.example.yaml` 的 schema 双向检查只覆盖 AppConfig；Hook、Skill 与 permission 分别由 Hook 示例、README 中的 Skill frontmatter 示例和 permission 兼容 fixture 覆盖，不能把它们伪装成 Config schema 字段。

#### C8 初始预算的最小值与显式 0

下表逐行补全并取代前文“初始默认预算与硬上限”中对应行的数值契约。除表内明确例外，未设置才应用默认；显式 0、负数、cap+1、类型溢出和违反组合规则均在任何网络、进程、会话写入或配置改写前失败。所有字节值使用二进制 KiB/MiB/GiB。

| 公开配置键 | 默认值 | 有效最小值 | 不可提高的硬上限 |
|---|---:|---:|---:|
| `tool.inline_output_bytes`（旧 `tool.max_output_bytes` 的唯一迁移目标） | 32 KiB | 1 B | 1 MiB |
| `tool.capture_bytes` | 64 MiB | 1 B | 512 MiB |
| `tool.timeout_ms` | 30000 ms | 1 ms | 86400000 ms |
| `artifact.max_file_bytes` | 64 MiB | 1 B | 512 MiB |
| `artifact.max_total_bytes` | 1 GiB | 1 B | 8 GiB |
| `artifact.retention_days` | 7 | 1 | 365 |
| `files.read_max_bytes` | 16 MiB | 1 B | 256 MiB |
| `files.scan_max_bytes` | 256 MiB | 1 B | 2 GiB |
| `files.scan_max_files` | 100000 | 1 | 1000000 |
| `files.scan_max_directories` | 25000 | 1 | 250000 |
| `files.scan_max_lines` | 1000000 | 1 | 10000000 |
| `instructions.max_file_bytes` | 64 KiB | 1 B | 1 MiB |
| `instructions.max_total_bytes` | 1 MiB | 1 B | 16 MiB |
| `instructions.max_files` | 64 | 1 | 1024 |
| `instructions.max_expanded_bytes` | 2 MiB | 1 B | 32 MiB |
| `instructions.max_include_depth` | 5 | 1 | 32 |
| `mcp.max_response_bytes` | 1 MiB | 1 B | 16 MiB |
| `mcp.max_tools` | 128 | 1 | 1024 |
| `mcp.max_pages` | 32 | 1 | 128 |
| `mcp.max_protocol_errors` | 32 | 1 | 256 |
| `llm.stream.max_response_bytes` | 16 MiB | 1 B | 64 MiB |
| `llm.stream.max_event_bytes` | 1 MiB | 1 B | 4 MiB |
| `llm.stream.max_events` | 100000 | 1 | 1000000 |
| `llm.stream.max_text_bytes` | 8 MiB | 1 B | 32 MiB |
| `llm.stream.max_thinking_bytes` | 8 MiB | 1 B | 32 MiB |
| `llm.stream.max_tool_arguments_bytes` | 1 MiB | 1 B | 8 MiB |
| `session.max_record_bytes` | 16 MiB | 1 B | 64 MiB |
| `session.max_session_bytes` | 256 MiB | 1 B | 1 GiB |
| `session.max_scan_files` | 1000 | 1 | 100000 |
| `session.max_scan_bytes` | 10 MiB | 1 B | 1 GiB |
| `session.retention_days` | 30 | 1 | 3650 |
| `session.gap_reminder_days` | 7 | 1 | 3650 |
| `diagnostics.max_items` | 100 | 1 | 1000 |
| `diagnostics.max_item_bytes` | 2 KiB | 1 B | 64 KiB |
| `diagnostics.max_total_bytes` | 2 MiB | 1 B | 16 MiB |
| `lifecycle.cleanup_timeout_ms` | 2000 ms | 1 ms | 2000 ms |

Resolve 还必须拒绝下列不自洽组合：

- `tool.inline_output_bytes <= tool.capture_bytes <= artifact.max_file_bytes <= artifact.max_total_bytes`；
- `instructions.max_file_bytes <= instructions.max_total_bytes` 且 `instructions.max_file_bytes <= instructions.max_expanded_bytes`；
- `llm.stream.max_event_bytes`、`max_text_bytes`、`max_thinking_bytes`、`max_tool_arguments_bytes` 分别不得高于 `max_response_bytes`；
- `session.max_record_bytes <= session.max_session_bytes`；
- `diagnostics.max_item_bytes <= diagnostics.max_total_bytes`。

旧 `tool.max_output_bytes` 没有第二套默认值或上限：单独出现时按同一 presence/value 映射到 `tool.inline_output_bytes`，与新键同时出现一律报冲突，原配置文件保持不变。

#### C14 勘误与跨文件数值输入

- `context.model_window_tokens` 的独立有效最小值从 1 更正为 2；因为 auto/manual margin 与 recent keep 的最小值均为 1 且必须分别严格小于窗口。默认值 200000、硬上限 10000000 不变；min fixture 必须与三个值均为 1 的联合配置一起验证。
- `mcp.servers.<name>.timeout_ms` 未设置时继承已 Resolve 的 `mcp.default_timeout_ms`，不是无条件继承字面量 30000；显式 0 仍非法。
- `hooks.yaml` 的 `version` 必填且只能为整数 1；缺失、0、负数、2、整数溢出或错误类型均由 Hook Loader 在 compile/副作用前拒绝。
- Hook rule `timeout` 不进入 `PartialAppConfig` 或 `ResolveConfig`。Hook Loader 保留 presence，compile 按 action 独立应用 command=30 秒、HTTP=10 秒、subagent=30 秒的默认值；显式 1 毫秒和 10 分钟接受，999 微秒、10 分钟加 1 纳秒、0、负数、`time.ParseDuration` 溢出及 prompt action 携带 timeout 均拒绝。
- permission RuleFile 的 `version` 由 permission loader 负责：字段缺失或显式 0 仅作为既有 legacy v1 非破坏读取，整数 1 为当前格式；负数、大于 1、整数溢出和错误类型都把该层标为损坏并触发既有危险操作 fail-closed，不能静默当作 v1，也不能改写原文件。

Hook predicate 中作为比较值的数字、Conversation/事件持久化版本与计数、测试 fixture 的数值以及内部不可配置的 `Limits` 不属于公开资源旋钮；它们仍分别受严格 schema、文件/字段字节预算和版本校验约束。`repoaudit.max_binary_bytes` 已由 T0.2 固定为恰好 1048576，不接受另一个默认或覆盖入口。

#### Skill `history` 的有界语义

Skill frontmatter 的既有 `history` 是公开数值输入，表示 isolated Skill 可携带的最近完整 turn 数：未设置默认 0，最小值 0，硬上限 1000。shared Skill 只能为 0；isolated Skill 接受 0–1000；负数、1001、YAML 整数溢出和错误类型由 `skill.ParseWithLimits`/`ValidateMetadata` 拒绝，错误不回显原标量。显式 0 是有效的“无历史”，是本节相对正数资源预算零值规则的明确例外。

```go
package contextmgr

type RequestMeasure struct {
	Bytes          int64
	PlanningTokens int64
}

type RequestBudgeter struct {
	// 计量版本与有效标记均不导出；只能由构造器产生。
}

func NewRequestBudgeter() RequestBudgeter
func (b RequestBudgeter) MeasureRequest(ctx context.Context, request provider.ChatRequest) (RequestMeasure, error)
func (b RequestBudgeter) MeasureConversationTurn(ctx context.Context, messages []conversation.Message) (RequestMeasure, error)

package orchestrator

type SkillHistoryPolicy struct {
	// 数值与有效标记均不导出，只能由 NewSkillHistoryPolicy 产生。
}

func NewSkillHistoryPolicy(
	maxSessionBytes int64,
	modelWindowTokens int64,
	autoMarginTokens int64,
) (SkillHistoryPolicy, error)

type HistoryLimitReason uint8

const (
	HistoryLimitNone HistoryLimitReason = iota
	HistoryLimitTurns
	HistoryLimitBytes
	HistoryLimitPlanningTokens
)

type SkillHistorySelection struct {
	Messages  []provider.ModelMessage
	Turns     int
	Truncated bool
	Reason    HistoryLimitReason
}

func (o *Orchestrator) selectSkillHistory(
	ctx context.Context,
	main *conversation.Conversation,
	requested int,
	base provider.ChatRequest,
) (SkillHistorySelection, error)
```

- `NewSkillHistoryPolicy` 是唯一构造入口，数值字段与 validity marker 均不导出；它在构造器内部固定 `maxTurns=1000`，并独立验证 `maxSessionBytes` 为 1 B–1 GiB、`modelWindowTokens` 为 2–10000000、`autoMarginTokens` 为 1–1000000 且严格小于窗口。唯一 Assembly 只把 Resolve 后的三个值交给构造器并向 Orchestrator 注入所得 value；selector 二次拒绝零值/无 marker policy，AST 测试禁止 Orchestrator 包内出现构造器外的 struct literal。以上上限属于构造器不可覆盖的常量，不接受 options、第二构造器或调用方提供的 cap，任何调用点都不能提高硬上限。
- Context Manager 的 `RequestBudgeter` 是 Skill 历史请求组装唯一的 canonical 计量实现。它是带私有计量版本与 validity marker 的 concrete value，只能由无参数 `NewRequestBudgeter` 构造；Assembly 只注入该 value，Orchestrator 二次拒绝零值/无 marker value。不提供可替换 interface、函数型 counter、options、第二构造器或可变注册入口，AST/组装测试禁止旁路实现。计量不读取 protocol 或 model，不改变既有任意非空默认 model、自定义 OpenAI-compatible model 或 Skill model override 的合法性。
- `MeasureRequest` 覆盖规范化后的完整请求：stable/dynamic system blocks、Skill SOP、model、当前输入及已有/选中历史、thinking 设置、完整 tool schema、tool call/result、cache/framing 与其他所有请求字段；`MeasureConversationTurn` 只读取候选完整 turn 最终允许发送的安全投影（安全文本、工具调用/结果元数据、opaque artifact Ref 与 framing），不得读取或复制 artifact 原文、诊断、持久化内部字段，并被定义为可安全相加的上界分量。实现使用 overflow-safe 累加且只读输入，不生成与整个输入大小成比例的编码副本。两方法都在同一 immutable Budgeter 上执行，必须在开始前、每个字段/块之间，以及长字段内部每处理至多 64 KiB 原始输入或嵌套 schema 每遍历至多 1024 个节点时检查 `ctx`；取消返回 `ctx.Err()` 且不发布部分计量，因此即使合法输入接近 1 GiB，也不能把取消延迟到整个输入计量完毕。
- `RequestMeasure.Bytes` 是对该规范化请求被保留、复制或 JSON 序列化的 payload bytes 的保守上界，不是 rune 数。计量器逐字段流式遍历：每个原始 string byte 按 JSON 最坏 6-byte 转义计入，结构键、分隔符、枚举、布尔、数字、slice/map/schema 及 request/message/tool/framing 的固定开销按版本化 canonical encoding 计入；overflow、无法规范化的 schema/值或不受支持的字段形态 fail closed。`MeasureConversationTurn` 还计入把该 turn 插入现有 request 所需的最坏分隔/framing，但不重复计整个 base。
- `RequestMeasure.PlanningTokens` 明确定义为 `ceil(Bytes/4)` 的版本化、确定性、本地规划估算，只用于与既有 `context.model_window_tokens`/`auto_margin_tokens` 做一致的截断决策；它不宣称等于或不低估任意远端模型的真实 tokenizer 结果。OpenAI-compatible 请求形状不约束远端 tokenizer，因此 Provider 对实际 context limit 仍是权威，远端超限沿既有 Provider 错误路径返回；C15 不新增 model/tokenizer 白名单、联网预计量或 token 策略调优。
- Bytes/PlanningTokens 规则进入版本化 byte-oracle 与 golden 测试：至少覆盖空/边界请求、ASCII、CJK、emoji、组合字符、无效 UTF-8 的 JSON 规范化、system block、Skill SOP、任意默认/override model 名、tool schema、tool call/result、opaque artifact Ref、cache 与 framing。byte oracle 对 Anthropic/OpenAI adapter 的实际规范化 payload 逐例断言 `measured.Bytes >= serialized payload bytes`；golden 固定相同输入的 deterministic 结果，并覆盖溢出、负值防御、未知字段形态和取消。任何 request 字段或 adapter 规范化变化必须与 canonical encoding、oracle 和 golden 在同一任务更新。
- 完成 policy/requested 二次校验和一次 `ctx` 检查后，`requested=0` 直接返回空的未截断选择，不构造、计量或复量请求，从而不改变默认无历史 Skill 的既有请求路径。仅当 `requested>0` 时，Orchestrator 才先组装不含历史的完整 `base` 请求并用同一 `ctx` 调用 `MeasureRequest`；调用前后都检查 `ctx`。本次可用历史字节数严格等于 `session.max_session_bytes - base.Bytes`，可用历史规划 token 数严格等于 `context.model_window_tokens - context.auto_margin_tokens - base.PlanningTokens`。Budgeter 返回负 `Bytes` 或负 `PlanningTokens`、任一减法溢出、base 已超过字节上限或达到规划 token 边界时，均在 Provider/工具/Store 副作用前安全失败；负值不得被减法解释为扩大预算。
- 选择器持有经构造器验证的 policy，从最新消息向前识别完整 user→assistant turn；tool call/result 链和所属 turn 原子纳入或原子舍弃。它在每个 turn 前检查 `ctx`，再让唯一 Budgeter 使用同一 `ctx` 对主会话中的只读 index range 计量，并在每次返回后再次检查 `ctx`；计量返回负 `Bytes` 或负 `PlanningTokens` 时 fail closed。选择器从 base measure 开始，以 overflow-safe 加法累积每个候选 turn 的 measure；只有新累计值同时满足剩余 turns/bytes/planning tokens 才转换并深拷贝为 C5 的安全 `provider.ModelMessage`。不能调用会复制整段会话的 `ContextMessages`，不能先 clone 全会话再截断，也不能让 `conversation.Message` 进入 Provider 请求。
- `requested` 小于 0 或大于 policy 的 1000 时二次 fail closed。存在更旧完整 turn 且已达到 requested 时使用 `HistoryLimitTurns`；候选 turn 同时超过 byte 与 planning-token 预算时使用 `HistoryLimitBytes`，否则使用实际触发项，形成固定优先级 turns→bytes→planning-tokens。预算截断设置 `Truncated=true` 与该唯一 `Reason`；不得包含半个 turn、未闭合工具链或不完整 assistant 结果。若可用完整历史本来就不多于 requested，则只返回实际完整 turns，`Truncated=false`、`Reason=HistoryLimitNone`。输出保持原顺序，不与主会话 alias，取消返回 context error 且不发布部分选择。
- 选中历史转换并插入后，Orchestrator 必须在发布请求前使用同一 Budgeter 对最终 `ChatRequest` 整体调用 `MeasureRequest`，并在调用前后检查 `ctx`。最终 `Bytes` 和 `PlanningTokens` 必须分别不高于增量累计上界；最终较小（例如共享 framing 只计一次）允许，最终任一值较大视为增量低估并 fail closed。只有最终 `Bytes <= session.max_session_bytes` 且最终 `PlanningTokens <= context.model_window_tokens - context.auto_margin_tokens` 才能继续；负值、溢出、超限或取消同样失败，不能通过删除半个 turn修补，也不能在复量前启动 Provider、工具或 Store 副作用。
- 对治理前已存在的 `history > 1000` Skill 采用非破坏迁移：原 SKILL.md 不删除、不移动、不覆盖、不自动截断；该定义从新 snapshot 中禁用/跳过，已固定到进行中 invocation 的旧 snapshot 不被异步改写。诊断只包含安全来源标识、字段路径 `history`、允许范围 `0..1000` 与“降低到不超过 1000”的迁移提示，不回显原数值或正文；修正后下一次原子 refresh 才重新启用。

C15 明确授权修改 `internal/config/{optional,partial,resolve,validate}.go`、`internal/skill/{types,parser,history,manager}.go`、`internal/permission/{loader,rule}.go`、`internal/hook/{loader,validate}.go`、`internal/contextmgr/manager.go`、`internal/orchestrator/independent.go`、`cmd/xagent/assembly.go` 及各自对应测试。公开示例/文档可以同步数值说明；其他 closed-world 文件约束不变。Task 必须分别把 AppConfig、Hook、Skill、permission 的验证放到各自 owner，不得重新合并成单一 Resolve 测试。

### C16：结果 DTO、权威 revision 与分阶段证据（已批准，2026-07-31）

Task 自包含复审发现，Plan 已给出 Conversation Store 方法名、性能消息结构和 R0/E 原则，但没有封闭四个结果 DTO、NDJSON codec、权威 checkout、删除中间状态与 M0 最终复验。C16 只补齐这些已批准行为的接口和证据协议；不新增业务命令、Provider、模型、发布渠道或自动发布能力。未被本节改写的 C1–C15 条款继续有效。

#### Conversation v2 结果闭集

以下定义补全并取代前文只列名称的 Conversation 结果类型：

```go
package conversation

type RecoveryStatus string

const (
	RecoveryClean       RecoveryStatus = "clean"
	RecoveryPartial     RecoveryStatus = "partial"
	RecoveryPlaceholder RecoveryStatus = "placeholder"
)

type RecoveryReport struct {
	Status            RecoveryStatus
	LastValidRevision uint64
	SkippedRecords    int
	Diagnostics       []diagnostics.Diagnostic
}

type ConversationSummary struct {
	ID           string
	Title        redact.SafeText
	UpdatedAt    time.Time
	MessageCount int
}

type ListEntry struct {
	Summary   ConversationSummary
	Available bool
	Recovery  RecoveryReport
}

type PersistedState struct {
	Revision       uint64
	MessageCount   int
	Digest         StateDigest
	MessagesDigest StateDigest
}

type ListResult struct {
	Entries      []ListEntry
	Truncated    bool
	ScannedFiles int
	ScannedBytes int64
	Diagnostics []diagnostics.Diagnostic
}

type LoadResult struct {
	Conversation *Conversation
	Available    bool
	Persisted    PersistedState
	Recovery     RecoveryReport
}

type SaveKind string

const (
	SaveNoop     SaveKind = "noop"
	SaveBatch    SaveKind = "batch"
	SaveSnapshot SaveKind = "snapshot"
)

type SaveResult struct {
	Kind      SaveKind
	Persisted PersistedState
}

type MaintenanceResult struct {
	ScannedFiles int
	ScannedBytes int64
	Deleted      int
	Skipped      int
	Truncated    bool
	Diagnostics []diagnostics.Diagnostic
}
```

- `RecoveryClean` 要求零 skipped record；`RecoveryPartial` 表示返回了最后一个摘要链有效的可用状态；`RecoveryPlaceholder` 表示单文件不可恢复但仍以安全占位出现在列表中。`ListEntry.Available=false` 只能搭配 placeholder；`LoadResult.Available=false` 时 `Conversation=nil` 且 Recovery 必须为 placeholder。
- List 以 `UpdatedAt` 降序、相同时间按 ID 原始字节升序稳定排列，placeholder 参与同一顺序。达到扫描预算返回已有可信 entries、`Truncated=true` 和安全诊断；只有根目录/整体枚举不可信才返回顶层 error。Load 的单文件可恢复损坏通过 `RecoveryPartial` 返回；Save 只有成功跨过持久化提交点后才发布新的 `PersistedState`，失败返回 error 且不能前移状态。
- Maintenance 的单文件失败累计 `Skipped` 和诊断并继续；根不可读或 context 取消才返回顶层 error。所有 diagnostics 必须已通过 RuntimeRedactor，任何 DTO 均不得包含 raw path、`legacyPath`、普通 string 消息正文或 artifact 原文。

#### 性能 NDJSON codec 与 revision 贯穿

C12 的 `Bootstrap`、`Begin`、`Command`、`Result` 各新增必填 `Revision string`（JSON 键均为 `revision`）；`Ready.Revision` 保持必填。baseline 的所有 frame 必须逐字等于 `607b3dd3cb9d5f01bcba9a047b816187b4512b4a`，current 的所有 frame 必须逐字等于 checker 已验证的当前 commit OID。任一空值、大小写/空白变体、frame 间变化或与外部期望不符都在计时样本被接受前失败。

```go
package perfprotocol

const (
	Version       = 1
	MaxFrameBytes = 1 << 20
)

type Encoder struct { /* 私有 writer 与状态 */ }
type Decoder struct { /* 私有 buffered reader 与状态 */ }

func NewEncoder(w io.Writer) (*Encoder, error)
func (e *Encoder) Encode(value any) error
func NewDecoder(r io.Reader) (*Decoder, error)
func (d *Decoder) Decode(value any) error
```

- codec 只接受非 nil borrowed I/O，永不关闭底层 pipe；Encode/Decode 的动态类型闭集是 `Bootstrap`、`Begin`、`Command`、`Ready`、`Result` 的值或非 nil 指针，其他类型拒绝。每个 frame 是恰好一个严格 JSON object 加一个 `\n`；Decoder 拒绝未知字段、重复 JSON key、重复顶层对象、空行、尾随非空白、超过 1 MiB 和没有换行的半帧，并在分配前以 cap+1 有界读取。干净 EOF 且没有待处理字节返回 `io.EOF`，中途 EOF 返回 `io.ErrUnexpectedEOF`。
- Encoder 先在 1 MiB＋1 byte 的有界 buffer 中完成严格编码并复核结果为单一 object/无重复 key，超限或非法时不写任何字节；成功时执行完整 frame＋换行写入，short write/写失败返回 error。两端错误只发布 safe code，不把 frame、fixture path、prompt 或响应正文写到 stderr。
- coordinator、baseline adapter 与 current adapter 只能使用该 codec；不得各自调用 Scanner/Encoder 建第二套帧规则。Task 必须原样列出 C12 的 Version、四个 Workload、两个 Phase、五个顶层 frame DTO 与嵌套 Observation、Manifest/View/Driver/BuildDriver 及四个函数签名，不能再用“按 C12”代替接口。

#### Checker 的 index 与权威 commit 模式

`xagent-check` 的所有 profile 都必须显式接受且只接受下列来源闭集：

```text
--audit-source=index                 # 本地开发/提交前，禁止 --revision
--audit-source=commit --revision=OID # CI、release、Rpre/D01–D17/R0/E 权威证据
```

- index 模式只审计显式 index，不扫描用户 worktree，不产生可写入 checklist 的权威通过结论。commit 模式要求 OID 为 40 位小写 canonical commit，逐字等于净化环境下解析的 `HEAD^{commit}`；profile、每个 child Step、commit repoaudit、performance coordinator/adapter/report 和最终 evidence record 必须携带同一值，不能从 branch/tag/workflow 文本、缓存或继承环境重新推断。
- 权威 commit 只能在新鲜隔离 checkout 中运行。profile 开始前、每个 child Step 紧邻执行前与返回后、以及 profile 结束时，都必须证明 index 与 HEAD 相同、tracked worktree 无差异、非忽略 untracked 与 ignored untracked 均为零；检查只输出计数/安全 code，不输出路径。任一 Step 产生任何额外或变化文件即使自身 exit 0 也使该 Step 与整个 profile 失败，并在下一 Step 前停止；coverage、测试二进制、module/performance cache 和证据中间件只能写工作区外的私有 `XAGENT_TASK_TMP`。ignored `.go` 或 embed 输入同样可能污染构建，因此不能只做一次启动检查。保留 M0 本地数据的普通工作区只能运行 index 模式，不能冒充权威证据。
- Git 子进程使用最小环境并显式禁用 replace objects；拒绝继承 `GIT_DIR`、`GIT_WORK_TREE`、`GIT_INDEX_FILE`、`GIT_OBJECT_DIRECTORY`、`GIT_ALTERNATE_OBJECT_DIRECTORIES`、`GIT_COMMON_DIR`、`GIT_NAMESPACE`、`GIT_REPLACE_REF_BASE` 及 `GIT_CONFIG*` 重定向，固定 `GIT_NO_REPLACE_OBJECTS=1` 并使用 `git --no-replace-objects` 或等价库调用。解析出的 Git object directory 不得存在非空 `info/alternates`，replace-ref namespace 即使存在也不得生效；C12 archive、HEAD 校验、repoaudit 和所有 profile 共用这一实现。
- commit repoaudit 的唯一 argv 语义为 `xagent-repo-check --source=commit --revision=<同一 OID>`；index hook 的唯一语义仍为 `--source=index`。任何 profile 不得隐式选择来源。权威证据记录 exact argv、checkout OID、cleanliness、OS/arch、Go 版本、required manifest 和 exit code，不能拼接不同 revision 或不同 checkout 的结果。

#### M0 preservation manifest 的唯一变更通道

repoaudit 的普通 index/commit/worktree 审计继续严格只读。preservation mode 是调用点与语义均封闭的四值枚举：`freeze`、`remove-index`、`verify-immediate` 只能由 T0.10 顺序调用，`verify-final` 只能由 T5.54 调用；其他 Task、hook、CI 和 profile 一律拒绝。它没有任意路径参数，只接受工作区外且父目录私有的绝对 manifest。

- 35 条目标由唯一选集规则产生：取 index 中 stage=0 且 raw path 匹配 `.claude/worktrees/**`、`.mewcode/memory/**`、`.mewcode/sessions/**`、精确 `fakeprovider` 或后缀 `.webarchive` 的**全部**条目；不接受“其他附件”猜测、子集选择或第二份名单。按 raw path bytes 升序，对每条串接 `mode SP oid SP stage TAB path NUL` 后计算 SHA-256；首次批准 count=35，digest=`0c44cba3839534b8ddd6d8153dc1def2f3f69ac0ea8acf745934ebd4064365f0`。count、stage、序列化或 digest 不同必须在任何 index mutation 前停止并重新审批。
- freeze 同时记录整个 pre-change index 身份、35 条目标身份、`.gitmodules` 不存在 sentinel，以及不跟随链接的工作区 lstat/type/mode/content 或 link-target/目录摘要。它通过已打开的私有父目录，以固定同目录 temp、regular mode 0600、create-exclusive 完整写入并从同一 handle 复验，随后 fsync file、no-replace atomic publish 为唯一 final manifest、fsync directory；只有 final publish 完成后才安全输出 count、目标集合 digest 与完整 manifest SHA-256，不输出路径。freeze 崩溃恢复只在重新证明完整 index 仍逐 byte 等于记录的 pre-change identity、35 条仍在 index、工作区指纹及 `.gitmodules` sentinel 均未变化且 remove-index 从未发生时，才可续发等于期望内容的完整 temp或精确清理截断/非法 temp后重建；已发布 final 永不覆盖，final 不同或无法证明仍处 pre-change 状态时 fail closed。
- remove-index 与两个 verify mode 都必须接收 freeze 输出的 `--expect-manifest-sha256`，经同一已打开父目录句柄 no-follow 打开既有文件、`fstat` 验证 regular mode 0600、重算全部 bytes 并逐字节匹配；它们不创建或改写 manifest，也不声称不可跨进程证明的 inode identity。remove-index 随后重新验证目标 identity、完整 index 无漂移与工作区指纹，再由进程内唯一固定 argv `git update-index --force-remove -z --stdin` 消费从该 manifest 解出的 NUL 路径；禁止 shell、glob、文本分隔、第二次目录发现或任何 worktree 删除。执行后立即证明新 index 恰等于 frozen index 减去 35 条目标。
- `verify-immediate` 紧接 mutation，证明 35 条已退出 index、其工作区对象完全未变、其他 index 条目未变且仍未创建 `.gitmodules`。`verify-final` 允许后续获批实现及用户在非目标范围的合法 index 变化，只复验：manifest content digest/regular-0600 权限未变、35 条仍退出 index、35 个工作区指纹与 freeze 相同、`.gitmodules` sentinel 仍成立；它不得比较或改写非目标 index，并另行运行当前 `--source=index` 审计。两种 mode 都只能从 manifest 读取目标，不能重新发现路径。
- 同一 manifest 由实现执行的唯一 owner 保留到 T5.54：R0 全部门禁后先执行一次 `verify-final` 并把安全结果写入 E 的证据块；E 全部门禁后、生成外部 attestation 前再执行一次并把结果纳入 attestation。只有 attestation 成功持久化并回读验证后才按精确路径清理。manifest 丢失、权限/content identity 变化或任一次无法验证时，AC1 保持未通过，不能从删除后的 index 重建或伪造证明。

#### 删除前缀与 R0/E 证据状态机

删除状态是下列 18 个值的闭集，顺序不可改变：

```text
Prefix00PreDelete
Prefix01ProviderSSE
Prefix02MCPHTTP
Prefix03MCPHTTPCloser
Prefix04MCPStdio
Prefix05MCPStdioUnix
Prefix06MCPStdioOther
Prefix07MCPJSONRPC
Prefix08MCPProtocol
Prefix09MCPLimits
Prefix10MCPRedact
Prefix11PermissionSandbox
Prefix12ToolPath
Prefix13InstructionSandbox
Prefix14HookCommandUnix
Prefix15HookCommandOther
Prefix16OrchestratorToolBatches
Prefix17ConversationFileStore
```

- `TestDeletionCandidateState` 在没有显式期望时只验证当前仓库恰为一个合法前缀，使普通 unit profile 在任一阶段都可运行。`xagent-check --profile=deletion --deletion-prefix=PrefixNN...` 才以 `-args -expected-deletion-prefix=PrefixNN...` 选择唯一期望，并要求根测试与该精确 subtest 各 pass 一次；未知、缺失、额外 subtest、错前缀或零匹配失败。
- pre-delete 权威 commit 记为 `Rpre`。T5.37–T5.53 每项只删除一个已批准文件并形成连续的权威 commit `D01`–`D17`；每个 `Dn` 的 commit tree、deletion profile、相关 required tests 及需要的原生/E2E job 全部绑定同一 canonical OID，绿色后才能开始 `D(n+1)`。不能在未提交 working tree 上用仍指向旧 HEAD 的结果冒充 post-change 证据。
- D17 后只允许格式、module metadata 和证据前检查形成 clean commit `R0`。R0 的 core/final/commit repoaudit/docs、macOS/Linux/Windows amd64 各自 native＋E2E，以及独立 performance job 都在各自 fresh checkout 绑定 R0；任一结果不能由其他平台、index 模式、缓存或另一个 OID替代。
- evidence commit `E` 只能修改 `README.md`、`docs/index.md` 和当前 `checklist.md` 的结果记录，内嵌结果继续绑定 R0；`R0..E` 出现其他路径即失败。E 再以相同 fresh-checkout 规则完整运行 core/final/commit repoaudit/docs、三平台各 native＋E2E及 performance，所有 frame/report 绑定 E。仓库外不可变 attestation 最后绑定 R0、E、两阶段 exact argv/result 与 M0 manifest digest/最终本地 verify 结果。
- attestation 后任何仓库写入使证据失效，必须产生新 R0/E 链并重跑。M0 本地复验与外部 clean-checkout 代码证据是两个显式证据域：前者只证明获批 35 项未被实施过程改写，后者只证明 commit tree；两者不能互相替代。

C16 明确授权调整 `internal/conversation/{conversation,message,store}.go`、`internal/e2e/perfprotocol/protocol.go`、`internal/e2e/perfworkload/workload.go`、`internal/e2e/performance.go`、`cmd/xagent-check/{main,run}.go`、`cmd/xagent-repo-check/main.go`、`internal/repoaudit/{preservation,deletion_candidates,ci_workflow}.go`、`.github/workflows/ci.yml` 及对应测试/fixture。其余 production closed-world 约束不变；四份规格全部批准前仍禁止实现写入。

### C17：可执行 codec、权威工作区与可复核证据（已批准，2026-07-31）

C16 获批后的独立可实现性复审发现五处仍可被两种实现解释的边界：Decode 的目标类型、1 MiB 帧口径、Git 配置与清洁度来源、删除提交的精确父链，以及 canonical evidence 的生成和回读。C17 只消除这些歧义并补充跨任务 preservation manifest 的生命周期例外；不改变 C16 已批准的业务行为、删除候选或验收范围。未被本节改写的 C1–C16 条款继续有效。

#### 严格 NDJSON 的方向、大小与事务语义

- C16/C17 所有**可写创建或 staging 状态**的“私有 mode”使用统一平台语义：POSIX 目录/文件分别为 0700/0600 且创建后以已打开 handle `fstat` 复验；Windows 使用只授予当前用户与必要系统主体访问权、拒绝其他用户的 DACL，并在创建后从同一 handle 回读验证。materialized source、authoritative checkout 与 finalized archive 随后按本节显式 0500/0400 或 read-only DACL 转换；该更窄状态优先，不能再套用可写 0700/0600。Windows 不用虚构的 mode bits 代替 ACL 证明。
- `MaxFrameBytes = 1 << 20` 只计算从首个 `{` 到匹配 `}` 的 JSON object payload bytes，**不含**唯一结尾 `\n`；因此合法 wire frame 总长最多为 `MaxFrameBytes+1`。不接受 BOM、空行、CRLF、payload 前后空白、第二个顶层值或末尾额外字节。
- `Encoder.Encode` 只接受五个批准 DTO（`Bootstrap`、`Begin`、`Command`、`Ready`、`Result`）的值或 non-nil pointer；`Decoder.Decode` 只接受这五个 DTO 的 non-nil pointer。typed nil、map、slice、scalar、接口包装的其他 struct 及所有未批准类型均在读写前拒绝。
- `NewEncoder`/`NewDecoder` 在保存 borrowed I/O 前同时拒绝 nil interface 与 typed nil，永不 Close。Decoder 以最多 `MaxFrameBytes+1` bytes 的有界读取寻找唯一 LF：payload 恰为上限且下一 byte 为 LF 时合法；恰读到 `MaxFrameBytes+1` 个非 LF bytes 时即使随后 EOF 也优先返回安全 frame-too-large；不超过上限但 EOF 前已有 bytes 时返回 `io.ErrUnexpectedEOF`，零待处理 bytes 的干净 EOF 才返回 `io.EOF`。
- 每次 Decode 只消费一个 LF 终止的 frame；LF 后已缓冲的下一 frame 留给下一次 Decode，属于合法 NDJSON。“第二顶层值/额外字节”只指同一个 LF 前的 payload。payload 必须先通过 `utf8.Valid`，再用显式迭代 stack 做 token 级遍历；`MaxNestingDepth=64`，拒绝任意层 object 的重复 key、未知字段、错误类型和尾随 token，避免递归栈/CPU DoS。
- Decoder 先解码并完整校验到同类型临时值，全部成功后才一次赋给调用方目标；任何失败都不得发布部分字段。除干净 `io.EOF` 外，任一 read/schema/protocol error 都把 Decoder 标为 poisoned，后续调用只返回固定 safe poisoned error，不再读底层 I/O。
- Encoder 在 JSON 编码前先按五个 DTO 的闭合字段图迭代遍历全部 string 与 `[]string` 元素，逐个要求 `utf8.Valid`；无效 UTF-8 不得由 `encoding/json` 静默替换。随后才在私有有界 buffer 中形成 payload，复核为对应 DTO 的单一严格 object 且大小不超过上限；任一 preflight/validation 失败写入零 byte且使 Encoder poisoned。成功后尝试完整写入 payload 加一个 LF；short write/writer error 可能已由不可事务的 `io.Writer` 写出前缀，必须返回失败并 poison，绝不能把部分 frame 当成功。coordinator 与两个 adapter 只能复用该 codec。

#### 权威 commit 工作区的直接清洁度证明

commit 模式入口构造不可变 `AuditRevision` 与 `WorkspaceBaseline`。入口、每个 Step 紧邻执行前、Step 返回后和 profile 结束时都重新计算，且必须同时满足：

1. 当前 `HEAD^{commit}` 仍逐字等于入口的 40 位小写 OID；HEAD 改变、detached 状态改变到另一 OID或出现 merge 中间状态均失败。
2. index 仅含 stage 0，path/mode/OID 集合逐项等于该 commit tree；不得有 unmerged、intent-to-add、skip-worktree 或 assume-unchanged 项。
3. 不依赖 `git status`、ignore、fsmonitor 或 untracked cache，而是从 checkout 根执行 no-follow `lstat` 全量 inventory；除经验证的唯一 Git administrative entry 外，文件、symlink、目录和其他节点必须与 commit tree 的 path/type/blob 或 link-target identity 完全相同。任何 ignored、non-ignored、空目录、socket、额外链接或其他未入树节点均计为污染。
4. Git mode 只允许 tree/index 的 directory 040000、regular 100644/100755 与 symlink 120000，gitlink 160000 拒绝。POSIX regular 的实际 mode 以“任一 execute bit 存在→100755，否则→100644”投影后比较；fresh checkout bootstrap 根及其工作区外私有祖先先固定为 0700，checkout 产生的 tracked directory 允许常规 0755/0700 等无 special bit 且 group/other 不可写的 mode，不把“私有目录”误解为拒绝标准 0755 checkout，随后在 authoritative baseline 前按下文统一收紧为只读。入口记录每个节点的精确 POSIX mode/uid/nlink 或 Windows DACL/reparse security identity，此后即使仍落在允许集合，任一权限变化也失败。Windows executable bit 只由 index==tree 证明，tracked directory/file 继承只授予当前用户与必要系统主体的私有根 DACL，handle inventory 比 regular/symlink/reparse type、security identity 与 bytes/target。inventory 使用仓库 object format 计算 blob identity，绝不跟随链接；计数、摘要或错误只输出安全 code，不输出路径或内容。
5. traversal 从已打开根目录 handle 逐段相对打开，POSIX 使用 no-follow 并在 hash 前后 `fstat` 比较 device/inode/type/size，Windows 使用 handle-relative/no-reparse 等价检查并比较 file ID；拒绝 mount/volume crossing、unexpected reparse、regular hard-link count 不为 1 及身份漂移，不能采用 `lstat(path)` 后再按字符串路径打开的 TOCTOU 流程。
6. commit checkout 与 materialized index 共用同一 symlink-closure 校验。120000 target 必须为合法相对 bytes；从 symlink 所在目录以 handle-relative/no-follow 逐段解析完整 chain，最终只能落到同一 source root 内由同一 tree/index 跟踪的 regular、directory 或 symlink chain 终点。绝对 target、解析后逃逸、进入 `.git`、cycle、broken 或 untracked target、跨 mount/volume、unexpected reparse、大小写/Unicode alias及解析期间 identity 漂移均在 child 启动前拒绝；每个 Step 前后和 profile 结束重复复验，不能仅 hash link-target 后允许 child 跟随到权威 source 外。

任何前置复验失败都不启动该 Step；任何 Step 返回后出现写入，即使 exit 0，也立即判该 Step/profile 失败并停止剩余 Step。所有 coverage、测试二进制、Go/module/performance cache、下载的安全证据摘要和报告只允许写到工作区外私有目录。

Git 行为不得受调用者配置影响：

- runner 按 ASCII 不区分大小写拒绝继承**全部**以 `GIT_` 开头的环境键，而不是维护危险键黑名单；owner 随后只设置固定白名单 `GIT_NO_REPLACE_OBJECTS=1`、`GIT_OPTIONAL_LOCKS=0`、`GIT_CONFIG_NOSYSTEM=1`、`GIT_CONFIG_SYSTEM=<TASK_TMP>/empty.gitconfig`、`GIT_CONFIG_GLOBAL=<TASK_TMP>/empty.gitconfig`、`GIT_ATTR_NOSYSTEM=1`、`GIT_TERMINAL_PROMPT=0`。必要 Git 子命令使用经 lstat/hash 固定并记录 version 与 executable SHA-256 的同一 binary，逐次加 `--no-pager --no-replace-objects`、`core.fsmonitor=false`、`core.untrackedCache=false`，不调用 hook、alias、pager、credential helper、askpass、SSH、textconv、external diff、filter 或调用者 `GIT_EXEC_PATH`。
- commit owner 的 local config 只允许下文 fresh checkout 的精确结构键；任何可能改变对象解析、路径、attributes、ignore、filter、diff、hook、credential、fsmonitor、alternate 或命令执行的其他 local key 都在首个 Git 操作前拒绝。CI checkout 必须关闭凭据持久化。
- object directory 不得有非空 `info/alternates`、`info/grafts`、`info/attributes` 或 `shallow`，不得存在 promisor/partial-clone 配置或缺失对象，replace refs 永不解析；index 的 fsmonitor/untracked-cache extension 即使存在也不得被读取作为证据。C12 archive、HEAD/parent/diff 校验、repoaudit 和所有 profile 共用同一净化 Git owner。
- fetch/checkout/config 净化结束、进入 authoritative baseline 时，`.git` administrative tree 的节点语法是闭集。固定 regular files 只有 `HEAD`（内容恰为 `AuditRevision+LF`）、canonical `config`、`index`，以及 `objects/[0-9a-f]{2}/[0-9a-f]{38}` loose objects或成对的 `objects/pack/pack-<40hex>.{pack,idx}`；固定 directories 只有 root、`objects`、`objects/info`、`objects/pack` 与实际含允许 loose object 的两位 fanout。refs、logs、hooks、info/exclude、description、FETCH_HEAD、ORIG_HEAD、packed-refs、commit-graph、multi-pack-index、keep/promisor/rev/bitmap、lock 及其他 side file/空目录全部拒绝。object identity 集必须恰为 AuditRevision 可达对象与该 profile 明示需要的固定 baseline revision 可达对象之并集；fetch 得到的额外对象在只读 baseline 前以净化 owner 重建对象库并删除，缺失、额外、thin/alternate 来源均失败。

该 owner 的跨包边界固定如下；字段与构造 marker 均不导出，调用方不能传任意 Git argv、替换 resolver 或绕过净化环境：

```go
package repoaudit

type GitEvidenceOwner struct { /* 私有 root、task dir、source、object format 与有效 marker */ }

type GitEvidenceSource string

const (
	GitEvidenceIndex  GitEvidenceSource = "index"
	GitEvidenceCommit GitEvidenceSource = "commit"
)

type WorkspaceSnapshot struct {
	Revision       string
	IndexSHA256    string
	InventorySHA256 string
	GitAdminSHA256 string
	IndexEntries   int
	InventoryNodes int
}

type CommitDelta struct {
	Commit  string
	Parent  string
	Changes []CommitChange
}

type CommitChange struct {
	Path       string
	ChangeKind string
	OldMode    string
	OldOID     string
	NewMode    string
	NewOID     string
}

func NewGitEvidenceOwner(repoRoot, taskDir string, source GitEvidenceSource, revision string) (*GitEvidenceOwner, error)
func (g *GitEvidenceOwner) VerifyCommitWorkspace(ctx context.Context, expectedRevision string) (WorkspaceSnapshot, error)
func (g *GitEvidenceOwner) MaterializeIndex(ctx context.Context) (sourceRoot string, indexSHA256 string, err error)
func (g *GitEvidenceOwner) VerifyMaterializedIndex(ctx context.Context, sourceRoot, expectedIndexSHA256 string) error
func (g *GitEvidenceOwner) ArchiveCommit(ctx context.Context, revision string, dst io.Writer) error
func (g *GitEvidenceOwner) ReadSingleParentDelta(ctx context.Context, revision string) (CommitDelta, error)
```

`NewGitEvidenceOwner` 对 repo root 与工作区外私有 task dir 都执行绝对路径、no-follow identity 和权限校验。commit source 要求非空 canonical revision 与 fresh checkout，index source 要求 revision 为空且只用内建 parser 打开 index/object database，二者都拒绝 linked worktree/submodule。index owner 不读取、执行或要求用户修改普通 clone 的 local config；remote/branch 等键对其没有行为作用，但 alternate、replace、graft、partial-clone 与缺失对象仍 fail closed。fresh commit checkout 必须使用普通 `.git` 目录；其 raw local config 允许键闭集为 `core.repositoryformatversion=0`、`core.filemode=<bool>`、`core.bare=false`、`core.logallrefupdates=true`、`core.ignorecase=<bool>`、`core.precomposeunicode=<bool>`，以及缺省或精确 `extensions.objectformat=sha1`。其他 local key（包括 remote、branch、HTTP、proxy、credential、hook、filter、attributes 与 worktree extension）一律拒绝；checkout/fetch 完成后必须先移除这些运行期配置，才可进入 profile。

路径域在构造时一次封闭：`XAGENT_EVIDENCE_DIR` 必须与 `XAGENT_TASK_TMP`、用户普通 worktree、fresh checkout repo root 及 index materialized source root 互不相等、互不为祖先/后代且不存在 symlink、reparse、hard-link、mount/volume 或大小写/Unicode alias；`XAGENT_TASK_TMP` 可以持有其自身 fresh checkout/source root，但不得包含 evidence dir。`XAGENT_EVIDENCE_DIR` 的直接父目录必须是 evidence owner 独占创建的工作区外私有 control root；该父目录只容纳 evidence dir 与下文固定 sibling lease/active sentinel，不能与其他 evidence/task/source root 共用。owner 对各根及所有祖先使用 handle-relative/no-follow identity 验证并在每次发布/Step 前复验；任何 alias、containment 或 identity 漂移在读取或删除证据前 fail closed。evidence dir 不能借 task cleanup 被清理，task dir 也不能借 evidence 保留规则逃过清理。

token 的合法从属关系同样固定：`<PRESERVATION_MANIFEST>` 必须逐段等于 `<EVIDENCE_DIR>/preservation/m0.manifest`；`<SOURCE_ROOT>` 与 `<FIXTURE_ROOT>` 必须是 `<TASK_TMP>` 下两个互不包含、互不 alias 的直接私有子树，fresh checkout repo root 只能占据 SOURCE_ROOT；Go/module/cache/temp 只能位于 TASK_TMP 的其他固定直接子树。`<GO_TOOLCHAIN>`、`<GIT_TOOL>` 与 `<APPROVED_SYSTEM_PATH>` 必须在两个私有根之外且只读，不能与任一 source/fixture/cache/evidence 节点共享 file identity。任何 token 不能通过 suffix 改变这些从属关系。

方法/source 组合 fail closed：`VerifyCommitWorkspace`、`ArchiveCommit`、`ReadSingleParentDelta` 只允许 commit owner，且每个 revision 参数必须等于构造时保存的 canonical revision或由该 revision 的固定 ancestry 校验显式授权；`MaterializeIndex`/`VerifyMaterializedIndex` 只允许 index owner。nil/typed-nil writer、空 path/digest、另一个 source root、第二次 materialize、构造后环境/owner identity 漂移均拒绝。

Snapshot digest 的 canonical bytes 不由实现自选：全部 path 按 raw bytes 升序；index 每项串接 `mode SP oid TAB path NUL`。inventory 的 `security-id` 在 POSIX 为 `mode-octal,uid-decimal,nlink-decimal`，在 Windows 为 `dacl-sha256,reparse-tag-hex`；directory 串接 `d SP security-id TAB path NUL`，regular 串接 `f SP verified-tree-mode SP security-id SP blob-oid SP size TAB path NUL`，symlink 串接 `l SP security-id SP blob-oid SP target-size TAB path NUL`。SHA-256 覆盖全部串接 bytes；`IndexEntries` 只计文件/symlink entries，`InventoryNodes` 计 directory、regular、symlink 全部节点但不计根与 `.git`。任何其他节点类型失败。materialized index 在 publish baseline 前以 children-first/root-last 收紧：POSIX 非 executable regular=0400、executable regular=0500、全部 directory=0500，symlink 只验证 target 且不得跟随；Windows regular/directory DACL 只允许当前用户与必要系统主体 read/execute/traverse/read-control并拒绝写入、append、delete/delete-child。`VerifyMaterializedIndex` 在每个 index Step 前后及 profile 结束按同一编码复算 source root并复验这些精确权限；child 改权限、内容或节点后立即停链。

commit checkout 在完成 fetch/config 净化后，以 children-first/root-last 把 tracked worktree 与 `.git` administrative tree 收紧为同一精确只读策略：POSIX 非 executable regular=0400、executable regular=0500、directory=0500；Windows 使用与 materialized index 相同的 current-user/system read/execute/traverse DACL并拒绝 data write、append、delete/delete-child。symlink 不跟随且只按 target/closure验证。每次 `VerifyCommitWorkspace` 仍以 handle-relative/no-follow inventory 对 `.git` 的闭集逐项计算 `GitAdminSHA256`：admin-relative raw path 升序，root 以唯一 `.` sentinel 计入；directory 串接 `d SP security-id TAB path NUL`，regular 串接 `f SP security-id SP content-sha256 SP size TAB path NUL`，directory 没有虚构 content hash/size。摘要必须恰好覆盖 root、固定结构目录与全部允许文件，拒绝 socket、link/reparse、额外或变化节点。Git/worktree read-only owner 不得写 source、object、ref 或 index；任何 Step 改权限或 bytes 同样停链，不能借“工作树外”逃过检查。

commit 模式所称 fresh 的可观测定义固定为：根下唯一 `.git` 是不跟随链接/reparse 的真实目录、非 linked worktree/submodule；local config 与 Git side files 满足上述闭集；目标对象及全部 reachable tree/blob 本地完整；入口 inventory clean；CI provider 的 job identity 绑定该 checkout OID。不得凭目录创建时间或名称声称 fresh。`ArchiveCommit` 还逐项比较 archive 与目标 commit tree，若 system/info/tracked attributes 导致 export-ignore、export-subst、漏项或内容替换即失败，不把改变后的 archive 交给 baseline。

#### Profile 来源矩阵与 revision 注入

所有 profile 都显式传 `--audit-source`，支持矩阵是闭集：

| Profile | index | commit | 证据等级 |
|---|---|---|---|
| `checker-self`、`static`、`unit`、`race`、`coverage`、`cross-build`、`repoaudit`、`docs`、`core` | 允许 | 允许 | index 仅供本地预检；commit 才可成为权威证据 |
| `native`、`e2e`、`deletion`、`required` | 允许 | 允许 | index 结果必须标记 `preliminary`，不得写入 checklist 或 attestation |
| `performance`、`final`、`release`、`attestation` | 拒绝 | 必须 | 只能在 fresh authoritative checkout 运行 |

- index profile 不能从用户 worktree 编译或读取输入。唯一 Git owner 先验证 index 只有 stage 0 且所有 blob 可解析，再按 raw path bytes 将其 handle-relative、no-follow 地 materialize 到 `XAGENT_TASK_TMP` 下的新私有 source root；绝对或词法逃逸 symlink、Windows 无法安全表达的 link/reparse、gitlink、缺 blob、case/Unicode path 碰撞均在启动 child 前失败。所有普通 Step 的 cwd 指向该只读快照，证据绑定完整 index digest；每个 Step 前后及 profile 结束调用 `VerifyMaterializedIndex`。只有 `xagent-repo-check --source=index` 直接读取原 index，且它仍不得枚举用户 worktree。
- commit profile 的 `--revision=<OID>` 由 checker 在入口验证一次并保存；checker 对每个 child 显式覆盖注入 `XAGENT_AUDIT_SOURCE=commit` 与 `XAGENT_AUDIT_REVISION=<同一 OID>`，不得让 child 从 HEAD、branch、tag、缓存或父环境重新推断。index child 固定 `XAGENT_AUDIT_SOURCE=index` 且不得存在 revision 环境值。
- child `xagent-repo-check` 的 argv 只能是 `xagent-repo-check --source=commit --revision=<同一 OID>` 或 index hook 的 `xagent-repo-check --source=index`。performance 明确区分三个 revision：`AuditRevision` 是 checker 注入的当前证据提交，固定 `BaselineRevision=607b3dd3cb9d5f01bcba9a047b816187b4512b4a`，`CurrentRevision=AuditRevision`；baseline 五类 frame 只匹配 BaselineRevision，current 五类 frame 只匹配 CurrentRevision，coordinator/report 同时记录并核对三者，禁止要求 baseline frame 等于 AuditRevision。
- 每项 evidence 记录入口 OID、Step 前后 workspace digest、canonical safe argv 元素数组、显式安全 env 键、required package/test/次数清单、实际/缺失/skip/超额计数和 exit 分类。required 的 Missing/Excess 只按完整 package＋test/subtest identity 与精确次数计算；所选 `go test` JSON 流中的任意 `skip`、package fail、test fail、malformed/truncated/trailing event 都使整个 Step 失败，即使该项不在 required 清单。重复/非法 manifest、错误 package 的同名测试、缓存导致的零匹配及 deletion 根/指定 subtest 任一缺失也 fail closed。
- `checker-self` 是 T5.26 后验证 checker 自身与 required gate 的唯一 profile。仅在 T5.26 首次实现 gate 之前，允许一次固定 checker bootstrap argv，并由执行者按同一 exact JSON 规则保存/人工核对；T5.26 验证通过后该例外永久结束，T5.27 及以后不得再以 raw `go test` 绕过 checker-self。
- `required` 只接受 `--deletion-prefix=PrefixNN...`，从已批准 Task 为该前缀冻结的唯一 CommandManifest 取得定向测试 Step，不能接受 package、正则、次数或任意 argv 参数；它与 `deletion` 是每个 Dn 的两个独立 job。非删除 stage、缺/未知/重复 prefix 或调用者附加测试选择均失败。

“canonical safe argv/env”是实际执行值的一一安全投影，不是自由脱敏文本。owner 先对实际 path-valued argv/env 做 handle/ACL/根边界校验，再只允许以下 token：`<TASK_TMP>`、`<EVIDENCE_DIR>`、`<PRESERVATION_MANIFEST>`、`<SOURCE_ROOT>`、`<FIXTURE_ROOT>`、`<GO_TOOLCHAIN>`、`<GIT_TOOL>`、`<APPROVED_SYSTEM_PATH>`。suffix 只能由 ASCII `[A-Za-z0-9._-]+` segments 组成，统一用 `/` 分隔、大小写敏感，禁止空 segment、`.`、`..`、反斜线、drive/UNC 与 percent encoding；支持 whole element、批准的 `--key=value`/`-o=value` 及 env value 三种固定位置，其他绝对路径或路径嵌入方式拒绝。

PathBindingEvidence 的 Kind 闭集及 token 对应固定为：TASK_TMP/EVIDENCE_DIR=`private-dir`，PRESERVATION_MANIFEST=`private-file`，SOURCE_ROOT=`source-root`，FIXTURE_ROOT=`fixture-root`，GO_TOOLCHAIN=`tool-dir`，GIT_TOOL=`tool-executable`，APPROVED_SYSTEM_PATH=`system-path-list`。handle identity 的 canonical bytes 不含 path：POSIX 单节点为 `kind SP dev-hex SP ino-hex SP exact-mode-octal SP uid-decimal SP nlink-decimal NUL`；Windows 单节点为 `kind SP volume-serial-hex SP file-id-hex SP reparse-tag-hex SP dacl-sha256 NUL`；system-path-list 按实际搜索顺序串接各已验证 directory identity。`HandleIdentitySHA256` 覆盖这些 bytes。record 保存投影后的完整元素和按 token raw bytes 排序的唯一 bindings，不保存或散列原始绝对路径；因此 logical argv 可复算而用户目录不能由 path hash 猜测。

EnvEvidence 只允许 Step manifest 声明的审计 revision/source、固定 Go/Git/offline/locale/timezone 值，以及上述 token 化的 private roots/toolchain。实际 PATH 的唯一形态是 Go tool directory 后接平台批准 system directory list；owner 在 POSIX `:`/Windows `;` 上逐项解析并验证后，record 一律投影为 `<GO_TOOLCHAIN>:<APPROVED_SYSTEM_PATH>`。Git 始终通过已解析的 `<GIT_TOOL>` absolute executable 启动，不进入 PATH。credential、token、proxy、askpass、SSH、用户 HOME、真实 CA/path、调用者 `GOENV/GOWORK/GOTOOLCHAIN` 或未声明键一律拒绝。

CommandManifest 另允许唯一动态 template token `<AUDIT_REVISION>`，只可占据完整 env value、完整 argv element 或批准的 `--revision=<AUDIT_REVISION>` value；它不是 path token，也没有 PathBinding。模板在 Task 批准时冻结。运行时 checker 先从 stage 唯一取得 Rpre/Dn/R0/E OID，要求实际 argv/env 逐字含该 OID，再反向替换成 `<AUDIT_REVISION>` 后与模板逐字段比较并计算 CommandManifestSHA256；未知 token、从 actual 临时生成 template、用另一个 stage OID 或遗漏 revision 均失败。

所有 authoritative Go Step 在进入 cleanliness baseline 前完成依赖预取，随后固定 `GOTOOLCHAIN=local`、`GOENV=off`、`GOWORK=off`、`GOPROXY=off`，并把 `GOCACHE`、`GOMODCACHE`、`GOPATH`、`GOTMPDIR`、`TMPDIR/TMP/TEMP` 与 HOME/USERPROFILE 等运行目录绑定到对应私有 token；performance 使用已冻结 content digest 的只读 module cache。GOOS/GOARCH/CGO、locale 与 timezone 由 Step manifest 明示，未声明或继承调用者值失败，Go 不得自动下载 toolchain/module或写用户目录。

required manifest digest 的 canonical bytes 同样固定：Required 按 package raw bytes、再按完整 test/subtest raw bytes 升序，每项串接 `package TAB test TAB expected-runs NUL`，ExpectedRuns 必须为正；SHA-256 覆盖全部串接 bytes。ActualRuns、Missing、Skipped、Excess 与所有 entry/node/count 均不得为负；`ActualRuns==ExpectedRuns`、Missing/Skipped/Excess 全为 0 才可 pass。

#### 删除提交的单父链与精确 diff

`Rpre`、`D01`–`D17`、`R0`、`E` 都是 canonical commit OID 或下述允许的同一 OID 别名，不能是 tag、branch、未提交 worktree 或 merge commit。固定映射如下：

| 提交 | Task / 前缀 | 唯一删除路径 |
|---|---|---|
| `D01` | T5.37 / `Prefix01ProviderSSE` | `internal/provider/sse.go` |
| `D02` | T5.38 / `Prefix02MCPHTTP` | `internal/mcpclient/http.go` |
| `D03` | T5.39 / `Prefix03MCPHTTPCloser` | `internal/mcpclient/http_closer.go` |
| `D04` | T5.40 / `Prefix04MCPStdio` | `internal/mcpclient/stdio.go` |
| `D05` | T5.41 / `Prefix05MCPStdioUnix` | `internal/mcpclient/stdio_unix.go` |
| `D06` | T5.42 / `Prefix06MCPStdioOther` | `internal/mcpclient/stdio_other.go` |
| `D07` | T5.43 / `Prefix07MCPJSONRPC` | `internal/mcpclient/jsonrpc.go` |
| `D08` | T5.44 / `Prefix08MCPProtocol` | `internal/mcpclient/protocol.go` |
| `D09` | T5.45 / `Prefix09MCPLimits` | `internal/mcpclient/limits.go` |
| `D10` | T5.46 / `Prefix10MCPRedact` | `internal/mcpclient/redact.go` |
| `D11` | T5.47 / `Prefix11PermissionSandbox` | `internal/permission/sandbox.go` |
| `D12` | T5.48 / `Prefix12ToolPath` | `internal/tool/path.go` |
| `D13` | T5.49 / `Prefix13InstructionSandbox` | `internal/instructions/sandbox.go` |
| `D14` | T5.50 / `Prefix14HookCommandUnix` | `internal/hook/command_unix.go` |
| `D15` | T5.51 / `Prefix15HookCommandOther` | `internal/hook/command_other.go` |
| `D16` | T5.52 / `Prefix16OrchestratorToolBatches` | `internal/orchestrator/tool_batches.go` |
| `D17` | T5.53 / `Prefix17ConversationFileStore` | `internal/conversation/file_store.go` |

- `parent(D01)=Rpre`，且对 `n=02..17`，`parent(Dn)=D(n-1)`；每个 Dn 恰有一个 parent。相对 parent 的 changed-path 集合必须恰为上表对应删除路径和 `internal/repoaudit/deletion_candidates_test.go`，前者只能是删除，后者只能把对应唯一状态值从 `present_no_live_references` 改为 `deleted`，其他 byte 不得变化。
- `DeltaSHA256` 的 canonical bytes 按 changed path raw bytes 升序，每项串接 `change-kind SP old-mode SP old-oid SP new-mode SP new-oid TAB path NUL`；删除项的 new mode/OID 使用单一 `-` sentinel，修改项两侧必须完整。SHA-256 覆盖恰好两项，不能散列面向人的 diff 文本。
- 每个 Dn 都在自身 fresh checkout 运行精确 `deletion` profile、该任务 required tests 和所需原生/E2E job，全部绑定 Dn 后才能创建下一提交。D05、D14 要求 Darwin/Linux 各 native＋e2e；D06、D15 要求 Windows native＋e2e；D11–D13 要求三平台各 native＋e2e。其他 Dn 执行 Task 指定的 race/E2E/cross-build，并同样记录 Dn。
- D17 后先在 D17 上以 performance 已预取并冻结 content digest 的只读 module cache，固定 `GOPROXY=off`、`GOSUMDB=off`、`GOENV=off`、`GOWORK=off`、`GOTOOLCHAIN=local` 与空 `GOFLAGS`，不继承其他调用者 Go 环境，并使用固定 Go toolchain执行 `go mod tidy -diff`；离线缺依赖直接失败。固定 toolchain 的合法无差异结果只能是 exit 0、stdout/stderr 均空，此时 `R0=D17`。合法差异结果只能是 exit 1、stderr 空、stdout 为可完整严格解析且只触及 `go.mod`/`go.sum` 非空子集的单一 unified diff；其他 exit、stderr、空/截断/多余输出或解析结果均失败。只允许在同一净化环境把捕获 stdout 逐 byte 应用一次，形成一个以 D17 为唯一 parent、changed paths 为该非空子集的 R0；R0 上再次运行必须 exit 0 且 stdout/stderr 均空，checker 还要核对 D17 捕获 stdout SHA-256、字节数、exit 与 commit delta。C16 所称“格式”全部前移到 Rpre 前完成；D17..R0 禁止修改源码、测试、文档、配置、CI 或其他文件。
- `parent(E)=R0` 且 E 恰有一个 parent。`README.md`、`docs/index.md` 与当前 `checklist.md` 必须在 Rpre 前已包含唯一、成对、不可嵌套的结构化 evidence/result markers；E 只能改变这三份文件 markers 内的 canonical 结果 block。审计以 R0 版本为基准逐 byte 证明 markers 外内容不变，并拒绝借白名单修改行为说明、Plan、Task、测试、配置或 CI。

markers 的 byte grammar 固定为一行 `<!-- xagent-evidence-v1:begin -->\n`、恰好一行 compact canonical JSON 加 LF、以及一行 `<!-- xagent-evidence-v1:end -->\n`；不接受缩进、围栏、第二个 block、嵌套 marker 或 marker 行上的其他 bytes。JSON DTO 固定如下：

```go
package repoaudit

type EvidenceCheckResult struct {
	ID             string `json:"id"` // AC1..AC38
	Result         string `json:"result"` // pass；pending block 不含本结构
	EvidenceSHA256 string `json:"evidence_sha256"`
}

type EvidenceDocumentResult struct {
	SchemaVersion      int                   `json:"schema_version"` // 1
	Document           string                `json:"document"` // readme、docs-index、checklist
	R0                 string                `json:"r0"` // pending 时空；pass 时 canonical OID
	R0EvidenceSHA256   string                `json:"r0_evidence_sha256"` // pending 时空
	Result             string                `json:"result"` // pending 或 pass
	Checks             []EvidenceCheckResult `json:"checks"`
}
```

Rpre 前三份 block 必须是唯一 canonical pending 形态：Document 各自正确，R0 与 R0EvidenceSHA256 为空字符串，Result=`pending`，Checks=`[]`。Rpre、D01–D17 与 R0 的 docs profile 只接受该形态。E 只允许把每个完整 pending JSON 行替换为 canonical pass 形态；三份 pass block 的 R0/R0EvidenceSHA256/Result 必须相同，README 与 docs-index 的 Checks 仍为空数组，checklist 的 Checks 按 AC1..AC38 数值顺序恰好 38 项且每项 EvidenceSHA256 绑定批准的 R0 safe summary 集。其他 pending/pass 混合、空字段、部分替换或第三状态均失败。E 或自身 OID不得出现在 block，从而不产生自引用；marker 内不能出现任意说明文字、命令字符串、路径、URL 或额外字段。

结果摘要的字节口径固定：每个 JobEvidence/PreservationEvidence 先取其 compact canonical JSON payload（无 LF）计算 SHA-256。`R0EvidenceSHA256` 输入依次为 11 个 R0 job 的 `r0 TAB job-name TAB job-evidence-sha NUL`，再加 `preservation TAB after_r0 TAB preservation-evidence-sha NUL`；顺序与 ExpectedEvidenceManifest 一致。每个 AC 的 `EvidenceSHA256` 输入是 `AC-id NUL r0-evidence-sha NUL`，因此 AC1–AC38 全部明确绑定同一完整 R0 门禁集与 after_r0 preservation，而不是由实现者任选证据子集。

#### Attestation record、生成入口与外部回读

仓库外证明的唯一 canonical record 使用无 map 的闭合 DTO；JSON 字段按下列声明顺序紧凑编码，UTF-8、无 HTML escape、无可选未知字段，payload 后只加一个 LF，SHA-256 只覆盖不含 LF 的 payload：

```go
package repoaudit

type RequiredTestEvidence struct {
	Package      string   `json:"package"`
	Test         string   `json:"test"`
	ExpectedRuns int      `json:"expected_runs"`
	ActualRuns   int      `json:"actual_runs"`
}

type EnvEvidence struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type PathBindingEvidence struct {
	Token                string `json:"token"`
	Kind                 string `json:"kind"`
	HandleIdentitySHA256 string `json:"handle_identity_sha256"`
}

type WorkspaceEvidence struct {
	HeadOID         string `json:"head_oid"`
	IndexSHA256     string `json:"index_sha256"`
	InventorySHA256 string `json:"inventory_sha256"`
	GitAdminSHA256  string `json:"git_admin_sha256"`
	IndexEntries    int    `json:"index_entries"`
	InventoryNodes  int    `json:"inventory_nodes"`
}

type CommandEvidence struct {
	Step                   string                 `json:"step"`
	ExecutableToken        string                 `json:"executable_token"`
	ExecutableVersion      string                 `json:"executable_version"`
	ExecutableSHA256       string                 `json:"executable_sha256"`
	Argv                   []string               `json:"argv"`
	Env                    []EnvEvidence          `json:"env"`
	PathBindings           []PathBindingEvidence  `json:"path_bindings"`
	AuditSource            string                 `json:"audit_source"` // 固定 commit
	Revision               string                 `json:"revision"`
	Before                 WorkspaceEvidence      `json:"before"`
	After                  WorkspaceEvidence      `json:"after"`
	Required               []RequiredTestEvidence `json:"required"`
	RequiredManifestSHA256 string                 `json:"required_manifest_sha256"`
	Missing                int                    `json:"missing"`
	Skipped                int                    `json:"skipped"`
	Excess                 int                    `json:"excess"`
	ExitCode               int                    `json:"exit_code"`
	Result                 string                 `json:"result"` // 只能为 pass
}

type JobSummaryRecord struct {
	SchemaVersion         int               `json:"schema_version"` // 固定 1
	Stage                 string            `json:"stage"` // rpre、d01..d17、r0 或 e
	Name                  string            `json:"name"`
	CommandManifestSHA256 string            `json:"command_manifest_sha256"`
	CIProvider            string            `json:"ci_provider"`
	RepositoryID          string            `json:"repository_id"`
	WorkflowPath          string            `json:"workflow_path"`
	WorkflowBlobOID       string            `json:"workflow_blob_oid"`
	WorkflowDependenciesSHA256 string        `json:"workflow_dependencies_sha256"`
	JobID                 string            `json:"job_id"`
	CheckoutOID           string            `json:"checkout_oid"`
	CIRunID               string            `json:"ci_run_id"`
	RunAttempt            uint64            `json:"run_attempt"`
	RunnerOS              string            `json:"runner_os"`
	RunnerArch            string            `json:"runner_arch"`
	GoVersion             string            `json:"go_version"`
	GoExecutableSHA256   string            `json:"go_executable_sha256"`
	GitVersion            string            `json:"git_version"`
	GitExecutableSHA256   string            `json:"git_executable_sha256"`
	PerformanceReportSHA256 string          `json:"performance_report_sha256"`
	Performance           []PerformanceMetricEvidence `json:"performance"`
	Commands              []CommandEvidence `json:"commands"`
	Result                string            `json:"result"` // 只能为 pass
}

type ProviderEvidenceRecord struct {
	SchemaVersion              int    `json:"schema_version"` // 固定 1
	CIProvider                 string `json:"ci_provider"`
	RepositoryID               string `json:"repository_id"`
	WorkflowPath               string `json:"workflow_path"`
	WorkflowBlobOID            string `json:"workflow_blob_oid"`
	WorkflowDependenciesSHA256 string `json:"workflow_dependencies_sha256"`
	JobID                      string `json:"job_id"`
	CheckoutOID                string `json:"checkout_oid"`
	CIRunID                    string `json:"ci_run_id"`
	RunAttempt                 uint64 `json:"run_attempt"`
	SafeSummarySHA256          string `json:"safe_summary_sha256"`
	Result                     string `json:"result"` // 只能为 pass
}

type WorkflowDependencyUse struct {
	SourceIdentity string `json:"source_identity"` // 父 manifest 内唯一 job/step identity
	Uses           string `json:"uses"` // 经 YAML 解码后的完整 uses scalar
	TargetIdentity string `json:"target_identity"`
}

type WorkflowDependencyLockEntry struct {
	Identity       string                  `json:"identity"` // canonical target frame 的 SHA-256
	Kind           string                  `json:"kind"` // remote-action、remote-workflow 或 container
	Repository     string                  `json:"repository"`
	Revision       string                  `json:"revision"` // 40 位 commit 或 sha256:<64hex>
	ManifestPath   string                  `json:"manifest_path"` // container 时为空
	ManifestSHA256 string                  `json:"manifest_sha256"` // container 时等于 digest 的 64hex
	Uses           []WorkflowDependencyUse `json:"uses"`
}

type WorkflowLocalManifestLockEntry struct {
	Identity       string                  `json:"identity"` // canonical target frame 的 SHA-256
	Kind           string                  `json:"kind"` // local-action 或 local-workflow
	ManifestPath   string                  `json:"manifest_path"`
	ManifestBlobOID string                 `json:"manifest_blob_oid"`
	ManifestSHA256 string                  `json:"manifest_sha256"`
	Uses           []WorkflowDependencyUse `json:"uses"`
}

type WorkflowDependencyLockRecord struct {
	SchemaVersion       int                              `json:"schema_version"` // 固定 1
	RootWorkflowPath    string                           `json:"root_workflow_path"`
	RootWorkflowBlobOID string                           `json:"root_workflow_blob_oid"`
	RootUses            []WorkflowDependencyUse          `json:"root_uses"`
	LocalManifests      []WorkflowLocalManifestLockEntry `json:"local_manifests"`
	Entries             []WorkflowDependencyLockEntry    `json:"entries"`
}

type EvidenceMarkerRenderResult struct {
	SchemaVersion    int                      `json:"schema_version"` // 固定 1
	R0               string                   `json:"r0"`
	R0EvidenceSHA256 string                   `json:"r0_evidence_sha256"`
	Documents        []EvidenceDocumentResult `json:"documents"` // readme、docs-index、checklist
}

type JobEvidenceImportRequest struct {
	SchemaVersion      int                       `json:"schema_version"` // 固定 1
	Stage              string                    `json:"stage"`
	Job                string                    `json:"job"`
	Summary            JobSummaryRecord          `json:"summary"`
	PerformanceReports []PerformanceReportRecord `json:"performance_reports"` // [] 或恰好一项
	Provider           ProviderEvidenceRecord    `json:"provider"`
}

type JobEvidence struct {
	Summary                JobSummaryRecord `json:"summary"`
	SafeSummarySHA256      string           `json:"safe_summary_sha256"`
	ProviderEvidenceSHA256 string           `json:"provider_evidence_sha256"`
}

type PerformanceMetricEvidence struct {
	Workload            string `json:"workload"`
	BaselineRevision    string `json:"baseline_revision"`
	CurrentRevision     string `json:"current_revision"`
	WarmupSamples       int    `json:"warmup_samples"`
	ScoredSamples       int    `json:"scored_samples"`
	BaselineMedianNanos uint64 `json:"baseline_median_nanos"`
	CurrentMedianNanos  uint64 `json:"current_median_nanos"`
	RatioPPM            uint64 `json:"ratio_ppm"`
}

type PerformanceReportRecord struct {
	SchemaVersion  int                         `json:"schema_version"` // 固定 1
	AuditRevision  string                      `json:"audit_revision"`
	BaselineRevision string                    `json:"baseline_revision"`
	CurrentRevision string                     `json:"current_revision"`
	Metrics        []PerformanceMetricEvidence `json:"metrics"`
}

type DeletionEvidence struct {
	Sequence          int           `json:"sequence"`
	Prefix            string        `json:"prefix"`
	Revision          string        `json:"revision"`
	Parent            string        `json:"parent"`
	DeltaSHA256       string        `json:"delta_sha256"`
	DeletedPath       string        `json:"deleted_path"`
	StatePath         string        `json:"state_path"`
	Jobs              []JobEvidence `json:"jobs"`
}

type R0TransitionEvidence struct {
	D17              string   `json:"d17"`
	R0               string   `json:"r0"`
	TidyExitCode     int      `json:"tidy_exit_code"` // 只能为 0 或 1
	TidyDiffBytes    int      `json:"tidy_diff_bytes"`
	TidyDiffSHA256   string   `json:"tidy_diff_sha256"`
	DeltaSHA256      string   `json:"delta_sha256"` // R0==D17 时空
	ChangedPaths     []string `json:"changed_paths"` // [] 或 go.mod/go.sum 非空子集
}

type ExpectedJobEvidence struct {
	Stage                        string `json:"stage"`
	Name                         string `json:"name"`
	CommandManifestSHA256        string `json:"command_manifest_sha256"`
}

type ExpectedEvidenceManifest struct {
	SchemaVersion int                   `json:"schema_version"` // 固定 1
	Jobs          []ExpectedJobEvidence `json:"jobs"`
}

type RequiredTestManifestEntry struct {
	Package      string `json:"package"`
	Test         string `json:"test"`
	ExpectedRuns int    `json:"expected_runs"`
}

type ExpectedPathBinding struct {
	Token string `json:"token"`
	Kind  string `json:"kind"`
}

type ExpectedCommandEvidence struct {
	Step             string                      `json:"step"`
	ExecutableToken  string                      `json:"executable_token"`
	Argv             []string                    `json:"argv"`
	Env              []EnvEvidence               `json:"env"`
	PathBindings     []ExpectedPathBinding       `json:"path_bindings"`
	Required         []RequiredTestManifestEntry `json:"required"`
}

type CommandManifestRecord struct {
	SchemaVersion int                       `json:"schema_version"` // 固定 1
	Commands      []ExpectedCommandEvidence `json:"commands"`
}

type PreservationEvidence struct {
	Phase          string   `json:"phase"` // after_r0 或 after_e
	Mode           string   `json:"mode"`  // verify-final
	ManifestSHA256 string   `json:"manifest_sha256"`
	TargetCount    int      `json:"target_count"`
	TargetDigest   string   `json:"target_digest"`
	VerifyArgv     []string `json:"verify_argv"`
	IndexAuditArgv []string `json:"index_audit_argv"`
	PathBindings   []PathBindingEvidence `json:"path_bindings"`
	ExitCode       int      `json:"exit_code"`
	Result         string   `json:"result"` // 只能为 pass
}

type AttestationRecord struct {
	SchemaVersion   int                    `json:"schema_version"` // 固定 1
	EvidenceManifestSHA256 string          `json:"evidence_manifest_sha256"`
	Rpre            string                 `json:"rpre"`
	Deletion        []DeletionEvidence     `json:"deletion"` // 恰为 D01..D17
	R0Transition    R0TransitionEvidence   `json:"r0_transition"`
	R0              string                 `json:"r0"`
	E               string                 `json:"e"`
	Jobs            []JobEvidence          `json:"jobs"`
	Preservation    []PreservationEvidence `json:"preservation"`
}

type RecordVerification struct {
	SchemaVersion             int    `json:"schema_version"` // 固定 1
	AttestationSHA256         string `json:"attestation_sha256"`
	EvidenceManifestSHA256    string `json:"evidence_manifest_sha256"`
	E                         string `json:"e"`
	Result                    string `json:"result"` // pass
}

type ArchivePreparationRecord struct {
	SchemaVersion             int    `json:"schema_version"` // 固定 1
	AttestationSHA256         string `json:"attestation_sha256"`
	RecordVerificationSHA256  string `json:"record_verification_sha256"`
	ManifestSHA256            string `json:"manifest_sha256"`
	TargetCount               int    `json:"target_count"`
	TargetDigest              string `json:"target_digest"`
	ExternalMode              string `json:"external_mode"` // none 或 verified
	ReadBackSHA256            string `json:"readback_sha256"` // none 时空
	ImmutableEvidenceSHA256   string `json:"immutable_evidence_sha256"` // none 时空
	Result                    string `json:"result"` // prepared
}

type ImmutableReceiptEvidence struct {
	SchemaVersion       int    `json:"schema_version"` // 固定 1
	Provider            string `json:"provider"`
	ObjectID            string `json:"object_id"`
	VersionID           string `json:"version_id"`
	RetentionMode       string `json:"retention_mode"` // compliance 或 append-only
	VerifiedAtUTC       string `json:"verified_at_utc"` // RFC3339 UTC
	RetainUntilUTC      string `json:"retain_until_utc"` // append-only 时空，否则 RFC3339 UTC
	AttestationSHA256   string `json:"attestation_sha256"`
	ReadBackSHA256      string `json:"readback_sha256"`
	Result              string `json:"result"` // verified
}

type ArchiveFinalizationRecord struct {
	SchemaVersion             int    `json:"schema_version"` // 固定 1
	AttestationSHA256         string `json:"attestation_sha256"`
	RecordVerificationSHA256  string `json:"record_verification_sha256"`
	ManifestSHA256            string `json:"manifest_sha256"`
	TargetCount               int    `json:"target_count"`
	TargetDigest              string `json:"target_digest"`
	PreparationSHA256         string `json:"preparation_sha256"`
	ExternalMode              string `json:"external_mode"` // none 或 verified
	ReadBackSHA256            string `json:"readback_sha256"` // none 时空
	ImmutableEvidenceSHA256   string `json:"immutable_evidence_sha256"` // none 时空
	Result                    string `json:"result"` // finalized
}
```

- 所有 canonical evidence 文件使用同一有界 decoder：最大 payload 为 16 MiB、唯一 LF 不计入上限，以 cap+1 读取；先要求 `utf8.Valid`，再以显式迭代 stack 做 token traversal，`MaxNestingDepth=64`，在任意 object 层拒绝 duplicate/unknown key，并拒绝 null、BOM、CRLF、前后空白、第二顶层值、尾随 bytes、半帧及错误字段类型。decoder 只写同类型临时 DTO，完整 schema/枚举/关系校验成功后才发布，不能用 Go 递归解码器把 nesting/CPU 边界交给输入决定。所有 slice 必须编码为 non-nil array（无项时 `[]`），不能用 `null` 区分状态。
- 除 marker pending 的 R0/hash、non-performance job 的 PerformanceReportSHA256、`R0TransitionEvidence` 在 `R0==D17` 时的 DeltaSHA256，以及 ArchivePreparation/ArchiveFinalizationRecord `external_mode=none` 时成对为空的 ReadBackSHA256/ImmutableEvidenceSHA256 外，OID/SHA 分别严格为 40/64 位小写十六进制；schema/action/result/stage/prefix/job/workload/token/kind/document/retention-mode 是本节闭集枚举。普通 identity/name 字段为 1–256 个安全 ASCII bytes，单 argv/env 元素最多 4096 bytes，单 summary/provider evidence 最多 1 MiB，完整 record 最多 16 MiB；计数遵守上述非负/精确关系，整数计算 overflow 即失败。
- `jobs/<stage>/<job>.summary.json` 的 payload 恰为 `JobSummaryRecord` compact canonical JSON 加一个 LF；`SafeSummarySHA256` 只覆盖不含 LF 的 payload，因此不存在自引用。`provider/<stage>/<job>.evidence` 恰为 `ProviderEvidenceRecord` 的同种 canonical encoding，`ProviderEvidenceSHA256` 只覆盖其 payload；外层 `JobEvidenceImportRequest.Stage/Job` 必须分别逐字等于 `Summary.Stage/Name`，Provider 不虚构 stage/name 字段，而是以 `SafeSummarySHA256` 及 CIProvider、RepositoryID、WorkflowPath、WorkflowBlobOID、WorkflowDependenciesSHA256、JobID、CheckoutOID、CIRunID、RunAttempt、Result 全部逐字段等于对应 summary 与人工核验的 provider 页面来绑定同一 job。任何不一致均失败，且不能保存任意日志、header、URL query、路径或 opaque bytes。performance report 恰为 `PerformanceReportRecord` 的同种 canonical encoding；`PerformanceReportSHA256` 只覆盖该 record payload，不进入其自身。
- `revision/r0-transition.json` 恰为 `R0TransitionEvidence` 的 canonical encoding，且 D17 必须等于 `Deletion[16].Revision`、R0 必须等于 `AttestationRecord.R0`。无 diff 分支要求 D17=R0、exit 0、bytes=0、TidyDiffSHA256 等于空字节 SHA-256、DeltaSHA256 为空且 ChangedPaths=`[]`；有 diff 分支要求 D17≠R0、exit 1、bytes>0、diff digest 等于 D17 捕获 stdout、DeltaSHA256 等于 R0 单父 commit delta、ChangedPaths 按 raw bytes 升序且恰为 `go.mod`/`go.sum` 非空子集。attestation 的内嵌结构必须逐字段等于该文件，不能只保留 OID 后在 archive finalization 时丢失 tidy 来源证据。
- 每个 job 的 `CommandManifestRecord` 必须由已批准 Task 中该 job 的 exact Step manifests 与 checker 唯一 `run.go` 清单双向生成并由静态测试逐 byte 比对；Task 未批准前不能冻结 Rpre。record 用 compact JSON＋LF，CommandManifestSHA256 只覆盖 payload。生成 summary 时，checker 从实际 CommandEvidence 投影出 Step、ExecutableToken、safe Argv/Env、token/kind 和 Required package/test/ExpectedRuns，必须逐字段等于 command manifest；Executable version/digest、workspace 与 actual counts/result 作为运行证据另行保留。这样 `required` job 不能替换成 trivial pass。
- `ExpectedEvidenceManifest.Jobs` 的全局顺序固定为 rpre、d01..d17、r0、e。rpre/r0/e 各按 `core`、`final`、`commit-repoaudit`（执行 `repoaudit` profile）、`docs`、`darwin-native`、`darwin-e2e`、`linux-native`、`linux-e2e`、`windows-native`、`windows-e2e`、`performance` 恰好 11 项。每个 Dn 先为 `deletion`、`required`；D05/D14 随后加 Darwin native/e2e、Linux native/e2e，D06/D15 加 Windows native/e2e，D11–D13 加 Darwin、Linux、Windows 各 native/e2e，其他 Dn 不加 job。每项 CommandManifestSHA256 必须匹配上述已批准 manifest；ExpectedEvidenceManifest 用 compact JSON＋LF，`EvidenceManifestSHA256` 只覆盖 payload；所有目录文件集合和 `DeletionEvidence.Jobs` 必须恰为对应 subsequence。
- `Deletion` 按 sequence 1..17 恰好 17 项，Prefix/revision/parent/两条 changed path 与本节表逐项一致，其 `Jobs` 恰为 ExpectedEvidenceManifest 中对应 Dn 的连续 job subsequence。`AttestationRecord.Jobs` 不重复 Dn job，只能按 rpre 的 11 项、r0 的 11 项、e 的 11 项顺序恰好保存 33 项；二者合并后必须与 ExpectedEvidenceManifest 的全量 jobs 一一且仅一次对应。Commands 保持固定执行顺序，Env 按 key 原始 byte 升序，PathBindings 按 token raw bytes 升序且 token 唯一，Required 按 package/test 原始 byte 升序。所有 OID、parent/diff、argv/env、workspace digest、required 次数、runner/toolchain、provider/repository/workflow/job identity、run ID/attempt、零 missing/skip/excess 和 pass/exit 0 必须可由输入安全摘要独立重算。
- 每个 Job 的 safe summary 与 CI provider run/job evidence 都是 record 的必需输入。只读 CI 不被假定能签名自定义摘要：job 必须在 provider 保留的日志/summary 中逐字显示 SafeSummarySHA256、repository/workflow/job/run/attempt、checkout OID、WorkflowDependenciesSHA256 与 pass result；最终验收者从独立 provider UI/API 人工核对这些值，只能通过 evidence owner 的固定字段导入入口生成 `ProviderEvidenceRecord`，不得下载或长期保存 raw 日志摘录。owner 对字段执行 whitelist、SafeText/ASCII/长度校验并与 summary 和当前权威 R0/E checkout 中对对应 historical commit 重算的 workflow blob/dependency digest逐字段比对，再原子发布 `.evidence`；这提供当前、可复核的人工来源证据，但不宣称密码学不可伪造。若要签名 provenance 或新增 CI `id-token/attestations:write` 权限，必须另开 Spec 变更并重新审批。
- workflow/reusable workflow/action 的供应链也是证据输入。仓库固定文件 `.github/xagent-actions.lock.json` 恰为 `WorkflowDependencyLockRecord` compact canonical JSON 加一个 LF，并使用同一 16 MiB/depth-64 strict decoder拒绝 duplicate/unknown/null/BOM/CRLF/trailing bytes；它无 map，RootUses 按 SourceIdentity raw bytes 严格升序，LocalManifests 与 Entries 各按 Identity raw bytes 严格升序，每个节点的 Uses 同样按 SourceIdentity 严格升序，所有 node identity 在 local/remote/container 全集唯一、source identity 在各自父节点唯一。RootUses 必须一一覆盖根 workflow 的全部 `uses:`；每条 edge 的 TargetIdentity 必须命中唯一 local 或 remote/container 节点，所有节点必须从 RootUses 可达，缺 target、额外不可达节点、重复 edge/identity 或有向环均失败。每个 LocalManifest 记录本仓库 action/reusable workflow 的规范 repo-relative path、historical blob OID、raw manifest SHA-256及全部 nested `uses:` edge；owner 从同一 historical checkout no-follow 读取每个 blob并递归解析，实际 edge 必须逐项等于 lock，因而本地 manifest 内的 remote/container/floating nested use 不能漏过。每个远端 action/reusable workflow entry 记录人工审核过的 repository、规范 manifest path、manifest SHA-256、完整 40 位 commit 与其全部 nested `uses:` edge；container entry 固定 `sha256:<64hex>`、空 ManifestPath、ManifestSHA256 等于 digest 的 64hex且 Uses=`[]`。远端 manifest 内的相对 `./path` 解析到同一远端 repository/revision 的另一 entry；tag、branch、短 SHA、floating major、动态表达式、未锁定 edge、路径逃逸及大小写/Unicode alias 均失败。
- node identity 与 edge 目标不由 lock 作者自由命名，Kind 全集恰为 `local-action`、`local-workflow`、`remote-action`、`remote-workflow`、`container`。每个节点先构造 target frame：ASCII `xagent-workflow-target-v1` 加 NUL，再依次对 Kind、Repository、Revision、ManifestPath、ManifestSHA256 编码 `uint64 big-endian byte-length || raw-bytes`；local 节点固定 Repository=`.`、Revision=`ManifestBlobOID`，remote 节点使用 owner/repo 与 40hex commit，container 使用规范 registry/image 与 `sha256:<64hex>`。Identity 必须等于该 frame 的 SHA-256 小写 64hex，且 `(Kind,Repository,Revision,ManifestPath)` 在节点全集唯一。parser 还必须从每个实际 `Uses` 和 source context 独立派生 target descriptor并重算 TargetIdentity：job-level `./file.yml`/`.yaml` 只能解析为同一 historical tree中的 local-workflow exact file；step-level `./dir` 只能 handle-relative 解析该目录下恰好一个 `action.yml` 或 `action.yaml` 为 local-action，拒绝绝对/逃逸/symlink/reparse、双 manifest、case/Unicode alias。远端 job-level `owner/repo/.github/workflows/file@40hex`、step-level `owner/repo[/dir]@40hex`、远端 manifest 内相对路径及 `docker://image@sha256:<64hex>` 分别按 source kind与 GitHub 解析规则派生唯一 remote-workflow、remote-action 或 container descriptor；解析出的 repository/revision/path/kind 必须逐字段等于目标节点，remote action 目录也只能对应人工审核为唯一的 action.yml/action.yaml。actual `uses: ./A` 指向 B、同 path 错 kind/hash或任何无法唯一派生的 TargetIdentity 均在 closure 检查前失败。
- root/local YAML 解析的资源与语法同样封闭：每个 payload 最大 1 MiB、UTF-8、单文档、深度最多 64，遍历所有 mapping/sequence 节点并拒绝 duplicate key、anchor、alias、merge key、自定义 tag、非字符串 `uses`、表达式和尾随文档。SourceIdentity 唯一语法是 workflow 的 `jobs/<safe-job-id>/uses`、`jobs/<safe-job-id>/steps/<zero-based-decimal-index>/uses` 或 composite action 的 `runs/steps/<zero-based-decimal-index>/uses`；job id 只接受 GitHub 合法安全 ASCII，十进制 index 不带多余前导零。远端 lock 中的 SourceIdentity 也按对应 kind 使用同一语法，由人工审核与 strict decoder校验。
- lock 在 Task 审批时冻结 exact RootUses/LocalManifests/Entries、远端 commit、manifest path/hash 与 nested edge，是经人工审核的 commit-pinned 供应链契约；运行时 importer 不联网、不获取远端 action bytes，也不宣称重新证明远端服务当前内容。每个 historical checkout 只从本地 Git object database 读取 root workflow、全部 LocalManifests 与 lock file：RootWorkflowPath/RootWorkflowBlobOID 和每个 local path/blob/hash 必须等于实际 tree，解析出的 root/local `uses:` 必须逐项等于对应 edge；随后只对远端 lock 的严格 schema、排序、引用与 reachable closure 做离线重建。`WorkflowDependenciesSHA256` 的唯一输入是 ASCII `xagent-workflow-dependencies-v1` 加 NUL，再依次对 root workflow path、其 40hex blob OID、固定 lock path、lock 的 40hex blob OID及不含 LF 的 canonical lock payload编码 `uint64 big-endian byte-length || raw-bytes`；SHA-256 覆盖完整 frame。这样 digest 同时绑定 historical workflow blob、全部 local blob、lock blob/OID 与完整 graph，repoaudit、summary、provider evidence 和 importer 都核对同一值。
- ephemeral CI runner 只允许上传名为 `xagent-safe-evidence-<stage>-<job>` 的传输 artifact，内容恰为该 job 的单一 summary；performance job 再含对应单一 report。两者都是已脱敏、受上述 1 MiB/typed schema 约束的安全文件，禁止 raw stdout/stderr、coverage、binary、fixture、path、manifest、用户数据或凭据。上传不增加 repository/attestation/id-token 写权限；artifact 只负责搬运，不是权威证明。最终验收者在 provider UI/API 按 run/job/attempt 人工取得并核对安全 artifact 与页面字段，拒绝合并下载和额外文件，再把仅含 typed safe fields 的 `JobEvidenceImportRequest` 交给下述唯一导入 action；evidence owner 本身不联网、不读取 provider 凭据或 raw log。
- performance Job 的 `Summary.PerformanceReportSHA256` 必须非空且 `Summary.Performance` 按四个 Workload 固定顺序恰好四项；它必须逐字段等于由对应 report bytes 重建出的 `PerformanceReportRecord.Metrics`，report 的 Audit/Baseline/CurrentRevision 遵守三值模型。每项 warmup=5、scored=30、两个 median 均为正；30 个计分时长按无符号纳秒值非降序排列为 `s[0]..s[29]`，`MedianNanos=s[14]+(s[15]-s[14])/2`，固定向下取整到整纳秒。唯一通过门禁是精确关系 `CurrentMedianNanos*5 <= BaselineMedianNanos*6`：实现以 `math/bits.Mul64` 分别形成两个 128 位乘积并按高、低 limb 字典序比较，任一算术或输入异常 fail closed。`RatioPPM=floor(CurrentMedianNanos*1000000/BaselineMedianNanos)` 仍以不溢出的整数运算重算，只用于展示和逐字段一致性，不得作为通过依据；无法表示为 `uint64` 时失败。summary/report 重建、performance profile、attestation `generate` 与 `verify-record` 都必须各自从原字段重算同一精确门禁，不能信任已存的 `RatioPPM` 或先前 pass。非 performance Job 的 report digest 为空且 Performance 是 non-nil 空数组。
- evidence owner 在 private control root 中使用唯一固定 sibling regular file `.xagent-evidence.lease` 协调所有 ledger 访问，并以唯一固定 empty regular file `.xagent-evidence.active` 证明 ledger 已越过 bootstrap；二者都不位于 `XAGENT_EVIDENCE_DIR`、不属于 archive。bootstrap 只允许先绝对/no-follow 打开并验证 private control root 的精确 POSIX 0700 owner 或 Windows 私有 DACL，保存 root handle identity；为定位 lease 所需的这一步不得读取/创建 evidence dir 内容，取得锁后必须从同一 handle 再做完整复验。control root 的合法启动组合只有：A=`lease/dir/active 全缺且无其他 child`（首次），B=`仅 lease 存在`（创建 lease 后崩溃），C=`lease+空 evidence dir 存在而 active 缺失`（创建 dir 后崩溃），D=`lease+evidence dir+active 存在`（正常）。lease 缺失但 dir/active 任一存在、active 存在但 dir 缺失、C 的 dir 非空、额外 child或类型/identity 漂移一律永久 fail closed，绝不重建 lease/ledger。
- A 从已打开 control-root handle 以 `O_NOFOLLOW|O_CLOEXEC|O_CREAT|O_EXCL`/Windows CreateNew 创建精确 0600/private-DACL、empty regular lease；并发得到 EEXIST 时只可回到同一 handle 重新枚举后走 existing 分支。B/C/D 以 `O_NOFOLLOW|O_CLOEXEC` 且不带 create/truncate/Windows OpenExisting 打开。两条分支随后必须汇合：先复验 regular/empty/mode/owner/nlink/file ID，明确设置 close-on-exec/Windows non-inheritable，再在同一唯一、不可复制的 owner handle 上取得排他锁。POSIX 固定 `flock(LOCK_EX)`；Windows 固定 OVERLAPPED Offset/OffsetHigh=`0/0`、length low/high=`1/0`（允许越过空文件 EOF）、flags 只含 `LOCKFILE_EXCLUSIVE_LOCK`而不含 `LOCKFILE_FAIL_IMMEDIATELY`，以阻塞方式等待同一 `[0,1)` byte range，`ERROR_IO_PENDING` 只可等待该同一 operation完成，其他 error 或意外成功范围均失败；释放固定用同一 OVERLAPPED/range 的 `UnlockFileEx` 并要求成功。跨进程测试必须证明第二 owner 在第一 owner 释放前不能进入 ledger。
- 取得锁后先无条件 fsync/FlushFileBuffers lease，再同步 control root，并从同一 handles 复验 control-root/lease identity与 A–D 组合；这一步也覆盖 EEXIST 竞争者先于 creator 初始 flush 取得锁的情况。A/B 才可创建精确私有空 evidence dir：创建后先打开并复验 directory handle、同步空目录本身，再同步 control root以持久化 dentry，随后重新枚举并要求仍为 C；C 也必须先复验目录为空并重复 directory/control-root 同步。只有完成这些步骤，才以 no-follow/create-exclusive 创建精确 POSIX 0400/Windows只读 DACL的 empty active sentinel，完整复验并同步 sentinel file，再同步 control root，最后从同一 handles 重新枚举并要求稳定 D；只有此时才能创建首个 ledger child。D 先复验 active exact type/mode/DACL/empty/file identity与 evidence dir identity再打开既有 ledger。任一平台不能提供上述 directory metadata durable flush 时 fail closed。
- lease handle 不得进入 `ExtraFiles`/STARTUPINFO handle list，child launch 在 fork/posix_spawn/Windows 边界显式关闭全部非白名单 handle，因此 Go child、孙进程或错误路径不能继承/dup 它；释放前先完成最后同步与身份复验，再由唯一 owner unlock/close。这样 owner crash 关闭唯一 open-file-description/handle并由 OS 释放锁，archive 根只读化也不影响 phase④重新打开同一 sibling lease；active 存在而 dir 消失则可区分为数据丢失而不会误当 bootstrap。
- 所有会观察或变更 ledger 的入口——preservation freeze及其 manifest recovery、remove-index、verify-immediate、verify-final 与结果发布、R0 transition 发布、job import、marker render、generate、verify-record、verify-readback、finalize-archive、verify-archive——都必须从上述 bootstrap 后、首次读取 evidence root 前到最后一次 fsync/FlushFileBuffers 与权限复验后持有这一个排他 lease；remove-index 还必须跨 manifest 首读、精确 index mutation、post-check 与 index/parent fsync 全程持锁。未取得、identity 漂移或锁实现不受当前平台支持时不得读取、创建、发布、删除或收紧 ledger。外部不合作进程不在威胁模型内，但其造成的 identity/content 漂移仍由每次 handle 复验拒绝。
- 在同一 lease 内执行统一 phase gate：`archive-prepared.json` 或其固定 publish temp 一旦出现，除 `finalize-archive` 与只在完整 phase④运行的 `verify-archive` 外，任何入口都立即失败且不得写 ledger；`archive-finalization.json` 或其 temp 出现后同样只允许这两个入口。finalize 从 phase②首次全量复验开始，跨 manifest 删除、目录同步、phase③复验与 finalization 发布一直持锁，其他 import/readback/validator 不可能在删除窗口插入状态。
- ledger 中每个生成或导入文件都使用同一 crash-safe publish：在目标同目录以固定 `.<basename>.publish.tmp`、no-follow、create-exclusive 和私有权限创建 regular temp，完整写入后从同一 handle 重新校验 canonical bytes/长度/digest，fsync/FlushFileBuffers 文件，再以 no-replace atomic publish 到唯一 final name，最后 fsync/等价 flush 父目录。不得直接 truncate final、跨目录 rename、覆盖既有 final 或在 publish 前删除输入。`preservation/m0.manifest`、CI summary/performance report 的生成、artifact 导入、ProviderEvidenceRecord、preservation result、R0 transition、attestation/verification/preparation/finalization 及 optional readback/receipt 都服从此协议；manifest 另叠加 C16 的 pre-change 恢复前提。
- 原子 publish 的恢复状态也是闭集：final 已存在且逐 byte 等于本次期望内容时视为幂等成功，并在安全核验后删除同名完整或截断 temp；final 不存在而 temp 完整且逐 byte 等于期望内容时继续 no-replace publish；final 不存在且 temp 截断或无法形成任何合法 canonical 值时，只按精确 temp 名清理后重建；完整合法但不等于期望内容的 temp、已存在但内容/类型/权限/digest 不同的 final、第二 temp或额外文件一律 fail closed。任何清理都先从已打开父目录复验 regular file identity，不递归、不跟随链接，也不由 glob 发现目标。prepared/finalization 与 manifest 的 phase-aware 规则优先于本通则，manifest 已缺失时绝不把仅存的 prepared temp晋升为 published preparation。
- attestation profile 的 action 闭集为 `record-r0-transition`、`import-job-evidence`、`render-evidence-markers`、`generate`、`verify-record`、`finalize-archive`、`verify-archive` 与可选的 `verify-readback`；exact argv 只把 `--attestation-action` 设为对应值，并固定 `--profile=attestation --audit-source=commit --revision=<当前权威 OID>`，不接受 stage/job/path/URL/credential 等附加 argv。record 与 render 只允许 R0，import 按下文允许 R0 或 E，`generate`、`verify-record`、`verify-readback`、`finalize-archive`、`verify-archive` 全部只允许 E。共同安全文件集由 ExpectedEvidenceManifest 展开为每个 `jobs/<stage>/<job>.summary.json`、`provider/<stage>/<job>.evidence`，另含 `revision/r0-transition.json`、两份 preservation result 与 `performance/{rpre,r0,e}/performance.report.json`；不使用 glob。
- `record-r0-transition` 必须是 R0 pending-marker fresh checkout 中任何 pre-E import 之前的唯一 producer，stdin 必须立即 EOF。它用批准 Task 冻结的同一 Go executable、只读 module cache与离线 env在私有 task source 中重新观察 D17；stdout/stderr 都以 1 MiB cap+1 有界采集，超限即终止进程树并失败。若当前 R0 即 D17，要求 tidy exit 0且 stdout/stderr 为空；若 R0 是 D17 的唯一 child，则要求其 delta 只含 `go.mod`/`go.sum` 非空子集，在 D17 运行 tidy 得到 exit 1、stderr 空及可严格解析的完整非空 stdout，逐 byte应用一次后得到的 tree 必须等于 R0，再在 R0 运行得到 exit 0且 stdout/stderr 为空。action 从这些即时观察与 Git single-parent delta 唯一构造 `R0TransitionEvidence`，原始 diff 只存在于有界 task temp/内存，完成后精确清理，不进入 ledger、stdout或错误；`revision/r0-transition.json` 不存在时原子发布，正确 final/temp 崩溃状态按通则补齐或幂等成功，任何不一致既有状态失败。
- `import-job-evidence` 从 stdin 只读恰好一个 canonical `JobEvidenceImportRequest` 加 LF 后 EOF，使用同一 16 MiB/depth-64 decoder；Stage/Job 必须在 ExpectedEvidenceManifest 中唯一且分别等于 Summary.Stage/Name，Provider 的 SafeSummarySHA256 必须等于重编码 Summary payload 的 SHA-256，其余共有 CI/repository/workflow/dependencies/job/revision/run/attempt/result 字段逐项相等；JobSummary.Result 及每个 Command.Result 都只能为 pass。非 performance job 要求 PerformanceReports=`[]` 且 summary report digest 为空；performance job 要求恰好一项 report并逐字段/digest重建通过。action 在当前 fresh checkout 从每个声明的 historical CheckoutOID 读取 root workflow/lock blobs并按上述离线规则重算供应链 digest，再按统一 publish 协议幂等写 summary、可选 report和 provider record；若三文件组在崩溃后仅发布正确子集，重跑只补齐缺项，任何已存在不一致项失败。该 action 不接受 raw artifact/log bytes，不访问网络，也不能覆盖或选择 ledger 路径。
- import 在首次或恢复性 publish 前必须先验证所有当时已知的不可变关系。R0 入口从当前 commit 本地对象库重建唯一 Rpre→D01…D17→R0 stage/OID 表、17 个单父 exact delta 与 canonical `R0TransitionEvidence`；E 入口还重建 `parent(E)=R0`、三份 marker pass payload和 R0..E 仅三条 marker变化。请求的 Summary/Provider CheckoutOID、每个 Command 的 AuditSource/Revision 及 Before/After.HeadOID 必须等于该 stage 唯一 OID，Stage/Job、CommandManifestSHA256、runner OS/arch 与 job/platform组合必须等于 ExpectedEvidenceManifest 和批准 Task 的唯一映射。任一 ancestry、delta、marker、transition、workflow lock 或映射不符都在写入任何 final/temp 前失败，不能先永久导入再留给 generate 才发现。
- import 的 revision/stage 只有两个合法阶段。pre-E 阶段必须在三份 evidence marker 均为 canonical pending 的 R0 fresh checkout，以 `--revision=<R0>` 只导入 rpre、d01..d17 与 r0 jobs。所有 pre-E 安全文件及 `after_r0` 到齐后，唯一 `render-evidence-markers` action 才可运行：它在同一 lease 下逐项重验全集，计算 11 个 R0 JobEvidence、R0EvidenceSHA256 与 AC1..AC38 hashes，不写 ledger/worktree，只向 stdout 输出恰好一个 canonical `EvidenceMarkerRenderResult` payload加 LF 后 EOF，stderr 只能是安全错误码。Documents 必须按 readme、docs-index、checklist 恰好三项且为上文唯一 pass 形态；Task 冻结的无自由参数 updater 只从该已解码 DTO 逐字段 canonical 重编码三项，分别替换三份文档唯一完整 pending block，不能读取 stdout 文本片段、修改其他 bytes或自行计算 hash，随后提交得到 E。post-E 阶段必须在三份 marker 均为 canonical pass、`parent(E)=R0` 且 R0..E 只含 marker变化的 E fresh checkout，以 `--revision=<E>` 只导入 e jobs；它先逐 byte 复验全部 pre-E files，不得改写或补造 pre-E stage。R0 与 E 从批准 Task/`run.go` 重建的 CommandManifest/ExpectedEvidenceManifest payload 必须逐 byte 相同，E 只能新增自身 stage evidence与 `after_e`；未知/混合 stage、在 R0 导入 e、在 E 导入 pre-E 或另一 revision 均失败。
- generate 要求 `preservation/m0.manifest` 存在，verification/preparation/finalization/optional external final 或 temp 全部不存在；`attestation.json` 不存在时按统一协议发布，从 final 已完成并 fsync 后崩溃重跑时则只接受它逐 byte 等于本次从全部安全 summaries、provider evidence、R0 transition、preservation results及同一 handle raw manifest 重建的唯一期望 record，并在全量复验后幂等成功。其他已存在内容、类型、权限或 temp 状态按统一恢复规则失败；它始终重算 Rpre→D01–D17→R0→E、D17→R0 tidy/delta 与所有 digest，raw manifest path/identity 不进入输出。
- verify-record 要求 manifest 与有效 attestation 存在，preparation/finalization/optional external final 或 temp 全部不存在；`record-verification.json` 不存在时原子发布，若 exact final 已发布后崩溃则重建同一期望 payload并在逐 byte一致时幂等成功，不一致或其他 phase 状态失败。它不改写 attestation，在 E fresh checkout 重验 canonical bytes、全部链、结果和 digest。generate＋verify-record＋provider 页面人工核验构成 Spec AC1/AC33 的必需证据。
- `verify-readback` 不联网且除上述固定 profile/revision/action 外不接受 path、URL、provider 或 credential argv；它只在有效 record-verification 已存在、preparation/finalization final 与 temp 全部不存在时运行。stdin 的唯一 framing 是先一个 canonical `AttestationRecord` payload加 LF，再一个 canonical `ImmutableReceiptEvidence` payload加 LF，随后 EOF；action-level framer 分别施加 16 MiB、UTF-8、depth-64、duplicate/unknown/null 拒绝规则，不能把第二 frame 当尾随值。第一 frame 必须逐 byte等于 ledger canonical attestation，receipt 的 AttestationSHA256/ReadBackSHA256 必须分别等于原 record与第一 frame payload digest，且只含上表固定安全字段，不能保存 provider raw response、URL query、header、凭据或日志。两份 final 均不存在时按统一协议顺序发布 `attestation.readback.json` 与 `immutable-receipt.evidence`；崩溃留下正确单项或两项时只补齐缺项/幂等成功，任何既有不一致项失败。VerifiedAtUTC/RetainUntilUTC 只接受规范 UTC `YYYY-MM-DDTHH:MM:SSZ`；`retention_mode=compliance` 要求 `RetainUntilUTC-VerifiedAtUTC >= 365*24h` 且验证时受控时钟不早于 VerifiedAtUTC，`append-only` 要求 RetainUntilUTC 为空并由人工验证公开永久日志。后续 verify-archive 重算记录内的时间差与格式，不伪称仍剩余 365 天；未选择外存时二者必须都不存在。
- `finalize-archive` 在上述全局 lease 和同一 evidence-root owner handle 下只接受四个 phase：① manifest 有、prepared 无、finalization 无；② manifest 有、prepared 有、finalization 无；③ manifest 无、prepared 有、finalization 无；④ manifest 无、prepared 有、finalization 有。manifest 与 finalization 同时存在、manifest/prepared 同时缺失、finalization 存在但 prepared 缺失及任何其他组合均 fail closed。prepared temp 只可在 phase①且 manifest 仍有效时按通则续发，manifest 已缺失而只有 prepared temp 时不可恢复；finalization temp 只可在 phase③且 published prepared 有效时续发。任一完整 temp 内容不等于该 phase 从权威输入重建的期望 payload 时失败。
- phase①先运行与 verify-record 等价的纯只读全量 validator：展开 ExpectedEvidenceManifest，逐项复验全部 summary/provider/performance/R0 transition/preservation、attestation、record-verification、workflow/deletion/revision/digest、optional pair、目录闭集且无其他 temp/文件，并从同一 handle 重算 raw manifest；成功后把两份 preservation result 已一致的 ManifestSHA256/TargetCount/TargetDigest 全部写入 `ArchivePreparationRecord` 并原子发布 `archive-prepared.json`，转入 phase②。phase②在删除前必须重新运行同一全量 validator并证明 preparation 逐字段等于本次重建结果，任一不一致都在 manifest 尚存时停止；随后才按精确 handle 删除唯一 manifest并 fsync/flush `preservation` 目录，转入 phase③。
- phase③虽然不能再读取 raw manifest，仍必须对其余完整 archive 重跑同一 validator，并要求 attestation、两份 preservation result、record-verification 与 preparation 中的 ManifestSHA256/TargetCount/TargetDigest 全部一致，同时复验全部其他 bytes、optional pair和目录闭集；任一不一致为不可恢复失败，不得发布 finalization。验证通过后以 PreparationSHA256 绑定 preparation payload，原子发布 `archive-finalization.json`，转入 phase④。phase④只有在 manifest 精确缺失、prepared/finalization 与全 archive 均重新验证正确时才视为幂等内容成功，并继续/复验下述只读化；绝不能重建 manifest、猜测其 SHA 或把不可能状态当成功。
- finalization record 发布后，owner 以 child files→最深目录→evidence root 的固定顺序幂等只读化：POSIX 所有 canonical regular 文件精确 0400、目录精确 0500 且 owner 不变；Windows DACL 只授予当前用户与必要系统主体 read/traverse/read-control，并拒绝 write-data、append、delete 与 delete-child，owner 仅保留恢复 ACL 所需控制权。每个转换从同一 handle 回读验证并同步目录元数据，根最后转换；中断可重复执行。`verify-archive` 要求 finalization 存在、manifest 精确缺失及全部 canonical 文件/optional pair 状态精确，重新校验 bytes/digest/权限；若内容均已 final 且只读化尚未完成，它唯一允许的写操作是继续上述权限收紧并复验，不能创建/改写 record 或删除其他文件。
- AttestationSHA256、RecordVerificationSHA256、PreparationSHA256 及 optional readback/immutable receipt SHA 均覆盖对应 canonical payload bytes（排除唯一 LF）。ArchivePreparationRecord 与 ArchiveFinalizationRecord 的 ManifestSHA256/TargetCount/TargetDigest 必须逐字段相同且 TargetCount=35；`external_mode=none` 均要求两个 optional SHA 为空且文件缺失，`verified` 要求二者为 64 位 digest、文件存在且已通过 verify-readback；finalization 的 AttestationSHA256、RecordVerificationSHA256、manifest 三字段及全部 external 对应字段还必须逐字等于 preparation。preparation/finalization record 都不含自身 digest。
- C17 明确以此取代 C16 把外部不可变存储作为必过条件的表述：WORM/object-lock/公开 append-only transparency store 是**可选增强**，不是 AC1/AC33 或 checklist 通过前提，因此不改变已批准 Spec。系统不默认选择供应商、不提升 CI 权限、不读取外部凭据，也不自动联网发布。
- 用户另行选择可选外存时，上传 bundle 必须同时含 `attestation.json` 与所有 canonical ProviderEvidenceRecord，不能只留会失去来源材料的 digest；object-lock 必须为 compliance/WORM 且验收时剩余 retention 不少于 365 天，或使用可公开独立核验的永久 append-only log。取得 provider immutable object/version evidence 后独立下载 record 为 `attestation.readback.json`，只把白名单安全字段导入 `ImmutableReceiptEvidence`；`verify-readback` 自动证明 bytes/digest/record，人工另验外存真实性与 lock/retention。未选择外存不得把本地 record 称为 external attestation，但不影响 Spec 验收。

#### Preservation manifest 的跨任务唯一例外

跨任务 evidence ledger 是“任务结束清理 `XAGENT_TASK_TMP`”规则的唯一例外。它位于工作区外、由同一 evidence owner 创建和持有的私有 `XAGENT_EVIDENCE_DIR`，与 task/workspace/source roots 遵守上文互不包含/alias 规则。private control root、sibling lease 与 active sentinel 同属这一跨任务生命周期例外，但不属于 archive 内容。ledger 内容闭集仅为固定 `preservation/m0.manifest`、`preservation/after_r0.json` 与 `after_e.json`、`revision/r0-transition.json`、Rpre/D01–D17/R0/E 的 safe job summary 与 canonical provider evidence、三份 performance report、canonical attestation/verification/preparation/finalization records，以及用户选择可选外存时的 canonical readback/receipt；publish 进行中只额外允许每个目标至多一个固定同目录 temp。不得混入完整测试 stdout、cache、二进制、任意 provider log 或用户内容。manifest、`after_r0`、control root、lease、active sentinel 和已收集安全证据在产生任务结束时不得删除、移动或改写；其他 task 临时产物仍按精确路径清理。

`Preservation` 必须按 `after_r0`→`after_e` 恰好两项，二者 `Mode=verify-final`、`ManifestSHA256`、`TargetCount=35` 和批准的 `TargetDigest` 逐字相同，且各自 canonical safe argv、exit 0/result pass 独立存在。E 第二次 `verify-final`、generate、verify-record 与 provider 页面人工核验全部通过后，只能由 finalize-archive 的 prepared→manifest-delete→finalization 状态机删除含用户对象 identity 的 manifest；安全 summaries、provider evidence、R0 transition、performance reports、两份 preservation result、attestation、verification、preparation 与 finalization records 组成 durable evidence archive，并由 verify-archive 复验。archive 保留在 `XAGENT_EVIDENCE_DIR`，向用户报告实际位置和 record digest但不把路径写入 E。C17 不提供 archive、evidence dir、active sentinel、lease 或 control root 的删除/重命名 action；当前实现即使收到用户精确删除请求也必须先另开 Spec，定义持同一 lease 的 tombstone、等待者排空、children-first 删除与 lease-last 不可重建状态机并重新审批，不能直接 unlink 后复用同名 lock inode。失败时保留 ledger并只报告安全摘要，不能递归清理父目录或重建 manifest。

C17 明确授权新增或调整 `internal/e2e/perfprotocol/{protocol,codec}.go`、`internal/repoaudit/{git_source,workspace,evidence}_{unix,windows,unsupported}.go`、`internal/repoaudit/{git_source,workspace,deletion_candidates,ci_workflow,attestation,evidence}.go`、`cmd/xagent-check/{main,run,evidence}.go`、`.github/workflows/ci.yml`、`.github/xagent-actions.lock.json` 及对应测试/fixture。平台文件只实现本节已批准的 handle/file-ID/DACL/no-follow/atomic-publish/lease 边界，不新增业务能力。C17 不授权实现外部存储客户端、增加 CI 写权限、运行时联网重取远端依赖或接触用户凭据；其余 production closed-world 约束不变。四份规格全部批准前仍禁止实现写入。

### C18：ContextManager 安全结果扇出与唯一 Artifact 所有权（已批准，2026-08-03）

T3.19 实施前复审发现：ContextManager 仍按 `context.tool_result_threshold_chars`/`context.tool_results_threshold_chars` 把 Conversation 中的安全文本重新写入 `context_blobs`，自行生成 `artifact.Ref`；与此同时，生产组装根尚未把 T2.18 的 Capture 接入全部工具结果生产者，Orchestrator 仍会在安全访问器为空时读取 `tool.Result` 的兼容公开字段。该状态形成第二套外置 owner，绕过 Artifact Store 的首字节预算、容量、保留期和原子 Commit，也无法证明 Provider、用户视图与持久化视图来自同一次 ResultFactory 构造。C18 只封闭 F7、F10、F18、F20 已批准的边界，不新增工具、协议或业务行为；未被本节改写的 C1–C17 条款继续有效。

#### 唯一数据流与所有权

```text
工具输出第一字节
  → tool.Capture（tool.inline_output_bytes / tool.capture_bytes）
    → 阈值内：Abort staging，只保留有界预览
    → 超阈值或已接受正字节后不完整：artifact.Store.Commit，得到 opaque artifact.Ref
  → 唯一 ResultFactory 一次构造四个安全视图
  → ContextManager.ProjectToolResult 只调用四个访问器并校验一致性
    ├── ModelContent       → 当前 run 的 Provider 临时投影
    ├── UserView          → Event / TUI / Hook
    ├── PersistedContent  → Conversation / JSONL
    └── OutputMeta        → opaque Ref / 截断元数据
```

- `artifact.Store` 是完整工具输出的唯一 owner。T3.19 只封闭 ResultFactory/安全投影契约，T3.19a 只封闭 `tool.Capture` 窄注入依赖与安全 candidate path，并在测试中注入真实或 poison Store；不得提前创建第二个生产组装入口。C8 artifact root 的解析和真实 Store 的唯一生产创建仍只属于既定 T4.24/T4.25b–e，最终组装根每次工具调用只通过该工具的窄构造闭包签发新的 Capture/Writer，不新增通用 CaptureFactory 类型或可替换注册表。ContextManager、Orchestrator、Conversation、TUI、Hook、Memory 和 Provider 均不能取得 Store、Writer、Reader、真实路径或 artifact payload。
- `tool.inline_output_bytes` 是唯一内联阈值；`tool.capture_bytes` 是唯一单次原始采集上限。两者只在 Capture 构造时决定 Abort/Commit。ContextManager 只接收同一 Resolve 值用于 fail-closed 一致性校验，不能再次截断、写文件或生成 Ref。既有 `context.tool_result_threshold_chars`、`context.tool_results_threshold_chars` 与 `context.preview_chars` 不再参与生产外置决策；兼容配置输入仍按 C15 严格解析，但不得形成第二条运行时路径。
- 删除 ContextManager 的 `dataDir`、`context_blobs`、`os`/`filepath` 文件能力、自建 SHA/Ref、外置 payload codec 和扫描候选逻辑。它不得依赖 `artifact.Store` 或任何含 Open/Read/Write 方法的接口，也不得通过 Ref 推导路径。

#### ContextManager 唯一安全结果入口

```go
package contextmgr

type ManagerOptions struct {
	Context           config.ContextConfig
	InlineOutputBytes int64
	RuntimeRedactor   *redact.RuntimeRedactor
}

type ToolResultProjection struct {
	ModelContent      redact.SafeText
	UserView          tool.UserView
	PersistedContent  redact.SafeText
	OutputMeta        tool.OutputMeta
}

func New(provider provider.Provider, options ManagerOptions) (*Manager, error)
func (m *Manager) ProjectToolResult(result tool.Result) (ToolResultProjection, error)
func (m *Manager) UpdateUsage(conversation *conversation.Conversation, usage provider.Usage) error
```

为使 ContextManager 能在不接触 payload 的前提下验证 Capture/Ref 关系，C18 在既有 `tool.OutputMeta` 闭集末尾增加 `CapturedBytes int64`；ResultFactoryInput 同步接收该值。它是 Capture 实际接受的原始字节数，不是预览长度、rune 数或 JSON 长度。

- `New` 是唯一构造入口；拒绝 nil RuntimeRedactor、无效 Context 数值和不在 C15 `1..1 MiB` 闭区间内的 `InlineOutputBytes`。Context 启用时 nil Provider 非法；显式禁用时允许 nil Provider，但 ProjectToolResult 和 UpdateUsage 仍可用。不保留旧 `(provider, dataDir, ContextConfig)` 构造器、variadic fallback、默认阈值或第二 options 类型。
- `ProjectToolResult` 必须以本节精确签名存在，并且是 ContextManager 全部 production files 中唯一接收或持有 `tool.Result` 的入口；字段、接口、容器、其他函数/方法/闭包参数和返回值均不得持有该类型。方法体只允许对同一个参数标识符 `result` 形成 `result.ModelContent()`、`result.UserView()`、`result.PersistedContent()`、`result.OutputMeta()` 四个直接零参数调用且各恰好一次，随后只处理返回的不可变安全值；禁止别名、取址、装箱为 interface、传给 helper、method expression、反射或 marshal，也禁止读取或回退到 `Result.CallID/Name/Summary/Content/Data/Error/Truncated` 等兼容字段、解析 PersistedContent 恢复预览/路径。除 `internal/tool/result.go` 的访问器定义、`result_factory.go` 的唯一构造和测试外，repo production 中四访问器的唯一调用点就是该方法，Orchestrator 只能消费 `ToolResultProjection`。
- UserView 与 OutputMeta 的 Artifact、Truncated、TruncationReason 必须逐字段相同；`CapturedBytes` 只存在于 OutputMeta 且非负。非 nil Ref 的 `ID` 恰为 64 位小写十六进制且不得含路径语义，`Bytes=OutputMeta.CapturedBytes`、`CreatedAt` 非零、`Available=true`；UserView/OutputMeta 两份 Ref 的 `ID`、`Bytes`、`CreatedAt`、`Available`、`Complete` 必须全部相同。有 Ref 时只允许 `Truncated=true`：`Complete=true` 只对应 `inline_preview_limit`，`Complete=false` 只对应 `capture_hard_limit`、`artifact_hard_limit`、`capture_write_failure` 或 `capture_canceled`。无 Ref 时只允许 `CapturedBytes <= InlineOutputBytes`、`Truncated=false` 且空 TruncationReason。零字节 after-start write failure 及 Commit failure 不发布 Capture 的不一致元数据或 unavailable Ref；caller 抛弃该 CaptureResult，规范化为 `CapturedBytes=0`、无 Ref、非截断、空 TruncationReason 的有界 synthetic error Result，并只通过 SafeError 表达失败。UserView.Preview 必须是有效 UTF-8 且 bytes 不超过 `InlineOutputBytes`。任一空视图、无效状态、不一致、超限、伪 Ref 或 CancelledBeforeStart 结果均 fail closed，且不发布部分 projection。
- `UpdateUsage` 只接收 Provider 发布的最终 usage 值快照，拒绝负值和算术溢出；它只更新 Conversation 的数值元数据，不读取 Provider response、工具结果或 artifact。T3.26 仍负责最终 usage 的唯一有序提交时机。

#### ResultFactory 闭环与 Orchestrator 扇出

- T3.19 先完成 Result/ResultFactory/OutputMeta 的包内 seal、ContextManager 自身的无文件 capability、唯一 ProjectToolResult 和 usage 值边界；紧邻新增 T3.19a，只消费该已封闭结果契约并完成 producer safe candidate、Orchestrator 安全 projection 通道和临时 ModelContent 槽。二者不创建 Artifact Store、不解析 artifact root，也不依赖任何 T4 任务。
- 内置工具、MCP、Hook synthetic result、拒绝、未知工具、参数错误、超时和取消后结果的目标状态是全部通过唯一 ResultFactory 构造；`CancelledBeforeStart` 继续没有 Result。T3.19a 先封闭构造和消费 API，并为尚未由最终 Assembly 注入的旧生产入口保留一个明确、不可进入 ContextManager 的迁移 adapter；迁移 adapter 不得写 Conversation v2 或伪造 Ref，其删除截止点固定为 T4.29a。
- 字节流工具通过各自已注入的窄 Capture 构造闭包在执行前取得 Capture；Read/Grep/Glob/Bash/MCP 等用户数据输出从第一字节进入 Capture，不得完整读入内存后再补喂。Finish 后只把 preview、opaque Ref、CapturedBytes 与截断原因交给 ResultFactory。T3.19/T3.19a 的包级与编排测试使用 T1.25–T1.28 的真实测试 Store 封闭该链；非流式 synthetic result 只能提供有界输入且不得携带 Ref。
- Orchestrator 的安全路径把 Result 先交给 `ContextManager.ProjectToolResult`，成功后才单点扇出。Event/TUI/Hook 只用 UserView；Conversation 的 tool-result Content/ToolState 只保存 PersistedContent、UserView 的安全状态/摘要/错误及 OutputMeta 的 opaque Ref。
- ModelContent 槽由单次 `executionState` 独占，不使用 Orchestrator 全局 map。每项 key 为当前 conversation ID、request generation、iteration、run 内单调 ordinal 和 append 后的 message index；call ID 只作一致性字段，不能单独定位。每项 ModelContent 受 `session.max_record_bytes` 限制，槽累计受 `session.max_session_bytes` 限制，溢出或无法唯一映射时在 Provider/Store 副作用前失败。
- Provider mapper 只在同一 executionState、同一 iteration 且 message index/ordinal 全部匹配时用临时 ModelContent 覆盖该条 PersistedContent；其余历史只使用 Conversation 中的 PersistedContent。槽在下一次 Provider request 完成规范化、计量并交给 StreamChat 后立即清除，run 的所有同步错误、Provider 错误、保存失败、取消、会话切换与正常出口再兜底清除。若 Prepare/摘要在交付前删除或移动对应消息，则删除已淘汰槽并按 survivor 原 index→新 index 的唯一映射 rebase；不能唯一映射就 fail closed。
- T4.24 仍是 artifact root 的唯一解析点；T4.25b 创建并登记唯一 Store owner；T4.25c 用唯一 RuntimeRedactor 创建唯一 ResultFactory，并从该 Store/最终 limits 创建各内置 producer 的窄 Capture 闭包；T4.25d 向 MCP 注入同一 ResultFactory 与 Capture 闭包，向纯 synthetic Hook adapter 只注入同一 ResultFactory；T4.25e 注入 ContextManager 并使上述 Orchestrator 安全路径成为唯一生产扇出。T4.29a 原子切换 `main.go` 后，才删除 `tool.Result` 公开 payload、直接 composite literal、`Success`/`Failure` 兼容入口、raw fallback、旧 Registry/Bash 构造入口与全部迁移 adapter。协议兼容只能保留包内 wire DTO。

#### 验证与授权文件面

- `TestContextManagerNeverReadsRawArtifact` 同时验证 capability shape、生产 AST 和 canary：Manager 不含 path/Store/Reader/Writer；所有 ContextManager production/build variant 不导入 `artifact`、`os`、`filepath`、`io/fs`、`syscall`，没有 `context_blobs` 或 Open/Create/Read/Write/Rename/Walk 调用。生产 ContextManager 恰有一个接收 `tool.Result` 的声明，签名必须是上述 ProjectToolResult；方法体对同一未复制、未取址、未转 interface 的参数直接调用四个访问器各一次，禁止 helper、method expression、reflection 或 marshal。测试提供一旦 Open 即 panic 的 Store，但只把 opaque Ref 交给 ResultFactory；运行 projection、摘要、保存和重载后 open count 必须为零，所有安全渠道均无 raw/path canary。
- 阈值矩阵固定 `InlineOutputBytes=8`：无 Ref 正例至少覆盖 ASCII `1234567`/`12345678` 的 `CapturedBytes=7/8`，以及 UTF-8 `你a🙂` 的 8 bytes，三者均非截断、空原因；ASCII `123456789` 与 UTF-8 `你ab🙂` 均为 9 bytes，只要无 Ref 就必须失败，任何无 Ref且 `Truncated=true` 也必须失败。有 Ref 正例覆盖 preview 0..8 bytes、`CapturedBytes>8` 的完整输出与 after-start 已接受正字节的不完整输出；逐字段变异 `ID`、`Bytes`、`CreatedAt`、`Available`、`Complete`、Truncated、TruncationReason 及 `OutputMeta.CapturedBytes`，证明两份 Ref 任一不一致、`Ref.Bytes != OutputMeta.CapturedBytes`、preview 9 bytes、有 Ref但非截断或 unavailable Ref 都失败。完整 Ref 覆盖 `inline_preview_limit`，不完整 Ref 覆盖 `capture_hard_limit`、`artifact_hard_limit`、`capture_write_failure`、`capture_canceled`；零字节 after-start write failure 按上述 synthetic error 规范化，Commit failure 不发布原 CaptureResult。旧 Context 三阈值以 `(tool_result, tool_results, preview)=(1,1,1)` 与 `(1048576,1048576,1048576)` 两组对照，除这些值外输入完全相同，同一个 Result 的完整 ToolResultProjection 必须 DeepEqual，Context 操作与摘要也一致；只有改变 `tool.inline_output_bytes` 才允许 Capture Abort/Commit 与 projection 验证变化。
- T3.19a 集成测试证明安全路径的 Provider 得到 ModelContent，Event/TUI/Hook 得到 UserView，Conversation/JSONL/重启只得到 PersistedContent+Ref；并发 run 使用相同 call ID、跨 iteration ordinal 重置、摘要 survivor rebase、保存失败、Provider 错误、取消和会话切换后均无槽串用或残留。T4.25c/d/e 的 Assembly 测试再证明唯一 Store/Capture/ResultFactory 的真实生产注入；T4.29a 的 repoaudit 最终禁止直接 Result 构造、兼容字段读取、raw fallback 和 ModelContent 持久化。
- 每个新增 required 根测试都进入闭合 `(Package, Test, ExpectedRuns=1)` manifest。T5.26 前人工核对 `go test -json` 中根测试恰好一次 run+pass、无 skip/fail、package pass且进程 exit 0；零匹配、同名跨包顶替、missing 或 excess 均失败。T5.26 后由 required-test gate 自动判定。
- C18 不改变既有 Assembly 里程碑顺序。T3.19 只授权 `internal/contextmgr/{manager,test}.go`、`internal/tool/{capture,result,result_factory}.go`、`internal/repoaudit/context_result_refs_test.go` 及机械构造调用点；T3.19a 授权 `internal/orchestrator/{chat,agent_loop,tool_batches,stream_collector}.go`、`internal/conversation/{message,jsonl_record}.go`、`internal/events/events.go`、`internal/hook` 结果 adapter、`internal/tui/messages.go`、`internal/tool/{executor,bash,read,grep,glob,write,edit,registry}.go`、`internal/mcpclient/adapter.go` 及直接测试/fixture，用于建立安全路径与迁移 adapter，不得创建 Store owner。
- `cmd/xagent/assembly.go` 仅在 T4.24、T4.25b–e 按原任务权限修改；`cmd/xagent/main.go` 在此前只允许保持编译且不产生第二 owner 的机械构造调用点更新，生产入口切换仍只属于 T4.29a。任何 T3.19 子任务不得依赖 T4.*；T4.25c 增加 T3.19a 依赖，最终零引用/删除门禁归 T4.29a。其余 closed-world 约束不变。

#### C18a：executionState 临时槽文件授权（已批准，2026-08-03）

C18 已批准的 ModelContent 临时槽必须由现有 `executionState` concrete owner 持有，而该类型实际定义在 `internal/orchestrator/execution_state.go`。因此 T3.19a 的授权文件面补充该文件及其直接测试，仅用于加入 C18 已定义的 conversation ID、request generation、iteration、ordinal、message index 键、checked byte accounting、survivor rebase 和全出口清理；不得在其中持有 `tool.Result`、UserView、Artifact payload/路径或 Store capability，不得创建 Orchestrator 全局 map、第二投影入口或新增业务行为。C18 其余接口、数据流、阶段边界与验证要求不变。

#### C18b：Result 生产者与 Capture 注入闭环（已批准，2026-08-03）

T3.19/T3.19a Task 终审确认两项实现信息必须在 Plan 层闭合：当前 `internal/orchestrator/skill_runtime.go` 的 load-skill 成功/失败路径和 `internal/tool/load_skill.go` 的内部路由失败仍直接构造兼容 Result；同时，C18 只描述“窄 Capture 构造闭包”，尚未固定其函数形状、Counter owner 和 T4 前后的生效边界。C18b 只补齐这些既有安全结果生产者及依赖注入细节，不新增工具、配置、Store owner 或业务行为；批准后以下条款取代 C18 中把 Result/ResultFactory/OutputMeta package seal 归给 T3.19a 的单句表述：

- T3.19 完成 `Result`、`ResultFactory`、`ResultFactoryInput` 与 `OutputMeta` 的包内四视图契约/seal，并完成 ContextManager 投影验证；T3.19a 只消费该已封闭契约，将所有结果生产者迁入安全 candidate path、建立 Orchestrator 扇出与临时 ModelContent 槽。T3.19a 不再次定义 Result 类型、OutputMeta 字段或第二 factory。
- T3.19a 的生产文件授权增加 `internal/orchestrator/skill_runtime.go` 与 `internal/tool/load_skill.go` 及直接测试。load-skill 成功、失败、内部路由拒绝、unknown/invalid/denied/timeout/cancel-after-start 与其他 synthetic 结果全部接收 Assembly 最终注入的同一个 ResultFactory；不得自行调用 `NewResultFactory`、直接构造 `tool.Result`、调用 `Success`/`Failure` 或形成专用第二 factory。T4.29a 的兼容入口删除与零引用门禁明确覆盖这两个文件。
- 不新增命名的通用 CaptureFactory 类型或可替换 registry。每个需要采集用户字节流的 producer 只接收等价于 `func(context.Context, artifact.Metadata) (*tool.Capture, error)` 的未导出窄函数依赖；闭包由唯一 Assembly 创建并捕获唯一 Artifact Store、resolved `tool.inline_output_bytes`、resolved `tool.capture_bytes` 及对应不可变 hard cap。每次调用都新建只含 Bytes 维度的 operation-local `budget.Counter`，再创建一个新的 Capture/Writer；producer 永远不能取得 Store、Counter 构造参数、Reader、路径或其他调用的 Capture。
- Read/Grep/Glob/Bash/MCP producer 必须在读取/接收用户输出第一字节前调用该闭包一次，所有接受字节只写该 Capture，并恰好调用一次 Finish；一次调用只能产生一次 Abort 或 Commit。Capture 创建失败在读取/启动用户输出前 fail closed；零字节 write failure 或 Commit failure 按 C18 规范转为无输出 synthetic SafeError。非流式 synthetic producer 只接收同一 ResultFactory，不接收 Capture 闭包；纯 synthetic Hook result adapter 尤其不得取得可触达 Store 的能力。
- T3.19a 在 T4 Assembly 之前只建立显式 safe candidate path、窄依赖形状、迁移隔离和测试注入。旧 `main.go` 仍只能使用不可进入 ProjectToolResult、不可写 Conversation v2、不可伪造 Ref 的隔离迁移路径；不得宣称此时旧生产入口已获得首字节 Capture。T4.25c 用唯一 RuntimeRedactor 创建唯一 ResultFactory，并从 T4.25b 的唯一 Store/最终 resolved limits 创建内置工具闭包；T4.25d 向 MCP 注入同一 ResultFactory 与 Capture 闭包、向纯 synthetic Hook adapter 只注入同一 ResultFactory；T4.25e 注入 ContextManager/Orchestrator candidate。只有 T4.29a 原子发布该 candidate 并删除迁移 adapter/公开 Result payload/raw fallback 后，唯一安全链才成为生产事实。
- C18b 不改变 T4.24、T4.25b–e、T4.29a 的顺序或 owner 数量；T3.19/T3.19a 仍不得解析 artifact root、创建生产 Store、修改 `cmd/xagent/assembly.go` 或提前切换 `main.go`。C18/C18a 其余数据矩阵、ModelContent 生命周期、required-test 与 closed-world 约束保持不变。

#### C18c：Capture 不完整终态与 MCP 迁移选择文件授权（已批准，2026-08-03）

T3.19a 最终 closed-world 审查确认，C18b 已批准的两项既有行为需要落在其 concrete owner 文件中：producer 在已接受正字节后因外部 I/O、等待或取消失败时，必须由 `Capture` 原子记录不完整终态；生产 MCP Manager 在 T4.25d 注入真实 Capture 前，必须显式选择 C18b 已批准的 legacy adapter。C18c 只补齐这两个文件授权，不改变 Spec、接口数据流、required 根、Assembly 顺序或发布边界。

- T3.19a 的生产文件授权增加 `internal/tool/capture.go` 及直接测试，仅允许增加 `MarkIncomplete(error, CaptureTruncationReason)`：它与 `Write`、`Finish` 共用同一互斥和 first-terminal-error-wins 状态，拒绝 nil error、无效原因及 Finish 后调用；不得暴露 Writer、Store、Counter、路径或 payload，不得新增第二 Finish/Commit/Abort owner。该授权只实现 C18b 已批准的“已接受正字节后不完整即 Commit incomplete Ref”，不重新定义 T3.19 的 Capture/Result 契约。
- T3.19a 的生产文件授权增加 `internal/mcpclient/manager.go` 的 `buildToolCandidates` 单一构造调用点及直接测试，仅允许在 T4.25d 前显式调用 `NewLegacyRemoteToolAdapter`。Manager 不接收 Capture 闭包，不创建 Store/Writer/Counter/Ref，不进入 `ProjectToolResult`，也不把 legacy Result 写入 Conversation v2；T4.25d 才注入真实 MCP Capture，T4.29a 才原子删除 legacy 选择。
- `SafeResultProducer` 的声明留在已授权的 `internal/tool/registry.go`；`internal/tool/tool.go` 不增加 T3.19a 接口。Registry 的 immutable `View` 只能复用已经完成安全校验的 Tool，既不注册新 Tool，也不构造 candidate Executor，因此 `internal/tool/view.go` 不传播 `safeCandidate` 标志且不加入授权面。
- MCP candidate 目前只能证明 Capture 在 adapter 调用已解码 `CallToolResult` 前创建、DTO 文本逐块写入且每次调用使用新的 Capture/Writer/Counter；不得把它描述为 transport/wire 首字节采集。真实 Manager/Transport 首字节链仍仅属于 T4.25d。C18/C18a/C18b 的其余约束保持不变。

## Spec 覆盖自检

| 需求 | 主要归属 | 设计证据 |
|---|---|---|
| F1–F2 | repoaudit、Git source、CI | 私有目录、secret、gitlink、二进制规则及 index-only 止血 |
| F3–F6 | permission、safefs、proctree、App/TUI | 原始身份、一次性 Ticket、protected slot、degraded gate、确认视图 |
| F7–F8 | redact、netpolicy、config、diagnostics | 统一秘密注册、SafeText、逐跳/拨号复验 |
| F9–F13 | budget、artifact、tool、safefs、instructions | 采集前预算、私有原文、进程树、句柄遍历、include 图 |
| F14–F18 | provider、mcpclient、netpolicy、proctree | 累计协议预算、所有权链、显式流 Close、usage 终态 |
| F19–F24 | conversation、orchestrator、tool | v2 摘要链、部分列表、配置生效、有界并发、精确取消 |
| F25–F30 | app、command、tui、cmd/xagent | 导航事务、状态 reset、响应布局、统一帮助、成功 help/version |
| F31–F33 | 平台实现、testutil、CI、repoaudit、docs | 显式 build tags、三平台测试、稳定性与追溯门禁 |

自检结论：

- F1–F33 均有模块、接口、交互和文件归属，无未分配需求。
- 核心接口均给出所有者、关闭语义、错误或状态边界；实现者不需要依赖隐含的全局对象。
- 依赖从组装层向安全基础层单向收敛；permission 不反向依赖 tool，TUI 不依赖领域服务，MCP transport 不依赖 Manager。
- 与 Spec 不冲突：不新增 Provider、MCP 协议能力、业务工具、Skill 类型、远程同步或自动发布，也不承诺通用敌意代码沙箱。
