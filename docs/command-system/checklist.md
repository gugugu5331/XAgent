# Command System Checklist

> 每一项都通过运行命令或观察实际行为验证。只有记录实际结果和证据后才能勾选。

## 注册、解析与分发

- [x] C1 / AC1：默认注册中心恰好包含十个公开命令，元数据含名称、批准的别名、描述、用法、类型、参数提示、隐藏状态和处理行为，并覆盖 Local/UI/Prompt 三类。（验证：运行 `go test ./internal/command -run 'Test.*Builtin|TestRegistry' -v`，观察公开数量、字段和类型断言通过。）

- [x] C2 / AC2：名称—名称、名称—别名、别名—别名、定义内重复和仅大小写不同的冲突都会在构造时失败，强制构造会 panic 且错误指出冲突双方。（验证：运行 `go test ./internal/command -run 'TestRegistry.*Conflict|TestMustNew' -v`，观察每种冲突用例通过。）

- [x] C3 / AC3：空输入和纯空白不执行；命令名称大小写不敏感；参数保留原始大小写与内部空格。（验证：运行 `go test ./internal/command -run TestDispatch -v`，观察 empty、mixed-case、space/tab/newline 和 argument preservation 用例通过。）

- [x] C4 / AC4：未知 `/unknown` 只显示本地错误和 `/help` 引导，不产生用户消息或 Provider 请求。（验证：运行 `go test ./internal/app -run 'Test.*UnknownCommand' -v`，观察 Provider 调用数为 0 且 Notice/Error 含 `/help`。）

- [x] C5 / AC5：普通非斜杠文本仍通过现有 Agent 路径发送并进入流式响应。（验证：运行 `go test ./internal/app -run 'Test.*Plain.*Input|Test.*CommandDispatch' -v`，观察普通输入产生一次请求和用户事件。）

- [x] C6 / AC6：使用内存替代 Controller、不启动真实 TUI 或 Provider，也能执行所有内置命令并观察调用。（验证：运行 `go test ./internal/command -run TestBuiltin -v`，观察 fake Controller 调用断言全部通过。）

## 帮助与 Tab 补全

- [x] C7 / AC7：唯一前缀或精确别名按 Tab 后直接变成规范命令名，但不执行命令。（验证：运行 `go test ./internal/app -run 'TestCommandCompletion.*Single|TestCommandCompletion.*Exact' -v`，观察输入写回且 Provider/Handler 调用数为 0。）

- [x] C8 / AC8：多个可见匹配项按稳定顺序弹出候选菜单，可用 Up/Down 选择，并由 Enter 写回输入而不立即执行。（验证：运行 `go test ./internal/tui -run TestCommandMenu -v` 和 `go test ./internal/app -run 'TestCommandCompletion.*Multiple' -v`，观察菜单和二次 Enter 语义通过。）

- [x] C9 / AC9：`/permissions`、`/mcp`、`/diagnostics` 等隐藏兼容入口不出现在单匹配、多匹配或菜单中。（验证：运行 `go test ./internal/command -run 'TestComplete.*Hidden' -v`，观察隐藏项断言通过。）

- [x] C10 / AC10：`/help`、`/h`、`/?` 都显示十个公开命令的名称、别名、描述、用法和参数提示，且不显示隐藏入口。（验证：运行 `go test ./internal/command -run 'TestBuiltin.*Help' -v`，比较三种输入输出并检查隐藏名称缺席。）

- [x] C11：补全只处理命令名；输入已经进入参数区时按 Tab 不提供参数候选。（验证：运行 `go test ./internal/command -run 'TestComplete.*Argument' -v`，观察候选为空。）

## 十个公开命令

- [x] C12 / AC11：有活动会话时 `/compact` 与 `/ctx` 显示实际压缩结果；无活动会话时显示本地提示，不 panic。（验证：运行 `go test ./internal/app -run 'Test.*Compact.*Command' -v`，观察有/无会话分支通过。）

- [x] C13 / AC12：`/session` 与 `/sess` 显示真实会话 ID、消息数量、当前模式和 streaming 状态，且不请求 Provider。（验证：运行 `go test ./internal/app -run 'Test.*Session.*Command' -v`，核对快照字段和 Provider 计数。）

- [x] C14 / AC13：`/memory` 与 `/mem` 显示 user/project 启用状态、索引数量和脱敏诊断，不展示记忆正文。（验证：运行 `go test ./internal/app -run 'Test.*Memory.*Command' -v`，观察两级数量、诊断和敏感值断言通过。）

- [x] C15 / AC14：`/permission` 与 `/perm` 只显示权限模式、各层规则数量和加载错误，执行前后权限状态不变。（验证：运行 `go test ./internal/app -run 'Test.*Permission.*Command' -v`，比较执行前后状态。）

- [x] C16 / AC15：`/status` 与 `/st` 显示 Provider、模型、模式、Token、Cache、streaming、MCP 和最近错误，字段与当前 Model 一致。（验证：运行 `go test ./internal/app -run 'Test.*Status.*Command' -v`，观察完整字段断言通过。）

