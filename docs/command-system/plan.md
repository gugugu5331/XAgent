# Command System Plan

## 架构概览

采用五层协作结构：

1. **命令核心层**

   新增独立命令包，使用标准库实现命令定义、注册、冲突检查、解析、查找、分发、帮助排序和补全匹配。该层不知道 Bubble Tea、TUI、Provider 或 Orchestrator，只依赖抽象控制接口。

2. **内置命令层**

   在命令包中集中声明十个公开命令和三个隐藏兼容入口。每条定义携带完整元数据与处理函数。公开 `/memory` 根据是否存在参数，在“新摘要行为”和旧子命令兼容行为之间分流。

3. **应用适配层**

   App 持有命令注册中心和当前会话模式，并提供命令控制接口的具体适配器。适配器负责把“显示消息、清屏、压缩、查询会话/记忆/权限/状态、切换模式、发送预设提示词”等抽象操作映射到现有应用状态与服务。

   所有命令输出在适配器边界统一脱敏。提示词命令产生的用户消息由适配器转入现有请求生命周期，保持流式事件、历史保存和 Token 统计行为一致。

4. **TUI 交互层**

   输入组件继续负责文本输入；新增轻量候选菜单模型。App 拦截 Tab：

   - 单个结果：直接写回规范命令名。
   - 多个结果：打开候选菜单。
   - 菜单中使用方向键选择、Enter 确认、Esc 关闭。

   候选菜单只接收展示数据，不读取注册中心，也不执行命令。

5. **Orchestrator 模式入口**

   为 Orchestrator 增加“文本 + 明确运行模式”的发送入口。App 发送普通消息或 `/review` 展开提示时，传入当前会话模式。现有单次模式解析入口保留给内部兼容测试和非 App 调用，新 App 路径不再依赖用户文本中的 `/plan` 前缀。

   `/plan` 将 App 当前模式设为 Plan；`/do` 恢复 Default。新建、加载或切换会话时统一重置为 Default。

## 需求覆盖

| 需求 | 设计归属 | 验证层级 |
|---|---|---|
| F1–F2 | 命令定义与注册中心；启动时强制构造 | 注册中心单元测试、App 初始化测试 |
| F3–F6 | 解析器与 App 输入分流 | 解析器测试、App Update 测试 |
| F7–F8 | 三类命令处理函数与抽象控制接口 | 替代控制器测试 |
| F9–F10 | 注册中心补全查询、候选菜单、Tab 键处理 | 补全单元测试、TUI/App 交互测试 |
| F11–F13 | 十个公开内置命令 | 内置命令测试、App 行为测试 |
| F14–F16 | App 会话模式、状态栏、明确模式发送入口 | App/Orchestrator 测试、端到端测试 |
| F17 | `/review` 固定提示词与正常发送路径 | 命令测试、会话历史测试 |
| F18 | 三个隐藏入口及 `/memory` 参数兼容分支 | 兼容回归测试 |
| F19 | App 控制器的统一脱敏输出边界 | 敏感值测试 |
| F20 | Enter/Tab 分流及 Provider 调用计数 | App 集成测试 |
| N1–N2 | 有序定义切片、规范化索引、稳定排序 | 重复构造与重复补全测试 |
| N3–N4、N9 | 纯本地解析/补全与替代控制器 | 无 Provider 单元测试 |
| N5–N8、N10–N12 | 适配现有服务、状态原子更新、兼容入口 | 全量回归、静态检查、tmux 验收 |

## 核心数据结构与接口

### `command.Type`

```go
type Type string

const (
    TypeLocal  Type = "local"
    TypeUI     Type = "ui"
    TypePrompt Type = "prompt"
)
```

### `command.Mode`

```go
type Mode string

const (
    ModeDefault Mode = "default"
    ModePlan    Mode = "plan"
)
```

命令层只认识 Default 与 Plan。`/do` 表示切回 Default，不引入第三种持久状态。

### `command.Definition`

