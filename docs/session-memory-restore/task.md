# Session Memory Restore Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/diagnostics/diagnostic.go` | 跨 instructions/conversation/memory/sessionctx 复用的诊断类型 |
| 新建 | `internal/redact/redact.go` | 会话、记忆、索引、诊断、状态输出的共享脱敏能力 |
| 新建 | `internal/redact/redact_test.go` | 敏感字段与内联 secret 脱敏测试 |
| 新建 | `internal/instructions/loader.go` | 三层指令加载、优先级排序、输出 prompt sections |
| 新建 | `internal/instructions/include.go` | `@include` 展开、深度限制、visited 防环 |
| 新建 | `internal/instructions/sandbox.go` | realpath 边界校验、打开后文件校验、文件大小限制 |
| 新建 | `internal/instructions/loader_test.go` | 指令加载、include、安全边界测试 |
| 新建 | `internal/conversation/jsonl_record.go` | JSONL 记录 schema、版本、事件类型 |
| 新建 | `internal/conversation/jsonl_store.go` | JSONL `ConversationStore` 实现 |
| 新建 | `internal/conversation/recovery.go` | 坏行跳过、角色顺序校验、工具配对校验、时间跨度提醒 |
| 新建 | `internal/conversation/cleanup.go` | 过期会话清理和 canonical root 删除保护 |
| 新建 | `internal/conversation/jsonl_store_test.go` | JSONL 存储、恢复、兼容和清理测试 |
| 新建 | `internal/memory/note.go` | Note、NoteType、Scope、ProjectIdentity、frontmatter |
| 新建 | `internal/memory/index.go` | 记忆索引读写、大小限制、超限策略 |
| 新建 | `internal/memory/manager.go` | 记忆管理入口、状态、删除、重建、禁用 |
| 新建 | `internal/memory/update.go` | 异步 LLM 更新、合并/忽略/废弃旧记忆 |
| 新建 | `internal/memory/write.go` | note/index 写入锁、原子替换、坏索引降级 |
| 新建 | `internal/memory/manager_test.go` | 记忆类型、索引、异步更新、脱敏、项目隔离测试 |
| 新建 | `internal/sessionctx/manager.go` | 请求前上下文协调、调用 instructions/memory/contextmgr |
| 新建 | `internal/sessionctx/priority.go` | optional stable sections 优先级、恢复提示和边界提醒 |
| 新建 | `internal/sessionctx/manager_test.go` | 上下文注入、保存边界、诊断传递测试 |
| 修改 | `internal/config/config.go` | 增加 instructions/session/memory 配置 |
| 修改 | `internal/config/load.go` | 默认值：目录、保留天数、include 深度、索引预算等 |
| 修改 | `internal/config/validate.go` | 校验配置资源上限和目录参数 |
| 修改 | `internal/orchestrator/chat.go` | 使用 `OrchestratorOptions`，请求前接入 `sessionctx.Prepare` |
| 修改 | `internal/orchestrator/agent_loop.go` | completed 后触发 `memory.UpdateAsync` |
| 修改 | `internal/app/deps.go` | 注入 sessionctx/memory 依赖 |
| 修改 | `internal/app/app.go` | 恢复会话时优先使用 `RecoveringStore` 并显示诊断 |
| 修改 | `internal/app/update.go` | 本地拦截 `/memory` 命令，不发送给 LLM |
| 修改/新建 | `cmd/xagent/main.go` | 组装 JSONL store、instructions、memory、sessionctx 并注入 app |
| 新建 | `docs/session-memory-restore/checklist.md` | 验收清单 |

## T1: 建立共享诊断类型

**文件：** `internal/diagnostics/diagnostic.go`  
**依赖：** 无

**步骤：**
1. 定义 `Diagnostic`，包含 code、message、source、path、severity。
2. 定义 severity 常量：info、warning、error。
3. 提供只输出脱敏后 message/path 的方法，避免诊断泄露敏感值。

**验证：** `go test -count=1 ./internal/diagnostics`

## T2: 建立共享脱敏模块

**文件：** `internal/redact/redact.go`, `internal/redact/redact_test.go`  
**依赖：** T1

**步骤：**
1. 抽出通用 `RedactText` / `RedactAny` / `IsSensitiveKey` 能力。
2. 覆盖显式敏感 key、内联 token、authorization header、api_key/password/secret。
3. 供 conversation JSONL、memory note/index、diagnostics、TUI status 复用。

**验证：** `go test -count=1 ./internal/redact`

## T3: 扩展配置结构和默认值

