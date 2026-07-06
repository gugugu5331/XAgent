# Session Memory Restore Plan

## 架构概览

本章拆成四个互相独立、在 orchestrator 请求前汇合的模块：项目指令、JSONL 会话存档、自动记忆、上下文注入协调器。

### 1. 项目指令模块：`internal/instructions`

负责加载三层手写 Markdown 指令：项目根、项目根下 `.mewcode/`、用户目录 `~/.mewcode/`。模块输出一组带优先级和诊断信息的 prompt section，供现有 `prompt.OptionalStableSections` 注入。`@include` 在该模块内完成展开，统一处理最大深度、visited 防环、realpath 边界校验、打开后文件校验和非法引用诊断。

### 2. JSONL 会话存档模块：`internal/conversation`

保留现有 `ConversationStore` 接口，对底层存储实现做替换或新增实现：每个会话一个 JSONL 文件，每行是带版本和事件类型的记录。会话列表不读 meta 文件，而是扫描 JSONL 得出 ID、标题、消息数、更新时间。`Save` 采用增量追加语义，只写入尚未持久化的消息；检测到行数冲突或重复追加风险时返回诊断或写入 snapshot 修复记录。旧 JSON 会话通过兼容加载或只读迁移进入新模型，迁移失败不删除原文件。

### 3. 自动记忆模块：`internal/memory`

负责用户级和项目级长期笔记：每条笔记是带 frontmatter 的 Markdown，另有索引文件。模块提供读取索引、异步更新笔记、重建索引、删除笔记、禁用自动记忆等能力。自动更新只在 Agent Loop 自然完成后触发；候选更新请求交给 LLM 判断新增、合并或忽略，并在写入前做敏感信息过滤。

### 4. 上下文注入协调器：`internal/sessionctx`

负责在每次 provider 请求前汇总上下文来源，并按优先级交给 prompt/orchestrator：

1. 固定系统提示
2. 项目级指令
3. 用户级指令
4. 记忆索引
5. 会话恢复提示
6. 当前会话历史
7. 当前用户输入

该模块不直接拼接最终 prompt，而是输出 `prompt.Section` 和必要的 `conversation.Message`，复用现有 `prompt.Build`、`conversation.ContextMessages` 和 `contextmgr.Prepare`。这样可以避免把项目指令、长期记忆和会话压缩逻辑混在一个函数里。

### 5. App/Orchestrator 接入

`cmd/xagent/main.go` 启动时创建 JSONL 会话存储、指令加载器、记忆管理器和会话上下文协调器，并通过 `app.Deps` 与 orchestrator options 注入运行时。`orchestrator.stream` 在构造 `provider.ChatRequest` 前执行：

1. 加载/刷新项目指令和记忆索引。
2. 恢复或修正会话异常状态。
3. 调用已有 `contextmgr.Prepare` 处理 token 超限。
4. 将额外 stable sections 传入 `prompt.Build`。
5. 将上下文消息传给 provider。

Agent Loop 完成且 stop reason 为 completed 时，异步触发自动记忆更新；其他 stop reason 不触发。

## 核心数据结构

### `instructions.Source`

```go
type Source struct {
    Name      string
    Path      string
    Priority  int
    Scope     Scope
    Content   string
    Diagnostics []Diagnostic
}
```

表示一份展开后的指令来源。`Priority` 数值越小越靠前，项目级高于用户级。`Scope` 区分项目根、项目 `.mewcode`、用户目录。指令 section 只在 optional stable sections 内排序，固定系统提示仍保持最高优先级。

### `instructions.Loader`

```go
type Loader struct {
    ProjectRoot string
    UserDir     string
    MaxDepth    int
    MaxBytes    int64
}
```

负责加载三层指令文件和展开 `@include`。内部维护 visited 集合，所有 include 路径先 realpath 再判断是否仍在允许目录内；打开文件后再次校验文件类型、大小和真实路径，或拒绝可疑符号链接以降低 TOCTOU 风险。

### `conversation.JSONLRecord`

```go
type JSONLRecord struct {
    Version   int
    Type      RecordType
    Time      time.Time
    Message   *Message
    Snapshot  *Conversation
}
```

每行 JSONL 的可演进记录。常规消息用 `Message`，旧格式迁移或未来压缩恢复可用 `Snapshot`，但真实列表信息仍从 JSONL 扫描得出。

### `conversation.JSONLStore`

```go
type JSONLStore struct {
    Dir              string
    CanonicalRoot    string
    RetentionDays    int
    MaxScanFiles     int
    MaxScanBytes     int64
    LegacyStore      ConversationStore
    persistedCounts  map[string]int
}
```

