# 子 Agent 委派与后台任务系统 Plan

本设计基于已批准的 spec.md，目标是把模型工具调用和 TUI 显式入口收敛到同一条子 Agent 任务路径。实现只增加运行时状态隔离和进程内后台任务能力，不引入 Worktree、团队编排或跨会话恢复。

## 架构概览

采用“角色快照、任务管理、运行时执行、界面适配”四层结构，统一 Agent 工具和 TUI 入口都经过同一个任务提交服务。

~~~text
配置与角色来源
  项目 / 用户 / 内置 / 插件
              │
              ▼
      agentrole.Manager
   解析、校验、覆盖、原子快照
              │
              ├─────────────── sealed tool.Registry
              │
主 Agent Agent 工具 ─┐
TUI /agent 入口 ────┴──► subagent.TaskManager
                              │
             ┌────────────────┼────────────────┐
             ▼                ▼                ▼
          排队/并发        前后台切换       任务事件/结果
                              │
                              ▼
                 orchestrator.SubagentRunner
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
      独立消息状态         独立权限状态        独立用量/读缓存
                              │
                              ▼
       共享 Provider、Hook Engine、项目文件系统
                              │
             ┌────────────────┴────────────────┐
             ▼                                 ▼
       任务事件订阅                         ResultInbox
             │                                 │
             ▼                                 ▼
          TUI 任务页                 主 Agent 安全回灌边界
~~~

1. internal/agentrole 负责角色领域，不依赖 Orchestrator、App 或 TUI。它从四类来源加载 Markdown 角色，构建不可变快照，并提供角色目录、来源诊断、工具元信息和模型别名解析。

2. internal/tool 增加一个稳定 Agent 系统工具定义。它只负责向模型公开参数 Schema，不直接启动任务；模型调用和 TUI 显式入口都转换成同一个 typed Submit 请求。Agent 与 load_skill 使用系统路由，普通 Executor 不会直接执行它们。

3. internal/subagent.TaskManager 负责任务登记、准入、FIFO 队列、并发上限、状态机、任务级取消、前后台承载方式、确认 Broker、事件日志、终态保留和结果 Inbox。它不复用现有 runTracker 或 Hook 异步池，因此后台任务不会阻塞普通会话的 idle、导航或切换会话。

4. orchestrator.SubagentRunner 是唯一真正运行子 Agent Loop 的组件。它复用现有 Provider、Hook、工具 Executor 和循环逻辑，但为每个任务创建独立的 RuntimeState：

   - Defined 从空白子会话构造标准系统/安全指令、项目指令、角色正文和任务内容；
   - Fork 在委派瞬间捕获父请求的不可变消息、系统块、工具定义和缓存策略，后续只追加角色提示与任务消息；
   - 两种路径都在准备阶段一次性计算最终工具视图，执行期间不重新读取角色或父会话。

5. 前台/后台是任务承载方式，不是两份执行实例。任务从提交开始就使用 Manager 自有取消上下文；前台调用方只是同步等待和订阅任务。Fork 直接进入后台，显式后台和手动切后台使用相同的 Detach 路径，首轮模型请求超过默认 10 秒时自动执行该路径。切换不重启、不复制、不重新排队任务；切后台后父请求取消不再影响任务。

6. 子 Agent 的消息、Session 临时授权、确认队列、文件读取缓存、Token 计数、循环进度和取消状态全部独占。Provider 客户端、Hook Engine、项目文件系统、不可变工具注册元数据等基础设施共享，但不共享上述可变状态。角色权限只能继承或收紧父权限；父会话临时授权永不复制。

7. 工具能力使用单调收窄链（前台不应用后台白名单，进入/切换后台时才应用）：

~~~text
父委派时可用集合
  ∩ 角色白名单（未声明表示不额外收窄）
  − 角色黑名单
  − 全局禁止工具（至少 Agent）
   ∩（仅当 placement=background 时的后台允许集合）
  ∩ 当前模式与既有安全约束
~~~

同一冻结集合同时用于 Provider 工具定义、伪造调用预检和实际执行。过滤后的子 Agent 永远不能重新加入 Agent，从而禁止孙 Agent。

8. Fork 的 Provider 请求保留父请求的稳定系统前缀和缓存策略。若角色或安全过滤改变工具定义，则只复用仍然一致的系统缓存边界，并关闭不再匹配的工具缓存断点；不为了缓存重新暴露被过滤工具。OpenAI 路径忽略 Anthropic 专属缓存元数据，但保持请求正确性。

9. 每个任务的原始事件由 TaskManager 持续排空并包装为带任务标识、全局修订号和任务内序号的机器事件。TUI 使用独立任务订阅，不把后台事件放入主请求的 stale-envelope。任务详情显示状态、角色、类型、来源、时间、停止原因、摘要、用量、确认卡片和有界的最近轨迹。

10. 终态只构造一次不可变 Completion，同一份快照用于 TaskManager、终端事件、TUI 和结果 Inbox。主 Agent 正在运行时，结果只在下一安全的模型/工具回灌边界提供；主轮已结束时保留到下一次请求消费，绝不自动发起新的模型回合。思考内容和完整工具轨迹不进入主会话。

11. Assembly 接线顺序为：解析模型别名 → 注册内置工具、Skill 系统工具和 Agent 工具 → 注册 MCP 工具 → 封存工具注册表 → 构建角色 Manager 与 TaskManager → 注入 Orchestrator → 由 App/TUI 使用窄任务服务。应用关闭时先停止任务准入、取消并有界收尾所有子任务，再关闭普通交互、Hook、MCP 和 Provider。

本轮仍明确不包含 Worktree 文件隔离、团队编排、跨会话/跨进程任务恢复、插件目录扫描和新 Provider 协议。

### 设计契约约定

1. 代码块中的未加包名前缀的类型属于当前小节所标注的包；跨包类型始终使用完整限定名。所有返回给其他模块的切片、映射、指针和快照都必须是深拷贝，内部快照只通过不可变引用或原子替换读取。

2. `diagnostics.SafeError` 和 `diagnostics.SafeDiagnostic` 是跨模块错误/诊断的唯一公开载体。底层 `error`、Provider 原始响应、文件路径和工具输出只能在组件内部存在；离开组件前统一脱敏、限长并映射为固定错误码。

3. `subagent.Status` 是任务执行状态，`subagent.Placement` 是承载方式，两者正交。自动后台只改变 Placement，不产生 `StatusTimedOut`；后者只表示配置的任务墙钟上限或 Provider 超时。

## 核心数据结构

### 1. 角色定义与快照：internal/agentrole

~~~go
type Source string

const (
    SourcePlugin  Source = "plugin"
    SourceBuiltin Source = "builtin"
    SourceUser    Source = "user"
    SourceProject Source = "project"
)

type ModelAlias string

const (
    ModelInherit ModelAlias = "inherit"
    ModelHaiku   ModelAlias = "haiku"
    ModelSonnet  ModelAlias = "sonnet"
    ModelOpus    ModelAlias = "opus"
)

type PermissionMode string

const (
    PermissionInherit    PermissionMode = "inherit"
    PermissionStrict     PermissionMode = "strict"
    PermissionDefault    PermissionMode = "default"
    PermissionPermissive PermissionMode = "permissive"
)

type Metadata struct {
    Name           string
    Description    redact.SafeText
    ToolAllow      []string // nil=未声明；非 nil 空切片=显式禁止全部工具
    ToolDeny       []string
    Model          ModelAlias
    MaxIterations  *int     // nil=未声明；0=合法的零轮次上限
    PermissionMode PermissionMode
}

type Definition struct {
    Metadata
    Instructions redact.SafeText
    Provenance
    Fingerprint  string
}

type Provenance struct {
    Source     Source
    SourceID   string
    ProviderID string
    Origin     redact.SafeText
}

type CatalogItem struct {
    Name        string
    Description redact.SafeText
    Source      Source
    SourceID    string
    ProviderID  string
}

type ToolMetadata struct {
    Name           string
    ReadOnly       bool
    SideEffectFree bool
    ConcurrentSafe bool
}

type ModelMetadata struct {
    Alias     ModelAlias
    Concrete  string
    Provider  string
    Available bool
    Dynamic   bool // inherit=true；Concrete 为无父请求时的 Default fallback
}

// Limits combines per-entry, per-source and whole-refresh budgets. A role
// body/frontmatter that exceeds a byte limit is rejected, never silently
// truncated; aggregate fields are decremented while loading each source.
type Limits struct {
    MaxFiles            int
    MaxEntryBytes       int64
    MaxFrontmatterBytes int64
    MaxBodyBytes        int64
    MaxNameBytes        int64
    MaxDescriptionBytes int64
    MaxInstructionBytes int64
    MaxToolNameBytes     int64
    MaxToolListBytes     int64
    MaxOriginBytes       int64
    MaxSourceIDBytes     int64
    MaxProviderIDBytes   int64
    MaxRootBytes         int64
    MaxModelBytes        int64
    MaxTotalBytes        int64
    MaxToolNames        int
    MaxProviders        int
    MaxCandidates       int
    MaxDiagnostics      int
}

func DefaultLimits() Limits
func (l Limits) Validate() error

type ModelAliases struct {
    Haiku  string
    Sonnet string
    Opus   string
}

type ModelCatalog struct {
    Default string
    // 内部保存固定 alias → concrete model 的不可变映射
}

type ModelValidator func(string) error

type ModelResolutionError struct {
    Alias  ModelAlias
    Reason ErrorCode
}

func (e *ModelResolutionError) Error() string // 只返回固定 Reason，不含 concrete model

type ModelCatalogOptions struct {
    ProviderID    string
    DefaultModel  string
    Aliases       ModelAliases
    MaxModelBytes int64
    Validate      ModelValidator
}

func NewModelCatalog(ModelCatalogOptions) (ModelCatalog, error)
func (c ModelCatalog) Resolve(alias ModelAlias, inheritedModel string) (string, error)
func (c ModelCatalog) Models() []ModelMetadata
~~~

`Resolve` 只接受四个固定别名；`inherit` 优先使用调用方捕获并再次通过当前 Provider 模型校验的 `inheritedModel`，为空时使用 `Default`。Defined 使用提交时父请求的当前模型作为 inheritedModel，TUI idle Defined 使用 Default，Fork 使用委派瞬间父请求快照中的模型。其他别名或缺失 concrete mapping 返回 `model_alias_unavailable`，不静默回退。未配置某个固定 alias 合法且在 ModelMetadata 中标为 Available=false，角色本身仍有效，只有实际委派该角色时失败；显式空值或超长值在 config resolve 阶段 fail-closed，Provider 不接受的 concrete model 在 assembly 构建 `ModelCatalog` 时 fail-closed。`ModelCatalog` 同时导出按 alias 排序的不可变 `[]ModelMetadata`，每个任务只读取映射，不修改全局 Provider 配置。

`ModelCatalog.Resolve` 的映射失败返回动态类型为 `*ModelResolutionError`，调用方按类型而非错误字符串判断。委派准备把它映射为 `model_alias_unavailable`，安全摘要必须包含 canonical role name 和固定 alias（均限长、无路径/凭据），并明确是角色解析失败还是 inherited model 无效。

~~~go
type Candidate struct {
    Metadata
    Instructions redact.SafeText
    Origin      redact.SafeText
    Valid       bool
    Diagnostics []diagnostics.SafeDiagnostic
}

type FileSource struct {
    Source Source
    ID     string
    FS     fs.FS
    Root   string
}

type SourceProvider interface {
    ID() string
    LoadRoles(context.Context, ProviderLoadOptions) ([]Candidate, error)
}

type RegisteredProvider struct {
    ProviderID string
    Provider   SourceProvider
}

type ErrorCode string

const (
    ErrProviderLimit        ErrorCode = "role_provider_limit"
    ErrProviderFailed       ErrorCode = "role_provider_failed"
    ErrModelAliasUnavailable ErrorCode = "role_model_alias_unavailable"
)

type ProviderLoadOptions struct {
    // Limits 是唯一权威上限；Provider 必须在读取/解析过程中遵守，
    // 不得先无界加载再截断。
    Limits Limits
}

