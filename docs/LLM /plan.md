# XAgent Plan

## 架构概览

XAgent 使用 Go 实现，整体分为命令入口、配置、提示词/资源、TUI 框架、TUI 应用、会话、Provider、流式编排八个核心部分。

**命令入口层**负责启动程序、加载配置、初始化本地数据目录、创建 Provider、加载历史会话，并启动 TUI 应用。入口层不直接处理模型协议细节，也不直接渲染界面，只负责把依赖组装起来。

**配置层**负责从 YAML 文件读取模型供应商配置。配置包含 protocol、model、base_url、api_key 四个核心字段，并补充 Claude extended thinking 和 thinking 显示相关配置。配置层负责在模型请求前发现缺失或无效字段，并返回面向用户可理解的错误。

**提示词/资源层**负责管理 XAgent 首版对话所需的系统提示词、默认提示文本、界面文案和可复用资源。它向流式编排层提供本轮请求需要携带的系统提示词，向 TUI 应用层提供固定文案，避免这些内容散落在 Provider 或界面渲染代码里。

**TUI 框架层**负责封装底层终端框架能力，包括程序生命周期、键盘事件、文本输入、列表选择、视口滚动和布局渲染。它提供通用 UI 基础组件，不包含 XAgent 业务规则。

**TUI 应用层**负责 XAgent 的具体交互体验，包括会话列表、历史会话选择、消息展示、输入区域、提交操作、流式渲染、错误反馈、响应耗时展示和退出操作。TUI 应用层只消费统一的对话事件，不关心底层使用 Anthropic Claude 还是 OpenAI。

**会话层**负责维护当前会话消息、把多轮上下文提供给模型请求、在模型回复完成后追加消息，并把会话历史保存到本地文件。程序重启后，会话层负责读取已有历史并提供给 TUI 应用层选择。

**Provider 层**定义统一的大模型流式接口。Anthropic Claude Provider 和 OpenAI Provider 分别实现各自协议，把底层 SSE 事件转换成统一的流式事件，包括文本增量、thinking 增量、完成事件和错误事件。

**流式编排层**连接 TUI 应用、提示词/资源、会话和 Provider。用户提交输入后，它记录开始时间，构造包含系统提示词和会话上下文的请求，调用 Provider 流式接口，把事件转发给 TUI 应用层渲染，并在完成后记录响应耗时、更新会话历史和触发保存。

## 核心数据结构

### AppConfig
表示 XAgent 的完整运行配置。

字段：
- `LLM LLMConfig`：大模型供应商配置。
- `UI UIConfig`：界面行为配置。
- `Storage StorageConfig`：本地会话存储配置。

### LLMConfig
表示模型后端配置。

字段：
- `Protocol string`：协议类型，允许值为 `anthropic` 或 `openai`。
- `Model string`：模型名称。
- `BaseURL string`：API 请求地址。
- `APIKey string`：认证密钥，来自 YAML 明文字段。
- `Thinking ThinkingConfig`：Claude extended thinking 配置。

### ThinkingConfig
表示 Claude extended thinking 相关配置。

字段：
- `Enabled bool`：是否向 Claude 请求 extended thinking。
- `Show bool`：是否在 TUI 中显示 thinking 内容。
- `BudgetTokens int`：thinking token 预算。

### UIConfig
表示界面行为配置。

字段：
- `ShowResponseTimer bool`：是否显示响应耗时。
- `StartMode string`：启动模式，允许值为 `list` 或 `new`；`list` 表示启动后先进入历史会话列表，`new` 表示直接创建新会话。

### StorageConfig
表示本地存储配置。

字段：
- `DataDir string`：会话历史保存目录。

### Message
表示一条会话消息。

字段：
- `Role MessageRole`：消息角色。
- `Content string`：消息正文。
- `CreatedAt time.Time`：消息创建时间。

### MessageRole
表示消息角色。

取值：
- `user`：用户消息。
- `assistant`：模型最终回复。
- `thinking`：Claude thinking 内容，仅用于本地展示和保存，不作为普通助手回复发送给 OpenAI。

### Conversation
表示一个可继续的会话。

