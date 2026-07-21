# Skill System Plan

## 架构概览

采用“全局有效快照 + 每个执行上下文独立活动集”的结构，避免把热更新状态、会话状态和 Agent Loop 状态混在一起。

```text
三级 Skill 来源
      │
      ▼
Skill Loader / Parser
      │ 解析、覆盖、冲突与工具校验
      ▼
Skill Manager ──────── 原子 Effective Snapshot
      │                          │
      │                          ├─ 轻量目录：名称 + 说明
      │                          ├─ 动态斜杠命令
      │                          └─ 按需取得完整定义
      │
      ├───────────────┐
      ▼               ▼
主会话 Activity    独立执行 Activity
共享 Skill 快照     仅目标 Skill
      │               │
      └──────┬────────┘
             ▼
      Execution Profile
 活动 SOP / 模型 / 工具交集 / 只读资源根
             │
             ▼
       Orchestrator
 Prompt 重建、工具暴露、调用拦截、独立运行
```

核心组件：

1. **Skill Loader / Parser**：扫描单文件和目录型 Skill，只负责格式、路径、大小限制及来源信息。单文件错误转为诊断；同层重名作为快照级错误。
2. **Skill Manager**：持有三级来源配置、文件指纹和最后一个有效不可变快照。首次加载使用严格模式；热更新使用“构建候选 → 全量校验 → 原子替换”，失败时保留旧快照。
3. **Activity**：主会话和每个独立执行各自持有活动 Skill 快照。激活时复制完整 SOP 和参数结果，因此后续磁盘更新不会改变正在使用的指令。Activity 负责模型一致性与白名单交集。
4. **Execution Profile**：每个 Provider iteration 从 Activity 生成不可变执行配置，包含轻量目录、活动 SOP、请求模型、有效工具名、Skill 只读资源根及独立执行深度。Agent 调用 `load_skill` 后，只在 iteration 边界原子切换 Profile。
5. **Prompt 集成**：轻量 Skill 目录作为稳定系统区块注入；活动 SOP 作为高显著度动态系统区块注入，位置高于普通运行时提醒，但仍低于固定系统安全约束。
6. **工具集成**：基础工具注册中心保持完整不变。每个请求根据运行模式与 Skill 白名单生成只读工具视图，同时在视图和执行拦截层强制保留系统级 `load_skill`。过滤同时作用于 Provider 工具定义和实际调用校验。
7. **Skill Orchestration**：`load_skill` 由 Orchestrator 作为系统工具处理，而不是让普通工具反向控制 Agent。共享 Skill 原子更新当前 Activity；独立 Skill 交给隔离 Runner，并复用现有事件、权限和取消链路。
8. **Independent Runner**：从主历史提取最近 N 个完整用户轮次，创建不持久化的临时会话，使用独立 Activity 和禁用 Memory 更新的执行配置。结束后只返回最终摘要；禁止再次启动独立执行。
9. **命令集成**：App 在输入、补全和请求边界调用热更新检查，以固定命令定义和当前 Skill 快照重新构造命令注册中心。Skill 命令统一调用同一 Skill 执行入口；硬编码 review 命令移除，由内置 review Skill 提供。
10. **Provider 与资源根集成**：Provider 请求增加可选模型覆盖。读取类工具支持请求级额外只读根；写入类工具继续只接受项目根。内置能力包通过嵌入资源加载，必要的辅助资源以受控只读形式提供。

## 核心数据结构与接口

以下签名为 Go 设计基线。

### Skill 定义

```go
package skill

type Source string

const (
    SourceBuiltin Source = "builtin"
    SourceUser    Source = "user"
    SourceProject Source = "project"
)

type Mode string

const (
    ModeShared   Mode = "shared"
    ModeIsolated Mode = "isolated"
)

type Metadata struct {
    Name         string   `yaml:"name"`
    Description  string   `yaml:"description"`
    AllowedTools []string `yaml:"allowed_tools,omitempty"`
    Mode         Mode     `yaml:"mode"`
    History      int      `yaml:"history,omitempty"`
    Model        string   `yaml:"model,omitempty"`
}

type Definition struct {
    Metadata
    Body        string
    EntryPath   string
    PackageRoot string
    Source      Source
    Fingerprint string
}
```