```go
type Definition struct {
    Name        string
    Aliases     []string
    Description string
    Usage       string
    Type        Type
    ArgHint     string
    Hidden      bool
    Handler     Handler
}
```

名称和别名内部不带 `/`。注册时统一去除首尾空白并转小写；展示时再加 `/`。

### `command.Invocation`

```go
type Invocation struct {
    CanonicalName string
    MatchedName   string
    Args          string
    Raw           string
}
```

- `CanonicalName`：规范命令名。
- `MatchedName`：用户实际命中的名称或别名，已规范化。
- `Args`：第一个空白边界之后的原始参数。
- `Raw`：去除首尾空白后的完整输入。

### `command.Handler`

```go
type ExecutionContext struct {
    Registry   *Registry
    Controller Controller
}

type Handler func(context ExecutionContext, invocation Invocation) error
```

执行上下文让 `/help` 等处理函数读取当前注册中心的可见定义，同时仍只通过 Controller 操作应用。处理函数不返回 Bubble Tea 命令，也不引用具体界面类型。需要发送 AI 请求时调用控制器，由 App 适配器暂存对应的异步命令并交还 Update 循环。

### `command.Registry`

```go
type Registry struct {
    definitions []Definition
    lookup      map[string]int
}
```

主要接口：

```go
func New(definitions ...Definition) (*Registry, error)
func MustNew(definitions ...Definition) *Registry
func Builtins() []Definition
func (r *Registry) Dispatch(input string, controller Controller) DispatchResult
func (r *Registry) Complete(input string) []Suggestion
func (r *Registry) Visible() []Definition
```

`New` 校验空名称、无效类型、空处理函数、定义内重复别名和跨定义冲突。`MustNew` 在错误时 panic，用于应用启动。

`definitions` 保持规范命令名排序；`lookup` 只用于查找，不参与输出顺序。

### `command.DispatchResult`

```go
type DispatchKind string

const (
    DispatchEmpty     DispatchKind = "empty"
    DispatchPlainText DispatchKind = "plain_text"
    DispatchExecuted  DispatchKind = "executed"
    DispatchUnknown   DispatchKind = "unknown"
)

type DispatchResult struct {
    Kind       DispatchKind
    Text       string
    Invocation Invocation
    Err        error
}
```

App 根据 Kind 决定早返回、发送普通消息或返回命令适配器产生的异步命令。未知斜杠命令由分发器通过控制器显示错误，结果仍标记为 `DispatchUnknown`。

### `command.Suggestion`

```go
type Suggestion struct {
    Name        string
    Aliases     []string
    Description string
    ArgHint     string
}
```

即使前缀命中别名，Suggestion 也只返回一次规范命令，防止候选重复。

### `command.Controller`

```go
type Controller interface {
    DisplayNotice(text string)
    DisplayError(err error)
    SendUserMessage(text string)

    ClearMessages()
    SwitchMode(mode Mode)
    CurrentMode() Mode
    RefreshStatus()

    CompactContext() (string, error)
    SessionStatus() SessionStatus
    MemoryStatus() MemoryStatus
    PermissionStatus() PermissionStatus
    RuntimeStatus() RuntimeStatus
    TokenUsage() TokenUsage

    LegacyMemory(args string) (string, error)
    LegacyMCPStatus() (string, error)
    LegacyDiagnostics() (string, error)
}
```

这些方法表达产品能力，不暴露 Bubble Tea、Lip Gloss、具体状态结构或渲染组件。

### 状态快照

```go
type SessionStatus struct {
    ID           string
    MessageCount int
    Mode         Mode
    Streaming    bool
}

type MemoryScopeStatus struct {
    Enabled bool
    Count   int
}

type MemoryStatus struct {
    User        MemoryScopeStatus
    Project     MemoryScopeStatus
    Diagnostics []string
}

type PermissionStatus struct {
    Mode         string
    SessionRules int
    LocalRules   int
    ProjectRules int
    UserRules    int
    LoadErrors   int
}

type TokenUsage struct {
    Input         int64
    Output        int64
    CacheCreation int64
    CacheRead     int64
}

type RuntimeStatus struct {
    Provider    string
    Model       string
    Mode        Mode
    Streaming   bool
    MCP         string
    RecentError string
}
```

