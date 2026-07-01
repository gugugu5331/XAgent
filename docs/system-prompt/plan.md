# System Prompt Plan

## 架构概览

本章新增一个纯数据的 prompt 构建层，负责把稳定系统提示和动态系统补充分开生成。`internal/prompt` 必须作为叶子包，不依赖 `provider`、`orchestrator`、`conversation` 或 `tool`，避免循环依赖和跨层耦合。

Orchestrator 在每次 Agent Loop provider 调用前构造 `prompt.BuildRequest`，再把 prompt 输出转换成 provider DTO。Provider 层不再直接依赖 `tool.Registry` 来动态生成工具定义，而是接收不可变、已排序的工具定义快照。这样稳定系统提示和工具描述都能形成可缓存、可测试的稳定前缀。

Anthropic provider 将稳定 system blocks 映射为 `[]anthropic.TextBlockParam`，并在最后一个稳定 system block 设置 ephemeral cache control；工具定义快照映射为 `[]anthropic.ToolUnionParam`，并在最后一个 tool definition 设置 ephemeral cache control。动态 system blocks 排在稳定 blocks 后，不设置 cache control。OpenAI provider 将稳定和动态 system blocks 合并为普通 system message，忽略 cache metadata。

Usage 事件扩展缓存字段，Agent Loop 内按单次用户请求累计展示 input/output/cache creation/cache read tokens。动态补充消息运行时生成，不写入 conversation history；为了调试，可在后续任务中保存非用户可见的 prompt metadata，但本章不新增完整历史重放机制。

## 核心数据结构

### prompt.Section

```go
type Section struct {
    Name     string
    Priority int
    Content  string
    Stable   bool
}
```

- `Name`：模块名，用于测试和调试。
- `Priority`：拼装优先级，固定模块按小到大排列。
- `Content`：模块文本。
- `Stable`：是否属于稳定提示块。

### prompt.RunMode

```go
type RunMode string

const (
    RunModeDefault RunMode = "default"
    RunModePlan    RunMode = "plan"
    RunModeDo      RunMode = "do"
)
```

`internal/prompt` 不导入 orchestrator，因此定义自己的轻量 mode 类型。Orchestrator 负责把自身 `RunMode` 转换为 `prompt.RunMode`。

### prompt.BuildRequest

```go
type BuildRequest struct {
    Mode      RunMode
    Iteration int
    ProjectRoot string
    OptionalStableSections []Section
}
```

- `Mode`：默认、Plan、Do。
- `Iteration`：Agent Loop provider 调用轮次，从 1 开始。
- `ProjectRoot`：动态环境信息，只进入补充消息。
- `OptionalStableSections`：本章保留插槽，调用方可传空。

### prompt.Block

```go
type Block struct {
    Name    string
    Content string
    Stable  bool
}
```

Prompt 包只输出通用 block，不引用 provider 类型。

### prompt.Bundle

```go
type Bundle struct {
    StableBlocks  []Block
    DynamicBlocks []Block
}
```

- `StableBlocks`：字节稳定的系统块，进入 Anthropic cache 边界。
- `DynamicBlocks`：运行时补充消息，不写入会话历史，不进入 cache 边界。

### provider.SystemBlock

```go
type SystemBlock struct {
    Name      string
    Content   string
    Cacheable bool
}
```

Provider 层的系统块 DTO。由 orchestrator 从 `prompt.Block` 转换而来。

### provider.ToolDefinition

```go
type ToolDefinition struct {
    Name        string
    Description string
    Schema      tool.Schema
}
```

Provider 层工具定义快照。它是不可变数据，不持有 `*tool.Registry`。Orchestrator 根据当前模式从 registry 生成并排序后传给 provider。

### provider.CachePolicy

```go
type CachePolicy struct {
    EnablePromptCache bool
}
```

本章只需要总开关。Anthropic 使用它设置 cache control；OpenAI 忽略该策略但保持请求成功。

### provider.ChatRequest

```go
type ChatRequest struct {
    StableSystem  []SystemBlock
    DynamicSystem []SystemBlock
    SystemPrompt  string
    Messages      []conversation.Message
    Thinking      config.ThinkingConfig
    Tools         []ToolDefinition
    Cache         CachePolicy
}
```

`SystemPrompt` 保留兼容旧测试和旧调用，但 orchestrator 新路径只写 `StableSystem`/`DynamicSystem`。Provider 内部用 `systemBlocks(req)` 兼容：如果结构化字段为空但 `SystemPrompt` 非空，则生成一个默认稳定 block。