type ProviderRegistry interface {
    Register(SourceProvider) error
    Seal() error
    Snapshot() []RegisteredProvider
}

func NewProviderRegistry(maxProviders int, maxProviderIDBytes int64) (ProviderRegistry, error)

type Snapshot struct {
    Generation  uint64
    Fingerprint string
    Catalog     []CatalogItem
    Definitions map[string]Definition
    Diagnostics []diagnostics.SafeDiagnostic
    DiagnosticsDropped uint64
    Tools       []ToolMetadata
    Models      []ModelMetadata
}

func (d Definition) Clone() Definition
func (s Snapshot) Clone() Snapshot

type ResolvedRole struct {
    Generation uint64
    Definition Definition
}

type ManagerOptions struct {
    Sources   []FileSource
    Plugins   ProviderRegistry
    Tools     []ToolMetadata
    Models    ModelCatalog
    Limits    Limits
    Redactor  *redact.RuntimeRedactor
}

type RefreshResult struct {
    Published   bool
    Changed     bool
    Generation  uint64
    Snapshot    Snapshot
    Diagnostics []diagnostics.SafeDiagnostic
    DiagnosticsDropped uint64
    Error       *diagnostics.SafeError // 候选被拒绝时填充；旧快照不替换
}

type Manager interface {
    Snapshot() Snapshot
    Resolve(name string) (ResolvedRole, bool)
    Refresh(context.Context) (RefreshResult, error)
}

func NewManager(context.Context, ManagerOptions) (Manager, error)
~~~

四类来源的目录和身份固定为：项目来源使用 `${ProjectRoot}/.xagent/agents/`，`SourceID=project`；用户来源使用 `${UserConfigRoot}/agents/`，`SourceID=user`（平台路径由 Assembly 解析，Linux 可来自 XDG 配置根，macOS 使用平台配置根）；内置来源使用 `internal/agentrole/builtins/` 的 `go:embed` FS，`SourceID=builtin`；插件来源不扫描目录，只接受已 `Seal` 的 `ProviderRegistry`，每个规范化 `ProviderID` 固定属于 plugin tier。项目/用户目录不存在视为空，内置目录缺失或损坏属于装配错误。`FileSource.ID` 是来源身份，候选不能自报 tier、SourceID 或 ProviderID；Manager 在绑定来源批次时注入完整 `Provenance`。

`ProviderRegistry.Register` 在首次注册时读取并规范化 ProviderID，拒绝空值、非法字符、超长值和同名重复；`Seal` 后把按 canonical ProviderID 排序的 `RegisteredProvider` 深拷贝快照冻结，之后不再调用 provider 的可变 `ID()`，也不允许注册、替换或动态增删。`MaxProviders` 只计 plugin provider 数量，超限返回固定的 registry 错误。

刷新时 Manager 只消费 sealed 的 `RegisteredProvider.ProviderID`，不再用插件运行中的 `ID()` 重新判定 provenance；若 Provider 返回协议不符合约定的候选批次，整次 refresh fail-closed。

因此后文“ProviderID 与 `ID()` 不一致”的协议校验仅指注册/封存时的 `RegisteredProvider` 绑定；Candidate 本身不携带或自报 ProviderID，Manager 不会在刷新期间再次调用插件的 ID 方法。

Provider 的 `Valid=false` 永不被 Manager 升级，`Valid=true` 仍可被结构复核降为无效；`Valid=false` 必须至少带一条 SafeDiagnostic，否则 Manager 添加固定 `role_candidate_invalid` 诊断；`Valid=true` 可以带 warning/info，但只要包含 error severity 或结构复核失败就降为无效。FileSource 只允许 builtin/user/project，ProviderRegistry 中的 provider 固定注入 plugin tier，候选自报 Source 不参与优先级。ProviderRegistry 最多注册 `MaxProviders` 个 provider；Manager 按稳定 ProviderID 顺序调用，并把全量 refresh 剩余的候选、总字节和诊断 budget 写入每次 `ProviderLoadOptions.Limits`，而不是给每个 provider 一份完整额度。Provider 必须流式读取，在任一文件、总字节、候选数、工具/正文或诊断上限的 `cap+1` 处停止并返回动态类型为 `*diagnostics.SafeError`、code=`role_provider_limit` 的错误。其他非 context 错误必须为 code=`role_provider_failed` 的 SafeError；Manager 不把插件原始 error 透出。`LoadRoles` 的结果是全有或全无：任何 non-nil error（包括 context 取消）都使同时返回的 partial candidates 被丢弃，候选快照不发布。Manager 在每批成功返回后立即对 Candidate 数量、规范化字段/正文字节和诊断数量做二次计数并扣减全局 budget，超限或 ProviderID 与 `ID()` 不一致时拒绝整个候选快照，不能依赖插件自报字节数；Provider 的返回 slice 本身也必须有界。

角色 Manager 按 plugin → builtin → user → project 的低到高顺序处理有效定义，再按来源 ID、规范化 origin 排序；后者覆盖前者，最终优先级固定为 project > user > builtin > plugin。插件 Provider 按 F8 返回已验证、已脱敏的 Candidate；Manager 仍用当前进程 RuntimeRedactor 重新处理 Description/Instructions/Origin/diagnostics，并做 canonical 字段、工具/模型引用、Limits、Provenance 和 fingerprint 的结构校验，但不要求知道插件原始 Markdown 存储。无效高优先级定义只产生脱敏诊断；同一来源出现多个有效同名角色使候选快照失败。首次加载遇到该冲突或不可恢复来源错误时 `NewManager` 返回结构化错误且不发布半成品；运行中 `Refresh` 返回 `Published=false` 的 `RefreshResult`，旧快照和运行任务保持不变。`Snapshot`、`ResolvedRole`、`Definition.Clone` 深拷贝所有嵌套切片/指针；内部用 refresh mutex 串行构建候选、以原子 generation 指针发布，context 取消不发布，fingerprint 相同返回 `Changed=false`，旧候选不能覆盖更新 generation。

角色 Markdown 的严格解析契约如下：frontmatter 必须从文件第一行 `---` 开始并以独立 `---` 结束，只允许一个 YAML 文档、UTF-8 和已知字段 `name`、`description`、`allowed_tools`、`denied_tools`、`model`、`max_iterations`、`permission_mode`；未知字段、重复键、错误类型和超出 `Limits` 均拒绝。解析 YAML 节点时先拒绝 `AliasNode`、任意 anchor、mapping key `<<`（merge key）以及带 `TaggedStyle` 的显式标准/custom tag；只允许解析器为普通标量推导出的隐式类型标签。也拒绝 YAML directive、多个 document 和尾随非空文档，随后再以 `KnownFields(true)` 解码，从而不能通过展开、合并或自定义 tag 绕过字段白名单和重复键检查。省略 model/permission_mode 等价于 `inherit`；任何已知字段显式 null 均非法，空字符串或非精确小写枚举值也非法。`max_iterations` 只接受 YAML 非负整数，0 合法，负数、浮点、字符串和 int overflow 均拒绝。只有 `allowed_tools` key 完全缺失才表示 nil（不额外限制），显式空数组表示隐藏全部工具。名称去空白、ASCII 小写规范化并须匹配 `[a-z0-9][a-z0-9_-]{0,63}`；description 经 trim、脱敏后必须仍非空且在限额内；工具名去空白、去重、稳定排序，未知白/黑名单工具产生错误诊断并使候选无效；正文 trim、脱敏后必须非空，且作为每一轮都存在的角色系统块。读取时先限制原始 entry/frontmatter/body，再脱敏并进行第二次 UTF-8 安全限长检查，防止 redaction 扩长。文件枚举只接受 Root 的直接子级小写 `*.md` 常规文件，忽略隐藏文件和子目录，按 canonical origin 排序，不跟随 Root 外符号链接。

解析和指纹由 Manager 统一完成，插件不能通过自报 `Valid`、`Source`、`ProviderID` 或 `Fingerprint` 绕过校验。`Candidate` 只携带已脱敏的元信息、正文、Origin、有效性和诊断；Manager 在绑定来源批次时唯一注入 `Source`/`ProviderID`，再构造不可变 `Definition` 并计算 fingerprint：

~~~go
type ParseOptions struct {
    Origin     string
    Limits     Limits
    Redactor   *redact.RuntimeRedactor
}

func ParseMarkdown([]byte, ParseOptions) (Candidate, error)
func DiscoverFiles(context.Context, FileSource, Limits, *redact.RuntimeRedactor) ([]Candidate, error)
~~~

`Definition.Fingerprint` 由规范化后的 source/sourceID/provider/origin、元信息、排序后的工具列表和正文按固定字段顺序计算；`Snapshot.Fingerprint` 还包含全部候选身份/有效性、安全 diagnostics、DiagnosticsDropped、排序后的 Tools/Models 元数据，忽略插件传入的 fingerprint。Catalog 按 canonical role name、Tools 按 exact tool name、Models 按 alias、Diagnostics 按 source rank/sourceID/provider/origin 以及 SafeDiagnostic 的 Code/Source/Hint/Severity/Message 全字段排序；map hash 一律先排序 key。超过全局诊断上限时保留稳定前缀并追加 `role_diagnostics_truncated` sentinel，同时记录 DiagnosticsDropped。`ModelCatalog` 的“可用”含义是：当前已装配 Provider 对应的 concrete model 字符串通过注入的 ModelValidator 且非空；本轮不新增远程模型发现协议。`llm.model_aliases` 的配置解析、长度校验和 `ModelCatalog` 构造在 assembly 阶段一次完成。

来源 ID、ProviderID 必须规范为小写 ASCII 并匹配 `[a-z0-9][a-z0-9._-]*`；分别受 `MaxSourceIDBytes`、`MaxProviderIDBytes` 限制且同 tier ID 唯一。Root 必须非空、受 `MaxRootBytes` 限制并位于安全文件边界内；未知 Source 在装配时拒绝。description/origin 禁止 NUL、ESC 等终端控制字符，正文只允许安全的换行与 tab；展示层仍执行现有终端转义。不存在的可选 user/project 目录视为空；单个 role entry unreadable/invalid/symlink/non-regular 只使该候选无效并产生安全诊断，不遮蔽低层有效角色；Root 自身 escape/symlink/类型错误、provider 协议违规、总候选/文件上限和同层有效冲突才拒绝整个候选快照。`Limits.MaxFiles` 按 FileSource 计；`MaxProviders`、`MaxCandidates`、`MaxDiagnostics` 和 `MaxTotalBytes` 都按一次全量 refresh 聚合计数，不能被来源或 provider 数量相乘放大。Refresh 的候选拒绝通过 `RefreshResult.Error` 返回且方法 error=nil；方法 error 只用于 context cancellation/deadline 或无法安全构造 SafeError 的内部故障，二者互斥，Snapshot 字段始终是当前旧快照。

指纹编码采用版本化、长度前缀的 canonical encoding 后 SHA-256；诊断截断 sentinel 占用 `MaxDiagnostics` 的最后一个配额，且 `MaxDiagnostics<1` 在 DefaultLimits.Validate 中拒绝。文件枚举使用 bounded `ReadDir(n)`/safefs 流式遍历，达到 `MaxFiles+1` 即停止并诊断，不先无界分配整个目录。

配置层保留 presence 语义：`PartialLLMConfig.ModelAliases` 的 `haiku/sonnet/opus` 使用 `Optional[string]`，缺失表示 alias 未配置（Available=false），显式空值或超长值表示配置错误；解析后的 `config.LLMConfig` 只携带已校验值。顶层 `subagent:` 映射 `SubagentConfig`，同时承载可收窄的 `RoleLimits`、运行 `Limits`（并发、队列、事件/结果保留、后台白名单、自动后台阈值和可选 MaxTaskDuration）。启动时一次性 `Validate`，任何负数、零值不允许的字段、乘法溢出或超过 hard cap 都 fail-closed；未配置的 role limit 使用 `agentrole.DefaultLimits()`。