- [x] C17 / AC16：`/clear` 与 `/cls` 只清空消息显示；Conversation 消息和 Agent 上下文不变；执行 Compact 不会把隐藏历史重新显示；重新加载会话后历史恢复可见。（验证：运行 `go test ./internal/tui -run TestMessagesViewClear -v` 与 `go test ./internal/app -run 'TestClearCommand' -v`。）

- [x] C18 / AC17：`/plan` 与 `/p` 立即显示 `[PLAN]`，连续多条普通请求都使用 Plan 模式，直到显式切换。（验证：运行 `go test ./internal/app -run 'TestCommandMode.*Plan' -v`，观察连续请求捕获模式均为 Plan。）

- [x] C19 / AC18：`/do` 与 `/d` 立即恢复 `[DEFAULT]`，后续普通请求使用 Default 模式。（验证：运行 `go test ./internal/app -run 'TestCommandMode.*Do|TestCommandMode.*Default' -v`。）

- [x] C20 / AC19：Plan 状态不持久化；创建、加载其他会话或重新构造 App 后都恢复 `[DEFAULT]`。（验证：运行 `go test ./internal/app -run 'TestCommandMode.*Reset' -v`，观察三种生命周期用例通过。）

- [x] C21 / AC21：`/review` 与 `/rv` 展开完整固定提示，作为用户消息显示并保存；提示要求审查未提交改动、按严重度报告证据且不修改文件。（验证：运行 `go test ./internal/app -run TestReviewCommand -v`，核对 UI、Conversation 和 Provider 捕获文本完全一致。）

## 模式与 Agent 集成

- [x] C22 / AC20：明确 Plan 模式在首轮、工具结果后回复及所有后续 Agent Loop iteration 中保持只读；写入类工具不暴露或被拒绝。（验证：运行 `go test ./internal/orchestrator -run 'Test.*PlanMode|Test.*SendWithMode' -v`，观察所有捕获请求 mode 与工具集合一致。）

- [x] C23：Default 模式恢复完整工具集合，现有旧式 `Send("/plan ...")` 与 `Send("/do ...")` 内部入口仍通过原测试。（验证：运行 `go test ./internal/orchestrator -run 'Test.*SendWithMode|TestParseRunRequest' -v`。）

- [x] C24：普通输入与 `/review` 共用请求生命周期，包括输入清空、RequestSession、streaming、取消、usage 清零和历史保存。（验证：运行 `go test ./internal/app -run 'TestReviewCommand|TestModelTracksRequestSessionLifecycle|TestUsage' -v`。）

- [x] C25 / AC24：帮助、补全、清屏、模式切换、Session/Memory/Permission/Status、未知命令及隐藏兼容命令全部保持 Provider 调用数为 0；只有普通文本、`/review` 和 Compact 自身需要的摘要请求可以调用 Provider。（验证：运行 `go test ./internal/app -run 'TestLocalCommandsDoNotCallProvider|TestCommandCompletion' -v`，核对计数。）

## 兼容与安全

- [x] C26 / AC22：`/permissions status`、`/mcp status`、`/diagnostics`、`/memory status|index|off|delete|rebuild` 保持改造前的输出和状态变化。（验证：运行 `go test ./internal/app -run 'TestPermissionsStatusCommand|TestMCPStatusCommand|TestDiagnosticsCommand|TestMemoryCommands' -v`。）

- [x] C27 / AC23：向会话、记忆、诊断、MCP、路径和最近错误注入模拟 API Key、Token 与密码后，所有公开及隐藏命令输出都不包含原始值。（验证：运行 `go test ./internal/app -run 'Test.*Redact|Test.*Sensitive' -v`，观察敏感 canary 全部缺席。）

- [x] C28：无参数命令收到多余参数时显示对应 Usage，不静默忽略，也不把输入发送给 Agent。（验证：运行 `go test ./internal/command -run 'TestBuiltin.*Usage' -v`，观察所有公开无参数命令的错误断言通过。）

- [x] C29：帮助、补全和注册顺序在重复构造与重复执行中保持一致。（验证：运行 `go test ./internal/command -count=20`，期望所有重复运行通过且无顺序波动。）

## 构建与自动化质量

- [x] C30：所有新增和修改 Go 文件符合 gofmt，且差异中没有空白错误。（验证：运行 `gofmt -d internal/command internal/app internal/tui internal/orchestrator`，期望无输出；运行 `git diff --check`，期望退出码 0。）

- [x] C31：命令、TUI、Orchestrator 与 App 定向测试全部通过。（验证：运行 `go test ./internal/command ./internal/tui ./internal/orchestrator ./internal/app`，期望退出码 0。）

- [x] C32 / AC25：全项目测试通过，没有会话、权限、记忆、上下文、Provider、工具或 TUI 回归。（验证：运行 `go test ./...`，期望退出码 0。）

- [x] C33 / AC25：全项目静态检查通过。（验证：运行 `go vet ./...`，期望退出码 0。）

- [x] C34：当前源码可以构建独立可执行文件，二进制对应当前工作区而非旧产物。（验证：运行 `go build -o /private/tmp/xagent-command-system ./cmd/xagent`，再运行 `go version -m /private/tmp/xagent-command-system`，观察模块路径为 `xagent/cmd/xagent` 且构建成功。）