实现现有 `ConversationStore` 接口。负责创建会话、追加消息、加载恢复、扫描列表、旧格式兼容和过期清理。内部用单进程锁保护追加写，并用 `persistedCounts` 记录每个会话已经持久化的消息数量，确保 `Save` 只追加增量消息；如果磁盘记录与内存计数冲突，则返回诊断或写入 snapshot 修复记录，不能重复追加完整历史。

### `conversation.RecoveringStore`

```go
type RecoveringStore interface {
    Recover(ctx context.Context, id string) (*Conversation, RecoveryReport, error)
}
```

JSONL store 额外实现的恢复接口。`ConversationStore.Load` 保持现有兼容签名；需要恢复诊断的调用方先检测 store 是否实现 `RecoveringStore`，否则退回普通 `Load`。

### `conversation.RecoveryReport`

```go
type RecoveryReport struct {
    SkippedBadLines      int
    TruncatedFromLine    int
    InsertedGapReminder  bool
    MigratedLegacy       bool
    Diagnostics          []Diagnostic
}
```

描述一次恢复过程中发生了什么，用于 TUI 状态和测试断言。

### `memory.ProjectIdentity`

```go
type ProjectIdentity struct {
    RootRealPath string
    ConfigHash   string
    ID           string
}
```

表示项目级记忆的稳定身份。`ID` 由规范化真实项目根和有效配置摘要生成，用于项目记忆目录、索引隔离、删除和清理范围校验。

### `memory.Note`

```go
type Note struct {
    ID          string
    Type        NoteType
    Scope       Scope
    Title       string
    Source      string
    Confidence  string
    CreatedAt   time.Time
    UpdatedAt   time.Time
    Body        string
    Supersedes  []string
}
```

表示一条长期笔记。`Type` 包括用户偏好、纠正反馈、项目知识、参考资料。`Scope` 区分用户级和项目级。`Supersedes` 用于用户纠正旧记忆时标记覆盖关系。

### `memory.Index`

```go
type Index struct {
    Scope      Scope
    Path       string
    Lines      []string
    Bytes      int
    Diagnostics []Diagnostic
}
```

表示可注入上下文的记忆索引。索引必须控制在 200 行和 25KB 内。超限策略为：先重建索引；重建后仍超限则截断低优先级条目；如果单条笔记导致索引超限，则拒绝写入该笔记并记录诊断。

### `memory.Manager`

```go
type Manager struct {
    Provider    provider.Provider
    UserDir     string
    ProjectDir  string
    Enabled     bool
    MaxIndexLines int
    MaxIndexBytes int
}
```

负责读取索引、异步更新笔记、删除笔记、重建索引、敏感信息过滤和失败诊断。

### `sessionctx.Manager`

```go
type Manager struct {
    Instructions *instructions.Loader
    Memory       *memory.Manager
    Context      *contextmgr.Manager
}
```

统一协调请求前上下文准备。它不直接调用 provider，而是返回 prompt sections、恢复诊断和是否需要保存会话。`PreparedContext` 不返回替代 messages；需要插入时间跨度提醒、恢复提示或压缩结果时直接修改 `conversation.Conversation`，最终仍由 `conversation.ContextMessages` 统一生成 provider messages。

### `sessionctx.PreparedContext`

```go
type PreparedContext struct {
    StableSections []prompt.Section
    MessagesChanged bool
    Diagnostics    []Diagnostic
}
```

表示一次请求前准备结果。`StableSections` 进入 `prompt.Build`，`MessagesChanged` 表示会话被修复、压缩或插入时间提醒后需要保存。

## 模块设计

### `internal/instructions`

**职责：**

- 加载三层项目指令：
  1. 项目根目录指令
  2. 项目 `.mewcode/` 指令
  3. 用户目录 `~/.mewcode/` 指令
- 按优先级输出 stable prompt section。
- 展开 `@include` 引用。
- 对 include 做安全校验：最大深度、visited 防环、realpath 边界、打开后文件校验、文件大小限制。
- 记录非法 include、循环引用、过深引用等诊断。

**对外接口：**

```go
func (l Loader) Load(ctx context.Context) ([]prompt.Section, []Diagnostic)
```

**依赖：**

- `internal/prompt`
- 标准库文件系统接口
- 不依赖 `orchestrator`、`provider` 或 `conversation`

---

### `internal/conversation`

**职责：**