首版默认 role limits（字节按 UTF-8 编码计）为：MaxFiles=256、MaxEntryBytes=256KiB、MaxFrontmatterBytes=32KiB、MaxBodyBytes/MaxInstructionBytes=128KiB、MaxNameBytes=64、MaxDescriptionBytes=8KiB、MaxToolNameBytes=128、MaxToolListBytes=8KiB、MaxOriginBytes/MaxSourceIDBytes/MaxProviderIDBytes/MaxModelBytes=256、MaxRootBytes=4KiB、MaxTotalBytes=64MiB、MaxToolNames=128、MaxProviders=64、MaxCandidates=512、MaxDiagnostics=256；每项都有更高 hard cap，配置只能收窄。默认 subagent limits 为 MaxTaskBytes=32KiB、MaxConcurrent=4、MaxQueued=32、MaxRetainedTasks=256、MaxTaskTombstones=1024、MaxGlobalEvents=8192、MaxEventsPerTask=1024、MaxEventBytes=64KiB、MaxSubscriberBuffer=128、MaxResultBytes=64KiB、MaxPendingResults=256、MaxResultTotalBytes=8MiB、MaxResultsPerClaim=32、ReadCacheMaxEntries=128、ReadCacheMaxBytes=8MiB、ReadCacheMaxValueBytes=1MiB、ReadCacheMaxDependenciesPerEntry=4096、MaxIDBytes=128、AutoBackgroundAfter=10s、MaxTaskDuration=0。

~~~go
// internal/config (presence-aware decode types)
// The following fields are additions to the existing config structs; all
// approved legacy fields remain unchanged.
type PartialModelAliases struct {
    Haiku  Optional[string] `yaml:"haiku"`
    Sonnet Optional[string] `yaml:"sonnet"`
    Opus   Optional[string] `yaml:"opus"`
}

type PartialLLMConfig struct {
    ModelAliases PartialModelAliases `yaml:"model_aliases"`
}

type PartialAppConfig struct {
    Subagent PartialSubagentConfig `yaml:"subagent"`
}

type PartialSubagentConfig struct {
    RoleLimits           PartialRoleLimits `yaml:"role_limits"`
    MaxTaskBytes         Optional[int64]   `yaml:"max_task_bytes"`
    MaxConcurrent        Optional[int64]   `yaml:"max_concurrent"`
    MaxQueued            Optional[int64]   `yaml:"max_queued"`
    MaxRetainedTasks     Optional[int64]   `yaml:"max_retained_tasks"`
    MaxTaskTombstones    Optional[int64]   `yaml:"max_task_tombstones"`
    MaxGlobalEvents      Optional[int64]   `yaml:"max_global_events"`
    MaxEventsPerTask     Optional[int64]   `yaml:"max_events_per_task"`
    MaxEventBytes        Optional[int64]   `yaml:"max_event_bytes"`
    MaxSubscriberBuffer  Optional[int64]   `yaml:"max_subscriber_buffer"`
    MaxResultBytes       Optional[int64]   `yaml:"max_result_bytes"`
    MaxPendingResults    Optional[int64]   `yaml:"max_pending_results"`
    MaxResultTotalBytes  Optional[int64]   `yaml:"max_result_total_bytes"`
    MaxResultsPerClaim   Optional[int64]  `yaml:"max_results_per_claim"`
    ReadCacheMaxEntries  Optional[int64]   `yaml:"read_cache_max_entries"`
    ReadCacheMaxBytes    Optional[int64]   `yaml:"read_cache_max_bytes"`
    ReadCacheMaxValueBytes Optional[int64] `yaml:"read_cache_max_value_bytes"`
    ReadCacheMaxDependenciesPerEntry Optional[int64] `yaml:"read_cache_max_dependencies_per_entry"`
    MaxRoleNameBytes     Optional[int64]   `yaml:"max_role_name_bytes"` // subagent Submit role parameter
    AutoBackgroundAfterMS Optional[int64] `yaml:"auto_background_after_ms"`
    MaxTaskDurationMS    Optional[int64]   `yaml:"max_task_duration_ms"`
    BackgroundTools      Optional[[]string] `yaml:"background_tools"`
}

type PartialRoleLimits struct {
    MaxFiles              Optional[int64] `yaml:"max_files"`
    MaxEntryBytes         Optional[int64] `yaml:"max_entry_bytes"`
    MaxFrontmatterBytes   Optional[int64] `yaml:"max_frontmatter_bytes"`
    MaxBodyBytes          Optional[int64] `yaml:"max_body_bytes"`
    MaxNameBytes          Optional[int64] `yaml:"max_name_bytes"` // role Markdown canonical name
    MaxDescriptionBytes   Optional[int64] `yaml:"max_description_bytes"`
    MaxInstructionBytes   Optional[int64] `yaml:"max_instruction_bytes"`
    MaxToolNameBytes      Optional[int64] `yaml:"max_tool_name_bytes"`
    MaxToolListBytes      Optional[int64] `yaml:"max_tool_list_bytes"`
    MaxOriginBytes        Optional[int64] `yaml:"max_origin_bytes"`
    MaxSourceIDBytes      Optional[int64] `yaml:"max_source_id_bytes"`
    MaxProviderIDBytes    Optional[int64] `yaml:"max_provider_id_bytes"`
    MaxRootBytes          Optional[int64] `yaml:"max_root_bytes"`
    MaxModelBytes         Optional[int64] `yaml:"max_model_bytes"`
    MaxTotalBytes         Optional[int64] `yaml:"max_total_bytes"`
    MaxToolNames          Optional[int64] `yaml:"max_tool_names"`
    MaxProviders          Optional[int64] `yaml:"max_providers"`
    MaxCandidates         Optional[int64] `yaml:"max_candidates"`
    MaxDiagnostics        Optional[int64] `yaml:"max_diagnostics"`
}

type SubagentConfig struct {
    RoleLimits      agentrole.Limits
    Limits          subagent.Limits
    BackgroundTools []string
}

type LLMConfig struct {
    ModelAliases ResolvedModelAliases `yaml:"-"`
}

type ResolvedModelAliases struct {
    Haiku  string
    Sonnet string
    Opus   string
}

func (a ResolvedModelAliases) ToAgentRole() agentrole.ModelAliases

type AppConfig struct {
    Subagent SubagentConfig `yaml:"-"`
}

func ResolveSubagentConfig(PartialSubagentConfig) (SubagentConfig, error)
func ResolveModelAliases(PartialModelAliases, int64) (ResolvedModelAliases, error)
~~~

现有 `config.ResolveConfig` 是 presence → resolved 的唯一所有者：它先完成配置层合并，再调用 `ResolveModelAliases`（缺失保留为空、显式空值拒绝）和 `ResolveSubagentConfig`（应用默认值、overflow-safe 转成 int/duration、校验 hard caps），最后统一执行 resolved config validation。Assembly 只读取 `config.AppConfig`，不得再次解释 Optional 或自行补默认值。

### 2. 工具路由与能力视图：internal/tool

~~~go
type ExecutionRoute uint8

const (
    RouteExecutor ExecutionRoute = iota
    RouteSystem
)

type ExecutionPolicy struct {
    ReadOnly       bool
    SideEffectFree bool
    ConcurrentSafe bool
}

func (p ExecutionPolicy) AllowsBackgroundByDefault() bool

type BackgroundPolicy struct {
    Enabled      bool
    AllowedNames []string // 稳定排序；nil 表示由默认策略导出
    Fingerprint  string
}

func DefaultBackgroundPolicy(*Registry) (BackgroundPolicy, error)
func (p BackgroundPolicy) Validate(*Registry) error
func (p BackgroundPolicy) Allows(name string) bool
~~~

`AllowsBackgroundByDefault` 要求 `ReadOnly && SideEffectFree && ConcurrentSafe`。默认策略只导出已注册且三项均为真的工具（首版明确标记 `Read`、`Glob`、`Grep`）；`Enabled=false` 表示前台任务不额外应用后台集合。显式扩展只能引用已注册名称，不能绕过角色、Plan、Hook、权限或硬安全检查。

ToolDescriptor 增加 Route 字段；load_skill 与 Agent 为 RouteSystem。注册表增加一次性 `Seal()`，在内置工具、MCP 工具和系统工具全部注册后封存；封存后注册、策略元数据和执行目标均不可变。

~~~go
const AgentToolName = "Agent"

type AgentToolInput struct {
    Task      string `json:"task"`
    Type      string `json:"type"`      // defined | fork
    Role      string `json:"role,omitempty"`
    Placement string `json:"placement,omitempty"` // default | foreground | background
}
~~~

Agent 的公开 schema 只包含上述四个字段、固定枚举和统一长度上限；工具名、字段名和事件 `EventKind` 不因角色/插件变化。SystemToolRouter 将已脱敏、已校验的 `AgentToolInput` 与当前 InvocationRef/ParentRef 组合成 `subagent.SubmitInput`，TUI 命令不得另建提交路径。

模型工具调用的路由顺序固定为 `CapabilitySet.Allows` 预检 → 过滤则直接返回带 `FilterReason` 的结构化拒绝 → `RouteSystem` 才进入 SystemToolRouter → 普通工具才进入 normalize/hard-check/Hook/permission/confirmation/ticket/ScopedExecutor 链。子任务的 `Depth>0` 或全局禁止命中时在第一步拒绝，因此伪造 Agent 调用不会创建任务、消耗队列或触发 Provider。

~~~go
type CapabilitySet struct {
    Registry    *Registry // 不可变 View
    Names       []string
    Fingerprint string
    Rejections  map[string]FilterReason
}

type FilterReason string

const (
    FilterRoleAllow      FilterReason = "role_allow"
    FilterRoleDeny       FilterReason = "role_deny"
    FilterGlobalDeny     FilterReason = "global_deny"
    FilterBackgroundDeny FilterReason = "background_deny"
    FilterPlanMode       FilterReason = "plan_mode"
    FilterRecursive      FilterReason = "recursive_delegate"
    FilterUnknown        FilterReason = "unknown_tool"
)

func BuildCapabilitySet(
    parent *Registry,
    role *agentrole.Definition,
    background BackgroundPolicy,
    globalDenied map[string]struct{},
    planMode bool,
) (CapabilitySet, error)

func (c CapabilitySet) Allows(name string) bool
func (c CapabilitySet) Clone() CapabilitySet
~~~

过滤顺序是：`(父冻结集合 ∩ role allow（未声明表示不额外收窄） − role deny − global deny)`，然后仅当 `background.Enabled=true` 时再与 `AllowedNames` 取交集，最后应用当前 Plan/硬约束；前台构造的 View 不因后台白名单丢失工具。`CapabilitySet.Names` 和 `Rejections` 只在构造/Clone 时复制，Provider 工具定义、伪造调用预检和实际执行始终引用同一个 sealed View。Agent 始终在 global deny 中，绝不使用 `AlwaysInclude` 复活。

前台任务在准备阶段同时计算 foreground 与 background 两个快照；`Detach` 原子地将 active View 换成已收窄的 background View。切换不重启任务，切换前已经发出的模型调用仍在返回后按新 View 预检，后续 Provider 请求只发送新 View 的定义；已经通过预检并进入 running 的工具按原取消/收尾语义继续，不因 placement 切换中断或重跑。

~~~go
type ReadCacheLimits struct {
    MaxEntries             int
    MaxBytes               int64
    MaxValueBytes          int64
    MaxDependenciesPerEntry int
}

type ReadCacheKey struct {
    Tool                 string
    ArgumentsFingerprint string
}

type FileVersion struct {
    Path    string
    Size    int64
    ModTime int64
    Digest  string
}

type CachedReadResult struct {
    State            ExecutionState
    Status           ResultStatus
    Summary          redact.SafeText
    Preview          redact.SafeText
    Artifact         *artifact.Ref
    CapturedBytes    int64
    Truncated        bool
    TruncationReason redact.SafeText
    Error            *SafeError
}

type AuthorizedResultCache interface {
    Lookup(context.Context, ValidatedCall) (Result, bool, error)
    Store(context.Context, ValidatedCall, Result) error
}

type ReadCache struct { /* 任务独占；内部锁、LRU、有界字节计数和共享 ResultFactory */ }

