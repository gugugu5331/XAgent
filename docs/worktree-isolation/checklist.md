# 子 Agent Worktree 隔离 Checklist

> 状态：已审批
> 依赖：[spec.md](./spec.md)、[plan.md](./plan.md)、[task.md](./task.md)
> 每一项必须通过运行测试、命令或真实场景观察验证；代码存在但没有行为证据不算通过。
>
> 2026-08-16 自动化验收：I1–I4、AC1–AC50、Q1–Q8 已通过，证据见 [verification.md](./verification.md)。AC51、E1–E3 要求人工 tmux/真实会话观察，尚未执行并保持未勾选。

## 实现完整性

- [x] I1 Role、配置、Worktree、Workspace、工具绑定、Prompt Scope、Hook/MCP、Orchestrator、SubAgent、Janitor、Assembly 和 UI 模块均已实现，且没有未实现的 panic/stub 分支。（验证：`go build ./cmd/xagent`，并核对 `go test ./...` 没有因未实现分支 skip）
- [x] I2 `worktree.Manager`、`workspace.Factory`、`sessionctx.StablePreparer`、Tool WorkspaceBinder 和两阶段 PreparedTask 均至少被一个生产调用方使用。（验证：`go build ./...`，结合 `rg` 检查接口实现和 Assembly 注入链）
- [x] I3 未声明隔离的 shared 路径仍使用原有工具、Prompt、权限和任务生命周期，且不会触发 Worktree Git 调用。（验证：`go test ./internal/orchestrator ./internal/subagent ./cmd/xagent -run 'Shared|WorktreeDisabled|Baseline'`）
- [x] I4 Worktree 管理 capability 不进入模型可见 Registry、Prompt、MCP DTO 或 Hook 输入。（验证：契约测试及 `rg -n 'worktree\.Manager|GitMutator' internal/tool internal/provider internal/hook`，期望无 capability 暴露）

## 角色声明与任务归属

- [x] AC1 未声明 isolation 和 `isolation: worktree` 分别保持共享与启用隔离；显式 `shared`、空值、未知字符串和非字符串均被拒绝并产生脱敏诊断。（验证：`go test ./internal/agentrole -run Isolation`）
- [x] AC2 隔离角色的 Defined、带隔离角色的 Fork 获得独立 Worktree；无角色 Fork 和非隔离角色保持共享目录。（验证：`go test ./internal/orchestrator -run 'DefinedWorktree|ForkWorktree|Shared'`）
- [x] AC3 并行提交两个相同隔离角色任务时，Workspace ID、目录和本地分支均不同；重启只识别遗留资源，不恢复模型执行或伪造运行状态。（验证：`go test ./internal/orchestrator ./cmd/xagent -run 'ParallelWorktree|RestartRecovery'`）

## 受控根、忽略规则与名称安全

- [x] AC4 clean 仓库创建和删除隔离任务后，主目录 Git 状态不出现受管目录；角色、Skill 和其他 `.xagent` 内容仍可见。（验证：真实临时仓库执行 `git status --porcelain`，并运行 `go test ./cmd/xagent -run WorktreeIgnore`）
- [x] AC5 受管根未精确忽略、已被跟踪或忽略范围过大时，创建在产生 Worktree/branch 前失败，运行时不修改或提交 ignore 文件。（验证：`go test ./internal/worktree ./cmd/xagent -run 'CheckIgnore|WorktreeIgnore'`）
- [x] AC6 合法嵌套名称得到稳定映射；空段、`.`、`..`、`.git`、`.lock`、绝对路径、反斜杠、控制字符、超长/超深名称和大小写等价冲突在任何写操作前被拒绝。（验证：`go test ./internal/worktree -run 'LogicalName|CaseCollision'`）
- [x] AC7 父目录/中间段符号链接和“检查后、操作前”替换竞态使创建、恢复、删除失败关闭，仓库外、主目录和其他 Worktree 不被修改。（验证：`go test ./internal/worktree -run 'Symlink|Traversal|TOCTOU'`）

## 创建、基准 commit 与持久状态

- [x] AC8 主目录同时存在 staged、unstaged、untracked 内容时仍可创建；子 Worktree 初始树严格等于锁内解析的 HEAD commit，不包含 dirty 内容。（验证：`go test ./internal/worktree -run IntegrationCreateDirtyBase`）
- [x] AC9 非 Git、unborn HEAD、非 commit HEAD、Git 不支持 Worktree和分支冲突只使隔离任务失败，不留下可用工作区，普通功能继续运行。（验证：`go test ./internal/worktree ./cmd/xagent -run 'UnsupportedRepository|WorktreeUnavailable'`）
- [x] AC10 在创建意图、分支、worktree add、初始化、ready 发布和删除阶段崩溃后，重启把残留分类为可继续、可回滚、partial 或 manual，不把半写元数据当成功。（验证：`go test ./internal/worktree -run 'Crash|Partial|ManualAttention'`）
- [x] AC11 两个进程竞争同一身份只产生一个目录/分支；不同 Workspace 仅在 Git 公共状态步骤串行，执行阶段可并行且无死锁、无限等待。（验证：`go test ./internal/worktree -run CrossProcessLifecycle`）

