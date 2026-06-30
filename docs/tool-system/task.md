# XAgent Tool System Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/tool/tool.go` | Tool 接口、Schema、ToolCall、ToolResult、ToolError |
| 新建 | `internal/tool/schema.go` | Schema 辅助构造 |
| 新建 | `internal/tool/path.go` | 项目根目录路径安全检查 |
| 新建 | `internal/tool/registry.go` | 工具注册中心和 Provider 定义转换 |
| 新建 | `internal/tool/executor.go` | 工具执行、超时、拒绝、输出截断 |
| 新建 | `internal/tool/read.go` | Read 工具 |
| 新建 | `internal/tool/write.go` | Write 工具 |
| 新建 | `internal/tool/edit.go` | Edit 工具 |
| 新建 | `internal/tool/bash.go` | Bash 工具 |
| 新建 | `internal/tool/glob.go` | Glob 工具 |
| 新建 | `internal/tool/grep.go` | Grep 工具 |
| 新建 | `internal/tool/tool_test.go` | 工具核心、路径、执行器测试 |
| 新建 | `internal/tool/builtin_test.go` | 六个内置工具测试 |
| 修改 | `internal/provider/provider.go` | 增加工具定义、ToolCall、工具事件 |
| 修改 | `internal/provider/anthropic.go` | Claude 工具定义、tool_use 流式解析、tool_result 回灌 |
| 修改 | `internal/provider/openai.go` | OpenAI tools、tool_calls 流式拼接、tool role 回灌 |
| 新建 | `internal/provider/tool_parse_test.go` | Provider 工具调用解析测试 |
| 修改 | `internal/conversation/message.go` | 增加工具调用/工具结果消息类型 |
| 修改 | `internal/conversation/conversation.go` | 追加工具调用/工具结果、上下文转换 |
| 修改 | `internal/events/events.go` | 增加工具行、确认和工具结果相关事件 |
| 修改 | `internal/orchestrator/chat.go` | 安全工具调用、危险工具确认、最终回复请求 |
| 修改 | `internal/tui/messages.go` | Claude Code 风格工具行渲染 |
| 修改 | `internal/tui/status.go` | 工具确认/错误状态展示 |
| 修改 | `internal/app/deps.go` | 注入工具注册中心和执行器 |
| 修改 | `internal/app/app.go` | 增加等待工具确认状态 |
| 修改 | `internal/app/update.go` | 处理工具事件和允许/拒绝 |
| 修改 | `cmd/xagent/main.go` | 初始化工具系统 |
| 新建 | `.claude/tool_smoke.go` | 工具系统 smoke test |
| 新建 | `docs/tool-system/task.md` | 本任务文档 |
| 新建 | `docs/tool-system/checklist.md` | 本章验收清单 |

## T1: 定义工具核心类型

**文件：** `internal/tool/tool.go`  
**依赖：** 无  
**步骤：**
1. 定义 `Tool` 接口。
2. 定义 `ToolSchema`。
3. 定义 `ToolSchemaProperty`。
4. 定义 `ToolRisk`，包含 `safe` 和 `dangerous`。
5. 定义 `ToolInput`。
6. 定义 `ToolCall`。
7. 定义 `ToolResult`。
8. 定义 `ToolResultStatus`，包含 `success`、`error`、`denied`、`timeout`。
9. 定义 `ToolError`。
10. 定义稳定错误码常量，例如 `path_outside_project`、`not_found`、`multiple_matches`、`timeout`、`invalid_arguments`、`tool_not_found`、`multiple_tool_calls_not_supported`。

**验证：** 运行 `go test ./internal/tool`，期望编译通过。

## T2: 实现 Schema 辅助构造

**文件：** `internal/tool/schema.go`  
**依赖：** T1  
**步骤：**
1. 实现字符串字段 Schema 辅助函数。
2. 实现布尔字段 Schema 辅助函数。
3. 实现对象 Schema 构造辅助函数。
4. 确保生成结构可转换为 Anthropic/OpenAI JSON Schema。
5. 为六个工具准备可复用 Schema 构造方式。

**验证：** 运行 `go test ./internal/tool`，期望编译通过。

## T3: 实现项目路径安全检查

