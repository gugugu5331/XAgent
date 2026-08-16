# 子 Agent 委派与后台任务系统 Tasks

本任务表基于已批准的 [spec.md](spec.md) 与 [plan.md](plan.md)。每个任务保持一个聚焦工作单元；完成任务后必须先执行该任务的验证，再进入依赖它的任务。

## 文件清单

| 操作 | 文件或目录 | 职责 |
|---|---|---|
| 新建 | `internal/agentrole/` | 角色类型、严格 Markdown/YAML 解析、四级来源、Provider 注册、快照与模型别名 |
| 修改/新建测试 | `internal/config/` | presence-aware 的模型别名、角色限制和运行限制解析与校验 |
| 修改/新建测试 | `internal/tool/` | Agent system route、能力视图、后台策略、读缓存和任务作用域 Executor |
| 修改/新建测试 | `internal/permission/` | TaskScope、任务票据 Issuer/Verifier 和权限收紧 |
| 修改/新建测试 | `internal/conversation/` | Fork 深快照、子 Agent 通知消息、JSONL 校验/迁移 |
| 修改/新建测试 | `internal/provider/` | PromptPrefixSnapshot、缓存边界和 `subagent_result` provider 映射 |
| 新建 | `internal/subagent/` | Submit 服务、任务状态机、调度、事件、确认和 ResultInbox |
| 修改/新建测试 | `internal/orchestrator/` | 子 Agent RuntimeState、Defined/Fork Runner、循环与取消接线 |
| 修改/新建测试 | `internal/command/` | `/agent`、`/tasks`、`/task` 的统一 TaskIntent |
| 修改/新建测试 | `internal/app/` | 组装依赖、任务事件订阅、生命周期关闭顺序 |
| 修改/新建测试 | `internal/tui/` | 任务列表/详情、确认卡、切后台、异步通知和去重 |
| 新建 | `internal/testutil/subagent_fixture.go` | 无真实 Provider/TUI 的可控 Runner、Provider 和事件 fixture |
| 修改/新建测试 | `cmd/xagent/assembly_subagents.go`、`cmd/xagent/assembly.go`、`cmd/xagent/lifecycle.go` | 唯一生产组装路径和关闭接线 |
| 修改 | `config.example.yaml` | 模型别名、角色来源和 subagent 限制示例 |

## 有序任务

### T1：建立 presence-aware 配置字段

**文件：** `internal/config/partial.go`、`internal/config/config.go`

**依赖：** 无

**步骤：**

1. 增加 `PartialModelAliases`、`PartialSubagentConfig` 和 `Optional[T]` 字段，保留现有配置字段语义。
2. 增加 resolved `SubagentConfig`、`ResolvedModelAliases`，不让运行层再次解释 presence。

**验证：** `go test ./internal/config -run 'Test.*Presence|Test.*Partial'`；缺失、显式空值和显式非空值分别产生预期状态。

### T2：实现限制默认值和 fail-closed 校验

**文件：** `internal/agentrole/types.go`、`internal/config/resolve.go`、`internal/config/validate.go`

**依赖：** T1

**步骤：**

1. 实现 role 与 subagent 两组默认限制、hard cap、正值检查和 overflow-safe 转换。
2. 校验并发、队列、事件、结果、读缓存、ID、自动后台阈值及可选任务墙钟上限。

**验证：** `go test ./internal/config ./internal/agentrole -run 'Test.*Limit|Test.*Overflow|Test.*Default'`；非法或溢出配置启动前失败。

### T3：实现模型别名目录

**文件：** `internal/agentrole/model.go`、`internal/config/resolve.go`

**依赖：** T1、T2

**步骤：**

1. 实现 `ModelCatalog`、四种固定别名和 `ModelResolutionError`。
2. 让 `inherit` 使用捕获的父模型或默认模型，其他别名严格映射到已校验 concrete model。

**验证：** `go test ./internal/agentrole ./internal/config -run 'Test.*Model'`；缺失映射返回固定错误且不改变全局 Provider 配置。

### T4：实现严格 Markdown/YAML 解析

**文件：** `internal/agentrole/parser.go`、`internal/agentrole/parser_test.go`

**依赖：** T2

**步骤：**

