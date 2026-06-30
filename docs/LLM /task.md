# XAgent Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `go.mod` | Go 模块定义和依赖声明 |
| 新建 | `cmd/xagent/main.go` | 程序入口，加载配置并启动 TUI |
| 新建 | `internal/config/config.go` | AppConfig、LLMConfig、ThinkingConfig、UIConfig、StorageConfig |
| 新建 | `internal/config/load.go` | YAML 配置加载 |
| 新建 | `internal/config/validate.go` | 配置校验 |
| 新建 | `internal/resources/prompts.go` | 系统提示词 |
| 新建 | `internal/resources/labels.go` | TUI 固定文案 |
| 新建 | `internal/conversation/message.go` | Message、MessageRole |
| 新建 | `internal/conversation/conversation.go` | Conversation 聚合与追加消息逻辑 |
| 新建 | `internal/conversation/store.go` | ConversationStore 接口 |
| 新建 | `internal/conversation/file_store.go` | JSON 文件会话存储 |
| 新建 | `internal/provider/provider.go` | Provider、ChatRequest、StreamEvent、Usage |
| 新建 | `internal/provider/factory.go` | Provider 工厂 |
| 新建 | `internal/provider/sse.go` | 通用 SSE 读取辅助 |
| 新建 | `internal/provider/anthropic.go` | Anthropic Claude 流式实现 |
| 新建 | `internal/provider/openai.go` | OpenAI 流式实现 |
| 新建 | `internal/orchestrator/chat.go` | Send 流式编排、计时、保存 |
| 新建 | `internal/tui/program.go` | TUI 框架运行封装 |
| 新建 | `internal/tui/input.go` | 输入框组件封装 |
| 新建 | `internal/tui/list.go` | 历史会话列表组件封装 |
| 新建 | `internal/tui/messages.go` | 消息视图和流式渲染组件 |
| 新建 | `internal/tui/status.go` | 状态栏组件 |
| 新建 | `internal/app/deps.go` | AppDeps 依赖集合 |
| 新建 | `internal/app/events.go` | AppEvent、应用事件类型 |
| 新建 | `internal/app/app.go` | TUI 应用模型和初始化 |
| 新建 | `internal/app/update.go` | 用户输入、流式事件、错误和退出处理 |
| 新建 | `config.example.yaml` | 示例配置文件 |

## T1: 初始化 Go 模块

**文件：** `go.mod`  
**依赖：** 无  
**步骤：**
1. 创建 Go module，模块名使用 `xagent`。
2. 添加 Bubble Tea、Bubbles、Lip Gloss、YAML 解析库依赖。
3. 确认 Go 版本。

**验证：** 运行 `go mod tidy`，期望生成依赖且无错误。

## T2: 定义配置结构

**文件：** `internal/config/config.go`  
**依赖：** T1  
**步骤：**
1. 定义 `AppConfig`。
2. 定义 `LLMConfig`，包含 `Protocol`、`Model`、`BaseURL`、`APIKey`、`Thinking`。
3. 定义 `ThinkingConfig`，包含 `Enabled`、`Show`、`BudgetTokens`。
4. 定义 `UIConfig`，包含 `ShowResponseTimer`、`StartMode`。
5. 定义 `StorageConfig`，包含 `DataDir`。
6. 添加 YAML tag。

**验证：** 运行 `go test ./internal/config`，期望编译通过。

## T3: 实现 YAML 配置加载

**文件：** `internal/config/load.go`  
**依赖：** T2  
**步骤：**
1. 实现 `Load(path string) (*AppConfig, error)`。
2. 从指定路径读取 YAML 文件。
3. 解析到 `AppConfig`。
4. 对缺省 UI 和 storage 字段填充默认值。
5. 调用配置校验。

**验证：** 运行 `go test ./internal/config`，期望编译通过。

## T4: 实现配置校验