**文件：** `internal/tool/path.go`  
**依赖：** T1  
**步骤：**
1. 实现 `ResolveProjectPath(projectRoot, requestedPath)`。
2. 拒绝空路径。
3. 规范化项目根目录和请求路径。
4. 支持相对路径和绝对路径。
5. 检查最终路径必须在项目根目录内。
6. 越界时返回 `path_outside_project` 结构化错误。
7. 实现 `RelativeToRoot(projectRoot, absolutePath)`。
8. 处理符号路径时以最终绝对路径做根目录约束。

**验证：** 运行 `go test ./internal/tool`；`../` 和项目外绝对路径返回越界错误，项目内路径通过。

## T4: 实现工具注册中心

**文件：** `internal/tool/registry.go`  
**依赖：** T1-T3  
**步骤：**
1. 定义 `Registry`。
2. 实现 `Register`。
3. 重复工具名返回错误。
4. 工具名按大小写敏感匹配。
5. 未知工具名通过执行器返回 `tool_not_found`。
6. 实现 `Get`。
7. 实现 `List`。
8. 实现 `AnthropicDefinitions`。
9. 实现 `OpenAIDefinitions`。
10. 实现创建默认注册中心并登记六个内置工具。

**验证：** 运行 `go test ./internal/tool`；重复注册返回错误，六个默认工具均可查找。

## T5: 实现工具执行器和统一截断策略

**文件：** `internal/tool/executor.go`  
**依赖：** T1-T4  
**步骤：**
1. 定义 `Executor`。
2. 实现 `NeedsConfirmation`。
3. 实现 `Execute`。
4. 在执行前解析 JSON 参数。
5. 找不到工具返回 `tool_not_found`。
6. 参数 JSON 无效返回 `invalid_arguments`。
7. 使用 context 超时控制执行时间。
8. 执行超时返回 `timeout`。
9. 实现 `Denied` 构造拒绝结果。
10. 定义统一截断函数。
11. 对 `ToolResult.Content` 截断。
12. 对 stdout/stderr 类数据分别截断。
13. 对匹配列表限制数量。
14. 截断后设置 `ToolResult.Truncated = true`。
15. 截断后在 `Summary` 或 `Data` 中标记 truncated。

**验证：** 运行 `go test ./internal/tool`；fake tool 超时返回 timeout，大输出返回 truncated。

## T6: 实现 Read 工具

**文件：** `internal/tool/read.go`  
**依赖：** T1-T5  
**步骤：**
1. 定义 Read 工具。
2. Schema 包含 `path`。
3. 风险级别为 safe。
4. 使用路径安全检查解析路径。
5. 读取项目内文本文件内容。
6. 文件不存在返回结构化 `not_found` 错误。
7. 路径越界返回结构化 `path_outside_project` 错误。
8. 返回内容、字节数、路径和摘要。
9. 输出过大时配合 Executor 截断。

**验证：** 运行 `go test ./internal/tool`；读取项目内文本文件成功，读取项目外路径失败。

## T7: 实现 Write 工具

**文件：** `internal/tool/write.go`  
**依赖：** T1-T5  
**步骤：**
1. 定义 Write 工具。
2. Schema 包含 `path` 和 `content`。
3. 风险级别为 dangerous。
4. 使用路径安全检查解析路径。
5. 写入内容到项目内文件。
6. 必要时创建父目录。
7. 返回写入路径和字节数。
8. 路径越界返回结构化错误。
9. 不在工具内部处理用户确认，确认由 Executor/Orchestrator/TUI 流程处理。

**验证：** 运行 `go test ./internal/tool`；写项目内临时文件成功，越界路径失败。

## T8: 实现 Edit 工具

**文件：** `internal/tool/edit.go`  
**依赖：** T1-T5  
**步骤：**
1. 定义 Edit 工具。
2. Schema 包含 `path`、`old_text`、`new_text`。
3. 风险级别为 dangerous。
4. 使用路径安全检查解析路径。
5. 读取目标文件。
6. 统计 `old_text` 出现次数。
7. 匹配不到返回 `not_found`。
8. 匹配多次返回 `multiple_matches`。
9. 唯一匹配时替换并写回。
10. 返回替换成功摘要。
11. 不在工具内部处理用户确认。

**验证：** 运行 `go test ./internal/tool`；唯一匹配替换成功，零匹配和多匹配返回结构化错误。

## T9: 实现 Bash 工具

