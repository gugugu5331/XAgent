# Command System Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|---|---|---|
| 新建 | `internal/command/definition.go` | 命令类型、定义、调用、分发结果与状态快照 |
| 新建 | `internal/command/controller.go` | 渲染无关的 Controller 接口 |
| 新建 | `internal/command/registry.go` | 注册、规范化、冲突检测和可见定义 |
| 新建 | `internal/command/parser.go` | 输入分类、解析、查找与分发 |
| 新建 | `internal/command/completion.go` | 命令名与别名补全 |
| 新建 | `internal/command/builtins.go` | 十个公开命令、隐藏兼容入口与 review 提示 |
| 新建 | `internal/command/registry_test.go` | 注册合法性与冲突测试 |
| 新建 | `internal/command/parser_test.go` | 解析、参数保真和未知命令测试 |
| 新建 | `internal/command/completion_test.go` | 补全、去重、隐藏和排序测试 |
| 新建 | `internal/command/builtins_test.go` | 使用替代 Controller 验证内置命令 |
| 修改 | `internal/orchestrator/chat.go` | 增加明确模式发送入口 |
| 修改 | `internal/orchestrator/chat_test.go` | 验证明确定义模式与旧入口兼容 |
| 修改 | `internal/tui/input.go` | 增加补全写回能力 |
| 新建 | `internal/tui/command_menu.go` | 候选菜单状态与渲染 |
| 新建 | `internal/tui/command_menu_test.go` | 候选菜单行为测试 |
| 修改 | `internal/tui/messages.go` | 增加只清空显示的 Clear |
| 修改 | `internal/tui/messages_test.go` | 验证清屏不修改源会话数据 |
| 修改 | `internal/tui/status.go` | 增加 DEFAULT/PLAN 模式标记 |
| 新建 | `internal/tui/status_test.go` | 模式状态栏测试 |
| 修改 | `internal/app/deps.go` | 注入可选命令注册中心 |
| 修改 | `internal/app/app.go` | 保存注册中心、模式、菜单、最近错误和清屏状态 |
| 新建 | `internal/app/command_controller.go` | 命令 Controller 的 App 适配器 |
| 新建 | `internal/app/commands.go` | 输入分发、统一提交与补全辅助 |
| 修改 | `internal/app/update.go` | 统一 Enter/Tab/菜单键盘路径，移除命令旁路 |
| 新建 | `internal/app/commands_test.go` | 命令、模式、补全、脱敏和 Provider 计数测试 |
| 修改 | `internal/app/update_test.go` | 调整并保留旧命令回归测试 |
| 修改 | `README.md` | 记录十个公开命令、别名与模式语义 |

## T1：定义命令核心类型

**文件：** `internal/command/definition.go`、`internal/command/controller.go`  
**依赖：** 无  
**覆盖：** F1、F7、F8

**步骤：**

1. 定义 `TypeLocal`、`TypeUI`、`TypePrompt` 和 `ModeDefault`、`ModePlan`。
2. 定义 `Definition`、`Invocation`、`ExecutionContext`、`Handler`、`DispatchKind`、`DispatchResult` 和 `Suggestion`。
3. 定义 Session、Memory、Permission、Token、Runtime 状态快照。
4. 按 plan.md 的完整方法集定义 `Controller` 接口，保持该包只依赖标准库。

**验证：** 运行 `go test ./internal/command`，期望新包编译通过且无循环依赖。

## T2：实现注册中心构造与规范化

**文件：** `internal/command/registry.go`  
**依赖：** T1  
**覆盖：** F1、F2、N1、N2、N10

**步骤：**

1. 实现名称规范化：去除可选 `/`、清理首尾空白、转小写。
2. 在构造时深拷贝 Definition 与 Aliases。
3. 校验空名称、无效类型、空 Handler、空别名和定义内重复别名。
4. 检测名称—名称、名称—别名和别名—别名冲突，错误包含冲突键和两条定义。
5. 按规范名称稳定排序定义，建立名称/别名到定义索引。
6. 实现 `New`、`MustNew` 和返回副本的 `Visible`。