**文件：** `internal/config/validate.go`  
**依赖：** T2  
**步骤：**
1. 实现 `Validate(config *AppConfig) error`。
2. 校验 protocol 只能是 `anthropic` 或 `openai`。
3. 校验 model、base_url、api_key 非空。
4. 校验 start mode 只能是 `list` 或 `new`。
5. thinking budget 小于等于 0 时填充默认值。
6. 返回面向用户可理解的错误信息。

**验证：** 运行 `go test ./internal/config`，期望编译通过。

## T5: 添加示例配置

**文件：** `config.example.yaml`  
**依赖：** T2-T4  
**步骤：**
1. 写入 `llm.protocol` 示例。
2. 写入 `llm.model` 示例。
3. 写入 `llm.base_url` 示例。
4. 写入 `llm.api_key` 示例。
5. 写入 thinking、ui、storage 示例字段。

**验证：** 使用示例配置运行配置加载测试或 `go test ./internal/config`，期望配置可解析。

## T6: 实现提示词和界面文案资源

**文件：** `internal/resources/prompts.go`, `internal/resources/labels.go`  
**依赖：** T1  
**步骤：**
1. 定义 `PromptProvider` 实现。
2. 实现 `SystemPrompt()`。
3. 系统提示词明确当前仅支持纯对话。
4. 系统提示词明确不支持 tool use、文件操作、代码编辑、命令执行。
5. 实现 `UILabel(key string) string`。
6. 添加历史列表、输入提示、错误提示、响应耗时等文案。

**验证：** 运行 `go test ./internal/resources`，期望编译通过。

## T7: 定义会话消息结构

**文件：** `internal/conversation/message.go`  
**依赖：** T1  
**步骤：**
1. 定义 `MessageRole`。
2. 定义 `user`、`assistant`、`thinking` 三种角色。
3. 定义 `Message`，包含角色、内容、创建时间。
4. 添加 JSON tag。

**验证：** 运行 `go test ./internal/conversation`，期望编译通过。

## T8: 定义 Conversation 聚合和追加逻辑

**文件：** `internal/conversation/conversation.go`  
**依赖：** T7  
**步骤：**
1. 定义 `Conversation`。
2. 实现创建空会话的逻辑。
3. 实现追加 user 消息。
4. 实现追加 assistant 消息。
5. 实现追加 thinking 消息。
6. 实现从第一条用户文本生成默认标题。
7. 实现更新时间更新。
8. 实现 `ContextMessages`，只返回 user 和 assistant 消息。

**验证：** 运行 `go test ./internal/conversation`，期望编译通过。

## T9: 定义 ConversationStore 接口

**文件：** `internal/conversation/store.go`  
**依赖：** T8  
**步骤：**
1. 定义 `ConversationStore` 接口。
2. 包含 `List`、`Load`、`Save`、`Create` 方法。
3. 方法签名与 plan.md 保持一致。

**验证：** 运行 `go test ./internal/conversation`，期望编译通过。

## T10: 实现 JSON 文件会话存储

**文件：** `internal/conversation/file_store.go`  
**依赖：** T8-T9  
**步骤：**
1. 实现 `FileStore`。
2. 初始化时确保数据目录存在。
3. 每个会话保存为一个 JSON 文件。
4. 实现创建会话并生成唯一 ID。
5. 实现保存会话。
6. 实现加载指定会话。
7. 实现列出所有会话并按更新时间倒序排序。
8. 保存失败时返回可理解错误。

**验证：** 运行 `go test ./internal/conversation`，期望编译通过；手动创建临时目录保存和读取会话成功。

## T11: 定义 Provider 统一接口和事件

**文件：** `internal/provider/provider.go`  
**依赖：** T7  
**步骤：**
1. 定义 `ChatRequest`。
2. 定义 `Usage`。
3. 定义 `StreamEventType`。
4. 定义 `text_delta`、`thinking_delta`、`done`、`error`。
5. 定义 `StreamEvent`。
6. 定义 `Provider` 接口。
7. 确保 Provider 只依赖统一会话消息，不暴露协议细节。