func NewReadCache(ReadCacheLimits, *ResultFactory) (*ReadCache, error)
func (c *ReadCache) Get(ReadCacheKey, []FileVersion) (CachedReadResult, bool)
func (c *ReadCache) Put(ReadCacheKey, []FileVersion, CachedReadResult) error
func (c *ReadCache) Lookup(context.Context, ValidatedCall) (Result, bool, error)
func (c *ReadCache) Store(context.Context, ValidatedCall, Result) error
func (c *ReadCache) CloneEmpty() *ReadCache
func (c *ReadCache) Close()

type ScopedExecutor struct { /* 绑定 Base Executor、ScopeID 和票据验证器 */ }

type CapabilitySource interface {
    Current() CapabilitySet // 返回当前 placement 的 sealed View 快照
}

func NewScopedExecutor(*Executor, permission.TicketVerifier, string, CapabilitySource, *ReadCache) (*ScopedExecutor, error)
func (e *Executor) ExecuteValidatedAuthorizedWithVerifier(
    context.Context,
    ValidatedCall,
    permission.ExecutionTicket,
    permission.TicketVerifier,
    AuthorizedResultCache,
) Result
func (e *ScopedExecutor) ExecuteValidatedAuthorized(
    context.Context,
    ValidatedCall,
    permission.ExecutionTicket,
) Result
~~~

ReadCache 只缓存已脱敏的 Read/Glob/Grep 结果；key 包含工具名、规范化完整参数和冻结 read-root identity 的 fingerprint，依赖集合包含所有相关文件/目录版本并稳定排序，任一依赖变化即 miss。缓存值不是裸 SafeText，而是能通过共享 ResultFactory 重新物化当前 CallID 的 `CachedReadResult`；它深拷贝 model/user/persistence 所需的安全摘要、preview、状态、截断信息和 opaque artifact ref，不保存原始输出或旧 CallID。条目、依赖数、字节和单值大小超限时拒绝写入并产生可恢复诊断。`subagent.Limits` 的四个 ReadCache 字段逐项构造 `ReadCacheLimits`，并要求它们为正、`MaxValueBytes <= MaxBytes`，所有计数/加法 overflow-safe。它不跨任务共享，也不改变项目文件系统。

ScopedExecutor 要求其 task verifier 的固定 ScopeID 与 TaskID 一致，并在调用 Base Executor 的最后一个线性化点重新读取 `CapabilitySource.Current()`；若任务已 Detach、工具被 background View 移除，则丢弃旧票据并返回对应 FilterReason，不执行工具。通过能力重检后，ScopedExecutor 调用 `ExecuteValidatedAuthorizedWithVerifier`，由 Base Executor 使用该任务 verifier 原子验证并消费票据恰好一次；只有越过这一授权 start boundary 后，Base 才对 Read/Glob/Grep 调用 `AuthorizedResultCache.Lookup`，hit 时物化完整 `tool.Result`，miss 时执行工具并用 `Store` 缓存安全模板。缓存错误降级为可观测 miss，不绕过执行链；它绝不先消费票据再调用现有二次消费路径。这一设计覆盖跨任务票据复用及 waiting_confirmation 期间切后台的竞态；已经进入 Base Executor running 的调用按既有取消/收尾语义继续。

### 3. 委派请求与任务服务：internal/subagent

~~~go
type ID string

type ExecutionType string

const (
    TypeDefined ExecutionType = "defined"
    TypeFork    ExecutionType = "fork"
)

type PlacementIntent string

const (
    PlacementDefault    PlacementIntent = "default"
    PlacementForeground PlacementIntent = "foreground"
    PlacementBackground PlacementIntent = "background"
)

type Placement string

const (
    Foreground Placement = "foreground"
    Background Placement = "background"
)

type Origin string

const (
    OriginModel Origin = "model"
    OriginTUI   Origin = "tui"
)

type ParentRef struct {
    ConversationID string
    ExecutionID    string
    RequestGeneration uint64
}

type InvocationRef struct {
    ToolCallID string // 模型 Agent 工具调用；TUI 提交时为空
}

type SubmitInput struct {
    Task      string
    Type      ExecutionType
    Role      string
    Placement PlacementIntent
    Origin    Origin
    Parent    ParentRef
    Invocation InvocationRef
}
~~~

SubmitInput 是唯一不可信输入边界。模型工具和 TUI 都调用同一个 Submit，由该入口完成清理、限长、类型、角色、父引用、调用身份和权限模式校验。`PlacementDefault` 对 Defined 解析为 foreground，对 Fork 解析为 background；显式 background/Fork 在登记前即确定后台承载。模型同步前台结果用 `Invocation.ToolCallID` 与父循环的原始调用配对；TUI 该字段必须为空。Fork 会在这里把 placement 规范化为后台，并在同一串行边界捕获父 Conversation、当前 Provider 请求前缀、工具 View、模型、Plan/权限模式快照；之后不再读取父可变状态。

`OriginModel` 必须提供有效 ConversationID、ExecutionID、RequestGeneration 和 ToolCallID，四者由当前执行状态共同校验；跨会话、旧 generation 或伪造调用拒绝。`OriginTUI` 必须绑定当前 ConversationID，ToolCallID 为空；Defined 可在没有活跃 Provider 请求时从 sealed Registry、应用默认 permission/Plan profile、默认模型和标准环境构建父能力基线，且仍不继承激活 Skill。TUI Fork 必须在 Orchestrator 的会话串行边界为“当前已完整提交的父消息”和当前系统/工具/模式构造一份新的 budgeted PromptPrefixSnapshot；只有与最近请求 fingerprint 相同的稳定前缀才复用其 cache boundary，不得直接复用一份消息或环境已过期的旧 Provider 请求。若当前状态无法安全投影，则返回 `parent_snapshot_unavailable`。ParentRef 的 RequestGeneration 只表示父请求代数，不与角色 Snapshot.Generation 混用。

~~~go
type Submission struct {
    ID        ID
    Type      ExecutionType
    Role      string
    Origin    Origin
    Parent    ParentRef
    Placement Placement
    Status    Status
    Revision  uint64
    CreatedAt time.Time
}

type Status string

const (
    StatusQueued              Status = "queued"
    StatusRunning             Status = "running"
    StatusWaitingConfirmation Status = "waiting_confirmation"
    StatusCompleted           Status = "completed"
    StatusFailed              Status = "failed"
    StatusCancelled           Status = "cancelled"
    StatusTimedOut            Status = "timed_out"
    StatusLimitReached        Status = "limit_reached"
)

type StopReason string

const (
    StopCompleted          StopReason = "completed"
    StopMaxIterations      StopReason = "max_iterations"
    StopCancelled          StopReason = "cancelled"
    StopApplicationClosed  StopReason = "application_closed"
    StopTaskTimeout        StopReason = "task_timeout"
    StopProviderError      StopReason = "provider_error"
    StopToolError          StopReason = "tool_error"
    StopUnknownToolLimit   StopReason = "unknown_tool_limit"
    StopInternalError      StopReason = "internal_error"
)

type Usage struct {
    InputTokens              int64
    OutputTokens             int64
    CacheCreationInputTokens int64
    CacheReadInputTokens     int64
}

type Completion struct {
    ID         ID
    Status     Status
    Summary    redact.SafeText
    SummaryTruncated bool
    TruncationReason redact.SafeText
    StopReason StopReason
    Usage      Usage
    Error      *diagnostics.SafeError
    EndedAt    time.Time
}

func (c Completion) Clone() Completion

type TaskSnapshot struct {
    ID                  ID
    Revision            uint64 // 该任务最后一次权威更新的全局 Revision
    Type                ExecutionType
    Origin              Origin
    Role                string
    RoleSource          agentrole.Source
    RoleSourceID        string
    RoleProviderID      string
    RoleOrigin          redact.SafeText
    RoleGeneration      uint64
    Placement           Placement
    Status              Status
    Parent              ParentRef
    CreatedAt           time.Time
    StartedAt           *time.Time
    EndedAt             *time.Time
    Iteration           int
    MaxIterations       int
    StopReason          StopReason
    Summary             redact.SafeText
    SummaryTruncated    bool
    TruncationReason    redact.SafeText
    Error               *diagnostics.SafeError
    Usage               Usage
    EventsDropped       uint64
    PendingConfirmation *events.ToolConfirmationRequest
}

func (s TaskSnapshot) Clone() TaskSnapshot

type TaskListSnapshot struct {
    Watermark uint64 // 与 Tasks 在同一 EventHub 读锁下捕获
    Tasks     []TaskSnapshot
}

type TaskDetailSnapshot struct {
    Watermark   uint64 // 与 Task/RecentEvents 在同一 EventHub 读锁下捕获
    Task        TaskSnapshot
    RecentEvents []Event
}

func (s TaskListSnapshot) Clone() TaskListSnapshot
func (s TaskDetailSnapshot) Clone() TaskDetailSnapshot
~~~

`Completion` 是摘要字段的唯一来源：`TaskSnapshot` 与 `ResultNotification` 必须逐字段复制 `Summary`、`SummaryTruncated`、`TruncationReason`、`StopReason`、`Usage` 和深拷贝后的 `Error`。`SummaryTruncated=false` 时 TruncationReason 必须为空；为 true 时必须使用固定、脱敏且有界的原因码。任何投影不得再次独立截断并制造不同摘要。

`Status` 的唯一合法转移为：`queued → running | cancelled`；`running → waiting_confirmation | completed | failed | cancelled | timed_out | limit_reached`；`waiting_confirmation → running | failed | cancelled | timed_out | limit_reached`。正常完成只能从 running 发生，queued 不消耗墙钟且不能直接完成/超时/达到上限。终态不可再转移，且每个任务只构造一次 Completion；取消、墙钟超时和 Shutdown 的竞态由 Manager 的状态锁线性化，已线性化的终态优先，重复操作返回幂等成功或明确的 `task_terminal`。`StatusTimedOut` 只由 `MaxTaskDuration`/Provider 超时产生，自动后台不使用它。

终态映射固定为：模型不再请求工具→`completed/completed/Error=nil`；max iterations 或 unknown-tool 上限→`limit_reached/max_iterations|unknown_tool_limit/limit_reached`；用户/任务/应用取消→`cancelled/cancelled|application_closed/cancelled`；墙钟或 Provider deadline→`timed_out/task_timeout/timed_out`；Provider、工具致命或内部故障→`failed/provider_error|tool_error|internal_error` 及对应 SafeError。普通权限拒绝/确认 deny 是可恢复工具结果，恢复 running，不构成终态 StopReason。

~~~go
type ErrorCode string

const (
    ErrInvalidTask             ErrorCode = "invalid_task"
    ErrTaskTooLarge             ErrorCode = "task_too_large"
    ErrContextBudgetExceeded    ErrorCode = "context_budget_exceeded"
    ErrInvalidType             ErrorCode = "invalid_type"
    ErrInvalidPlacement        ErrorCode = "invalid_placement"
    ErrInvalidParent           ErrorCode = "invalid_parent"
    ErrParentSnapshotUnavailable ErrorCode = "parent_snapshot_unavailable"
    ErrUnknownRole             ErrorCode = "unknown_role"
    ErrModelAliasUnavailable   ErrorCode = "model_alias_unavailable"
    ErrPermissionEscalation    ErrorCode = "permission_escalation"
    ErrRecursiveDelegate       ErrorCode = "recursive_delegate"
    ErrQueueFull               ErrorCode = "queue_full"
    ErrTaskNotFound             ErrorCode = "task_not_found"
    ErrTaskExpired              ErrorCode = "task_expired"
    ErrTaskTerminal             ErrorCode = "task_terminal"
    ErrInvalidTransition        ErrorCode = "invalid_transition"
    ErrConfirmationNotFound     ErrorCode = "confirmation_not_found"
    ErrConfirmationStale        ErrorCode = "confirmation_stale"
    ErrPermanentNotAllowed      ErrorCode = "permanent_not_allowed"
    ErrProviderFailed           ErrorCode = "provider_failed"
    ErrToolFailed               ErrorCode = "tool_failed"
    ErrCancelled                ErrorCode = "cancelled"
    ErrTimedOut                 ErrorCode = "timed_out"
    ErrLimitReached             ErrorCode = "limit_reached"
    ErrShutdown                 ErrorCode = "shutdown"
    ErrInternal                 ErrorCode = "internal"
    ErrEventCursorExpired       ErrorCode = "event_cursor_expired"
    ErrInboxFull                ErrorCode = "inbox_full"
    ErrAlreadyConsumed          ErrorCode = "already_consumed"
    ErrResultPublishFailed      ErrorCode = "result_publish_failed"
)