**验证：** 运行 `go test ./internal/command`，期望包编译通过；用一个临时单测或现有测试入口确认 `MustNew` 对冲突 panic。

## T3：覆盖注册中心冲突与不可变性

**文件：** `internal/command/registry_test.go`  
**依赖：** T2  
**覆盖：** AC1、AC2、N1、N2

**步骤：**

1. 测试合法定义的元数据、三种类型和可见排序。
2. 分别测试名称—名称、名称—别名、别名—别名、定义内重复和大小写归一冲突。
3. 测试无效名称、别名、类型和 Handler 会返回错误。
4. 修改构造入参和 `Visible` 返回值，确认注册中心内部数据不变。
5. 测试 `MustNew` panic 文本包含冲突名称和定义来源。

**验证：** 运行 `go test ./internal/command -run 'TestRegistry|TestMustNew'`，期望全部通过。

## T4：实现输入解析与统一分发

**文件：** `internal/command/parser.go`  
**依赖：** T1、T2  
**覆盖：** F3–F6、F20

**步骤：**

1. 将空输入、普通文本和斜杠输入分类。
2. 用第一个 Unicode 空白拆分命令名和参数，保留参数大小写与内部空格。
3. 通过索引解析规范名称或别名，并构造 Invocation。
4. 对未知斜杠命令调用 `DisplayError`，返回 `DispatchUnknown`。
5. 对已知命令构造包含 Registry 和 Controller 的 ExecutionContext，执行 Handler。
6. Handler 错误交给 `DisplayError` 并保留在 DispatchResult，绝不回落为普通文本。

**验证：** 运行 `go test ./internal/command`，期望解析器编译通过。

## T5：覆盖解析、参数保真与未知命令

**文件：** `internal/command/parser_test.go`  
**依赖：** T4  
**覆盖：** AC3–AC6、AC24

**步骤：**

1. 测试空字符串和纯空白返回 `DispatchEmpty`。
2. 测试普通文本返回 Trim 后的 `DispatchPlainText`。
3. 测试大小写不同的规范名和别名均命中同一定义。
4. 测试空格、Tab 和换行边界，断言参数原始大小写与内部空格保留。
5. 测试未知斜杠命令只显示 `/help` 引导，不调用 Handler 或 SendUserMessage。
6. 测试 Handler 错误被显示且仍属于命令结果。

**验证：** 运行 `go test ./internal/command -run TestDispatch`，期望全部通过。

## T6：实现命令补全查询

**文件：** `internal/command/completion.go`  
**依赖：** T2  
**覆盖：** F9、F10、N1、N2、N9

**步骤：**

1. 仅接受以 `/` 开头且未进入参数区的补全输入。
2. 先检查规范名称或别名精确命中；命中时只返回该规范命令。
3. 未精确命中时，对规范名称和别名做大小写不敏感前缀匹配。
4. 以规范命令名去重，排除 Hidden 定义。
5. 按规范名称稳定排序，复制别名、描述和参数提示到 Suggestion。

**验证：** 运行 `go test ./internal/command`，期望补全代码编译通过且不引入外部依赖。

## T7：覆盖补全精确优先、去重和隐藏

**文件：** `internal/command/completion_test.go`  
**依赖：** T6  
**覆盖：** AC7–AC9

**步骤：**

1. 测试唯一前缀返回一个规范命令。
2. 测试 `/p` 精确别名优先于 `/permission` 前缀。
3. 测试名称和多个别名同时匹配时只返回一次。
4. 测试多个候选按规范名称稳定排序。
5. 测试隐藏命令永不返回。
6. 测试参数区、普通文本和空输入不返回候选。

**验证：** 运行 `go test ./internal/command -run TestComplete`，期望全部通过。

## T8：登记公开命令元数据并实现帮助与模式命令

**文件：** `internal/command/builtins.go`  
**依赖：** T2、T4  
**覆盖：** F1、F7、F11、F13–F15

**步骤：**

