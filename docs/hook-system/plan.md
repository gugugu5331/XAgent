# Lifecycle Hook System Plan

## 1. 设计目标与约束

本章在不复用现有 TUI `events` 流的前提下，引入一个进程级 Lifecycle Hook Engine。Engine 在启动时一次性加载并校验规则，在明确的 App、Orchestrator、Context Manager 接点接收不可变事件快照，按稳定顺序执行动作，并把运行期失败隔离为诊断。

设计必须同时满足以下边界：

- 配置错误在任何 MCP 连接、Hook Shell 或 Hook HTTP 副作用发生前阻止启动。
- 运行期 Hook 技术失败全部 fail-open；只有合法的同步 `tool_before` deny 可以改变工具执行结果。
- Hook 位于 Plan Mode 与不可绕过硬安全之后、普通权限规则和用户确认之前。
- 用户级和项目级 Hook 都是受信任的高权限自动化：`tool_before` 的 Command/HTTP 本身可能在普通工具尚未获用户许可时产生进程、文件或网络副作用；“不能绕过权限”只表示它不能执行被拦工具或伪造 Executor Grant，不表示 Hook action 受普通工具确认保护。
- 主执行与独立 Skill 共享规则、session Prompt、进程级 once 和异步执行器，但 execution/turn 状态互相隔离。
- 没有 Hook 文件或没有有效规则时，Provider 请求、Conversation、权限、Skill、命令和 UI 输出保持现有行为。
- Hook Prompt 只进入实际 Agent Loop Provider 请求，不进入 Context Summary、Memory 或其他内部 Provider 调用。
- 本轮不实现热更新、once 持久化、显式 priority 和真实 SubAgent 调度。

## 2. 总体架构

```text
~/.config/xagent/hooks.yaml ─┐
                             ├─► Loader / Strict Validator ─► Immutable Rule Snapshot
<project>/.xagent/hooks.yaml ┘              │
                                            ▼
                                  Process-wide Hook Engine
                         ┌──────────────────┼──────────────────┐
                         │                  │                  │
                    Dispatcher        Prompt State       Async Executor
                 condition/action   next/turn/session     4 workers + 64 queue
                         │                  │                  │
             ┌───────────┼──────────┐       │                  │
             ▼           ▼          ▼       ▼                  ▼
           App      Orchestrator  Context  Agent Loop      Command / HTTP
      system/session turn/message   Manager  prompt build   / SubAgent no-op
                         tool       compact
```

生命周期所有者固定如下：

| 层级 | 所有者 | 责任 |
| --- | --- | --- |
| System | `cmd/xagent` + `internal/app` | 严格加载、依赖就绪后的 start、资源关闭前的 stop、幂等关闭 |
| Session | `internal/app` | Conversation 新建/恢复/切换/退出、保存和状态清理顺序 |
| Turn / Message | `internal/orchestrator` | Agent Run 边界、完整消息写入边界、终止状态映射 |
| Tool | `internal/orchestrator` | 公共前置安全链、同步 deny、执行后结果事件 |
| Compact | `internal/contextmgr` | 只包围真实压缩尝试，报告同一份前置统计 |
| Prompt Provider 接入 | `internal/orchestrator` + `internal/prompt` | 在实际 Agent 请求前租借并按顺序注入 Prompt |

Engine 是独立业务组件，不导入 Bubble Tea、TUI event 或具体渲染类型。调用者只依赖窄接口，测试可注入 recorder/no-op。

## 3. 包与文件规划

### 3.1 新增 `internal/hook`

```text
internal/hook/
  api.go              # Runtime 接口、事件/状态枚举、ExecutionRef、ToolDecision
  config.go           # YAML DTO、action union、duration 与 source 类型
  loader.go           # 两个固定路径读取、文件大小检查、user→project 合并
  validate.go         # 严格字段、事件字段目录、组合兼容性与资源限制校验
  event.go            # EventContext 类型、深复制、规范 JSON、sequence/time/ID
  fields.go           # 事件可用字段表与点路径读取
  condition.go        # all/any、typed exact、glob/regex、negate
  template.go         # 严格标量占位符预编译与原子渲染
  engine.go           # 稳定 dispatch、panic 隔离、deny 短路、生命周期清理
  once.go             # idle/pending/done 原子状态机
  prompt_state.go     # next/turn/session 存储、排序、租借、提交与释放
  async.go            # 4 worker、64 queue、非阻塞入队、2 秒关闭
  command.go          # 受信 Shell Runner、受控环境、stdin、输出/超时限制
  http.go             # URL/解析/重定向策略、body 限制与超时
  decision.go         # allow/deny 严格 JSON 解码与安全 reason
  diagnostic.go       # 稳定诊断码及无载荷诊断构造
  limits.go           # F59–F60 的单一默认上限来源
  noop.go             # 无规则时的零行为 Runtime
```

包名采用单数 `hook`，与现有 `skill`、`permission`、`tool` 命名风格一致。

### 3.2 新增共享 matcher

```text
internal/matcher/
  matcher.go          # 区分大小写的完整值 string exact/glob
  matcher_test.go
```

`permission` 保留命令和路径规范化、规则评分以及公开 YAML schema，只把底层完整值 exact/glob 委托给 `internal/matcher`。Hook 的类型化数字/布尔 exact、regex 和 negate 仍留在 `internal/hook`，避免权限 YAML 意外获得新语法。

### 3.3 修改的现有模块

```text
cmd/xagent/main.go                    # 加载顺序、Engine 构造、组合关闭、run() error
internal/app/deps.go                  # 注入 hook.Runtime
internal/app/app.go                   # system/session 生命周期与幂等 Close
internal/app/update.go                # 取消后等待真实 Turn 收尾
internal/tui/program.go               # 返回最终 Model，保证异常返回也执行 App Close

internal/orchestrator/chat.go         # Turn 包装、Prompt 租借、run tracker
internal/orchestrator/agent_loop.go   # assistant Message 边界与统一终止
internal/orchestrator/execution_state.go # execution/turn/session identity
internal/orchestrator/tool_batches.go # 公共 Tool gate 与 before/after
internal/orchestrator/skill_runtime.go # load_skill 并入公共 gate
internal/orchestrator/independent.go  # isolated execution 生命周期与去重消息

internal/permission/authorizer.go     # 硬安全与普通权限拆分
internal/permission/matcher.go        # 复用共享 matcher、UseNumber 参数
internal/tool/validation.go           # 注册表成员与 JSON object 结构校验
internal/tool/tool.go                 # hook_denied 错误码

internal/contextmgr/manager.go        # 非变异 preflight 与 CompactionObserver
internal/sessionctx/manager.go        # 持久/临时压缩准备模式透传

internal/prompt/section.go            # Hook Prompt 输入及统一有序 Block
internal/prompt/builder.go            # 固定安全块之后的精确插入位置
internal/provider/provider.go         # 有序 System blocks 与 request-attempt 终态握手兼容入口
internal/provider/openai.go           # 使用有序 blocks
internal/provider/anthropic.go        # 使用有序 blocks 和原 Cacheable 标记

internal/diagnostics/diagnostic.go    # 有界结构化 attributes
internal/diagnostics/collector.go     # attributes 深复制、脱敏、限长、稳定展示
internal/redact/redact.go             # 敏感展开组件注册与短 secret 安全匹配

.gitignore                            # 允许提交 .xagent/hooks.yaml
README.md                             # 配置、重启与 Shell/HTTP/Prompt 受信风险说明
docs/hook-system/example.yaml         # 可解析的完整示例
```

保留现有公开构造函数；新增依赖通过 options 或 nil-safe 字段注入。现有测试和调用者不传 Hook 时自动使用 no-op。

## 4. 核心类型与接口

以下签名是实现基线，允许在不改变语义的情况下调整命名。

### 4.1 配置模型