- 新增 JSONL 存储实现，同时保持现有 `ConversationStore` 接口。
- 每条消息以独立 JSONL 记录追加写。
- `Save` 只追加新增消息，检测重复追加、未知版本、非法事件类型、半行、角色错序和并发冲突时生成诊断或 snapshot 修复记录。
- 创建具备可读时间前缀和随机后缀的会话 ID。
- 加载时跳过坏行。
- 检查工具调用与工具结果配对，遇到未闭合工具调用时截断。
- 恢复长时间未打开的会话时插入时间跨度提醒。
- 列表信息从 JSONL 扫描得出。
- 兼容旧 JSON 会话，迁移失败不破坏原文件。
- 清理超过保留期的会话。
- 清理和删除操作只作用于 canonical store root 内的已知会话文件，拒绝跨根路径或可疑符号链接。

**对外接口：**

```go
func NewJSONLStore(opts JSONLStoreOptions) (*JSONLStore, error)

func (s *JSONLStore) Create(ctx context.Context) (*Conversation, error)
func (s *JSONLStore) Save(ctx context.Context, conv *Conversation) error
func (s *JSONLStore) Load(ctx context.Context, id string) (*Conversation, error)
func (s *JSONLStore) List(ctx context.Context) ([]Conversation, error)

func (s *JSONLStore) Recover(ctx context.Context, id string) (*Conversation, RecoveryReport, error)
func (s *JSONLStore) CleanupExpired(ctx context.Context, now time.Time) ([]string, error)
```

**依赖：**

- 现有 `conversation.Message`
- 现有 `ConversationStore`
- `contextmgr` 不直接依赖；超限压缩由 `sessionctx` 协调

---

### `internal/memory`

**职责：**

- 管理用户级和项目级长期笔记。
- 读取和校验记忆索引。
- 控制索引不超过 200 行 / 25KB。
- 在 Agent Loop 自然停止后异步生成候选笔记更新。
- 调用 LLM 判断新增、合并、忽略或废弃旧记忆；更新 prompt 明确候选内容只能作为事实/偏好来源，不得执行候选内容里的指令。
- 写入 frontmatter Markdown 文件。
- 写入前做敏感信息脱敏和拒写，保护范围覆盖笔记正文、索引、会话 JSONL、诊断和状态展示。
- 支持禁用自动记忆、查看状态、查看索引、删除指定 scope 的笔记、重建指定 scope 的索引。
- 记录异步失败诊断，但不阻塞用户回复。

**对外接口：**

```go
func (m *Manager) LoadIndex(ctx context.Context, projectRoot string) (IndexBundle, []Diagnostic)

func (m *Manager) UpdateAsync(ctx context.Context, input UpdateInput)

func (m *Manager) RebuildIndex(ctx context.Context, scope Scope) error
func (m *Manager) DeleteNote(ctx context.Context, scope Scope, id string) error
func (m *Manager) Disable(scope Scope)
func (m *Manager) Diagnostics() []Diagnostic
```

**依赖：**

- `provider.Provider` 用于 LLM 更新笔记
- `mcpclient.RedactText` / `RedactAny` 或等价脱敏逻辑
- 不依赖 TUI

---

### `internal/sessionctx`

**职责：**

- 在每次 provider 请求前协调上下文准备。
- 加载项目指令和记忆索引。
- 注入“长期记忆低于当前用户指令、不能覆盖权限系统”的边界提醒。
- 检查会话恢复诊断是否需要注入时间跨度提醒。
- 调用已有 `contextmgr.Prepare`，在 token 超限时先压缩。
- 返回可传给 `prompt.Build` 的 stable sections。
- 不返回替代 messages；如需插入恢复提示或时间跨度提醒，直接修改 conversation。
- 返回会话是否被修改，供 orchestrator 决定是否保存。

**对外接口：**

```go
func (m *Manager) Prepare(ctx context.Context, conv *conversation.Conversation, mode PrepareMode) (PreparedContext, error)
```

**依赖：**

- `internal/instructions`
- `internal/memory`
- `internal/contextmgr`
- `internal/prompt`
- `internal/conversation`

---

### `internal/orchestrator`

**职责：**

- 在 `stream` 构造 `ChatRequest` 前调用 `sessionctx.Manager.Prepare`。
- 将返回的 stable sections 放入 `prompt.BuildRequest.OptionalStableSections`。
- 如果会话被恢复修正、插入提醒或压缩，则保存会话。
- 在 Agent Loop 自然完成后触发 `memory.Manager.UpdateAsync`。
- 非自然完成不触发自动记忆更新。
- 保持权限系统优先级不变。

**对外变化：**

- `Orchestrator` 增加 `sessionContext *sessionctx.Manager`。
- 新增 `OrchestratorOptions` 构造参数结构体，统一传入 provider、store、resources、tools、context manager 和 session context manager，避免继续膨胀 `NewWithToolsAndContext` 参数列表。