1. 按批准的名称、别名、描述、Usage、Type、ArgHint 和 Hidden 状态建立公开定义。
2. 实现 `/help`，从 ExecutionContext.Registry.Visible 生成稳定多行帮助。
3. 实现 `/clear`，拒绝多余参数并调用 ClearMessages。
4. 实现 `/plan` 与 `/do`，拒绝多余参数、切换模式并刷新状态。
5. 确保帮助展示十个公开命令且不硬编码隐藏命令列表。

**验证：** 运行 `go test ./internal/command`，期望 Builtins 能通过 `MustNew` 构造且无别名冲突。

## T9：实现公开本地状态命令

**文件：** `internal/command/builtins.go`  
**依赖：** T8  
**覆盖：** F12、F19

**步骤：**

1. 实现 `/compact`：调用 CompactContext，显示实际结果或错误。
2. 实现 `/session`：格式化 ID、消息数、模式和 streaming。
3. 实现无参数 `/memory`：格式化 user/project 启用状态、数量和诊断。
4. 实现 `/permission`：格式化模式、各层规则数和加载错误。
5. 实现 `/status`：组合 RuntimeStatus 与 TokenUsage。
6. 对所有无参数公开命令统一校验多余参数并显示 Usage 错误。

**验证：** 运行 `go test ./internal/command`，期望所有处理函数编译且不引用 App/TUI 包。

## T10：实现 review 与隐藏兼容命令

**文件：** `internal/command/builtins.go`  
**依赖：** T9  
**覆盖：** F17、F18、F20

**步骤：**

1. 定义完整 review 固定提示词，包含审查范围、优先级、证据格式和禁止修改要求。
2. 实现 `/review` 与 `/rv`，把展开提示交给 SendUserMessage。
3. 注册隐藏 `/permissions`、`/mcp`、`/diagnostics` 定义并校验参数。
4. 让 `/memory` 有参数时调用 LegacyMemory，保持原子命令用法和错误。
5. 确认隐藏定义不进入 Visible，公开定义数量保持十个。

**验证：** 运行 `go test ./internal/command`，期望 `MustNew(Builtins()...)` 成功。

## T11：使用替代 Controller 验证全部内置命令

**文件：** `internal/command/builtins_test.go`  
**依赖：** T10  
**覆盖：** AC1、AC6、AC10–AC15、AC21、AC22、AC24

**步骤：**

1. 实现记录所有 Controller 调用的内存替代对象。
2. 表驱动测试十个公开命令及全部别名。
3. 断言 `/help` 元数据完整且隐藏命令缺席。
4. 断言模式、清屏、压缩、Session、Memory、Permission、Status 调用正确。
5. 断言 `/review` 发送完整提示，不直接显示缩写命令。
6. 断言隐藏兼容入口和 Memory 子命令委托正确。
7. 断言多余参数产生 Usage 错误。

**验证：** 运行 `go test ./internal/command`，期望整个命令核心包全部通过。

## T12：增加 Orchestrator 明确模式入口

**文件：** `internal/orchestrator/chat.go`  
**依赖：** 无  
**覆盖：** F14、F16、N5

**步骤：**

1. 新增 `SendWithMode`，清理并拒绝空文本，校验运行模式。
2. 把用户消息追加、事件通道创建和 Agent Loop 启动迁入新入口。
3. 修改现有 `Send`：继续调用 `parseRunRequest`，再委托 `SendWithMode`。
4. 检查 Agent Loop、工具结果后的回复和后续 iteration 均继续携带初始 mode，修正任何回落 Default 的内部调用。

**验证：** 运行 `go test ./internal/orchestrator`，期望现有模式与 Agent Loop 测试通过。

## T13：验证明确模式和旧发送入口

**文件：** `internal/orchestrator/chat_test.go`  
**依赖：** T12  
**覆盖：** AC5、AC20、N5

**步骤：**

1. 测试 `SendWithMode` 保存原始普通文本而非命令前缀。
2. 测试 Plan 模式首轮及工具结果后的 provider 请求均保持 Plan 动态提示和只读工具。
3. 测试 Default 模式恢复完整工具注册中心。
4. 测试空文本和无效 mode 返回错误且不写会话。
5. 保留现有 `Send("/plan ...")`、`Send("/do ...")` 兼容测试。