```go
type File struct {
    Version int          `yaml:"version"`
    Hooks   []RuleConfig `yaml:"hooks"`
}

type RuleConfig struct {
    Event   Event           `yaml:"event"`
    If      *ConditionGroup `yaml:"if,omitempty"`
    Once    bool            `yaml:"once,omitempty"`
    Async   bool            `yaml:"async,omitempty"`
    Timeout Duration        `yaml:"timeout,omitempty"`
    Action  ActionConfig    `yaml:"action"`
}

type ConditionGroup struct {
    All []Predicate `yaml:"all,omitempty"`
    Any []Predicate `yaml:"any,omitempty"`
}

type Predicate struct {
    Field  string    `yaml:"field"`
    Match  MatchType `yaml:"match"`
    Value  any       `yaml:"value"`
    Negate bool      `yaml:"negate,omitempty"`
}

type ActionConfig struct {
    Type      ActionType        `yaml:"type"`
    Command   string            `yaml:"command,omitempty"`
    Env       map[string]string `yaml:"env,omitempty"`
    Decision  bool              `yaml:"decision,omitempty"`
    URL       string            `yaml:"url,omitempty"`
    Method    string            `yaml:"method,omitempty"`
    Headers   map[string]string `yaml:"headers,omitempty"`
    SendEvent *bool             `yaml:"send_event,omitempty"`
    Content   string            `yaml:"content,omitempty"`
    Scope     PromptScope       `yaml:"scope,omitempty"`
    Agent     string            `yaml:"agent,omitempty"`
    Input     string            `yaml:"input,omitempty"`
}
```

`SendEvent` 使用指针区分“未配置，默认 true”和显式 false。Loader 把 DTO 编译为不可变 `Rule`，其中包含规范 source、从 1 开始的声明序号、跨文件 `effective_rule_ordinal`、已编译 regex/template、展开后的静态环境和动作默认 timeout。

Action 使用单一 YAML 对象，但校验器按 `type` 应用严格字段白名单。例如 `prompt` 出现 `url`、`command` 出现 `headers` 都是启动错误，不能因为 Go union 字段存在而静默接受。

### 4.2 EventContext

```go
type EventContext struct {
    SchemaVersion int               `json:"schema_version"`
    Event         Event             `json:"event"`
    Sequence      uint64            `json:"sequence"`
    OccurredAt    string            `json:"occurred_at"`
    Project       ProjectContext    `json:"project"`
    Execution     *ExecutionContext `json:"execution,omitempty"`
    Session       *SessionContext   `json:"session,omitempty"`
    Turn          *TurnContext      `json:"turn,omitempty"`
    Message       *MessageContext   `json:"message,omitempty"`
    Tool          *ToolContext      `json:"tool,omitempty"`
    Compact       *CompactContext   `json:"compact,omitempty"`
}
```

构造规则：

- Engine 用 atomic counter 分配 `sequence`，时间源和 ID 源可在测试中注入。
- `occurred_at` 使用 RFC3339Nano；`project.root` 在构造 Engine 时转为绝对 clean path。
- `ExecutionRef` 保存 session ID、execution ID、turn ID、`main | isolated_skill` 和原始 RunMode；Do Mode 对外映射为 Hook `default`。
- Message ID 只存在于 Hook 生命周期，同一 before/after 复用，不改变 Conversation JSON 格式。
- `tool.arguments` 用 `json.Decoder.UseNumber` 解码一次并深复制；所有消费者读取同一不可变快照。
- 不适用对象保持 nil 并从 JSON 省略。after 事件在对应对象中增加状态，不用空对象占位。
- 构造器先深复制动态数据再冻结为事件快照；条件、Command、HTTP 和 Prompt 都读取同一份未被各 action 单独改写的快照，数字和布尔类型不变。内部用私有表示或 copy-on-read，不能把可修改 map/slice 暴露给 action runner。`tool.result` 在进入 builder 前已经限长脱敏；其他原始字段只有在进入诊断、decision reason 或用户可见错误时才按 N8 脱敏，不能为了某个 action 改写共享快照。
- Engine 可缓存一次规范 JSON。超过 1 MiB 时保留内存快照供条件和 Prompt 点路径读取，但 Command stdin 以及 `send_event: true` 的 HTTP 动作失败并诊断。

事件 builder 与对象组合固定如下：

| 事件 | 必须附加的对象与阶段字段 |
| --- | --- |
| `system_start` / `system_stop` | 只有公共顶层与 `project` |
| `session_start` | `session.id`、`session.state = new | resumed` |
| `session_end` | `session.id`、`session.end_reason = switch | exit` |
| `turn_start` | `execution`、`session.id`、`turn.id` |
| `turn_end` | start 对象，加 `turn.status` 和可选安全 `turn.error` |
| `message_before` / `message_after` | `execution`、`session.id`、`turn.id`、同一 `message.id/role/content` |
| `tool_before` | `execution`、`session.id`、`turn.id`、`tool.call_id/name/arguments` |
| `tool_after` | before 对象，加 `tool.status/duration_ms/result`；result 只含限长脱敏后的模型可见字段 |
| `compact_before` | 可用的 session/execution/turn，加 `compact.reason/before` |
| `compact_after` | before 对象与同一前置统计，加 status、可用的 after 统计和可选安全 error |

`compact.reason` 版本 1 固定为 `auto | manual`；是否来自 isolated run 由 `execution.kind` 区分。消息数使用压缩实际处理的 `conversation.ContextMessages` 数量，Token 使用现有 `EstimateConversationTokens` 口径。

Engine 的状态转换也包在便利方法内：`SessionStart` 先建立 active session 再 dispatch，使 start Prompt 可以绑定；`BeginTurn` 先建立 execution/turn binding 再 dispatch；`EndTurn` 和 `SessionEnd` 都是先 dispatch、后清理。`SystemStart`、`Shutdown`、Session start/end 均幂等，异常 fallback closer 不会重复发事件。

System 生命周期使用原子 `not_started → started → stopped`：只有成功进入 started 并完成一次 `system_start` dispatch 后，`Shutdown` 才允许产生 `system_stop`。若严格加载之后、`app.New/SystemStart` 之前的依赖构造失败，fallback closer 只停止尚未启用的 worker/资源，不执行 stop 规则；并发或重复 Shutdown 只由一个调用完成状态转换。

### 4.3 Runtime 接口

```go
type Runtime interface {
    SystemStart(context.Context)
    Shutdown(context.Context) error

    SessionStart(context.Context, string, SessionState)
    SessionEnd(context.Context, string, SessionEndReason)

    BeginTurn(context.Context, string, ExecutionKind, HookMode) ExecutionRef
    EndTurn(context.Context, ExecutionRef, TurnStatus, string)

    BeginMessage(context.Context, ExecutionRef, MessageRole, string) MessageToken
    EndMessage(context.Context, MessageToken)
    BeforeTool(context.Context, ExecutionRef, ToolInput) ToolDecision
    AfterTool(context.Context, ExecutionRef, ToolInput, ToolOutput, time.Duration)

    BeforeCompact(context.Context, CompactBinding, CompactInput) CompactToken
    AfterCompact(context.Context, CompactToken, CompactOutput)
    AcquirePrompts(context.Context, ExecutionRef) (PromptLease, error)
}
```

生命周期方法不向业务调用者返回技术错误；Engine 自己记录诊断并继续。`BeginMessage` 生成 ID、冻结 role/content 并分发 before，append 后用其不可变 token 分发 after，调用者不能误用不同 ID/正文。`BeforeTool` 只返回 `Continue` 或合法 `Deny`。配置加载和 Engine 构造是唯一会因 Hook 配置返回启动错误的路径。

`EndTurn` 先分发事件，再清除 turn Prompt 与该 execution 未消费的 next；`SessionEnd` 先分发事件，再清除 session Prompt。调用者不能提前清理，避免 end Hook 看不到仍应存在的作用域。

### 4.4 Prompt lease

```go
type PromptLease interface {
    Blocks() []PromptBlock
    Commit()
    Release()
}
```

`AcquirePrompts` 原子租借当前 execution/turn/session 可见条目，并暂时保留符合条件的 next。不能把 `StreamChat` 返回等同于“已发送”：Anthropic 当前会先返回 channel、再在 goroutine 中发请求，OpenAI 也可能在已发送后才同步返回网络错误。

每条 next entry 维护 `available | leased(lease_id)`；同一 session-bound next 在并发 main/isolated acquire 时只能进入一个 lease。`Blocks()` 返回内容副本，Commit/Release 幂等。若 EndTurn/SessionEnd 已清理 owner，迟到的 observer、Commit 或 Release 只做 no-op，不能复活过期条目。

为了让 next 真正对应 owner 的下一次发送，PromptState 还维护 context-aware send gate。存在 available/leased execution-next 时，同 execution 的后来请求必须等待；存在 session-next 时，后来的 main/isolated Agent 请求也必须在 Provider 调用前等待当前 lease Commit/Release，不能绕过后先发送。首个 lease 在 `MarkSent` 或 `Finish(true)` 提交时立刻释放 gate；只有 Provider 以 `Finish(false)` 明确确认本次 attempt 已终止且没有越过发送边界时才回滚，之后其他 stream 才能取得同一条 next。等待可由请求 context 取消。