**文件：** `internal/tool/bash.go`  
**依赖：** T1-T5  
**步骤：**
1. 定义 Bash 工具。
2. Schema 包含 `command`。
3. 风险级别为 dangerous。
4. 在项目根目录执行命令。
5. 明确 Bash 不提供系统级沙箱，只保证工作目录为项目根目录。
6. 捕获 stdout。
7. 捕获 stderr。
8. 捕获 exit_code。
9. 超时时返回 timeout 状态。
10. 非零退出码作为结构化 error 结果返回。
11. Bash 确认提示必须能展示完整 command。
12. 不在工具内部处理用户确认。

**验证：** 运行 `go test ./internal/tool`；`pwd` 在项目根目录执行，非零命令返回 error，超时命令返回 timeout。

## T10: 实现 Glob 工具

**文件：** `internal/tool/glob.go`  
**依赖：** T1-T5  
**步骤：**
1. 定义 Glob 工具。
2. Schema 包含 `pattern`。
3. 风险级别为 safe。
4. 在项目根目录内执行 glob 匹配。
5. 只返回文件路径。
6. 路径结果转换为相对项目根目录。
7. 无结果返回结构化结果而不是崩溃。
8. 限制返回文件数量。

**验证：** 运行 `go test ./internal/tool`；`internal/**/*.go` 返回 Go 文件，无匹配返回空结果摘要。

## T11: 实现 Grep 工具

**文件：** `internal/tool/grep.go`  
**依赖：** T1-T5  
**步骤：**
1. 定义 Grep 工具。
2. Schema 包含 `pattern`、可选 `path`、可选 `regex`。
3. 风险级别为 safe。
4. 在项目根目录或指定子路径内搜索文本文件。
5. 支持普通文本匹配。
6. 支持正则匹配。
7. 返回文件、行号和摘要。
8. 无结果返回结构化结果。
9. 限制匹配结果数量。
10. 指定 path 越界时返回结构化错误。

**验证：** 运行 `go test ./internal/tool`；搜索已有字符串返回位置，无结果返回结构化空结果，越界 path 返回错误。

## T12: 扩展会话消息类型和历史格式

**文件：** `internal/conversation/message.go`, `internal/conversation/conversation.go`  
**依赖：** T1  
**步骤：**
1. 增加 tool_call 消息角色或类型。
2. 增加 tool_result 消息角色或类型。
3. Message 增加 `ToolCallID`。
4. Message 增加 `ToolName`。
5. Message 增加 `RawToolArguments`。
6. Message 增加 `ToolResultContent`。
7. Message 增加 `ToolResultStatus`。
8. Message 增加 `ToolResultSummary`。
9. Message 增加 `ToolErrorCode`。
10. 实现追加工具调用消息。
11. 实现追加工具结果消息。
12. `ContextMessages` 保留 tool_call 和 tool_result 的顺序。
13. 历史恢复时能从 tool_call/tool_result 重建 ToolDisplay。
14. thinking 消息仍不作为普通上下文传给 OpenAI。

**验证：** 运行 `go test ./internal/conversation`；保存和加载含工具消息的会话成功，顺序保持正确。

## T13: 扩展 Provider 统一类型

**文件：** `internal/provider/provider.go`  
**依赖：** T1, T12  
**步骤：**
1. `ChatRequest` 增加工具定义字段。
2. 增加统一工具定义结构。
3. `StreamEventType` 增加 `tool_call`。
4. `StreamEvent` 增加 `ToolCall` 字段。
5. 定义 Provider 层多工具调用处理策略：本阶段收到多个工具调用时返回 `multiple_tool_calls_not_supported` 工具结果或错误事件，不执行任何工具。
6. 增加工具结果回灌所需的请求字段或消息结构。
7. 保持普通文本事件兼容。

**验证：** 运行 `go test ./internal/provider`，期望编译通过。

## T14: 接入 Anthropic 工具调用

**文件：** `internal/provider/anthropic.go`  
**依赖：** T4, T12, T13  
**步骤：**
1. 将注册中心的 Anthropic 工具定义转换为 SDK 请求参数。
2. 在 Claude 请求中携带 tools。
3. 解析流式 tool_use content block。
4. 拼接 input_json_delta。
5. 保留 Claude tool_use ID。
6. 工具调用完成时输出统一 `tool_call` 事件。
7. 如果一次响应出现多个工具调用，返回 `multiple_tool_calls_not_supported`。
8. 工具结果回灌时构造 Claude user tool_result block。
9. 保持普通文本流式输出不退化。

**验证：** 运行 `go test ./internal/provider`；用 fake 事件验证工具参数碎片可拼接，多工具调用返回不支持。

