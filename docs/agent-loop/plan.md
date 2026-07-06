# XAgent Agent Loop Plan

## 架构概览

Agent Loop 在现有 `orchestrator` 之上抽出一个循环执行层。新的循环层负责解析用户请求模式、反复调用 Provider、收集流式响应、执行工具批次、回写会话历史，并在触发停止条件时发送完成事件。TUI 仍只消费 `events.Event`，不直接了解 Provider 或工具执行细节。

整体调用链保持：`app.Model` 接收用户输入 → `orchestrator.Send` 启动一次 Agent Run → Agent Loop 多轮调用 Provider 和 Tool Executor → 事件流推给 TUI → 会话 Store 持久化最终状态。

Provider 层继续只负责把 OpenAI/Anthropic 流式协议转换为统一 `provider.StreamEvent`。工具层继续通过 `tool.Registry` 和 `tool.Executor` 执行具体工具。新增逻辑集中在 orchestrator 层，避免把 Agent Loop 规则散落到 TUI、Provider 或 Tool 内部。

本设计刻意把“能不能把工具暴露给模型”和“收到工具调用后能不能执行”做双层约束：Plan Mode 首先只向模型暴露读类工具；如果模型仍通过历史或 Provider 异常请求非读工具，orchestrator 再做系统级拒绝。这样既减少错误请求，也保证安全边界不依赖提示词。

## 核心数据结构

### RunMode

```go
type RunMode string

const (
    RunModeDefault RunMode = "default"
    RunModePlan    RunMode = "plan"
    RunModeDo      RunMode = "do"
)
```

用途：表示当前用户请求的执行模式。`/plan` 只允许读类工具，`/do` 和普通请求使用全量工具。该模式只作用于当前请求，不写入持久全局状态。

### RunRequest

```go
type RunRequest struct {
    UserText string
    Mode     RunMode
}
```

用途：表示解析前缀后的用户请求。`UserText` 是去掉 `/plan` 或 `/do` 后的任务内容。

### RunOptions

```go
type RunOptions struct {
    MaxIterations       int
    MaxUnknownToolCalls int
}
```

用途：Agent Loop 的安全上限。默认 `MaxIterations=10`，`MaxUnknownToolCalls=2`。先作为 orchestrator 内部默认值实现，后续章节再接入配置文件。

### StopReason

```go
type StopReason string

const (
    StopReasonCompleted     StopReason = "completed"
    StopReasonMaxIterations StopReason = "max_iterations"
    StopReasonCancelled     StopReason = "cancelled"
    StopReasonUnknownTool   StopReason = "unknown_tool"
    StopReasonProviderError StopReason = "provider_error"
)
```

用途：机器可读停止原因，用于事件流和 TUI 状态展示。

### StreamCollector

```go
type StreamCollector struct {
    AssistantText strings.Builder
    ThinkingText  strings.Builder
    ToolCalls     []tool.Call
    Usage         *provider.Usage
    Done          bool
}
```

用途：消费一轮 Provider 流。一边把文本/思考增量实时转发给 UI，一边收集完整文本、thinking、tool calls 和 usage，返回给循环判断下一步。`Done` 表示 Provider 正常结束且没有要求继续消费流。

### ToolBatch

```go
type ToolBatch struct {
    Calls      []tool.Call
    Concurrent bool
}
```

用途：表示一批可一起执行的工具调用。安全工具批次可以并发，危险或副作用工具批次必须串行。

### ToolExecution

```go
type ToolExecution struct {
    Call   tool.Call
    Result tool.Result
    Index  int
}
```

用途：保存工具调用原始顺序和执行结果。并发执行时按 `Index` 复原顺序后再写入会话历史。

### AgentProgress Event Payload

在 `events.Event` 中增加：

```go
type AgentProgress struct {
    Iteration  int
    Max        int
    StopReason string
    Message    string
}
```

