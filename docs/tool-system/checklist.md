# XAgent Tool System Checklist

> 每一项通过运行代码或观察行为来验证，聚焦工具系统行为。

## 实现完整性
- [ ] 统一工具抽象存在且包含名称、描述、参数 Schema、风险级别和执行能力（验证：运行 `go test ./internal/tool`）
- [ ] 工具注册中心可以登记工具并按名称查找（验证：注册六个默认工具后能查到 Read/Write/Edit/Bash/Glob/Grep）
- [ ] 重复注册工具名会返回错误（验证：单元测试重复注册同名工具）
- [ ] 注册中心默认只暴露 Read、Write、Edit、Bash、Glob、Grep 六个工具（验证：启动后检查 Provider 收到的工具定义，不包含网络、浏览器、HTTP、MCP、Git、PR 或 Issue 工具）
- [ ] 注册中心能生成 Anthropic 工具定义（验证：单元测试输出包含名称、描述和 input schema）
- [ ] 注册中心能生成 OpenAI 工具定义（验证：单元测试输出包含 function name、description 和 parameters）
- [ ] Read 工具能读取项目内文本文件（验证：读取临时文本文件，结果包含内容和字节数）
- [ ] Read 工具访问项目外路径时返回结构化错误（验证：读取 `../` 路径，错误码为 `path_outside_project`）
- [ ] Write 工具能写入项目内文件（验证：执行工具后文件内容符合输入）
- [ ] Write 工具访问项目外路径时返回结构化错误（验证：写入项目外路径被拒绝）
- [ ] Edit 工具唯一匹配时能替换内容（验证：临时文件中 old_text 出现一次，替换后内容正确）
- [ ] Edit 工具零匹配时返回结构化错误（验证：错误码为 `not_found`）
- [ ] Edit 工具多匹配时返回结构化错误（验证：错误码为 `multiple_matches`）
- [ ] 文件工具不能通过项目内符号链接访问项目外路径（验证：创建指向项目外的 symlink，Read/Write/Edit/Grep 访问该 symlink 时返回 `path_outside_project`）
- [ ] Bash 工具在项目根目录执行命令（验证：执行 `pwd`，输出为项目根目录）
- [ ] Bash 工具返回 stdout、stderr、exit_code 和 timed_out（验证：执行成功和失败命令都能看到结构化数据）
- [ ] Bash 非零退出码作为结构化错误结果返回（验证：执行 `exit 2`，状态为 error）
- [ ] Bash 超时时返回 timeout 结果（验证：执行超过超时的命令，状态为 timeout）
- [ ] Glob 工具能返回项目内匹配文件（验证：匹配 `internal/**/*.go` 返回 Go 文件）
- [ ] Glob 无匹配时返回结构化空结果（验证：匹配不存在模式，程序不崩溃）
- [ ] Grep 工具能返回匹配文件、行号和摘要（验证：搜索已存在字符串）
- [ ] Grep 无匹配时返回结构化空结果（验证：搜索不存在字符串，程序不崩溃）
- [ ] 工具输出过大时会截断并标记（验证：构造大输出，`Truncated` 为 true）
- [ ] 过大的工具结果写入会话历史时保存的是截断内容和截断标记，而不是完整大输出（验证：构造大 Read/Bash/Grep 输出，保存会话后检查 JSON 中内容已截断且 `Truncated` 为 true）
- [ ] 工具执行失败不会导致程序崩溃或会话中断（验证：触发文件不存在、越界、非零退出等错误后仍可继续）

## Provider 集成
- [ ] Anthropic 请求能携带工具定义（验证：fake/mock 请求检查 tools 字段）
- [ ] Anthropic 流式 tool_use 能转换为统一 ToolCall（验证：fake 事件包含工具名、ID、参数）
- [ ] Anthropic input_json_delta 参数碎片能正确拼接（验证：分片 JSON 拼接成完整参数）
- [ ] Anthropic 工具结果能按 tool_result 格式回灌（验证：构造工具结果后下一次请求上下文包含 tool_result）
- [ ] OpenAI 请求能携带 tools 定义（验证：mock 请求体包含 tools）
- [ ] OpenAI 流式 tool_calls delta 能转换为统一 ToolCall（验证：mock SSE 输出完整工具调用）
- [ ] OpenAI arguments 参数碎片能正确拼接（验证：分片 arguments 拼接成完整 JSON）
- [ ] OpenAI 工具结果能按 tool role 格式回灌（验证：下一次请求上下文包含 tool_call_id 对应 tool 消息）
- [ ] 普通无工具文本流式响应不受影响（验证：mock 普通 SSE 仍逐步输出文本）
- [ ] 一次响应中多个工具调用不会被执行（验证：Provider 或 Orchestrator 返回 `multiple_tool_calls_not_supported`）

