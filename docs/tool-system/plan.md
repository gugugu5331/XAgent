# XAgent Tool System Plan

## 架构概览

工具系统在现有 XAgent 架构上新增工具定义、工具注册、工具执行、权限确认、Provider 工具协议适配、TUI 工具行展示六个部分，并扩展流式编排层支持“一次工具调用 → 工具结果回灌 → 最终回复”的流程。

**工具定义层**负责定义统一 Tool 接口、工具元信息、参数 Schema、工具调用请求、工具执行结果和错误结构。六个内置工具都通过这个统一接口暴露能力。

**工具注册层**负责集中登记内置工具，按名称查找工具，并把工具元信息转换为 Anthropic Claude 和 OpenAI 各自需要的工具定义格式。Provider 不直接依赖具体工具实现，只消费注册中心转换后的工具定义。

**工具执行层**负责根据统一工具调用请求找到工具、校验参数、检查路径范围、执行工具、处理超时，并把成功、失败、拒绝、超时都包装成结构化工具结果。它还负责限制输出大小，避免过大结果进入模型上下文。

**权限确认层**负责识别危险工具调用。读文件、找文件、搜代码默认可执行；写文件、改文件、执行命令在执行前生成确认请求，由 TUI 展示工具名称、关键参数和潜在影响。用户允许后执行，用户拒绝后返回结构化拒绝结果。

**Provider 工具协议适配层**扩展 Anthropic Claude Provider 和 OpenAI Provider。两个 Provider 在请求时携带工具定义，在流式响应中识别工具调用事件并拼接 JSON 参数碎片，再统一输出上层可消费的工具调用事件。普通文本流式响应保持原有行为。

**流式编排层**从“单次模型流式响应”扩展为“一次工具调用编排”。用户提交消息后，编排层发起带工具定义的模型请求；如果模型返回文本，则按现有方式流式展示；如果模型返回工具调用，则暂停文本流，向 TUI 发送工具行事件，必要时等待用户确认，执行工具，把结果追加到会话历史，再发起一次最终回复请求。若最终回复再次请求工具，系统不继续执行，只提示本阶段不支持自动 Agent Loop。

**TUI 工具展示层**在消息视图中新增 Claude Code 风格工具行。工具开始时展示 `● Tool(args)`，执行中显示进行中状态，完成后展示简短摘要，失败或拒绝时用可区分样式展示错误或拒绝摘要。普通 user/assistant 消息渲染不退化。

**会话层**扩展消息类型，保存工具调用和工具结果，使模型在回灌后的最终回复中能看到工具输出。会话持久化继续使用本地 JSON，过大的工具结果会截断并标记。

## 核心数据结构

### Tool
统一工具接口。

方法：
- `Name() string`：返回工具名称。
- `Description() string`：返回工具说明。
- `Schema() ToolSchema`：返回参数 Schema。
- `Risk() ToolRisk`：返回工具风险级别。
- `Execute(ctx context.Context, input ToolInput) ToolResult`：执行工具并返回结构化结果。

### ToolSchema
表示工具参数 Schema。

字段：
- `Type string`：Schema 根类型，首版固定为 `object`。
- `Properties map[string]ToolSchemaProperty`：参数字段定义。
- `Required []string`：必填字段列表。

### ToolSchemaProperty
表示单个参数字段。

字段：
- `Type string`：字段类型。
- `Description string`：字段说明。
- `Enum []string`：可选枚举值。

### ToolRisk
表示工具风险级别。

取值：
- `safe`：默认可执行，例如读文件、找文件、搜代码。
- `dangerous`：执行前需要用户确认，例如写文件、改文件、执行命令。

### ToolInput
表示一次工具调用输入。

字段：
- `Name string`：工具名称。
- `CallID string`：模型侧工具调用 ID。
- `RawArguments string`：拼接完成后的 JSON 参数。
- `Arguments map[string]any`：解析后的参数对象。

### ToolCall
表示 Provider 解析出来的统一工具调用。

字段：
- `ID string`：工具调用 ID。
- `Name string`：工具名称。
- `ArgumentsJSON string`：完整 JSON 参数字符串。

### ToolResult
表示工具执行后的结构化结果。