名称规范固定为小写 `[a-z0-9][a-z0-9_-]{0,63}`。YAML 使用严格字段解析，未知字段作为该文件的解析错误，避免拼写错误静默失效。

### 轻量目录与有效快照

```go
type CatalogItem struct {
    Name         string
    Description  string
    Mode         Mode
    SlashEnabled bool
}

type Snapshot struct {
    Generation  uint64
    Fingerprint string
    Catalog     []CatalogItem
    Definitions map[string]Definition
    Diagnostics []diagnostics.Diagnostic
}
```

`Snapshot` 构造完成后不可修改；所有切片和映射在对外返回时复制。`Catalog` 与活动指令都按规范名称排序。

### 来源与加载配置

```go
type Limits struct {
    MaxFiles      int
    MaxEntryBytes int64
    MaxBodyBytes  int
    MaxArgsBytes  int
}

type SourceFS struct {
    Source Source
    FS     fs.FS
    Root   string
}

type ManagerOptions struct {
    Sources         []SourceFS
    ToolNames       []string
    ReservedCommand []string
    Limits          Limits
    Redact          func(string) string
}

type Manager struct {
    // 内部使用 mutex + atomic snapshot
}

func NewManager(options ManagerOptions) (*Manager, error)
func (m *Manager) Snapshot() Snapshot
func (m *Manager) Resolve(name string) (Definition, bool)
func (m *Manager) RefreshIfChanged(ctx context.Context) (RefreshResult, error)

type RefreshResult struct {
    Changed     bool
    Generation  uint64
    Diagnostics []diagnostics.Diagnostic
}
```

`NewManager` 执行严格首次加载；`RefreshIfChanged` 先比较轻量文件清单指纹，只有变化时才解析候选快照。

### 活动状态

```go
type Activated struct {
    Name         string
    Description  string
    Mode         Mode
    Instructions string
    AllowedTools []string
    Model        string
    PackageRoot  string
    Source       Source
    Fingerprint  string
    Args         string
}

type Activity struct {
    // mutex + map[string]Activated
}

func NewActivity() *Activity
func (a *Activity) Activate(def Definition, args string) (Activated, error)
func (a *Activity) Snapshot() ActivitySnapshot
func (a *Activity) Clear()

type ActivitySnapshot struct {
    Active       []Activated
    Model        string
    AllowedTools []string // nil 表示不额外限制
    ReadRoots    []string
}
```

`Activate` 先完成参数限制、模板替换、模型一致性和白名单合并验证，再一次性更新映射。任何失败都不改变原状态。

### 请求执行配置

```go
type ExecutionProfile struct {
    Catalog          []CatalogItem
    Activity         ActivitySnapshot
    Model            string
    AllowedTools     map[string]struct{} // nil 表示全部基础工具
    ReadRoots        []string
    IndependentDepth int
    Persist          bool
    UpdateMemory     bool
}

type ProfileRequest struct {
    Snapshot         Snapshot
    Activity         ActivitySnapshot
    DefaultModel     string
    BaseTools        []string
    ReadOnly         bool
    IndependentDepth int
}

func BuildProfile(request ProfileRequest) (ExecutionProfile, error)
```

`ReadOnly` 由 Orchestrator 根据当前 RunMode 提供，避免 Skill 包反向导入 Orchestrator。

### Skill 调用

```go
type Invocation struct {
    Name   string
    Args   string
    Raw    string
    Origin InvocationOrigin
}

type InvocationOrigin string

const (
    OriginSlash     InvocationOrigin = "slash"
    OriginAgentTool InvocationOrigin = "agent_tool"
)

type PreparedInvocation struct {
    Definition Definition
    Activated  Activated
    Mode       Mode
    History    int
}
```

App 和系统工具都先调用同一个准备入口，确保命令调用与 Agent 自主加载使用相同的解析、冲突和参数规则。

### Orchestrator 执行接口