Acquire 是线性化边界：它发生在其他 request 构造都完成、即将交给 Provider 之前，并在锁内封存本次 next generation；该边界之后并发新增的 session-next 属于下一 generation。若首个请求 pre-send Release，原 entries 回到 available，并与后来新增 entries 按 sequence/ordinal 一起由下一个 waiter 取得。barrier 测试覆盖“先 acquire 后发送”“先 acquire 后失败”和并发插入。

`provider.ChatRequest` 因此增加 nil-safe、request-local 的 attempt observer：

```go
type RequestObserver interface {
    MarkSent()
    Finish(sent bool)
}
```

Provider 拥有 attempt 生命周期及其单调的 `sent` latch。请求到达不可逆 outbound transport 边界时调用 `MarkSent`；HTTP 实现用 request context/transport trace 接线并把已观察到的写出状态锁存，fake Provider 可精确控制该边界。Provider 在每条同步、异步、错误和取消路径上都必须恰好一次调用 `Finish(sent)`，且只有在底层 transport attempt 和相关 callback 已完全静止、不可能再迟到调用 `MarkSent` 后，才允许 `Finish(false)`。异步 Provider 必须先完成该握手，再发送 terminal stream event 和关闭输出 channel。

Orchestrator 的线程安全 adapter 遵循：

- `MarkSent` 原子锁存 sent 并立即 `Commit`，next 消费且 send gate 释放；之后的 HTTP status、流错误或取消都不回滚。
- `Finish(true)` 是提交的幂等兜底；`Finish(false)` 只有在 adapter 也从未观察到 `MarkSent` 时才 `Release`。
- 请求一旦交给 Provider，caller cleanup、stream error/close 和 context cancel 都不得自行 `Release`；必须等待 Provider 的终态握手。只有尚未交给 Provider 的本地同步构造失败可直接回滚 lease。
- adapter 对重复终态调用做防御性 no-op，但 Provider 的强契约仍是 `Finish` 恰好一次、`Finish(false)` 后绝无迟到发送 callback；违反契约在测试中直接失败，不能依靠 `sync.Once` 掩盖。

Summary、Memory 等内部 Provider 请求不取得 lease，也不设置 observer。测试必须分别覆盖 pre-send `Finish(false)`、写出后 `MarkSent`/`Finish(true)`、异步发送、并发 session-next 竞争，以及取消与实际写出交错时 `Finish(false)` 等待 callback 静止的 barrier 场景。

## 5. 配置发现与集中校验

### 5.1 启动顺序

```text
解析主配置与创建 RuntimeRedactor
  → 取得绝对 project root 与 user home
  → 读取 ~/.config/xagent/hooks.yaml（可缺失）
  → 读取 <project>/.xagent/hooks.yaml（可缺失）
  → 严格解析、集中校验、编译规则快照
  → 创建其余 store/provider/registry/MCP/Skill 依赖
  → 创建一个进程级 Hook Engine
  → app.New 完成 Orchestrator 与 UI 状态
  → system_start
  → 可选的初始 session_start
  → 启动 TUI
```

Hook 文件验证必须发生在 `mcpManager.Start` 之前。Hook 不是 `config.AppConfig` 的子树，避免改变现有主配置 `KnownFields` 行为。用户路径固定为 `~/.config/xagent/hooks.yaml`，不改用 macOS `UserConfigDir` 的其他目录。

### 5.2 YAML 解析

Loader 用同一次打开的 reader 加 `io.LimitReader(max+1)` 检查单文件 256 KiB 上限，再从已读有界 bytes 解码为 `yaml.Node`，避免 size-check/read 的 TOCTOU。显式 schema walker 负责：

- 未知顶层/规则/条件/action 字段；
- 缺失 `version`、`hooks`、`event` 或 `action`；
- version 不等于 1；
- 节点类型错误、重复 key、非标量 map key；
- 第二个 YAML document、alias、anchor、merge key 和 custom tag；
- 每文件最多 256 条规则、每规则最多 32 个 predicate；
- all/any 必须二选一且非空，不允许嵌套；
- event/action/scope/match 枚举；
- predicate `value` 只接受 string、有限十进制 number 或 bool，拒绝 null、NaN/Inf、时间 tag、object 和 array；
- duration、glob、正则、模板、字段路径和 action 组合；
- F59 中其余配置期字节/数量上限。

DTO 解码保留字段 presence，不能用 Go 零值猜测是否出现：显式 `timeout: 0s` 必须报错，错误 action 上即使写 `decision: false` 也必须因字段不适用而报错。

错误结构保留 source path、line、column、字段点路径和规则序号。两个文件先分别完整校验，任一错误使启动失败；不采用“跳过坏规则”或“保留旧快照”。

启动错误只报告 schema、位置、限制和安全类别，不回显 command、Prompt、正则值、Header/env 值或展开后的秘密；整条错误在写 stderr 前再经过 RuntimeRedactor。

### 5.3 合并和稳定身份

- 文件读取顺序固定 user → project，不做目录遍历。
- `RuleKey = canonical absolute source path + declared ordinal`，只用于 once 和诊断。
- `effective_rule_ordinal` 按合并后的切片从 1 连续分配。
- 规则快照完成后不再修改；本进程不重读文件。
- 环境变量在编译阶段只按 `${ENV_NAME}` 语法展开，随后做长度校验；不额外接受 `$NAME` 或事件占位符。引用未定义变量是启动错误，避免 decision Hook 因空认证值运行期失败后静默 fail-open；字面量 `${...}` 使用 `$${...}` 转义。只有引用变量名、Command env key 或 HTTP Header name 被 `redact.IsSensitiveKey` 判定为敏感时，才注册对应的非空替换组件；完全静态的敏感字段注册其完整值，不为每条规则注册“前缀 + 同一 secret”的组合，全部值去重。RuntimeRedactor 对很短的注册值只做完整 token/字段值匹配，不做全局单字符 substring 替换，避免普通文本被破坏；Engine 构造后再读取 `MaxSecretBytes` 供流式脱敏使用。

### 5.4 字段与组合兼容性

校验器维护 12 个事件的可用字段目录。固定字段必须能由目标事件提供；只有 `tool.arguments.<path>` 允许动态扩展。典型非法组合包括：

- `turn_start` 条件读取 `turn.status`；
- `tool_before` 模板读取 `tool.result.content`；
- 非 tool 事件读取 `tool.arguments.*`；
- Prompt scope 不在 F38 矩阵内；
- 任意 Prompt 或任意 `tool_before` 使用 async；
- 非同步 `tool_before` Command/HTTP 配置 `decision: true`；
- 非 Command/HTTP 动作配置 decision；
- Prompt 配置 timeout；
- Command/HTTP/SubAgent timeout 非正数或超过 10 分钟。

## 6. 条件与模板

### 6.1 点路径读取

字段解析器在加载期把路径编译为 segments。运行期读取返回 `{value, exists, scalarKind}`，不通过反射修改对象。

- 固定 schema 路径由事件目录验证。
- `tool.arguments.*` 运行期逐层遍历 object；数组不支持数字下标路径。
- 缺失字段、穿过非 object、最终对象/数组用于 scalar matcher 时返回“不匹配”，不是错误。
- JSON/YAML 数字转换为规范 decimal 表示后比较；`1` 与 `1.0` 相等，字符串 `"1"` 不等。

### 6.2 Matcher

```text
exact(string)  → 原样、区分大小写、完整值
exact(number)  → 无精度损失的规范数值相等
exact(bool)    → 布尔相等
glob(string)   → internal/matcher 的完整值 glob
regex(string)  → 启动期编译为 \A(?:pattern)\z
```

使用 Go RE2 的绝对文本边界 `\A/\z`，不能用会受 `(?m)` 和尾换行影响的 `^/$`。只有字段存在且类型合法后才应用 negate。`all` 从左到右短路 false，`any` 从左到右短路 true；matcher 的意外错误或 panic 生成诊断并使该规则不匹配，Dispatcher 继续后续规则。

### 6.3 模板

模板只识别完整的 `{{a.b.c}}` 占位符 token，不提供函数、管道、循环、条件、默认值或任意表达式。加载期完成语法和固定字段可用性校验；运行期要求字段存在且为 scalar：

- string 直接插入；
- bool 使用 `true | false`；
- number 使用规范 decimal 文本；
- nil、object 或 array 使整次动作失败。

先把全部占位符渲染到临时缓冲区，再脱敏和检查限制，最后一次性提交。任何失败都不能留下部分 Prompt 或部分 SubAgent input。

## 7. 动作执行设计

### 7.1 统一结果