## 编排与会话
- [ ] 用户提交后首次模型请求包含工具定义（验证：fake Provider 收到工具列表）
- [ ] 模型返回安全工具调用时，工具会执行一次（验证：fake Read 调用计数为 1）
- [ ] 安全工具结果会追加到会话历史（验证：会话中存在 tool_call 和 tool_result 消息）
- [ ] 工具结果回灌后会发起一次最终回复请求（验证：fake Provider 收到第二次请求）
- [ ] 最终回复文本会流式展示（验证：事件流中出现 text_delta）
- [ ] 最终回复再次请求工具时不会执行第二轮工具（验证：工具调用计数仍为 1，并出现不支持 Agent Loop 的提示）
- [ ] 危险工具调用会进入等待确认状态（验证：Write/Edit/Bash 触发 tool_waiting_confirmation）
- [ ] 用户允许危险工具后工具才执行（验证：发送允许决策后工具调用计数增加）
- [ ] 用户拒绝危险工具后工具不执行（验证：发送拒绝决策后工具调用计数不增加）
- [ ] 用户拒绝会生成 denied 工具结果并回灌（验证：会话中存在 denied tool_result）
- [ ] 工具失败会作为结构化结果回灌给模型（验证：Edit 多匹配或 Bash 非零退出后，最终回复请求包含错误结果）
- [ ] 会话保存和加载能保留工具调用与工具结果顺序（验证：保存后重新加载，消息顺序一致）

## TUI 行为
- [ ] 工具调用开始时显示 Claude Code 风格工具行（验证：TUI 中出现 `● Read(path)`）
- [ ] 工具执行中显示 running 状态（验证：fake 慢工具执行期间工具行显示进行中）
- [ ] 工具成功后显示简短摘要（验证：Read 成功后显示读取字节数或文件摘要）
- [ ] 工具失败后以可区分样式显示错误摘要（验证：Edit 多匹配失败后工具行显示错误）
- [ ] 工具被拒绝后显示 denied 状态（验证：拒绝 Bash 后显示用户已拒绝）
- [ ] 写文件工具执行前显示确认提示，并展示目标 path 和写入影响摘要（验证：Write 请求时界面提示 y/n，且可看到目标文件路径）
- [ ] 改文件工具执行前显示确认提示，并展示目标 path 和替换影响摘要（验证：Edit 请求时界面提示 y/n，且可看到目标文件路径和替换摘要）
- [ ] Bash 工具执行前显示确认提示和完整 command（验证：Bash 请求时界面展示完整命令）
- [ ] Bash 工具确认提示不声称提供系统级沙箱（验证：Bash 确认界面只说明将在项目根目录执行该命令，不暗示命令无法访问系统其他路径）
- [ ] 等待确认时按 `y` 允许执行（验证：按 y 后工具执行）
- [ ] 等待确认时按 `n` 拒绝执行（验证：按 n 后工具不执行）
- [ ] 等待确认时 Enter 不提交新输入（验证：等待确认按 Enter 不触发新模型请求）
- [ ] 等待确认时 q/ctrl+c 仍可退出（验证：等待确认状态下退出正常）
- [ ] 历史会话恢复时能展示工具行（验证：加载含工具调用的会话，工具行可见）
- [ ] 工具行、工具摘要和工具错误信息不显示 api_key 等敏感配置（验证：界面和保存的工具结果中搜索配置 api_key 字符串，不应出现）

## 编译与测试
- [ ] 工具模块测试通过（验证：运行 `go test ./internal/tool`）
- [ ] Provider 工具解析测试通过（验证：运行 `go test ./internal/provider`）
- [ ] 编排层测试通过（验证：运行 `go test ./internal/orchestrator`）
- [ ] 会话层测试通过（验证：运行 `go test ./internal/conversation`）
- [ ] TUI 层测试通过（验证：运行 `go test ./internal/tui ./internal/app`）
- [ ] 全量测试通过（验证：运行 `go test ./...`）
- [ ] 程序可编译（验证：运行 `go build ./cmd/xagent`）

## 端到端场景
- [ ] 场景 1：普通聊天 → 模型不调用工具 → 回复流式输出（验证：mock 普通响应，TUI 行为与旧版一致）
- [ ] 场景 2：用户问“读取某文件” → 模型调用 Read → TUI 显示 `● Read(path)` → 显示读取摘要 → 模型基于内容最终回复（验证：全过程无崩溃，结果正确）
- [ ] 场景 3：用户要求写文件 → 模型调用 Write → TUI 显示确认 → 用户按 y → 文件写入 → 模型最终回复（验证：文件内容正确）
- [ ] 场景 4：用户要求写文件 → 模型调用 Write → TUI 显示确认 → 用户按 n → 文件不写入 → 模型基于拒绝结果回复（验证：文件不存在或未改变）
- [ ] 场景 5：模型调用 Edit 且 old_text 多次匹配 → TUI 显示错误工具行 → 错误结果回灌 → 模型说明需要更精确原文（验证：结构化错误可观察）
- [ ] 场景 6：模型调用 Bash 执行超时命令 → TUI 显示 timeout → 模型基于 timeout 结果回复（验证：程序不中断）
- [ ] 场景 7：模型一次返回多个工具调用 → 系统不执行工具 → 回灌不支持多工具调用结果（验证：没有任何工具产生副作用）
- [ ] 场景 8：工具结果回灌后的最终回复再次请求工具 → 系统不执行第二轮工具 → 提示后续 Agent Loop 阶段支持（验证：工具调用计数不增加）