字段：
- `ID string`：会话唯一标识。
- `Title string`：会话标题，默认从第一条用户消息生成。
- `Messages []Message`：会话消息列表。
- `CreatedAt time.Time`：会话创建时间。
- `UpdatedAt time.Time`：最近更新时间。

### ChatRequest
表示一次模型请求。

字段：
- `SystemPrompt string`：系统提示词。
- `Messages []Message`：发送给模型的上下文消息。
- `Thinking ThinkingConfig`：thinking 请求配置。

### StreamEvent
表示 Provider 输出给上层的统一流式事件。

字段：
- `Type StreamEventType`：事件类型。
- `Delta string`：本次增量文本。
- `Err error`：错误信息。
- `Usage *Usage`：可选的用量信息。

### StreamEventType
表示流式事件类型。

取值：
- `text_delta`：最终回复文本增量。
- `thinking_delta`：thinking 文本增量。
- `done`：响应完成。
- `error`：响应失败。

### Usage
表示模型返回的用量信息。

字段：
- `InputTokens int`：输入 token 数。
- `OutputTokens int`：输出 token 数。

### ChatResult
表示一次完整响应结果。

字段：
- `AssistantText string`：模型最终回复全文。
- `ThinkingText string`：thinking 全文。
- `Duration time.Duration`：本轮响应耗时。
- `Usage *Usage`：可选用量信息。

### Provider
统一模型后端接口。

方法：
- `StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)`：发起一次流式对话请求，返回统一流式事件通道。
- `Name() string`：返回 Provider 名称，用于状态显示和错误信息。

### ConversationStore
会话存储接口。

方法：
- `List(ctx context.Context) ([]Conversation, error)`：列出本地历史会话。
- `Load(ctx context.Context, id string) (*Conversation, error)`：加载指定会话。
- `Save(ctx context.Context, conversation *Conversation) error`：保存会话。
- `Create(ctx context.Context) (*Conversation, error)`：创建新会话。

### PromptProvider
提示词/资源接口。

方法：
- `SystemPrompt() string`：返回首版纯对话系统提示词。
- `UILabel(key string) string`：返回 TUI 固定文案。

## 模块设计

### 命令入口模块
**职责：** 启动 XAgent，组装运行依赖。  
**对外接口：** 提供 `main` 程序入口。  
**依赖：** 配置模块、提示词/资源模块、Provider 模块、会话模块、TUI 应用模块。

启动流程：
1. 加载 YAML 配置。
2. 初始化本地数据目录。
3. 根据 protocol 创建对应 Provider。
4. 创建会话存储。
5. 创建提示词/资源 Provider。
6. 启动 TUI 应用。

### 配置模块
**职责：** 读取、解析和校验 YAML 配置。  
**对外接口：**
- `Load(path string) (*AppConfig, error)`：读取配置。
- `Validate(config *AppConfig) error`：校验配置字段。

**依赖：** YAML 解析库。

校验规则：
- protocol 必须是 `anthropic` 或 `openai`。
- model、base_url、api_key 不能为空。
- thinking 仅对 Anthropic 生效；OpenAI 配置 thinking 时不发出 thinking 请求。
- thinking budget token 小于等于 0 时使用默认值。

### 提示词/资源模块
**职责：** 集中管理系统提示词和界面固定文案。  
**对外接口：**
- `SystemPrompt() string`：返回纯对话阶段的系统提示词。
- `UILabel(key string) string`：返回界面文案。

**依赖：** 无。

首版系统提示词要求：
- 明确 XAgent 当前是纯对话助手。
- 不声明具备 tool use、文件操作、代码编辑或命令执行能力。
- 回答应优先简洁、直接、可执行。
- 当用户要求执行当前阶段不支持的能力时，应说明当前版本尚不支持。

### TUI 框架模块
**职责：** 封装底层 TUI 框架的通用能力。  
**对外接口：**
- `Run(model tea.Model) error`：运行 TUI 程序。
- 通用输入框、列表、消息视图、状态栏渲染组件。

**依赖：** Bubble Tea、Bubbles、Lip Gloss。