字段：
- `CallID string`：对应工具调用 ID。
- `Name string`：工具名称。
- `Status ToolResultStatus`：执行状态。
- `Summary string`：给 TUI 展示的简短摘要。
- `Content string`：给模型消费的详细结果文本。
- `Data map[string]any`：结构化数据，例如路径、字节数、退出码、匹配数量。
- `Error *ToolError`：失败、拒绝或超时时的错误信息。
- `Truncated bool`：结果是否被截断。

### ToolResultStatus
表示工具结果状态。

取值：
- `success`：执行成功。
- `error`：执行失败。
- `denied`：用户拒绝执行。
- `timeout`：执行超时。

### ToolError
表示结构化错误。

字段：
- `Code string`：稳定错误码，例如 `path_outside_project`、`not_found`、`multiple_matches`、`timeout`。
- `Message string`：面向模型和用户可理解的错误说明。
- `Recoverable bool`：模型是否可以通过调整参数重试。

### ToolRegistry
工具注册中心接口。

方法：
- `Register(tool Tool) error`：登记工具。
- `Get(name string) (Tool, bool)`：按名称查找工具。
- `List() []Tool`：列出工具。
- `AnthropicDefinitions() []AnthropicToolDefinition`：转换为 Claude 工具定义。
- `OpenAIDefinitions() []OpenAIToolDefinition`：转换为 OpenAI 工具定义。

### ToolExecutor
工具执行器。

字段：
- `Registry ToolRegistry`：工具注册中心。
- `ProjectRoot string`：项目根目录。
- `Timeout time.Duration`：默认工具超时。
- `MaxOutputBytes int`：工具结果最大返回大小。

方法：
- `Execute(ctx context.Context, call ToolCall) ToolResult`：执行安全工具。
- `BuildDeniedResult(call ToolCall) ToolResult`：构造用户拒绝结果。
- `NeedsConfirmation(call ToolCall) bool`：判断是否需要用户确认。

### ToolDisplay
表示 TUI 中的一条 Claude Code 风格工具行。

字段：
- `CallID string`：工具调用 ID。
- `Name string`：工具名称。
- `Args string`：用于展示的关键参数摘要。
- `Status ToolDisplayStatus`：展示状态。
- `Summary string`：完成、失败或拒绝后的摘要。

### ToolDisplayStatus
表示工具行展示状态。

取值：
- `pending`：模型刚请求工具。
- `waiting_confirmation`：等待用户确认。
- `running`：工具执行中。
- `success`：工具执行成功。
- `error`：工具执行失败。
- `denied`：用户拒绝执行。

### ToolConfirmationRequest
表示危险工具确认请求。

字段：
- `Call ToolCall`：待执行工具调用。
- `Display ToolDisplay`：TUI 展示用工具行。
- `RiskSummary string`：风险说明。

## 模块设计

### 工具定义模块
**职责：** 定义 Tool 接口、工具 Schema、工具输入输出、风险级别、错误结构。  
**对外接口：**
- `Tool`
- `ToolSchema`
- `ToolInput`
- `ToolCall`
- `ToolResult`

**依赖：** context 标准库。

设计要点：
- 所有工具必须返回结构化 `ToolResult`，不能 panic 或直接中断会话。
- 工具错误使用稳定错误码，便于模型理解和重试。
- 工具风险级别由工具自身声明。

### 工具注册模块
**职责：** 集中登记六个内置工具，并提供 Provider 工具定义转换。  
**对外接口：**
- `NewRegistry(projectRoot string) *Registry`
- `Register(tool Tool) error`
- `Get(name string) (Tool, bool)`
- `AnthropicDefinitions() []AnthropicToolDefinition`
- `OpenAIDefinitions() []OpenAIToolDefinition`

**依赖：** 工具定义模块、六个内置工具模块。

设计要点：
- 工具名称使用稳定英文名：`Read`、`Write`、`Edit`、`Bash`、`Glob`、`Grep`。
- 注册重复工具名时返回错误。
- 转换 Provider 定义时复用同一份 `ToolSchema`。

### 内置工具模块
**职责：** 实现六个核心工具。  
**对外接口：**
- `NewReadTool(projectRoot string) Tool`
- `NewWriteTool(projectRoot string) Tool`
- `NewEditTool(projectRoot string) Tool`
- `NewBashTool(projectRoot string) Tool`
- `NewGlobTool(projectRoot string) Tool`
- `NewGrepTool(projectRoot string) Tool`

**依赖：** 工具定义模块、路径安全辅助、文件系统、os/exec、filepath、正则库。