1. 解析首行/尾行 frontmatter，执行 `KnownFields(true)`、重复键检查和类型校验。
2. 在解码前拒绝 alias、anchor、merge key、显式 standard/custom tag、directive、多文档和尾随文档。
3. 实现名称规范化、工具列表 nil/空数组语义、正文脱敏和二次 UTF-8 限长。

**验证：** `go test ./internal/agentrole -run 'TestParse|TestYAML|TestFrontmatter'`；每类非法 YAML 均有结构化诊断，合法角色正文完整保留。

### T5：实现四级角色文件发现

**文件：** `internal/agentrole/discovery.go`、`internal/agentrole/builtin.go`、`internal/agentrole/discovery_test.go`

**依赖：** T4

**步骤：**

1. 接入 `${ProjectRoot}/.xagent/agents/`、`${UserConfigRoot}/agents/` 和 `internal/agentrole/builtins/` 的 embedded builtin 根。
2. 只遍历 Root 直接子级，统计所有 entry，拒绝 Root 越界/symlink/非目录，稳定排序 origin。

**验证：** `go test ./internal/agentrole -run 'TestDiscover|TestSourceDirectory|TestSymlink'`；可选目录缺失视为空，Root 安全错误拒绝刷新。

### T6：实现插件 Provider 注册与封存

**文件：** `internal/agentrole/provider.go`、`internal/agentrole/provider_test.go`

**依赖：** T2

**步骤：**

1. 在 Register 时规范化 ProviderID、检查重复/长度/数量并保存 `RegisteredProvider`。
2. `Seal` 后发布按 ProviderID 排序的不可变快照，禁止动态注册和重新读取 provider ID。

**验证：** `go test ./internal/agentrole -run 'TestProviderRegistry'`；封存后注册、替换、重复和超限均 fail-closed。

### T7：实现角色 Manager 与原子 generation 快照

**文件：** `internal/agentrole/snapshot.go`、`internal/agentrole/manager.go`、`internal/agentrole/manager_test.go`

**依赖：** T3、T5、T6

**步骤：**

1. 将 Candidate 按来源批次绑定唯一 `Provenance`，构造 `Definition` 并重算 fingerprint。
2. 按 plugin → builtin → user → project 合并覆盖，处理无效高优先级、同层有效重名和诊断预算。
3. 以 mutex 构建、原子指针发布完整快照；运行任务继续绑定旧 generation。

**验证：** `go test ./internal/agentrole -run 'TestManager|TestSnapshot|TestOverride|TestRefresh'`；刷新失败不替换旧快照，返回深拷贝。

### T8：封存工具注册表并补工具元数据

**文件：** `internal/tool/registry.go`、`internal/tool/tool.go`、`internal/tool/view.go`、相关测试

**依赖：** 无

**步骤：**

1. 为 Agent/load_skill 标记 `RouteSystem`，为普通工具声明只读、无副作用、并发安全属性。
2. 实现一次性 `Registry.Seal` 和 sealed View，防止过滤后通过 `AlwaysInclude` 复活工具。

**验证：** `go test ./internal/tool -run 'Test.*Seal|Test.*Route|Test.*Metadata'`；封存后注册和工具定义漂移均被拒绝。

### T9：实现单调能力视图与后台策略

**文件：** `internal/tool/execution_policy.go`、`internal/tool/view.go`、测试

**依赖：** T8

**步骤：**

1. 实现父集合、角色 allow/deny、全局禁止、Plan/硬约束和后台白名单的交集/差集链。
2. 同时准备 foreground/background View，记录 `FilterReason`，`Detach` 时原子切换 active View。

**验证：** `go test ./internal/tool -run 'TestCapability|TestBackground|TestFilter'`；前台不应用后台白名单，实际后台切换后只会继续收窄。

### T10：实现 Agent system-route 输入与路由

**文件：** `internal/tool/agent.go`、`internal/orchestrator/system_tool_router.go`、测试

**依赖：** T8、T9、T16

**步骤：**

1. 固定 Agent 工具 schema、`defined|fork` 和 placement 枚举及输入上限。
2. 在 CapabilitySet 预检后把 Agent 路由到 `subagent.Service.Submit`；递归、过滤和未知调用不创建任务。

**验证：** `go test ./internal/tool ./internal/orchestrator -run 'Test.*Agent|Test.*SystemToolRouter|Test.*Recursive'`。

### T11：实现 Conversation 深快照和通知消息