## 只读快速恢复

- [x] AC12 完整工作区快速恢复只执行 allowlist 内的 GitReader 查询和文件读取，不执行 add/remove/repair/prune/config write/ref write/初始化。（验证：`go test ./internal/worktree -run RecoveryReadOnly`，检查记录型 fake 的调用列表）
- [x] AC13 篡改权威元数据、工作区标记、`.git` 指向、branch tip、Worktree 注册、仓库身份或目录路径后恢复均被拒绝，现场不被接管、修复或删除。（验证：`go test ./internal/worktree -run RecoveryIdentityMismatch`）
- [x] AC14 删除或修改 manifest 中复制文件、软链和 ignored 文件后恢复失败，现有文件不被重新复制、覆盖或修复。（验证：`go test ./internal/worktree -run RecoveryManifestMismatch`）

## 环境初始化

- [x] AC15 显式配置复制、Git hooks、只读依赖软链和 ignored-copy 后，仅声明目标出现；未声明 `.env`、本地文件和依赖不被扫描或复制。（验证：`go test ./internal/worktree -run 'InitCopy|IgnoredCopy|GitHooks|InitLink'`）
- [x] AC16 路径遍历、源内 symlink、特殊文件、超大/超多/超深源和特殊权限位均失败关闭；复制文件不扩大权限。（验证：`go test ./internal/worktree -run 'InitCopySafety|InitLimits|InitPermissions'`）
- [x] AC17 允许的只读依赖可读不可写；指向主目录、其他 Worktree、Git metadata 或可写依赖的软链初始化失败。（验证：`go test ./internal/worktree ./internal/workspace -run 'InitLink|ReadonlyDependency'`）
- [x] AC18 Worktree 专属 Git hook 只影响目标 Worktree，不改变主目录、其他 Worktree和用户全局 Git 配置。（验证：`go test ./internal/worktree -run GitHooksIsolation`）
- [x] AC19 初始化每一步失败时，未变化且完全可归因的产物安全回滚；加入未知文件或修改产物时保留现场且不强删。（验证：`go test ./internal/worktree -run InitRollback`）
- [x] AC20 初始化源、错误和环境中的模拟密钥、Token、用户路径及超长内容不进入日志、事件、诊断、metadata 或任务结果，输出已脱敏限长。（验证：`go test ./internal/worktree ./internal/orchestrator -run 'InitSecret|Redact|Canary'`）

## 任务级运行时与工具边界

- [x] AC21 主 Agent 和两个隔离子 Agent 同时读写同名文件时，各自只看到自身工作区，子 Agent 写入不影响主目录或其他 Worktree。（验证：`go test ./internal/orchestrator ./cmd/xagent -run WorktreeE2E`）
- [x] AC22 工具/子进程收到任务独立的绝对 cwd、安全根、写 capability 和临时目录；进程全局 cwd 未变化，主根执行对象未被复用。（验证：`go test ./internal/workspace ./internal/tool -run 'Factory|WorkspaceBinding|NoChdir'`）
- [x] AC23 正常命令通过绝对路径、`..` 或 symlink 写主目录/其他 Worktree 时被拒绝；文档明确这不是同用户恶意进程的 OS 沙箱。（验证：`go test ./internal/safefs ./internal/tool -run 'Traversal|Symlink|BashContainment'`，并 review 用户文档）
- [x] AC24 隔离任务 runtime hook 的项目 identity、cwd、配置来源和写边界绑定当前 Worktree；无法任务化 action 被禁用，shared hook 行为不变。（验证：`go test ./internal/hook ./internal/workspace -run 'WorkspaceFactory|CommandContainment'`）
- [x] AC25 Independent、Contextual、Fixed、Unknown 四类工具中，仅通过适配的前两类出现在模型定义；后两类和伪造调用不执行，并返回有界过滤诊断。（验证：`go test ./internal/tool -run 'WorkspacePolicy|WorkspaceBinding|ForgedCall'`）
- [x] AC26 固定主 cwd 的本地 MCP 和仅远端自述 Independent 的 MCP 被过滤；只有本地可信配置声明 Independent 的 MCP 可用。（验证：`go test ./internal/mcpclient ./internal/tool -run Workspace`）

## Prompt、项目指令、Memory 与缓存