内部 action runner 返回：

```go
type ActionOutcome struct {
    Success  bool
    Decision *ToolDecision
    Duration time.Duration
}
```

`Success` 只表示动作按协议完成。合法 allow、合法 deny、Prompt 原子写入和 SubAgent 诊断 no-op 都算成功；进程/网络/模板/协议/超时/队列/取消错误都失败。失败细节只进入 diagnostics，Dispatcher 不把 error 返回业务流程。

### 7.2 Command

Command Runner 与 Agent `Bash` Tool 完全独立：

- 固定使用 `/bin/sh -c <static command>`，cwd 为绝对 project root。
- `command` 与 `env` 都是启动期静态值，校验器拒绝事件占位符；事件数据只能经 stdin 进入进程。
- env key 必须符合 POSIX 名称 `[A-Za-z_][A-Za-z0-9_]*`、不得为 `PWD`，并遵守 128 B/64 项限制；展开后 value 拒绝 NUL。
- 基础环境只复制 `PATH`、`HOME`、`TMPDIR`、`LANG`、`LC_ALL`、`LC_CTYPE`、`TERM`，再应用规则静态 env，最后强制 `PWD=project root`，不允许规则伪造 PWD。
- 规范 EventContext JSON 写 stdin；无法在 1 MiB 内序列化则不启动进程。
- 默认 timeout 30 秒；显式 timeout 使用 rule 值，并同时响应 Engine root/调用 context 取消。
- stdout、stderr 使用独立 32 KiB hard-limit writer。任一越界立即取消进程组，动作失败；不能截断后继续解析 decision。
- Unix Runner 使用独立 process group；stdin 写入后总是关闭，stdout/stderr 从启动起并行 drain。取消/超限先向整个组发 SIGTERM，短 grace 后 SIGKILL，并用 `WaitDelay`/等价 join 保证 `Wait`、pipe goroutine 和孙进程测试可收束。
- 原始输出只存在于有界内存；不写文件、不进 Conversation、不进普通 TUI。诊断只记录阶段和大小类别，不复制输出。
- `decision: true` 时只把有界 stdout 交给严格 decision parser；stderr 永不参与模型结果。
- Runner 不调用工具 Executor、permission 或 Hook Dispatcher，因此不会递归触发事件。

### 7.3 HTTP

加载期 URL 策略：

- URL 必须静态、绝对、无 fragment，长度不超过 2 KiB。
- URL 不做环境变量展开，也不接受 EventContext 占位符；只有 Header value 使用 §5.3 的启动期环境展开。
- 拒绝 URL userinfo、opaque URL、空 host 和控制字符，避免凭据混入 URL 或解析歧义。
- scheme 只允许 HTTPS，或 host 精确为 `localhost`/loopback IP literal 的 HTTP。
- method 默认 POST，限定 GET/POST/PUT/PATCH/DELETE。
- Header 名称按 HTTP token、值按合法 field value 校验，拒绝 CR/LF/NUL；名称大小写不敏感去重。`Host`、`Content-Length` 禁止配置；`send_event: true` 时由 Runner 唯一设置 `Content-Type: application/json`，冲突配置启动失败。最多 32 个 Header，名称和值分别受限。
- URL/Header 禁止事件模板；Header 环境展开值注册到 RuntimeRedactor。

运行期使用禁用环境代理的专用 `http.Client`、resolver 和 dialer，避免 loopback 请求被 `HTTP_PROXY` 转发到外部：

- 连接 `localhost` 时检查全部解析候选均为 loopback，并只拨号已验证地址，防止 DNS rebinding。
- 每次 redirect 重新校验；最多 3 次，只允许 same-origin，禁止 HTTPS 降级。
- Redirect 必须保留原 method 和 event body；不能采用 301/302/303 的默认 POST→GET/丢 body 行为。每一跳从有界 `GetBody` 重建请求。
- `send_event` 默认 true，body 为同一 EventContext JSON，Content-Type 为 `application/json`；false 时不构造事件 body。
- 默认 timeout 10 秒；Transport 的 response-header 上限固定为 64 KiB，响应 body 在自动解压之后应用 64 KiB hard limit。
- 非 2xx、网络/解析/redirect/timeout/body 超限全部失败。
- `decision: true` 只解析成功 2xx 的完整有界 response body。

HTTP Client、resolver、dialer 在测试中可注入；自动化测试只使用 `httptest` 和 loopback，不访问公网。

### 7.4 Prompt

Prompt action 不是外部任务，而是同步更新 `PromptState`：

1. 从同一 EventContext 快照读取原值并原子渲染；只对最终 Provider block 做统一脱敏，不回写或派生另一份事件快照。
2. 检查 128 KiB 单片段限制。
3. 解析当前 session/execution/turn binding。
4. 在锁内同时检查目标 owner 总量和当前 Agent 请求可见的 session + execution/turn/next 聚合总量都不超过 256 KiB。
5. 以完整 entry 一次性追加；失败时状态不变。

Entry 保存 scope、binding、sequence、effective ordinal、source 和 content。每次 snapshot 按 `(sequence, effective_rule_ordinal)` 排序，并为每条规则生成独立 Provider system block，保留来源边界。

预算 owner 不按 scope 各算一份：execution-bound next 与该 Turn 的 turn entries 统一计入 execution owner；session-bound next 与 session entries 统一计入 session owner。每个 owner 最多 256 KiB，同时任一 `PromptLease.Blocks()` 可见的跨 owner 聚合也最多 256 KiB。所有检查与写入在同一锁内完成，不能通过 next/turn 或 main/isolated 并发绕过。

`scope` 省略时固定为 `turn`。加载期使用以下完整矩阵，矩阵外组合直接拒绝启动：

| event | 允许 scope |
| --- | --- |
| `system_start`、`system_stop`、`session_end` | 不允许 Prompt action |
| `session_start` | `next`、`session` |
| `turn_start`、`message_before`、`message_after`、`tool_before`、`tool_after` | `next`、`turn`、`session` |
| `compact_before`、`compact_after` | `next`、`turn`、`session` |
| `turn_end` | `session` |

`/compact` 在 Turn 外触发时仍允许命中已通过加载校验的 compact/turn 规则，但因没有 active Turn 而产生 `hook_prompt_scope_unavailable`，不写入任何 Prompt，压缩本身继续。

Prompt 模板作者把哪个 EventContext scalar 放进 system block 是显式的高权限选择；动态 message/tool 文本不会获得可证明的“提示词转义”。文档必须提示这种内容可能携带 prompt injection。无论模型如何理解 Hook Prompt，Plan policy、Skill tool view、硬安全、普通 permission 和 Executor Grant 都继续由代码强制，Prompt 不能修改这些边界。

作用域：

| scope | binding | 可见性 | 消费/清理 |
| --- | --- | --- | --- |
| next，有 execution | execution ID | 该 execution 下一次真实 Agent 请求 | Provider 接受请求后消费；turn_end 丢弃残留 |
| next，无 execution | session ID | 该 session 内最先实际发送的主或 isolated Agent 请求 | 接受后消费；session_end 丢弃残留 |
| turn | turn ID | 该 Turn 剩余所有 iteration | turn_end 后清除 |
| session | session ID | 主执行及 isolated execution | session_end 后清除 |

`/clear` 只清 TUI 展示，不结束 Conversation，因此不清 Hook Prompt。Conversation 新建、恢复、切换或退出按 Session 边界清理。

### 7.5 SubAgent 占位

SubAgent 只验证 `agent`、`input` template 和限制。模板与运行期完整渲染结果都不得超过 64 KiB；触发后先原子渲染，记录 `hook_subagent_not_implemented`，不创建 Agent、不产生嵌套事件、不返回工具决策，并以成功 no-op 提交 once。若配置 async，则 no-op 仍经过受管队列，以验证相同的 admission/once/关闭语义。

### 7.6 工具 decision

Decision parser 先要求输入是有效 UTF-8，再使用 token 级 JSON object decoder 记录已见 key；它必须拒绝重复 key、未知 key、非 string value、额外 token 和尾随非空内容，只接受：

```json
{"decision":"allow"}
{"decision":"deny","reason":"non-empty"}
```

allow 不允许 reason，deny 必须有经 trim 后非空且不超过 2 KiB 的 string。解析前后 JSON whitespace 可存在，任何重复/额外字段、额外 JSON、正文、未知值、无效 UTF-8 或超限都按失败处理。deny reason 在协议校验后移除 ANSI、NUL 和除换行/Tab 外的终端控制字符，再统一脱敏；安全化后若为空则该 decision 失败。不能通过清理或截断把非法超长决策变成合法 deny。