**文件：** `internal/conversation/snapshot.go`、`internal/conversation/message.go`、`internal/conversation/jsonl_record.go`、测试

**依赖：** 无

**步骤：**

1. 在 ordered-commit 边界实现深拷贝、临时会话物化和状态清零。
2. 增加 `RoleSubagentNotification`、字段上限、JSONL 校验/迁移和 NotificationID 幂等去重。

**验证：** `go test ./internal/conversation -run 'Test.*Snapshot|Test.*SubagentNotification|Test.*JSONL'`。

### T12：实现 Provider PromptPrefixSnapshot

**文件：** `internal/provider/prompt_snapshot.go`、`internal/provider/request.go`、Provider adapter 测试

**依赖：** T8、T11

**步骤：**

1. 捕获已预算/脱敏的 system、message、tool、model、thinking 和 cache prefix，并校验 OrderedSystem 互斥布局。
2. 实现 Defined/Fork child 构造；工具指纹变化时只保留系统缓存边界。
3. 为 `ModelMessageRoleSubagentResult` 增加 OpenAI/Anthropic 固定 marker 映射。

**验证：** `go test ./internal/provider -run 'Test.*Prompt|Test.*Cache|Test.*SubagentResult'`。

### T13：实现任务权限作用域与任务票据

**文件：** `internal/permission/scoped.go`、`internal/permission/identity.go`、`internal/permission/ticket.go`、测试

**依赖：** T9

**步骤：**

1. 实现父权限模式到角色模式的严格收紧和 escalation 拒绝。
2. 深拷贝只读规则层，创建独立 Session、确认 scope、任务私钥/nonce 和绑定 ScopeID 的票据 Issuer/Verifier。

**验证：** `go test ./internal/permission -run 'Test.*TaskScope|Test.*Restrict|Test.*Ticket'`；跨任务票据、父临时授权和永久授权均不能复用。

### T14：实现任务级读缓存

**文件：** `internal/tool/read_cache.go`、ResultFactory 相关文件、测试

**依赖：** T8、T13

**步骤：**

1. 实现任务独占、有界 LRU、参数/read-root fingerprint 和文件版本依赖校验。
2. 缓存完整 `CachedReadResult` 安全模板，不保存原始输出或旧 CallID。

**验证：** `go test ./internal/tool -run 'Test.*ReadCache|Test.*AuthorizedResult'`；命中时重新物化当前 CallID，依赖变化或超限时 miss。

### T15：实现 ScopedExecutor 最终能力重检

**文件：** `internal/tool/scoped_executor.go`、`internal/tool/executor.go`、测试

**依赖：** T9、T13、T14

**步骤：**

1. 在最后线性化点读取当前 Capability View，Detach 后移除的工具丢弃旧票据。
2. 通过任务 Verifier 原子消费票据一次，并在授权 start boundary 后接入 ReadCache。

**验证：** `go test ./internal/tool -run 'Test.*ScopedExecutor|Test.*DetachRace'`；waiting_confirmation 切后台不能绕过后台过滤或跨任务消费票据。

### T16：建立 subagent 公共类型、错误和限制

**文件：** `internal/subagent/types.go`、`errors.go`、`limits.go`、测试

**依赖：** T2

**步骤：**

1. 定义 SubmitInput、ParentRef、状态机、Completion、TaskSnapshot、事件 one-of 类型和固定错误码。
2. 实现所有 Clone、深拷贝、状态/终态组合校验和限制校验。

**验证：** `go test ./internal/subagent -run 'Test.*Type|Test.*Clone|Test.*Limit|Test.*Transition'`。

### T17：实现任务确认 Broker

**文件：** `internal/subagent/confirmation.go`、测试

**依赖：** T13、T16

**步骤：**

1. 按 TaskID 分区排队确认，匹配 TaskID/ConfirmationID/CallID。
2. 实现 allow once/session、deny、stale、Close 唤醒和取消映射；永久授权 fail-closed。

**验证：** `go test ./internal/subagent -run 'Test.*Confirmation'`；并行任务确认互不消费，取消只唤醒对应等待者。

### T18：实现 EventHub 与有界事件日志

**文件：** `internal/subagent/event_hub.go`、事件类型测试

**依赖：** T16

**步骤：**