func SafeError(ErrorCode, redact.SafeText, bool) *diagnostics.SafeError
~~~

Service 的 API 错误和 Completion.Error 均使用上述固定码、脱敏摘要和 `Recoverable` 标志；普通 Go error 只保留在组件内部或表示调用方 context 取消。正常完成的 Completion 必须 `Error=nil`，其余状态与 StopReason/Error 的对应关系由 Manager 校验后才发布。

任务管理器使用 Runner 工厂与事件接收接口解耦执行实现：

~~~go
// AgentEvent 是 Provider/TUI 无关的安全投影。复用 events.Event 时只允许
// Text/Thinking/Tool*/AgentProgress/Usage/Diagnostic/Confirmation，
// IndependentID 必须清空，不携带主会话 envelope。
type AgentEvent struct {
    Kind    events.Type
    Payload events.Event
    Range   *DeltaRange // 合并时记录脱敏后 stream 的连续 byte offset
}

type DeltaRange struct { From, To int64 }

type EventSink func(AgentEvent) error

type PreparedMetadata struct {
    Type                    ExecutionType
    Role                    string
    RoleSource              agentrole.Source
    RoleSourceID            string
    RoleProviderID          string
    RoleOrigin              redact.SafeText
    RoleGeneration          uint64
    Model                   string
    MaxIterations           int
    MaxUnknownToolCalls     int
    PermissionMode          string
    Depth                   int
    ForegroundToolFingerprint string
    BackgroundToolFingerprint string
}

type PreparedTask interface {
    // Manager-owned task context is supplied. Runner stops after cancellation
    // and returns one candidate Completion; Manager worker owns panic recovery
    // and constructs the authoritative Completion.
    Run(context.Context, EventSink) Completion
    Metadata() PreparedMetadata
}

type RunnerFactory interface {
    // Prepare is called before queue publication. It must capture and freeze
    // role generation, parent Conversation/Prompt/Tool/Mode snapshots and
    // model resolution; failure leaves no task or queue item.
    Prepare(context.Context, ID, SubmitInput) (PreparedTask, error)
}
~~~

~~~go
type EventKind string

const (
    EventQueued                EventKind = "queued"
    EventRunning               EventKind = "running"
    EventWaitingConfirmation   EventKind = "waiting_confirmation"
    EventIterationStarted      EventKind = "iteration_started"
    EventTextDelta             EventKind = "text_delta"
    EventThinkingDelta         EventKind = "thinking_delta"
    EventTool                  EventKind = "tool"
    EventConfirmationRequested EventKind = "confirmation_requested"
    EventConfirmationResolved  EventKind = "confirmation_resolved"
    EventProgress              EventKind = "progress"
    EventUsage                 EventKind = "usage"
    EventPlacementChanged      EventKind = "placement_changed"
    EventDiagnostic            EventKind = "diagnostic"
    EventCompletion            EventKind = "completed"
    EventFailure               EventKind = "failed"
    EventCancellation          EventKind = "cancelled"
    EventTimeout               EventKind = "timed_out"
    EventLimit                 EventKind = "limit_reached"
    EventResultPublished       EventKind = "result_published"
    EventGap                   EventKind = "event_gap"
)

type Event struct {
    Revision   uint64
    TaskID     ID
    Sequence   uint64
    At         time.Time
    Kind       EventKind
    Agent      *AgentEvent
    Snapshot   *TaskSnapshot
    Completion *Completion
    Result     *ResultNotification
    Gap        *GapDescriptor
    Placement  *PlacementChange
    Decision   *ConfirmationDecisionDisplay
}

func (e Event) Clone() Event
func (e Event) ValidateOneOf() error

type GapDescriptor struct {
    FromRevision uint64 // inclusive
    ToRevision   uint64 // inclusive；订阅者至少错过此闭区间
    Reason       string
}

type PlacementChange struct {
    From   Placement
    To     Placement
    Reason string // explicit, automatic_timeout, manual, fork
}

type ConfirmationDecisionDisplay struct {
    ConfirmationID string
    CallID         string
    Action         events.PermissionAction
    Allowed        bool
}

// EventKind has a strict one-of payload invariant. The authoritative mapping is:
//   queued/running/waiting_confirmation/iteration_started/progress -> Snapshot
//   text_delta/thinking_delta/tool/confirmation_requested/usage/diagnostic -> Agent
//   confirmation_resolved -> Snapshot + Decision
//   placement_changed -> Snapshot + Placement
//   completed/failed/cancelled/timed_out/limit_reached -> Snapshot + Completion
//   result_published -> Result
//   event_gap -> Gap
// No other pointer field may be non-nil. ValidateOneOf rejects both missing and
// extra payloads before an event enters the authoritative log.

Runner 只把允许的 inner event 类型投影到 AgentEvent；`Done`/`Error` 仅作为 stop input，`MainTraceReset`、`UserSubmitted` 和任何 `IndependentID` 不得穿过子任务 EventHub。每个 tool batch/call 带模型序号，Runner 先按 batch/call index 排列并提交工具状态/结果，再由 EventHub 分配到达序号，保证并发工具结果仍按模型调用顺序回流。所有 `Event` 的指针 payload 和 `Clone` 返回值都做深拷贝，文本/思考/错误/摘要按同一限额截断并标记。

type ResultNotification struct {
    NotificationID string
    CompletionRevision uint64
    CompletionSequence uint64
    CreatedAt      time.Time
    TaskID         ID
    Parent         ParentRef
    Status         Status
    Summary        redact.SafeText
    SummaryTruncated bool
    TruncationReason redact.SafeText
    StopReason     StopReason
    Usage          Usage
    Error          *diagnostics.SafeError
}

func (n ResultNotification) Clone() ResultNotification

type ForegroundOutcome struct {
    Completion *Completion // 非 nil 表示同步终态
    Detached   bool        // true 表示已接受后台，Completion 为空
}

type Service interface {
    Submit(context.Context, SubmitInput) (Submission, error)
    List(context.Context) (TaskListSnapshot, error)
    Get(context.Context, ID) (TaskDetailSnapshot, error)
    Cancel(context.Context, ID) error
    MoveToBackground(context.Context, ID) error
    ResolveConfirmation(context.Context, ID, events.ToolConfirmationDecision) error
    Subscribe(context.Context, uint64) (<-chan Event, error) // after/exclusive global Revision
    Await(context.Context, ID) (Completion, error)           // 只等待终态
    AwaitForeground(context.Context, ID) (ForegroundOutcome, error)
    ClaimResults(context.Context, ResultClaimOptions) (ResultClaim, error)
    AckResults(context.Context, string, ParentRef) error
    ReleaseResults(context.Context, string, ParentRef) error
    Shutdown(context.Context) error
}

type ResultInbox interface {
    Reserve(context.Context, ID, ParentRef) error
    ReleaseReservation(context.Context, ID, ParentRef) error
    Publish(context.Context, ResultNotification) error
    Claim(context.Context, ResultClaimOptions) (ResultClaim, error)
    Ack(context.Context, string, ParentRef) error
    Release(context.Context, string, ParentRef) error
}

type ResultClaim struct {
    ClaimID       string
    Owner         ParentRef // 本次消费请求的已认证 owner；不是结果来源 ParentRef
    SerializedBytes int64
    Notifications []ResultNotification
}

type ResultClaimOptions struct {
    Owner            ParentRef
    MaxNotifications int
    MaxBytes         int64 // 固定 schema 序列化后的总字节上限
}

type ConfirmationBroker interface {
    Request(context.Context, events.ToolConfirmationRequest) (events.ToolConfirmationDecision, error)
    Resolve(events.ToolConfirmationDecision) error
    Pending() *events.ToolConfirmationRequest
    Close(error)
}
~~~

`Await` 的等待者取消只取消等待，不取消任务；模型工具路由使用 `AwaitForeground`，在终态或原子 Detach 事件中返回。ResultInbox 对同一 ConversationID 的来源结果按 `CompletionRevision`、TaskID 稳定排序；Claim 只选择同时满足 `MaxNotifications <= Limits.MaxResultsPerClaim` 和固定 schema `MaxBytes` 的最老连续前缀，不删除消息而创建带 `Owner` 的 lease。没有一条能放入预算时返回空 claim（ClaimID 为空）且不占 lease；只有携带完全匹配的 `ClaimID+Owner` 的 Ack 才删除，Release 可重试且会释放 lease。`ReleaseReservation` 仅释放尚未 Publish 的 TaskID 容量预留：Prepare/登记失败和同步前台工具结果成功配对时调用，重复调用幂等；已经 Detach 或 Publish 的任务不能误释放。容量超限返回 `inbox_full` 并产生可观测投影失败诊断，不写入未脱敏内容。

每个任务独占一个 ConfirmationBroker，确认必须同时匹配 TaskID（由 Service 路由）、ConfirmationID 和 CallID；`Pending` 返回 detached deep copy。重复、过期或错误任务的决定返回 `confirmation_stale/not_found`。Cancel/Shutdown 调用 Close 唤醒唯一等待者并映射为任务 cancellation；`allow_permanent` 在子任务中 fail-closed 返回 `permanent_not_allowed`，不得降级为 session/once，`allow_session` 只写入该任务的新 Session。TaskID、NotificationID、ClaimID 均由注入 IDGenerator 生成并校验非空、唯一、无控制字符且不超过 MaxIDBytes。Claim lease 绑定消费方的 ConversationID、ExecutionID、RequestGeneration 和当前主请求身份，并在 `ResultClaim.Owner` 中返回；非 owner 的 Ack/Release 返回 `invalid_transition`，不能跨会话或跨请求伪造。消费请求 context 结束而未 Ack 时 Inbox 自动执行同 owner Release，防止 lease 永久占用；显式 Release 与自动 Release 幂等。父会话结束后由下一请求以自己的 ParentRef 新建 lease，而不是复用旧请求身份。

~~~go
type Limits struct {
    MaxTaskBytes        int64
    MaxIDBytes           int64
    MaxRoleNameBytes    int64
    MaxConcurrent       int
    MaxQueued           int
    MaxRetainedTasks    int
    MaxTaskTombstones   int
    MaxGlobalEvents     int
    MaxEventsPerTask    int
    MaxEventBytes       int64
    MaxSubscriberBuffer int
    MaxResultBytes      int64
    MaxPendingResults   int
    MaxResultTotalBytes int64
    MaxResultsPerClaim   int
    ReadCacheMaxEntries  int
    ReadCacheMaxBytes    int64
    ReadCacheMaxValueBytes int64
    ReadCacheMaxDependenciesPerEntry int
    AutoBackgroundAfter time.Duration
    MaxTaskDuration     time.Duration // 0=不启用；与自动后台阈值独立
}
~~~

默认值为并发 4、排队 32、自动后台 10 秒，`MaxTaskDuration=0`；所有配置在启动时校验，溢出或非法值 fail-closed，并要求 `MaxRetainedTasks >= MaxConcurrent + MaxQueued`（overflow-safe）。并发槽从任务真正运行开始占用，并在 waiting_confirmation 期间保留；队列槽只计 queued 项。达到 `MaxRetainedTasks` 时只按终态时间、TaskID 的稳定顺序淘汰已终态任务，永不淘汰 queued/running/waiting_confirmation。淘汰 ID 进入独立、最多 `MaxTaskTombstones` 个的有界 tombstone 集合；tombstone 按淘汰时间、TaskID 稳定过期，集合外的更旧 ID 与从未见过的 ID 都返回 `task_not_found`，集合内返回 `task_expired`。未消费通知由独立有界 Inbox 保留。

~~~go
type ManagerOptions struct {
    Runner          RunnerFactory
    Limits          Limits
    Inbox           ResultInbox
    Redactor        *redact.RuntimeRedactor
    Clock           func() time.Time
    IDGenerator     func() (ID, error)
    ShutdownTimeout time.Duration
}

func NewManager(ManagerOptions) (Service, error)
~~~