App 适配器从现有 Model、Orchestrator、Memory Manager 和 TUI Status 构造快照。`DisplayNotice` 与 `DisplayError` 是统一脱敏出口。

### App 状态扩展

```go
type Model struct {
    // 现有字段……
    commandRegistry *command.Registry
    commandMenu     tui.CommandMenu
    mode            orchestrator.RunMode
    lastError       error
    messagesCleared bool
}
```

`lastError` 独立保存最近错误，避免 `/status` 自身更新 Notice 后丢失待展示错误。

`Deps` 增加可选的 `CommandRegistry *command.Registry`，便于测试注入；为空时使用 `command.MustNew(command.Builtins()...)`，在应用开始接收输入前完成冲突检测。

### TUI 候选菜单

```go
type CommandMenuItem struct {
    Name        string
    Description string
    ArgHint     string
}

type CommandMenu struct {
    Items    []CommandMenuItem
    Selected int
    Visible  bool
}
```

主要接口：

```go
func (m *CommandMenu) Open(items []CommandMenuItem)
func (m *CommandMenu) Close()
func (m *CommandMenu) Move(delta int)
func (m CommandMenu) SelectedItem() (CommandMenuItem, bool)
func (m CommandMenu) View() string
```

输入组件补充：

```go
func (i *Input) SetValue(value string)
```

### Orchestrator 明确模式入口

```go
func (o *Orchestrator) SendWithMode(
    ctx context.Context,
    conv *conversation.Conversation,
    userText string,
    mode RunMode,
) (<-chan events.Event, error)
```

现有 `Send` 继续解析旧的一次性模式文本，然后委托给 `SendWithMode`。App 的新输入路径直接调用 `SendWithMode`，从 Model 当前模式传入 Plan 或 Default。

## 模块设计

### `internal/command`

#### `definition.go`

定义核心枚举、结构和状态快照。只依赖标准库。

#### `controller.go`

定义渲染无关的 `Controller` 接口。

#### `registry.go`

负责定义复制、规范化、合法性校验、冲突检测、有序定义和查找索引。冲突错误包含规范化冲突名及两条相关定义。

#### `parser.go`

负责输入分类、空白边界解析、名称查找、未知命令引导、处理函数调用和分发结果。分发时把当前 Registry 与 Controller 组成 `ExecutionContext` 传给处理函数。处理函数错误统一交给 `Controller.DisplayError`，不得回落到 Agent。

#### `completion.go`

只对尚未进入参数区的斜杠输入补全，同时匹配规范名称和别名，以规范命令去重，排除隐藏定义并稳定排序。

#### `builtins.go`

集中创建十个公开定义：

| 命令 | 类型 | 控制器能力 |
|---|---|---|
| `/help` | Local | 读取公开定义并显示格式化帮助 |
| `/compact` | Local | 压缩上下文、显示实际结果 |
| `/clear` | UI | 清空当前消息显示 |
| `/plan` | UI | 切换 Plan、刷新状态 |
| `/do` | UI | 切换 Default、刷新状态 |
| `/session` | Local | 读取会话快照 |
| `/memory` | Local | 无参数时显示新摘要；有参数时进入旧子命令兼容处理 |
| `/permission` | Local | 读取权限快照 |
| `/status` | Local | 读取运行状态与 Token 快照 |
| `/review` | Prompt | 发送固定审查提示词 |

同时注册三个隐藏定义：

- `/permissions`：只接受 `status`，委托旧权限状态能力。
- `/mcp`：只接受 `status`，委托旧 MCP 状态能力。
- `/diagnostics`：无参数，委托旧诊断能力。

所有公开无参数命令收到多余参数时显示对应 Usage，不静默忽略。旧 `/memory` 子命令保持现有参数规则和错误文本。