**验证：** 运行 `go test ./internal/provider`，期望编译通过。

## T12: 实现 Provider 工厂

**文件：** `internal/provider/factory.go`  
**依赖：** T2, T11  
**步骤：**
1. 实现 `New(config.LLMConfig) (Provider, error)`。
2. protocol 为 `anthropic` 时返回 Anthropic Provider。
3. protocol 为 `openai` 时返回 OpenAI Provider。
4. 未知 protocol 返回可理解错误。

**验证：** 运行 `go test ./internal/provider`，期望编译通过。

## T13: 实现 SSE 读取辅助

**文件：** `internal/provider/sse.go`  
**依赖：** T11  
**步骤：**
1. 实现从 HTTP response body 逐行读取 SSE。
2. 识别 `event:` 行。
3. 识别 `data:` 行。
4. 支持空行作为一个事件结束。
5. 支持 OpenAI `[DONE]` 数据。
6. 读取异常时返回错误事件。

**验证：** 运行 `go test ./internal/provider`，期望编译通过；用字符串 reader 验证 SSE chunk 可解析。

## T14: 实现 Anthropic Provider

**文件：** `internal/provider/anthropic.go`  
**依赖：** T11-T13  
**步骤：**
1. 定义 Anthropic Provider 结构。
2. 构造 Claude Messages 流式请求。
3. 设置认证和必要请求头。
4. 将统一 `ChatRequest` 转换为 Anthropic 请求体。
5. thinking enabled 时加入 extended thinking 参数。
6. 发起 HTTP 请求。
7. 解析 SSE 事件。
8. 将文本增量转换为 `text_delta`。
9. 将 thinking 增量转换为 `thinking_delta`。
10. 将完成事件转换为 `done`。
11. 将错误转换为 `error`。

**验证：** 运行 `go test ./internal/provider`，期望编译通过；用 mock HTTP server 返回 Anthropic 风格 SSE，期望收到 text/thinking/done 事件。

## T15: 实现 OpenAI Provider

**文件：** `internal/provider/openai.go`  
**依赖：** T11-T13  
**步骤：**
1. 定义 OpenAI Provider 结构。
2. 构造 OpenAI Chat Completions 兼容流式请求。
3. 设置认证和请求头。
4. 将统一 `ChatRequest` 转换为 OpenAI 请求体。
5. 设置 stream 为 true。
6. 发起 HTTP 请求。
7. 解析 SSE chunk。
8. 将文本增量转换为 `text_delta`。
9. 将 `[DONE]` 转换为 `done`。
10. 将错误转换为 `error`。
11. 忽略 Claude thinking 配置。

**验证：** 运行 `go test ./internal/provider`，期望编译通过；用 mock HTTP server 返回 OpenAI 风格 SSE，期望收到 text/done 事件。

## T16: 定义应用事件

**文件：** `internal/app/events.go`  
**依赖：** T11  
**步骤：**
1. 定义 `AppEventType`。
2. 定义用户消息已提交事件。
3. 定义 assistant 文本增量事件。
4. 定义 thinking 文本增量事件。
5. 定义完成事件，包含耗时。
6. 定义错误事件。
7. 定义 `AppEvent`。

**验证：** 运行 `go test ./internal/app`，期望编译通过。

## T17: 定义 AppDeps

**文件：** `internal/app/deps.go`  
**依赖：** T2, T6, T9, T11  
**步骤：**
1. 定义 `AppDeps`。
2. 包含配置、Provider、ConversationStore、PromptProvider。
3. 包含启动所需的初始会话列表或 store 引用。

**验证：** 运行 `go test ./internal/app`，期望编译通过。

## T18: 实现流式编排