```go
type SkillRuntime interface {
    PrepareSkill(invocation skill.Invocation, activity *skill.Activity) (skill.PreparedInvocation, error)
}

type RunRequest struct {
    UserText string
    Mode     RunMode
    Profile  skill.ExecutionProfile
}

type RunResult struct {
    FinalText string
    Usage     provider.Usage
    Duration  time.Duration
    Reason    StopReason
}

type IndependentRequest struct {
    Invocation skill.PreparedInvocation
    Main       *conversation.Conversation
    Profile    skill.ExecutionProfile
}

func (o *Orchestrator) RunIndependent(
    ctx context.Context,
    request IndependentRequest,
    out chan<- events.Event,
) (RunResult, error)
```

现有 Agent Loop 会从“只写事件”调整为同时返回 `RunResult`，主执行仍保持原事件行为；独立执行使用返回的 `FinalText` 直接回流，不再发起摘要请求。

### 工具视图与执行保护

```go
type ViewOptions struct {
    AllowedNames  map[string]struct{}
    AlwaysInclude []string
    ReadOnly      bool
}

func (r *Registry) View(options ViewOptions) (*Registry, error)
```

请求开始时生成工具视图；Provider 工具定义、批处理分类和实际调用校验全部使用同一视图。完整基础注册中心与 Executor 保持不变，避免白名单永久修改全局工具状态。

读取工具增加请求级只读根：

```go
type ReadScope struct {
    ProjectRoot string
    ExtraRoots  []string
}
```

该范围随执行 Context 传递，只对 `Read`、`Glob`、`Grep` 生效。

### Provider 模型覆盖

```go
type ChatRequest struct {
    Model         string
    StableSystem  []SystemBlock
    DynamicSystem []SystemBlock
    Messages      []conversation.Message
    Thinking      config.ThinkingConfig
    Tools         []ToolDefinition
    Cache         CachePolicy
}
```

Anthropic 和 OpenAI Provider 优先使用 `ChatRequest.Model`；为空时继续使用原配置模型。

### 命令桥接

`command.Definition` 增加可选展示标记：

```go
type Definition struct {
    // 现有字段
    Badge string // 例如 “Skill/shared”
}
```

`Suggestion` 同步携带 `Badge`。Controller 增加一个渲染无关入口：

```go
ExecuteSkill(name string, args string, raw string) error
```

动态 Skill Handler 只负责调用该接口，不直接依赖 App、Orchestrator 或 TUI。

## 模块设计

### `internal/skill`：领域核心

职责：

- 定义 Metadata、Definition、Snapshot、Activity 和 ExecutionProfile。
- 解析严格 YAML frontmatter 与 Markdown 正文。
- 扫描根目录下的 `*.md` 和直接子目录中的 `SKILL.md`，不把辅助 Markdown 误识别为独立 Skill。
- 执行三级覆盖、同层冲突检测、白名单验证和保留命令判定。
- 维护轻量文件清单指纹与原子有效快照。
- 完成 `{{args}}` 字面替换、活动集模型校验和白名单交集。
- 提取最近 N 个完整用户轮次。
- 生成轻量目录文本和活动 Skill 系统区块内容。

依赖标准库、`internal/diagnostics` 和 `internal/conversation`，明确不依赖 App、TUI、Provider、Orchestrator 或 Command。

解析规则：

- frontmatter 必须以文件开头的 `---` 起始，并由第二个独立 `---` 结束。
- YAML 使用严格字段模式。
- `history` 在 shared 模式必须为 0；isolated 模式允许 0 或正数。
- `allowed_tools` 去空白、拒绝空项、去重后按名称排序，但工具名称保持大小写精确匹配。
- 单文件 Skill 的包根是文件所在目录；目录型 Skill 的包根是包含 `SKILL.md` 的目录。
- 本地路径经过绝对化、符号链接求值和所属来源根校验。

### `internal/skill/builtin`：内置能力包

- 通过嵌入文件系统提供 `commit`、`review`、`test` 三个真实目录型 Skill。
- 每个样板使用与用户 Skill 相同的解析路径，不在代码中构造特殊 Definition。
- 内置目录包含辅助资源时，激活阶段按需物化到进程临时只读目录；进程退出时清理。
- 物化失败按激活错误处理，不污染当前 Activity。

### `internal/prompt`：系统区块组装

新增两个区块：