`/review` 的固定提示词要求检查当前工作区未提交改动，优先报告正确性、安全性、回归风险和测试缺口，按严重程度给出文件与位置证据，并且不修改文件、不提交代码。

### `internal/app`

#### `app.go`

扩展 Model，保存注册中心、当前模式、补全菜单、最近错误和清屏标记。初始化、创建和加载会话时统一重置模式、菜单和清屏状态。

#### `deps.go`

增加可选命令注册中心依赖，供测试注入。与工具注册中心使用不同的明确字段名。

#### `command_controller.go`

实现命令控制接口并包装 `*Model`：

- Notice/Error 在写入状态前脱敏，错误同时更新 `lastError`。
- Clear 只清空消息视图和流式缓冲，设置 `messagesCleared`。
- Mode 在命令 Mode 与 Orchestrator RunMode 之间转换。
- Compact 调用现有上下文压缩；消息区未隐藏时才重新同步压缩结果。
- Session、Memory、Permission、Runtime 和 Token 映射现有状态。
- Legacy 复用现有 MCP、诊断和 Memory 行为。
- SendUserMessage 调用统一请求提交助手并暂存 `tea.Cmd`。
- RefreshStatus 同步模式和 MCP 状态。

#### `commands.go`

抽出输入分发、统一用户请求提交、命令补全、候选确认，以及帮助/状态/兼容命令格式辅助函数。普通输入和 `/review` 共用同一请求提交路径。

#### `update.go`

按流式取消、权限确认、候选菜单、Tab、Enter、普通输入的优先级处理键盘。删除现有本地命令条件链，所有斜杠输入统一经过注册中心。事件错误同时写入 `lastError`。

### `internal/tui`

- `input.go`：增加 `SetValue`，保持焦点并把光标移到末尾。
- `command_menu.go`：实现候选菜单状态、移动、选择、关闭和渲染。
- `messages.go`：增加只清空视图的 `Clear`。
- `status.go`：增加 Mode 字段，始终展示 `[DEFAULT]` 或 `[PLAN]`。

### `internal/orchestrator`

- `chat.go`：新增 `SendWithMode`，验证文本与模式，在整次 Agent Loop 中使用传入模式；现有 `Send` 委托新入口。
- `run_request.go`：保留旧一次性模式解析，供非 App 兼容入口使用。

### 文档与测试

- README 更新十个公开命令、别名和模式说明，不展示隐藏兼容入口。
- 命令核心、TUI 菜单、状态栏、清屏、App 分流、模式生命周期、明确模式、脱敏和旧命令均建立自动化测试。
- 最终按 `CLAUDE.md` 使用 tmux 运行真实 TUI 端到端场景。

## 模块交互

### 启动注册

```text
App.New
  → 读取注入的 CommandRegistry
  → 若为空，调用 MustNew(Builtins...)
  → 规范化名称和别名
  → 检查所有冲突
  → 成功后构造 Model
  → 失败则 panic，应用不接受输入
```

### 普通输入

```text
用户按 Enter
  → Registry.Dispatch
  → 判定为 PlainText
  → App.submitUserMessage
  → 读取 Model 当前模式
  → Orchestrator.SendWithMode
  → 正常事件流、历史保存、Token 统计
```

### 本地或界面命令

```text
用户输入 /command 并按 Enter
  → Registry 查找规范名称或别名
  → 调用 Definition.Handler
  → Handler 只调用 Controller
  → App Controller 更新 Model/Status/Message View
  → 清空输入框
  → 不调用 Orchestrator
```

未知斜杠输入走同一路径，但只显示“未知命令，请使用 `/help`”，不进入普通输入分支。

### `/review` 提示词命令

```text
/review 或 /rv
  → Handler 取得固定完整提示词
  → Controller.SendUserMessage
  → App.submitUserMessage
  → 按当前 Default/Plan 模式调用 SendWithMode
  → 展开后的提示词作为 user 消息显示并持久化
```

### 模式切换

