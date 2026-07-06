# XAgent Agent Loop Checklist

> 每一项都通过运行代码或观察行为验证，聚焦系统行为。

## 实现完整性

- [ ] Agent Loop 可以在一次用户请求内进行多轮模型请求和工具调用（验证：运行 Agent Loop 单元测试，观察 Read → Grep → 最终回复在一次 Send 中完成）。
- [ ] 模型不再请求工具时循环正常结束，停止原因是 `completed`（验证：无工具 fake provider 测试通过）。
- [ ] 达到最大迭代次数时停止继续请求模型，停止原因是 `max_iterations`（验证：max iteration 单元测试通过）。
- [ ] 上下文取消后不再启动新的模型请求或工具执行（验证：cancel 单元测试通过）。
- [ ] 连续未知工具达到限制后停止，停止原因是 `unknown_tool`（验证：unknown tool 单元测试通过）。
- [ ] Provider 流错误会停止循环并展示错误（验证：provider error 单元测试通过）。
- [ ] Provider usage 可用时通过事件流输出，不可用时循环仍正常完成（验证：usage provider 测试和 collector 测试通过）。
- [ ] `/plan` 输入只影响当前请求，不创建持久模式（验证：`/plan` 后发送普通消息，普通消息使用默认工具集）。
- [ ] `/do` 输入使用全量工具执行当前请求（验证：`/do` 单元测试中 Write/Edit/Bash 工具可被暴露或执行）。
- [ ] `/plan` 和 `/do` 前缀不会原样进入模型用户内容（验证：fake provider 捕获到的 user message 不包含前缀）。

## 工具执行与回写

- [ ] 一轮返回多个 safe 工具调用时，所有工具都执行（验证：multi-safe 单元测试通过）。
- [ ] 多个 safe 工具可以并发执行，但结果按模型原始顺序写入会话（验证：并发顺序测试通过）。
- [ ] dangerous 工具按模型原始顺序串行执行（验证：dangerous serial 测试通过）。
- [ ] safe 工具不会越过前面的 dangerous 工具提前执行（验证：mixed batch 测试通过）。
- [ ] Plan Mode 中 `Write`、`Edit`、`Bash` 不会执行，而是作为工具结果回写拒绝原因（验证：plan blocked tool 测试通过）。
- [ ] 未知工具不会执行，会回写 `tool_not_found` 结构化结果（验证：unknown tool result 测试通过）。
- [ ] 工具失败不会让进程崩溃，失败结果会回写给模型并允许下一轮继续（验证：tool failure recovery 测试通过）。
- [ ] 每个 tool_call 都有对应 tool_result，OpenAI/Anthropic 历史转换保持合法（验证：provider message conversion 测试通过）。
- [ ] 一轮先输出 assistant 文本再请求工具时，会话历史顺序为 assistant → tool_call → tool_result（验证：history ordering 测试通过）。

## 事件流与 TUI

- [ ] 文本增量实时推给 UI，同时完整 assistant 文本保存进会话历史（验证：stream collector 测试通过）。
- [ ] thinking 增量在启用显示时实时推给 UI，并按现有规则保存或忽略（验证：thinking collector 测试通过）。
- [ ] 每轮循环都会发出进度事件，包含当前轮次和最大轮次（验证：progress event 测试通过）。
- [ ] 停止时会发出包含 StopReason 的进度事件（验证：stop reason event 测试通过）。
- [ ] TUI 状态栏能展示 Agent Loop 当前轮次，例如“第 3/10 轮”（验证：status View 测试通过）。
- [ ] TUI 状态栏能展示停止原因，例如“达到迭代上限”或“未知工具过多”（验证：status stop reason 测试通过）。
- [ ] TUI 能展示 token usage；usage 缺省时不显示且不报错（验证：status usage 测试通过）。
- [ ] 等待危险工具确认时，状态栏优先显示等待确认，不被普通进度覆盖（验证：confirmation priority 测试通过）。
- [ ] 多轮工具行实时顺序保持正确，不会漂到最终回复之后（验证：messages timeline 测试通过）。

## Plan Mode

- [ ] `/plan <任务>` 只向模型暴露 `Read`、`Glob`、`Grep`（验证：fake provider 捕获 tools，确认没有 `Write/Edit/Bash`）。
- [ ] `/plan` 中模型请求写文件时不会产生文件写入（验证：plan write blocked 测试检查目标文件不存在或内容未变）。
- [ ] `/plan` 中模型请求执行命令时不会启动 Bash（验证：plan bash blocked 测试检查 fake command 未执行）。
- [ ] `/plan` 可以使用读类工具多轮观察项目并最终输出计划文本（验证：plan loop smoke 通过）。
- [ ] `/do <任务>` 可以使用全量工具执行任务（验证：do mode smoke 或单元测试通过）。
- [ ] `/plan` 产出的计划作为普通会话内容保留，不要求独立结构化计划对象（验证：会话历史中存在 assistant 计划文本）。

## 停止条件

- [ ] 正常完成时发送 Done，输入框恢复可用（验证：app/update 集成测试或 smoke 观察）。
- [ ] 达到迭代上限时不再请求模型，不再启动工具（验证：fake provider 调用次数等于上限）。
- [ ] ctx cancel 后不启动下一批工具（验证：cancel 测试观察 fake executor 未被调用）。
- [ ] Provider error 后保存已完成 assistant 文本和工具结果（验证：provider error 测试检查 conversation messages）。
- [ ] 连续未知工具停止前，未知工具结果已回写给模型至少一次（验证：unknown tool 测试检查 tool_result）。

## 编译与测试

- [ ] 项目全部 Go 测试通过（验证：运行 `go test ./...`）。
- [ ] CLI 入口构建通过（验证：运行 `go build ./cmd/xagent`）。
- [ ] 既有工具系统 smoke 通过（验证：运行 `go run .claude/tool_smoke.go`，输出 `TOOL_SMOKE_OK`）。
- [ ] 新增 Agent Loop smoke 通过（验证：运行 `go run .claude/agent_loop_smoke.go`，输出 `AGENT_LOOP_SMOKE_OK`）。
- [ ] diff 没有格式或空白错误（验证：运行 `git diff --check`）。
- [ ] 多 agent worktree 临时目录未被纳入提交（验证：运行 `git status --short`，不暂存 `.claude/worktrees/`）。

## 端到端场景

- [ ] 场景 1：用户请求“读取 note.txt 后搜索关键词并总结” → XAgent 在一次请求内完成 Read、Grep 和最终总结（验证：Agent Loop smoke 或 tmux 场景观察到多轮 progress、工具行和最终回复）。
- [ ] 场景 2：用户输入 `/plan 修改配置加载逻辑` → XAgent 只读取/搜索项目并输出计划，不写文件、不执行命令（验证：目标文件未变化，工具行只出现 Read/Glob/Grep 或 blocked 结果）。
- [ ] 场景 3：用户输入 `/do 根据计划修改文件` → XAgent 可以使用全量工具执行，包括写入或编辑项目内文件（验证：目标文件内容按预期变化，工具行显示执行结果）。
- [ ] 场景 4：模型持续请求工具超过上限 → XAgent 停止并显示“达到迭代上限”（验证：fake provider smoke 触发 max_iterations）。
- [ ] 场景 5：模型连续请求未知工具 → XAgent 回写未知工具结果后停止并显示“未知工具过多”（验证：unknown tool smoke 或单元测试）。