## T15: 接入 OpenAI 工具调用

**文件：** `internal/provider/openai.go`  
**依赖：** T4, T12, T13  
**步骤：**
1. 将注册中心的 OpenAI 工具定义加入请求体。
2. 解析流式 `tool_calls` delta。
3. 按 index 维护工具调用状态。
4. 拼接 arguments JSON 碎片。
5. 保留 OpenAI tool_call_id。
6. 工具调用完成时输出统一 `tool_call` 事件。
7. 如果一次响应出现多个工具调用，返回 `multiple_tool_calls_not_supported`。
8. 工具结果回灌时构造 OpenAI tool role message。
9. 保持普通文本流式输出不退化。

**验证：** 运行 `go test ./internal/provider`；用 mock OpenAI SSE 返回 tool_calls delta，期望得到完整工具调用事件；多工具调用返回不支持。

## T16: 扩展应用事件和确认通道

**文件：** `internal/events/events.go`  
**依赖：** T1  
**步骤：**
1. 增加 `tool_pending` 事件。
2. 增加 `tool_waiting_confirmation` 事件。
3. 增加 `tool_running` 事件。
4. 增加 `tool_success` 事件。
5. 增加 `tool_error` 事件。
6. 增加 `tool_denied` 事件。
7. 定义 `ToolConfirmationDecision`。
8. 工具确认事件携带用于返回用户选择的 channel 或 request ID。
9. 事件结构增加 ToolDisplay、ToolConfirmationRequest、ToolResult 字段。

**验证：** 运行 `go test ./internal/events`，期望编译通过。

## T17: 扩展流式编排层：安全工具调用

**文件：** `internal/orchestrator/chat.go`  
**依赖：** T1-T16  
**步骤：**
1. Orchestrator 增加 Registry 和 Executor。
2. 首次模型请求携带工具定义。
3. 收到普通文本时保持现有流式输出。
4. 收到安全工具调用时发送 tool_pending。
5. 发送 tool_running。
6. 执行工具。
7. 发送 tool_success 或 tool_error。
8. 追加工具调用和工具结果到会话历史。
9. 发起一次最终回复请求。
10. 保存会话。

**验证：** 运行 `go test ./internal/orchestrator`；fake Provider 返回 Read 工具调用时，fake Tool 执行一次并触发最终回复。

## T18: 扩展流式编排层：危险工具确认和边界

**文件：** `internal/orchestrator/chat.go`  
**依赖：** T17  
**步骤：**
1. 收到危险工具调用时发送 tool_waiting_confirmation。
2. 编排层等待 TUI 返回允许或拒绝。
3. 用户允许时发送 tool_running 并执行工具。
4. 用户拒绝时构造 denied 结果。
5. 发送 tool_denied。
6. 追加 denied 工具结果到会话历史。
7. 最终回复再次请求工具时不执行第二轮工具。
8. 返回或展示不支持 Agent Loop 的提示。
9. 多工具调用时不执行任何工具，并回灌 `multiple_tool_calls_not_supported` 结果。

**验证：** 运行 `go test ./internal/orchestrator`；fake 确认允许和拒绝路径都能完成；最终回复再次工具调用不会执行。

## T19: 扩展 TUI 消息视图工具行

**文件：** `internal/tui/messages.go`  
**依赖：** T16  
**步骤：**
1. 定义消息视图中的工具行数据。
2. 支持追加工具行。
3. 支持按 CallID 更新工具行状态。
4. 渲染 `● Tool(args)`。
5. 成功状态显示摘要。
6. 失败状态使用可区分样式。
7. 拒绝状态显示用户已拒绝。
8. 历史会话恢复时能渲染工具消息。

**验证：** 运行 `go test ./internal/tui`；构造 ToolDisplay 渲染，输出包含 `● Read(path)` 和摘要。

## T20: 扩展 TUI 状态栏和确认提示

**文件：** `internal/tui/status.go`, `internal/app/app.go`  
**依赖：** T16-T19  
**步骤：**
1. 状态栏支持显示等待工具确认。
2. 应用模型增加等待确认状态。
3. 保存当前待确认工具调用。
4. View 中展示允许/拒绝提示。
5. 提示包含工具名和关键参数摘要。
6. 等待确认时，按 `y` 允许。
7. 等待确认时，按 `n` 拒绝。
8. 等待确认时，Enter 不提交新输入。
9. 等待确认时，`q`/`ctrl+c` 仍可退出。