- [x] AC27 Defined 隔离任务加载 Worktree 的项目指令、路径说明和项目 Memory，同时继续使用委派时冻结的 Role 快照。（验证：`go test ./internal/orchestrator ./internal/sessionctx -run DefinedWorktree`）
- [x] AC28 Fork 保留父消息和 Global/User 内容，移除主目录 Project/Runtime 内容，并从 Worktree 重建项目指令、Memory 和路径说明。（验证：`go test ./internal/orchestrator ./internal/provider -run ForkWorkspace`）
- [x] AC29 主目录与两个 Worktree 对同一相对路径、指令和 Memory key 不跨根命中缓存；进入/退出不触发全局缓存清空。（验证：`go test ./internal/tool ./internal/instructions ./internal/memory -run Workspace`）
- [x] AC30 只有任务私有 Prompt 使用绝对 Worktree 路径；TUI、持久日志、跨任务事件和最终结果只显示稳定 ID 或仓库相对位置。（验证：`go test ./internal/orchestrator ./internal/app -run 'WorkspacePath|WorkspaceSummary|Redact'`）

## 结算、终态与失败路径

- [x] AC31 Worktree 创建后发生参数校验、准入关闭、队列拒绝、事件发布或 Runner 准备失败时，每条路径恰好结算一次，无资源泄漏且不提前发布终态。（验证：`go test ./internal/subagent ./internal/orchestrator -run AdmissionSettlement`）
- [x] AC32 正常、失败、取消、超时、达到轮次上限、排队取消和应用关闭均执行幂等结算；结算不执行 commit/push/fetch/merge/rebase/upstream 修改。（验证：`go test ./internal/subagent -run 'Settling|Cancel|Timeout|Shutdown'`，检查 fake Git 调用）
- [x] AC33 子进程或 runtime hook 延迟退出时，系统有界等待/取消后才释放 lease；无法确认停止则保留并返回“结算未完成”，不提前报告清理。（验证：`go test ./internal/workspace ./internal/subagent -run 'CloseTimeout|SettlementIncomplete'`）
- [x] AC34 迟到 Runner completion、重复取消和重复 Settle 只产生一个一致终态，迟到事件不能覆盖执行/Workspace 快照。（验证：`go test ./internal/subagent ./internal/orchestrator -run 'LateCompletion|IdempotentSettle'`）

## 变更保护与安全删除

- [x] AC35 staged、unstaged、conflict、intent-to-add、untracked、未知 ignored、变化的初始化文件/软链和子模块变化均被识别为受保护成果并拒绝删除。（验证：`go test ./internal/worktree -run ProtectedChanges`）
- [x] AC36 无新 commit、无 upstream、本地 upstream、缺失 remote-tracking ref、tip 不可达、tip 可达和网络不可用场景中，只有本地可证明 tip 已包含于有效远端 tracking ref 时继续删除；检查不联网。（验证：`go test ./internal/worktree -run Unpushed`）
- [x] AC37 dirty/未推送工作区保留目录与本地分支；clean 无提交/clean 已推送工作区删除目录和精确临时分支；远端、主分支和用户历史不变。（验证：`go test ./internal/worktree -run IntegrationSettlement`）
- [x] AC38 删除前 branch 移动、其他 Worktree 使用分支、目录身份替换、lease 抢占均阻止删除；目录已删而 branch 未删被记录为 partial，不退化为递归强删。（验证：`go test ./internal/worktree -run 'SafeDelete|DeletePartial|RefCAS'`）
- [x] AC39 每个终态包含 isolation、Workspace ID、BaseOID、临时分支、settlement、cleanup、retained 和原因，不含不必要绝对路径。（验证：`go test ./internal/subagent ./internal/orchestrator -run WorkspaceSummary`）

## 后台过期清理

- [x] AC40 可控时钟下，TTL 从首次终态或最后有效 lease 时刻计算；读取、重复扫描和检查失败不刷新过期时间。（验证：`go test ./internal/worktree -run JanitorTTL`）
- [x] AC41 路径越界、无权威 metadata、仓库不匹配、active、dirty、unpushed、初始化产物变化候选均不删；只有三层过滤全部通过的安全候选被删。（验证：`go test ./internal/worktree -run JanitorFilter`）
- [x] AC42 一个进程 Active、另一个 Janitor 扫描时，Janitor 因无法取得独占 lease 跳过；任务结束并满足保护条件后后续扫描才删除。（验证：`go test ./internal/worktree -run CrossProcessLifecycle`）
- [x] AC43 候选数、总时间或并发达到单轮预算时停止本轮，前台仍可用，剩余候选留后续扫描；诊断聚合、限频并记录丢弃数。（验证：`go test ./internal/worktree -run 'JanitorBudget|JanitorDiagnostics'`）

## 兼容性、平台与资源治理

