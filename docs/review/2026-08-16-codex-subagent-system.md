# codex/subagent-system Review Brief

> 生成时间：2026-08-16 11:48 CST
> 分支：codex/subagent-system
> 基于 commit：233b20b [AI][feature][子 Agent 委派与后台任务系统][实现 Defined/Fork、任务调度、隔离权限、结果回流与 TUI 管理]

## 背景与动机

本轮在既有 SubAgent 系统上增加 Git Worktree 隔离，当前 review 聚焦 T55 的 Submit 准入失败结算与 T61 的 Assembly 接线。目标是确认 PreparedTask ownership 不会在失败路径丢失，并确保隔离任务严格继承已解析的全局能力开关。

## 改动策略

Submit 在 ownership 交给 Scheduler 前负责失败结算；交接后由 Scheduler 统一进入 `settling`。Assembly 只向 Runner 注入窄 Worktree Manager/Workspace Binder，并按 Worktree、Workspace、Orchestrator、Inbox、SubAgent、Janitor 的顺序注册 owner，关闭时逆序回收。

## 改动范围

| 模块/职责 | 文件 | 做了什么 |
|-----------|------|----------|
| Admission ownership | `internal/subagent/manager.go` | Prepare 后的 metadata、observer、admission、event 发布失败统一调用 Settle |
| Worktree Assembly | `cmd/xagent/assembly.go`、`cmd/xagent/assembly_subagents.go` | 构造并注入 Worktree graph，注册 owner 并启动 Janitor |
| Workspace context | `internal/workspace/context_binding.go` | 为每个 Worktree 重建项目 Instructions/Memory 上下文 |

## Review 重点

1. **失败结算的可恢复性** — `Settle` panic 或非法返回不能让 Manager 丢失唯一的 PreparedTask/Worktree ownership。
2. **能力开关一致性** — `memory.enabled=false` 必须同时约束主 Session 与隔离 Worktree Session。
3. **生命周期顺序** — Janitor 只能在 owner 注册成功后启动，回滚与关闭必须逆序且单点失败不跳过剩余 owner。
4. **隐私边界** — 外部错误、Workspace 路径和 Memory 内容不得通过诊断或结果投影泄露。

## 已识别风险

- 🟡 T55：Admission `Settle` panic 被吞后会释放 reservation 并丢失 PreparedTask ownership。已用 quarantine、独立 cleanup budget 和 Shutdown fail-close 修复。
- 🟡 T61：`memory.enabled=false` 未传入 Worktree ContextFactory，隔离 Prompt 仍会注入 project Memory。已通过配置冻结和禁用 project/user Memory 修复。
- 🟡 T63：capability probe 在冻结 root 后使用 path-based `MkdirTemp`，root swap 可向 replacement 写入并遗留目录。已改为 handle-relative create/lock/remove，Darwin swap 压测零产物。
- 🟡 T63：initializer/readonly-link capability 未前置，可能先创建 writable roots 再 fatal。已在 scratch/artifact 前探测平台与可信 capability，缺失时只禁用 Worktree。
- 🟡 T63：仅 `LookPath` 和未校验 regular `.git` 会误分类假 Git/畸形 metadata。已改为严格 bounded linked metadata 校验和真实只读 Git probe。

## 测试情况

**已覆盖：**

- Admission metadata、observer、二次 admission、Queued publish 失败各自 Settle 一次。
- Worktree owner 注册顺序、non-Git shared-only、清理错误脱敏。
- `memory.enabled=false` 下 ContextFactory 不构造或注入 project Memory，配置指针修改不影响冻结值。

**缺失：**

- `Settle` panic/非法 Completion 后 ownership 可恢复，Shutdown 等待且不双释放；Admission target 与 race 已通过。
- Assembly 真实 Worktree Runtime 的 memory canary 在 `memory.enabled=false` 时不可见；target 与 race 已通过。
- Capability unavailable 的 root-swap、malformed `.git`、假 Git、Windows initializer 和缺 readonly-link 测试均已通过。

## 结论

🟢 五条 P1 均已完成修复；全仓 test、关键包 race、vet、build 与跨平台编译均已通过。人工 tmux 验收仍按 checklist 保留待执行。