```text
/plan
  → Model.mode = Plan
  → Status.Mode = plan
  → RefreshStatus
  → 后续每次 submitUserMessage 都传 Plan

/do
  → Model.mode = Default
  → Status.Mode = default
  → RefreshStatus
```

创建或加载会话时执行同一个模式重置辅助函数。切换命令不写入会话历史。

### Tab 补全

```text
用户按 Tab
  → Registry.Complete(input)
      ├─ 已进入参数区：不处理
      ├─ 精确命中名称/别名：返回对应规范命令
      ├─ 一个前缀命令：直接写回
      ├─ 多个前缀命令：打开菜单
      └─ 无匹配：显示本地提示
```

精确名称或别名优先于其他前缀匹配。例如 `/p` 精确命中 `/plan` 的别名，即使 `/permission` 也以 `p` 开头，仍直接补全为 `/plan`。

多候选菜单使用 Up/Down 移动、Enter 写回规范名称但不执行、Esc 关闭；继续输入其他字符时关闭旧菜单。隐藏命令在候选生成阶段过滤。

### `/clear`

```text
/clear
  → MessagesView.Clear
  → messagesCleared = true
  → Conversation.Messages 保持不变
```

后续新消息只追加到空显示视图。压缩时若消息已隐藏，不把旧会话重新灌回显示区。重新加载会话后历史重新可见。

### 隐藏兼容入口

```text
/permissions status → hidden permissions handler
/mcp status         → hidden mcp handler
/diagnostics        → hidden diagnostics handler
/memory <args>      → visible memory handler 的 legacy 分支
```

兼容入口经过同一 Registry 和 Controller，不保留 App Update 中的旁路条件。

## 文件组织

```text
internal/
├── command/
│   ├── definition.go          — 核心枚举、定义、调用、结果、快照
│   ├── controller.go          — 渲染无关的 Controller 接口
│   ├── registry.go            — 注册、规范化、冲突检测、公开列表
│   ├── parser.go              — 输入分类、解析、查找与分发
│   ├── completion.go          — 可见命令补全与稳定排序
│   ├── builtins.go            — 十个公开命令、隐藏兼容命令、review 提示
│   ├── registry_test.go       — 定义校验与全部冲突组合
│   ├── parser_test.go         — 空输入、普通输入、参数保真、未知命令
│   ├── completion_test.go     — 精确优先、别名、去重、隐藏、排序
│   └── builtins_test.go       — 替代 Controller 下的十个命令行为
├── app/
│   ├── app.go                 — Model 字段、初始化、会话模式重置
│   ├── deps.go                — 可选 CommandRegistry 注入
│   ├── command_controller.go  — Controller 的 App 适配器与脱敏边界
│   ├── commands.go            — Enter 分流、请求提交、Tab 补全辅助
│   ├── update.go              — 键盘优先级和统一命令入口
│   ├── commands_test.go       — 命令集成、模式、补全、Provider 计数
│   └── update_test.go         — 保留并调整现有本地命令回归测试
├── tui/
│   ├── input.go               — 增加 SetValue
│   ├── command_menu.go        — 候选菜单状态和渲染
│   ├── command_menu_test.go   — 打开、移动、选择、关闭、渲染
│   ├── messages.go            — 增加仅视图 Clear
│   ├── messages_test.go       — 清屏不影响源会话切片
│   ├── status.go              — 增加 DEFAULT/PLAN 标记
│   └── status_test.go         — 默认退化与模式展示
└── orchestrator/
    ├── chat.go                — 增加 SendWithMode，现有 Send 委托
    ├── run_request.go         — 保留旧一次性模式解析
    └── chat_test.go           — 明确模式、多轮 Plan 与兼容入口测试

docs/
└── command-system/
    ├── spec.md
    ├── plan.md
    ├── task.md
    └── checklist.md

README.md                       — 更新公开命令、别名和模式说明
```

不修改 Provider、Conversation 持久化格式、Memory 存储格式、Permission 规则格式或配置文件 schema。

## 技术决策