1. `skill-catalog`：内容仅含名称和说明，作为稳定区块参与 Prompt cache；Skill generation 改变时自然形成新缓存键。
2. `active-skills`：包含系统边界、每个活动 Skill 的名称、来源、包根和完整 SOP。该动态区块排在普通 runtime reminder 前，但固定身份、安全约束和工具安全规则仍在它之前。

活动区块每轮由当前 Profile 重建，不能写入 Conversation。

### `internal/tool`：工具视图与只读资源根

- Registry 增加不可变过滤视图，不复制或重注册底层 Tool。
- 工具视图依次经过基础工具、Skill 白名单、Plan 只读限制，再强制加入 `load_skill`。
- 调用校验和 Provider 定义读取同一个视图。
- `Read`、`Glob`、`Grep` 从 Context 获取请求级 `ReadScope`。
- 额外根只允许读取，且每个目标路径都重新求值符号链接。
- `Write`、`Edit` 和 Bash 的项目根及权限语义保持不变。

`load_skill` 在 Registry 中提供固定的 `name` 与 `args` schema。它是安全级系统工具，但普通 Executor 不直接执行；Orchestrator 在工具批处理前识别并处理，防止反向依赖和普通工具超时包裹独立 Agent 运行。

### `internal/orchestrator`：Skill 执行协调

新增请求级运行状态，持有当前 Activity、当前 ExecutionProfile、独立深度、是否持久化、是否更新 Memory、最终助手文本与累计 Usage。

共享 Skill：

- 斜杠调用在 Agent Loop 启动前完成激活。
- Agent 工具调用在当前 iteration 结束后激活，并为下一 iteration 重建 Profile。
- 多个 `load_skill` 调用严格串行。
- 模型冲突发生在 Activity 提交前，因此失败调用不改变状态。

独立 Skill：

- 从主会话提取已完成轮次，然后追加一条临时用户消息表示原始 Skill 调用。
- 使用相同 Provider、基础 Registry、Executor、Authorizer 和事件通道。
- `Persist=false`、`UpdateMemory=false`。
- 独立内部的共享 Skill 只修改临时 Activity。
- 独立内部再次调用 isolated Skill 返回可恢复工具错误。
- 最终 `RunResult.FinalText` 直接作为摘要；失败或取消时不生成摘要。

Agent 主动调用 isolated Skill 时，该调用成为当前主任务的终止动作。Orchestrator 不把系统工具调用、临时工具轨迹或内部 Conversation 写入主历史；独立最终文本直接作为本轮主助手回复并结束当前 Agent Loop。shared Skill 的加载工具调用仍按正常工具结果继续下一 iteration。

### `internal/provider`：请求级模型

- `ChatRequest.Model` 非空时覆盖 Provider 配置模型。
- Anthropic 与 OpenAI 序列化测试分别验证覆盖值。
- API Key、Base URL、协议、超时和 Thinking 配置不随 Skill 改变。

### `internal/command`：动态定义支持

- Definition 和 Suggestion 增加 Badge。
- Help 与补全菜单显示 `Skill/shared` 或 `Skill/isolated`。
- 提供从稳定基础定义重新构造 Registry 的能力，仍保留启动冲突检测。
- 硬编码 review Definition 删除。
- `/rv` 保留为隐藏兼容命令，固定转发到当前有效 review Skill；它不是可配置 Skill alias。
- 其他固定命令及全部别名进入保留名称集合。

### `internal/app`：交互边界与主 Activity

Model 新增 Skill Manager、主会话 Activity、当前 Skill generation 和基础命令 Definition 副本。

- 在 Enter、Tab 和提交普通请求前调用热更新检查。
- generation 改变时，以基础命令和 `SlashEnabled` Skill 重建 Registry。
- 热更新失败只显示诊断，不替换当前 Registry。
- Skill 命令统一进入 `ExecuteSkill`：shared 激活后使用原始命令作为主用户消息启动普通 Agent Loop；isolated 先选取历史，再记录原始命令并启动 Independent Runner。
- `/clear` 在清消息显示后调用 Activity.Clear。
- 新建、加载会话时创建新的空 Activity。
- Provider 状态栏继续显示应用默认模型；Skill 请求中的实际模型通过执行进度/状态提示展示，结束后恢复默认显示。