- [x] AC44 不使用隔离角色时，普通对话、Defined/Fork、Skill、Plan Mode、权限、Memory、MCP、Hook、会话恢复和前后台任务与基线一致，且无 Worktree Git 调用。（验证：`go test ./...` 加 shared-mode Git recorder 回归测试）
- [x] AC45 创建的是标准 Git Worktree 和普通本地分支，不修改全局配置；缺少 Git/安全锁能力的平台只禁用隔离，普通功能可运行。（验证：真实 `git worktree list --porcelain`、`git show-ref`，以及 `go test ./cmd/xagent -run WorktreeUnavailable`）
- [x] AC46 Shell 元字符显示内容、超大 Git 输出、超多初始化规则和大量候选不会被 shell 二次解释或突破内存、句柄、进程、锁等待和时间硬上限，超限只影响当前阶段。（验证：`go test ./internal/worktree ./internal/workspace -run 'Limits|Budget|ShellArgument'`）
- [x] AC47 同仓库和不同仓库达到现有 SubAgent 并发上限时，Git 公共状态安全串行，无关任务执行仍并行，无死锁、重复资源或全局性能退化。（验证：`go test -race ./internal/worktree ./internal/workspace ./internal/subagent ./internal/orchestrator -run Concurr`）

## 故障注入与真实 Git

- [x] AC48 可替换 Git、时钟、锁、文件系统和身份源覆盖单元/故障测试；临时真实 Git 仓库对创建、恢复、hooks、dirty、upstream、删除和并发得出一致结果。（验证：`go test ./internal/worktree -run 'Integration|FaultInjection'`）
- [x] AC49 创建、初始化、准入、进入、终态、目录删除和分支删除逐阶段崩溃，同时覆盖 traversal、symlink swap、大小写碰撞、伪造 metadata、ignored 变化和 Unknown 工具；没有误删、越界写或错误成功。（验证：`go test ./internal/worktree ./internal/tool ./internal/orchestrator -run 'Crash|TOCTOU|Forgery|Unknown'`）
- [x] AC50 格式化、静态检查、构建、全部单元/集成测试、race 和真实 Git 测试全部通过；任何实现层缺失时不得标记完成。（验证：执行“编译与质量门禁”全部命令并保存实际输出）

## 编译与质量门禁

- [x] Q1 Go 文件已格式化。（验证：运行 `gofmt` 后 `git diff --check` 无格式残留）
- [x] Q2 所有单元和集成测试通过。（验证：`go test ./...`）
- [x] Q3 目标并发包通过 race 检测。（验证：`go test -race ./internal/worktree ./internal/workspace ./internal/subagent ./internal/orchestrator`）
- [x] Q4 静态检查通过。（验证：`go vet ./...`）
- [x] Q5 XAgent 可构建。（验证：`go build ./cmd/xagent`）
- [x] Q6 Git diff 无空白错误，受管目录未被跟踪，且不存在 `os.Chdir`、强制 Worktree 删除或远端分支删除实现。（验证：`git diff --check`、`git ls-files '.xagent/worktrees/**'` 为空，并对本次改动执行禁止模式扫描）
- [x] Q7 测试未接触用户仓库、用户全局 Git 配置或网络；环境无 Git 时只明确 skip 真实集成层。（验证：检查测试环境变量和临时目录，运行断网测试）
- [x] Q8 用户已有未跟踪 `.xagent/` 内容和 `cmd/xagent/project_skills_test.go` 未被覆盖或纳入本功能提交。（验证：实现前后快照及最终 `git status --short`）


- [ ] AC51 在 tmux 中启动真实 XAgent，使用 `isolation: worktree` 且具备安全读写工具的角色发起真实修改任务；观察自动创建独立 Worktree、私有 Prompt 路径正确、修改只落入子目录、先结算后返回结果，并分别完成“无变更自动清理”和“有变更安全保留”两条流程。（验证：保存 tmux 操作记录、任务状态、`git worktree list --porcelain`、主目录/子目录 `git status --porcelain` 和最终 WorkspaceSummary）
- [ ] E1 同时运行主 Agent 和两个隔离子 Agent 修改同名文件；观察三个工作目录结果互不覆盖，两个子任务目录/分支唯一，主目录 dirty 内容不进入子任务。（验证：真实 XAgent/tmux 场景及三个目录内容对比）
- [ ] E2 重启后对保留 Worktree 执行快速恢复；观察只读恢复成功但模型任务不续跑。篡改 manifest 后再次恢复，观察拒绝且现场保留。（验证：记录重启前后任务列表、Git recorder/诊断和文件状态）
- [ ] E3 推进可控 TTL 并运行 Janitor；观察 active、dirty、unpushed 和身份异常候选被跳过，只有完全安全候选删除，前台任务不被阻塞。（验证：Janitor 聚合诊断、Worktree 列表和前台任务时序）

## 验收记录模板

验收执行时为每项补充：执行时间、命令或操作、实际结果、证据位置、通过/不通过。任何不通过项修复后必须重新执行原验证，不得只更新文字结论。