Manager 从提交起以应用 lifecycle context + 自身 MaxTaskDuration 构造 task context，不继承调用方 deadline/cancel；调用方 context 仅用于规范化、父快照捕获和准备阶段。前台绑定一个可原子解除的 parent-cancel bridge，Fork/显式后台不建立该 bridge。`Detach` 的线性化点若先于父取消，则任务继续；反之任务可进入 cancelled，但绝不重启、复制或重新排队。MaxTaskDuration 从 StartedAt/worker running 线性化点起算，queued 不消耗墙钟；自动后台只计首个 Provider 请求并与其独立。队列满、关闭后的新提交和超限请求立即返回对应 SafeError。

EventHub 由 Manager 常驻消费 Runner 的 EventSink，Runner 不直接向 TUI 的可慢通道发送。全局 Revision 和任务 Sequence 都从 1 开始严格递增；接近整数上限时停止新准入并保留终态 revision，不能在耗尽后再分配事件。`Subscribe(after)` 以全局 Revision 为 exclusive cursor，先从 `MaxGlobalEvents` 有界日志重放，再接实时流；cursor 早于最早保留 Revision 时返回 `event_cursor_expired` 并要求调用方用 `List/Get` 重同步。`List/Get` 在同一个 EventHub 读锁下返回状态/轨迹及全局 Watermark；消费者随后调用 `Subscribe(Watermark)`，日志重放覆盖锁释放后的所有新事件，因此 Gap 重同步没有状态—订阅竞态。每个订阅者使用有界缓冲；文本/思考增量可以按脱敏后连续 offset 无损合并，慢订阅者 buffer 满时由 EventHub 非阻塞地占用一个保留槽投递恰好一个 `EventGap`（包含丢失的闭区间和重同步原因），随后关闭该订阅 channel；消费者必须用 List/Get+新 cursor 重同步，不能把 Gap 当作终态。`EventGap` 是 subscriber-local 信号，不进入 authoritative log、不消耗新的 Revision；其 `Revision=ToRevision`、`TaskID=""`、`Sequence=0`。若订阅者在 Gap 投递前已取消，则只关闭 channel，不等待或重试慢消费者。Manager 的 authoritative log、TaskSnapshot、确认、工具终态、Placement 和 Completion 不丢失；Subscriber 反压不能阻塞 Worker 或应用关闭。

`MaxEventsPerTask` 只限制任务详情中的最近轨迹：超限时按 Sequence 淘汰最旧的非终态轨迹并记录 dropped count，Completion、当前 TaskSnapshot、PendingConfirmation 和最后一个 PlacementChange 单独保留，不占该轨迹额度。`MaxGlobalEvents` 的淘汰仅影响 cursor replay；所有权威当前状态仍由 `List/Get` 重建。

ResultInbox 以 ConversationID 作为跨主请求 owner key，原 ExecutionID/RequestGeneration 只保留为来源校验和通知元数据，因此父轮结束后下一请求仍能消费；不同会话绝不串线。Submit 在接受任务前为潜在异步结果 Reserve，前台同步完成后释放；因此终态 Publish 不会因普通容量竞争失败。Orchestrator 先让 Context Manager 为结果注入保留可用字节，再以该值构造 ResultClaimOptions；Claim 为当前消费 ParentRef 建立 lease，且 `SerializedBytes` 必须等于最终固定 schema 投影的实测字节。只有同一 `ClaimID+Owner` 的主请求越过 Provider 请求接受边界后才能 Ack，构造/预算/adapter validation/Provider 启动失败或取消时由同一 owner Release 供下一请求重试。按 `CompletionRevision`、TaskID 稳定排序；全局 pending 数、总字节、单条字节和单次 claim 数均受 Limits 限制，满载时显式返回 `inbox_full`，不静默淘汰未消费结果。

模型可见的异步结果使用版本化固定 schema：`ResultMessage{schema_version, task_id, status, summary, summary_truncated, truncation_reason, stop_reason, usage, error:{code,message,recoverable?}}`，字段按该顺序以安全 JSON/固定 marker 投影，通知按 `CompletionRevision`、TaskID 排序后逐条追加；不包含 ParentRef、路径、完整工具轨迹或原始凭据。

TaskManager 的 EventReducer 是 TaskSnapshot 的唯一写入点：`ToolWaitingConfirmation` 设置 waiting/pending；确认 resolve、ToolRunning 或 ToolDenied 清空 pending 并恢复 running；usage 先由 Runner 转成任务累计值，Reducer 只接受非负、单调且不溢出的累计快照。内层 Agent Loop 的 `events.Done/Error` 只作为 Runner stop input，不直接发布外层终态；终态由 `completeOnce` 原子构造。

Manager 验证 Runner 返回的 Completion：ID 必须匹配、Status 必须终态、EndedAt/Usage/StopReason/Error 组合合法，所有文本已脱敏并在限额内；panic 或不合法返回值统一映射为 `internal`。Summary 由固定 projector 从已安全收集的最终 assistant 文本生成，不额外调用模型；失败/取消时保留已产生的安全文本，没有文本则使用固定状态摘要，超长时 UTF-8 截断并显式标记。TaskSnapshot 终态字段、terminal Event、ResultNotification 和 TUI 都只能从这份 Completion 深拷贝投影。若终态后的 Inbox Publish 因内部故障失败，不修改 Completion，而是发布 `result_publish_failed` 诊断并从 Manager 保留的终态快照有界重试到 Shutdown。

### 4. 运行时隔离结构：internal/orchestrator

~~~go
type RuntimeProfile struct {
    Model             string
    PermissionMode    permission.Mode
    PlanMode          bool
    Depth             int
    ReadRoots         []string
    Persist           bool // 子任务固定 false
    UpdateMemory      bool // 子任务固定 false
    MaxUnknownToolCalls int
    ForegroundTools   tool.CapabilitySet
    BackgroundTools   tool.CapabilitySet
    MaxIterations     int
}

type CancelBridge interface {
    Detach() bool // 原子解除父取消传播；重复调用幂等
    Close()
}

type CapabilitySwitch interface {
    tool.CapabilitySource
    MoveToBackground() (changed bool, current tool.CapabilitySet)
}

type TaskRuntimeState struct {
    TaskID         subagent.ID
    Parent         subagent.ParentRef
    Role           *agentrole.ResolvedRole
    Conversation   *conversation.Conversation
    Profile        RuntimeProfile
    ActiveTools    CapabilitySwitch
    Authorizer     *permission.Authorizer
    Executor       *tool.ScopedExecutor
    Confirm        subagent.ConfirmationBroker
    ReadCache      *tool.ReadCache
    Usage          provider.Usage
    Iteration      int
    Prompt         provider.PromptPrefixSnapshot
    HookSessionID  string
    HookExecution  hook.ExecutionRef
    Context        context.Context
    Cancel         context.CancelCauseFunc
    ParentBridge   CancelBridge
}
~~~

每个任务独占 TaskRuntimeState；`ResolvedRole` 是 generation 绑定的深拷贝，Fork 无附加角色时为 nil。`RuntimeProfile` 不复用可变的 `skill.ExecutionProfile`，防止 Skill Activity、allow map 或 IndependentDepth 串入子任务。CapabilitySwitch 以任务锁/原子指针切换 sealed View，Provider 构造、伪造调用预检和 Executor 都只读 Current，避免 Detach data race。Depth 在 child 中固定为 1，SystemToolRouter 在任何 Depth > 0 的运行时直接返回 `recursive_delegate`，且不会进入 Submit/队列。主会话也通过同一组显式依赖进入 Agent Loop，避免循环隐式读取 Orchestrator 全局授权、确认表或计量字段。

权限作用域：

~~~go
// internal/permission
type TaskScopeOptions struct {
    ScopeID        string
    Mode           Mode
    AllowPermanent bool
}

type TaskScope struct {
    ScopeID   string
    Authorizer *Authorizer
    Issuer     TicketIssuer
    Verifier   TicketVerifier
}

func (a *Authorizer) NewTaskScope(TaskScopeOptions) (TaskScope, error)
func RestrictMode(Mode, agentrole.PermissionMode) (Mode, error)
~~~

权限严格性偏序为 `strict < default < permissive`。role=`inherit` 保留父模式；其余值只能取不高于父模式的最严格结果，任何放宽请求返回 `permission_escalation`。首版 AllowPermanent=false；NewTaskScope 深拷贝只读 user/project/local 规则层，创建全新 Session 和固定绑定 ScopeID 的任务票据 authority，不复制父 Session 规则。该 authority 的 Issuer/Verifier 共享同一任务私钥与 nonce 集，签发时把 ScopeID 纳入 ExecutionTicket 的私有 MAC 输入，Verifier 只接受自己的 ScopeID；不同任务即使 CallID、参数身份相同也无法消费对方票据。TaskScope.Authorizer 使用其中的 Issuer，ScopedExecutor 使用配对 Verifier，Base Executor 最终只消费一次。任务可获得一次性或任务内 Session 授权。

### 5. Fork 与 Provider 缓存快照：internal/conversation、internal/provider

~~~go
// internal/conversation
type ConversationSnapshot struct {
    // 私有、深拷贝的 ID、Title、Messages、ContextMetadata 和时间字段
}

func TakeSnapshot(*Conversation) (ConversationSnapshot, error)
func (s ConversationSnapshot) MaterializeEphemeral(id string, now time.Time) *Conversation

const RoleSubagentNotification MessageRole = "subagent_notification"

type SubagentNotificationMessage struct {
    NotificationID   string
    TaskID           string
    Status           string
    Summary          redact.SafeText
    SummaryTruncated bool
    TruncationReason redact.SafeText
    StopReason       string
    CreatedAt        time.Time
}

func AppendSubagentNotification(*Conversation, SubagentNotificationMessage) error
~~~

`TakeSnapshot` 只能在 Orchestrator 对父消息完成上一完整批次 ordered commit、尚未开始下一轮修改的串行边界调用，先做有界深拷贝再返回；不得从裸 Messages slice 并发读取。快照物化时生成新的会话 ID，CreatedAt/UpdatedAt 使用 task clock；Title 仅供任务详情显示，不进入 Prompt。只保留模型可见且已脱敏的 user/assistant/tool/context 消息；Context 只复制 summary/boundary 语义字段，Token 计量、失败计数和时间字段归零；thinking、transient UI 消息、父授权、确认、读缓存和循环进度均不复制。

`RoleSubagentNotification` 只持久化用户已看到的 bounded 摘要（通知 ID、任务 ID、状态、摘要、停止原因），不带完整轨迹、ParentRef 或 ToolCallID；JSONL 校验、版本迁移和 TUI 渲染显式支持该 role，并校验字段上限、状态枚举和 `NotificationID` 非空。TUI 按 `NotificationID` 去重，事件重放、重连或重复展示不得追加第二条持久记录；Provider 的普通 `ContextMessages` 排除它。模型若需要结果，只使用 ResultInbox 在安全边界注入的瞬时 `ModelMessageRoleSubagentResult`，从而既遵守主会话保存规则，又避免持久摘要被当成用户/assistant 历史重复送模。

~~~go
// internal/provider
type PromptPrefixSnapshot struct {
    OrderedSystem bool // true: 使用 System；false: 使用 StableSystem+DynamicSystem
    Model         string
    System        []SystemBlock // Hook ordered-system 路径
    StableSystem  []SystemBlock
    DynamicSystem []SystemBlock
    Messages      []ModelMessage
    Tools         []ToolDefinition
    Thinking      config.ThinkingConfig
    Cache         CachePolicy
    MessagePrefix int
    ToolFingerprint string
    Fingerprint   string
}

func CapturePromptPrefix(ChatRequest) (PromptPrefixSnapshot, error)
func (s PromptPrefixSnapshot) Validate() error
func (s PromptPrefixSnapshot) BuildChild(
    appendedSystem []SystemBlock,
    appendedMessages []ModelMessage,
    tools []ToolDefinition,
) ChatRequest
~~~