工具行为：
- `Read`：输入 `path`，读取项目内文本文件，返回内容、字节数、是否截断。
- `Write`：输入 `path`、`content`，写入项目内文件，返回写入路径和字节数。
- `Edit`：输入 `path`、`old_text`、`new_text`，要求 `old_text` 在文件中唯一匹配，成功后替换。
- `Bash`：输入 `command`，在项目根目录执行，返回 stdout、stderr、exit_code、timed_out。
- `Glob`：输入 `pattern`，返回项目内匹配文件列表。
- `Grep`：输入 `pattern`、可选 `path`、可选 `regex`，返回匹配文件、行号和摘要。

### 路径安全模块
**职责：** 规范化文件路径并保证访问不越过项目根目录。  
**对外接口：**
- `ResolveProjectPath(projectRoot string, requestedPath string) (string, error)`
- `RelativeToRoot(projectRoot string, absolutePath string) string`

**依赖：** filepath 标准库。

设计要点：
- 拒绝空路径。
- 清理 `..`、符号路径和绝对路径。
- 解析后的绝对路径必须仍在项目根目录内。
- 越界返回 `path_outside_project` 结构化错误。

### 工具执行模块
**职责：** 执行工具调用、处理超时、限制输出、统一错误包装。  
**对外接口：**
- `NewExecutor(registry ToolRegistry, projectRoot string, timeout time.Duration, maxOutputBytes int) *Executor`
- `NeedsConfirmation(call ToolCall) bool`
- `Execute(ctx context.Context, call ToolCall) ToolResult`
- `Denied(call ToolCall) ToolResult`

**依赖：** 工具注册模块、工具定义模块、context、time。

设计要点：
- 执行前解析 JSON 参数。
- 找不到工具返回 `tool_not_found`。
- 参数 JSON 无效返回 `invalid_arguments`。
- 执行超时返回 `timeout`。
- 输出超过限制时截断并设置 `Truncated`。
- 工具自身失败统一包装为 `ToolResult`。

### Provider 工具适配模块
**职责：** 扩展 Provider 请求和流式事件，支持工具定义和工具调用事件。  
**对外接口变化：**
- `ChatRequest` 增加 `Tools []ToolDefinition`。
- `StreamEvent` 增加工具调用相关类型。
- `StreamEvent` 可携带 `ToolCall`。
- Provider 增加支持工具结果回灌的请求构造能力。

**依赖：** Provider 模块、工具定义模块。

Anthropic 适配：
- 请求中加入 Claude tool definitions。
- 流式解析 tool_use content block。
- 拼接 input_json_delta。
- 输出统一 `tool_call` 事件。
- 工具结果回灌时构造 `tool_result` 消息。

OpenAI 适配：
- 请求中加入 OpenAI tools。
- 流式解析 `tool_calls` delta。
- 按 index/id/name 拼接 arguments JSON。
- 输出统一 `tool_call` 事件。
- 工具结果回灌时构造 `tool` role 消息。

### 流式编排模块
**职责：** 实现一次工具调用编排流程。  
**对外接口变化：**
- `Orchestrator` 增加 ToolRegistry、ToolExecutor。
- `Send` 在请求中附带工具定义。
- `Send` 能输出工具展示事件、确认事件、工具结果事件和最终回复事件。

处理流程：
1. 追加用户消息。
2. 发起带工具定义的模型流式请求。
3. 如果模型返回普通文本，保持现有流式展示。
4. 如果模型返回工具调用，向 TUI 发送工具行 pending 事件。
5. 若工具危险，向 TUI 发送确认请求并等待用户选择。
6. 用户允许后执行工具；用户拒绝则构造 denied 结果。
7. 将工具结果追加到会话历史。
8. 发起一次最终回复请求。
9. 最终回复文本流式展示。
10. 如果最终回复再次请求工具，不执行，返回说明本阶段不支持自动 Agent Loop。

### TUI 工具展示模块
**职责：** 在消息视图中展示 Claude Code 风格工具行，并处理危险工具确认。  
**对外接口变化：**
- 消息视图支持追加和更新 `ToolDisplay`。
- 应用事件支持 `tool_pending`、`tool_waiting_confirmation`、`tool_running`、`tool_success`、`tool_error`、`tool_denied`。
- TUI 应用状态支持等待工具确认。

