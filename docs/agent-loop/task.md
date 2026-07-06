# XAgent Agent Loop Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 修改 | `internal/events/events.go` | 增加 AgentProgress、UsageUpdated、StopReason/Usage 事件载荷 |
| 修改 | `internal/tui/status.go` | 展示 Agent Loop 迭代进度、停止原因和可选 usage |
| 修改 | `internal/app/update.go` | 消费新增事件并更新状态栏 |
| 修改 | `internal/provider/openai.go` | 如可获得 usage，转换为统一 provider Usage 事件 |
| 修改 | `internal/provider/anthropic.go` | 如可获得 usage，转换为统一 provider Usage 事件 |
| 修改 | `internal/provider/provider.go` | 如需要，补充 usage stream event 语义 |
| 修改 | `internal/tool/registry.go` | 增加只读 registry 构建能力 |
| 修改 | `internal/orchestrator/chat.go` | 保留 Send 入口，改为调用 Agent Loop |
| 新建 | `internal/orchestrator/run_request.go` | RunMode、RunRequest、RunOptions、StopReason、前缀解析、mode system prompt |
| 新建 | `internal/orchestrator/stream_collector.go` | Provider 流双路收集，实时转发文本并收集完整响应 |
| 新建 | `internal/orchestrator/tool_batches.go` | 工具过滤、风险分批、批量执行、顺序恢复 |
| 新建 | `internal/orchestrator/agent_loop.go` | Agent Loop 主循环、停止条件、会话保存 |
| 修改 | `internal/orchestrator/chat_test.go` | 迁移既有测试，增加多轮 loop、停止条件和 mode 测试 |
| 修改 | `internal/provider/tool_parse_test.go` | 覆盖 usage 事件和多工具事件兼容性 |
| 修改 | `internal/tui/messages_test.go` | 如事件展示受影响，补进度/状态测试 |
| 新建 | `.claude/agent_loop_smoke.go` | Agent Loop smoke：多轮工具调用、计划模式、上限停止 |
| 新建 | `docs/agent-loop/checklist.md` | 后续验收清单 |

## T1: 扩展事件模型

**文件：** `internal/events/events.go`
**依赖：** 无

**步骤：**
1. 增加事件类型 `AgentProgress` 和 `UsageUpdated`。
2. 增加 `AgentProgress` 载荷，包含 `Iteration`、`Max`、`StopReason`、`Message`。
3. 增加 `UsageDisplay` 载荷，包含 `InputTokens`、`OutputTokens`。
4. 在 `Event` 中增加 `Progress *AgentProgress` 和 `Usage *UsageDisplay` 字段。
5. 保持现有事件类型和字段不变，避免破坏现有调用方。

**验证：** `go test ./internal/events ./internal/app ./internal/tui` 编译通过。

## T2: 扩展 TUI 状态栏

**文件：** `internal/tui/status.go`
**依赖：** T1

**步骤：**
1. 在 `Status` 增加 `AgentIteration`、`AgentMaxIterations`、`StopReason`、`StopMessage`、`InputTokens`、`OutputTokens` 字段。
2. `View()` 中在 streaming 或 waiting confirmation 之外显示 Agent Loop 进度，例如“第 3/10 轮”。
3. `View()` 中在停止后显示停止原因文案。
4. 如果 usage 非零，显示 token 用量。
5. 保持原有 provider/model/耗时/错误展示。

**验证：** 新增或更新 status 单元测试，运行 `go test ./internal/tui`。

## T3: App 消费新增事件

**文件：** `internal/app/update.go`
**依赖：** T1, T2

**步骤：**
1. 在事件处理 switch 中消费 `EventAgentProgress`，更新 `m.status.AgentIteration`、`AgentMaxIterations`、`StopReason`、`StopMessage`。
2. 消费 `EventUsageUpdated`，更新 `m.status.InputTokens` 和 `OutputTokens`。
3. 在用户提交新请求时清空上一轮 Agent 进度、停止原因和 usage。
4. Done/Error 时保持现有输入框恢复行为不变。
5. 等待危险工具确认时继续优先显示确认状态，不被普通进度文案覆盖。

**验证：** `go test ./internal/app ./internal/tui` 编译通过。