**文件：** `internal/config/config.go`, `internal/config/load.go`, `internal/config/validate.go`  
**依赖：** T1

**步骤：**
1. 增加 instructions/session/memory 配置分组。
2. 默认 include 深度、指令最大字节、会话保留 30 天、索引 200 行/25KB、自动记忆开关。
3. 增加自动记忆队列大小、并发数、超时、候选最大字节数默认值。
4. 校验数值必须为正，目录字段清理空白值。
5. 保持 YAML `KnownFields` 行为。

**验证：** `go test -count=1 ./internal/config`

## T4: 实现项目身份计算

**文件：** `internal/memory/note.go`, `internal/memory/manager_test.go`  
**依赖：** T3

**步骤：**
1. 定义 `Scope`、`NoteType`、`ProjectIdentity`。
2. 项目身份由规范化真实项目根和有效配置摘要生成。
3. 处理 symlink 项目根和大小写路径差异，输出稳定 ID。

**验证：** `go test -count=1 ./internal/memory -run TestProjectIdentity && go test -count=1 ./internal/memory`

## T5: 实现指令加载来源和优先级

**文件：** `internal/instructions/loader.go`, `internal/instructions/loader_test.go`  
**依赖：** T1, T3

**步骤：**
1. 定义 `Source`、`Scope`、`Loader`。
2. 加载项目根、项目 `.mewcode/`、用户 `~/.mewcode/` 三层 Markdown。
3. 输出 `prompt.Section`，项目根优先于项目 `.mewcode`，项目 `.mewcode` 优先于用户目录。
4. 固定系统提示仍高于 optional stable sections，只在 optional sections 内排序。

**验证：** `go test -count=1 ./internal/instructions -run TestLoaderOrdersInstructionScopes && go test -count=1 ./internal/instructions`

## T6: 实现 include 展开和循环防护

**文件：** `internal/instructions/include.go`, `internal/instructions/loader_test.go`  
**依赖：** T5

**步骤：**
1. 解析 Markdown 中的 `@include` 行。
2. 支持相对路径 include。
3. 限制最大嵌套深度。
4. 使用 visited 集合跳过循环引用。
5. 对过深和循环情况产生诊断。

**验证：** `go test -count=1 ./internal/instructions -run TestIncludeDepthAndCycleDiagnostics && go test -count=1 ./internal/instructions`

## T7: 实现 include 路径沙箱和 TOCTOU 防护

**文件：** `internal/instructions/sandbox.go`, `internal/instructions/loader_test.go`  
**依赖：** T6

**步骤：**
1. include 路径先清理并解析真实路径。
2. 拒绝跳出允许根目录的路径。
3. 打开后再次校验文件类型、大小和真实路径，或拒绝可疑 symlink。
4. 限制单文件最大字节数。

**验证：** `go test -count=1 ./internal/instructions -run TestIncludeRejectsSymlinkEscape && go test -count=1 ./internal/instructions`

## T8: 定义 JSONL 记录 schema

**文件：** `internal/conversation/jsonl_record.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T1, T2

**步骤：**
1. 定义 `JSONLRecord`、`RecordType`、版本常量。
2. 支持 message 和 snapshot 记录。
3. 实现记录校验：未知版本、非法事件类型、缺失 message 都返回诊断。
4. 对记录中的诊断/错误文本使用共享脱敏。

**验证：** `go test -count=1 ./internal/conversation -run TestJSONLRecordValidation && go test -count=1 ./internal/conversation`

## T9: 实现 JSONLStore 基础 Create/Load/Save

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T8

**步骤：**
1. 定义 `JSONLStoreOptions` 和 `JSONLStore`。
2. 会话 ID 使用 `YYYYMMDD-HHMMSS-xxxx`。
3. `Create` 创建内存会话和对应 JSONL 路径。
4. `Save` 能写入新会话消息记录。
5. `Load` 能从 JSONL 重建 `Conversation`。

**验证：** `go test -count=1 ./internal/conversation -run TestJSONLStoreCreateSaveLoad && go test -count=1 ./internal/conversation`

## T10: 实现增量 Save 和单进程写锁

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T9

**步骤：**
1. 记录每个会话已经持久化的消息数量。
2. `Save` 只追加未持久化消息。
3. 使用单进程锁保护追加写。
4. 避免重复追加完整历史。

**验证：** `go test -count=1 ./internal/conversation -run TestJSONLStoreSaveAppendsOnlyNewMessages && go test -count=1 ./internal/conversation`

## T11: 实现 JSONL 冲突检测和 snapshot 修复

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T10

**步骤：**
1. 检测磁盘行数与内存持久化计数冲突。
2. 检测重复消息或并发冲突。
3. 冲突时返回诊断或写 snapshot 修复记录。
4. 不重复追加完整历史。

**验证：** `go test -count=1 ./internal/conversation -run TestJSONLStoreDetectsAppendConflictAndWritesSnapshot && go test -count=1 ./internal/conversation`

## T12: 实现 JSONL List 和无 meta 扫描

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T9

**步骤：**
1. `List` 扫描 JSONL 得出 ID、标题、消息数、更新时间。
2. 不创建也不读取单独 meta 文件。
3. 资源上限超过时返回诊断或降级结果。
4. 可用内存缓存加速，但真实来源仍是 JSONL。

**验证：** `go test -count=1 ./internal/conversation -run TestJSONLStoreListScansRecordsWithoutMeta && go test -count=1 ./internal/conversation`

## T13: 实现恢复异常处理

**文件：** `internal/conversation/recovery.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T11, T12