**验证：** 运行 `go test ./internal/orchestrator -run 'Test.*Mode|TestParseRunRequest'`，期望全部通过。

## T14：增加输入补全写回

**文件：** `internal/tui/input.go`  
**依赖：** 无  
**覆盖：** F9

**步骤：**

1. 新增 `SetValue` 包装 textinput.SetValue。
2. 写回后保持输入焦点。
3. 把光标移动到文本末尾，确保继续输入不会插到中间。

**验证：** 运行 `go test ./internal/tui`，期望现有输入与 TUI 测试通过。

## T15：实现轻量命令候选菜单

**文件：** `internal/tui/command_menu.go`  
**依赖：** 无  
**覆盖：** F9、F10

**步骤：**

1. 定义 CommandMenuItem 和 CommandMenu。
2. 实现 Open、Close、Move、SelectedItem，处理空列表和边界。
3. 选择策略使用首尾循环或稳定边界，并在实现与测试中保持一致。
4. View 展示规范名称、描述和非空 ArgHint，并突出当前项。
5. Open 时复制输入列表，避免外部修改影响菜单。

**验证：** 运行 `go test ./internal/tui`，期望菜单代码编译通过。

## T16：覆盖候选菜单状态和渲染

**文件：** `internal/tui/command_menu_test.go`  
**依赖：** T15  
**覆盖：** AC8

**步骤：**

1. 测试空候选不显示，非空候选从第一项开始。
2. 测试上下移动、边界策略和 SelectedItem。
3. 测试 Close 清理可见状态与选择。
4. 测试 View 包含名称、描述、参数提示和选中标记。
5. 修改 Open 的原始切片，确认菜单内容未变。

**验证：** 运行 `go test ./internal/tui -run TestCommandMenu`，期望全部通过。

## T17：实现仅清空消息视图

**文件：** `internal/tui/messages.go`  
**依赖：** 无  
**覆盖：** F13

**步骤：**

1. 新增 `Clear`，清空 messages、assistantBuffer 和 thinkingBuffer。
2. 不返回或修改 Conversation，不改变 showThinking 配置。
3. 确保清空后可以继续 AppendUser、AppendAssistantDelta 和 UpsertTool。

**验证：** 运行 `go test ./internal/tui`，期望现有 MessagesView 测试通过。

## T18：验证清屏不修改会话源数据

**文件：** `internal/tui/messages_test.go`  
**依赖：** T17  
**覆盖：** AC16

**步骤：**

1. 用 Conversation 消息初始化 MessagesView。
2. 添加 assistant/thinking 流式缓冲并调用 Clear。
3. 断言 View 为空、缓冲不会重新出现。
4. 断言原 Conversation.Messages 内容和长度不变。
5. 断言 Clear 后新消息能正常渲染。

**验证：** 运行 `go test ./internal/tui -run TestMessagesViewClear`，期望全部通过。

## T19：在状态栏渲染持久模式

**文件：** `internal/tui/status.go`  
**依赖：** 无  
**覆盖：** F15

**步骤：**

1. 为 Status 增加 Mode 字段。
2. 在 View 首段渲染 `[DEFAULT]` 或 `[PLAN]`。
3. 空值、未知值统一安全退化为 `[DEFAULT]`。
4. 保持 Provider、Model、streaming、usage、MCP、Notice、Error 现有展示不变。

**验证：** 运行 `go test ./internal/tui`，期望状态栏编译且现有渲染测试通过。

## T20：覆盖状态栏模式展示

**文件：** `internal/tui/status_test.go`  
**依赖：** T19  
**覆盖：** AC17–AC19

**步骤：**

1. 测试 default、plan、空值和未知值。
2. 断言模式标记始终位于 Provider/Model 之前。
3. 断言加入模式后 Token、Cache、MCP 和 Error 文本仍保留。

**验证：** 运行 `go test ./internal/tui -run TestStatus`，期望全部通过。

## T21：接入注册中心与会话模式状态

**文件：** `internal/app/deps.go`、`internal/app/app.go`  
**依赖：** T10、T15、T17、T19  
**覆盖：** F2、F14、F15、N8、N12