用途：TUI 展示“第几轮”“停止原因”“当前进度”。Provider 无关。`StopReason` 为空表示循环仍在进行中。

### UsageDisplay

在 `events.Event` 中增加可选 usage 载荷：

```go
type UsageDisplay struct {
    InputTokens  int
    OutputTokens int
}
```

用途：Provider 透出 token 用量时向 UI 上报。Provider 不提供时不发该事件。

## 模块设计

### orchestrator 请求解析

**职责：** 识别 `/plan`、`/do` 消息前缀，生成 `RunRequest`。

**对外接口：**

```go
func parseRunRequest(text string) (RunRequest, error)
```

**规则：**
- `/plan <任务>` → `RunModePlan`，传给模型的用户内容是 `<任务>`。
- `/do <任务>` → `RunModeDo`，传给模型的用户内容是 `<任务>`。
- 其他输入 → `RunModeDefault`。
- 前缀后内容为空时返回非空输入错误。
- 只识别行首前缀；正文中出现 `/plan` 或 `/do` 不触发模式切换。

### System Prompt 组装

**职责：** 根据当前 RunMode 给模型附加模式约束。

**接口：**

```go
func (o *Orchestrator) systemPrompt(mode RunMode) string
```

**规则：**
- Default/Do：使用现有系统提示，并说明可以循环使用工具直到任务完成。
- Plan：在系统提示后追加只读计划约束：只能观察和计划，不能修改文件或执行命令；最终输出可执行计划。
- 模式约束只进入当前请求的 Provider 调用，不写入会话历史。

### Tool Registry 选择

**职责：** 控制向模型暴露哪些工具。

**接口：**

```go
func (o *Orchestrator) registryForMode(mode RunMode) *tool.Registry
```

**规则：**
- Default/Do：返回完整 registry。
- Plan：返回只包含 `Read`、`Glob`、`Grep` 的只读 registry。
- 如果只读 registry 构建失败或缺工具，应返回可观测错误，不静默退化成全工具。

### orchestrator Agent Loop

**职责：** 多轮执行 ReAct 循环，管理停止条件和会话保存。

**对外接口：**

```go
func (o *Orchestrator) Send(ctx context.Context, conv *conversation.Conversation, userText string) (<-chan events.Event, error)
```

`Send` 保持现有公开接口，但内部改为：解析请求 → 追加用户消息 → 启动 `runAgentLoop`。

内部接口：

```go
func (o *Orchestrator) runAgentLoop(ctx context.Context, conv *conversation.Conversation, req RunRequest, out chan<- events.Event, start time.Time) StopReason
```

**行为：**
1. 从第 1 轮开始，发送进度事件。
2. 调用 Provider，并按当前模式传入 system prompt 和 tool registry。
3. 使用 `StreamCollector` 收集一轮响应。
4. 保存本轮 assistant/thinking 文本；如果一轮先输出文本再请求工具，文本必须位于工具消息之前。
5. 如果没有工具调用，保存会话并以 `completed` 停止。
6. 如果有工具调用，按模式和风险分批执行工具。
7. 回写所有工具结果，进入下一轮。
8. 达到上限、取消、未知工具过多或 Provider 错误时保存已完成内容并停止。
9. 每次停止都发送带 `StopReason` 的进度事件，再发送 Done 或 Error。

### StreamCollector

**职责：** 消费一轮 Provider 流，完成“双路收集”。

**对外接口：**

```go
func collectProviderStream(ctx context.Context, stream <-chan provider.StreamEvent, out chan<- events.Event) (StreamCollector, StopReason, error)
```

**行为：**
- TextDelta 立即转发，同时追加到 `AssistantText`。
- ThinkingDelta 立即转发，同时追加到 `ThinkingText`。
- ToolCall 收集 `ToolCalls` 后结束本轮收集；Provider 不应在同一轮 ToolCall 后继续发送文本。
- Usage 收集后发 usage 事件。
- Done 表示本轮无更多工具调用。
- Error 返回 provider error 停止。
- ctx 取消时返回 `StopReasonCancelled`。