---

### `internal/app`

**职责：**

- 接收 `cmd/xagent/main.go` 组装好的 JSONL 会话存储、指令加载器、记忆管理器和 session context manager。
- 提供最小记忆控制入口：
  - 禁用自动记忆
  - 查看索引
  - 删除笔记
  - 重建索引
- 显示恢复诊断和记忆更新失败诊断。
- 仍然通过现有 TUI 输入/状态机制呈现，不新增复杂 UI 框架。

**对外变化：**

- 增加本地命令：`/memory status`、`/memory index`、`/memory off`、`/memory delete <scope> <id>`、`/memory rebuild <scope>`。
- 命令不发送给 LLM。

## 模块交互

### 新会话启动

1. `cmd/xagent/main.go` 初始化 JSONL 会话存储、指令加载器、记忆管理器和 session context manager，并通过依赖结构传入 app/orchestrator。
2. 用户进入新会话时，conversation store 创建新 JSONL 会话 ID。
3. 用户提交第一条消息。
4. `orchestrator.stream` 调用 `sessionctx.Prepare`。
5. `sessionctx` 加载项目/用户指令和记忆索引，返回 stable sections。
6. `orchestrator` 调用 `prompt.Build`，把 stable sections 放入 `OptionalStableSections`。
7. provider 请求得到完整上下文：固定系统提示 + 指令 + 记忆索引 + 当前会话。

### 恢复旧会话

1. `app.loadConversation` 优先检测 store 是否实现 `RecoveringStore` 并调用 `Recover`；否则退回普通 `Load`。
2. store 跳过坏行，校验事件顺序，遇到未闭合工具调用时截断。
3. 如果距离上次活动太久，插入时间跨度提醒。
4. 返回 conversation 和 recovery diagnostics。
5. TUI 状态显示恢复诊断。
6. 下一次请求前 `sessionctx` 调用 `contextmgr.Prepare`，如果上下文超限先压缩。

### Agent Loop 自然完成后的自动记忆

1. `runAgentLoop` 正常完成，stop reason 为 completed。
2. orchestrator 复制本轮用户输入、最终回复、会话摘要和当前记忆索引。
3. 调用 `memory.UpdateAsync` 后立即返回，不阻塞 UI。
4. memory worker 调 LLM 判断新增、合并、忽略或废弃笔记。
5. 写入用户级或项目级 Markdown 笔记。
6. 重建索引；如果索引超过限制，先重建，再截断低优先级条目；如果单条笔记导致超限，拒绝写入并记录诊断。

### 记忆控制命令

1. 用户输入 `/memory status`、`/memory index`、`/memory off`、`/memory delete <scope> <id>` 或 `/memory rebuild <scope>`。
2. app 在本地拦截，不发送给 LLM。
3. app 调用 memory manager。
4. TUI 显示结果或错误。

## 文件组织