父 Agent 在调用 Provider 前保存这一轮已经完成预算检查的精确请求快照。当该轮返回 Agent 工具调用时，SystemToolRouter 使用生成该调用的 pre-StreamChat 快照，并在父 Conversation 的 ordered-commit 锁下捕获截至上一完整工具批次的消息；当前尚未配对 tool_result 的 Agent 调用只记录在 `InvocationRef`，不放进子 Provider 消息，避免构造悬空 tool_call。TUI idle Fork 则在同一串行边界把当前全部已完成父消息和当前环境投影为新的 PromptPrefixSnapshot；只有 fingerprint 相同的最近稳定系统/工具前缀保持原 cache boundary。Fork 从得到的不可变快照构建，之后不重新读取父会话、Skill 或项目指令。`CapturePromptPrefix` 物化 `ToolDefs` 为深拷贝 `Tools`，不复制父 RequestObserver 或可执行 Registry 指针；所有内容都经过现有预算和脱敏边界。

角色正文和任务消息只能追加在父稳定前缀之后，并由 Runner 在每轮重新构造请求时保留。`PromptPrefixSnapshot.MessagePrefix/ToolFingerprint` 记录父请求缓存身份，`BuildChild` 据此派生现有 CachePolicy：Anthropic 仅在工具指纹完全一致时继承工具缓存断点，工具被收窄时保留字节一致的 system/message 前缀及系统断点并关闭 tool breakpoint；OpenAI 忽略 Anthropic 元数据但保持请求内容相同。

Prepare 会把角色追加块、任务内容和工具定义纳入 child context budget；若追加后超出 Provider/应用窗口，返回 `context_budget_exceeded`（不裁剪父稳定前缀、不重读父会话），任务不入队。

Fork 的 Provider 真相源始终是委派边界已经经过 context budget/compaction 的 `PromptPrefixSnapshot.Messages`：模型工具调用使用生成该调用的 pre-StreamChat 消息，TUI Fork 使用提交瞬间重新投影的当前完整消息。ConversationSnapshot 只用于隔离后的任务审计/展示；Runner 不会在第二轮重新把父 Conversation 全量投影回来。每轮请求都由冻结的父 ModelMessage 前缀加子任务本轮后新增的 assistant/tool/user delta 构成，从而避免双重历史或让首次请求未见的旧消息重新出现。

`PromptPrefixSnapshot` 有显式 `OrderedSystem` 布尔值，因而不依赖空切片区分布局：`OrderedSystem=true` 时只允许 `System` 非 nil，`StableSystem/DynamicSystem` 必须为 nil；`false` 时 `System` 必须为 nil，稳定/动态两段可以为空。`CapturePromptPrefix` 在入口拒绝同时提供两套表示并深拷贝实际请求；`BuildChild` 只能在同一表示上追加角色/任务块，不改变布局。system/tool/message fingerprints 以最终实际发送序列计算，避免重复块或缓存边界漂移；`Validate` 在捕获、构造和提交 Provider 前各执行一次。

ResultInbox 的 `subagent_result` 是内部通知类型，不直接伪造 Provider wire role。同步前台完成只用原 Agent ToolCallID 回写一次现有 tool_result；异步结果在安全边界投影为新的 `ModelMessageRoleSubagentResult`。OpenAI adapter 将它编码为一个没有 tool-call 字段、内容以固定 `[subagent_result]` marker 开头的 `user` message；Anthropic adapter 使用同样 marker 的单个 user text block。两者都只携带固定 schema 的安全 JSON，不携带 ParentRef、路径或完整轨迹。该内部 role 不改变外部 Provider 协议，且绝不能与已完成的原 ToolCallID 再配对。

~~~go
const ModelMessageRoleSubagentResult ModelMessageRole = "subagent_result"
~~~

ChatRequest 的统一 Validate 在进入任一 Provider adapter 前拒绝未知 ModelMessageRole；OpenAI/Anthropic 对 `ModelMessageRoleSubagentResult` 的映射必须分别有 schema 测试，禁止沿用当前 switch default 的静默忽略行为。

## 模块交互

### 启动与快照装配

~~~text
Config Load/Validate
      │
      ├─ 解析 llm.model_aliases（presence-aware）
      └─ 校验 subagent.role_limits + subagent runtime limits
      ▼
Execution Assembly
      │
      ├─ 注册内置工具
      ├─ 注册 load_skill / Agent 系统工具
      └─ 注册共享 ResultFactory
      ▼
MCP Adapter Assembly
      │
      └─ 注册 MCP 工具
      ▼
Registry.Seal()
      │
      ├─ 导出 ToolMetadata
      ├─ `cfg.LLM.ModelAliases.ToAgentRole()` → `NewModelCatalog(ModelCatalogOptions{ProviderID, DefaultModel, Aliases, Validate})`
      ├─ 构建 ModelCatalog
      ├─ Seal Plugin Role Providers
      └─ 创建 agentrole.Manager
      ▼
Orchestrator + RunnerFactory + TaskManager
      ▼
App/TUI 订阅任务事件
~~~

角色 Manager 必须在 Registry 封存之后创建，确保角色校验使用最终工具集合。首次角色快照存在同层有效冲突时，装配失败；普通单文件错误只进入诊断并按覆盖规则处理。

### 模型调用与 TUI 调用的统一提交链

~~~text
模型 Agent 工具调用                    TUI /agent 命令
        │                                      │
        └──────────────┬───────────────────────┘
                       ▼
             SystemToolRouter / TaskIntent
                       ▼
              subagent.Service.Submit
                       │
        ┌──────────────┼──────────────┐
        ▼              ▼              ▼
  输入规范化       角色解析       placement 规范化
                       │
                       ▼
              队列/并发容量预留
                       │
                       ▼
              RunnerFactory.Prepare
                       │
        ┌──────────────┴──────────────┐
        ▼                             ▼
   Defined 准备                    Fork 准备
                       │
                       ▼
              任务记录进入 queued
                       ▼
                 Worker 执行
~~~

准备阶段失败会释放调度与 ResultInbox 容量预留并返回结构化错误，不留下任务记录或队列项。模型和 TUI 只在 Origin、父引用和前后台意图上不同。

### Defined 执行链

1. 绑定当前角色快照 generation。
2. 创建空白临时 Conversation 和空 Activity。
3. 构建标准系统/安全块、项目指令、角色正文和任务消息。
4. 计算父工具、角色限制、全局禁止后的 foreground/background 两份 CapabilitySet；只有实际 background View 应用后台白名单。
5. 创建独立 Authorizer、Confirmation Broker、ReadCache、用量计数器和 Hook Session。Hook Engine 仍共享，但每个任务使用 `SessionID="subagent:"+TaskID`：启动时一次 `SessionStart`，每轮 Provider 请求前后严格配对 `BeginTurn`/`EndTurn`，终态、取消或关闭时调用一次 `SessionEnd`；Hook 的 session prompt、turn 状态和执行身份不得读取或写入父 SessionID。
6. `MaxIterations=min(global Agent Loop 上限, role max_iterations（如有）)`，并冻结 `MaxUnknownToolCalls` 等流预算；有效 max_iterations=0 时在请求 Provider 前直接生成 limit_reached；其他情况复用 Agent Loop，无工具请求时生成 completed Completion。
7. 前台任务在阈值内完成时，由 SystemToolRouter 将摘要作为 Agent 工具结果回写父循环。
8. 已转后台的任务只返回接受结果，终态通过 ResultInbox 回流。

Defined 不读取父 Conversation 消息，也不复制父 Activity 或当前 Skill。

### Fork 执行链

父 Agent 在每次 StreamChat 前保存精确请求快照：

~~~text
父 executionState
   │
   ├─ Conversation 深拷贝
   ├─ Stable/Dynamic System blocks
   ├─ 已过滤 Tool definitions
   ├─ Model / Thinking / RunMode
   └─ CachePolicy + Prefix fingerprint
             │
             ▼
        Fork Prepare
             │
   ┌─────────┴─────────┐
   ▼                   ▼
可选角色动态提示       任务末尾消息
             │
             ▼
        子 Agent 首次请求
~~~

父会话之后的消息、Skill 刷新、工具注册变化或权限 Session 规则对 Fork 不可见。工具过滤导致指纹变化时只保留匹配的系统缓存边界。

### 子 Agent 单轮循环与确认

~~~text
Worker
  │
  ▼
iteration_started（Iteration=1 即首轮）
  │
  ▼
构造冻结 Prompt + 当前子 Conversation
  │
  ▼
Provider.StreamChat
  │
  ├─ Text/Thinking/Usage → Task EventHub
  ├─ Agent 调用 → recursive_delegate 拒绝
  └─ 普通工具 → CapabilitySet 预检
                         │
              Hard Check → Hook → Permission
                         │
                    ┌────┴────┐
                    ▼         ▼
                  deny       ask
                                │
                                ▼
                       Task Confirmation Broker
                                │
                           TUI 任务确认卡
                                │
                         allow / deny / cancel
                                │
                                ▼
                           循环继续
~~~

取消、Provider 错误、工具致命错误和达到轮次上限都阻止后续模型请求与新工具启动。

### 前台、自动后台和手动后台

~~~text
foreground queued/running/waiting_confirmation
        │
        ├─ 首轮模型请求超过阈值
        └─ 用户手动切后台
                │
                ▼
          原子 Detach()
                │
        ┌───────┴────────┐
        ▼                ▼
停止父取消桥       Placement=background
        │                │
        └──────不重启、不复制──────┘

显式 background / Fork
        │（登记前已确定，不安装父取消桥）
        ▼
background queued → running / waiting_confirmation
~~~

前台 Agent 路由调用 AwaitForeground：终态事件转为唯一工具结果，placement 转后台事件结束前台等待并返回接受结果。自动阈值只计首个 Provider 请求的墙钟时间；terminal 与 Detach 在同一任务锁下先提交者胜出，避免同步结果和后台接受结果双回流。公开 Await 只等待终态。

### 事件发布与 TUI 消费

~~~text
Runner raw event
      │
      ▼
TaskManager EventHub
  ├─ 深拷贝/脱敏/限长
  ├─ 分配 Revision
  ├─ 分配 TaskID 内 Sequence
  ├─ 更新 TaskSnapshot
  └─ 保存有界事件记录
      │
      ├─ App 长期订阅 → TaskState/ViewModel
      ├─ TUI 列表/详情
      └─ 无 TUI 时仍由 Manager 排空
~~~

连续文本/思考增量可无损合并并标记范围；确认、工具状态和终态事件不可丢失。

`Service.List` 返回带原子 Watermark、且不超过 `MaxRetainedTasks` 的深拷贝；排序完全稳定且与 map/调度顺序无关：先按 active 标志（queued/running/waiting_confirmation 在前），active 内按状态 rank（queued=0、running=1、waiting_confirmation=2），再按 `CreatedAt` descending，最后按 canonical `TaskID` ascending；终态按 `EndedAt` descending、`TaskID` ascending。`Get` 返回带同类 Watermark 的任务与有界最近事件深拷贝，已淘汰但有 tombstone 的 ID 返回 `task_expired`，从未见过的 ID 返回 `task_not_found`，不暴露内部指针或可变 map。TUI 只消费 TaskState/ViewModel，不把任务事件写入会话导航事务；任务详情显示 RoleSource/SourceID/ProviderID、安全摘要截断标记、确认卡、PlacementChange reason 和有界最近轨迹。

### 结果回流

~~~text
Completion
   │
   ├─ TaskManager terminal snapshot
   ├─ terminal task event
   ├─ TUI 完成/失败通知
   └─ ResultInbox.Publish(ParentRef)
                         │
              ┌──────────┴──────────┐
              ▼                     ▼
        父 Agent 仍运行          父轮已结束
              │                     │
      下一安全回灌边界          保留至下一请求
              │                     │
              └──────────┬──────────┘
                         ▼
       internal ResultClaim / subagent_result projection
~~~