### provider.Usage

```go
type Usage struct {
    InputTokens              int64
    OutputTokens             int64
    CacheCreationInputTokens int64
    CacheReadInputTokens     int64
}
```

Anthropic 从 `MessageDeltaUsage` 映射全部字段；OpenAI 只填 input/output，缓存字段保持 0。使用 `int64` 对齐 SDK token 字段。

### events.UsageDisplay / tui.Status

扩展相同四个字段，用于请求级累计展示：

```go
type UsageDisplay struct {
    InputTokens              int64
    OutputTokens             int64
    CacheCreationInputTokens int64
    CacheReadInputTokens     int64
}
```

Status 行展示为：`Tokens: X in / Y out | Cache: C create / R read`，只有缓存字段非零时显示 Cache 段。

## 模块设计

### internal/prompt

**职责：** 生成稳定系统提示和动态系统补充。

**依赖边界：** 只能依赖标准库，不能导入 provider、orchestrator、conversation、tool、resources、app、tui。

**文件：**

- `internal/prompt/section.go`：定义 `Section`、`RunMode`、`BuildRequest`、`Block`、`Bundle`。
- `internal/prompt/builder.go`：实现稳定模块排序、空行拼装和可选模块追加。
- `internal/prompt/sections.go`：实现七个固定模块文本。
- `internal/prompt/dynamic.go`：实现特殊标签动态补充和轮次注入策略。
- `internal/prompt/prompt_test.go`：稳定性、模块顺序、动态隔离和转义测试。

**接口：**

```go
func Build(req BuildRequest) Bundle
func StableSections(optional []Section) []Section
func DynamicBlocks(req BuildRequest) []Block
```

**关键规则：**

- 固定模块顺序：身份、系统约束、任务模式、动作执行、工具使用、语气风格、文本输出。
- 每个固定模块包含 spec 中要求的核心句义。
- 稳定模块之间空行分隔。
- 动态补充使用 `<system-reminder>` 标签，内容只来自运行时可信状态。
- `ProjectRoot` 等动态值必须经过 XML/文本转义或结构化格式化。
- 轮次策略：iteration 1 输出完整模式说明；`iteration % 3 == 0` 输出关键约束重复；其他轮次输出精简模式提醒。

### internal/resources

**职责：** 保留 UI labels 和旧 prompt 兼容。

**改动：**

- 保留 `SystemPrompt()`，但内容改为调用 `prompt.Build` 的默认稳定文本或标记为 legacy fallback。
- 不让 resources 负责 prompt 组装，避免 labels provider 继续膨胀。

### internal/orchestrator

**职责：** 在每次 Agent Loop provider 调用前构造 prompt bundle 和工具定义快照。

**改动：**

- `stream(ctx, conv, mode, includeTools, iteration)` 接收 Agent Loop iteration。
- `runAgentLoop` 调用 `o.stream(ctx, conv, req.Mode, true, iteration)`。
- `finalReply` 等旧路径使用 iteration 1。
- 新增 `buildPromptBundle(mode RunMode, iteration int) prompt.Bundle`。
- 新增 `toolDefinitionsForMode(mode RunMode, includeTools bool) ([]provider.ToolDefinition, error)`。
- 删除 `systemPrompt(mode)` 的模式字符串拼接职责，改为动态 prompt block。

**动态环境：**

- `ProjectRoot` 从 executor 获取；没有 executor 时为空。
- 动态补充不调用 `conversation.Append*`，不落入普通历史。
- 本章不新增 turn metadata 存储；调试复现通过测试和事件观察完成。

### internal/provider

**职责：** 把结构化 prompt 和工具定义快照映射到不同 provider 的请求协议。

**改动：**

- 扩展 `ChatRequest`、新增 `SystemBlock`、`ToolDefinition`、`CachePolicy`、扩展 `Usage`。
- 增加 helper：`systemBlocks(req ChatRequest) []SystemBlock`。
- 增加 helper：`toolDefinitionsFromRegistry(registry *tool.Registry) []ToolDefinition` 放在 orchestrator 或 provider-adjacent helper 中，但 provider request 不持有 registry。

**OpenAI：**

- 把 stable/dynamic blocks 按顺序合并为 system message 内容。
- 不发送 Anthropic cache metadata。
- 继续使用 `stream_options.include_usage=true`。

**Anthropic：**