## 8. Dispatcher、once 与异步执行

### 8.1 Dispatch 算法

```text
构造并冻结 EventContext，分配 sequence
  → 按 effective_rule_ordinal 扫描 snapshot
  → event 不同：跳过
  → condition 不匹配：跳过
  → once 原子 reserve；pending/done：跳过
  → async：非阻塞入队，成功返回；失败 release + diagnostic
  → sync：执行并 recover panic
       ├─ failure：release once + diagnostic，继续规则
       ├─ allow：commit once，继续规则
       ├─ deny：commit once，立即返回 deny
       └─ success：commit once，继续规则
```

只有同步 `tool_before` 能返回 deny。异步完成顺序不影响 dispatch 结果和工具决策。

### 8.2 once 状态机

```text
idle ──reserve──► pending ──success──► done
  ▲                  │
  └── failure / timeout / cancel / queue reject ──┘
```

状态 key 使用规范 source + 声明序号。条件在 reserve 前求值。同步和异步共用同一 mutex/CAS 语义；并发看到 pending 或 done 都跳过，不等待。重启后 map 为空。

### 8.3 异步执行器

- 生产默认 4 个 worker，channel 只容纳 64 个等待任务，因此最多 4 running + 64 queued。
- 入队必须 non-blocking；满队列立即失败、释放 once 并诊断。
- 一个 dispatch 只冻结一次 EventContext。入队前先把 action 所需数据变为有界 payload：Command/`send_event` HTTP 共享最多 1 MiB 的规范 JSON，SubAgent 只保存最多 64 KiB 的完整渲染 input，静态 HTTP 不保存事件正文；EventContext 超限的 payload action 在入队前失败。job 只保存该有界 payload 与 rule 引用，不为每条规则复制完整快照，也不引用 Turn/Conversation 可变对象。
- 异步任务在成功入队时就创建 `Engine root + rule timeout` deadline；timeout 包含排队等待，worker 取到已过期任务时不执行并释放 once，避免饱和队列把旧事件延后到不可控时间。
- 任务不因 turn/session 结束取消；worker 对每个任务单独 recover panic，走与同步动作相同的 diagnostic/once-release 路径。
- `Close/Shutdown` 是 `sync.Once` 保护的状态机，不能重复 close channel 或向关闭 channel 发送。

admission 状态检查、nonblocking channel send 与关闭 channel 必须受同一 RWMutex/状态机保护；不能先检查 atomic flag 再与 `close(queue)` 竞态。Shutdown 先取得写锁切换为 closing、阻止新 sender，再关闭队列；worker 不关闭共享队列。

关闭顺序固定：

```text
同步 dispatch system_stop（仍允许它的 async action 入队）
  → 关闭新 admission
  → 等待队列和运行任务，最多 2 秒
  → cancel Engine root context
  → Command Runner 终止进程组、HTTP 取消请求，并在固定 1 秒 post-cancel join 窗口内等待 worker 退出
  → 对被取消/未完成数量写一条汇总诊断
```

2 秒是正常 drain 窗口，不包含之前同步执行的 `system_stop` 规则；两个文件最多合并 512 条规则，因此极端同步 dispatch 上界是 512 × 每条 10 分钟，即 85 小时 20 分钟。本版本依照已批准规格不另加会跳过规则的 aggregate dispatch timeout，这属于受信配置的明确风险。异步关闭阶段在 system_stop 返回后总计最多 3 秒。Command/HTTP runner 必须在 root cancel 后终止自己的 I/O、子进程和内部 goroutine；1 秒 join 超限记录诊断并返回，不能让 `Shutdown` 无限等待。该取消契约通过泄漏与 race 测试证明，正常实现不得遗留 worker。

## 9. 生命周期接线

### 9.1 System 与 Session

`app.Deps` 增加 nil-safe `Hooks hook.Runtime`。`app.New` 完成 Orchestrator、命令、状态和依赖装配后调用 `SystemStart`，再根据 `ui.start_mode` 创建初始 Conversation，保证 start 顺序。

Conversation 切换采用事务式顺序：

```text
先 load/create 候选 Conversation（失败则旧 Session 不动）
  → 等待当前 Turn 已结束
  → final save 旧 Conversation
  → session_end(old, switch)
  → 清 Skill/模式/旧 Session 状态
  → 激活候选 Conversation
  → session_start(new|resumed)
```

进程退出采用：

```text
取消并等待活动 Agent Run 完成 turn_end
  → final save 当前 Conversation
  → session_end(exit)
  → 清 Skill/Session 状态
  → Hook Shutdown（含 system_stop 与 async drain）
  → MCP/其他资源关闭
```

保存失败表示“保存尝试已经完成”，错误仍显示/诊断，但不能跳过 session_end。所有 Close 路径幂等。

`tui.Run` 返回最终 tea.Model，主函数无论正常退出还是 TUI error 都对最终 App Model 调用 `Close(context.Context) error`。Bubble Tea 的 Model 会按值复制，因此 close-once、active request 和 wait group 放在所有 Model 副本共享的 `*lifecycleState` 中，不能把 `sync.Once` 直接嵌进值 Model。

退出键的 `Update` 不同步等待或关闭资源：无 active run 时只返回 `tea.Quit`；有 active run 时先进入 canceling 并继续消费事件，待 run 收尾后下一次退出。真正的 session/system/MCP close 由 TUI 返回后的 main coordinator 串行执行。`main` 改为 `run() error`，并额外 defer 同一个幂等 coordinator，覆盖 App 尚未创建或 TUI 异常提前返回的情况；不再用中途 `os.Exit` 绕过 defer，也不重复关闭 MCP。

Coordinator 不复用一个已过期的 2 秒 context：Hook Shutdown 使用自己的 rule timeout、2 秒 drain 和 1 秒 join；完成后再为 MCP 创建独立的新 2 秒 context。否则 MCP 预算会提前取消 system_stop，或 system_stop 会吃完 MCP 关闭预算。

### 9.2 Turn 与 Message

`SendRequest` 完成输入、mode、Conversation 和 profile 验证后才创建 execution：

`executionState` 从一开始就持有不可变 `ExecutionRef`；`streamWithProfile`、tool batch、Skill system route 和 compaction adapter 都显式接收该 state/ref，不从全局变量推断“当前执行”。这是 Prompt、Tool 和 Compact 三条接线的共同前置重构。

```text
BeginTurn / turn_start
  → message_before(user)
  → AppendUserMessage
  → message_after(user)
  → UserSubmitted UI event
  → runAgentLoop（任意 Provider iteration）
  → EndTurn / turn_end
  → terminal Done/Error UI event
```

Agent Loop 中每次 Provider stream 收束后，如果产生要写入 Conversation 的 assistant text，就用同一 message ID 包围一次 append。thinking、delta、tool call/result、context summary、Memory 和 UI 临时拷贝不触发 Message。

所有退出分支归一成 `RunResult{Reason, Err, FinalText, Usage, Duration}`。内层 `runAgentLoop` 不再发送 Done/Error 等 terminal UI event，只返回结果；main 与 isolated 的统一 run wrapper 依次执行 assistant 收尾、`EndTurn`、terminal UI event、关闭 stream，再映射状态：

| Agent stop reason | Hook turn.status |
| --- | --- |
| completed | completed |
| max_iterations | max_iterations |
| canceled | canceled |
| provider/tool/internal/unknown-tool 等错误终止 | error |

`turn.error` 只放脱敏安全摘要。`EndTurn` 放在统一 defer/finalizer 中，使用 Engine 生命周期 context，使请求 context 已取消时仍能分发 canceled end；之后才允许 UI 清 request 或接受下一条输入。

Orchestrator 增加覆盖普通与直接 isolated Skill 的活动 run tracker 和 `WaitIdle(ctx)`。当前 `cancelRequest` 不再立即把 streaming/request 清空，而是发送 cancel、设置 `canceling=true` 并保持输入禁用。`listen` 在 channel 关闭时必须发送显式 `eventStreamClosedMsg`，不能返回 nil；App 收到 terminal event/closed msg 且 run tracker 已收尾后才清 request，消除“第二次 q 先发 session_end”的竞态。

### 9.3 Compact

Context Manager 新增局部接口：

```go
type CompactionObserver interface {
    Before(context.Context, Attempt) any
    After(context.Context, any, Result, error)
}
```

该接口由 Orchestrator/App 侧的窄 adapter 实现：adapter 把 `Attempt/Result` 转成 `hook.BeforeCompact/AfterCompact` 输入。`contextmgr` 不导入 `hook`，`hook` 也不导入 `contextmgr`，避免基础 Engine 与上下文实现形成反向依赖。