### 工具过滤和批处理

**职责：** 根据模式和工具风险级别决定执行方式。

**接口：**

```go
func (o *Orchestrator) filterToolCall(mode RunMode, call tool.Call) (tool.Result, bool)
func (o *Orchestrator) makeToolBatches(calls []tool.Call) []ToolBatch
```

`filterToolCall` 返回 `(result, blocked)`：
- `blocked=false` 表示允许正常执行。
- `blocked=true` 表示不执行工具，直接把 `result` 当作工具结果回写。

**Plan Mode 规则：**
- 允许 `Read`、`Glob`、`Grep`。
- 拒绝 `Write`、`Edit`、`Bash` 和未知非读类工具。
- 拒绝结果作为普通工具结果回写，并允许模型下一轮继续生成计划。

**未知工具规则：**
- 未知工具始终不执行，返回 `tool_not_found` 结果。
- 连续未知工具计数按“模型轮次”累计：一轮中只要存在未知工具且没有任何成功执行的已知工具，就计为一次连续未知轮。
- 任一已知工具成功执行或模型输出无工具最终回复后，连续未知计数清零或结束。

**批处理规则：**
- 连续 `RiskSafe` 工具组成一个并发批次。
- 每个 `RiskDangerous` 工具单独成一个串行批次。
- 批次按模型原始工具顺序执行。
- 并发批次内部结果按原始顺序回写。
- 被 Plan Mode 阻止的工具不参与并发执行，但仍按原始顺序生成工具结果。

### 工具执行器适配

**职责：** 执行单个或一批工具，并输出事件。

**接口：**

```go
func (o *Orchestrator) executeToolBatches(ctx context.Context, mode RunMode, batches []ToolBatch, out chan<- events.Event) []ToolExecution
```

**行为：**
- 对每个工具发送 pending/running/result 事件。
- 安全并发批次使用 goroutine 并发执行。
- 串行批次按顺序执行。
- ctx 取消后不启动新的工具；已启动工具依赖 executor 的 ctx 超时/取消返回。
- 已有危险工具确认机制继续生效；本章不新增确认机制。
- 确认中的危险工具如果 ctx 取消，返回 cancelled 停止，而不是继续等待用户输入。

### 会话回写

**职责：** 保证 Provider 工具协议顺序合法。

**规则：**
- 每个 tool call 必须写入一个 `RoleToolCall`。
- 每个 tool call 必须紧随其后或按 Provider 可接受顺序写入对应 `RoleToolResult`。
- 并发工具也必须按模型原始顺序回写。
- 如果一轮 assistant 文本先于 tool call 出现，先写 assistant 文本，再写工具消息。
- 中途停止时保存已经完整产生的消息；未完成的工具不写入成功结果。

### events 扩展

**职责：** 增加 Agent Loop 进度、停止原因和 usage 事件。

新增事件类型：

```go
AgentProgress Type = "agent_progress"
UsageUpdated  Type = "usage_updated"
```

`events.Event` 增加：

```go
Usage    *UsageDisplay
Progress *AgentProgress
```

TUI 可以展示这些事件，也可以忽略 usage 缺省。

### TUI 集成

**职责：** 展示循环进度和停止原因，不参与循环控制。

**行为：**
- 收到 AgentProgress 时更新状态栏，例如“第 3/10 轮”。
- 收到停止原因时显示“已完成 / 达到迭代上限 / 已取消 / 未知工具过多 / Provider 错误”。
- 收到 UsageUpdated 时可在状态栏显示 token 用量；没有 usage 时不显示。
- 继续复用现有工具行显示 pending/running/success/error/denied。

## 模块交互