**步骤：**

1. 在 Deps 增加明确命名的可选命令注册中心。
2. 在 Model 增加 registry、menu、mode、lastError 和 messagesCleared。
3. New 在无注入时使用 `MustNew(Builtins()...)`，并同步 Status.Mode。
4. 抽取模式/视图重置辅助函数。
5. startNewConversation 和 loadConversation 调用重置辅助函数，恢复 Default、关闭菜单并清除 messagesCleared。
6. 在 View 中把可见候选菜单放在输入框附近。

**验证：** 运行 `go test ./internal/app -run 'TestNew|TestLoadConversation'`，期望 App 初始化和会话加载测试通过。

## T22：实现 Controller 的显示、模式和清屏能力

**文件：** `internal/app/command_controller.go`  
**依赖：** T21  
**覆盖：** F8、F13–F15、F19

**步骤：**

1. 建立持有 `*Model` 和待返回 `tea.Cmd` 的适配器。
2. 实现 DisplayNotice/DisplayError，并在最终写状态前调用现有脱敏函数。
3. DisplayError 同时更新 lastError。
4. 实现 ClearMessages，调用 MessagesView.Clear 并设置 messagesCleared。
5. 实现 CurrentMode、SwitchMode 和 RefreshStatus，完成 command/orchestrator mode 转换。
6. 实现 TokenUsage 和 RuntimeStatus 的纯状态映射。

**验证：** 运行 `go test ./internal/app`，期望适配器满足 `command.Controller` 编译检查。

## T23：实现 Session、Memory 与 Permission 快照

**文件：** `internal/app/command_controller.go`  
**依赖：** T22  
**覆盖：** F12、F19

**步骤：**

1. 实现 SessionStatus：处理无活动会话并统计消息数量。
2. 实现 MemoryStatus：读取 Manager.Status 和 user/project 索引数量。
3. 把记忆诊断与索引读取错误转成待脱敏文本，不泄露条目正文。
4. 实现 PermissionStatus：映射 Orchestrator.PermissionStatus。
5. 对 nil Memory/Orchestrator 依赖提供明确本地状态，不 panic。

**验证：** 运行 `go test ./internal/app`，期望快照代码编译；相关测试中状态字段与 fake 依赖一致。

## T24：实现压缩与隐藏兼容 Controller 能力

**文件：** `internal/app/command_controller.go`  
**依赖：** T23  
**覆盖：** F12、F18、F19

**步骤：**

1. 实现 CompactContext，复用现有 CompactContext 与 compactNotice。
2. 仅在 `messagesCleared=false` 时把压缩后的 Conversation 同步回 MessagesView。
3. 把现有 Memory 子命令行为迁入 LegacyMemory，保留 scope、参数和错误文本。
4. 实现 LegacyMCPStatus 与 LegacyDiagnostics，复用现有汇总和脱敏边界。
5. 删除对旧 Model.handleXxxCommand 的依赖，为后续移除旁路做准备。

**验证：** 运行 `go test ./internal/app -run 'TestMemory|TestMCP|TestDiagnostics|TestCompact'`，期望现有兼容测试继续通过或仅需入口适配。

## T25：抽取统一请求提交与输入分发

**文件：** `internal/app/commands.go`  
**依赖：** T12、T21、T24  
**覆盖：** F3–F8、F14、F17、F20

**步骤：**

1. 把 Enter 分支中创建会话、context/cancel、Orchestrator Send、RequestSession 和状态清零逻辑抽为 `submitUserMessage`。
2. 改用 `SendWithMode`，根据 Model.mode 传 Default 或 Plan。
3. 实现 Controller.SendUserMessage，通过统一 helper 提交并保存待返回 tea.Cmd。
4. 实现 `dispatchInput`，处理 Empty、PlainText、Executed、Unknown 四种结果。
5. 命令和未知命令都清空输入；普通发送失败时保留可重试的错误状态。
6. 确保 `/review` 与普通文本共用完整请求生命周期。

**验证：** 运行 `go test ./internal/app`，期望 App 编译且现有普通请求生命周期测试通过。