**验证：** 运行 `go test ./internal/app ./internal/tui`；等待确认状态下界面包含允许/拒绝提示。

## T21: 处理 TUI 工具事件和确认输入

**文件：** `internal/app/update.go`  
**依赖：** T18-T20  
**步骤：**
1. 处理 tool_pending。
2. 处理 tool_running。
3. 处理 tool_success。
4. 处理 tool_error。
5. 处理 tool_denied。
6. 处理 tool_waiting_confirmation。
7. 用户按 `y` 时通知编排层允许。
8. 用户按 `n` 时通知编排层拒绝。
9. 确认结束后恢复正常状态。

**验证：** 运行 `go test ./internal/app`；fake 工具确认事件后，允许和拒绝路径都能更新状态。

## T22: 初始化工具系统

**文件：** `cmd/xagent/main.go`, `internal/app/deps.go`  
**依赖：** T1-T21  
**步骤：**
1. 命令入口确定项目根目录。
2. 创建默认工具注册中心。
3. 创建工具 Executor。
4. 将注册中心和 Executor 注入 Orchestrator/AppDeps。
5. 确认普通启动流程不变。

**验证：** 运行 `go test ./...` 和 `go build ./cmd/xagent`，期望通过。

## T23: 编写工具单元测试

**文件：** `internal/tool/tool_test.go`, `internal/tool/builtin_test.go`  
**依赖：** T1-T11  
**步骤：**
1. 测试 Read 越界。
2. 测试 Write 越界。
3. 测试 Edit 零匹配。
4. 测试 Edit 多匹配。
5. 测试 Bash 非零退出。
6. 测试 Bash 超时。
7. 测试 Glob 无匹配。
8. 测试 Grep 无匹配。
9. 测试输出截断。
10. 测试注册中心重复注册。

**验证：** 运行 `go test ./internal/tool`，期望通过。

## T24: 编写 Provider 工具解析测试

**文件：** `internal/provider/tool_parse_test.go`  
**依赖：** T13-T15  
**步骤：**
1. 测试 OpenAI arguments 分片拼接。
2. 测试 Anthropic input_json_delta 分片拼接。
3. 测试 OpenAI 多工具调用返回不支持。
4. 测试 Anthropic 多工具调用返回不支持。
5. 测试普通文本流式事件不受影响。

**验证：** 运行 `go test ./internal/provider`，期望通过。

## T25: 编写工具系统 smoke test

**文件：** `.claude/tool_smoke.go` 或测试文件  
**依赖：** T1-T22  
**步骤：**
1. 构造 fake Provider 返回 Read 工具调用。
2. 执行编排层 Send。
3. 验证 TUI 事件中出现工具行。
4. 验证 Read 工具结果进入会话。
5. 验证最终回复被渲染。
6. 构造 Edit 多匹配失败场景。
7. 验证结构化错误结果可回灌。
8. 构造危险工具拒绝场景。
9. 验证 denied 结果可回灌。

**验证：** 运行 smoke test，期望工具调用、工具结果、最终回复、错误回灌和拒绝回灌均可观察。

## T26: 全量编译和回归验证

**文件：** 全部相关文件  
**依赖：** T1-T25  
**步骤：**
1. 运行 `gofmt -w cmd internal`。
2. 运行 `go test ./...`。
3. 运行 `go build ./cmd/xagent`。
4. 用 mock 普通对话验证无工具路径仍流式输出。
5. 用 mock 工具调用验证工具行和结果摘要。
6. 用危险工具 mock 验证确认提示和拒绝路径。
7. 验证工具结果和工具行不显示 api_key。

**验证：** 所有命令通过；普通对话、工具调用、危险工具确认均可观察通过。

## 执行顺序

```text
T1 → T2 → T3 → T4 → T5
  → T6 → T7 → T8 → T9 → T10 → T11
  → T12 → T13
  → T14 → T15
  → T16
  → T17 → T18
  → T19 → T20 → T21
  → T22
  → T23 → T24 → T25 → T26
```

自检结果：
- 已补充 Bash 安全边界说明。
- 已补充多工具调用不支持策略。
- 已补充 Anthropic/OpenAI 工具结果回灌结构要求。
- 已补充 TUI 确认通道和确认按键。
- 已补充工具输出截断策略。
- 已补充历史工具消息保存字段。
- 已补充工具和 Provider 单元测试任务。