**文件：** `internal/orchestrator/chat.go`  
**依赖：** T6, T8-T11, T16  
**步骤：**
1. 定义 Orchestrator。
2. 实现 `Send(ctx, conversation, userText)`。
3. 拒绝空用户文本。
4. 追加 user 消息。
5. 发送用户消息已提交事件。
6. 构造包含系统提示词和上下文的 `ChatRequest`。
7. 调用 Provider 流式接口。
8. 将 `text_delta` 转换为应用事件。
9. 将 `thinking_delta` 转换为应用事件。
10. 累积 assistant 文本和 thinking 文本。
11. done 时追加 assistant 和可选 thinking 消息。
12. 保存会话。
13. 发送包含响应耗时的完成事件。
14. error 时发送错误事件并结束本轮事件流。

**验证：** 运行 `go test ./internal/orchestrator`，期望编译通过；用 fake Provider 发送 delta/done，期望会话被更新并生成完成事件。

## T19: 实现 TUI 框架运行封装

**文件：** `internal/tui/program.go`  
**依赖：** T1  
**步骤：**
1. 封装 Bubble Tea program 启动。
2. 提供 `Run(model tea.Model) error`。
3. 保持框架层不依赖业务模块。

**验证：** 运行 `go test ./internal/tui`，期望编译通过。

## T20: 实现 TUI 输入框组件

**文件：** `internal/tui/input.go`  
**依赖：** T1  
**步骤：**
1. 封装 Bubbles textinput。
2. 提供输入值读取。
3. 提供清空输入。
4. 提供禁用/启用状态，用于 streaming 期间控制输入。
5. 支持提交键。

**验证：** 运行 `go test ./internal/tui`，期望编译通过。

## T21: 实现 TUI 历史列表组件

**文件：** `internal/tui/list.go`  
**依赖：** T8  
**步骤：**
1. 封装 Bubbles list。
2. 将 Conversation 转换为可显示列表项。
3. 展示标题和更新时间。
4. 提供当前选中会话 ID。
5. 提供新建会话入口项。

**验证：** 运行 `go test ./internal/tui`，期望编译通过。

## T22: 实现 TUI 消息视图组件

**文件：** `internal/tui/messages.go`  
**依赖：** T7-T8  
**步骤：**
1. 实现消息视图渲染。
2. 区分 user、assistant、thinking 消息。
3. 支持追加 assistant 文本增量。
4. 支持追加 thinking 文本增量。
5. thinking hidden 时不渲染 thinking。
6. 流式过程中保持已有内容结构可读。

**验证：** 运行 `go test ./internal/tui`，期望编译通过；用示例消息渲染，期望 user/assistant/thinking 区块区分显示。

## T23: 实现 TUI 状态栏组件

**文件：** `internal/tui/status.go`  
**依赖：** T2  
**步骤：**
1. 展示当前 Provider。
2. 展示当前模型。
3. 展示 streaming 状态。
4. 展示最近响应耗时。
5. 展示错误信息。
6. 避免展示 api_key。

**验证：** 运行 `go test ./internal/tui`，期望编译通过；渲染状态栏时不包含 api_key。

## T24: 实现 TUI 应用模型初始化

**文件：** `internal/app/app.go`  
**依赖：** T16-T23  
**步骤：**
1. 定义应用状态枚举。
2. 定义 app model。
3. 保存 AppDeps。
4. 初始化历史列表。
5. 按 `StartMode` 进入会话列表或新会话。
6. 初始化输入框、消息视图和状态栏。

**验证：** 运行 `go test ./internal/app`，期望编译通过。

## T25: 实现 TUI 用户输入和会话选择更新逻辑

**文件：** `internal/app/update.go`  
**依赖：** T18, T24  
**步骤：**
1. 处理历史列表选择。
2. 处理新建会话。
3. 处理输入框更新。
4. 处理提交非空用户文本。
5. 提交后清空输入框。
6. 提交空输入时不调用 Orchestrator。
7. 提交后进入 streaming 状态。

**验证：** 运行 `go test ./internal/app`，期望编译通过；用空输入触发提交时不会调用发送逻辑。