- 把 stable/dynamic blocks 转为 `[]anthropic.TextBlockParam`。
- 稳定 blocks 在前，动态 blocks 在后。
- 启用 cache 时，最后一个稳定 block 设置 `CacheControl: anthropic.NewCacheControlEphemeralParam()`。
- 工具定义快照映射为 `anthropic.ToolParam`；启用 cache 时，最后一个工具设置 cache control。
- 工具为空时，只在稳定 system block 设置 cache control。
- 动态 system blocks 不设置 cache control。

### internal/provider/anthropic.go

**职责：** Anthropic prompt cache 和 cache usage。

**设计：**

- `toAnthropicSystemBlocks(req ChatRequest) []anthropic.TextBlockParam`。
- `toAnthropicTools(definitions []ToolDefinition, cache CachePolicy) []anthropic.ToolUnionParam`。
- `MessageDeltaEvent.Usage` 映射 `InputTokens`、`OutputTokens`、`CacheCreationInputTokens`、`CacheReadInputTokens`。
- 如 SDK 需要 beta header 才启用 prompt caching，则在实现阶段用 SDK option 增加对应 header；若当前 API 已默认支持，则不额外加 header。任务阶段必须用测试或 SDK 类型验证。

### internal/provider/openai.go

**职责：** 结构化 prompt 兼容退化。

**设计：**

- `toOpenAIMessages` 先插入一个 system message，内容为 stable blocks 和 dynamic blocks 依序空行拼接。
- 保持 `stream_options.include_usage=true`。
- 缓存字段保持 0。

### internal/tool

**职责：** 工具描述强化和稳定定义快照。

**改动：**

- 在各工具 `Description()` 中补充与该工具相关的关键规则。
- 新增或调整 registry helper 生成按工具名排序的 `[]provider.ToolDefinition` 快照。
- Schema `Properties` 是 map；Go JSON marshal 会稳定排序 string keys，但测试仍需覆盖连续生成请求体一致性。

**决策：** 本章采用按工具名排序输出 provider definitions，而不是依赖注册顺序。这样插件或未来扩展注册顺序变化时更稳定。

### internal/events / internal/tui / internal/app

**职责：** 展示请求级累计 usage/cache 指标。

**改动：**

- `events.UsageDisplay` 增加 cache creation/read 字段并改用 `int64`。
- `app.Update` 在单次用户请求内累计四类 token，新请求提交时清零。
- `tui.Status` 展示普通 token 和 cache token；cache 字段全为 0 时不显示 Cache 段。

## 模块交互

1. 用户提交请求。
2. `orchestrator.Send` 解析 `RunRequest`，只保存清理后的用户文本。
3. `runAgentLoop` 第 N 次 iteration 调用 `stream`。
4. `stream` 构造 `prompt.BuildRequest{Mode, Iteration, ProjectRoot}`。
5. `prompt.Build` 返回 `StableBlocks` 和 `DynamicBlocks`。
6. `stream` 将 prompt blocks 转为 `provider.SystemBlock`，将当前 registry 转为按名排序的 `[]provider.ToolDefinition` 快照。
7. `stream` 构造 `provider.ChatRequest`，包含 system blocks、history、tool definitions、thinking、cache policy。
8. Provider 映射请求：
   - Anthropic：tools 和 stable system blocks 设置 cache boundary，dynamic system blocks 排后面。
   - OpenAI：system blocks 合并为 system message。
9. Provider 流式返回 text/thinking/tool/usage。
10. `collectProviderStream` 转发 usage event。
11. app 累计 usage，TUI 展示普通 token 和 cache token。
12. 动态补充不会进入 conversation history。

## 文件组织

```text
internal/
├── prompt/
│   ├── section.go      — Section、RunMode、BuildRequest、Block、Bundle
│   ├── builder.go      — 稳定模块排序与拼装
│   ├── sections.go     — 七个固定模块文本
│   ├── dynamic.go      — 特殊标签动态补充与轮次策略
│   └── prompt_test.go  — 稳定性、模块顺序、动态注入测试
├── provider/
│   ├── provider.go     — ChatRequest/SystemBlock/ToolDefinition/CachePolicy/Usage 扩展
│   ├── anthropic.go    — cache_control 与 cache usage 映射
│   ├── openai.go       — system blocks 兼容映射
│   └── tool_parse_test.go
├── orchestrator/
│   ├── chat.go         — stream 构造结构化 prompt request 和工具快照
│   ├── agent_loop.go   — iteration 传递
│   └── chat_test.go    — Plan Mode 注入和历史不污染测试
├── tool/
│   ├── registry.go     — 稳定排序 definitions 输入
│   └── tool_test.go
├── events/events.go    — UsageDisplay cache 字段
├── app/update.go       — usage 累计
└── tui/status.go       — cache usage 展示
```