1. 为每个事件分配全局 Revision/任务 Sequence，执行 one-of 校验和深拷贝投影。
2. 实现 bounded replay、Watermark、Gap 重同步和慢订阅者隔离；Runner 事件持续被排空。

**验证：** `go test ./internal/subagent -run 'Test.*EventHub|Test.*Gap|Test.*Watermark'`。

### T19：实现 ResultInbox 与 claim lease

**文件：** `internal/subagent/result_inbox.go`、测试

**依赖：** T16

**步骤：**

1. 实现 Reserve/ReleaseReservation、按 ConversationID 排序的 Publish。
2. 实现 bounded Claim、owner lease、Ack/Release、context 自动 Release 和容量错误。

**验证：** `go test ./internal/subagent -run 'Test.*ResultInbox|Test.*Claim|Test.*Lease'`；同一结果不会跨会话或被非 owner 确认两次。

### T20：实现 subagent.Manager 状态存储与终态投影

**文件：** `internal/subagent/manager.go`、`scheduler.go`、测试

**依赖：** T16、T18、T19

**步骤：**

1. 实现准入、任务记录、状态转移、终态 tombstone、List/Get 快照和稳定排序。
2. 让 Completion 成为 TaskSnapshot、terminal event、ResultNotification 和 TUI 投影的唯一来源。

**验证：** `go test ./internal/subagent -run 'Test.*Manager|Test.*TaskSnapshot|Test.*Completion|Test.*Tombstone'`。

### T21：实现调度器和前后台 Placement

**文件：** `internal/subagent/scheduler.go`、`manager.go`、测试

**依赖：** T20

**步骤：**

1. 加入默认并发 4、队列 32、FIFO worker、任务墙钟上限和 overflow-safe 准入。
2. 实现显式后台、Fork 强制后台、10 秒首轮自动后台和手动 `Detach`，切换不重启/不复制。

**验证：** `go test ./internal/subagent -run 'Test.*Scheduler|Test.*Placement|Test.*Detach|Test.*Queue'`。

### T22：实现任务 RuntimeState 和 RunnerFactory

**文件：** `internal/orchestrator/task_runtime.go`、`internal/orchestrator/subagent_runner.go`、测试

**依赖：** T3、T9、T12、T13、T14、T15、T20

**步骤：**

1. 创建每任务独占 Conversation、Authorizer、Broker、ReadCache、Usage、CancelCause、CapabilitySwitch 和 Hook Session。
2. 暴露 PreparedTask/RunnerFactory，使 Manager 不读取 Orchestrator 全局授权、确认或计量字段。

**验证：** `go test ./internal/orchestrator -run 'Test.*TaskRuntime|Test.*RunnerFactory|Test.*Isolation'`。

### T23：实现 Defined 准备路径

**文件：** `internal/orchestrator/subagent_runner.go`、`internal/contextmgr/` 相关接线、测试

**依赖：** T7、T12、T22

**步骤：**

1. 从空白会话构造标准系统/安全块、项目指令、角色正文和任务消息，不复制父消息/Skill Activity。
2. 解析模型、计算 foreground/background View，绑定有效 max iterations；零轮次直接 limit_reached。

**验证：** `go test ./internal/orchestrator -run 'Test.*Defined|Test.*ZeroIteration'`；Provider fixture 看不到父会话消息。

### T24：实现 Fork 准备路径

**文件：** `internal/orchestrator/subagent_runner.go`、`internal/conversation/snapshot.go`、`internal/provider/prompt_snapshot.go`、测试

**依赖：** T11、T12、T22

**步骤：**

1. 在父 ordered-commit 串行边界捕获完整 Conversation/Prompt/Tool/Model/Mode 快照。
2. 追加可选角色动态系统块和任务消息；强制后台且不建立父取消桥。

**验证：** `go test ./internal/orchestrator -run 'Test.*Fork|Test.*ParentSnapshot|Test.*PromptPrefix'`；父会话后续变化不影响首次子请求。

### T25：接入非交互 Agent Loop 与 Hook 生命周期

**文件：** `internal/orchestrator/agent_loop.go`、`execution_state.go`、`tool_scheduler.go`、`subagent_runner.go`、`internal/hook/lifecycle.go`、测试

**依赖：** T17、T22、T23、T24

**步骤：**

1. 复用 Agent Loop 的多轮请求、工具批次顺序、用量累计和停止条件，阻止 Agent system route 递归。
2. 每任务执行 `SessionStart`、每轮 `BeginTurn/EndTurn`，终态/取消/关闭执行一次 `SessionEnd`。