展示规则：
- 工具开始：`● Read(path)`。
- 工具运行：保留工具行并显示进行中状态。
- 工具成功：追加摘要，例如 `Read 1.2KB`、`Edited file.go`。
- 工具失败：用可区分样式展示错误摘要。
- 工具拒绝：展示用户已拒绝。
- 危险工具确认：展示工具名、关键参数和允许/拒绝操作提示。

### 会话模块
**职责：** 扩展会话消息以保存工具调用和工具结果。  
**对外接口变化：**
- 增加工具调用消息角色或消息类型。
- 增加工具结果消息保存逻辑。
- `ContextMessages` 能把工具调用和结果转换为 Provider 所需上下文。

设计要点：
- 模型需要看到工具结果全文或截断后的内容。
- TUI 需要能从历史会话恢复工具行。
- thinking 消息仍不作为普通上下文传给 OpenAI。

## 模块交互

### 启动流程
1. 命令入口加载配置、Provider、会话存储和资源 Provider。
2. 命令入口确定项目根目录。
3. 工具注册模块创建注册中心。
4. 注册中心登记六个内置工具：Read、Write、Edit、Bash、Glob、Grep。
5. 工具执行模块创建 Executor，绑定注册中心、项目根目录、默认超时和输出大小限制。
6. 命令入口把注册中心和 Executor 注入流式编排层。
7. TUI 应用启动。

### 普通无工具对话流程
1. 用户在 TUI 输入问题并提交。
2. 流式编排层追加 user 消息。
3. 流式编排层发起带工具定义的模型请求。
4. Provider 返回文本增量事件。
5. TUI 按现有方式流式渲染 assistant 回复。
6. 响应完成后保存 user 和 assistant 消息。
7. 该流程不显示工具行。

### 安全工具调用流程
1. 用户提交请求。
2. Provider 在流式响应中解析到工具调用。
3. Provider 输出统一 `tool_call` 事件。
4. 流式编排层停止等待更多 assistant 文本，构造 `ToolCall`。
5. TUI 显示工具行，例如 `● Read(path)`，状态为 pending。
6. Executor 判断该工具不需要确认。
7. TUI 更新工具行状态为 running。
8. Executor 执行工具。
9. Executor 返回结构化 `ToolResult`。
10. TUI 更新工具行状态为 success 或 error，并显示摘要。
11. 会话层追加工具调用和工具结果。
12. 流式编排层发起一次最终回复请求。
13. Provider 返回最终文本增量。
14. TUI 流式渲染最终 assistant 回复。
15. 响应完成后保存会话，本轮结束。

### 危险工具确认流程
1. 用户提交请求。
2. Provider 解析到 Write、Edit 或 Bash 工具调用。
3. TUI 显示工具行，例如 `● Bash(go test ./...)`。
4. Executor 判断该工具需要确认。
5. TUI 展示确认提示，包含工具名、关键参数和允许/拒绝操作。
6. 用户选择允许时，TUI 向流式编排层返回允许结果。
7. 流式编排层执行工具，并按安全工具调用流程继续。
8. 用户选择拒绝时，Executor 构造 denied 工具结果。
9. TUI 更新工具行为 denied。
10. 会话层追加拒绝结果。
11. 流式编排层发起一次最终回复请求，让模型基于拒绝结果解释或调整。
12. 响应完成后保存会话，本轮结束。

### 工具失败流程
1. 工具执行时出现文件不存在、路径越界、命令非零退出、命令超时、搜索无结果、参数错误或改文件匹配数不对。
2. 工具返回结构化 `ToolResult`，状态为 error 或 timeout。
3. TUI 工具行显示失败状态和简短错误摘要。
4. 会话层追加失败工具结果。
5. 流式编排层把失败结果回灌给模型。
6. 模型生成一次最终回复，说明失败原因或给出调整建议。
7. 程序不崩溃，会话不中断，用户可以继续输入或退出。

### 最终回复再次请求工具流程
1. 工具结果回灌后，流式编排层发起最终回复请求。
2. 如果 Provider 再次返回工具调用事件，流式编排层不执行第二轮工具。
3. TUI 显示提示：本阶段不支持自动 Agent Loop。
4. 会话保存已完成的工具调用和结果。
5. 用户可以在下一轮手动继续提问。