## T26：接入 App 命令补全辅助

**文件：** `internal/app/commands.go`  
**依赖：** T6、T14、T15、T25  
**覆盖：** F9、F10、F20

**步骤：**

1. 实现 `completeCommand` 调用 Registry.Complete。
2. 单候选直接写回 `/<canonical>` 并关闭菜单。
3. 多候选映射为 tui.CommandMenuItem 并打开菜单。
4. 无候选显示本地 `/help` 提示，不请求 Provider。
5. 实现 `acceptCommandCompletion`，只写回输入并关闭菜单。

**验证：** 运行 `go test ./internal/app`，期望补全辅助代码编译通过。

## T27：重构 Update 键盘与命令入口

**文件：** `internal/app/update.go`  
**依赖：** T25、T26  
**覆盖：** F3–F6、F9、F18、F20

**步骤：**

1. 在权限确认之后优先处理可见候选菜单的 Up/Down/Enter/Esc。
2. 在 Chat、非 streaming 状态拦截 Tab 并调用 completeCommand。
3. 把 Chat Enter 的本地命令条件链替换为 dispatchInput。
4. 删除或迁移旧 compact/MCP/diagnostics/permissions/memory handler 和重复格式函数。
5. 非菜单普通按键先关闭过期菜单，再交给 textinput。
6. EventError 同步 lastError；其余事件行为保持不变。

**验证：** 运行 `go test ./internal/app`，期望原有 Update 测试和编译通过。

## T28：验证本地分流与 Provider 调用边界

**文件：** `internal/app/commands_test.go`  
**依赖：** T27  
**覆盖：** AC3–AC6、AC10–AC15、AC24

**步骤：**

1. 建立可计数 Provider 和最小 App Model 测试助手。
2. 表驱动提交 help、compact、session、memory、permission、status、clear、plan、do、unknown 与别名。
3. 断言所有本地/UI/未知命令均清空输入且 Provider 计数不变。
4. 断言普通文本产生一次请求并保留文本。
5. 断言命令大小写不敏感、参数错误显示 Usage。

**验证：** 运行 `go test ./internal/app -run 'TestCommandDispatch|TestLocalCommandsDoNotCallProvider'`，期望全部通过。

## T29：验证模式生命周期、review 和清屏

**文件：** `internal/app/commands_test.go`  
**依赖：** T28  
**覆盖：** AC16–AC21

**步骤：**

1. 断言 `/plan` 后状态栏为 PLAN，连续普通请求均以 Plan 发送。
2. 断言 `/do` 恢复 Default，后续请求不使用 Plan。
3. 断言创建和加载会话重置 Default，模式不写入 Conversation。
4. 执行 `/clear`，断言视图空但 Conversation.Messages 不变；Compact 不重新显示旧消息。
5. 执行 `/review`，断言完整提示作为用户消息显示、保存并只产生一次 Provider 请求。
6. 在 Plan 中执行 `/review`，断言仍使用 Plan 模式。

**验证：** 运行 `go test ./internal/app -run 'TestCommandMode|TestReviewCommand|TestClearCommand'`，期望全部通过。

## T30：验证 App 的单匹配和多候选交互

**文件：** `internal/app/commands_test.go`  
**依赖：** T27  
**覆盖：** AC7–AC9

**步骤：**

1. 输入唯一前缀按 Tab，断言规范命令写回且不执行。
2. 输入 `/p`，断言精确别名补全 `/plan`。
3. 输入多匹配前缀，断言菜单打开、排序稳定且隐藏命令缺席。
4. 用 Up/Down/Enter 选择，断言只写回输入；第二次 Enter 才执行。
5. 用 Esc 和普通字符关闭菜单，断言焦点和文本更新正常。
6. 断言所有补全过程 Provider 计数不变。

**验证：** 运行 `go test ./internal/app -run TestCommandCompletion`，期望全部通过。

## T31：迁移兼容、脱敏和状态回归测试

**文件：** `internal/app/update_test.go`、`internal/app/commands_test.go`  
**依赖：** T27  
**覆盖：** AC13–AC15、AC22、AC23