接线使用 request-local options，而不是修改 Manager 上的全局 observer：

```go
type PrepareOptions struct {
    Mode             Mode // auto | manual
    PersistArtifacts bool
    Observer         CompactionObserver
}

func (m *Manager) PrepareWithOptions(ctx context.Context, conv *conversation.Conversation, opts PrepareOptions) (Result, error)
```

现有 `Prepare/CompactNow` 保留为兼容 wrapper。`sessionctx.Manager` 增加对应的 `PrepareWithOptions` 透传：main 使用 `PersistArtifacts=true`，isolated 使用 auto + false，manual 使用 true。

`Attempt` 包含 reason、消息数和估算 Token。Manager 在任何 mutation 前先做只读 preflight：检测待外置结果、摘要阈值和熔断状态。只有以下情况分发事件：

- manual `/compact`：始终是一轮真实尝试，即使最终 Changed=false；
- auto：确实将外置至少一条结果或将尝试摘要；
- isolated transient：确实将尝试内存摘要。

`compact_after` 复用 before token 中保存的同一份前置统计，并在可用时添加后置统计。Manager 内部单独维护 `attemptErr`：即使 auto summary 错误尚未达到对业务返回 error 的熔断阈值，after 仍报告 error；对调用者返回值继续保持现有策略。Observer/Hook 错误自身 fail-open。

生命周期 identity 通过 request-local context/observer adapter 传递，不在 Context Manager 存全局“当前 Turn”：

- 主 Agent 自动压缩带 main ExecutionRef；
- 独立 Skill 临时压缩带 isolated ExecutionRef；
- `/compact` 只带 active session，不伪造 execution/turn。

Session Context 增加 transient prepare 入口。独立 Skill 继续加载 instructions/memory，但允许阈值触发不落盘的内存摘要；禁用会产生孤儿 blob 的持久外置，并且不保存临时 Conversation。

只读 preflight 不调用会执行 `conversation.EnsureContext` 的现有估算路径；新增纯函数 snapshot estimator，先从不可变消息视图计算 before stats 和计划，真正进入 attempt 后才更新 Conversation metadata。

Prompt 的 `AcquirePrompts` 必须发生在 Session/Context preparation 完成后，因此 `compact_after` 生成的 Prompt 可以进入即将发送的 Agent 请求。Context Manager 自己的 summary Provider 请求不调用 Hook Prompt API，也不触发 Message/Turn。

## 10. Prompt 拼装与 Provider 兼容

当前实现把所有 stable block 放在 dynamic block 前，无法把 Hook 精确插入“固定安全指令之后、Skill 和普通运行时上下文之前”。因此增加统一有序 block 路径：

```text
1. 当前 `fixedStableSections()` 的全部不可变系统块，原顺序和内容不变
2. Hook Prompt blocks（non-cacheable，sequence/ordinal 顺序）
3. project instructions / memory / restore boundary 等 optional stable blocks
4. Skill catalog metadata
5. active Skill SOP
6. runtime mode reminder
```

这里的“Skill 前”同时覆盖轻量 catalog 与 active SOP。Hook 不能删除、替换或移动第 1 层。

`provider.ChatRequest` 增加首选 `System []SystemBlock`；旧 `StableSystem`/`DynamicSystem`/`SystemPrompt` 保留兼容。Provider 按以下优先级读取：

1. `System` 非空时严格保持其顺序和每块 Cacheable 标记；
2. 否则走当前 Stable → Dynamic；
3. 两者都空时走 legacy SystemPrompt。

`CachePolicy` 增加 ordered-path 专用的 `SystemBreakpointName` 与 `CacheTools`；普通旧路径沿用当前默认，Hook 路径把 breakpoint 指向最后一个 fixed block 并设置 `CacheTools=false`。

有序路径在 wire 上也保留 block 边界：Anthropic 一块对应一个 content block；OpenAI 一块对应一条按序排列的 system message，不再把 Hook entries 用空行合成一条。无 Hook 的 legacy 路径仍维持当前单 system message，避免无关请求变化。`SystemBlock.Name` 只用于本地定位，不把绝对 source path 发给 Provider。

当没有可见 Hook Prompt 时，Orchestrator 继续构造现有 Stable/Dynamic 路径，保证请求内容与 cache boundary 尽可能逐字节不变。存在 Hook Prompt 时才使用统一有序路径。Anthropic 当前把 cache breakpoint 放在最后一个 Cacheable block，因此该路径只允许 Hook 之前的最后一个 fixed block 成为 system breakpoint；Hook 及其后的 optional/catalog/active/runtime blocks 都不能再声明 breakpoint。同时禁用该请求的 tool-definition cache breakpoint，否则它会把更早的动态 Hook 一并纳入缓存前缀。这会降低后置稳定内容与工具定义的缓存收益，是满足精确顺序与 non-cacheable 语义的明确取舍。

## 11. Tool 安全链与 `load_skill`

### 11.1 结构预检

`internal/tool` 增加 `ValidatedCall`：

```go
type ValidatedCall struct {
    Call      Call
    Tool      Tool
    Arguments map[string]any
}

func (r *Registry) ValidateCall(Call) (ValidatedCall, error)
```

预检把空白 arguments 兼容性归一为 `{}`；其他输入必须是且只有一个 JSON object、没有 trailing JSON，并用 `UseNumber` 保持数字。这里执行公共结构校验；工具专属业务字段校验仍由具体 Tool/`load_skill` handler 完成，因此专属错误属于“已经进入执行”的 error，并触发 `tool_after`。

Registry 使用两个只读视图避免顺序含糊：Provider-visible view 继续隐藏 Plan Mode 写工具；preflight view 只应用 Skill allowlist 和系统工具保留，不提前应用 Plan 过滤。这样伪造调用可稳定区分为：基础注册表未知/Skill 不可见时在第 1 层拒绝；已注册但 Plan 不允许时在第 2 层拒绝。两类都发生在 Hook 前。

### 11.2 Permission 拆分

`Authorizer.Decide` 拆为可组合阶段，同时保留旧方法作为兼容 wrapper。`permission` 不能导入 `tool.ValidatedCall`，因为 Tool Executor 已依赖 permission；Orchestrator 只把无包耦合的 `permission.Call + map[string]any` 转交：

```go
NormalizeArguments(call Call, arguments map[string]any, context Context) (NormalizedCall, error)
CheckHard(normalized, permission.Context) *Decision
DecideOrdinary(normalized, permission.Context) Decision
Decide(call, context) Decision // 依次调用以上阶段
```

`CheckHard` 包含 permission 配置损坏保护、权限配置文件保护、路径 sandbox 和 Bash blacklist；普通 layer 匹配、mode 默认策略及用户 ask 留在 `DecideOrdinary`。Plan Mode 在 Orchestrator 的显式 profile policy 阶段先执行，保留 `load_skill` 的既有系统工具例外。

`NormalizeArguments` 失败属于 Hook 前的结构/硬安全拒绝，不触发 `tool_before` 或 `tool_after`；只有通过公共 preflight 后、进入具体 Tool/`load_skill` handler 才发现的业务参数错误，才算 actual execution error 并触发 `tool_after`。

首次 `ValidateCall` 得到的 arguments 必须贯穿 Hook、permission 和 actual execution，不再用普通 `json.Unmarshal` 解成 float64。Executor 增加 `ExecuteValidatedAuthorized(ctx, ValidatedCall, Grant)`，`ExecuteAuthorized` 仅作为旧调用兼容 wrapper；`ResolveUserDecision` 同样增加接收既有 `NormalizedCall` 的入口，用户确认后不重新解析。`load_skill` 专用 handler 也从 validated map 做字段/未知 key 校验。这样 Hook snapshot、MCP arguments 和 Grant fingerprint 使用完全相同的 JSON 类型和值。

### 11.3 固定执行链

```text
基础 registry + Skill/profile visibility + JSON object 结构
  → Plan Mode policy（保留 load_skill 系统例外）
  → permission Normalize + CheckHard
  → hook.BeforeTool（同步）
       ├─ deny → hook_denied tool result，短路
       └─ continue
  → ordinary permission rule / user confirmation
       ├─ deny → permission_denied tool result
       └─ allow + Grant
  → actual executor or system route
  → hook.AfterTool
  → TUI result + Conversation tool history
```

未知工具、profile/Skill 白名单隐藏工具、Plan 拒绝、硬安全拒绝、Hook deny、普通权限拒绝都不触发 `tool_after`。Hook allow 只表示继续，不能生成 Grant 或跳过后续权限。

