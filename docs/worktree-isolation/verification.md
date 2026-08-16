# 子 Agent Worktree 隔离验收记录

> 执行日期：2026-08-16（Asia/Shanghai）
> 分支：`codex/subagent-system`
> 基线 commit：`233b20b`
> 范围：T1–T69 的自动化实现与质量门禁

## 结论

自动化验收通过。Worktree 创建/恢复/初始化/结算/Janitor、任务级 Workspace、Prompt/Memory/Hook/Tool 隔离、两阶段任务结算、Assembly 降级、UI 投影、真实 Git 双任务 E2E 和秘密 canary 均已有行为证据。

尚未执行 `checklist.md` 中要求人工 tmux 观察的 AC51、E1、E2、E3。它们保持未勾选；其中对应的自动化场景已由真实临时 Git、恢复/Janitor 集成测试覆盖，但不冒充人工真实会话证据。

## 最终质量门禁

| 门禁 | 实际命令 | 结果 |
| --- | --- | --- |
| 全部单元/集成测试 | `GOSUMDB=sum.golang.org go test ./... -count=1 -timeout=300s` | 通过；`internal/worktree` 20.028s |
| 关键并发包 race | `GOSUMDB=sum.golang.org go test -race ./internal/worktree ./internal/workspace ./internal/subagent ./internal/orchestrator -count=1 -timeout=420s` | 通过；31.747s / 2.217s / 2.625s / 21.797s |
| 静态检查 | `GOSUMDB=sum.golang.org go vet ./...` | 通过 |
| 主程序构建 | `GOSUMDB=sum.golang.org go build ./cmd/xagent` | 通过；生成的临时根目录二进制已删除 |
| Windows 编译 | `GOOS=windows GOARCH=amd64 go test -exec=true ./cmd/xagent ./internal/worktree ./internal/workspace -run '^$'` | 通过 |
| Linux 编译 | `GOOS=linux GOARCH=amd64 go test -exec=true ./cmd/xagent ./internal/worktree ./internal/workspace -run '^$'` | 通过 |
| 格式/空白 | `git diff --check` | 通过 |
| 受管目录未跟踪 | `git ls-files '.xagent/worktrees/**'` | 空 |
| 禁止模式 | 扫描 `os.Chdir`、`worktree repair/prune`、`branch -D`、远端删除 ref | 无命中 |

## 关键场景证据

| 场景 | 验证 | 结果 |
| --- | --- | --- |
| 双隔离任务 | `go test ./internal/orchestrator ./cmd/xagent -run WorktreeE2E -count=10` 及 race | 通过；两个真实 Worktree/branch/root 唯一，同名文件互不覆盖，主根 dirty 不变 |
| 能力降级 | `go test ./cmd/xagent ./internal/orchestrator -run WorktreeUnavailable -count=20` 及 race | 通过；非 Git/缺 Git/缺锁/不支持 initializer 只禁用 Worktree，shared 继续 |
| 创建与初始化 | `go test ./internal/worktree -run 'IntegrationCreate|InitCopy|IgnoredCopy|GitHooks|InitLink'` | 已包含于全包通过；真实 Git、hooks、copy/link/ignored-copy 均覆盖 |
| 恢复与删除保护 | `go test ./internal/worktree -run 'IntegrationRecovery|IntegrationSettlement|ProtectedChanges|Unpushed'` | 已包含于全包通过；dirty/unpushed 保留，安全 clean 才删除 |
| 多进程与 Janitor | `go test ./internal/worktree -run 'CrossProcessLifecycle|Janitor'` 及 race | 已包含于全包/race通过；Active/Janitor、双 Janitor、CAS 和预算覆盖 |
| 任务结算 | `go test ./internal/subagent -run 'AdmissionSettlement|Settling|Cancel|Timeout|Shutdown'` 及 race | 通过；所有失败/取消路径先 settling，再终态，owner cleanup 有界 |
| Prompt/上下文 | `go test ./internal/orchestrator ./internal/provider -run 'DefinedWorktree|ForkWorkspace'` | 通过；主 Project 不泄漏，Worktree Project/Runtime 重建 |
| Memory 禁用 | `go test ./internal/workspace ./cmd/xagent -run 'MemoryDisabled|AssemblySubagentMemoryDisabled'` 及 race | 通过；`memory.enabled=false` 不构造/注入 task project Memory |
| 秘密 canary | `go test ./internal/worktree ./internal/app -run 'Secret|Redact|Canary' -count=20` 及 race | 通过；路径、remote/URL、环境、初始化内容和 free error 不进入公共输出/TUI |

## Review 修复记录

- 静态 `tasks` symlink、宽 `.gitignore` 和检查后替换均在首个 Git/Record 副作用前 fail-close。
- Store CAS、Active lease 和 repository lock 已用真实子进程验证跨进程互斥。
- Admission `Settle` panic/非法 Completion 进入 quarantine，不释放 reservation 或丢失 PreparedTask ownership。
- `memory.enabled=false` 同时约束主 Session 与 Worktree Session。
- capability probe 全部 handle-relative；Darwin root swap 压测零产物。
- malformed `.git`、假 Git executable、缺 readonly-link/initializer capability 不再误分类。
- Manager 生命周期公共错误固定分类、限长并保留内部 `errors.Is/As`，不泄漏底层 canary。

详细改动意图与两条中期 review finding 见 [review brief](../review/2026-08-16-codex-subagent-system.md)。

## 保留边界

- Git CLI 不接受已打开的目录 fd；最终 identity 重验与 `git worktree add` 自行解析 pathname 之间无法实现真正的 identity-conditional CAS。实现已把重验贴近 argv 调用并保守拒绝可观测替换，但不把它描述为同用户恶意进程的 OS 沙箱。
- 自动 orphan/superseded manifest 删除因 POSIX 缺少 inode-conditional unlink 而禁用，安全优先；元数据由人工或后续可证明安全的机制治理。
- Windows 当前在 initializer capability 探测阶段禁用 Worktree 隔离；主程序和 shared SubAgent 可运行。
- `Init.Link` 在 Assembly 没有可信 readonly-link capability 来源时禁用 Worktree 隔离，不静默忽略规则。
- linked checkout 的 `.git` 文件会严格验证，但当前版本仍将其作为 optional unavailable，不启用隔离。

## 本地数据保护

最终状态仍保留用户原有未跟踪 `.xagent/` 与 `cmd/xagent/project_skills_test.go`；它们未被读取改写、删除或纳入本功能文件。`/.xagent/worktrees/` 是根 `.gitignore` 的唯一新增规则。