**步骤：**

1. 把现有 diagnostics、MCP、permissions 和 memory 测试改为统一 registry 入口。
2. 保留旧命令文本和原行为断言。
3. 断言隐藏命令不出现在 help 和补全。
4. 为 Session/Memory/Permission/Status 注入模拟 token、API key、密码和敏感路径。
5. 断言 Notice、Error、帮助和兼容输出均不包含原始敏感值。
6. 断言 `/status` 能显示 lastError，而执行 status 本身不会丢失最近错误。

**验证：** 运行 `go test ./internal/app -run 'Test.*Command|Test.*Redact|Test.*Status'`，期望全部通过。

## T32：更新公开命令文档

**文件：** `README.md`  
**依赖：** T10、T27  
**覆盖：** F11–F18

**步骤：**

1. 将命令表更新为十个公开命令及推荐别名。
2. 说明 `/plan` 持续到 `/do`，会话切换或重启恢复 DEFAULT。
3. 说明 `/clear` 只清屏、不删除历史或上下文。
4. 说明 `/review` 展开提示并进入 AI 对话。
5. 不列出隐藏兼容命令，但不删除其他相关能力说明。

**验证：** 运行 `rg -n '/help|/compact|/clear|/plan|/do|/session|/memory|/permission|/status|/review' README.md`，期望十个公开命令均出现且语义完整。

## T33：格式化并执行定向构建检查

**文件：** 所有新增和修改的 Go 文件  
**依赖：** T3、T5、T7、T11、T13、T16、T18、T20、T29、T30、T31  
**覆盖：** N4、N11

**步骤：**

1. 对本功能涉及的 Go 文件运行 `gofmt -w`。
2. 检查 `internal/command` 没有导入 App、TUI、Provider 或 Orchestrator。
3. 运行命令、TUI、Orchestrator、App 四个包的定向测试。
4. 修复编译、格式和接口不一致后重新运行。

**验证：** 运行 `go test ./internal/command ./internal/tui ./internal/orchestrator ./internal/app`，期望四个包全部通过。

## T34：执行全量自动化验证

**文件：** 全项目  
**依赖：** T32、T33  
**覆盖：** AC25、N5、N11

**步骤：**

1. 运行全量测试。
2. 运行静态检查。
3. 复查失败是否来自本功能或现有工作区状态，不掩盖真实回归。
4. 修复本功能导致的问题并重跑，记录实际输出。

**验证：** 运行 `go test ./...` 和 `go vet ./...`，期望均以退出码 0 完成。

## T35：核对实现范围与最终差异

**文件：** 本任务文件清单中的全部文件  
**依赖：** T34  
**覆盖：** F1–F20、N1–N12

**步骤：**

1. 对照 spec.md 检查每条 F 需求都有实现和测试证据。
2. 对照 plan.md 检查类型、接口、依赖方向和文件职责未漂移。
3. 检查没有新增用户自定义命令、参数补全、权限切换或持久化模式等越界能力。
4. 检查 Git diff 只包含本功能文件和已批准文档，不改动用户已有无关文件。
5. 不创建 Git 提交，等待 checklist 阶段执行端到端验收。

**验证：** 运行 `git diff --check`，并用 `git status --short` 核对文件范围；期望无空白错误且无本任务造成的范围外改动。

## 执行顺序

```text
T1 → T2 → T3
      ├→ T4 → T5
      ├→ T6 → T7
      └→ T8 → T9 → T10 → T11

T12 → T13                  （可与 command/TUI 任务并行）
T14                        （可并行）
T15 → T16                  （可并行）
T17 → T18                  （可并行）
T19 → T20                  （可并行）

T10 + T15 + T17 + T19 → T21 → T22 → T23 → T24
T12 + T21 + T24 → T25
T6 + T14 + T15 + T25 → T26 → T27
T27 → T28 → T29
   ├→ T30
   └→ T31

T10 + T27 → T32
T3 + T5 + T7 + T11 + T13 + T16 + T18 + T20 + T29 + T30 + T31 → T33
T32 + T33 → T34 → T35
```