组件：
- 输入框组件：处理输入、提交和清空。
- 列表组件：展示历史会话并支持选择。
- 消息视图组件：展示多轮消息和流式增量。
- 状态栏组件：展示 Provider、模型、响应耗时和错误状态。

### TUI 应用模块
**职责：** 实现 XAgent 的具体界面状态和交互逻辑。  
**对外接口：**
- `NewApp(deps AppDeps) tea.Model`：创建 TUI 应用模型。

**依赖：** TUI 框架模块、流式编排模块、会话模块、配置模块、提示词/资源模块。

界面状态：
- `session_list`：历史会话列表。
- `chat`：当前会话对话界面。
- `streaming`：正在接收和渲染模型响应。
- `error`：显示错误并允许继续操作。

主要交互：
- 启动后按配置进入历史列表或新会话。
- 在历史列表中选择会话或新建会话。
- 在对话界面输入用户文本。
- 用户提交非空文本后，界面立即把该用户文本作为一条 user 消息展示到消息区。
- 用户提交后清空输入框，并进入等待/流式响应状态。
- 提交空输入时忽略请求并保持输入状态。
- 流式事件到达时更新当前 assistant/thinking 展示块。
- 响应完成后显示耗时并恢复输入。
- 错误发生后显示反馈，并保留已提交的用户文本，允许用户继续输入、重试或退出。
- 用户触发退出时保存当前会话并结束程序。

### 会话模块
**职责：** 管理会话生命周期、用户文本、模型回复、上下文消息和本地持久化。  
**对外接口：**
- `Create(ctx context.Context) (*Conversation, error)`：创建新会话。
- `List(ctx context.Context) ([]Conversation, error)`：列出历史会话。
- `Load(ctx context.Context, id string) (*Conversation, error)`：加载历史会话。
- `Save(ctx context.Context, conversation *Conversation) error`：保存会话。
- `AppendUserMessage(conversation *Conversation, text string)`：追加用户提交的文本。
- `AppendAssistantMessage(conversation *Conversation, text string)`：追加助手最终回复。
- `AppendThinkingMessage(conversation *Conversation, text string)`：追加 thinking 内容。
- `ContextMessages(conversation *Conversation) []Message`：返回发送给模型的上下文消息。

**依赖：** 本地文件系统、JSON 编解码。

存储规则：
- 每个会话保存为一个 JSON 文件。
- 会话文件包含 ID、标题、创建时间、更新时间和消息列表。
- 用户每次提交的文本保存为 `user` 消息。
- 模型最终回复保存为 `assistant` 消息。
- 会话标题默认从第一条用户文本生成。
- 会话列表按更新时间倒序展示。
- 上下文消息包含历史 user 消息和 assistant 消息。
- thinking 消息可保存到本地，但不作为普通 assistant 消息发送给 OpenAI。

### Provider 模块
**职责：** 统一不同模型后端的流式对话能力。  
**对外接口：**
- `New(config LLMConfig) (Provider, error)`：根据 protocol 创建 Provider。
- `Provider.StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)`：发起流式对话。
- `Provider.Name() string`：返回 Provider 名称。

**依赖：** HTTP 客户端、SSE 解析。

Anthropic Provider：
- 使用 Claude Messages 风格协议。
- 根据配置添加 extended thinking 参数。
- 解析 Anthropic 流式事件。
- 把文本增量转换为 `text_delta`。
- 把 thinking 增量转换为 `thinking_delta`。
- 把完成事件转换为 `done`。
- 把错误事件转换为 `error`。

OpenAI Provider：
- 使用 OpenAI Chat Completions 或兼容流式协议。
- 发起 stream 请求。
- 解析 OpenAI SSE chunk。
- 把文本增量转换为 `text_delta`。
- 把完成事件转换为 `done`。
- 把错误事件转换为 `error`。
- 忽略 Claude thinking 配置。

### 流式编排模块
**职责：** 协调用户文本、上下文构造、Provider 流式调用、TUI 事件转发、计时和会话保存。  
**对外接口：**
- `Send(ctx context.Context, conversation *Conversation, userText string) (<-chan AppEvent, error)`：提交一次用户文本并返回应用事件流。