## T4: 补齐 Provider Usage 事件

**文件：** `internal/provider/provider.go`, `internal/provider/openai.go`, `internal/provider/anthropic.go`, `internal/provider/tool_parse_test.go`
**依赖：** 无

**步骤：**
1. 确认 `provider.StreamEvent` 的 `Usage *Usage` 字段语义：Provider 可在流中任意时刻发送 usage，缺省表示不可用。
2. 如果 OpenAI/Anthropic 当前 SDK 或 mock 响应中能读取 usage，转换为 `StreamEvent{Type: StreamEventDone 或 UsageUpdated 对应事件语义, Usage: ...}`；如果无法稳定获得，则保持缺省但补测试确认缺省不会报错。
3. 确保新增 usage 处理不影响现有 text/tool/error 流。
4. 补 provider 测试覆盖带 usage 的响应或缺省 usage 的兼容路径。

**验证：** `go test ./internal/provider`。

## T5: 增加只读 Registry 能力

**文件：** `internal/tool/registry.go`, `internal/tool/tool_test.go`
**依赖：** 无

**步骤：**
1. 增加方法或函数创建只读 registry，只包含 `Read`、`Glob`、`Grep`。
2. 保持完整 `NewRegistry(projectRoot)` 行为不变。
3. 增加测试确认只读 registry 包含 `Read/Glob/Grep`，不包含 `Write/Edit/Bash`。
4. 确认 Anthropic/OpenAI tool definitions 只输出只读工具。

**验证：** `go test ./internal/tool`。

## T6: 增加请求模式解析

**文件：** `internal/orchestrator/run_request.go`, `internal/orchestrator/chat_test.go`
**依赖：** 无

**步骤：**
1. 定义 `RunMode`、`RunRequest`、`RunOptions`、`StopReason` 常量。
2. 实现 `parseRunRequest(text string) (RunRequest, error)`。
3. 只识别行首 `/plan ` 和 `/do ` 前缀。
4. 前缀后内容为空返回与空输入一致的错误。
5. 实现默认 options：`MaxIterations=10`、`MaxUnknownToolCalls=2`。
6. 测试正文中出现 `/plan` 或 `/do` 不触发模式切换。

**验证：** 增加 parse 单元测试，运行 `go test ./internal/orchestrator -run TestParseRunRequest`。

## T7: 增加模式化 System Prompt 与 Registry 选择

**文件：** `internal/orchestrator/run_request.go`, `internal/orchestrator/chat.go`, `internal/orchestrator/chat_test.go`
**依赖：** T5, T6

**步骤：**
1. 实现 `systemPrompt(mode RunMode)`，在现有 system prompt 后追加 Default/Do 或 Plan 的模式约束。
2. 实现 `registryForMode(mode RunMode)`。
3. Plan mode 返回只读 registry；Default/Do 返回完整 registry。
4. 只读 registry 构建失败时返回错误，不回退全工具。
5. 修改 Provider 请求构造，使每轮请求使用 mode-specific prompt 和 registry。
6. 测试 mode 约束不写入 conversation history。

**验证：** 增加测试断言 `/plan` 请求不暴露 `Write/Edit/Bash` 工具定义，运行 `go test ./internal/orchestrator`。

## T8: 实现流式收集器

**文件：** `internal/orchestrator/stream_collector.go`, `internal/orchestrator/chat_test.go`
**依赖：** T1, T4, T6

**步骤：**
1. 定义 `StreamCollector` 结构体。
2. 实现 `collectProviderStream(ctx, stream, out)`。
3. TextDelta/ThinkingDelta 实时发出 events，同时写入 builder。
4. ToolCall 收集 `ToolCalls` 后返回给调用方。
5. Usage 转换成 `UsageUpdated` 事件。
6. Done 标记本轮完成。
7. Error 返回 `StopReasonProviderError` 和 error。
8. ctx 取消返回 `StopReasonCancelled`。
9. 测试 ToolCall 后 collector 不继续等待额外 Done，避免工具轮次卡住。

**验证：** 增加 fake stream 测试，确认文本实时转发且完整收集；运行 `go test ./internal/orchestrator -run TestCollectProviderStream`。

## T9: 实现工具模式过滤