```text
internal/
├── diagnostics/
│   └── diagnostic.go      — 跨 instructions/conversation/memory/sessionctx 复用的诊断类型
├── instructions/
│   ├── loader.go          — 三层指令加载与优先级排序
│   ├── include.go         — @include 展开、深度限制、visited 防环
│   ├── sandbox.go         — realpath 边界校验与打开后文件校验
│   └── loader_test.go
├── conversation/
│   ├── jsonl_store.go     — JSONL ConversationStore 实现
│   ├── jsonl_record.go    — JSONL 记录 schema、版本、事件类型
│   ├── recovery.go        — 坏行跳过、工具配对校验、时间跨度提醒
│   ├── cleanup.go         — 30 天过期会话清理
│   └── jsonl_store_test.go
├── memory/
│   ├── note.go            — Note、NoteType、Scope、frontmatter
│   ├── manager.go         — 记忆管理入口
│   ├── index.go           — MEMORY.md 索引读写、大小限制
│   ├── update.go          — 异步 LLM 更新、合并/忽略/废弃
│   ├── redact.go          — 写入前脱敏
│   └── manager_test.go
├── sessionctx/
│   ├── manager.go         — 请求前上下文协调
│   ├── priority.go        — 指令/记忆/恢复提示优先级
│   └── manager_test.go
├── app/
│   ├── app.go             — 初始化依赖
│   └── update.go          — /memory 本地命令
├── orchestrator/
│   ├── chat.go            — 请求前 Prepare 接入
│   └── agent_loop.go      — completed 后触发自动记忆
└── config/
    ├── config.go          — memory/instructions/session 配置
    ├── load.go            — 默认值
    └── validate.go        — 配置校验

cmd/
└── xagent/
    └── main.go            — 组装 JSONL store、instructions、memory、sessionctx 并注入 app

docs/session-memory-restore/
├── spec.md
├── plan.md
├── task.md
└── checklist.md
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 指令注入位置 | 作为 `prompt.OptionalStableSections` 注入 | 复用现有 prompt 缓存与 section 优先级机制 |
| 指令优先级 | 项目根 > 项目 `.mewcode` > 用户 `~/.mewcode` | 项目约束比个人偏好更具体，优先遵循 |
| include 安全 | realpath 边界校验 + 打开后文件校验 | 防 `..`、symlink、路径混淆和 TOCTOU 风险 |
| 会话存储 | 新增 JSONL store，实现现有 `ConversationStore`，`Save` 只追加增量 | 不破坏调用方接口，同时避免重复追加完整历史 |
| 会话元数据 | 不维护 meta 文件，只扫描 JSONL，可用内存缓存加速 | 满足 spec，避免双写状态不一致 |
| 旧格式兼容 | 优先只读加载旧 JSON；迁移成功后写 JSONL，失败保留原文件 | 降低破坏历史会话的风险 |
| 自动记忆触发 | 只在 stop reason 为 completed 时异步触发 | 避免错误、中断和工具循环污染长期记忆 |
| 记忆去重 | LLM 根据已有索引判断新增/合并/忽略 | 符合本阶段“不做复杂检索”的边界 |
| 记忆索引 | 只注入受限索引，不注入全文；超限先重建、再截断低优先级、最后拒写单条超限笔记 | 控制 token 成本，降低隐私暴露面 |
| 用户控制 | 先用本地 slash 命令，不引入完整命令框架 | 最小改动满足查看、禁用、删除、重建 |
| 诊断展示 | 通过 status notice/error 和测试可读 report 暴露 | 不先扩复杂 TUI，但保证可观测 |
| 敏感信息过滤 | 写长期记忆、会话 JSONL、索引和诊断前统一脱敏与拒写 | 防工具输出和配置密钥进入持久存储或可见状态 |
| 初始化位置 | 在 `cmd/xagent/main.go` 组装依赖，`app.Deps` 只接收依赖 | 符合现有启动结构，避免 app 层承担构造职责 |
| Orchestrator 构造 | 使用 `OrchestratorOptions` 替代继续扩展长参数列表 | 避免构造函数参数膨胀和调用点脆弱 |
| 项目身份 | realpath(projectRoot) + 有效配置 hash | 支撑项目级记忆隔离、清理和索引目录稳定性 |
| sessionctx 消息契约 | 不返回替代 messages，只修改 conversation 并返回 changed/diagnostics | 统一由 `conversation.ContextMessages` 生成 provider 历史，避免上下文来源分散 |

## 测试验收映射

| 验收 | 测试类型 | Fixture / 操作 | 可观测结果 |
|------|----------|----------------|------------|
| AC1-AC2, AC19-AC20 | 单元 + 集成 | 项目/用户指令、合法 include、循环 include、越界路径、项目外 symlink | section 顺序正确，非法 include 被跳过并产生诊断 |
| AC3-AC7, AC21-AC22 | 单元 + 集成 | 多轮 JSONL、坏行、半行、未知版本、错序角色、伪造 tool_result、旧 JSON | 可恢复历史保留，异常部分跳过/截断，原文件不被破坏 |
| AC8-AC9 | 集成 | 超长会话、长时间未打开会话 | 请求前触发压缩，插入时间跨度提醒 |
| AC10, AC25-AC26 | 单元 | 过期会话、跨项目 project ID、canonical root 外路径 | 只清理允许范围内过期文件，跨项目记忆不注入 |
| AC11, AC15, AC17, AC23, AC27 | 单元 + 集成 | completed stop reason、非 completed stop reason、重复记忆、纠正旧记忆、候选 prompt injection | 只在自然完成后异步更新，重复合并/忽略，指令性候选不变成规则 |
| AC12-AC14, AC18, AC28 | 单元 | 四类 note、frontmatter、索引超限、敏感 token/API key/authorization | note/index 格式正确，超限策略生效，持久文件和诊断无明文敏感值 |
| AC24 | app 集成 | `/memory status/index/off/delete <scope> <id>/rebuild <scope>` | 命令本地拦截，不进入 LLM，成功/失败可见 |
| 端到端 | tmux | 启动 XAgent，创建指令、旧会话、memory fixture，发送真实请求 | provider 请求前包含指令和索引，恢复诊断可见，异步记忆不阻塞回复 |