**依赖：** Provider 模块、会话模块、提示词/资源模块、配置模块。

处理流程：
1. 拒绝空用户文本。
2. 将用户文本追加为当前会话的 user 消息。
3. 向 TUI 应用层发送用户消息已提交事件，用于立即渲染用户文本。
4. 记录开始时间。
5. 构造包含系统提示词、上下文消息和 thinking 配置的请求。
6. 调用 Provider 流式接口。
7. 将 Provider 事件转换为 TUI 应用事件。
8. 累积 assistant 文本和 thinking 文本。
9. 完成后追加 assistant 消息和可选 thinking 消息。
10. 保存会话。
11. 发送包含响应耗时的完成事件。
12. 发生错误时发送错误事件；已提交的用户文本保留在当前会话中，用户可以继续输入、重试或退出。

## 模块交互

### 启动流程
1. 命令入口模块启动。
2. 配置模块读取并校验 YAML 配置。
3. 命令入口模块初始化本地数据目录。
4. Provider 模块根据 `protocol` 创建 Anthropic 或 OpenAI Provider。
5. 会话模块创建本地会话存储并读取历史会话索引。
6. 提示词/资源模块创建默认资源 Provider。
7. 命令入口模块把配置、Provider、会话存储、提示词/资源 Provider 注入 TUI 应用模块。
8. TUI 框架模块运行 TUI 应用。

### 新会话对话流程
1. 用户在 TUI 应用中选择新建会话。
2. TUI 应用调用会话模块创建空会话。
3. 用户在输入区域输入文本并提交。
4. TUI 应用将用户文本交给流式编排模块。
5. 流式编排模块拒绝空文本；非空文本会被追加为 user 消息。
6. TUI 应用立即渲染本轮 user 消息，并清空输入区域。
7. 流式编排模块从提示词/资源模块读取系统提示词。
8. 流式编排模块从会话模块读取上下文消息。
9. 流式编排模块调用当前 Provider 的流式接口。
10. Provider 使用对应协议向模型 API 发起 SSE 流式请求。
11. Provider 把底层 SSE 事件转换成统一 StreamEvent。
12. 流式编排模块把 StreamEvent 转换成 AppEvent。
13. TUI 应用逐步渲染 assistant 文本增量。
14. 如果收到 thinking 增量且配置允许显示，TUI 应用在 thinking 区域逐步渲染。
15. 如果收到 thinking 增量但配置隐藏，TUI 应用不显示 thinking 内容。
16. 响应完成后，流式编排模块追加 assistant 消息和可选 thinking 消息。
17. 会话模块保存当前会话。
18. TUI 应用显示响应耗时并恢复输入状态。

### 继续历史会话流程
1. 启动后 TUI 应用展示历史会话列表。
2. 用户选择一个历史会话。
3. TUI 应用调用会话模块加载该会话。
4. TUI 应用渲染历史 user、assistant 和可显示的 thinking 消息。
5. 用户继续输入并提交。
6. 流式编排模块使用该会话已有 user/assistant 消息构造上下文。
7. 后续流程与新会话对话流程一致。

### 错误流程
1. 配置加载或校验失败时，命令入口模块把错误交给 TUI 应用展示，或在 TUI 启动前以终端错误形式展示。
2. 用户提交空文本时，TUI 应用不调用 Provider，并保持输入状态。
3. API 请求失败、认证失败、模型错误或 SSE 中断时，Provider 发出错误事件。
4. 流式编排模块把错误事件转换成应用错误事件。
5. TUI 应用展示可理解错误信息。
6. 错误发生后，TUI 应用恢复可操作状态，用户可以继续输入、重试或退出。

### 退出流程
1. 用户在 TUI 中触发退出操作。
2. TUI 应用请求会话模块保存当前会话。
3. 保存成功后，TUI 框架模块结束程序。
4. 保存失败时，TUI 应用展示错误，并允许用户再次退出或返回继续操作。

## 文件组织