| 决策点 | 选择 | 理由与取舍 |
|---|---|---|
| 命令核心位置 | 独立 `internal/command` 包 | 避免继续扩大 App Update；可脱离 Bubble Tea 和 Provider 单测，为后续 Skill 命令扩展保留清晰边界 |
| 注册数据结构 | 有序定义切片 + 规范化查找索引 | 切片保证帮助和补全顺序稳定；索引保证名称与别名查找直接；不依赖 map 遍历顺序 |
| 注册可变性 | 构造时深拷贝定义和别名 | 防止调用方后续修改原切片，破坏冲突检测结果和确定性 |
| 启动冲突处理 | App 默认注册使用 `MustNew`，冲突直接 panic | 完全满足“启动阶段撞名立即退出”；测试仍可使用返回 error 的 `New` 精确断言 |
| 命令名规范化 | 去除 `/`、TrimSpace、转小写 | 用户输入大小写不敏感，元数据内部保持统一；展示层统一补 `/` |
| 参数解析 | 以第一个 Unicode 空白为边界，保留其后参数内容 | 兼容空格和 Tab，且不破坏参数大小写或内部空格 |
| 补全优先级 | 精确名称/别名优先，之后才做前缀匹配 | 确保 `/p`、`/h` 等显式别名可直接补全，不被更长命令前缀干扰 |
| 候选去重 | 以规范命令名去重 | 同一命令可能同时被名称和别名命中，但菜单只应展示一次 |
| 命令执行抽象 | 同步 Handler + Controller 接口 | 注册、帮助、查询和模式切换保持简单；App 适配器负责把发送请求转换为 Bubble Tea 异步命令 |
| AI 请求入口 | 普通输入和 `/review` 共用 `submitUserMessage` | 避免提示词命令绕过会话保存、流式状态、取消、Token 清零或错误处理 |
| 持久模式归属 | 模式保存在 App Model，不写 Conversation | 满足当前会话交互期间持续、新建/切换/重启恢复默认；不改变持久化 schema |
| `/do` 的模式值 | 切回 `RunModeDefault` | 已批准规格只有 `[DEFAULT]` 与 `[PLAN]` 两种持续模式；现有 `RunModeDo` 仅保留给旧内部一次性入口 |
| Orchestrator 改造 | 新增 `SendWithMode`，现有 `Send` 委托 | 新 App 可以显式传递持续模式，同时降低现有调用和测试的回归风险 |
| `/clear` 实现 | 清空视图并维护 `messagesCleared` | 只清屏、不改历史；避免 `/compact` 等后续本地操作意外把旧消息重新显示 |
| 帮助与本地结果展示 | 使用状态 Notice/Error 通道 | 延续现有本地命令交互，不向 Conversation 写入系统消息；支持多行帮助文本 |
| 最近错误 | Model 独立保存 `lastError` | `/status` 更新 Notice 时仍能报告此前最近错误，不依赖当前瞬时 Error 字段 |
| 敏感信息处理 | 所有 Controller 输出在 App 边界统一脱敏 | 即使命令格式化遗漏，也有最终防线；复用现有项目安全策略 |
| 隐藏兼容方式 | 兼容入口也注册到统一 Registry | 消除 Update 旁路；统一获得未知命令保护、脱敏和“不得发送给模型”保证 |
| `/memory` 兼容 | 同一公开定义按参数分支 | 避免注册两个同名命令造成冲突；无参数执行新摘要，有参数保持旧行为 |
| 多候选菜单 | 自建轻量状态模型，不复用完整列表组件 | 候选量很小，不需要过滤、分页和复杂委托；减少焦点与按键冲突 |
| Provider/存储改动 | 不改协议和持久化格式 | 本功能只改变输入分流与运行模式传递，避免扩大迁移和兼容范围 |
| `/compact` 执行模型 | 保持现有同步调用语义 | 本阶段只迁移到统一命令机制，不额外重构上下文压缩生命周期 |
| 提交策略 | 不自动创建 Git 提交 | 当前用户没有授权提交；任务完成后只提供改动和验证证据 |