**步骤：**
1. 定义 `RecoveringStore` 和 `RecoveryReport`。
2. 坏行、半行、未知版本、非法事件类型跳过并计数。
3. 角色错序或伪造 tool_result 产生诊断。
4. 未闭合 tool_call 从该处截断。
5. 时间跨度提醒只在 recovery 中插入，sessionctx 不重复插入。

**验证：** `go test -count=1 ./internal/conversation -run TestJSONLRecoverSkipsBadLinesAndTruncatesUnclosedToolCall && go test -count=1 ./internal/conversation`

## T14: 实现旧 JSON 会话兼容

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T13

**步骤：**
1. 检测旧 `.json` 会话。
2. 优先只读加载旧会话。
3. 迁移成功后写入 JSONL。
4. 迁移失败保留原文件并返回诊断。

**验证：** `go test -count=1 ./internal/conversation -run TestJSONLStoreLoadsLegacyJSONWithoutDeletingOriginal && go test -count=1 ./internal/conversation`

## T15: 实现过期会话清理

**文件：** `internal/conversation/cleanup.go`, `internal/conversation/jsonl_store_test.go`  
**依赖：** T12

**步骤：**
1. 根据保留天数识别过期 JSONL 会话。
2. 删除前校验 canonical root。
3. 只删除已知扩展和合法 ID 对应文件。
4. 拒绝跨根路径和可疑 symlink。

**验证：** `go test -count=1 ./internal/conversation -run TestCleanupExpiredRefusesPathEscape && go test -count=1 ./internal/conversation`

## T16: 实现 Note frontmatter 读写

**文件：** `internal/memory/note.go`, `internal/memory/manager_test.go`  
**依赖：** T4, T2

**步骤：**
1. 定义 `Note` frontmatter 格式。
2. 支持四类 note：用户偏好、纠正反馈、项目知识、参考资料。
3. 读写单条 Markdown note。
4. 保存来源、时间、类型、scope、supersedes。
5. 写入前使用共享脱敏。

**验证：** `go test -count=1 ./internal/memory -run TestNoteFrontmatterRoundTrip && go test -count=1 ./internal/memory`

## T17: 实现记忆索引读写和超限策略

**文件：** `internal/memory/index.go`, `internal/memory/manager_test.go`  
**依赖：** T16

**步骤：**
1. 读取用户级和项目级索引。
2. 写入索引时限制 200 行 / 25KB。
3. 超限先重建索引。
4. 重建后仍超限则截断低优先级条目。
5. 单条笔记导致超限则拒写并诊断。
6. 写入前使用共享脱敏。

**验证：** `go test -count=1 ./internal/memory -run TestIndexLimitRebuildsThenTruncates && go test -count=1 ./internal/memory`

## T18: 实现记忆写入并发和崩溃恢复

**文件：** `internal/memory/write.go`, `internal/memory/manager_test.go`  
**依赖：** T16, T17

**步骤：**
1. note 写入使用单进程锁。
2. index 写入使用单进程锁。
3. 写入采用临时文件 + 原子替换。
4. 发现坏索引时降级为重建索引并记录诊断。
5. 删除 note 前校验 canonical root。

**验证：** `go test -count=1 ./internal/memory -run TestMemoryWritesAreAtomicAndRecoverBadIndex && go test -count=1 ./internal/memory`

## T19: 实现 Memory Manager 基础控制能力