## T26: 实现 TUI 流式事件、错误和退出逻辑

**文件：** `internal/app/update.go`  
**依赖：** T25  
**步骤：**
1. 处理用户消息已提交事件并渲染 user 文本。
2. 处理 assistant 文本增量并更新消息视图。
3. 处理 thinking 文本增量并按配置显示或隐藏。
4. 处理完成事件并展示响应耗时。
5. 完成后恢复输入状态。
6. 处理错误事件并展示错误反馈。
7. 错误后恢复可操作状态。
8. 处理退出操作并保存当前会话。

**验证：** 运行 `go test ./internal/app`，期望编译通过；用 fake AppEvent 序列验证 user、assistant、thinking、done、error 状态更新。

## T27: 实现程序入口

**文件：** `cmd/xagent/main.go`  
**依赖：** T3-T6, T10-T26  
**步骤：**
1. 读取配置路径，默认使用本地配置文件路径。
2. 加载并校验配置。
3. 初始化会话 store。
4. 创建 resources provider。
5. 创建模型 Provider。
6. 创建 TUI app。
7. 调用 TUI framework run。
8. 配置加载失败时输出可理解错误。

**验证：** 运行 `go run ./cmd/xagent`，配置缺失时显示可理解错误；配置存在时进入 TUI。

## T28: 全量编译和基础测试

**文件：** 全部 Go 文件  
**依赖：** T1-T27  
**步骤：**
1. 运行 `go test ./...`。
2. 修复编译错误。
3. 修复单元测试错误。
4. 运行 `go build ./cmd/xagent`。
5. 确认生成二进制。

**验证：** `go test ./...` 和 `go build ./cmd/xagent` 均通过。

## T29: 手动验证配置和 TUI 启动

**文件：** `config.example.yaml`, `cmd/xagent/main.go`, TUI 相关文件  
**依赖：** T28  
**步骤：**
1. 复制示例配置为本地配置。
2. 填入可用模型配置。
3. 启动 XAgent。
4. 观察历史列表或新会话界面。
5. 输入空文本并提交。
6. 确认不会发起请求且仍可继续输入。
7. 退出程序。

**验证：** 终端中可进入 TUI，空输入不请求模型，退出正常。

## T30: 手动验证流式对话、多轮上下文和会话保存

**文件：** 全部运行路径  
**依赖：** T29  
**步骤：**
1. 启动 XAgent。
2. 输入第一条真实问题。
3. 观察回复逐步渲染。
4. 观察回复完成后显示耗时。
5. 输入第二条依赖前文的问题。
6. 观察模型能基于前文回答。
7. 退出程序。
8. 重新启动 XAgent。
9. 从历史列表选择刚才会话。
10. 继续输入问题。
11. 确认历史上下文仍生效。

**验证：** 流式渲染、多轮上下文、响应计时、跨启动历史恢复均可观察通过。

## T31: 手动验证 Anthropic/OpenAI 切换和 thinking 显示

**文件：** Provider、配置、TUI 相关文件  
**依赖：** T30  
**步骤：**
1. 使用 Anthropic 配置启动。
2. 开启 thinking enabled 和 show。
3. 提交问题并观察 thinking 与最终回复区分展示。
4. 将 thinking show 改为 false。
5. 再次提交问题并确认只显示最终回复。
6. 切换为 OpenAI 配置。
7. 提交问题并确认流式回复正常。
8. 确认 OpenAI 不显示 Claude thinking 内容。

**验证：** Anthropic、OpenAI 均可流式对话；thinking 显示配置生效。

## 执行顺序

```text
T1
→ T2 → T3 → T4 → T5
→ T6
→ T7 → T8 → T9 → T10
→ T11 → T12 → T13 → T14 → T15
→ T16 → T17 → T18
→ T19 → T20 → T21 → T22 → T23
→ T24 → T25 → T26
→ T27 → T28 → T29 → T30 → T31
```