Hook deny 构造：

```go
tool.Result{
    Status:  tool.StatusDenied,
    Content: safeReason,
    Error: &tool.Error{
        Code:        tool.ErrHookDenied, // "hook_denied"
        Message:     safeReason,
        Recoverable: true,
    },
}
```

结果沿现有 tool_call/tool_result history 进入下一次模型请求，不写 session/local/project/user 权限规则。UI 可复用当前 ToolDenied 展示，但不显示 Hook 配置和内部输出。

`tool_after` 在 executor/system route 返回、结果完成执行器限长和 RuntimeRedactor 后、UI/Conversation 写入前同步分发。`duration_ms` 只计算实际 executor/system handler 从进入到返回的耗时，不包含 Hook preflight 或用户确认等待；Event 状态映射为 success、timeout 或 error，并只附带有限的模型可见 content/error。

### 11.4 `load_skill` 消除旁路

当前 `load_skill` 在 `handleSkillToolCalls` 中绕过普通 tool batch。计划抽取公共 `prepareRegisteredCall`，所有内置、MCP 和 `load_skill` 都先经过 registry/Plan/hard/Hook。

Hook allow 后，`load_skill` 进入 `systemRoute`：

- 有意跳过普通 permission（保持现有系统级豁免）；
- 执行严格参数解析、Skill resolve/activate 或 isolated runner；
- handler 成功、语义失败或 timeout 都触发 `tool_after`；
- Hook deny 不激活 Skill、不触发 after；
- isolated route 的 after 包围完整 handler，主 trace 即使被替换也不影响 Hook 事件已发生。

## 12. 独立 Skill 语义

`RunIndependent` 为临时 Agent Run 创建新的 `ExecutionRef(kind=isolated_skill)` 和 turn ID，session ID 继续使用主 Conversation ID，不分发 session start/end。

```text
父 main execution（可能存在）
  └─ load_skill tool route
       └─ isolated execution / isolated turn
            ├─ logical user message before/write/after
            ├─ provider/tool/compact iterations
            ├─ logical final assistant before/write/after
            └─ isolated turn_end
```

消息去重规则：临时 Conversation 中驱动 isolated Agent 的 user 和最终 assistant 是 Hook 的权威逻辑消息；为了 UI 或持久主 Conversation 做的 raw command/final summary bridge copy 标记为 internal copy，不再触发第二组 Message Hook。

状态共享/隔离：

| 状态 | main 与 isolated |
| --- | --- |
| Rule snapshot / diagnostics / async pool | 共享 |
| process once | 共享 |
| session Prompt 与 session-bound next | 共享 |
| execution-bound next / turn Prompt | 按 execution/turn 隔离 |
| Skill Activity / tool allowlist / model | 保持现有独立 profile |
| Session lifecycle | 只由主 Conversation 所有者分发 |

Agent 触发 isolated Skill 时父 Turn 仍按自身最终状态结束；child Turn 是独立嵌套 Agent Run。父 execution 上产生的 next/turn Prompt 不泄露给 child，child 的 execution next/turn Prompt 也不泄露回父级。

## 13. 诊断、脱敏与资源边界

### 13.1 结构化诊断

现有 `diagnostics.Diagnostic` 增加可选、有界的 `Attributes map[string]string`，与单条规则关联的 Hook 诊断固定写入：

```text
event, rule_ordinal, effective_rule_ordinal, action, stage, duration_ms
```

canonical source 复用 `Source`，`hooks[n]` 复用 `Path`。Attributes 固定最多 8 个，key 最多 64 B、value 最多 256 B、序列化总量最多 2 KiB。Collector 在 `Add` 和 `List` 两侧都深复制 map，按 key 排序展示，并对 Source/Path/key/value 做 UTF-8、ANSI/终端控制字符清理和统一脱敏。旧 JSON 字段与旧 Text 输出在无 attributes 时保持不变。

异步关闭超时是跨规则汇总，不伪造某条配置规则：它写入 `event=system_stop`、`stage=shutdown`、真实 `duration_ms` 和被取消/未完成 `count`，并将不适用的 Source、Path、规则序号和 action 留空。

稳定诊断码至少包括：

```text
hook_condition_failed
hook_template_failed
hook_command_failed
hook_http_failed
hook_decision_invalid
hook_action_timeout
hook_async_queue_full
hook_shutdown_cancelled
hook_subagent_not_implemented
hook_prompt_scope_unavailable
hook_limit_exceeded
hook_action_panic
```

诊断 message 只描述错误类别和安全摘要，最多 2 KiB。不得放 EventContext、message content、tool arguments、Header、URL query secret、stdout/stderr、HTTP body/response 或工具原始结果。现有 `/diagnostics` 自动读取同一 Collector，不新增普通聊天消息。

Collector 始终是权威存储，运行期间所有 Hook 诊断都可由 `/diagnostics` 查看。对于 TUI 已退出后才产生的 shutdown drain/cancel 诊断，组合 closer 额外把同一条已清理的安全摘要写到 stderr；不写 payload，也不引入新的持久日志。这样关闭异常不会因交互入口消失而完全不可见。

### 13.2 限制落实点

| 边界 | 检查阶段 | 超限行为 |
| --- | --- | --- |
| YAML/rule/predicate/action 配置 | load/compile | 启动失败 |
| EventContext JSON 1 MiB | event build | 条件仍可用；payload action 失败 |
| Command stdout/stderr | streaming writer | 终止进程，动作失败 |
| HTTP request/response/redirect | client | 动作失败 |
| Prompt template/fragment/owner total | compile/render/locked commit | 不提交任何部分 |
| deny reason | decision parse | 整个 decision 非法并 fail-open |
| diagnostic summary | collector | 安全 UTF-8 限长，不复制原值 |

所有 byte 上限按 UTF-8 字节计算；截断只允许用于非决策的安全展示。凡是会影响 deny 判定的 stdout/response/reason 超限都必须整体失败。

## 14. 向后兼容策略

- 所有新增依赖可为 nil，老构造函数委托到 options 并注入 no-op Runtime。
- 两个 Hook 文件都缺失或规则为空时，不启动 worker、不改变 Prompt block 路径、不增加 Conversation 消息或 UI notice。
- Permission 的 `Decide` 保留原签名和结果；现有 exact/glob 规范化、评分和 YAML 拒绝 regex/negate 的行为由回归测试锁定。
- Tool Executor 保留授权 Grant 校验；Hook allow 不替代 Grant。
- `load_skill` 的权限豁免、Skill tool whitelist、独立历史窗口、模型选择和主历史摘要回流保持不变，只增加公共 Hook 边界。
- `/clear`、`/plan`、`/do`、`/compact` 和 Session 命令仍是本地命令，不产生虚假 Turn/Message。
- Provider legacy `StableSystem`/`DynamicSystem`/`SystemPrompt` API 保留；只有 Orchestrator 有 Hook Prompt 时使用新 ordered blocks。
- Conversation JSON 不增加 Hook ID、Prompt 或诊断字段，恢复旧会话无需迁移。
- Hook 配置不热更新；修改后重启，避免执行中 snapshot、once 和 Prompt 状态迁移。
- README/example 的信任提示覆盖：任意 Shell、HTTP 事件数据外传、普通权限确认前的 action 副作用、把动态数据提升为 system Prompt，以及外部 decision reason。Hook deny 是技术失败时 fail-open 的自动化策略，不描述为不可绕过的安全边界。

## 15. 测试策略

### 15.1 Hook 包单元测试

| 文件 | 覆盖重点 |
| --- | --- |
| `loader_test.go` | 两文件发现/合并、source/ordinal、严格 YAML、行列/路径、Prompt 默认 turn 与组合矩阵、所有配置上限 |
| `event_test.go` | 12 种 payload、对象省略、固定 ID/time、sequence、深复制、UseNumber、JSON 上限 |
| `condition_test.go` | all/any、typed exact、完整 glob/regex（含 multiline/尾换行）、case/no-trim、negate、缺失字段 |
| `template_test.go` | 严格 token、事件字段目录、动态路径、scalar 格式、原子失败 |
| `command_test.go` | shell/cwd/stdin/env、输出边界、取消/timeout、decision、secret canary |
| `http_test.go` | method/body/header、HTTPS/loopback、DNS、redirect、status、timeout/body 限制 |
| `decision_test.go` | 精确协议、重复/额外 key、无效 UTF-8、控制字符、空或超长 reason、stderr 排除 |
| `prompt_state_test.go` | request-attempt next lease、并发 session send gate、sent latch/终态幂等、turn/session 生命周期、共享/隔离、排序和聚合总量 |
| `engine_test.go` | 稳定规则顺序、失败继续、panic recover、首 deny 短路、SubAgent no-op |
| `once_test.go` | sync/async reserve、成功提交、失败/queue/cancel 释放、竞争 |
| `async_test.go` | 4 running、64 queued、排队 timeout、满队列、panic、send/close race、bounded/idempotent shutdown |
| `diagnostic_test.go` | 诊断码、attributes、限长、敏感 canary 不出现 |
| `limits_test.go` | F59/F60 每个 limit 与 limit+1，尤其禁止超限后截断 deny |