### `cmd/xagent`：启动装配

启动顺序调整为：

1. 加载配置与项目根。
2. 构造基础工具和 MCP 工具。
3. 注册系统级 `load_skill` 工具。
4. 得到完整工具名称集合。
5. 构造三级 Skill Manager，执行严格首次加载。
6. 构造 Executor、Orchestrator、Session Context 和 App。
7. 在接受 TUI 输入前完成 Skill、工具和命令冲突检查。

## 模块交互

### 启动加载

```text
Main
  → 注册内置工具
  → 启动 MCP 并注册可用 MCP 工具
  → 注册 load_skill
  → 收集完整工具名
  → Skill Manager 扫描 builtin/user/project
      → 单文件错误转诊断并跳过
      → 同层重名则失败
      → 三级覆盖
      → 校验最终白名单
      → 标记短命令冲突
      → 发布 generation=1 快照
  → App 用基础命令 + Skill 命令构造 Registry
  → 启动 TUI
```

### 共享 Skill 短命令

```text
用户输入 /commit message
  → App 热更新检查
  → Command Registry 命中 Skill handler
  → Manager 从当前快照解析 commit
  → Activity.Activate(commit, "message")
      → 替换 {{args}}
      → 校验模型冲突
      → 计算候选白名单交集
      → 原子提交
  → 主会话追加原始用户消息 "/commit message"
  → 构建 Execution Profile
  → Agent Loop
      → 轻量目录进入 stable system
      → commit SOP 进入 active-skills
      → 使用 Skill 模型或默认模型
      → 只暴露有效工具视图
  → 助手和工具历史正常保存在主会话
```

后续普通输入复用同一个 Activity；SOP 不会被复制为用户消息。

### Agent 自主加载共享 Skill

```text
普通用户请求
  → 首轮 Provider 只看到 Skill 目录与 load_skill
  → Provider 调用 load_skill(name, args)
  → Orchestrator 串行处理系统工具
  → 准备并原子激活共享 Skill
  → 保存正常 tool_call / tool_result
  → 为下一 iteration 重建 Execution Profile
  → 后续 iteration 开始携带完整 SOP、模型覆盖和工具交集
```

### 独立 Skill 短命令

```text
用户输入 /review target
  → App 热更新检查并解析 Skill
  → 在写入新命令前，从主历史复制最近 N 个完整轮次
  → 主会话保存原始 "/review target"
  → 创建临时 Conversation
      → 放入复制轮次
      → 追加临时用户调用
  → 创建只含 review 的临时 Activity
  → Independent Runner 执行
      → 使用独立模型/白名单/只读根
      → 转发流式文本、工具进度、权限确认
      → 不保存临时 Conversation
      → 不更新 Memory
  → 成功：最终文本保存为主会话 Assistant 消息
  → 失败/取消：显示错误，不写成功摘要
```

独立工具事件标记为 transient。TUI 实时展示，但完成后只保留最终摘要；重新从主 Conversation 渲染时不会出现临时工具轨迹。

### Agent 自主加载独立 Skill

```text
主 Agent 调用 load_skill(isolated)
  → Orchestrator 暂停父 Agent Loop
  → 不把该系统工具调用写入主 Conversation
  → 使用已完成的主历史轮次启动 Independent Runner
  → 临时事件继续发往当前 TUI
  → 成功：把最终文本作为当前主轮次的 Assistant 回复
  → 结束父 Agent Loop，不再调用主模型总结
```

### 工具过滤与执行

```text
基础 Registry
  ∩ Skill allowed_tools
  ∩ RunMode 允许集合
  + load_skill
      │
      ├─ 生成 Provider Tool Definitions
      └─ 校验模型返回的每个 Tool Call
```

即使模型伪造未暴露工具名，执行层仍拒绝。通过过滤的危险工具继续经过 Authorizer 和用户确认。

### 模型覆盖

```text
ActivitySnapshot.Model
  ├─ 非空 → ChatRequest.Model
  └─ 空   → 配置默认模型
```

Agent 自主加载共享 Skill 后，模型覆盖从下一 iteration 生效。独立运行结束或 Activity.Clear 后，不修改 Provider 本身，因此自然恢复默认模型。

### 热更新