- [x] C35：最终差异只包含批准的命令系统代码、测试、README 和四份文档，不包含本任务造成的范围外改动。（验证：运行 `git status --short` 和 `git diff --stat`，把本任务文件与 task.md 文件清单逐项对照；用户原有脏文件单独标记但不修改。）

## 端到端场景

- [x] C36：在 tmux 中启动固定 mock Provider 和当前源码构建的 XAgent，应用进入新会话且状态栏初始显示 `[DEFAULT]`。（验证：一个 tmux pane 运行 `go run .claude/mock_openai.go`，另一个 pane 运行 `/private/tmp/xagent-command-system -config .claude/mock-config.yaml`；使用 `tmux capture-pane -p` 记录启动画面。）

- [x] C37：真实 TUI 中输入 `/help`，立即显示十个公开命令；输入 `/unknown`，显示 `/help` 引导；两次操作都没有出现“正在响应”。（验证：使用 `tmux send-keys` 输入并用 `tmux capture-pane -p` 保存实际画面。）

- [x] C38：真实 TUI 中输入 `/c` 后按 Tab，出现 `/clear` 与 `/compact` 候选；选择 `/clear` 后第一次 Enter 只完成输入，第二次 Enter 才清屏。（验证：分阶段 capture pane，对比候选、输入框和清屏结果。）

- [x] C39：真实 TUI 中执行 `/plan` 后状态栏显示 `[PLAN]`，执行 `/status` 显示 mode=plan；执行 `/do` 后恢复 `[DEFAULT]`。（验证：每一步使用 `tmux capture-pane -p` 记录模式标记和状态摘要。）

- [x] C40：真实 TUI 中执行 `/review`，消息区显示展开后的完整审查提示而不是 `/review` 缩写，mock Provider 返回流式回复，会话未发生工具写入。（验证：等待流结束后 capture pane，并检查项目 `git status --short` 没有因 review 新增实现改动。）

- [x] C41：端到端清理完成，不遗留 mock server、tmux session 或临时会话进程。（验证：终止专用 tmux session，确认 `tmux list-sessions` 不含测试会话；临时二进制保留在 `/private/tmp` 由系统清理。）

## 验收记录

验收时间：2026-07-21（Asia/Shanghai）。

- C1–C11：`go test ./internal/command` 通过。注册中心测试覆盖五类冲突、构造期 panic、不可变副本和稳定排序；解析测试覆盖空输入、大小写、Unicode 空白、参数保真、未知命令与 Handler 错误；帮助和补全测试覆盖十个公开命令的完整元数据、三种类型、全部别名、隐藏过滤、精确优先和参数区禁用。
- C12–C29：`go test ./internal/app ./internal/tui ./internal/orchestrator` 通过。App 测试使用计数 Provider 验证本地命令零请求、普通输入与 review 各一次请求、active/missing conversation 的 compact、状态快照、Memory 双层索引数量、Permission 只读、清屏与重载、Plan 连续请求、Do 恢复、创建/加载/重启模式重置、Plan 中 review、单/多候选与菜单关闭路径；既有隐藏命令与脱敏回归测试继续通过。Orchestrator 测试验证 Plan 首轮、工具结果和多 iteration 都保持只读约束。
- C29 的确定性复验：`go test ./internal/command -count=20` 通过（`ok xagent/internal/command 0.611s`）。
- C30：`gofmt -d internal/command internal/app internal/tui internal/orchestrator` 无输出；`git diff --check` 退出码 0。
- C31–C34：四包定向测试通过；`go test ./...` 全部通过；`go vet ./...` 退出码 0；`go build -o /private/tmp/xagent-command-system ./cmd/xagent` 成功，`go version -m` 显示模块路径 `xagent/cmd/xagent`、Go `1.26.4` 和当前 dirty 工作区构建信息。
- C35：`git status --short` 与 task.md 文件清单核对完成。本任务只修改 README、批准文档及列出的 command/app/orchestrator/tui 文件；原有 `.claude/worktrees`、`.mewcode`、`fakeprovider` 和 webarchive 脏项未被改动。端到端测试产生的单个新会话文件已精确删除。
- C36–C40：使用 `/private/tmp/xagent-command-system`、本地 mock Provider 和隔离的临时配置在 tmux 会话 `xagent-command-e2e` 实测。启动画面显示 `[DEFAULT]`；`/help` 显示恰好十个公开命令；`/unknown` 显示 `/help` 引导；`/c` + Tab 显示按 `clear, compact` 排序的菜单，第一次 Enter 仅写回 `/clear`、第二次 Enter 才执行；`/plan`、`/status`、`/do` 依次观察到 `[PLAN]`、`mode=plan`、`[DEFAULT]`；`/review` 显示完整展开提示且 mock 流式回复为“你好，这是流式回复。”。
- C41：已终止 `xagent-command-e2e`；随后 `tmux list-sessions` 不含该会话，端口 `18080` 无监听进程。测试产生的工作区会话文件已删除；验收二进制与临时配置仅保留在 `/private/tmp`。