```text
app.Update
  └─ orchestrator.Send(ctx, conv, rawUserText)
       ├─ parseRunRequest(rawUserText)
       ├─ AppendUserMessage(conv, cleanedUserText)
       └─ runAgentLoop
            ├─ emit AgentProgress(iteration)
            ├─ provider.StreamChat(ChatRequest{Messages, ToolDefs=registryForMode(mode), SystemPrompt=systemPrompt(mode)})
            ├─ collectProviderStream → TextDelta/ThinkingDelta/ToolCalls/Usage
            ├─ append assistant/thinking messages
            ├─ if no tool calls → save conversation → Progress(completed) → Done
            ├─ filter tool calls for mode and unknown tool handling
            ├─ makeToolBatches(toolCalls)
            ├─ executeToolBatches
            │    ├─ ToolPending / ToolRunning
            │    ├─ tool.Executor.Execute, Denied, or blocked result
            │    └─ ToolSuccess / ToolError / ToolDenied
            ├─ append tool_call/tool_result messages in original order
            └─ next iteration
```

## 文件组织

```text
internal/orchestrator/
├── chat.go              — 保留 Send 入口，接入 Agent Loop
├── agent_loop.go        — runAgentLoop、停止条件、循环主流程
├── run_request.go       — RunMode、RunRequest、parseRunRequest、systemPrompt
├── stream_collector.go  — collectProviderStream 双路流式收集
├── tool_batches.go      — 工具模式过滤、风险分批、批量执行
└── chat_test.go         — 既有测试保留，新增 Agent Loop 场景

internal/events/
└── events.go            — 新增 AgentProgress、UsageUpdated 事件载荷

internal/app/
└── update.go            — 消费新增进度/usage 事件，更新 status

internal/tui/
└── status.go            — 展示迭代进度、停止原因和可选 usage

internal/tool/
└── registry.go          — 如需要，增加构建只读 registry 的能力

.claude/
└── agent_loop_smoke.go  — 新增 Agent Loop smoke 场景

docs/agent-loop/
├── spec.md
├── plan.md
├── task.md
└── checklist.md
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| Agent Loop 所在层 | orchestrator | orchestrator 已经拥有 Provider、Store、Resources、Registry、Executor，适合协调循环，不污染 TUI/Provider/Tool。 |
| Send 公共接口 | 保持不变 | 避免 app 大改；`/plan` `/do` 在 orchestrator 内解析。 |
| 默认最大迭代次数 | 10 | 足够覆盖一般本地开发任务，同时作为安全兜底。 |
| 未知工具停止阈值 | 连续 2 个未知工具轮次 | 一次未知工具可以回写让模型纠正，连续两轮说明模型无法恢复。 |
| Plan Mode 入口 | 消息前缀 `/plan`、`/do` | 符合本章范围，避免引入独立 UI 状态机。 |
| Plan Mode 权限 | 只暴露读类工具 + 执行前系统过滤 | 双层保护，既减少模型误调，也防止非读工具被执行。 |
| Plan Mode 系统提示 | 当前请求追加模式约束 | 不污染会话历史，符合“当前请求生效”的 spec。 |
| 多工具并发 | 仅连续安全工具并发 | 保证副作用工具顺序可预测，兼顾读类工具效率。 |
| 混合工具顺序 | 安全工具不能越过前面的危险工具 | 保持模型原始意图和可复现性。 |
| 结果回写顺序 | 始终按模型原始 tool call 顺序 | 满足 Provider 工具协议，也保证测试稳定。 |
| 停止原因 | 机器可读事件 + TUI 文案 | 便于 UI 展示、测试断言和后续扩展。 |
| Usage 事件 | 可选透出 | Provider 不一定提供 usage，不能阻塞循环。 |
| 确认机制 | 复用现有危险工具确认 | 本章明确不扩展权限/确认系统，降低范围。 |
| 配置接入 | 本章先用内部默认值 | 避免扩大配置文件兼容范围，后续章节再暴露配置。 |