```text
Enter / Tab / submit
  → Manager 比较来源清单指纹
  → 无变化：立即返回
  → 有变化：
      → 构造完整候选快照
      → 跳过单文件解析错误
      → 检查同层重名
      → 三级覆盖
      → 校验白名单
      → 计算动态命令
      → 成功：原子发布新 generation
      → 失败：保留旧 generation
  → generation 改变时重建 Command Registry
```

当前 Activity 存的是 Definition 副本，不引用 Manager 映射，因此刷新、删除或覆盖不会改变活动 SOP。重新调用同名 Skill 时才从最新 generation 取定义。

### 清除与会话生命周期

```text
/clear
  → 清空 MessagesView
  → Activity.Clear
  → 状态恢复默认模型与无限制 Skill 工具集
  → Conversation.Messages 保持不变

新建会话 / 加载会话
  → 创建新的空 Activity
  → 不从 Conversation 恢复 Skill 状态

应用重启
  → 重新发现目录
  → Activity 初始为空
```

### 目录资源读取

```text
活动 Skill PackageRoot
  → 规范化并加入 Profile.ReadRoots
  → Context 传给 Read / Glob / Grep
  → 每次路径访问重新校验真实路径属于 ProjectRoot 或 ReadRoots
```

额外根不会传给写入工具，也不会改变 Bash 或权限系统的边界。

## 文件组织

```text
internal/
├── skill/
│   ├── types.go                 # Metadata、Definition、Snapshot、Mode、Source
│   ├── parser.go                # strict YAML frontmatter + Markdown 解析
│   ├── discovery.go             # 单文件/目录型扫描、路径与大小边界
│   ├── snapshot.go              # 三级覆盖、同层冲突、白名单与命令冲突
│   ├── manager.go               # 初始加载、指纹缓存、原子热更新
│   ├── activity.go              # 激活快照、参数替换、模型冲突、Clear
│   ├── profile.go               # 模型、工具交集、只读根执行配置
│   ├── history.go               # 最近 N 个完整用户轮次提取
│   ├── prompt.go                # 轻量目录与 active-skills 内容
│   ├── builtin.go               # go:embed 来源和临时资源物化
│   ├── parser_test.go
│   ├── discovery_test.go
│   ├── manager_test.go
│   ├── activity_test.go
│   ├── profile_test.go
│   ├── history_test.go
│   └── builtins/
│       ├── commit/SKILL.md
│       ├── review/SKILL.md
│       └── test/SKILL.md
│
├── orchestrator/
│   ├── skill_runtime.go         # 统一 PrepareSkill 与 load_skill 处理
│   ├── independent.go           # 临时会话、事件转发、摘要回流
│   ├── execution_state.go       # Profile、持久化、Memory、执行深度策略
│   ├── skill_runtime_test.go
│   ├── independent_test.go
│   ├── agent_loop.go            # 返回 RunResult，支持 Profile 原子切换
│   ├── chat.go                  # Prompt/Model/Tool View 接入
│   ├── run_request.go           # 请求携带执行 Profile
│   ├── tool_batches.go          # 系统工具分支和双层调用校验
│   └── tool_definitions.go      # 从请求工具视图生成稳定定义
│
├── tool/
│   ├── load_skill.go            # 系统工具名称、schema、sentinel Tool
│   ├── view.go                  # Registry 不可变过滤视图
│   ├── read_scope.go            # Context 请求级额外只读根
│   ├── registry.go              # View 所需复制/筛选支持
│   ├── path.go                  # 多只读根真实路径校验
│   ├── read.go                  # 使用 ReadScope
│   ├── glob.go                  # 使用 ReadScope
│   ├── grep.go                  # 使用 ReadScope
│   ├── view_test.go
│   └── read_scope_test.go
│
├── prompt/
│   ├── section.go               # BuildRequest 增加 Skill 区块输入
│   ├── builder.go               # 稳定目录 + 动态活动区块排序
│   ├── dynamic.go               # active-skills 位于 runtime reminder 前
│   └── prompt_test.go
│
├── provider/
│   ├── provider.go              # ChatRequest.Model
│   ├── anthropic.go             # 请求级模型覆盖
│   ├── openai.go                # 请求级模型覆盖
│   ├── anthropic_schema_test.go # 覆盖模型序列化断言
│   └── tool_parse_test.go       # OpenAI 模型与工具定义断言
│
├── command/
│   ├── definition.go            # Definition/Suggestion 增加 Badge
│   ├── controller.go            # ExecuteSkill
│   ├── builtins.go              # 移除硬编码 review，保留隐藏 /rv shim
│   ├── completion.go            # Badge 进入候选
│   ├── builtins_test.go
│   └── completion_test.go
│
├── events/
│   ├── events.go                # 独立运行与 transient 事件标记
│   └── events_test.go
│
├── tui/
│   ├── command_menu.go          # 候选显示 Badge
│   ├── messages.go              # 临时工具轨迹的显示与收尾清理
│   ├── status.go                # 当前 Skill/实际请求模型状态提示
│   ├── command_menu_test.go
│   └── messages_test.go
│
└── app/
    ├── deps.go                  # 注入 Skill Manager
    ├── app.go                   # 主 Activity、generation、基础命令
    ├── skills.go                # 热更新、命令重建、Skill 执行入口
    ├── commands.go              # Enter/Tab 前刷新，动态 Registry
    ├── command_controller.go    # ExecuteSkill、/clear 清 Activity
    ├── update.go                # 独立事件与请求生命周期
    ├── skills_test.go
    └── commands_test.go

cmd/xagent/main.go                  # 启动顺序、三级来源和严格首次校验
README.md                           # Skill 格式、目录、模式、样板和热更新
docs/skill-system/spec.md           # 已批准规格
docs/skill-system/plan.md           # 本技术设计
docs/skill-system/task.md           # 下一阶段任务拆解
docs/skill-system/checklist.md      # 后续验收清单
```