**验证：** `go test ./internal/orchestrator ./internal/hook -run 'Test.*Subagent|Test.*Hook.*Lifecycle|Test.*Loop'`。

### T26：接入取消、超时和应用关闭

**文件：** `internal/orchestrator/subagent_runner.go`、`internal/subagent/manager.go`、`internal/app/lifecycle.go`、测试

**依赖：** T21、T25

**步骤：**

1. 取消时关闭 Broker、取消任务 context、停止新 Provider/工具启动并沿用已有工具收尾语义。
2. 按准入停止→取消任务→有界等待→关闭任务 Hook→普通交互→共享基础设施的顺序实现幂等 Shutdown。

**验证：** `go test ./internal/subagent ./internal/orchestrator ./internal/app -run 'Test.*Cancel|Test.*Shutdown|Test.*Timeout'`。

### T27：接入统一 Submit 与模型 Agent 工具

**文件：** `internal/orchestrator/system_tool_router.go`、`internal/subagent/manager.go`、`internal/tool/agent.go`、测试

**依赖：** T10、T20、T22

**步骤：**

1. 让模型调用校验 InvocationRef/ParentRef/RequestGeneration，并通过唯一 `Service.Submit` 登记任务。
2. 前台终态只回写一次原 ToolCallID；后台/Fork 只返回接受结果并走 ResultInbox。

**验证：** `go test ./internal/orchestrator ./internal/subagent -run 'Test.*Submit|Test.*AgentTool|Test.*ToolCall'`。

### T28：接入 TUI TaskIntent 与任务操作

**文件：** `internal/command/task_intent.go`、`internal/command/builtins.go`、`internal/app/tasks.go`、测试

**依赖：** T20、T27

**步骤：**

1. 增加 `/agent`、`/tasks`、`/task` value-only intent，TUI 与模型使用同一 Submit 参数校验。
2. 接入列表、详情、取消、切后台和确认决策，并为过期/不存在任务返回固定错误。

**验证：** `go test ./internal/command ./internal/app -run 'Test.*Task|Test.*AgentIntent'`。

### T29：实现 TUI 任务页面与异步通知

**文件：** `internal/tui/tasks.go`、`task_detail.go`、`view_model.go`、`confirmation.go`、`status.go`、测试

**依赖：** T18、T28

**步骤：**

1. 消费 `subagent.Service` 事件，展示状态、Placement、角色 provenance、用量、确认卡和有界轨迹。
2. 在当前主对话按 NotificationID 去重展示完成/失败通知，不写入导航事务。

**验证：** `go test ./internal/tui -run 'Test.*Task|Test.*Notification|Test.*Confirmation'`。

### T30：实现主 Agent 结果安全回灌

**文件：** `internal/orchestrator/result_projection.go`、`internal/subagent/result_inbox.go`、`internal/provider/` 适配器测试

**依赖：** T12、T19、T27、T29

**步骤：**

1. 在完整工具 batch 后、下一 Provider 请求前预留预算，Claim 最老连续结果并生成固定 `subagent_result` schema。
2. Provider 启动成功后 Ack，构造/校验/启动失败时 Release；不复用原 Agent ToolCallID，不自动启动新回合。

**验证：** `go test ./internal/orchestrator ./internal/provider ./internal/subagent -run 'Test.*Result|Test.*Replay|Test.*Ack|Test.*Release'`。

### T31：完成 Assembly 接线和配置示例

**文件：** `cmd/xagent/assembly_subagents.go`、`cmd/xagent/assembly.go`、`internal/app/deps.go`、`internal/app/events.go`、`config.example.yaml`

**依赖：** T3、T6、T8、T20、T26、T28

**步骤：**

1. 按 Registry.Seal → ModelCatalog → agentrole.Manager → RunnerFactory → subagent.Manager → App/TUI 的唯一顺序组装。
2. 让生产入口和候选/测试入口复用同一组装函数，补齐角色目录、模型别名和 subagent limits 示例。

**验证：** `go test ./cmd/xagent ./internal/app -run 'Test.*Assembly|Test.*Lifecycle'`；两条入口的依赖图一致。

### T32：接入任务事件订阅与前台等待