文档：

```text
docs/system-prompt/
├── spec.md
├── plan.md
├── task.md
└── checklist.md
```

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| prompt 构建位置 | 新增纯数据 `internal/prompt` 叶子包 | 避免 import cycle，便于稳定输出测试 |
| provider 请求结构 | `StableSystem` + `DynamicSystem` + `Tools []ToolDefinition` + 兼容 `SystemPrompt` | 明确缓存边界，避免 provider 持有 registry |
| Anthropic cache 边界 | 最后一个稳定 system block 和最后一个 tool definition 设置 ephemeral cache control | SDK 支持 `TextBlockParam.CacheControl` 和 `ToolParam.CacheControl`，符合稳定前缀缓存目标 |
| OpenAI 缓存 | 只做结构兼容和普通 usage | 本章缓存验证 Anthropic 优先，避免扩范围 |
| 动态补充保存 | 不进入 conversation history | 防止污染历史、误当用户输入、破坏缓存 |
| prompt metadata | 本章不新增持久化 metadata | 控制范围；如需完整重放留给后续历史/观测章节 |
| 轮次计数 | 按 Agent Loop iteration/provider 调用计数 | 一次用户请求会多次调用模型，约束必须覆盖续轮 |
| 工具定义传递 | Orchestrator 生成不可变快照传给 provider | provider 与 tool registry 解耦，稳定 cache 前缀 |
| 工具顺序 | provider tool definitions 按工具名排序 | 比注册顺序更抗未来扩展变化，减少 cache miss |
| Plan Mode 工具集合 | 继续使用只读 registry + 执行层拒绝 | 保持现有安全兜底；缓存按模式分前缀，可接受 |
| usage 聚合 | app 层请求级累计 | 用户关心一次请求整体成本，provider 事件保持单次调用粒度 |
| thinking 修正 | 不纳入本章 | 审阅发现 thinking budget 可能需修，但不属于 system prompt/cache 章节核心，后续单独处理 |

## 测试策略

### Prompt 单元测试

- 七个固定模块顺序稳定。
- 固定模块包含 spec 要求的核心句义。
- 可选稳定模块按 priority 追加，空可选模块无占位符。
- 连续两次 `prompt.Build` 输出字节一致。
- `ProjectRoot` 等动态字段只出现在 dynamic blocks。
- 动态补充包含 `<system-reminder>` 标签且转义动态值。
- iteration 1 / 2 / 3 分别输出完整 / 精简 / 重复关键约束。

### Provider 请求形状测试

- Anthropic 请求体中 stable system blocks 在 dynamic blocks 前。
- 最后一个 stable system block 有 cache_control。
- 最后一个 tool definition 有 cache_control。
- Dynamic system blocks 没有 cache_control。
- OpenAI 请求不包含 Anthropic 专属字段，且 system message 包含 stable + dynamic 内容。
- 同一工具集合连续构造请求体顺序一致。

### Orchestrator 集成测试

- 同一 `/plan` 请求的多次 provider 调用都收到 Plan Mode dynamic block。
- iteration 1 / 3 注入内容不同且符合策略。
- 动态补充不进入 `conversation.ContextMessages`，也不保存为 user message。
- Plan Mode 下 Write/Edit/Bash 不暴露或被执行层拒绝。

### Usage/UI 测试

- Anthropic cache creation/read 字段解析为 `provider.Usage`。
- `collectProviderStream` 转发四类 usage 字段。
- app 在同一用户请求内累计 usage，新请求清零。
- TUI cache 字段为 0 时不显示 Cache 段，非 0 时显示 create/read。

### 人工对比清单

后续 checklist 阶段固定表格字段：场景、输入、模式、fixture 文件、期望工具序列、禁止动作、期望最终输出特征、实际工具序列、是否触发确认、usage/cache 指标、通过/失败原因。

## Spec 覆盖映射

- F1-F4：`internal/prompt` 固定模块、可选模块、稳定拼装测试覆盖。
- F5-F7：`provider.ChatRequest` 分层、Anthropic cache control 测试覆盖。
- F8-F10：`prompt.DynamicBlocks`、orchestrator 不保存动态消息、转义/可信来源规则覆盖。
- F11-F13：orchestrator iteration 注入和 Plan/Do mode 动态补充测试覆盖。
- F14-F15：工具描述强化和 definitions 快照排序测试覆盖。
- F16-F17：provider usage、events、app、tui 测试覆盖。
- F18：后续 checklist 人工对比场景覆盖。