主要修改边界：

- `internal/skill` 是新增领域包，拥有发现、快照和活动规则。
- `internal/orchestrator` 承担执行协调，不把嵌套 Agent 逻辑放入 Tool 包。
- `internal/tool` 只提供过滤视图、系统工具声明和受控读取范围。
- `internal/app` 只管理当前 UI 会话 Activity 与动态命令，不解析 Skill 文件。
- `internal/command` 继续保持渲染无关，不导入 Skill 或 App。
- `internal/provider` 只增加请求级模型字段，不感知 Skill。
- 不修改会话 JSONL schema，不持久化 Skill 状态。
- 不增加外部依赖；复用项目已有 YAML 库和标准库嵌入文件系统。

## 技术决策

| 决策点 | 选择 | 理由 |
| --- | --- | --- |
| 状态模型 | 全局不可变 Snapshot + 执行上下文 Activity | 热更新不改写已激活指令，独立执行也不会污染主会话 |
| 热更新 | 交互边界 stat 清单 + 变化时解析候选 | 不引入 watcher 和后台竞态；无变化时成本低 |
| 更新提交 | 候选全量校验后原子换代 | 命令、目录和工具校验不会观察到不同 generation |
| YAML | 严格字段解析 | 拼错 `allowed_tools` 或 `history` 时显式诊断 |
| 名称格式 | `[a-z0-9][a-z0-9_-]{0,63}` | 可安全映射到命令、Prompt 标签和日志 |
| 工具名 | 大小写精确匹配 | 与现有 Registry 名称一致，避免模糊映射到错误工具 |
| 白名单空值 | nil 或空列表均表示不额外限制 | 保持简单 Skill 的默认兼容性 |
| 多白名单 | 取交集 | 满足最小权限原则 |
| 系统工具 | 注册在基础 Registry，但由 Orchestrator 拦截执行 | Provider 能发现，Executor 不承担嵌套 Agent 责任 |
| Prompt 位置 | 目录为 stable，活动 SOP 为优先 dynamic block | 未激活内容可缓存；活动指令每轮醒目且不污染历史 |
| Agent 中途激活 | iteration 边界原子切换 Profile | 首轮只看目录，工具加载后下一轮才看到完整 SOP |
| 独立摘要 | 直接使用独立最终回复 | 不增加额外模型成本，不扩大上下文 |
| Agent 触发独立 Skill | 终止父循环并采用独立最终回复 | 避免为了复述摘要再次调用主模型 |
| 模型覆盖 | `ChatRequest.Model` 请求级字段 | 不修改共享 Provider 实例，天然恢复默认 |
| 工具过滤 | Provider 暴露与执行校验共用 Registry View | 防止只隐藏 schema、仍可伪造调用的漏洞 |
| Skill 资源 | 请求级额外只读根 | 允许使用能力包资源，不扩大写权限 |
| 内置资源 | `go:embed`，辅助资源按需物化 | 二进制独立分发，同时兼容现有文件读取工具 |
| `/review` | 由内置 Skill 提供；`/rv` 为固定隐藏 shim | 完成迁移同时保留上一阶段兼容行为 |
| 动态命令 | 每个 generation 重建不可变 Registry | 复用现有冲突检测和稳定排序 |
| 活动持久化 | 不写 Conversation schema | 满足重启、切换会话恢复默认的要求 |
| 独立会话 | 内存 Conversation + 禁用 Store/Memory side effect | 保持隔离，不产生孤立会话文件和长期记忆污染 |