**文件：** `internal/app/update.go`、`internal/app/events.go`、`internal/tui/program.go`、测试

**依赖：** T18、T21、T29、T31

**步骤：**

1. 让 App 常驻排空 `subagent.Service` 事件流，TUI 通过该窄服务订阅，不把任务事件写入普通 stale envelope。
2. 接入 `AwaitForeground`：终态返回同步结果，Detach 返回接受后台且保持事件连续。

**验证：** `go test ./internal/app ./internal/tui -run 'Test.*Event|Test.*AwaitForeground|Test.*Detach'`。

### T33：补齐无真实 Provider 的隔离 fixture

**文件：** `internal/testutil/subagent_fixture.go`、fixture 测试

**依赖：** T16、T18、T22

**步骤：**

1. 提供可控 Provider、Runner、Clock、IDGenerator 和 TUI 事件消费者。
2. 覆盖多轮工具、流错误、确认、慢首轮、取消和结果注入场景。

**验证：** `go test ./internal/testutil -run 'Test.*SubagentFixture'`。

### T34：验证角色来源、覆盖和安全边界

**文件：** `internal/agentrole/*_test.go`、`internal/tool/*_test.go`、`internal/permission/*_test.go`

**依赖：** T7、T9、T15

**步骤：**

1. 覆盖四级来源覆盖、同层冲突、坏高层回退、刷新原子性和目录安全边界。
2. 覆盖工具收窄、递归拒绝、权限模式收紧、跨任务票据和读缓存授权边界。

**验证：** `go test ./internal/agentrole ./internal/tool ./internal/permission`。

### T35：验证执行、并发和状态机

**文件：** `internal/subagent/*_test.go`、`internal/orchestrator/*_test.go`

**依赖：** T21、T25、T26

**步骤：**

1. 覆盖并发 4/队列 32、状态合法转移、tombstone、Completion 单一来源和用量单调性。
2. 使用 `-race` 验证并行任务的消息、权限、确认、ReadCache、Hook、用量和取消隔离。

**验证：** `go test -race ./internal/subagent ./internal/orchestrator`。

### T36：验证事件、结果回流和通知持久化

**文件：** `internal/subagent/*_test.go`、`internal/orchestrator/result_projection_test.go`、`internal/conversation/*_test.go`

**依赖：** T19、T29、T30

**步骤：**

1. 覆盖 Event one-of、Revision/Sequence、Gap+Watermark 重同步、Claim lease 和 Ack/Release 竞态。
2. 覆盖 `RoleSubagentNotification` JSONL round-trip、迁移、NotificationID 去重和 Provider Context 排除。

**验证：** `go test -race ./internal/subagent ./internal/orchestrator ./internal/conversation`。

### T37：验证模型入口与 TUI 入口等价

**文件：** `internal/command/*_test.go`、`internal/app/*_test.go`、`internal/tui/*_test.go`

**依赖：** T27、T28、T32

**步骤：**

1. 对模型和 TUI 分别提交 Defined/Fork，比较参数校验、任务记录、事件序列和终态字段。
2. 验证显式后台、10 秒自动后台、手动切换、取消、等待确认和过期操作。

**验证：** `go test ./internal/command ./internal/app ./internal/tui`。

### T38：运行兼容性回归与端到端场景

**文件：** 现有普通对话、Skill、Plan Mode、Memory、MCP、会话恢复测试及新增 TUI e2e 测试

**依赖：** T31、T32、T34、T35、T36、T37

**步骤：**

1. 在不提交子 Agent 的情况下运行全部既有行为测试，确认普通路径等价。
2. 运行一个真实 TUI 场景：提交任务→查看详情→处理确认/切后台→查看完成通知与结果。

**验证：** `go test ./...`；按 checklist 记录真实 TUI 场景的可观察输出。

## 执行顺序

```text
T1 → T2 → T3 → T4 → T5 → T6 → T7
             ├→ T8 → T9 → T10
             ├→ T11 → T12
             └→ T13 → T14 → T15

T16 → T17 → T18 → T19 → T20 → T21
                         └→ T22 → T23/T24 → T25 → T26
T10 + T20 + T22 → T27 → T28 → T29 → T30
T31 → T32 → T33
T34/T35/T36/T37 → T38
```

所有任务验证通过后，才进入 `checklist.md` 阶段；本文件不授权跳过任务验证或提前编写实现。