安全回灌边界定义为：完整工具 batch 已按模型调用顺序提交且不存在悬空 tool_call 之后、下一次 Provider 请求构造之前。Orchestrator 先完成普通上下文预算/必要 compaction，并为至少一个 `MaxResultBytes` 上限内的固定 schema 消息保留注入空间，再用实际剩余字节 Claim 最老连续前缀；构造、统一 Validate 或 Provider 启动失败/取消时 Release。只有 `StreamChat` 成功返回已启动的 stream、即 Provider 请求接受边界越过后才 Ack；后续流错误不重新投递，避免模型可能已见结果时重复。模型可见 payload 只包含任务 ID、状态、脱敏摘要、停止原因、用量和可选 SafeError code/message；ParentRef 只作内部路由，不序列化。瞬时 `subagent_result` 不作为原 Agent ToolCallID 的 tool_result，也不自动启动模型回合。TUI 第一次展示完成/失败通知时，在主会话 ordered-commit 边界追加一次 `RoleSubagentNotification` 并走现有会话保存；重复 Event/重连按 NotificationID 去重。该持久记录不进入 Provider ContextMessages，详细结果仍只在进程内 TaskManager 中查看。

### 角色刷新与运行中任务

~~~text
来源变化
  ▼
构建候选快照 → 全量校验 → 原子发布
                         │
          ┌──────────────┴──────────────┐
          ▼                             ▼
     新提交任务绑定新代数          运行任务继续使用旧 Definition
~~~

### 取消与应用关闭

任务级取消：queued 项直接转 cancelled；running/waiting 项先关闭确认 Broker、以 cause 取消任务 Context、关闭 Provider stream、阻止新模型/工具，再按现有工具语义有界收尾并发布唯一 cancelled Completion。

应用关闭依次停止任务准入、取消队列及所有运行子任务、有界等待任务收尾、关闭各任务 Hook Session、再等待普通交互请求、保存会话，最后关闭 Hook、MCP 和 Provider。Shutdown 幂等；deadline 到达时返回 `timed_out` SafeError，尚未退出的任务保持 cancelled/failed 语义而不得伪造成功，EventHub/Inbox 只在所有可终止 producer 停止后关闭。

## 文件组织

~~~text
docs/subagent-system/
├── spec.md
├── plan.md
├── task.md
└── checklist.md

internal/agentrole/
├── types.go
├── parser.go
├── discovery.go
├── provider.go
├── snapshot.go
├── manager.go
├── model.go
├── builtin.go
├── builtins/explore.md
├── builtins/review.md
└── *_test.go

internal/subagent/
├── types.go
├── errors.go
├── limits.go
├── manager.go
├── scheduler.go
├── event_hub.go
├── confirmation.go
├── result_inbox.go
└── *_test.go

internal/tool/
├── agent.go
├── execution_policy.go
├── registry.go
├── view.go
├── read_cache.go
├── scoped_executor.go
└── *_test.go

internal/permission/
├── scoped.go
├── identity.go
├── ticket.go
└── *_test.go

internal/conversation/
├── snapshot.go
├── message.go       — RoleSubagentNotification 与 bounded 持久摘要
├── jsonl_record.go  — 新 role 的校验、版本迁移和去重读取
└── snapshot_test.go

internal/provider/
├── prompt_snapshot.go
├── request.go
├── anthropic_request.go
├── openai_request.go
└── *_test.go

internal/hook/
├── api.go
├── engine.go
├── lifecycle.go     — 每任务 SessionStart/BeginTurn/EndTurn/SessionEnd 契约
├── event.go
└── *_test.go

internal/orchestrator/
├── subagent_runner.go
├── system_tool_router.go
├── task_runtime.go
├── result_projection.go
├── agent_loop.go
├── execution_state.go
├── chat.go
├── tool_scheduler.go
├── tool_definitions.go
├── run_tracker.go
└── *_test.go

internal/command/
├── task_intent.go
├── builtins.go
├── definition.go
└── *_test.go

internal/app/
├── tasks.go
├── app.go
├── deps.go
├── state.go
├── update.go
├── events.go
├── lifecycle.go
└── *_test.go

internal/tui/
├── tasks.go
├── task_detail.go
├── view_model.go
├── layout.go
├── confirmation.go
├── status.go
└── *_test.go

internal/testutil/
├── subagent_fixture.go
└── *_test.go

internal/config/
├── partial.go       — model_aliases/subagent presence-aware fields
├── config.go        — resolved ModelAliases/SubagentConfig
├── resolve.go       — defaults and overflow-safe conversion
├── validate.go      — alias/limit hard validation
└── *_test.go

cmd/xagent/
├── assembly_subagents.go
├── assembly.go
├── assembly_context.go
├── lifecycle.go
├── main.go
└── *_test.go

config.example.yaml       — llm.model_aliases 与顶层 subagent.role_limits/runtime limits 示例
~~~

明确不修改或不新增第二入口的部分：

`assembly_subagents.go` 提供唯一共享的子 Agent 组装函数；候选 `assembly.go` 和现有生产 `main.go` 都必须调用它并完成同样的 Registry.Seal → agentrole.Manager → TaskManager 接线，不能只接一条入口或保留第二套运行时。

- 不改造 internal/skill 的领域类型；
- 不复用 IndependentID 表示任务；
- 不把后台任务加入现有 runTracker；
- 不把任务页接入现有会话保存/导航事务；
- 不新增 Worktree、团队编排或跨会话任务文件。

## 技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 角色领域边界 | 新建 internal/agentrole，不改造 internal/skill 类型 | 角色增加 deny list、权限模式、插件来源和未声明/显式空白名单语义，避免影响现有 Skill |
| 角色来源 | 项目 > 用户 > 内置 > 插件 | 与 Spec 固定优先级一致；插件统一归入一个 tier |
| 无效高优先级角色 | 只记录脱敏诊断，不遮蔽低层有效角色 | 高层损坏时低层仍可用；有效同层重名拒绝候选快照 |
| 快照发布 | 候选全量校验后一次性原子替换 | 运行任务绑定旧 generation，不观察到半更新状态 |
| 角色格式 | 严格 YAML KnownFields + 非空 Markdown 正文 | 未知字段和非法值 fail-closed，避免拼写错误改变安全边界 |
| 模型选择 | 仅 inherit/haiku/sonnet/opus，映射存放在 llm.model_aliases | 不允许角色提交任意模型 ID；每任务通过 ChatRequest.Model 覆盖 |
| 模型别名缺失 | 委派准备阶段返回 model_alias_unavailable | 禁止静默回退或影响全局 Provider |
| Agent 工具 | 一个 Agent Schema，type 字段区分 defined/fork | 保持名称、参数和事件契约稳定，模型与 TUI 共用 Submit |
| Agent 执行方式 | RouteSystem sentinel + Orchestrator 路由 | 普通 Executor 不直接创建任务，避免第二提交路径 |
| 防递归 | 全局禁止 Agent，并在预检层再次拒绝伪造调用 | 过滤 View 不用 AlwaysInclude 复活 Agent |
| Registry 生命周期 | MCP、内置工具和 Agent 全部注册后 Seal | 角色元信息、Provider 定义和执行 View 来自同一快照 |
| 工具过滤 | 父集合与角色 allow 取交集，再减 deny、全局禁止；仅 background placement 再与后台集合取交集，前台/后台双 View 在 Detach 原子切换 | 每一层只收窄，不允许后层重新开放工具 |
| 默认后台工具 | ReadOnly、SideEffectFree、ConcurrentSafe 三项同时满足 | 明确区分只读和无副作用；首版包含 Read、Glob、Grep |
| 后台工具扩展 | 配置可收窄或扩展已注册名称 | 扩展仍经过角色、权限、Plan、Hook 和硬安全链 |
| 任务调度器 | 独立 TaskManager，不复用 runTracker 或 Hook async pool | 支持稳定 ID、队列、单任务取消和事件查询 |
| 并发限制 | 最多 4 个 Worker、最多 32 个等待项 | 转后台不会突破上限；超限立即返回 queue_full |
| Context 所有权 | 从提交开始由 Manager 创建任务 Context | 通过可解除取消桥实现父请求与后台任务解耦 |
| 前后台切换 | Placement 与 Status 正交，Detach 只改承载方式 | 保留 ID、消息、工具进度和事件序号，不重启 |
| 自动后台阈值 | 默认首轮模型请求 10 秒，可配置 | 短任务保持交互反馈，慢首轮释放主交互 |
| Defined 上下文 | 空白 Conversation + 标准环境 + 角色正文 + 任务 | 不继承父消息、父 Activity 或当前 Skill |
| Fork 上下文 | 捕获 Provider 请求前的 Conversation/Prompt/Tool/Mode 快照 | 委派后父变化不可见，保留稳定请求前缀 |
| Fork 角色位置 | 追加 Dynamic System block，任务作为末尾请求消息 | 不污染父稳定缓存前缀 |
| Prompt Cache | 系统断点始终可继承；工具断点只在工具指纹一致时继承 | 安全收窄优先于缓存命中，绝不暴露被禁止工具 |
| 权限作用域 | 复制只读规则层，创建独立 Session、确认队列和票据 | 父临时授权不继承，任务授权不反向影响父 |
| 子任务永久授权 | 首版禁用，保留一次性和任务内 Session 授权 | 避免后台并发写共享权限文件 |
| Hook 身份 | 每任务独立 subagent TaskID Session/Execution | 共享 Hook Engine，但不继承父 Session Prompt |
| 文件读取缓存 | 每任务独立、版本校验且有界 | 避免缓存和文件状态串线 |
| 最大轮次 | min(globalMax, roleMax)，显式 0 直接 limit_reached | 角色不能扩大资源上限，且不多发 Provider 请求 |
| 事件模型 | subagent.Event 包装安全 events.Event，增加 TaskID/Revision/Sequence | 不复用主请求 envelope 或 IndependentID |
| 事件限额 | 文本/思考增量无损合并并标记范围；确认、工具和终态不可丢 | 同时满足有界保留和终态一致性 |
| Completion 来源 | 终态只构造一次不可变 Completion | Manager、TUI、事件和 Inbox 字段一致 |
| 结果回流 | 主 Agent 只在安全边界消费结构化 subagent_result | 不自动启动新模型回合，不写入完整轨迹 |
| 用户入口 | /agent、/tasks、/task value-only 意图 | 命令层不依赖 TUI，模型与用户共用任务服务 |
| 任务界面 | 独立 tasks/task_detail 屏幕 | 后台任务不受会话导航事务阻塞 |
| 任务持久化 | 仅进程内有界保留 | 符合本轮不做跨会话/跨进程恢复 |
| 关闭顺序 | 先 TaskManager，再普通交互，最后 Hook/MCP/Provider | 防止任务在共享基础设施关闭后继续工作 |
| 测试隔离 | Runner、Provider、TUI 都提供替代实现 | 可在无真实 Provider/完整 TUI 环境验证核心行为 |

### 关键取舍

1. Fork 的工具过滤可能改变父请求的工具定义。此时只继承字节一致的系统缓存边界，关闭不匹配的工具缓存断点；安全收窄优先于工具缓存命中。

2. 后台任务不加入普通 runTracker。导航和普通消息只等待交互请求；应用关闭通过 TaskManager 的独立 Shutdown 有界收尾后台任务。

3. 子 Agent 不使用现有独立 Skill 的浅拷贝路径。现有独立执行逻辑只复用 Agent Loop、事件脱敏和工具调度算法；权限、确认、消息和读缓存均通过新的任务作用域创建。

4. 任务终态不追加完整工具轨迹。同步前台 Agent 调用可产生配对工具结果；异步任务只通过结构化、脱敏、限长的通知回灌。

## Spec 覆盖自检

| Spec 范围 | 主要归属 |
|---|---|
| F1–F4、N1 | Agent 工具、TaskManager Submit、TaskIntent |
| F5–F12、N2–N4、N14、N16 | agentrole Parser、Snapshot、ModelCatalog、Config |
| F13–F18、N5–N9 | RunnerFactory、Conversation/Prompt Snapshot、CapabilitySet、TaskScope |
| F19–F23、N10、N18 | Orchestrator Agent Loop、Confirmation Broker、Task Context、Shutdown |
| F24–F28、N8–N9、N19 | Tool View、ExecutionPolicy、Registry Seal、Task Scheduler |
| F29–F33、N11–N13、N17 | TaskManager、EventHub、TaskSnapshot、Completion |
| F34–F38、N12、N14–N15、N20–N22 | ResultInbox、App/TUI、脱敏投影、Assembly/Lifecycle |

所有 F 需求均有明确模块归属；普通对话、Skill、权限、Plan Mode、Hook、Memory、MCP 和会话存储在无子任务路径下继续使用既有入口。