## 固定资源边界

首版使用代码常量，不新增配置项：

```text
每个来源最多发现 256 个入口
单个入口文件最大 256 KiB
展开后的 SOP 最大 128 KiB
单次参数最大 32 KiB
名称最大 64 字符
目录扫描只接受根目录 *.md 和直接子目录 SKILL.md
```

处理原则：

- 超限入口作为单文件错误跳过。
- 同层重名和最终白名单缺失属于快照级错误。
- 扫描时不跟随入口符号链接。
- 访问辅助资源时解析真实路径，并重新验证仍位于包根。
- 所有路径和错误进入 UI 或诊断前统一脱敏。

## 需求覆盖

| 需求 | 设计归属 |
| --- | --- |
| F1–F4 | Parser、Discovery、Definition、只读资源根 |
| F5–F8 | Snapshot Builder、Manager 首次加载与热更新 |
| F9–F11 | Catalog Prompt、Activity、系统级 `load_skill` |
| F12–F14 | 主 Activity、共享执行、原始命令历史 |
| F15–F17 | History 提取、Independent Runner、RunResult |
| F18–F19 | Execution Profile、请求级模型覆盖 |
| F20–F21 | Registry View、Plan 交集、执行层二次校验 |
| F22–F23 | 动态 Command Registry、Badge、review 迁移 |
| F24 | 文件指纹、候选快照、generation 重建 |
| F25–F26 | App 生命周期、Activity 原子 Clear/Activate |
| F27 | 嵌入式 commit/review/test 能力包 |
| F28 | Diagnostics、Redactor、事件和路径边界 |

没有未归属的功能需求。

## 风险控制

1. **Agent Loop 回归**：先提取 `RunResult` 而不改变事件顺序；保留全部现有 Agent Loop 测试，再增加 shared/isolated 对照测试。
2. **工具过滤不一致**：Profile 持有唯一 Registry View；schema、批处理和调用校验都依赖该 View，Executor 仍只处理已经通过视图和权限的调用。
3. **独立权限事件死锁**：父子共用同一 RequestSession 和事件流；同一时间只允许一个前台独立执行，确认请求继续由现有 TUI 通道解析。
4. **热更新竞态**：App 记录 generation；刷新与 Registry 重建在单次 Update 内连续完成，发布后再处理当前按键。
5. **活动快照漂移**：Activity 保存完整 Activated 值，Profile 只读取副本；Manager generation 不反向修改 Activity。
6. **路径越界**：只暴露当前活动包根；拒绝入口 symlink；每次读取都做真实路径归属检查；额外根不传给写工具。
7. **模型污染**：Provider 配置不可变，序列化请求时才选择 `ChatRequest.Model`。
8. **临时轨迹污染 UI 或历史**：事件增加 transient 标记；MessagesView 单独维护临时区域，结束时清理，仅提交最终文本。
9. **系统工具递归**：Profile 携带深度，深度大于 0 时 isolated 加载返回可恢复错误；shared 激活只更新临时 Activity。
10. **兼容命令漂移**：保留隐藏 `/rv` shim，基础命令元数据测试与动态 Skill 帮助测试同时覆盖。