**文件：** `internal/orchestrator/tool_batches.go`, `internal/orchestrator/chat_test.go`
**依赖：** T5, T6

**步骤：**
1. 实现 `filterToolCall(mode, call)`。
2. Plan mode 中允许 `Read/Glob/Grep`，拒绝其他工具。
3. 未知工具返回 `tool_not_found` 结果。
4. 被拒绝的工具结果使用原 callID/name，能作为 tool_result 回写。
5. 增加测试覆盖 plan mode 拒绝 `Write/Edit/Bash`，default/do 允许交给 executor。
6. 增加测试确认 unknown tool 结果可回写且计入未知工具轮次。

**验证：** `go test ./internal/orchestrator -run TestFilterToolCall`。

## T10: 实现工具风险分批

**文件：** `internal/orchestrator/tool_batches.go`, `internal/orchestrator/chat_test.go`
**依赖：** T9

**步骤：**
1. 定义 `ToolBatch` 和 `ToolExecution`。
2. 实现 `makeToolBatches(calls)`。
3. 连续 safe 工具合并为并发批次。
4. dangerous 工具单独成串行批次。
5. 混合顺序保持：safe 不能越过前面的 dangerous。
6. 被过滤/拒绝的工具保留原始 index，不破坏结果排序。
7. 增加测试覆盖 safe+safe、dangerous+safe、safe+dangerous+safe、blocked+safe。

**验证：** `go test ./internal/orchestrator -run TestMakeToolBatches`。

## T11: 实现批量工具执行

**文件：** `internal/orchestrator/tool_batches.go`, `internal/orchestrator/chat_test.go`
**依赖：** T9, T10

**步骤：**
1. 实现 `executeToolBatches(ctx, mode, batches, out)`。
2. 并发批次中每个工具发 pending/running/result 事件。
3. 串行批次按顺序执行。
4. 并发批次等待全部结果后，按原始 index 排序返回。
5. ctx 取消后不启动新批次。
6. 复用现有危险工具确认逻辑；确认中 ctx 取消时返回停止信号。
7. 确认危险工具不会被放进并发批次。
8. 增加测试覆盖并发 safe 工具结果顺序、串行 dangerous 工具顺序、确认取消。

**验证：** `go test ./internal/orchestrator -run TestExecuteToolBatches`。

## T12: 迁移现有单工具测试到 Agent Loop

**文件：** `internal/orchestrator/chat_test.go`
**依赖：** T8-T11

**步骤：**
1. 更新现有“执行 Read 后请求最终回复”测试，适配多轮 loop 而不是旧 finalReply 路径。
2. 更新“最终回复再次请求工具不递归”测试，改为验证 loop 可以继续工具调用直到上限或完成。
3. 保留“多工具每个 call 都有 result”的协议顺序断言。
4. 确认旧测试不是简单删除，而是迁移到新的 Agent Loop 语义。

**验证：** `go test ./internal/orchestrator`。

## T13: 实现 Agent Loop 主循环

**文件：** `internal/orchestrator/agent_loop.go`, `internal/orchestrator/chat.go`
**依赖：** T6, T7, T8, T9, T10, T11, T12

**步骤：**
1. 修改 `Send`：解析请求、追加清理后的用户消息、启动 goroutine 调用 `runAgentLoop`。
2. 实现 `runAgentLoop` 迭代 1..MaxIterations。
3. 每轮发 `AgentProgress`。
4. 每轮调用 Provider 并收集 stream。
5. 保存 assistant/thinking 文本，确保先文本后工具的顺序正确。
6. 没有工具调用时保存会话，发 completed progress 和 Done。
7. 有工具调用时过滤、分批执行、按顺序 append tool_call/tool_result。
8. Provider error、ctx cancel、未知工具连续上限、迭代上限时保存已完成会话并停止。
9. 删除或停用旧的“一次工具后 finalReply”递归逻辑，避免双路径冲突。
10. 所有停止路径都调用 store.Save，除非 store 本身返回错误。

**验证：** `go test ./internal/orchestrator`。

## T14: 覆盖停止条件测试

**文件：** `internal/orchestrator/chat_test.go`
**依赖：** T13