测试 seam：注入 clock、ID source、CommandRunner、HTTP client/resolver/dialer、async limits、shutdown grace 和 gated action runner。并发测试使用 barrier/channel，不用 `time.Sleep` 判断顺序。

### 15.2 集成与回归测试

| 文件 | 场景 |
| --- | --- |
| `internal/orchestrator/hook_test.go` | Turn/Message/Tool 顺序、多 iteration、built-in/MCP/load_skill、deny 回流、安全链位置、Prompt 注入、无 Hook 等价 |
| `internal/orchestrator/independent_hook_test.go` | isolated kind、无额外 session、消息去重、共享 once/session Prompt、execution 隔离 |
| `internal/contextmgr/hook_test.go` | 只包真实尝试、before/after 统计、错误路径、summary Provider 无 Hook Prompt |
| `internal/app/hook_test.go` | system/session、save-before-end、switch/exit、取消等待、Prompt 清理、diagnostics 可见 |
| `cmd/xagent/hook_startup_test.go` | 固定路径、非法配置早于副作用、start/stop 和组合 closer 顺序 |
| `internal/permission/permission_test.go` | 旧 matcher/YAML/Grant 行为不变 |
| `internal/prompt/prompt_test.go` | 精确 block 顺序、无 Hook 旧输出不变 |
| `internal/provider/*_test.go` | ordered/legacy system block、cacheable 标记、每条路径恰好一次 `Finish`、pre-send release/post-write commit、写出与取消交错时无迟到 callback 的 barrier |
| `internal/hook/docs_test.go` | example 可解析；README 含信任与重启警告 |

增加 tagged TUI 端到端测试，使用本地 scripted Provider、fake store/tool 和 loopback HTTP，覆盖生命周期、Prompt、allow/deny、timeout、async、once、diagnostics 与 isolated Skill；不连接真实 Provider 或公网。

### 15.3 验证命令

```bash
go test -count=1 ./internal/hook
go test -count=1 ./internal/orchestrator ./internal/contextmgr ./internal/app ./cmd/xagent
go test -race -count=1 ./internal/hook ./internal/orchestrator ./internal/contextmgr ./internal/app
go test -shuffle=on -count=20 ./internal/hook ./internal/orchestrator
go test -tags=integration -count=1 ./internal/app -run TestHookTUIEndToEnd
go test -count=1 ./...
go vet ./...
test -z "$(gofmt -l $(rg --files -g '*.go'))"
git diff --check
go build -o /private/tmp/xagent-hook-system ./cmd/xagent
```

`gofmt -l` 必须无输出；race、shuffle 和完整测试均通过后才进入人工端到端验收。

## 16. 需求与设计映射

| 需求 | 设计位置 |
| --- | --- |
| F1–F5 | §5 配置路径、严格校验、稳定 identity/ordinal、启动快照 |
| F6–F15 | §2、§9、§12 生命周期所有者、精确顺序、本地命令与 isolated |
| F16–F20 | §4.2 EventContext、UseNumber、同一快照、字段目录 |
| F21–F26 | §3.2、§6 条件、共享 matcher、权限兼容 |
| F27–F33 | §7.1–§7.3 Command/HTTP Runner 与网络安全 |
| F34–F40 | §4.4、§7.4–§7.5 Prompt lease/scope 与 SubAgent no-op |
| F41–F47 | §8 timeout、once、顺序、队列和 shutdown |
| F48–F53 | §11 固定工具安全链、decision、hook_denied 回流 |
| F54–F58 | §7.1、§8.1、§13.1 fail-open、panic 隔离、Diagnostics |
| F59–F60 | §7、§13.2 配置期/运行期硬上限 |
| N1–N4 | 不可变 snapshot、sequence/ordinal、同步决策、线程安全状态机 |
| N5–N6 | §5.1 早期纯加载、§10/§14 无 Hook 兼容路径 |
| N7–N10 | §7、§13 有界输入输出、脱敏、受信配置、无递归 |
| N11–N13 | §7.4、§10、§12 Prompt/isolated/安全约束隔离 |
| N14–N17 | §5、§8.3、§15 关闭、无外部依赖测试、race、版本拒绝 |

## 17. 主要风险与控制

| 风险 | 控制 |
| --- | --- |
| `load_skill` 或 MCP 绕过 Hook | 所有注册工具复用同一 `ValidatedCall` 和 preflight；专门集成测试 |
| Hook allow 意外绕过权限 | Hook 只返回 continue/deny，不产生 Grant；Executor 继续校验 Grant |
| 误把 Hook action 当作普通权限保护对象 | 文档明确 Hook 配置受信且可在确认前自行产生 Shell/HTTP/Prompt 副作用；打开不可信项目先审查配置 |
| 把 Hook deny 当成硬安全边界 | 文档明确 action/协议失败时 fail-open；真正硬安全仍由 Plan/permission/Executor 代码保证 |
| Prompt 插入位置破坏固定安全指令 | 统一 ordered blocks，固定安全块永远在 Hook 前；精确顺序测试 |
| 动态 Hook Prompt 降低缓存命中 | 仅有 Hook Prompt 时启用 ordered path；保留 Cacheable 元数据并记录为显式取舍 |
| session_end 早于 turn_end | App 取消后等待 Orchestrator run tracker；终止 UI event 在 EndTurn 后 |
| 独立 Skill 重复 Message Hook | 临时执行消息为权威，主历史 bridge 标记 internal copy |
| Context Summary 误消费 Prompt | Prompt lease 只在 Agent Loop stream 调用点取得；Context Manager 无该依赖 |
| async queue/Close 竞态 | admission 状态机、nonblocking enqueue、root cancel、idempotent close、race test |
| 受信配置堆叠大量同步长 timeout | 文档披露两文件 512 规则的极端 85h20m 上界；v1 按规格不增 aggregate timeout |
| once 并发重复 | condition 后原子 idle→pending；失败/拒绝入队明确 release |
| stdout/HTTP 超限被截断成 deny | 决策输入 hard fail，不允许截断后解析 |
| localhost DNS rebinding/redirect 绕过 | 验证全部解析地址、拨号已验证 IP、逐跳 same-origin/协议校验 |
| 诊断泄露消息或工具参数 | 固定 attributes，禁止 payload，统一 redactor 与 canary 测试 |
| 项目 Hook 执行受信 Shell | README 与 example 显著警告；项目配置可审查、修改后需重启 |

## 18. 实施顺序

1. 建立 `internal/matcher`、diagnostics attributes、lossless tool arguments 等兼容性基础，并先锁定回归测试。
2. 实现 Hook 配置 DTO、严格 Loader/Validator、EventContext、字段读取、条件和模板编译。
3. 实现 Command/HTTP/decision runner、PromptState、once、async 和 Engine dispatch；完成 Hook 包单元/race 测试。
4. 先重构统一 run/message wrapper：让 `executionState` 全程持有 ExecutionRef、内层 Agent Loop 只返回 RunResult，并锁定 turn_end/terminal 顺序。
5. 增加 Provider ordered blocks、request-attempt 终态握手与 Prompt lease 接入，先证明取消/写出竞态正确且“无 Hook 请求不变”。
6. 拆分 Permission 阶段，抽取公共 Tool gate，把普通工具、MCP 和 `load_skill` 接入 before/after 与 deny 回流。
7. 为 Context Manager 增加 preflight/observer/transient prepare，验证 summary 与 Hook Prompt 隔离。
8. 接入 isolated execution，再接入 App 的 System/Session、run tracker、TUI close signal 和幂等 coordinator。
9. 调整 main 启动/关闭顺序，补充 `.gitignore`、README 和 example。
10. 运行单包、集成、race、shuffle、全量 test/vet/build，最后执行 tagged TUI 与人工端到端验收。

每一步保持可编译，并优先提交底层无行为变化的重构，再提交 Hook 行为接线，便于定位回归。