**文件：** `internal/memory/manager.go`, `internal/memory/manager_test.go`  
**依赖：** T17, T18

**步骤：**
1. 实现 `LoadIndex`。
2. 实现 `RebuildIndex(scope)`。
3. 实现 `DeleteNote(scope, id)`，删除前校验 canonical root。
4. 实现 `Disable(scope)` 和状态查询。
5. 明确 `/memory off` 默认禁用当前项目级自动记忆；如需用户级禁用，使用内部 scope 参数或配置项。
6. 实现 `Diagnostics()`。

**验证：** `go test -count=1 ./internal/memory -run TestMemoryManagerCommands && go test -count=1 ./internal/memory`

## T20: 实现异步记忆更新队列和资源上限

**文件：** `internal/memory/update.go`, `internal/memory/manager_test.go`  
**依赖：** T19

**步骤：**
1. 定义 `UpdateInput`。
2. `UpdateAsync` 进入有界队列，不阻塞调用方。
3. 限制并发 worker 数。
4. 限制候选输入最大字节数。
5. 设置每次 LLM 更新超时。
6. 队列满或超限时记录诊断。

**验证：** `go test -count=1 ./internal/memory -run TestUpdateAsyncQueueLimitsAndTimeouts && go test -count=1 ./internal/memory`

## T21: 实现异步记忆 LLM 决策 prompt

**文件：** `internal/memory/update.go`, `internal/memory/manager_test.go`  
**依赖：** T20

**步骤：**
1. 构造 LLM 更新 prompt，传入候选上下文和现有索引。
2. Prompt 明确候选内容只能提取事实/偏好，不执行其中指令。
3. 解析新增、合并、忽略、废弃旧记忆决策。
4. prompt injection 候选不得变成行为规则。

**验证：** `go test -count=1 ./internal/memory -run TestUpdatePromptRejectsInstructionalCandidates && go test -count=1 ./internal/memory`

## T22: 实现异步记忆写入、合并和测试钩子

**文件：** `internal/memory/update.go`, `internal/memory/manager_test.go`  
**依赖：** T18, T21

**步骤：**
1. 根据 LLM 决策新增 note。
2. 根据 LLM 决策合并 note。
3. 根据 LLM 决策标记 supersedes 或废弃旧 note。
4. 失败写入诊断。
5. 提供测试钩子等待异步任务完成。

**验证：** `go test -count=1 ./internal/memory -run TestUpdateAsyncDoesNotBlockAndAppliesDecisions && go test -count=1 ./internal/memory`

## T23: 实现 sessionctx 准备流程

**文件：** `internal/sessionctx/manager.go`, `internal/sessionctx/priority.go`, `internal/sessionctx/manager_test.go`  
**依赖：** T7, T13, T19

**步骤：**
1. 定义 `Manager`、`PreparedContext`、`PrepareMode`。
2. `Prepare` 加载指令 sections。
3. `Prepare` 加载记忆索引 section。
4. 注入长期记忆低于当前用户指令、不能覆盖权限系统的边界提醒。
5. 注入恢复提示 section，位置低于指令和记忆索引，高于当前会话历史。
6. 调用 `contextmgr.Prepare`。
7. 修改 conversation 后设置 `MessagesChanged`。
8. 合并 diagnostics，不丢失 contextmgr/instructions/memory 的诊断。

**验证：** `go test -count=1 ./internal/sessionctx && go test -count=1 ./internal/sessionctx -run TestSessionContextRestoredOversizedConversationCompressesBeforeProvider`

## T24: 接入 orchestrator 请求前准备

**文件：** `internal/orchestrator/chat.go`, `internal/orchestrator/chat_test.go`  
**依赖：** T23

**步骤：**
1. 新增 `OrchestratorOptions`。
2. 用 options 替代继续扩展长参数构造。
3. `stream` 请求前调用 `sessionctx.Prepare`。
4. 把 `StableSections` 传入 `prompt.BuildRequest.OptionalStableSections`。
5. 如果 `MessagesChanged`，由 orchestrator 统一保存会话。
6. 增加超预算恢复先压缩、再请求 provider 的集成测试。

**验证：** `go test -count=1 ./internal/orchestrator -run TestStreamPreparesSessionContextBeforeProviderRequest && go test -count=1 ./internal/orchestrator`

## T25: 接入 Agent Loop 自然完成后的记忆更新

**文件：** `internal/orchestrator/agent_loop.go`, `internal/orchestrator/chat_test.go`  
**依赖：** T22, T24