```text
xagent/
├── go.mod
├── go.sum
├── cmd/
│   └── xagent/
│       └── main.go                  — 程序入口，加载配置并启动 TUI
├── internal/
│   ├── app/
│   │   ├── app.go                   — TUI 应用模型、状态机、事件处理
│   │   ├── deps.go                  — AppDeps 依赖集合
│   │   ├── events.go                — AppEvent、应用事件类型
│   │   └── update.go                — 用户输入、流式事件、错误和退出处理
│   ├── config/
│   │   ├── config.go                — AppConfig、LLMConfig、UIConfig、StorageConfig
│   │   ├── load.go                  — YAML 配置加载
│   │   └── validate.go              — 配置校验
│   ├── conversation/
│   │   ├── message.go               — Message、MessageRole
│   │   ├── conversation.go          — Conversation 聚合
│   │   ├── store.go                 — ConversationStore 接口
│   │   └── file_store.go            — JSON 文件会话存储
│   ├── orchestrator/
│   │   └── chat.go                  — Send 流式编排、计时、保存
│   ├── provider/
│   │   ├── provider.go              — Provider、ChatRequest、StreamEvent、Usage
│   │   ├── factory.go               — 根据 protocol 创建 Provider
│   │   ├── anthropic.go             — Anthropic Claude SSE 实现
│   │   ├── openai.go                — OpenAI SSE 实现
│   │   └── sse.go                   — 通用 SSE 读取辅助
│   ├── resources/
│   │   ├── prompts.go               — 系统提示词
│   │   └── labels.go                — TUI 固定文案
│   └── tui/
│       ├── program.go               — TUI 框架运行封装
│       ├── input.go                 — 输入框组件封装
│       ├── list.go                  — 历史会话列表组件封装
│       ├── messages.go              — 消息视图和流式渲染组件
│       └── status.go                — 状态栏组件
├── spec.md
├── plan.md
├── task.md
└── checklist.md
```

说明：
- `internal/tui` 是 TUI 框架层，只封装通用终端 UI 能力。
- `internal/app` 是 TUI 应用层，包含 XAgent 业务状态和交互规则。
- `internal/provider` 内部可以包含协议细节，但只通过统一 Provider 接口对外暴露。
- `internal/conversation` 负责会话数据结构和本地 JSON 持久化。
- `internal/resources` 集中放提示词和界面文案。

## 技术决策

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 开发语言 | Go | 用户已确认使用 Go；适合 CLI/TUI、并发流式处理和单二进制分发。 |
| TUI 技术 | Bubble Tea + Bubbles + Lip Gloss | Go 生态中成熟的 TUI 组合，适合输入框、列表、视口和状态栏。 |
| 配置格式 | YAML | 用户明确要求 YAML 管理 LLM 供应商信息。 |
| api_key 处理 | YAML 明文读取 | 用户明确选择首版允许 api_key 直接写在 YAML 中。 |
| Provider 抽象 | 统一 `Provider.StreamChat` 流式接口 | 满足 Anthropic/OpenAI 对上层一致，并方便后续新增后端。 |
| 流式协议 | 直接解析 SSE | spec 要求流式消费，不等待完整响应；直接解析便于同时支持 Anthropic 和 OpenAI。 |
| Claude thinking | 配置控制请求与显示 | 用户要求支持 extended thinking，并通过配置控制 thinking 内容是否显示。 |
| 会话持久化 | 每个会话一个本地 JSON 文件 | 可读、稳定、易调试，满足跨启动保存和恢复。 |
| 启动体验 | 默认展示历史会话列表，也允许配置直接新建会话 | 满足增强首版的历史会话选择，同时保留快速开始新会话。 |
| 上下文策略 | 首版发送当前会话全部 user/assistant 历史消息 | 简单可验证，符合“不做复杂上下文压缩”的边界。 |
| thinking 存储 | 可保存到本地，但不作为普通 assistant 消息发给 OpenAI | 保留可回看能力，同时避免污染 OpenAI 上下文。 |
| 错误处理 | 错误转成应用事件并显示在 TUI | 满足错误反馈和错误后继续操作的需求。 |
| 响应计时 | 流式编排层记录开始和结束时间 | 计时与 Provider 协议解耦，TUI 只负责展示结果。 |