### 历史会话恢复流程
1. 用户从历史会话列表选择包含工具调用的会话。
2. 会话层加载 user、assistant、tool call、tool result 等历史消息。
3. TUI 恢复普通消息和 Claude Code 风格工具行。
4. 用户继续提问时，Provider 上下文包含必要的工具结果历史。

## 文件组织

```text
xagent/
├── internal/
│   ├── tool/
│   │   ├── tool.go              — Tool 接口、ToolSchema、ToolInput、ToolResult、ToolError
│   │   ├── registry.go          — 工具注册中心、Provider 工具定义转换入口
│   │   ├── executor.go          — 工具执行、超时、拒绝结果、输出截断
│   │   ├── path.go              — 项目根目录路径规范化和越界检查
│   │   ├── schema.go            — Schema 辅助构造
│   │   ├── read.go              — Read 工具
│   │   ├── write.go             — Write 工具
│   │   ├── edit.go              — Edit 工具，唯一匹配替换
│   │   ├── bash.go              — Bash 工具
│   │   ├── glob.go              — Glob 工具
│   │   └── grep.go              — Grep 工具
│   ├── provider/
│   │   ├── provider.go          — ChatRequest/StreamEvent 增加工具定义和 ToolCall
│   │   ├── anthropic.go         — Claude 工具定义、tool_use 流式解析、tool_result 回灌
│   │   └── openai.go            — OpenAI tools、tool_calls delta 拼接、tool role 回灌
│   ├── orchestrator/
│   │   └── chat.go              — 一次工具调用编排和最终回复请求
│   ├── conversation/
│   │   ├── message.go           — 增加工具调用/工具结果消息角色或类型
│   │   └── conversation.go      — 追加工具调用/工具结果，ContextMessages 转换
│   ├── events/
│   │   └── events.go            — 增加工具行、确认、工具结果相关事件
│   ├── tui/
│   │   ├── messages.go          — 工具行渲染、状态更新、历史恢复
│   │   └── status.go            — 工具确认/错误状态展示
│   └── app/
│       ├── deps.go              — 注入 ToolRegistry、ToolExecutor
│       ├── app.go               — 增加等待工具确认状态
│       └── update.go            — 处理工具事件、用户允许/拒绝
├── cmd/
│   └── xagent/
│       └── main.go              — 初始化工具注册中心和 Executor
└── docs/
    └── tool-system/
        ├── spec.md
        ├── plan.md
        ├── task.md
        └── checklist.md
```

说明：
- 新增 `internal/tool`，集中放工具系统核心能力和六个内置工具。
- `provider` 只处理协议差异，不直接执行工具。
- `orchestrator` 负责把 Provider 工具调用、权限确认、工具执行和最终回复串起来。
- `tui` 和 `app` 只处理工具行展示与用户确认，不直接实现工具逻辑。
- 本章文档放在 `docs/tool-system/`，不覆盖首版根目录文档。

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 工具接口 | 自定义统一 `Tool` 接口 | 满足六个内置工具统一执行，并兼容 Anthropic/OpenAI 工具定义转换。 |
| 工具名称 | `Read`、`Write`、`Edit`、`Bash`、`Glob`、`Grep` | 与 Claude Code 工具行风格接近，展示时也直观。 |
| 工具执行范围 | 限制在项目根目录内 | 用户已确认；降低误读写系统文件风险。 |
| 危险工具权限 | Write/Edit/Bash 执行前 TUI 确认 | 用户已确认；避免模型未经确认写文件、改文件或执行命令。 |
| 工具结果格式 | 结构化 `ToolResult` + TUI 摘要 | 同时满足模型可调整和用户可观察。 |
| 错误处理 | 错误作为工具结果回灌 | 符合需求：失败不崩溃，会话不中断，模型可基于错误调整。 |
| Edit 语义 | 原文唯一匹配替换 | 用户明确要求；避免模糊替换导致误改。 |
| 输出限制 | 工具结果截断并标记 | 避免大文件或大搜索结果撑爆上下文。 |
| Provider 适配 | Provider 解析工具调用，Orchestrator 执行工具 | 保持协议解析和本地副作用执行分离。 |
| 工具调用轮次 | 只做一次工具结果回灌和最终回复 | 用户明确要求本章不做 Agent Loop。 |
| TUI 展示 | Claude Code 风格工具行 | 用户明确要求；提升可观察性。 |
| 历史保存 | 保存工具调用和工具结果 | 支持历史会话恢复和后续上下文使用。 |