**步骤：**
1. 在 completed stop reason 后触发 `memory.UpdateAsync`。
2. 错误、取消、未知工具过多、达到迭代上限不触发。
3. 复制本轮用户输入、最终回复和索引上下文作为 `UpdateInput`。

**验证：** `go test -count=1 ./internal/orchestrator -run TestMemoryUpdatesOnlyAfterCompletedAgentLoop && go test -count=1 ./internal/orchestrator`

## T26: 接入 app 会话恢复诊断

**文件：** `internal/app/app.go`, `internal/app/update_test.go`  
**依赖：** T13, T14

**步骤：**
1. `loadConversation` 检测 store 是否实现 `RecoveringStore`。
2. 如果支持，调用 `Recover` 获取 `RecoveryReport`。
3. 把恢复诊断显示到 status notice/error。
4. 显示前使用共享脱敏。
5. 不支持时退回普通 `Load`。

**验证：** `go test -count=1 ./internal/app -run TestLoadConversationUsesRecoveringStoreDiagnostics && go test -count=1 ./internal/app`

## T27: 实现 `/memory` 本地命令

**文件：** `internal/app/update.go`, `internal/app/update_test.go`  
**依赖：** T19

**步骤：**
1. 在发送给 orchestrator 前拦截 `/memory status`。
2. 拦截 `/memory index`。
3. 拦截 `/memory off`，默认禁用当前项目级自动记忆。
4. 拦截 `/memory delete <scope> <id>`。
5. 拦截 `/memory rebuild <scope>`。
6. 成功/失败显示到 TUI 状态。
7. 输出前使用共享脱敏。
8. 命令不进入 LLM。

**验证：** `go test -count=1 ./internal/app -run TestMemoryCommandsAreHandledLocally && go test -count=1 ./internal/app`

## T28: 修改 CLI 入口依赖组装

**文件：** `cmd/xagent/main.go`, `internal/app/deps.go`  
**依赖：** T3, T10, T19, T23, T24, T25

**步骤：**
1. 新建或恢复 `cmd/xagent/main.go` 入口。
2. 创建 JSONL store，保留旧 store 兼容入口。
3. 创建 instructions loader。
4. 创建 memory manager。
5. 创建 sessionctx manager。
6. 通过 app deps 和 orchestrator options 注入。

**验证：** `go build ./cmd/xagent`

## T29: 补充集成测试覆盖注入优先级

**文件：** `internal/sessionctx/manager_test.go`, `internal/orchestrator/chat_test.go`  
**依赖：** T23, T24

**步骤：**
1. 构造项目指令、用户指令和 memory index fixture。
2. 触发 provider request 捕获 stable sections。
3. 断言固定系统提示仍最高，optional sections 内项目级高于用户级。
4. 断言当前用户消息优先于长期记忆边界提醒。

**验证：** `go test -count=1 ./internal/sessionctx ./internal/orchestrator -run TestSessionContextPriority && go test -count=1 ./internal/sessionctx ./internal/orchestrator`

## T30: 补充端到端 fixture 和文档验收准备

**文件：** `docs/session-memory-restore/checklist.md`  
**依赖：** T1-T29

**步骤：**
1. 写入 checklist 验收项。
2. 明确 tmux E2E fixture：项目指令、旧 JSON 会话、JSONL 坏行、memory note/index。
3. 明确 fixture 准备命令、tmux 启动命令和请求内容。
4. 明确观察方式：provider 请求前上下文、TUI status、存储文件、诊断输出。
5. 明确异步记忆等待方式和失败观察项。

**验证：** checklist 包含 AC1-AC28 的可观测检查项，并包含可执行 tmux E2E 步骤。

## T31: 阶段性全量验证

**文件：** 全项目  
**依赖：** T1-T30

**步骤：**
1. 运行全量 Go 测试。
2. 构建 CLI。
3. 使用 gopls 检查新增/修改 Go 文件。
4. 修复发现的问题。

**验证：** `go test -count=1 ./... && go build ./cmd/xagent`

## 执行顺序

```text
T1 → T2 → T3 → T4
        ↘
         T5 → T6 → T7
T1 + T2 → T8 → T9 → T10 → T11
                     ↘
                      T12 → T13 → T14 → T15
T4 + T2 → T16 → T17 → T18 → T19 → T20 → T21 → T22
T7 + T13 + T19 → T23 → T24 → T25
T13 + T14 → T26
T19 → T27
T3 + T10 + T19 + T23 + T24 + T25 → T28
T23 + T24 → T29
T1-T29 → T30 → T31
```