**步骤：**
1. 测试模型无工具时正常 completed。
2. 测试达到 MaxIterations 时停止并发出 max_iterations。
3. 测试 Provider error 停止并保存已有 assistant 文本。
4. 测试 ctx cancel 不启动新工具。
5. 测试连续未知工具达到 2 轮后停止。
6. 测试停止时发出带 StopReason 的 AgentProgress。

**验证：** `go test ./internal/orchestrator -run 'TestAgentLoop.*Stop|TestAgentLoop.*Unknown'`。

## T15: 覆盖多轮和多工具测试

**文件：** `internal/orchestrator/chat_test.go`
**依赖：** T13

**步骤：**
1. 测试 Read → Grep → 最终回复的多轮 ReAct。
2. 测试一轮返回多个 safe 工具时都执行并回写。
3. 测试 safe 并发结果按原始顺序写入会话。
4. 测试 dangerous 工具串行，不与 safe 越序。
5. 测试工具失败后下一轮模型能继续。
6. 测试一轮先输出 assistant 文本再请求工具时，历史顺序为 assistant → tool_call → tool_result。
7. 测试 OpenAI/Anthropic message conversion 接收新历史顺序不报格式错误。

**验证：** `go test ./internal/orchestrator ./internal/provider -run 'TestAgentLoop.*Tool|TestAgentLoop.*Multi|Test.*Tool.*Message'`。

## T16: 覆盖 Plan Mode 测试

**文件：** `internal/orchestrator/chat_test.go`
**依赖：** T13

**步骤：**
1. 测试 `/plan` 去掉前缀后写入用户消息。
2. 测试 `/plan` 只暴露读类工具。
3. 测试 `/plan` 请求 `Write` 时不执行并回写拒绝结果。
4. 测试 `/do` 使用全量工具。
5. 测试普通消息不继承上一次 `/plan` 模式。
6. 测试 `/plan` 不产生文件写入或命令执行副作用。

**验证：** `go test ./internal/orchestrator -run 'TestPlanMode|TestDoMode'`。

## T17: 更新 TUI/App 事件测试

**文件：** `internal/tui/messages_test.go`, `internal/tui/status.go`, `internal/app/update.go`
**依赖：** T1, T2, T3, T13

**步骤：**
1. 补状态栏显示迭代进度测试。
2. 补状态栏显示停止原因测试。
3. 补 usage 展示测试。
4. 确认工具行实时顺序不因 loop 多轮回退。
5. 确认等待危险工具确认时，状态栏仍显示等待确认而不是普通迭代进度。

**验证：** `go test ./internal/tui ./internal/app`。

## T18: 新增 Agent Loop smoke

**文件：** `.claude/agent_loop_smoke.go`
**依赖：** T13-T17

**步骤：**
1. 使用 fake provider 模拟多轮：第一轮 Read，第二轮 Grep，第三轮最终回复。
2. 验证事件中出现多轮 progress、工具结果和最终 Done。
3. 模拟 `/plan` 请求写工具，验证未产生文件写入。
4. 模拟 max iteration，验证停止原因。
5. 模拟一轮多个 safe 工具，验证都执行且结果顺序稳定。
6. 输出 `AGENT_LOOP_SMOKE_OK`。

**验证：** `go run .claude/agent_loop_smoke.go` 输出 `AGENT_LOOP_SMOKE_OK`。

## T19: 全量回归

**文件：** 全项目
**依赖：** T1-T18

**步骤：**
1. 运行格式化。
2. 运行 `go test ./...`。
3. 运行 `go build ./cmd/xagent`。
4. 运行既有 `.claude/tool_smoke.go`。
5. 运行新增 `.claude/agent_loop_smoke.go`。
6. 查看 `git status --short`，确认没有误加入 `.claude/worktrees/`。

**验证：** 所有命令通过，无测试失败，未跟踪 worktree 未被纳入提交范围。

## 执行顺序

```text
T1 → T2 → T3
T4 ─────────┐
T5 → T7 ────┤
T6 ─────────┤
             └→ T8 → T9 → T10 → T11 → T12 → T13
                                                ↘
                                                 T14 → T15 → T16 → T17 → T18 → T19
```

T1-T6 可部分并行；T13 是集成点，必须等事件、Provider usage、请求解析、registry、collector、批处理和旧测试迁移都完成后再做。
