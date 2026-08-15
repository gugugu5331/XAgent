# XAgent 安全、可靠性与交付质量提升 Tasks（2026-07-30）

> 状态：M0–M5 及 C18/C18c Task 修订已批准（2026-07-31 至 2026-08-03）；Plan C1–C18c 已批准（2026-08-03）；当前目标：T4.29a（2026-08-10）
>
> 前置文档：`spec.md`、`plan.md`（含 C1–C18c）及本 Task（含 C18c 修订）已批准；`checklist.md` 的 C18 修订已批准
>
> 本文只定义实施顺序。所有实施写入均须遵守已批准的 Spec、Plan、Task 与 Checklist；未经批准不得扩展实现边界。

## 执行约束

1. 普通实施叶任务的单个代码改动步骤目标为 2–5 分钟；标题含“原子切换”“门禁”“E2E”“性能”“平台”“删除”或“证据”的任务是协调/验证任务，不声称总运行时间在 5 分钟内。公共契约切换仍必须由同一执行者持锁连续完成，不能留下可编译但不安全的双入口；长运行命令的等待时间与三平台外部 job 单独记录。
2. 每个任务先写或调整对应测试，再完成最小实现，并执行列出的验证；只有看到实际输出后才能标记完成。
3. `cmd/xagent`、`internal/events/events.go`、`internal/app/*` 和 `internal/orchestrator/*` 是共享集成面，同一时刻只允许一个执行者修改。
4. 安全硬边界不得以兼容 shim 弱化。权限 Ticket 与 Executor、MCP ownership、Conversation v2、唯一组装根分别按本文标注的原子切换点一次完成。
5. 删除候选只有在替代实现、调用方迁移、`rg` 零引用、旧格式兼容测试和三平台构建均通过后才能删除。
6. `.claude/worktrees/**`、`.mewcode/**`、`fakeprovider`、webarchive、照片及其他个人附件属于用户本地数据。M0 只允许从 Git index 移除并添加忽略规则，不得删除、移动、覆盖或改写 Git 历史。
7. 测试只能使用固定 canary；默认测试不得读取真实凭据、访问公网或依赖固定短 `sleep`。
8. 所有展示、模型输入、JSONL、memory、诊断和测试失败信息都必须通过 `RuntimeRedactor`；私有 artifact 是唯一允许保留 raw 工具输出的例外。
9. 除仅编译的 `go test -c` 外，所有 Go 测试验证必须使用 `-json`；带 `-run` 的命令只有在输出中出现每个精确 required test/subtest 的 `Action=pass`、次数满足且无 skip/fail 时才通过，零匹配即使进程 exit 0 也失败。T5.26 前逐项保存并核对安全 JSON 证据，T5.26 后一律由 required-test gate 自动判定。
10. 编译测试二进制、coverage、性能 adapter 和中间证据只写入本次执行器以安全临时目录 API 创建、权限为当前用户私有且唯一的绝对目录，并以 `XAGENT_TASK_TMP` 传给固定 argv；执行前必须证明变量非空、目标不在工作区且不是文件系统根。不得使用仓库路径或固定 `/tmp/xagent-*` 名称；任务结束由创建者按精确路径清理，失败时仅保留不含私密内容的摘要。
11. `xagent-check` 的每个 Step 名称、argv、cwd、env、timeout、依赖、required package/test 及期望次数均是闭合清单；实施者不得把表中的正则、package 或 profile 替换为“相关测试”“平台命令”等人工选择。CI/release 接受的 revision 只能是实际 `HEAD` 对应的 40 位小写十六进制 canonical commit OID；短 SHA、tag、branch、大小写变体、前后空白、可解析但不等于 `HEAD` 的 OID 均失败。

## 文件清单

| 操作 | 文件或目录 | 职责 |
|---|---|---|
| 修改 | `.gitignore`, `README.md`, `config.example.yaml`, `go.mod`, `go.sum` | 仓库边界、公开事实、配置示例和平台依赖 |
| 新建 | `.repoaudit.yaml`, `cmd/xagent-repo-check/*`, `internal/repoaudit/*` | index/commit/worktree 只读审计与门禁 |
| 新建 | `.github/xagent-actions.lock.json` | 人工审核、commit-pinned 的 workflow/action 完整依赖闭包 |
| 新建 | `.githooks/pre-commit` | 显式启用、只调用统一审计入口的本地提交门禁 |
| 新建/修改 | `cmd/xagent/{cli,version,assembly,lifecycle,main}*.go`, `cmd/xagent/e2e_*_test.go` | CLI 前置解析、唯一组装根、反向关闭及生产组装 E2E |
| 新建/修改 | `internal/{redact,budget,diagnostics,safefs,proctree,artifact,netpolicy}/**` | 无业务反向依赖的安全基础能力 |
| 修改/迁移 | `internal/{config,permission,tool,instructions,hook}/**` | presence-aware 配置、票据授权、安全执行与 Hook |
| 修改/迁移 | `internal/provider/**`, `internal/mcpclient/**` | 显式流生命周期、协议预算和 MCP ownership |
| 修改/迁移 | `internal/{conversation,contextmgr,sessionctx,prompt,memory,skill}/**` | v2 持久化及安全上下文边界 |
| 修改/迁移 | `internal/{events,orchestrator,app,command,tui}/**` | 有序编排、导航事务、状态边界和响应式 TUI |
| 新建/修改 | `internal/testutil/**`, `internal/e2e/**`, `cmd/xagent-check/**`, `.github/workflows/ci.yml` | 确定性 fixture/E2E 辅助、性能协议和统一质量门禁 |
| 新建/修改 | `docs/index.md`, `docs/**/{spec,plan,task,checklist}.md` | 当前状态、替代关系和证据追溯 |
| 条件删除 | `plan.md`“删除候选”章节列出的旧实现 | 仅在替代实现及所有迁移门禁通过后删除 |

## 实施数值契约快照（随已批准的 Plan C14–C15）

以下快照使实现任务不必自行猜测默认值或硬上限，并与已批准的 C14 共同约束实施。

| 范围 | 默认值 | 不可提高的硬上限 |
|---|---:|---:|
| 模型/会话内联工具预览 | 32 KiB | 1 MiB |
| 单次工具原始捕获与 artifact | 64 MiB | 512 MiB |
| Artifact 总容量 / 保留期 | 1 GiB / 7 天 | 8 GiB / 365 天 |
| Read 单文件读取 | 16 MiB | 256 MiB |
| Grep/Glob 累计扫描 | 256 MiB、10 万文件、2.5 万目录、100 万行 | 2 GiB、100 万文件、25 万目录、1000 万行 |
| Instruction | 单文件 64 KiB、累计 1 MiB、64 文件、展开 2 MiB、深度 5 | 单文件 1 MiB、累计 16 MiB、1024 文件、展开 32 MiB、深度 32 |
| MCP 单次响应 / 工具 / 分页 / 协议错误 | 1 MiB / 128 / 32 / 32 | 16 MiB / 1024 / 128 / 256 |
| Provider 原始响应 / 单事件 / 事件数 | 16 MiB / 1 MiB / 10 万 | 64 MiB / 4 MiB / 100 万 |
| Provider 正文 / thinking / 工具参数 | 8 MiB / 8 MiB / 1 MiB | 32 MiB / 32 MiB / 8 MiB |
| Conversation 单记录 / 单会话 / 列表扫描 | 16 MiB / 256 MiB / 1000 文件与 10 MiB | 64 MiB / 1 GiB / 10 万文件与 1 GiB |
| Diagnostics 项数 / 单项 / 总量 | 100 / 2 KiB / 2 MiB | 1000 / 64 KiB / 16 MiB |
| 取消与关闭清理 | 2 秒 | 2 秒 |

| 公开数值键 | 默认值 | 有效最小值 | 不可提高的硬上限 / 关系 |
|---|---:|---:|---|
| `tool.inline_output_bytes` | 32 KiB | 1 B | 1 MiB；旧 `tool.max_output_bytes` 的唯一迁移目标 |
| `tool.capture_bytes` | 64 MiB | 1 B | 512 MiB |
| `artifact.max_file_bytes` / `max_total_bytes` | 64 MiB / 1 GiB | 1 B / 1 B | 512 MiB / 8 GiB；file 不得高于 total |
| `artifact.retention_days` | 7 | 1 | 365 |
| `files.read_max_bytes` | 16 MiB | 1 B | 256 MiB |
| `files.scan_max_bytes` / `scan_max_files` / `scan_max_directories` / `scan_max_lines` | 256 MiB / 100000 / 25000 / 1000000 | 1 B / 1 / 1 / 1 | 2 GiB / 1000000 / 250000 / 10000000 |
| `instructions.max_file_bytes` / `max_total_bytes` / `max_files` / `max_expanded_bytes` / `max_include_depth` | 64 KiB / 1 MiB / 64 / 2 MiB / 5 | 1 B / 1 B / 1 / 1 B / 1 | 1 MiB / 16 MiB / 1024 / 32 MiB / 32；file 不得高于 total 或 expanded |
| `mcp.max_response_bytes` / `max_tools` / `max_pages` / `max_protocol_errors` | 1 MiB / 128 / 32 / 32 | 1 B / 1 / 1 / 1 | 16 MiB / 1024 / 128 / 256 |
| `llm.stream.max_response_bytes` | 16 MiB | 1 B | 64 MiB |
| `llm.stream.max_event_bytes` / `max_text_bytes` / `max_thinking_bytes` / `max_tool_arguments_bytes` | 1 MiB / 8 MiB / 8 MiB / 1 MiB | 1 B / 1 B / 1 B / 1 B | 4 MiB / 32 MiB / 32 MiB / 8 MiB；分别不得高于 response |
| `llm.stream.max_events` | 100000 | 1 | 1000000 |
| `session.max_record_bytes` / `max_session_bytes` | 16 MiB / 256 MiB | 1 B / 1 B | 64 MiB / 1 GiB；record 不得高于 session |
| `session.max_scan_files` / `max_scan_bytes` | 1000 / 10 MiB | 1 / 1 B | 100000 / 1 GiB |
| `diagnostics.max_items` / `max_item_bytes` / `max_total_bytes` | 100 / 2 KiB / 2 MiB | 1 / 1 B / 1 B | 1000 / 64 KiB / 16 MiB；item 不得高于 total |
| `llm.request_timeout_ms` | 120000 | 1 | 86400000 |
| `llm.thinking.budget_tokens` | 4096 | 1 | 1000000 |
| `agent.max_iterations` / `agent.max_unknown_tool_calls` | 10 / 2 | 1 / 1 | 1000 / 100 |
| `tool.timeout_ms` | 30000 | 1 | 86400000 |
| `context.tool_result_threshold_chars` / `tool_results_threshold_chars` | 32768 / 65536 | 1 / 1 | 1048576 / 4194304；前者不得高于后者 |
| `context.model_window_tokens` | 200000 | 2 | 10000000；分别大于 auto/manual margin 与 recent keep |
| `context.auto_margin_tokens` / `manual_margin_tokens` / `recent_keep_tokens` | 13000 / 3000 / 10000 | 1 / 1 / 1 | 各 1000000，且严格小于 model window |
| `context.recent_keep_messages` / `summary_failure_limit` / `preview_chars` | 5 / 3 / 2000 | 1 / 1 / 1 | 10000 / 100 / 1048576 |
| `mcp.default_timeout_ms` / server `timeout_ms` | 30000 / 继承已 Resolve 的 default | 1 / 1 | 各 86400000；显式 0 不等于未设置 |
| `session.retention_days` / `gap_reminder_days` | 30 / 7 | 1 / 1 | 各 3650 |
| `memory.max_index_lines` / `max_index_bytes` | 200 / 25600 | 1 / 1 | 100000 / 16777216；同时生效 |
| `memory.update_queue_size` / `update_concurrency` | 8 / 1 | 1 / 1 | 1024 / 64；concurrency 不得高于 queue |
| `memory.update_timeout_ms` / `max_candidate_bytes` | 30000 / 65536 | 1 / 1 | 86400000 / 16777216 |
| `lifecycle.cleanup_timeout_ms` | 2000 | 1 | 2000 |

Hook rule `timeout`：command 默认 30 秒、HTTP 默认 10 秒、subagent 默认 30 秒；有效最小值 1 毫秒、硬上限 10 分钟，prompt action 禁止 timeout。所有表中正数键均遵循“未设置才应用默认；显式 0、负数、cap+1、整数溢出和组合冲突均拒绝”，不能静默截断或由 leaf module 二次补默认。

跨文件 owner 固定如下：AppConfig 数值只由 Config decode/merge/Resolve 验证；`hooks.yaml` 必填整数 `version: 1` 且 rule timeout 由 Hook Loader/compile 验证；permission rule 缺失/0 作为 legacy v1 非破坏读取、整数 1 为当前格式，负数、2、溢出或错误类型把该层标为损坏并对危险操作 fail closed；Skill frontmatter `history` 未设置默认 0，isolated 接受 0–1000、shared 只接受 0，其他数值或类型由 Skill parser 拒绝。四类输入不得合并为单一 Config Resolve schema。

## M0：仓库止血与只读审计

### T0.1 固定本地运行产物忽略规则

**文件：** `.gitignore`
**依赖：** 无
**步骤：**
1. 增加 worktree、会话、memory、临时二进制、webarchive 和照片的精确模式。
2. 保留现有规则，不增加会吞掉产品源码或测试 fixture 的宽泛目录模式。

**验证：** 运行 `git check-ignore -v` 检查每类已识别样本均命中预期规则，且 `git check-ignore internal/tool/tool.go` 返回未忽略。
**覆盖：** F1 / AC1。

### T0.2 定义仓库审计类型与策略解析

**文件：** `.repoaudit.yaml`, `internal/repoaudit/types.go`, `internal/repoaudit/auditor.go`, `internal/repoaudit/auditor_test.go`
**依赖：** T0.1
**步骤：**
1. 定义 `Finding`、`Report`、`Policy`、`Source` 和三种审计目标。
2. 严格解析策略；gitlink 声明默认空，fixture 豁免必须包含路径、规则 ID 和内容摘要。
3. 固定 `.repoaudit.yaml` 初始 `max_binary_bytes=1048576`（1 MiB）且不可被单项豁免放大；二进制定义为流式校验中出现 NUL 或无效 UTF-8 的 blob，二进制 allowlist 默认空且每项必须精确绑定仓库相对路径、SHA-256 和字节数，只能豁免类型规则，不能豁免二进制尺寸、secret 或私有路径规则；纯文本不因本键单独被判超限。

**验证：** `go test -json -count=1 ./internal/repoaudit -run 'TestPolicyRejectsUnknownFields|TestFixtureExemptionRequiresDigest|TestPolicyFixesOneMiBBinaryLimit|TestBinaryAllowlistRequiresExactIdentity'` 通过。
**覆盖：** F2、F33 / AC2、AC33。

### T0.3 实现 NUL-safe Git 数据源

**文件：** `internal/repoaudit/git_source.go`, `internal/repoaudit/git_source_test.go`, `internal/repoaudit/preservation.go`, `internal/repoaudit/preservation_test.go`
**依赖：** T0.2
**步骤：**
1. 分别读取 Git index、指定提交树和显式 worktree，使用 NUL 分隔处理路径。
2. 将 Git/I/O 失败与规则 finding 分开返回，不解析面向人的状态文本。
3. 定义 M0 专用 NUL-safe preservation manifest model：对每个精确 index 条目记录原始路径字节、mode、OID，并用 handle-relative/no-follow inventory 冻结工作区对象类型与 mode；普通文件记录字节数和内容 SHA-256，符号链接只散列 link target 字节且不跟随，目录以 NUL-safe 递归清单形成摘要。model 不自行选择路径或写文件，持久化只交给 T0.3a 的 evidence owner。

**验证：** `go test -json -count=1 ./internal/repoaudit -run 'TestGitSourceHandlesWhitespaceAndNewlines|TestPreservationManifestIsNULSafeAndNeverFollowsLinks|TestPreservationManifestDetectsTypeModeOIDAndContentChange'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F1、F2 / AC1、AC2。

### T0.3a 建立跨任务 evidence owner、lease 与原子发布基座

**文件：** `internal/repoaudit/evidence.go`, `internal/repoaudit/evidence_unix.go`, `internal/repoaudit/evidence_windows.go`, `internal/repoaudit/evidence_unsupported.go`, `internal/repoaudit/evidence_test.go`, `internal/repoaudit/evidence_unix_test.go`, `internal/repoaudit/evidence_windows_test.go`
**依赖：** T0.3
**步骤：**
1. 封闭 `XAGENT_EVIDENCE_DIR`、task/workspace/source 与私有 control root 的绝对 no-follow identity、互不包含/alias 和专用 parent 关系；control root 内容只允许 evidence dir、`.xagent-evidence.lease` 与 `.xagent-evidence.active`。
2. 实现 Plan C17 的 A/B/C/D bootstrap：POSIX 用同一不可继承 owner FD 的 `flock(LOCK_EX)`，Windows 用 non-inheritable handle 和固定 `[0,1)` `LockFileEx`/`UnlockFileEx`；两平台都按 lease→control、dir→dir/control、active→active/control 的顺序同步并锁后复验，`lease` 缺失而 dir/active 存在时永久 fail closed。
3. 实现同目录固定 temp、no-follow/create-exclusive、完整回读校验、file sync、no-replace publish、parent sync 的通用 publisher，以及 final/temp 的幂等恢复闭集；既有合法但非期望内容、第二 temp、类型/权限/identity 漂移全部拒绝。
4. POSIX 固定 private file/dir mode 和 owner，Windows 固定 current-user/system 私有 DACL；lease/active/control root 不进入 archive、不能被 task cleanup 删除，也不提供删除或重建入口。child process 的 FD/HANDLE 白名单必须排除 lease。

**验证：** `go test -json -race -count=20 ./internal/repoaudit -run 'TestEvidenceRootsRejectContainmentAliasAndIdentityDrift|TestEvidenceLeaseSerializesProcessesAndIsNotInherited|TestEvidenceBootstrapCrashMatrixIsDurable|TestAtomicPublisherRecoversOnlyClosedStates|TestMissingLeaseWithExistingLedgerFailsClosed|TestEvidenceDACLAndModesAreExact'` 通过；Windows 跨进程测试证明第二 owner 在第一 owner 释放 `[0,1)` 前不能进入 ledger。
**覆盖：** F1、F31、F32 / AC1、AC31、AC32、AC34、AC37。

### T0.4 实现私有目录规则

**文件：** `internal/repoaudit/private_paths.go`, `internal/repoaudit/private_paths_test.go`
**依赖：** T0.3
**步骤：**
1. 检测批准范围中的私有目录和附件模式。
2. Finding 只保存规则 ID、路径和安全说明，不读取或回显文件正文。

**验证：** `go test -json -count=1 ./internal/repoaudit -run TestPrivatePathsAreRejectedWithoutContent` 通过。
**覆盖：** F1、F2 / AC1、AC2。

### T0.5 实现秘密扫描规则

**文件：** `internal/repoaudit/secrets.go`, `internal/repoaudit/secrets_test.go`
**依赖：** T0.2
**步骤：**
1. 对有界文本输入执行疑似凭据规则并支持精确 fixture 豁免。
2. 报告中只保留位置、规则和摘要，不保留匹配原文。

**验证：** `go test -json -count=1 ./internal/repoaudit -run 'TestSecretCanaryFailsWithoutLeak|TestExactFixtureExemption'` 通过，失败输出不含 canary。
**覆盖：** F2、F7 / AC2、AC7。

### T0.6 实现 gitlink 一致性规则

**文件：** `internal/repoaudit/gitlinks.go`, `internal/repoaudit/gitlinks_test.go`
**依赖：** T0.3
**步骤：**
1. 识别 mode `160000` 条目。
2. 仅当策略显式声明且 `.gitmodules` 有一致映射时放行；不得自动生成映射。

**验证：** `go test -json -count=1 ./internal/repoaudit -run TestUndeclaredGitlinkIsRejected` 通过。
**覆盖：** F1、F2 / AC1、AC2。

### T0.7 实现二进制与尺寸规则

**文件：** `internal/repoaudit/binaries.go`, `internal/repoaudit/binaries_test.go`
**依赖：** T0.3
**步骤：**
1. 按 T0.2 固定策略检测超过 1 MiB 的二进制和未精确允许的二进制；恰好 1 MiB 允许继续接受其他规则检查，1 MiB＋1 byte 的二进制必须命中尺寸规则，同尺寸文本不因二进制尺寸键失败。
2. 使用流式、有界检测，报告 blob 大小但不复制正文；精确二进制 allowlist 身份不匹配时失败，且永远不能放行超限、secret 或私有路径。

**验证：** `go test -json -count=1 ./internal/repoaudit -run 'TestBinarySizeBoundaryAtOneMiB|TestTextIsNotSubjectToBinarySizeLimit|TestUnlistedBinaryIsRejected|TestBinaryAllowlistCannotBypassOtherRules'` 通过。
**覆盖：** F2 / AC2。

### T0.8 聚合审计结果与退出分类

**文件：** `internal/repoaudit/auditor.go`, `internal/repoaudit/auditor_test.go`
**依赖：** T0.4、T0.5、T0.6、T0.7
**步骤：**
1. 以确定顺序聚合 findings 并限制报告数量。
2. 区分通过、规则命中和审计本身失败三种结果。

**验证：** `go test -json -count=1 ./internal/repoaudit -run TestAuditOrderingAndExitClass` 通过。
**覆盖：** F2 / AC2。

### T0.9 提供统一仓库检查命令

**文件：** `cmd/xagent-repo-check/main.go`, `cmd/xagent-repo-check/main_test.go`
**依赖：** T0.8、T0.3a
**步骤：**
1. 支持默认 index、显式 commit 和显式 worktree 三种只读模式。
2. 输出安全摘要，并映射通过、finding、工具错误到不同退出码。
3. preservation action 闭集固定为 `freeze`、`remove-index`、`verify-immediate`、`verify-final`，不接受 manifest/path/glob argv；前三者只允许 T0.10 在同一 owner lease 下顺序执行，`verify-final` 只允许 T5.53a/T5.53b。唯一 manifest 逐段固定为 `<EVIDENCE_DIR>/preservation/m0.manifest`。
4. freeze 使用 T0.3a 原子发布并实现 Plan C17 的 pre-index 恢复前提；remove-index 只从已验证 manifest 取得批准的 35 条并持 lease 跨 index mutation/post-check/fsync；verify-immediate/final 都从同一 handle 重算 identity。输出只含 action、count、集合 digest、manifest SHA 与 pass/error code，不含路径、link target、明细或正文。

**验证：** `go test -json -count=1 ./cmd/xagent-repo-check -run 'TestAuditModesAndExitCodes|TestPreservationActionsHaveNoPathSelector|TestPreservationFreezeRecoveryRequiresUnchangedPreIndexState|TestPreservationRemoveIndexHoldsEvidenceLease|TestPreservationVerifyNeverFollowsLinks'` 通过；required tests 均 pass，fixture 中规则命中退出非零且不显示 secret。
**覆盖：** F2、F32 / AC2、AC32。

### T0.10 将已识别私有条目仅移出 Git index

**文件：** Git index（不修改对应工作区文件）；工作区外 `XAGENT_EVIDENCE_DIR` 的固定 preservation ledger
**依赖：** T0.1、T0.9
**步骤：**
1. 在任何 index mutation 前用 `git ls-files -s -z` 冻结“路径字节＋mode＋object ID”的 NUL-safe 精确清单，要求恰为已确认的 35 项且全部属于批准的数据边界；集合、数量或对象身份变化时立即停止并重新请求批准。
2. 在同一 evidence owner lease 下先运行唯一 `freeze`，把 pre-change index identity、35 条目标、`.gitmodules` absent sentinel 与 no-follow 工作区指纹原子发布到固定 `preservation/m0.manifest`；只有 final manifest 完整持久化并回读一致后才继续。
3. 运行唯一 `remove-index`，只按 manifest 中 35 条执行 index-only removal并同步 index/parent；不得由调用者传路径，不得使用 glob、递归删除、移动、历史重写或创建 `.gitmodules`。随后在同一 lease 下执行 `verify-immediate`，证明目标已退出 index而所有工作区对象及 `.gitmodules` sentinel 均未变化。
4. manifest 作为跨任务唯一例外保留到 T5.54 finalize-archive；T0.10 及 task cleanup 都不得删除、移动、覆盖 evidence dir、manifest、lease 或 active sentinel。失败只保留 ledger和安全摘要，不递归清理父目录。

**验证：** `go run ./cmd/xagent-repo-check --preservation=verify-immediate` 返回安全摘要 `count=35` 且 manifest/集合摘要与 freeze 一致；`go run ./cmd/xagent-repo-check --source=index` 通过，并由 verifier 明确证明没有创建 `.gitmodules`。不得用 `test -e`、文本分隔、path argv 或跟随符号链接的检查替代。
**覆盖：** F1 / AC1。

### T0.11 建立 M0 回归门禁

**文件：** `internal/repoaudit/*_test.go`, `cmd/xagent-repo-check/main_test.go`
**依赖：** T0.10
**步骤：**
1. 增加 secret、gitlink、私有目录、附件和超限二进制的表驱动失败样本。
2. 增加正常源码和精确 fixture 豁免的通过样本。
3. 增加 preservation 四 action 闭集、A–D lease/bootstrap、freeze/remove-index/verify-immediate 崩溃恢复与 manifest 禁止提前清理的回归；测试只使用 synthetic index/ledger，不能触碰用户真实 index、`.gitmodules` 或数据。

**验证：** `go test -json -count=1 ./internal/repoaudit ./cmd/xagent-repo-check` 与当前 index 审计均通过。
**覆盖：** F1、F2 / AC1、AC2。

## M1：安全基础能力

### T1.1 建立不可伪造的 `SafeText`

**文件：** `internal/redact/safe_text.go`, `internal/redact/safe_text_test.go`
**依赖：** T0.11
**步骤：**
1. 定义字段不导出的 `SafeText` 及只读访问能力。
2. 禁止普通调用方通过 struct literal 注入未经脱敏文本。

**验证：** `go test -json -count=1 ./internal/redact -run TestSafeTextConstructionBoundary` 通过。
**覆盖：** F7 / AC7。

### T1.2 实现进程级运行时秘密注册表

**文件：** `internal/redact/runtime.go`, `internal/redact/runtime_test.go`
**依赖：** T1.1
**步骤：**
1. 实现并发安全的 `RegisterSecret` 与 `Redact`。
2. 覆盖短值、运行期展开值、重复注册和派生错误链。

**验证：** `go test -json -count=1 ./internal/redact -run TestRuntimeRedactorRegistersExpandedSecrets` 通过。
**覆盖：** F7 / AC7。

### T1.3 收敛结构化脱敏入口

**文件：** `internal/redact/redact.go`, `internal/redact/redact_test.go`
**依赖：** T1.2
**步骤：**
1. 让 header、URL userinfo/query、env、嵌套 JSON 和 PEM 样本走同一运行时 redactor。
2. 确保脱敏结果保持有效 UTF-8 且不返回注册秘密。

**验证：** `go test -json -count=1 ./internal/redact -run TestRuntimeSecretCanaryMatrix` 通过。
**覆盖：** F7 / AC7。

### T1.4 定义预算维度、默认值与硬上限

**文件：** `internal/budget/limits.go`, `internal/budget/error.go`, `internal/budget/counter_test.go`
**依赖：** T0.11
**步骤：**
1. 按本文件“实施数值契约快照”逐项固定字节数、文件数、目录数、行数、事件数等累计 `Dimension`、`Limits`、默认值和不可提高的硬上限；C14 的公开配置值由 T2.4 的 Config Resolve 唯一负责，不在 budget 包重复补默认。
2. 定义不携带载荷的 `LimitError`；每个维度表驱动覆盖 default、有效最小值、cap、cap+1、显式 0、负数和整数溢出，只有该维度契约明确允许的 0 才可接受。

**验证：** `go test -json -count=1 ./internal/budget -run 'TestEveryBudgetDimensionDefaultMinCapAndOverflow|TestLimitsRejectNegativeAndAboveHardCap'` 通过，JSON pass 记录必须包含两个测试且无 skip。
**覆盖：** F9、F12–F14 / AC9、AC12–AC14。

### T1.5 实现并发安全累计 `Counter`

**文件：** `internal/budget/counter.go`, `internal/budget/counter_test.go`
**依赖：** T1.4
**步骤：**
1. 实现消费前检查、Remaining 和不可变 Snapshot。
2. 保证多调用方共享累计值，不允许逐文件或逐事件重置。

**验证：** `go test -json -race -count=1 ./internal/budget -run TestCounterConsumesAtomically` 通过。
**覆盖：** F9、F12–F14 / AC9、AC12–AC14。

### T1.6 封闭预算整数溢出

**文件：** `internal/budget/overflow_test.go`, `internal/budget/counter.go`
**依赖：** T1.5
**步骤：**
1. 覆盖加法溢出、极大 observed 和并发临界值。
2. 溢出统一返回 `LimitError`，不得回绕为可用预算。

**验证：** `go test -json -count=1 ./internal/budget -run TestCounterOverflowFailsClosed` 通过。
**覆盖：** F9 / AC9。

### T1.7 实现诊断净化管线

**文件：** `internal/diagnostics/sanitize.go`, `internal/diagnostics/sanitize_test.go`
**依赖：** T1.2
**步骤：**
1. 依次执行运行时脱敏、控制字符清理和 UTF-8 边界截断。
2. 保留稳定 code/source/hint，丢弃 raw error payload。

**验证：** `go test -json -count=1 ./internal/diagnostics -run TestSanitizeRedactsControlsAndTruncatesUTF8` 通过。
**覆盖：** F2、F7、F14 / AC2、AC7、AC14。

### T1.8 实现有界聚合诊断 Sink

**文件：** `internal/diagnostics/{diagnostic,collector,sink,snapshot}.go`, `internal/diagnostics/collector_test.go`
**依赖：** T1.7、T1.5
**步骤：**
1. 按稳定安全身份聚合重复项并累计次数。
2. 达到项目数或总字节预算后只增加 dropped，Snapshot 返回不可变副本。

**验证：** `go test -json -race -count=1 ./internal/diagnostics -run TestBoundedSinkAggregatesAndDrops` 通过。
**覆盖：** F2、F5、F7、F14–F21 / AC2、AC5、AC7、AC14–AC21。

### T1.9 实现安全 HTTP 错误摘要

**文件：** `internal/diagnostics/http_summary.go`, `internal/diagnostics/http_summary_test.go`
**依赖：** T1.7、T1.5
**步骤：**
1. 只保留状态、媒体类型、有界安全预览和截断元数据。
2. 禁止认证头、原始 URL、请求体和完整响应体进入摘要。

**验证：** `go test -json -count=1 ./internal/diagnostics -run TestHTTPErrorSummaryNeverLeaksCanary` 通过。
**覆盖：** F7、F8、F14、F17 / AC7、AC8、AC14、AC17。

### T1.10 固定跨边界 canary 测试

**文件：** `internal/diagnostics/secret_canary_test.go`, `internal/redact/runtime_test.go`
**依赖：** T1.3、T1.8、T1.9
**步骤：**
1. 将同一 canary 放入 header、URL、env、错误链和结构化载荷。
2. 断言 SafeText 与 diagnostics snapshot 中均无原文。

**验证：** `go test -json -count=1 ./internal/redact ./internal/diagnostics -run Canary` 通过且测试失败消息也不打印 canary。
**覆盖：** F7 / AC7。

### T1.11 定义 `safefs` Bootstrap、Binding 与 capability

**文件：** `internal/safefs/api.go`, `internal/safefs/policy.go`, `internal/safefs/capability.go`, `internal/safefs/root_test.go`
**依赖：** T1.5
**步骤：**
1. 定义允许根、稳定 `Identity`、不可伪造 `Binding`、受保护槽位和 `Bootstrap` 返回值。
2. 让 Bootstrap 只向组装根返回 Capabilities 容器；Root 不能签发或升级能力。
3. 分离 Ordinary/Protected 写能力，并为已存在对象及“父目录身份＋未存在目标名”生成绑定。

**验证：** `go test -json -count=1 ./internal/safefs -run 'TestBootstrapSeparatesCapabilities|TestZeroAndCrossRootCapabilitiesFail|TestBindingTracksExistingAndMissingTargets|TestCapabilityCannotWriteProtectedSlot'` 通过。
**覆盖：** F4、F12、F13、F31 / AC4、AC12、AC13、AC31。

### T1.12 实现 POSIX 句柄相对打开

**文件：** `internal/safefs/root_posix.go`, `internal/safefs/path.go`, `internal/safefs/root_posix_test.go`
**依赖：** T1.11
**步骤：**
1. 使用显式 `darwin || linux` build tag 和 no-follow 的目录句柄链。
2. 让验证与最终 I/O 使用同一对象身份，拒绝父级跳转和链接替换。
3. `Root.Bind` 从已打开句柄生成现存对象身份，未创建目标绑定固定父句柄与名称。

**验证：** `go test -json -count=1 ./internal/safefs -run 'TestRootRejectsSymlinkSwap|TestBindUsesOpenedIdentity|TestBindMissingUsesParentHandle'` 在 POSIX 平台通过。
**覆盖：** F4、F12、F13、F31 / AC4、AC12、AC13、AC31。

### T1.13 实现 Windows 句柄相对打开

**文件：** `internal/safefs/root_windows.go`, `internal/safefs/root_windows_test.go`
**依赖：** T1.11
**步骤：**
1. 使用 Windows handle、reparse-point 检查和最终句柄路径验证。
2. 对盘符、UNC、大小写别名和路径逃逸保持等价拒绝语义。
3. Binding 使用 volume/file ID 或固定父 handle＋规范目标名，不受大小写别名替换影响。

**验证：** `GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/safefs-windows.test.exe" ./internal/safefs` 成功；Windows runner 上 `go test -json -count=1 ./internal/safefs -run 'TestWindowsRootRejectsReparseEscape|TestWindowsBindingIgnoresCaseAlias'` 的两个 required test 均 pass 且无 skip。
**覆盖：** F4、F12、F13、F31 / AC4、AC12、AC13、AC31。

### T1.14 实现有界可取消遍历

**文件：** `internal/safefs/walk.go`, `internal/safefs/root_test.go`
**依赖：** T1.12、T1.13
**步骤：**
1. 在打开目录、读取 entry 和调用 visitor 前检查 context 并消费累计预算。
2. 将局部扫描错误显式返回，不静默跳过。

**验证：** `go test -json -count=1 ./internal/safefs -run 'TestWalkCancellation|TestWalkBudgetAndErrors'` 通过。
**覆盖：** F12、F13 / AC12、AC13。

### T1.15 实现 POSIX 崩溃安全原子写

**文件：** `internal/safefs/atomic_posix.go`, `internal/safefs/root_posix_test.go`
**依赖：** T1.12
**步骤：**
1. 在同目录创建私有 staging，flush 后原子 replace，并按需同步父目录。
2. 在 replace 前再次验证 capability 和目标槽位身份。

**验证：** `go test -json -count=1 ./internal/safefs -run TestAtomicWritePreservesPreviousVersionOnFailure` 通过。
**覆盖：** F4、F5、F31 / AC4、AC5、AC31。

### T1.16 实现 Windows 崩溃安全原子写

**文件：** `internal/safefs/atomic_windows.go`, `internal/safefs/root_windows_test.go`
**依赖：** T1.13
**步骤：**
1. 使用同卷 staging 和安全 replace，复验目标 handle 与受保护槽位。
2. 失败时保留上一版本并关闭全部句柄。

**验证：** `GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/safefs-atomic-windows.test.exe" ./internal/safefs` 成功；Windows runner 上 `go test -json -count=1 ./internal/safefs -run TestWindowsAtomicWritePreservesPreviousVersionOnFailure` 的 required test pass 且无 skip。
**覆盖：** F4、F5、F31 / AC4、AC5、AC31。

### T1.17 增加 unsupported 拒绝式实现

**文件：** `internal/safefs/root_unsupported.go`
**依赖：** T1.12、T1.13
**步骤：**
1. 对支持范围外平台返回稳定的 unsupported 错误。
2. 不提供 no-op、字符串前缀或 `!unix` 弱化 fallback。

**验证：** `go test -json -count=1 ./internal/safefs -run TestUnsupportedBuildTagExcludesSupportedPlatformsAndRejects` 与 `GOOS=freebsd GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/safefs-freebsd.test" ./internal/safefs` 通过，前者 required test 明确 pass。
**覆盖：** F31 / AC31。

### T1.18 定义 `proctree` 进程、管道与清理契约

**文件：** `internal/proctree/api.go`, `internal/proctree/lifecycle.go`, `internal/proctree/protected_exec.go`, `internal/proctree/lifecycle_test.go`
**依赖：** T1.8、T1.11
**步骤：**
1. 定义 `ProtectionPlan`、`ProtectionRequired`、`Pipes`、`StartError{TargetStarted}` 和唯一 OS Wait 所有者的 `Process`。
2. `NewProtectionPlan` 只接受已打开 Root 与每次运行的私有 scratch；零值 plan fail closed。
3. Options 注入 `CleanupTimeout` 与 `Diagnostics`；固定“停写—软终止—有界等待—强杀—reap—关 pipe”状态机，调用者 context 只限制等待。
4. 内部清理不继承调用者取消且受 2 秒硬上限约束；硬超限执行最终强制关闭并只诊断一次，后续 Close 返回同一最终结果。

**验证：** `go test -json -race -count=1 ./internal/proctree -run 'TestProtectionPlanRejectsZeroValue|TestStartErrorNeverClaimsTargetStarted|TestProcessCloseIsIdempotentAndContinuesAfterWaiterTimeout|TestProcessCleanupTimeoutForcesCloseAndDiagnosesOnce'` 通过。
**覆盖：** F11、F16、F31 / AC11、AC16、AC31。

### T1.18a 创建每次运行的私有 scratch 与 ProtectionPlan

**文件：** `internal/proctree/protected_exec.go`, `internal/proctree/protected_exec_test.go`, `internal/safefs/policy.go`
**依赖：** T1.15、T1.16、T1.18
**步骤：**
1. 为每次进程启动创建用户专用 scratch Root，并在终态清理。
2. `NewProtectionPlan` 固定项目 Root、全部权限配置 Root/槽位和 scratch；计划不可序列化或事后放宽。
3. 只给 scratch 写能力，项目根及权限配置根对不受信进程只读。

**验证：** `go test -json -count=1 ./internal/proctree -run 'TestProtectionPlanOnlyAllowsScratchWrites|TestScratchIsPrivateAndPerRun'` 通过。
**覆盖：** F4、F11、F31 / AC4、AC11、AC31。

### T1.18b 固定 Process pipe 借用与关闭顺序

**文件：** `internal/proctree/lifecycle.go`, `internal/proctree/lifecycle_test.go`
**依赖：** T1.18
**步骤：**
1. `Pipes()` 始终返回同一组由 Process 拥有、调用方只借用的 parent-side handles。
2. Close 顺序固定为阻止新写、终止树、reap、关闭 pipes、等待 I/O；借用方不能靠关闭共享 channel 解除竞争。

**验证：** `go test -json -race -count=20 ./internal/proctree -run 'TestPipeOwnership|TestBlockedWriteCloseOrder'` 通过。
**覆盖：** F11、F16 / AC11、AC16。

### T1.19 实现 POSIX 进程组 Runner

**文件：** `internal/proctree/runner_posix.go`, `internal/proctree/runner_posix_test.go`
**依赖：** T1.18a、T1.18b、T1.12
**步骤：**
1. 以显式 POSIX build tag 建立进程组和三条 parent-side pipe。
2. 内部 supervisor 成为唯一 OS Wait/reap 所有者，外层 `Process.Wait` 只等待共享结果。
3. Close 对整个进程组执行终止、强杀和 reap，再关闭 pipe。

**验证：** `go test -json -count=1 ./internal/proctree -run TestPOSIXRunnerKillsDescendants` 通过。
**覆盖：** F11、F16、F31 / AC11、AC16、AC31。

### T1.20 实现 macOS 受保护执行策略

**文件：** `internal/proctree/protected_exec.go`, `internal/proctree/protected_darwin.go`, `internal/proctree/runner_posix_test.go`
**依赖：** T1.19、T1.18a、T1.15
**步骤：**
1. 校验系统 Seatbelt launcher 身份，并从 ProtectionPlan 生成只允许私有 scratch 写入的 profile。
2. 项目根和权限配置根保持只读；策略或 launcher 验证失败时在目标 exec 前返回 `protected_exec_unavailable`。

**验证：** macOS runner 执行 `go test -json -count=20 ./internal/proctree -run TestDarwinProtectedExec`，直接路径、变量、拼接、解释器和子孙进程样本均失败，scratch 可写，探测失败时目标 marker 不出现。
**覆盖：** F4、F11、F31 / AC4、AC11、AC31。

### T1.21 实现 Linux self-reexec 启动握手

**文件：** `internal/proctree/launcher_linux.go`, `cmd/xagent/internal_mode.go`, `cmd/xagent/main.go`, `internal/proctree/runner_posix_test.go`
**依赖：** T1.19、T1.18a、T1.15
**步骤：**
1. 增加普通 help 不可见的内部 launcher mode，只接受父进程继承句柄中的版本化 ProtectionPlan。
2. 使用 close-on-exec 错误管道区分“保护/exec 失败”和“目标已成功 exec”；普通 CLI 参数不能构造或放宽 plan。

**验证：** Linux 上 `go test -json -count=1 ./internal/proctree ./cmd/xagent -run 'TestLauncherAcceptsOnlyInheritedPlan|TestExecHandshakeReportsTargetNotStarted'` 通过。
**覆盖：** F4、F11、F31 / AC4、AC11、AC31。

### T1.21a 安装 Linux Landlock 保护策略

**文件：** `internal/proctree/protected_linux.go`, `internal/proctree/launcher_linux.go`, `internal/proctree/runner_posix_test.go`
**依赖：** T1.21
**步骤：**
1. 在 launcher 内探测 Landlock ABI/所需 rights，设置 `no_new_privs` 后只向私有 scratch 授予写能力。
2. 项目根和权限配置根保持只读；能力不足或规则安装失败时不 exec 目标。

**验证：** Linux runner 执行 `go test -json -count=20 ./internal/proctree -run TestLinuxProtectedExec`，绕过矩阵均失败、scratch 可写；无能力 fixture 返回 `protected_exec_unavailable`、`TargetStarted=false` 且无目标 marker。
**覆盖：** F4、F11、F31 / AC4、AC11、AC31。

### T1.22 实现 Windows Job Object Runner

**文件：** `internal/proctree/runner_windows.go`, `internal/proctree/runner_windows_test.go`
**依赖：** T1.18a、T1.18b、T1.13
**步骤：**
1. 建立 Job Object、suspended process primitive 和三条 parent-side pipe；本任务不得 resume 未安装保护的目标。
2. 内部 supervisor 保持唯一 OS Wait/reap 所有者；失败、取消和重复关闭终止 suspended process/job 并释放句柄。

**验证：** `GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/proctree-windows.test.exe" ./internal/proctree` 成功；Windows runner 上 `go test -json -count=1 ./internal/proctree -run TestWindowsRunnerKillsDescendants` 的 required test pass 且无 skip。
**覆盖：** F11、F16、F31 / AC11、AC16、AC31。

### T1.23 实现 Windows 受保护执行策略

**文件：** `internal/proctree/protected_windows.go`, `internal/proctree/runner_windows_test.go`
**依赖：** T1.22、T1.18a、T1.16
**步骤：**
1. 在 suspended child resume 前依次建立低完整性/受限 token、对 Root/槽位做 AccessCheck、加入 Job，只给本次私有 scratch 写权限。
2. 全部验证通过后才 resume 并完成 exec 握手；任一步失败都终止未恢复进程、关闭句柄并返回 `TargetStarted=false`。

**验证：** Windows runner 执行 `go test -json -count=20 ./internal/proctree -run TestWindowsProtectedExec`，绕过样本均失败、scratch 可写；能力失败时目标 marker 不出现。
**覆盖：** F4、F11、F31 / AC4、AC11、AC31。

### T1.24 增加 proctree unsupported 拒绝实现

**文件：** `internal/proctree/runner_unsupported.go`, `internal/proctree/runner_unsupported_test.go`, `internal/repoaudit/proctree_refs_test.go`
**依赖：** T1.20、T1.21a、T1.23
**步骤：**
1. 支持范围外平台、`Mode=0`、零值 ProtectionPlan 和能力不足统一返回不可执行错误。
2. 确认没有直接回退到裸 `exec.CommandContext`，每个拒绝场景的目标 marker 均不出现。

**验证：** `go test -json -count=1 ./internal/proctree ./internal/repoaudit -run 'TestUnsupportedRunnerFailsBeforeTargetStart|TestUnsupportedRunnerHasNoBareExecFallback'` 通过；AST/import 审计 required test 明确证明 unsupported 文件无 `os/exec` 导入或裸执行调用，两个测试均 pass 且无 skip。
**覆盖：** F4、F31 / AC4、AC31。

### T1.25 定义私有 Artifact Store API

**文件：** `internal/artifact/api.go`, `internal/artifact/file_store.go`, `internal/artifact/file_store_test.go`
**依赖：** T1.5、T1.11
**步骤：**
1. 定义不含路径的 `Ref`、`Writer`、`Store` 和 metadata。
2. 将 artifact 根固定在工作区外的用户私有目录，并拒绝工作区内配置。

**验证：** `go test -json -count=1 ./internal/artifact -run TestStoreRejectsWorkspaceRoot` 通过。
**覆盖：** F9、F10 / AC9、AC10。

### T1.26 实现 Artifact staging 与一次性终结

**文件：** `internal/artifact/writer.go`, `internal/artifact/file_store_test.go`
**依赖：** T1.25
**步骤：**
1. 从第一字节写私有 staging，并在写前消费单文件与总容量预算。
2. 强制 Writer 恰好 Commit 或 Abort 一次；超限 Ref 标记 `Complete=false`。

**验证：** `go test -json -race -count=1 ./internal/artifact -run 'TestWriterCommitAbortExactlyOnce|TestWriterHardLimitMarksIncomplete'` 通过。
**覆盖：** F9、F10 / AC9、AC10。

### T1.27 实现 Artifact 平台私有权限

**文件：** `internal/artifact/private_posix.go`, `internal/artifact/private_windows.go`, `internal/artifact/private_unsupported.go`, `internal/artifact/file_store_test.go`
**依赖：** T1.26
**步骤：**
1. POSIX 使用用户专用目录/文件权限，Windows 使用当前用户访问控制。
2. unsupported 平台拒绝创建，不返回真实路径给普通调用方。

**验证：** `go test -json -count=1 ./internal/artifact -run 'TestArtifactPOSIXPermissions|TestArtifactRefSerializationContainsNoPath'` 的两个 required test pass；`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/artifact-windows.test.exe" ./internal/artifact` 成功，Windows amd64 runner 上 `go test -json -count=1 ./internal/artifact -run TestArtifactWindowsACLIsCurrentUserOnly` required test pass 且无 skip。
**覆盖：** F10、F31 / AC10、AC31。

### T1.28 实现 Artifact 保留期与容量清理

**文件：** `internal/artifact/cleanup.go`, `internal/artifact/file_store_test.go`
**依赖：** T1.27
**步骤：**
1. 按保留期和总容量确定性选择过期对象，跳过活动 writer。
2. 清理错误返回有界结果，Close 幂等且不删除未确认对象。

**验证：** `go test -json -race -count=1 ./internal/artifact -run TestCleanupHonorsRetentionCapacityAndActiveWriters` 通过。
**覆盖：** F10 / AC10。

### T1.29 定义网络端点与初始策略

**文件：** `internal/netpolicy/policy.go`, `internal/netpolicy/endpoint.go`, `internal/netpolicy/error.go`, `internal/netpolicy/policy_test.go`
**依赖：** T1.2
**步骤：**
1. 定义 Purpose、Origin、AddressClass 和安全错误。
2. 拒绝 URL userinfo、未知协议和非回环明文 HTTP。

**验证：** `go test -json -count=1 ./internal/netpolicy -run TestInitialEndpointPolicy` 通过且错误不含敏感 URL 字段。
**覆盖：** F7、F8 / AC7、AC8。

### T1.30 实现 DNS 地址分类与绑定

**文件：** `internal/netpolicy/resolver.go`, `internal/netpolicy/policy_test.go`
**依赖：** T1.29
**步骤：**
1. 分类 loopback、private、link-local 和 public 地址并拒绝混合越界结果。
2. 将解析结果绑定到经验证 Endpoint，测试注入可控 resolver。

**验证：** `go test -json -count=1 ./internal/netpolicy -run TestResolverPreventsAddressClassChange` 通过。
**覆盖：** F8 / AC8。

### T1.31 构造受控 HTTP Client

**文件：** `internal/netpolicy/client.go`, `internal/netpolicy/policy_test.go`
**依赖：** T1.30
**步骤：**
1. 实现只接受 Endpoint 与 `ClientOptions` 的 Factory，以及只暴露 `Do`、`SDKHTTPClient`、`CloseIdleConnections` 的受控 wrapper；公开或跨包接口不得接收 `http.Client`、RoundTripper、Proxy、Dial/DialTLS、CheckRedirect、CookieJar 或协议 handler。
2. Factory 自行创建每个 owner 的私有标准 client/transport，覆盖代理、拨号、TLS 拨号和 redirect 行为；`TrustedRoots` 只用于标准 TLS 校验且在构造时深拷贝，nil 使用系统信任根。
3. SDK bridge 返回带同一受控 RoundTripper 的 client，并由创建者负责关闭 idle connections；恶意 transport 无注入入口，调用方后续修改证书池不能影响现有 Client。

**验证：** `go test -json -race -count=1 ./internal/netpolicy -run 'TestClientFactoryHasNoExternalTransportInjection|TestTrustedRootsAreCloned|TestClientFactoryScopesCredentials|TestSDKBridgeRetainsGuardedTransport|TestClientClosesIdleConnections'` 通过。
**覆盖：** F7、F8 / AC7、AC8。

### T1.32 实现逐跳重定向复验

**文件：** `internal/netpolicy/redirect.go`, `internal/netpolicy/redirect_test.go`
**依赖：** T1.31
**步骤：**
1. 每跳先移除敏感头，再复验协议、origin、DNS 和地址类别。
2. 默认敏感头集合不可被配置移除；拒绝 HTTPS 降级、跨源和 public 转 private/loopback。
3. 即使 SDK 自行构造下一跳，受控 RoundTripper 仍在发送前执行同样复验。

**验证：** `go test -json -count=1 ./internal/netpolicy -run TestRedirectNeverForwardsCredentialsAcrossBoundary` 通过。
**覆盖：** F8 / AC8。

### T1.33 实现拨号阶段复核

**文件：** `internal/netpolicy/dial.go`, `internal/netpolicy/dial_test.go`
**依赖：** T1.30、T1.31
**步骤：**
1. 仅拨打 Endpoint 已绑定且类别一致的地址。
2. DNS rebinding 或实际地址越界时在发送敏感字节前失败。

**验证：** `go test -json -count=1 ./internal/netpolicy -run TestDialRejectsRebindingBeforeRequest` 通过。
**覆盖：** F8 / AC8。

### T1.34 验证安全基础包边界

**文件：** `internal/{redact,budget,diagnostics,safefs,proctree,artifact,netpolicy}/*_test.go`
**依赖：** T1.3、T1.6、T1.10、T1.14、T1.17、T1.24、T1.28、T1.32、T1.33
**步骤：**
1. 增加 import-boundary 和幂等关闭回归测试。
2. 在支持平台编译全部基础包并运行 race；三平台原生运行 protected-exec 绕过测试，能力不足必须断言目标未启动而不能 skip。

**验证：** `go test -json -race -count=1 ./internal/redact ./internal/budget ./internal/diagnostics ./internal/safefs ./internal/proctree ./internal/artifact ./internal/netpolicy` 通过；macOS/Linux/Windows 原生 amd64 runner 分别执行 `go test -json -count=20 ./internal/proctree -run 'Test(Darwin|Linux|Windows)ProtectedExec|TestProtectionUnavailableFailsBeforeTargetStart'`，对应 required test 次数满足且无 skip；`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/security-base-windows.test.exe" ./internal/proctree` 成功。
**覆盖：** F4、F7–F14、F31 / AC4、AC7–AC14、AC31。

## M2：运行时安全边界

### T2.1 定义 presence-aware 配置 DTO

**文件：** `internal/config/optional.go`, `internal/config/partial.go`, `internal/config/config.go`
**依赖：** T1.34
**步骤：**
1. 为本文件数值快照中的 tool/artifact/files/instructions/mcp/llm.stream/session/diagnostics/lifecycle/context/memory/agent 及既有 LLM 公开键逐项定义 `Optional[T]` 与 `PartialAppConfig`；MCP server timeout 必须保留独立 presence，不能把未设置折叠成 0。
2. 保持最终 `AppConfig` 为确定值，不让业务模块读取 presence 状态。

**验证：** `go test -json -count=1 ./internal/config -run TestOptionalDistinguishesAbsentFalseAndZero` 通过。
**覆盖：** F22、F28 / AC22、AC28。

### T2.2 实现严格分层解码

**文件：** `internal/config/decode.go`, `internal/config/load.go`, `internal/config/decode_test.go`
**依赖：** T2.1
**步骤：**
1. 严格拒绝未知字段、显式 null、类型错误和重复冲突字段。
2. 错误只报告字段路径和来源，不回显配置标量。

**验证：** `go test -json -count=1 ./internal/config -run 'TestDecodeRejectsUnknownNullAndDuplicate|TestDecodeErrorDoesNotLeakValue'` 通过。
**覆盖：** F7、F28 / AC7、AC28。

### T2.3 实现四层确定性合并

**文件：** `internal/config/merge.go`, `internal/config/merge_test.go`
**依赖：** T2.2
**步骤：**
1. 固定 `默认 < 用户 < 项目 < 运行期/CLI` 顺序并记录字段 provenance。
2. 仅 `Set=false` 时继承，显式 false、0 和负数必须保留到验证阶段。

**验证：** `go test -json -count=1 ./internal/config -run TestMergePreservesExplicitFalseZeroAndNegative` 通过。
**覆盖：** F28 / AC28。

### T2.4 固定 AppConfig 数值清单与边界测试骨架

**文件：** `internal/config/optional.go`, `internal/config/partial.go`, `internal/config/resolve.go`, `internal/config/validate.go`, `internal/config/resolve_test.go`
**依赖：** T2.3、T1.4
**步骤：**
1. 建立与本文件数值快照逐键相等的 AppConfig exact-key manifest，以及复用的 absent/default、min、cap、cap+1、explicit-zero、negative、overflow 表驱动 helper。
2. 断言 Hook rule、permission RuleFile 与 Skill frontmatter `history` 不在 `PartialAppConfig`、Resolve switch 或 AppConfig manifest 中；这些输入只能由各自 owner 验证。

**验证：** `go test -json -count=1 ./internal/config -run '^(TestAppConfigNumericManifestIsExact|TestNonAppConfigNumericInputsAreAbsentFromPartialConfig)$'` 通过，两个 required test 均 pass 且无 skip。
**覆盖：** F9、F14、F22、F28 / AC9、AC14、AC22、AC28。

### T2.4a Resolve tool、artifact 与 files 数值

**文件：** `internal/config/resolve.go`, `internal/config/validate.go`, `internal/config/resolve_test.go`
**依赖：** T2.4
**步骤：**
1. 按快照实现 tool inline/capture/timeout、artifact file/total/retention 及 files read/scan 四维键的 default/min/cap，presence 只在未设置时应用默认。
2. 旧 `tool.max_output_bytes` 只映射到 `tool.inline_output_bytes`；新旧同时出现报冲突，原配置文件不改写。

**验证：** `go test -json -count=1 ./internal/config -run '^(TestResolveToolArtifactAndFilesNumericMatrix|TestLegacyToolOutputMapsOnlyToInlineAndConflictsWithNewKey)$'` 通过，两个 required test 均 pass 且无 skip。
**覆盖：** F9、F12、F28 / AC9、AC12、AC28、AC35。

### T2.4b Resolve Instruction 与 MCP 数值

**文件：** `internal/config/resolve.go`, `internal/config/validate.go`, `internal/config/resolve_test.go`
**依赖：** T2.4a
**步骤：**
1. 按快照实现 Instruction 五个键与 MCP response/tools/pages/protocol-errors/default/server timeout 的 default/min/cap。
2. MCP server timeout 未设置时继承已经 Resolve 的 `mcp.default_timeout_ms`；显式 0 必须失败，不能继承字面量 30000 或低优先级层值。

**验证：** `go test -json -count=1 ./internal/config -run '^(TestResolveInstructionAndMCPNumericMatrix|TestMCPServerTimeoutInheritsResolvedDefault)$'` 通过，两个 required test 均 pass 且无 skip。
**覆盖：** F13、F14、F28 / AC13、AC14、AC28。

### T2.4c Resolve LLM、Agent 与 stream 数值

**文件：** `internal/config/resolve.go`, `internal/config/validate.go`, `internal/config/resolve_test.go`
**依赖：** T2.4b
**步骤：**
1. 按快照实现 LLM request/thinking、Agent 两个累计限额与 stream response/event/events/text/thinking/tool-arguments 的 default/min/cap。
2. thinking 关闭时仍验证 budget 但不消费；所有 stream 子字节上限分别不得高于 response 上限。

**验证：** `go test -json -count=1 ./internal/config -run '^(TestResolveLLMAgentAndStreamNumericMatrix|TestStreamSubLimitsCannotExceedResponseLimit)$'` 通过，两个 required test 均 pass 且无 skip。
**覆盖：** F9、F17、F18、F28 / AC9、AC17、AC18、AC28。

### T2.4d Resolve Context 与 Session 数值

**文件：** `internal/config/resolve.go`, `internal/config/validate.go`, `internal/config/resolve_test.go`
**依赖：** T2.4c
**步骤：**
1. 按快照实现 Context 八个键与 Session record/session/scan/retention/gap 键的 default/min/cap；`context.model_window_tokens` 独立最小值固定为 2。
2. min fixture 同时令 auto/manual/recent token 值为 1；model window 必须分别严格大于三者，record 不得高于 session。

**验证：** `go test -json -count=1 ./internal/config -run '^(TestResolveContextAndSessionNumericMatrix|TestContextWindowMinimumAndThreeStrictRelations|TestSessionRecordCannotExceedSession)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F19、F22、F28 / AC19、AC22、AC28。

### T2.4e Resolve Memory、Diagnostics 与 Lifecycle 数值

**文件：** `internal/config/resolve.go`, `internal/config/validate.go`, `internal/config/resolve_test.go`
**依赖：** T2.4d
**步骤：**
1. 按快照实现 Memory 六个键、Diagnostics 三个键及 `lifecycle.cleanup_timeout_ms` 的 default/min/cap。
2. Memory concurrency 不得高于 queue，diagnostic item 不得高于 total；cleanup 只接受 1–2000 毫秒，不能关闭或提高。

**验证：** `go test -json -count=1 ./internal/config -run '^(TestResolveMemoryDiagnosticsAndLifecycleNumericMatrix|TestMemoryQueueAndDiagnosticTotalRelations|TestCleanupTimeoutCannotBeDisabledOrRaised)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F15、F17、F28 / AC7、AC15、AC17、AC28。

### T2.4f 封闭 AppConfig 组合关系与完整矩阵

**文件：** `internal/config/resolve.go`, `internal/config/validate.go`, `internal/config/resolve_test.go`
**依赖：** T2.4e
**步骤：**
1. 逐项固定 inline≤capture≤artifact file≤artifact total、instruction file≤total/expanded、stream 四个子限额≤response、record≤session、diagnostic item≤total、tool-result single≤multi、model window 三个严格关系及 memory concurrency≤queue。
2. 用 T2.4 manifest 证明每个 AppConfig 数值键恰好有一组 default/min/cap/cap+1/zero/negative/overflow 证据；错误只含字段路径与允许范围，leaf module 不得补默认或静默截断。

**验证：** `go test -json -count=1 ./internal/config -run '^(TestResolveRejectsEveryApprovedNumericCombination|TestEveryAppConfigNumericKeyHasCompleteBoundaryMatrix|TestResolveErrorsContainOnlyPathAndAllowedRange)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F9、F14、F22、F28 / AC9、AC14、AC22、AC28。

### T2.5 注册最终生效的运行期秘密

**文件：** `internal/config/resolve.go`, `internal/config/load.go`, `internal/config/resolve_test.go`
**依赖：** T2.4f、T1.2
**步骤：**
1. 只展开最终胜出的 LLM、MCP、Hook 等环境引用，并立即注册物化值。
2. 缺失变量或注册失败时在联网/启动进程前终止，错误不含变量值。

**验证：** `go test -json -count=1 ./internal/config -run TestResolveRegistersOnlyEffectiveExpandedSecrets` 通过。
**覆盖：** F7 / AC7。

### T2.6 固定 MCP 配置整项覆盖语义

**文件：** `internal/config/mcp.go`, `internal/config/mcp_test.go`, `internal/config/merge.go`
**依赖：** T2.5
**步骤：**
1. 不同名 server 合并，同名 server 由高优先级层整项替换。
2. 覆盖 disabled、env/header、timeout 和预算字段的严格验证。

**验证：** `go test -json -count=1 ./internal/config -run TestMCPServersMergeByNameWithWholeEntryReplacement` 通过。
**覆盖：** F7、F14、F28 / AC7、AC14、AC28。

### T2.7 保留旧配置兼容入口

**文件：** `internal/config/load.go`, `internal/config/resolve.go`, `internal/config/{decode,merge,resolve}_test.go`
**依赖：** T2.6
**步骤：**
1. 将旧 `tool.max_output_bytes` 映射为内联预览上限。
2. 新旧字段同时出现时明确报冲突，旧有效样本仍能加载。

**验证：** `go test -json -count=1 ./internal/config -run TestLegacyConfigCompatibilityAndConflict` 通过。
**覆盖：** F28 / AC28、AC35。

### T2.8 实现通用版本化调用身份

**文件：** `internal/permission/identity.go`, `internal/permission/bash_identity.go`, `internal/permission/identity_test.go`
**依赖：** T1.11、T2.5、T2.6
**步骤：**
1. 实现 `NewCallIdentity`，绑定工具名、规范参数、工作目录、safefs Bindings、环境摘要和 MCP target digest。
2. 使用版本化长度前缀编码，拒绝不稳定 map 顺序、重复 binding 和空工具名。

**验证：** `go test -json -count=1 ./internal/permission -run 'TestCallIdentityCanonicalizesArguments|TestCallIdentityBindsFileAndMCPResources'` 通过。
**覆盖：** F3、F5、F6 / AC3、AC5、AC6。

### T2.8a 实现 Bash 原始语义身份

**文件：** `internal/permission/bash_identity.go`, `internal/permission/identity_test.go`
**依赖：** T2.8
**步骤：**
1. 使用专用构造器绑定 Shell、工作目录稳定身份、环境摘要和未经 trim 的原始命令字节。
2. 覆盖空格、换行、引号、控制符及相同展示文本的碰撞样本。

**验证：** `go test -json -count=1 ./internal/permission -run TestBashIdentityDistinguishesRawSemanticBytes` 通过。
**覆盖：** F3、F6 / AC3、AC6。

### T2.9 实现一次性 Execution Ticket

**文件：** `internal/permission/ticket.go`, `internal/permission/ticket_test.go`
**依赖：** T2.8a
**步骤：**
1. 通过 `TicketIssuer.Issue(callID, identity)` 签发绑定 callID、identity、nonce、有效期和安全 epoch 的不可序列化票据。
2. 在同一临界区 VerifyAndConsume；拒绝零值、过期、重放、错 callID 和错 identity。

**验证：** `go test -json -race -count=1 ./internal/permission -run 'TestTicketCanBeConsumedExactlyOnce|TestFreshIssueUsesNewNonce|TestTicketIsNotSerializable|TestCallIDIsBoundOutsideIdentity'` 通过。
**覆盖：** F3、F5、F6、F24 / AC3、AC5、AC6、AC24。

### T2.10 建立权限健康状态与保守降级

**文件：** `internal/permission/health.go`, `internal/permission/health_test.go`, `internal/permission/authorizer.go`
**依赖：** T2.9、T1.8
**步骤：**
1. 任一权限层损坏时提高安全 epoch，并使未消费危险票据失效。
2. 仅允许内建低风险只读 allowlist 在受限根和预算下继续；危险 MCP 一律拒绝。

**验证：** `go test -json -count=1 ./internal/permission -run TestCorruptLayerFailsClosedForAllDangerousTools` 通过。
**覆盖：** F5 / AC5。

### T2.11 迁移权限规则加载器

**文件：** `internal/permission/loader.go`, `internal/permission/rule.go`, `internal/permission/health_test.go`
**依赖：** T2.10、T1.11
**步骤：**
1. 通过 `safefs` 加载用户、项目和本地层，区分缺失与损坏。
2. 无法证明等价的 Bash 自动允许规则标记 `legacy_untrusted`，保留原文件；加载或迁移均不得自动改写原文件。

**验证：** `go test -json -count=1 ./internal/permission -run '^TestLegacyRulesMigrateNonDestructively$'` 通过，required test pass 且无 skip。
**覆盖：** F5、F6 / AC5、AC6、AC35。

### T2.11a 固定 Permission RuleFile version 兼容矩阵

**文件：** `internal/permission/loader.go`, `internal/permission/rule.go`, `internal/permission/loader_version_test.go`, `internal/permission/health_test.go`
**依赖：** T2.11
**步骤：**
1. `version` 缺失或显式 0 只按既有 legacy v1 非破坏读取，整数 1 为当前格式；两条兼容路径都不得改写原文件。
2. 负数、大于 1、YAML 整数溢出或错误类型把该层标为损坏并触发危险操作 fail closed，不能静默解释成 v1；错误不回显原始标量。

**验证：** `go test -json -count=1 ./internal/permission -run '^(TestRuleFileVersionMissingAndZeroAreLegacyV1|TestRuleFileVersionOneIsCurrent|TestInvalidRuleFileVersionCorruptsLayerAndFailsClosed|TestRuleVersionFailureIsNonDestructiveAndSafe)$'` 通过，四个 required test 均 pass 且无 skip。
**覆盖：** F5、F6 / AC5、AC6、AC35。

### T2.12 收敛最小规则匹配

**文件：** `internal/permission/matcher.go`, `internal/permission/rule.go`, `internal/permission/permission_test.go`
**依赖：** T2.8、T2.11a
**步骤：**
1. 规则只决定是否再次询问，不直接成为执行能力。
2. 每次规则命中仍基于当次完整 identity 签发新 Ticket；MCP 规则匹配与最终 server config digest 的 Ticket 身份分开。
3. 永久范围必须结构化、最小且不含 secret。

**验证：** `go test -json -count=1 ./internal/permission -run 'TestRulesNeverSubstituteForTicket|TestMCPRuleDoesNotCrossServer'` 通过。
**覆盖：** F3、F6 / AC3、AC6。

### T2.13 使用专用 capability 原子写权限文件

**文件：** `internal/permission/writer.go`, `internal/permission/writer_test.go`
**依赖：** T2.11a、T1.15、T1.16
**步骤：**
1. 只接受组装根从该 Root 对应的 Bootstrap `OpenResult` 中保存之 `Capabilities.Protected()` 提取并注入的 capability。
2. 使用持有该 capability 的当前 `root.AtomicWrite`，失败保留旧版本且不更新内存规则。

**验证：** `go test -json -count=1 ./internal/permission -run 'TestPermissionWriterRequiresDedicatedCapabilityAndIsCrashSafe|TestPermissionWriterRejectsCrossRootCapability'` 通过。
**覆盖：** F4–F6 / AC4–AC6。

### T2.14 生成安全且可撤销的确认模型

**文件：** `internal/permission/decision.go`, `internal/permission/result.go`, `internal/permission/authorizer.go`, `internal/permission/permission_test.go`
**依赖：** T2.12、T2.13、T1.2
**步骤：**
1. 输出实际目标、安全风险、once/session/permanent 范围、规则落点和撤销提示。
2. 命令或参数含注册 secret 时禁用 permanent，并仅显示脱敏目标。

**验证：** `go test -json -count=1 ./internal/permission -run TestConfirmationExplainsScopeWithoutLeakingSecret` 通过。
**覆盖：** F6 / AC6。

### T2.15 原子切换 Authorizer 到 Ticket 模型

**文件：** `internal/permission/{authorizer,session,blacklist,result}.go`, `internal/permission/permission_test.go`, `internal/repoaudit/permission_refs_test.go`
**依赖：** T2.9、T2.10、T2.12、T2.14
**步骤：**
1. Permission 内固定执行硬约束、健康状态、普通规则/确认和 `TicketIssuer.Issue`；Hook 顺序由后续 Orchestrator 原子切换负责。
2. 移除可复制 `Grant` 的执行语义，确认不能覆盖受保护路径或平台能力失败。

**验证：** `go test -json -race -count=1 ./internal/permission` 与 `go test -json -count=1 ./internal/repoaudit -run TestLegacyGrantHasNoProductionExecutionReference` 通过；AST/import 审计忽略注释与迁移文档，只接受零生产执行引用，required test 明确 pass。
**覆盖：** F3–F6 / AC3–AC6。

### T2.16 定义工具策略、状态与三视图结果

**文件：** `internal/tool/execution_policy.go`, `internal/tool/execution_state.go`, `internal/tool/result.go`, `internal/tool/tool.go`, `internal/tool/executor_test.go`
**依赖：** T1.1、T2.15
**步骤：**
1. 分离 Risk、ReadOnly、ConcurrentSafe，固定六种执行状态。
2. `CancelledBeforeStart` 不生成 Result；Result 不含 raw stdout/stderr。

**验证：** `go test -json -count=1 ./internal/tool -run TestExecutionStateAndPolicyAreIndependent` 通过。
**覆盖：** F23、F24 / AC23、AC24。

### T2.17 实现一次性 ResultFactory

**文件：** `internal/tool/result_factory.go`, `internal/tool/result.go`, `internal/tool/executor_test.go`
**依赖：** T2.16、T1.2
**步骤：**
1. 从有界预览和 artifact metadata 一次生成 ModelContent、UserView、PersistedContent。
2. 三个视图均脱敏且只暴露 opaque Ref，禁止真实路径和 raw 缓冲。

**验证：** `go test -json -count=1 ./internal/tool -run TestResultFactoryProducesThreeSafeViews` 通过。
**覆盖：** F7、F10、F24 / AC7、AC10、AC24。

### T2.18 实现首字节流式采集

**文件：** `internal/tool/capture.go`, `internal/tool/capture_test.go`
**依赖：** T2.17、T1.26
**步骤：**
1. 第一字节同时进入私有 staging 和有界 UTF-8 预览，写前消费预算。
2. 内联以内 Abort；超过阈值 Commit；达到硬上限停止生产者并标记不完整。

**验证：** `go test -json -count=1 ./internal/tool -run TestCaptureStreamsToBoundedPreviewAndArtifact` 通过，并用分配断言证明内存不随完整输入线性增长。
**覆盖：** F9、F10 / AC9、AC10。

### T2.19 固定 Registry 与参数校验边界

**文件：** `internal/tool/registry.go`, `internal/tool/validation.go`, `internal/tool/executor_test.go`
**依赖：** T2.16
**步骤：**
1. Registry 返回不可变工具描述，参数校验在授权身份生成前完成。
2. 校验成功后产出键顺序确定的 canonical JSON；文件工具调用 Root.Bind，MCP 调用附最终配置 TargetDigest。
3. 支持 MCP raw JSON Schema，但远端 annotation 不能放宽本地策略。

**验证：** `go test -json -count=1 ./internal/tool -run 'TestRegistryValidatesBeforeAuthorizationAndKeepsPolicyLocal|TestCanonicalArgumentsAndBindingsAreDeterministic'` 通过。
**覆盖：** F5、F23、F24 / AC5、AC23、AC24。

### T2.20 原子切换唯一 Executor

**文件：** `internal/tool/executor.go`, `internal/tool/executor_test.go`
**依赖：** T2.9、T2.16、T2.17、T2.19
**步骤：**
1. Executor 用实际 Shell、当前 Root Identity、重新取得的 Bindings 和最终 MCP TargetDigest 重算身份并原子消费 Ticket。
2. 票据失败或启动前取消不调用工具；移除旧 Grant/直接执行入口。

**验证：** `go test -json -race -count=1 ./internal/tool -run 'TestExecutorConsumesTicketAtStartBoundary|TestResourceReplacementInvalidatesTicket|TestCancelBeforeStartHasNoResultOrSideEffect'` 通过。
**覆盖：** F3–F6、F24 / AC3–AC6、AC24。

### T2.21 迁移 Read 到句柄与预算

**文件：** `internal/tool/read.go`, `internal/tool/read_scope.go`, `internal/tool/file_tools_test.go`
**依赖：** T2.18、T2.20、T1.12、T1.13
**步骤：**
1. 通过当前 `root.OpenRead` 从第一字节限制单文件、行数和输出预算。
2. 在每次 read 前检查取消，底层错误返回安全受限结果。

**验证：** `go test -json -count=1 ./internal/tool -run TestReadCancellationLongLineAndBudget` 通过。
**覆盖：** F9、F12 / AC9、AC12。

### T2.22 迁移 Grep 到累计遍历预算

**文件：** `internal/tool/grep.go`, `internal/tool/read_scope.go`, `internal/tool/file_tools_test.go`
**依赖：** T2.21、T1.14
**步骤：**
1. 整次调用共享文件、目录、字节和行 Counter。
2. 遍历、打开和超长行处理均响应取消并报告局部错误。

**验证：** `go test -json -count=1 ./internal/tool -run TestGrepUsesSharedBudgetAndReportsScanErrors` 通过。
**覆盖：** F9、F12 / AC9、AC12。

### T2.23 迁移 Glob 到累计遍历预算

**文件：** `internal/tool/glob.go`, `internal/tool/read_scope.go`, `internal/tool/file_tools_test.go`
**依赖：** T2.21、T1.14
**步骤：**
1. 通过当前 `root.Walk` 匹配并确定性排序结果。
2. 限制目录、文件、结果数和累计字节，拒绝链接逃逸。

**验证：** `go test -json -count=1 ./internal/tool -run TestGlobIsBoundedCancelableAndSymlinkSafe` 通过。
**覆盖：** F9、F12 / AC9、AC12。

### T2.24 迁移 Write/Edit 到普通 capability

**文件：** `internal/tool/write.go`, `internal/tool/edit.go`, `internal/tool/file_tools_test.go`
**依赖：** T2.20、T1.15、T1.16
**步骤：**
1. 只接受组装根从该 Root 对应的 Bootstrap `OpenResult` 中保存之 `Capabilities.Ordinary()` 提取并注入的 capability，通过当前 `root.AtomicWrite` 处理文件。
2. 对删除、替换、rename、硬链接、符号链接和未创建权限槽位进行绕过测试。

**验证：** `go test -json -count=1 ./internal/tool -run TestWriteEditCannotMutateProtectedPermissionSlots` 通过。
**覆盖：** F4、F9 / AC4、AC9。

### T2.25 建立跨平台 Shell 选择

**文件：** `internal/tool/shell_posix.go`, `internal/tool/shell_windows.go`, `internal/tool/shell_unsupported.go`, `internal/tool/bash_posix_test.go`, `internal/tool/bash_windows_test.go`
**依赖：** T1.24、T2.8a
**步骤：**
1. 明确 POSIX 与 Windows 的 Shell/argv 语义并纳入调用身份。
2. unsupported 平台返回拒绝，不回退到任意宿主 shell。

**验证：** darwin/linux 的 `go test -json -count=1 ./internal/tool -run TestShellSelectionUsesExactPlatformArgv` required test pass；`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/tool-windows.test.exe" ./internal/tool` 成功。
**覆盖：** F3、F31 / AC3、AC31。

### T2.25a 补齐 proctree 安全组装契约

**文件：** `internal/proctree/api.go`, `internal/proctree/lifecycle.go`, `internal/proctree/protected_exec.go`, `internal/proctree/runner_posix.go`, `internal/proctree/launcher_linux.go`, `internal/proctree/runner_windows.go`, `internal/proctree/runner_factory_darwin.go`, `internal/proctree/runner_factory_linux.go`, `internal/proctree/runner_factory_windows.go`, `internal/proctree/runner_factory_unsupported.go`, `internal/proctree/lifecycle_test.go`, `internal/proctree/protected_exec_test.go`, `internal/proctree/assembly_contract_test.go`
**依赖：** T1.18a、T1.18b、T1.20、T1.21a、T1.23、T1.24
**步骤：**
1. 公开互斥 build tag 选择的 Runner factory 与每次创建独立私有 scratch 的 ProtectionPlan factory；unsupported 实现只能在目标启动前拒绝，任何 Start 失败或启动前取消都由 Runner 接管并清理已交付的 plan。
2. 为 Process 增加幂等 `CloseStdin`，由 owner 原子停止新写并发送 EOF；借用 pipe 的 `Close` 仍不能触及 owner handle。
3. 外部 package 测试证明只用公开契约即可构造 Runner 和单次 plan，且不暴露 scratch 写 capability、裸 `exec.Cmd` 或 OS handle。

**验证：** `go test -json -race -count=20 ./internal/proctree -run 'TestPublicRunnerFactorySelectsExactPlatform|TestProtectionPlanFactoryCreatesPrivatePerRunPlan|TestProcessCloseStdinSignalsEOFWithoutClosingBorrowedPipes'` 通过；`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/proctree-windows.test.exe" ./internal/proctree` 成功，required tests 无 skip。
**覆盖：** F4、F11、F31 / AC4、AC11、AC31。

### T2.26 迁移 Bash 到受保护 proctree

**文件：** `internal/tool/bash.go`, `internal/tool/bash_runtime_test.go`, `internal/tool/bash_posix_test.go`, `internal/tool/bash_windows_test.go`
**依赖：** T2.18、T2.20、T2.25、T2.25a、T1.20、T1.21、T1.23
**步骤：**
1. 仅以 `ProtectionRequired` 和已批准 ProtectionPlan 调用 Runner；只有返回 Process 才跨过启动边界。
2. 从 `Process.Pipes()` 排空 stdout/stderr 并接入同一操作预算与 artifact；stdin 用完即关闭。
3. 取消/超时调用 Process.Close，等待独立清理终止完整进程树；StartError 不生成已启动结果。

**验证：** `go test -json -race -count=20 ./internal/tool -run 'TestBashIdentityDistinguishesSemanticBytes|TestBashCannotWriteProtectedPermissionPaths|TestBashCancellationReapsDescendants|TestBashCaptureHonorsHardLimit'` 通过；Windows amd64 原生 runner 执行同组 required tests 无 skip。
**覆盖：** F3、F4、F9–F11、F31 / AC3、AC4、AC9–AC11、AC31。

### T2.27 完成内置工具执行入口切换

**文件：** `internal/tool/{executor,capture,bash,read,grep,glob,write,edit}.go`, `internal/permission/*`, `internal/tool/*_test.go`, `internal/repoaudit/tool_refs_test.go`
**依赖：** T2.21、T2.22、T2.23、T2.24、T2.26
**步骤：**
1. 检查所有内置工具只能经 Registry→Authorizer→Ticket→Executor 进入执行。
2. 删除或封闭直接调用、事后 truncate 和旧路径校验入口。

后续 Orchestrator 仍需原子接入 `硬约束/健康 → BeforeTool → 普通规则/确认 → Issue → Executor`，本任务不提前保留旁路。

**验证：** `go test -json -race -count=1 ./internal/permission ./internal/tool` 与 `go test -json -count=1 ./internal/repoaudit -run TestBuiltinToolsHaveOnlyExecutorEntryAndNoReadAllThenTruncate` 通过；required AST/data-flow 审计 test 明确 pass。
**覆盖：** F3–F12、F23、F24 / AC3–AC12、AC23、AC24。

### T2.28 定义 Instruction 图与缓存键

**文件：** `internal/instructions/types.go`, `internal/instructions/cache.go`, `internal/instructions/loader_test.go`
**依赖：** T1.34、T1.5、T1.11
**步骤：**
1. 定义来源、稳定文件身份、include 边和缓存键。
2. 缓存命中仍计入展开预算，不能用缓存绕过累计限制。

**验证：** `go test -json -count=1 ./internal/instructions -run TestInstructionCacheStillConsumesExpansionBudget` 通过。
**覆盖：** F13 / AC13。

### T2.29 实现有界 include 图展开

**文件：** `internal/instructions/include.go`, `internal/instructions/include_test.go`
**依赖：** T2.28、T1.14
**步骤：**
1. 以稳定身份去重重复/菱形 include，限制深度、文件、原始字节和展开字节。
2. 循环、超限和读取错误产生安全诊断，不返回部分未标记内容。

**验证：** `go test -json -count=1 ./internal/instructions -run TestIncludeGraphDeduplicatesAndEnforcesCumulativeLimits` 通过。
**覆盖：** F13 / AC13。

### T2.30 迁移 Instruction 加载到 safefs

**文件：** `internal/instructions/loader.go`, `internal/instructions/loader_test.go`, `internal/instructions/include_posix_test.go`, `internal/instructions/include_windows_test.go`
**依赖：** T2.29、T1.12、T1.13
**步骤：**
1. 发现与打开均使用同一 Root 句柄链并响应取消。
2. 增加并发切换符号链接/reparse point 的根外读取回归。

**验证：** `go test -json -race -count=20 ./internal/instructions -run 'TestIncludeLoadRejectsSymlinkRace|TestIncludeGraphDeduplicatesAndEnforcesCumulativeLimits'`、`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/instructions-windows.test.exe" ./internal/instructions` 和 Windows amd64 runner 的 `go test -json -count=20 ./internal/instructions -run TestIncludeLoadRejectsReparseRace` 均通过，required 记录次数满足且无 skip。
**覆盖：** F13、F31 / AC13、AC31。

### T2.31 迁移 Skill 发现到 safefs

**文件：** `internal/skill/discovery.go`, `internal/skill/discovery_test.go`
**依赖：** T1.14、T2.30
**步骤：**
1. 移除 Unix 专属直接导入，复用有界句柄遍历。
2. 保持既有 Skill 类型和优先级，不引入新业务能力。

**验证：** `go test -json -count=1 ./internal/skill -run TestDiscoveryIsBoundedAndCrossPlatform` required test pass；`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/skill-windows.test.exe" ./internal/skill` 成功。
**覆盖：** F7、F13、F31 / AC7、AC13、AC31。

### T2.31a 严格解析 Skill history 元数据

**文件：** `internal/skill/types.go`, `internal/skill/parser.go`, `internal/skill/history.go`, `internal/skill/parser_test.go`
**依赖：** T2.31
**步骤：**
1. 为既有 frontmatter `history` 保留 presence：未设置与显式 0 都产生确定值 0；isolated Skill 接受 0–1000，shared Skill 只接受 0。
2. 负数、1001、YAML 整数溢出和错误类型由 `ParseWithLimits`/`ValidateMetadata` 拒绝，错误只含安全来源标识、字段路径 `history` 和允许范围，不回显原标量或正文。

**验证：** `go test -json -count=1 ./internal/skill -run '^(TestSkillHistoryDefaultExplicitZeroAndIsolatedBoundaries|TestSharedSkillRejectsPositiveHistory|TestSkillHistoryRejectsNegativeCapPlusOneOverflowAndWrongTypeWithoutLeak)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F26 / AC7、AC26、AC35。

### T2.31b 非破坏跳过超限 Skill

**文件：** `internal/skill/manager.go`, `internal/skill/history.go`, `internal/skill/manager_test.go`
**依赖：** T2.31a
**步骤：**
1. refresh 遇到治理前已存在的 `history>1000` 定义时只从新 snapshot 禁用/跳过该 Skill，不删除、移动、覆盖或自动截断原 `SKILL.md`；已固定到进行中 invocation 的旧 snapshot 保持不变。
2. 诊断只发布安全来源标识、`history`、范围 `0..1000` 和降低值的迁移提示；修正后仅在下一次原子 refresh 重新启用。

**验证：** `go test -json -race -count=1 ./internal/skill -run '^(TestOverLimitHistorySkillIsSkippedWithoutSourceMutation|TestHistoryFixReenablesOnlyOnNextAtomicRefresh|TestInFlightSkillSnapshotIsNotRewritten)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F26 / AC7、AC26、AC35。

### T2.32 迁移命令 Hook 到 proctree

**文件：** `internal/hook/command.go`, `internal/hook/command_shell_posix.go`, `internal/hook/command_shell_windows.go`, `internal/hook/command_shell_unsupported.go`, `internal/hook/command_test.go`
**依赖：** T1.24、T2.18
**步骤：**
1. 命令 Hook 仅以 `ProtectionRequired` 和已批准 plan 通过共享 Runner 启动，Hook 规则只能增加限制。
2. 通过 `Process.Pipes` 安全采集输出，取消/关闭使用双 context 语义回收完整后代。

**验证：** `go test -json -count=1 ./internal/hook -run TestCommandHookUsesProcessTreeAndBoundedCapture` required test pass；`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/hook-windows.test.exe" ./internal/hook` 成功。
**覆盖：** F4、F7、F9、F11、F31 / AC4、AC7、AC9、AC11、AC31。

### T2.33 迁移 HTTP Hook 到 netpolicy

**文件：** `internal/hook/http.go`, `internal/hook/limits.go`, `internal/hook/http_test.go`
**依赖：** T1.34、T1.32、T1.33、T1.9
**步骤：**
1. 从 `ClientFactory` 获取客户端，删除 Hook 私有 URL/redirect 旁路，并把所创建 Client 的唯一关闭责任登记到 Hook Engine。
2. 对请求和响应首字节执行预算，错误写安全摘要。

**验证：** `go test -json -count=1 ./internal/hook -run TestHTTPHookRejectsUnsafeRedirectWithoutCredentialLeak` 通过。
**覆盖：** F7–F9 / AC7–AC9。

### T2.34 收敛 Hook 安全输入输出边界

**文件：** `internal/hook/api.go`, `internal/hook/engine.go`, `internal/hook/diagnostic.go`, `internal/hook/hard_boundary_test.go`
**依赖：** T2.17、T2.32、T2.33
**步骤：**
1. BeforeTool 只能拒绝，不得改写已绑定参数或签发 Ticket；AfterTool 只接收安全 Result。
2. Hook 诊断只接受 SafeText/SafeError，不得发布 raw action 输入、输出或配置标量。

**验证：** `go test -json -race -count=1 ./internal/hook -run '^(TestHookCannotWidenPermission|TestHookDiagnosticsAcceptOnlySafeValues)$'` 通过，两个 required test 均 pass 且无 skip。
**覆盖：** F4、F7–F9、F11 / AC4、AC7–AC9、AC11。

### T2.34a 固定 hooks.yaml schema version

**文件：** `internal/hook/loader.go`, `internal/hook/validate.go`, `internal/hook/loader_version_test.go`
**依赖：** T2.34
**步骤：**
1. Hook Loader 要求 `version` 必填且只能为整数 1；缺失、0、负数、2、YAML 整数溢出或错误类型均拒绝。
2. version 验证必须发生在 compile、网络 Client 构造、进程启动、worker 发布和任何文件改写之前，错误只含字段路径与允许值 1。

**验证：** `go test -json -count=1 ./internal/hook -run '^(TestHookVersionOneIsAccepted|TestHookVersionMissingZeroNegativeTwoOverflowAndWrongTypeFailBeforeEffects)$'` 通过，两个 required test 均 pass 且无 skip。
**覆盖：** F7、F28 / AC7、AC28。

### T2.34b 固定 Hook action timeout presence 与边界

**文件：** `internal/hook/loader.go`, `internal/hook/validate.go`, `internal/hook/timeout_test.go`
**依赖：** T2.34a
**步骤：**
1. compile 保留 rule timeout presence；command/HTTP/subagent 未设置分别应用 30 秒/10 秒/30 秒，显式 1 毫秒和 10 分钟接受。
2. 999 微秒、10 分钟加 1 纳秒、0、负数、`time.ParseDuration` 溢出以及 prompt action 携带 timeout 全部拒绝。该值不进入 Config Resolve，且与 2 秒 cleanup timeout 分属不同边界。

**验证：** `go test -json -count=1 ./internal/hook -run '^(TestHookTimeoutAbsentUsesActionDefaults|TestHookTimeoutAcceptsOneMillisecondAndTenMinutes|TestHookTimeoutRejectsBelowMinAboveCapZeroNegativeOverflowAndPrompt)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F9、F11、F28 / AC9、AC11、AC28。

### T2.34c 封闭 Hook worker 与 Client 生命周期

**文件：** `internal/hook/engine.go`, `internal/hook/async.go`, `internal/hook/lifecycle_test.go`
**依赖：** T2.34b
**步骤：**
1. Close 幂等；调用者超时只停止等待，内部 2 秒清理继续，最终结果缓存且失败只写一次共享 diagnostics。
2. 内部清理等待全部 Hook worker 后关闭 Engine 创建的 netpolicy Client；借用 Client 的调用点不得重复关闭。

**验证：** `go test -json -race -count=1 ./internal/hook -run '^(TestHookLifecycleClosesAllWorkers|TestHookCloseContinuesAfterWaiterTimeout|TestHookCleanupTimeoutForcesCloseAndDiagnosesOnce)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F8、F11 / AC7、AC8、AC11。

### T2.35 切换 Provider 请求与事件到安全 DTO

**文件：** `internal/provider/provider.go`, `internal/provider/request.go`, `internal/provider/event.go`, `internal/provider/observer.go`, `internal/provider/system_block_test.go`, `internal/provider/request_observer_test.go`
**依赖：** T1.10、T2.19
**步骤：**
1. 将 ChatRequest 的消息和 SystemBlock 内容改为 `SafeText`，不再接收 conversation.Message 或普通内容字符串。
2. 定义 `SafeToolCall` 与只携带 SafeText、SafeError、Usage 的 StreamEvent。
3. 工具参数脱敏后若 JSON 失效，返回 `unsafe_tool_arguments` 且不发布可执行调用。
4. 将 `RequestObserver`、`requestAttempt` 及其 sent/finish 一次性状态迁入 `observer.go`；请求未发送、已发送、取消和迟到回调都保持确定顺序，observer 不接收 raw 请求或错误载荷。

**验证：** `go test -json -race -count=1 ./internal/provider -run 'TestProviderDTOAcceptsOnlySafeText|TestUnsafeToolArgumentsAreRejected|TestRequestObserverContract|TestRequestObserverBarrier'` 通过。
**覆盖：** F7、F17 / AC7、AC17。

### T2.36 实现 ChatStream 双 context 生命周期

**文件：** `internal/provider/stream.go`, `internal/provider/provider.go`, `internal/provider/stream_lifecycle_test.go`
**依赖：** T2.35、T1.8
**步骤：**
1. 实现 Events、closeOnce、closeDone 和最终清理结果；底层流与事件通道只由生产者拥有。
2. 调用者 context 只限制等待，内部清理使用 Options 中不超过 2 秒的独立期限。
3. 首个等待者超时不覆盖最终清理结果；后续 Close 返回缓存结果。
4. 内部清理硬超限时执行最终强制关闭，并只向统一 Sink 写一次安全诊断。

**验证：** `go test -json -race -count=1 ./internal/provider -run 'TestChatStreamCloseIsIdempotent|TestCloseContinuesAfterWaiterTimeout|TestChatStreamCleanupTimeoutForcesCloseAndDiagnosesOnce'` 通过。
**覆盖：** F17 / AC17。

### T2.37 保证 Provider 生产者可被关闭解除阻塞

**文件：** `internal/provider/stream.go`, `internal/provider/stream_lifecycle_test.go`
**依赖：** T2.36
**步骤：**
1. 每次事件发送同时监听流关闭信号和派生 context。
2. 构造无人消费且事件通道已满的场景，确保 Close 取消底层读取并等待生产者退出。

**验证：** `go test -json -race -count=1 ./internal/provider -run TestChatStreamCloseUnblocksFullEventChannel` 通过。
**覆盖：** F17 / AC17。

### T2.38 实现 Provider 多维累计流预算

**文件：** `internal/provider/stream_limits.go`, `internal/provider/stream_limits_test.go`
**依赖：** T2.4、T2.37、T1.5
**步骤：**
1. 为 raw response、单事件、事件数、正文、thinking 和工具参数建立共享 Counter。
2. 每次读取、扩容和事件发布前消费预算；LimitError 不携带载荷。

**验证：** `go test -json -count=1 ./internal/provider -run TestProviderStreamLimitsAreCumulativeAndPreallocateSafe` 通过。
**覆盖：** F9、F17 / AC9、AC17。

### T2.39 迁移同步有界 SSE decoder

**文件：** `internal/provider/sse_decoder.go`, `internal/provider/sse_decoder_test.go`
**依赖：** T2.38
**步骤：**
1. 将现有 SSE 读取改为同步 decoder，不启动独立无人管理 goroutine。
2. 显式区分扫描错误、事件超限、正常协议终止和意外 EOF。

**验证：** `go test -json -count=1 ./internal/provider -run 'TestSSEDecoderReportsScannerError|TestSSEDecoderRejectsOversizedEvent'` 通过。
**覆盖：** F17 / AC17。

### T2.40 拆分 OpenAI 请求映射

**文件：** `internal/provider/openai.go`, `internal/provider/openai_request.go`, `internal/provider/openai_request_test.go`
**依赖：** T2.35
**步骤：**
1. 将消息、SystemBlock、工具 schema 与 cache 映射移入 request 文件。
2. 只从安全 DTO 生成请求，并保持现有合法请求 JSON 的语义。

**验证：** `go test -json -count=1 ./internal/provider -run 'TestOpenAIRequestUsesSafeDTO|TestOpenAIToolSchemaCompatibility'` 通过。
**覆盖：** F7、F18 / AC7、AC18。

### T2.41 实现 OpenAI 终态与 usage 状态机

**文件：** `internal/provider/openai_stream.go`, `internal/provider/openai_stream_test.go`
**依赖：** T2.37、T2.39、T2.40
**步骤：**
1. 固定 `Reading → FinishReasonSeen → UsageSeen → [DONE] → Completed`。
2. finish_reason 只更新状态，继续读取并发布唯一最终 Usage。

**验证：** `go test -json -count=1 ./internal/provider -run TestOpenAIFinishThenUsageThenDone` 通过。
**覆盖：** F18 / AC18。

### T2.42 封闭 OpenAI 五类退出路径

**文件：** `internal/provider/openai_stream.go`, `internal/provider/openai_stream_test.go`
**依赖：** T2.41
**步骤：**
1. 缺少 `[DONE]` 的 EOF、畸形 chunk 和 scanner error 均产生 SafeError，不发正常 Done。
2. 用 tracking body 覆盖正常、解析错、网络错、取消和消费者提前退出，逐项断言关闭。

**验证：** `go test -json -race -count=1 ./internal/provider -run 'TestOpenAIUnexpectedEOFIsError|TestOpenAIClosesBodyOnEveryExit'` 通过。
**覆盖：** F17、F18 / AC17、AC18。

### T2.43 拆分 Anthropic 请求映射

**文件：** `internal/provider/anthropic.go`, `internal/provider/anthropic_request.go`, `internal/provider/anthropic_schema_test.go`
**依赖：** T2.35
**步骤：**
1. 将消息、system/cache 与工具 schema 映射移入 request 文件。
2. 只从安全 DTO 生成 SDK 参数并保持现有有效 schema 语义。

**验证：** `go test -json -count=1 ./internal/provider -run 'TestAnthropicRequestUsesSafeDTO|TestAnthropicSchemaCompatibility'` 通过。
**覆盖：** F7、F17 / AC7、AC17。

### T2.44 纳管 Anthropic SDK stream

**文件：** `internal/provider/anthropic_stream.go`, `internal/provider/anthropic_stream_test.go`
**依赖：** T2.36、T2.38、T2.43
**步骤：**
1. 让 ChatStream 单独拥有 SDK stream，并在事件发布前应用预算和脱敏。
2. 正常、SDK error、取消和提前退出均关闭底层 stream 并等待生产者。

**验证：** `go test -json -race -count=1 ./internal/provider -run TestAnthropicClosesSDKStreamOnEveryExit` 通过。
**覆盖：** F7、F17 / AC7、AC17。

### T2.45 迁移 Provider Factory 到受控网络客户端

**文件：** `internal/provider/factory.go`, `internal/provider/factory_test.go`, `internal/provider/openai.go`, `internal/provider/anthropic.go`
**依赖：** T1.31、T1.32、T2.42、T2.44
**步骤：**
1. Factory 只接受已验证 Endpoint 与 netpolicy Client，不自行创建默认 http.Client。
2. OpenAI 使用 Client.Do；Anthropic 将 `SDKHTTPClient()` 原样交给 SDK，不替换 Transport。
3. Provider 只借用 Client；创建 Client 的组装根保留唯一关闭责任，并在 Provider 流全部结束后调用 `CloseIdleConnections`。

**验证：** `go test -json -count=1 ./internal/provider -run 'TestFactoryUsesInjectedNetpolicyClient|TestAnthropicKeepsGuardedSDKClient'` 通过。
**覆盖：** F7、F8、F17 / AC7、AC8、AC17。

### T2.46 建立 Provider secret 与生命周期联合回归

**文件：** `internal/provider/secret_canary_test.go`, `internal/provider/openai_stream_test.go`, `internal/provider/anthropic_stream_test.go`
**依赖：** T2.45
**步骤：**
1. 在 key、delta、tool args 和 HTTP/SDK error 中放入同一 canary。
2. 断言事件、SafeError、测试输出和 diagnostics 均无原文，并重复五类关闭路径。

**验证：** `go test -json -race -count=1 ./internal/provider -run 'Canary|EveryExit'` 通过。
**覆盖：** F7、F17、F18 / AC7、AC17、AC18。

### T2.47 迁移 MCP 协议 DTO 与有界 codec

**文件：** `internal/mcpclient/protocol/types.go`, `internal/mcpclient/protocol/jsonrpc.go`, `internal/mcpclient/protocol/codec.go`, `internal/mcpclient/protocol/codec_test.go`
**依赖：** T1.34、T1.5
**步骤：**
1. 迁移 JSON-RPC/MCP DTO，保持 numeric 与 string ID 可区分并 round-trip。
2. 在 json.Unmarshal 前消费 frame 预算；畸形/超限错误不含 payload。

**验证：** `go test -json -count=1 ./internal/mcpclient/protocol -run 'TestRPCIDRoundTrip|TestDecodeConsumesBudgetBeforeUnmarshal'` 通过。
**覆盖：** F14 / AC14。

### T2.48 定义 MCP Transport 与关闭状态机

**文件：** `internal/mcpclient/transport/transport.go`, `internal/mcpclient/transport/state.go`, `internal/mcpclient/transport/limits.go`, `internal/mcpclient/transport/state_test.go`
**依赖：** T2.47、T1.8
**步骤：**
1. 定义只传 raw frame 的 Start/Send/Receive/Close 契约和显式状态。
2. Options 注入独立清理期限及统一 Sink；重复 Close 等待同一最终结果。

**验证：** `go test -json -race -count=1 ./internal/mcpclient/transport -run 'TestTransportStateTransitions|TestTransportCloseContinuesAfterWaiterTimeout'` 通过。
**覆盖：** F14–F16 / AC14–AC16。

### T2.49 实现容量一且永不关闭的 pending

**文件：** `internal/mcpclient/pending.go`, `internal/mcpclient/connection_test.go`
**依赖：** T2.47、T2.48
**步骤：**
1. pending response channel 容量固定为 1，任何路径都不得 close。
2. `takePending` 原子移除，响应、取消、发送失败和连接终止只能有一个赢家。

**验证：** `go test -json -race -count=1 ./internal/mcpclient -run TestTakePendingHasSingleWinnerWithoutChannelClose` 通过。
**覆盖：** F15、F16 / AC15、AC16。

### T2.50 实现唯一 MCP receive loop

**文件：** `internal/mcpclient/connection.go`, `internal/mcpclient/connection_test.go`
**依赖：** T2.49、T1.8
**步骤：**
1. Connection 独占 Transport 并启动唯一 receive loop。
2. 合法响应完成对应 pending；未知/迟到 ID 只进入有界聚合诊断。
3. 连接终止一次性取走并失败全部 pending。
4. Transport 返回不可恢复的 framing/protocol 错误时，由 Connection 唯一进入 Failed、完成全部 pending 并关闭 Transport。

**验证：** `go test -json -race -count=1 ./internal/mcpclient -run 'TestConnectionHasSingleReceiveLoop|TestConnectionFailureCompletesAllPending|TestFatalReceiveErrorFailsConnectionAndPending'` 通过。
**覆盖：** F14–F16 / AC14–AC16。

### T2.51 封闭 MCP 响应、取消与 Connection Close 竞态

**文件：** `internal/mcpclient/connection.go`, `internal/mcpclient/connection_test.go`
**依赖：** T2.50
**步骤：**
1. 注入响应与 context cancel 同时发生的屏障。
2. 断言调用只完成一次、pending 归零、迟到响应有界且无 send-on-closed。
3. 首次 Connection Close 用独立清理 context 执行“拒绝发送→失败 pending→Transport.Close→等待 receive loop”；调用者超时不缓存，后续 Close 返回最终结果，硬超限只诊断一次。

**验证：** `go test -json -race -count=20 ./internal/mcpclient -run 'TestResponseCancellationRace|TestConnectionCloseContinuesAfterWaiterTimeout'` 通过。
**覆盖：** F15、F16、F32 / AC15、AC16、AC32。

### T2.52 实现 Manager 调用 lease

**文件：** `internal/mcpclient/lease.go`, `internal/mcpclient/lease_test.go`
**依赖：** T2.48
**步骤：**
1. 在同一锁区完成 Running 检查、Acquire 与 BeginClose。
2. Closing 后拒绝新 lease，已有 lease 可释放；重复 BeginClose 共享 closeDone。

**验证：** `go test -json -race -count=20 ./internal/mcpclient -run TestLeaseAcquireCloseRace` 通过。
**覆盖：** F15 / AC15。

### T2.53 实现 ServerSession 启动与累计发现预算

**文件：** `internal/mcpclient/session.go`, `internal/mcpclient/session_test.go`
**依赖：** T2.50、T2.52、T2.4
**步骤：**
1. 按 Transport.Start→Connection loop→initialize→capabilities→分页 tools/list 推进状态。
2. 全部分页共享 pages/tools/bytes Counter；超限或失败立即回滚且不发布半份工具。

**验证：** `go test -json -count=1 ./internal/mcpclient -run 'TestSessionStartupRollback|TestSessionDiscoveryUsesCumulativeBudget'` 通过。
**覆盖：** F14、F15 / AC14、AC15。

### T2.54 固定 MCP 远程工具本地安全策略

**文件：** `internal/mcpclient/adapter.go`, `internal/mcpclient/names.go`, `internal/mcpclient/adapter_test.go`
**依赖：** T2.17、T2.19、T2.53
**步骤：**
1. 远程工具默认危险、ReadOnly=false、ConcurrentSafe=false，annotation 不能放宽。
2. 注册名、server config digest 与安全参数进入 CallIdentity；返回值只经 ResultFactory 输出安全视图。

**验证：** `go test -json -count=1 ./internal/mcpclient -run 'TestRemoteToolDefaultsFailClosed|TestMCPIdentityBindsServerAndArguments|TestAdapterReturnsOnlySafeViews'` 通过。
**覆盖：** F5、F7、F23 / AC5、AC7、AC23。

### T2.55 迁移 MCP HTTP Transport 到 netpolicy

**文件：** `internal/mcpclient/factory.go`, `internal/mcpclient/transport/http/config.go`, `internal/mcpclient/transport/http/transport.go`, `internal/mcpclient/transport/http/transport_test.go`
**依赖：** T1.31–T1.33、T2.48
**步骤：**
1. HTTP Transport 通过 `ClientFactory`、已验证 Endpoint 和固定 Options 创建专属 netpolicy Client，删除私有 URL/redirect 旁路。
2. Transport 是该 Client 的创建者与唯一所有者；每个 Send 只使用 `Client.Do`，本地 Close 最后且只调用一次 `CloseIdleConnections`。

**验证：** `go test -json -count=1 ./internal/mcpclient/transport/http -run 'TestHTTPTransportRequiresGuardedClient|TestHTTPTransportOwnsAndClosesClientOnce'` 通过。
**覆盖：** F7、F8、F15 / AC7、AC8、AC15。

### T2.56 跟踪并关闭所有 MCP HTTP body

**文件：** `internal/mcpclient/transport/http/body_registry.go`, `internal/mcpclient/transport/http/body_registry_test.go`
**依赖：** T2.55
**步骤：**
1. response 到达即注册，消费结束即移除。
2. Close 原子取走并关闭全部活动 body，注册与 Close 竞态不能遗漏。

**验证：** `go test -json -race -count=20 ./internal/mcpclient/transport/http -run TestBodyRegistryCloseRace` 通过。
**覆盖：** F15 / AC15。

### T2.57 实现独立 HTTP JSON 响应预算

**文件：** `internal/mcpclient/transport/http/json.go`, `internal/mcpclient/transport/http/limits_test.go`
**依赖：** T2.47、T2.56
**步骤：**
1. 未知或伪造 Content-Length 时仍逐块先消费预算再分配/解码。
2. 超限关闭当前 body 且只失败当前调用，连接仍可用于后续独立请求。

**验证：** `go test -json -count=1 ./internal/mcpclient/transport/http -run TestChunkedJSONLimitIsPerCall` 通过。
**覆盖：** F14 / AC14。

### T2.58 实现 MCP HTTP SSE 累计预算

**文件：** `internal/mcpclient/transport/http/sse.go`, `internal/mcpclient/transport/http/limits_test.go`
**依赖：** T2.47、T2.56、T1.8
**步骤：**
1. 对 raw 字节、单事件、事件数和协议错误累计计量。
2. 无法重新同步的超限或连续恶意错误由 Receive 返回不含 payload 的 fatal error；上层 Connection 统一进入 Failed、失败 pending 并关闭 Transport。

**验证：** `go test -json -count=1 ./internal/mcpclient/transport/http -run 'TestChunkedSSEUsesCumulativeBudget|TestMaliciousProtocolErrorsAreBounded'` 通过。
**覆盖：** F9、F14、F15 / AC9、AC14、AC15。

### T2.59 实现 HTTP `RemoteSessionCloser`

**文件：** `internal/mcpclient/transport/http/session.go`, `internal/mcpclient/transport/http/session_test.go`, `internal/mcpclient/session.go`
**依赖：** T2.53、T2.55
**步骤：**
1. HTTP Transport 保存最近一次有效 session ID 并实现 CloseRemote；stdio 不实现该接口。
2. ServerSession 首次 Close 启动不继承调用者取消的内部清理，在 Connection 可用时先以独立有界 context 调用远端清理，再无条件进入 Connection/Transport 本地关闭。
3. 调用者 context 只限制等待且等待错误不缓存；后续 Close 返回最终清理结果，内部硬超限只向统一 Sink 诊断一次。

**验证：** `go test -json -race -count=1 ./internal/mcpclient/... -run 'TestRemoteSessionCleanupPrecedesLocalClose|TestRemoteCleanupFailureStillClosesLocally|TestSessionCloseContinuesAfterWaiterTimeout'` 通过。
**覆盖：** F15 / AC15。

### T2.60 完成 MCP HTTP 双 context Close

**文件：** `internal/mcpclient/transport/http/transport.go`, `internal/mcpclient/transport/http/transport_test.go`
**依赖：** T2.56–T2.59
**步骤：**
1. 本地 Close 拒绝新发送、取消 I/O、关闭 body、等待 worker，不重复远端清理。
2. 调用者超时只返回等待错误；内部清理继续并缓存最终结果，硬超限只诊断一次。

**验证：** `go test -json -race -count=20 ./internal/mcpclient/transport/http -run TestHTTPTransportCloseLifecycle` 通过。
**覆盖：** F15 / AC15。

### T2.61 使用受保护 Process 启动 stdio Transport

**文件：** `internal/mcpclient/transport/stdio/config.go`, `internal/mcpclient/transport/stdio/transport.go`, `internal/mcpclient/transport/stdio/transport_test.go`
**依赖：** T1.24、T2.48
**步骤：**
1. 仅以 ProtectionRequired 和已批准 plan 调用 Runner；StartError 时不得进入 Running。
2. 从 Process.Pipes 取得唯一 stdin/stdout/stderr，Transport 不接触裸进程或 OS handle。

**验证：** `go test -json -count=1 ./internal/mcpclient/transport/stdio -run TestStartRequiresProtectedProcessAndBorrowedPipes` 通过。
**覆盖：** F4、F16、F31 / AC4、AC16、AC31。

### T2.62 实现 stdio 有界 framing

**文件：** `internal/mcpclient/transport/stdio/framing.go`, `internal/mcpclient/transport/stdio/limits_test.go`
**依赖：** T2.47、T2.61
**步骤：**
1. newline frame 在扩容和反序列化前消费累计字节/事件预算。
2. 超长或畸形 frame 返回不含 payload 的 fatal framing error，不继续盲读；Connection 统一失败 pending 并关闭 Transport。

**验证：** `go test -json -count=1 ./internal/mcpclient/transport/stdio -run TestOversizedFrameFailsSharedTransport` 通过。
**覆盖：** F14、F16 / AC14、AC16。

### T2.63 持续排空并限制 stdio stderr

**文件：** `internal/mcpclient/transport/stdio/stderr.go`, `internal/mcpclient/transport/stdio/stderr_test.go`
**依赖：** T2.61、T1.8
**步骤：**
1. Start 后立即持续读取 stderr 到 EOF。
2. 只向统一 Sink 写入有界脱敏摘要、重复计数和 dropped，不保存 raw。

**验证：** `go test -json -count=1 ./internal/mcpclient/transport/stdio -run TestStderrFloodDoesNotBlockOrLeak` 通过。
**覆盖：** F7、F16 / AC7、AC16。

### T2.64 实现单一可中断 stdio writer

**文件：** `internal/mcpclient/transport/stdio/writer.go`, `internal/mcpclient/transport/stdio/writer_test.go`
**依赖：** T2.61
**步骤：**
1. 所有发送进入单 writer 队列，取消等待不为每次写创建无主 goroutine。
2. writer 取消只触发 Transport/Process 关闭；借用的 stdin 仍由 `Process.Close` 在终止、reap 后唯一关闭，pending 由 Connection 统一完成。

**验证：** `go test -json -race -count=20 ./internal/mcpclient/transport/stdio -run TestCancelledWriteDoesNotLeakGoroutine` 通过。
**覆盖：** F16 / AC16。

### T2.65 实现 stdio supervisor 的唯一等待语义

**文件：** `internal/mcpclient/transport/stdio/supervisor.go`, `internal/mcpclient/transport/stdio/supervisor_test.go`
**依赖：** T2.61、T2.63、T2.64
**步骤：**
1. supervisor 只调用可重复等待的 Process.Wait；底层 Process 内部仍是唯一 OS Wait/reap 所有者。
2. 将同一退出结果广播给 Receive、Close 和 diagnostics，不关闭共享 channel 制造竞态。

**验证：** `go test -json -race -count=20 ./internal/mcpclient/transport/stdio -run TestSupervisorObservesSingleProcessResult` 通过。
**覆盖：** F16 / AC16。

### T2.66 完成 stdio 双 context Close 与失败回滚

**文件：** `internal/mcpclient/transport/stdio/transport.go`, `internal/mcpclient/transport/stdio/transport_test.go`
**依赖：** T2.62–T2.65
**步骤：**
1. Close 固定为停止接收写请求→`Process.Close`→等待 writer/framing/stderr/supervisor；Transport 不直接关闭或“释放”借用 pipe。
2. 在 initialize 前后注入失败/超时，断言后台清理继续且进程、pipe、worker 最终归零。
3. 突发响应后退出重复 20 次不得 panic 或丢失终态。

**验证：** `go test -json -race -count=20 ./internal/mcpclient/transport/stdio -run 'TestStdioCloseAndStartupRollback|TestBurstThenExit'` 通过。
**覆盖：** F15、F16、F32 / AC15、AC16、AC32。

### T2.67 实现 Manager 发布与幂等关闭

**文件：** `internal/mcpclient/manager.go`, `internal/mcpclient/factory.go`, `internal/mcpclient/manager_lifecycle_test.go`
**依赖：** T2.51–T2.54、T2.59、T2.60、T2.66
**步骤：**
1. 只有 Running Session 原子发布工具快照；失败 Session 不暴露 adapter。
2. Closing 后拒绝新 lease，依次取消、远端清理、本地关闭、等待 receive loop/既有 lease，再发布 Closed。
3. 首次 Close 使用独立内部清理 context；调用者 context 只限制等待且其错误不缓存，后续 Close 返回同一最终结果，内部硬超限执行强制路径并只诊断一次。
4. Snapshot/Tools/CallTool 与 Close 并发无竞态。

**验证：** `go test -json -race -count=20 ./internal/mcpclient -run 'TestManagerPublishesOnlyRunningSessions|TestManagerCloseSnapshotRace|TestManagerCloseContinuesAfterWaiterTimeout'` 通过。
**覆盖：** F15、F16 / AC15、AC16。

### T2.68 建立 MCP 全链路 secret 回归

**文件：** `internal/mcpclient/secret_canary_test.go`, `internal/mcpclient/adapter_test.go`, `internal/mcpclient/transport/http/secret_canary_test.go`, `internal/mcpclient/transport/stdio/stderr_test.go`
**依赖：** T2.54、T2.60、T2.66、T2.67
**步骤：**
1. 在 env/header、返回值、HTTP error、协议错误和 stderr 中放入同一 canary。
2. 检查 Tool Result、diagnostics、测试输出和 Manager snapshot 均无原文；恶意重定向目标未收到认证头。

**验证：** `go test -json -race -count=1 ./internal/mcpclient/... -run 'Canary|RedirectNeverForwards'` 通过。
**覆盖：** F7、F8、F14–F16 / AC7、AC8、AC14–AC16。

### T2.69 原子切换 Provider/MCP 跨包接口

**文件：** `internal/provider/sse.go`, `internal/mcpclient/{http,http_closer,stdio,stdio_unix,stdio_other,jsonrpc,protocol,limits,redact}.go`, `internal/orchestrator/{chat,stream_collector}.go`, `internal/events/events.go`, `internal/app/deps.go`, `cmd/xagent/main.go`, `internal/repoaudit/provider_mcp_refs_test.go`
**依赖：** T2.46、T2.68
**步骤：**
1. 在一个连续工作单元内把全部生产调用方迁到安全 DTO、`ChatStream`、MCP Manager/adapter、Connection 和新 Transport；Events 只携带 C5 安全 DTO，正常路径建立明确的 stream owner 与 Close。
2. 移除旧入口的导出、注册与生产引用，加入静态零引用断言；旧源文件只标记为 M5 条件删除候选，本任务不得物理删除。
3. 全仓编译与正常路径测试通过后才结束接口切换；错误、取消和消费者提前退出的完整关闭矩阵由 T3.25 集中补齐并回归。

**验证：** `go test -json -count=1 ./...`、`go test -json -race -count=1 ./internal/provider ./internal/mcpclient/... ./internal/orchestrator ./internal/events ./internal/app` 和 `go test -json -count=1 ./internal/repoaudit -run TestLegacyProviderMCPEntrypointsHaveNoProductionReferences` 通过；候选文件仍保留。
**覆盖：** F7、F8、F14–F18、F31 / AC7、AC8、AC14–AC18、AC31。

### T2.70 执行 M2 运行时安全门禁

**文件：** `internal/{config,permission,tool,instructions,hook,provider,mcpclient}/**`, `cmd/xagent/internal_mode.go`
**依赖：** T2.7、T2.27、T2.30、T2.31、T2.31a、T2.31b、T2.34、T2.34a、T2.34b、T2.34c、T2.51、T2.69
**步骤：**
1. 运行授权碰撞、受保护路径、预算、Provider 五类退出、MCP close/stdio 敏感场景。
2. 对并发和生命周期包运行 race，并为 darwin/linux/windows 构建相关测试二进制。

**验证：** `go test -json -race -count=1 ./internal/config ./internal/permission ./internal/tool ./internal/instructions ./internal/hook ./internal/provider ./internal/mcpclient/...` 与 `go test -json -race -count=20 ./internal/provider ./internal/mcpclient/... -run 'EveryExit|Close|Cancellation|Burst|BlockedWrite|ResponseCancellationRace'` 通过且 required 记录完整；随后执行 `GOOS=darwin GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/proctree-darwin.test" ./internal/proctree`、`GOOS=linux GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/proctree-linux.test" ./internal/proctree`、`GOOS=windows GOARCH=amd64 go test -c -o "$XAGENT_TASK_TMP/proctree-windows.test.exe" ./internal/proctree`、`GOOS=darwin GOARCH=amd64 go build -o "$XAGENT_TASK_TMP/xagent-darwin" ./cmd/xagent`、`GOOS=linux GOARCH=amd64 go build -o "$XAGENT_TASK_TMP/xagent-linux" ./cmd/xagent`、`GOOS=windows GOARCH=amd64 go build -o "$XAGENT_TASK_TMP/xagent-windows.exe" ./cmd/xagent`，全部通过。
**覆盖：** F3–F18、F23、F24、F28、F31、F32 / AC3–AC18、AC23、AC24、AC28、AC31、AC32、AC35。

## M3：Conversation 与 Orchestrator 正确性

### T3.1 定义 Conversation v2 状态契约

**文件：** `internal/conversation/conversation.go`, `internal/conversation/message.go`, `internal/conversation/store.go`
**依赖：** T2.70、T1.1、T1.25、T2.17
**步骤：**
1. 按 Plan 定义 `PersistedState`、`ListResult`、`LoadResult`、`SaveResult` 和 `MaintenanceResult`，让调用方能区分成功、无变化、部分结果与恢复诊断。
2. v2 消息只保存 `SafeText`、安全工具状态和 opaque `artifact.Ref`；`ExternalPath` 只允许存在于 legacy DTO。

**验证：** `go test -json -count=1 ./internal/conversation -run TestV2TypesContainNoRawPathOrUnsafeText` 通过。
**覆盖：** F7、F10、F19、F20、F24 / AC7、AC10、AC19、AC20、AC24。

### T3.2 实现规范化状态摘要

**文件：** `internal/conversation/state_digest.go`, `internal/conversation/state_digest_test.go`
**依赖：** T3.1
**步骤：**
1. 使用带版本、固定字段顺序的规范 DTO 计算 SHA-256，并统一 nil 与 empty 的表达。
2. 摘要覆盖消息、工具状态、artifact Ref、摘要、上下文和元数据；任何持久化字段变化都必须改变摘要。

**验证：** `go test -json -count=1 ./internal/conversation -run TestStateDigestCoversEveryPersistedField` 通过。
**覆盖：** F20 / AC20。

### T3.3 定义并校验 v2 JSONL record

**文件：** `internal/conversation/jsonl_record.go`, `internal/conversation/jsonl_record_test.go`
**依赖：** T3.2
**步骤：**
1. 实现 batch/snapshot 二选一、版本、session ID、revision、`PreviousDigest` 和 `Digest` 校验。
2. 拒绝未知版本、重复 payload、缺失字段、空 payload 和不连续 revision。

**验证：** `go test -json -count=1 ./internal/conversation -run TestV2RecordValidationMatrix` 通过。
**覆盖：** F19、F20 / AC19、AC20。

### T3.4 建立有界 record codec

**文件：** `internal/conversation/jsonl_record.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T3.3、T1.5、T2.4
**步骤：**
1. 编码和解码前执行 `session.max_record_bytes`，读取期间累计执行 `session.max_session_bytes`。
2. 超限错误只包含 session ID、预算维度和限制，不携带消息正文或 raw record。

**验证：** `go test -json -count=1 ./internal/conversation -run TestRecordAndSessionBudgetsUseC8Keys` 通过。
**覆盖：** F19 / AC19。

### T3.5 实现 Save 分类器

**文件：** `internal/conversation/jsonl_save.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T3.2–T3.4
**步骤：**
1. 规范摘要完全相同返回 `SaveNoop`，不得写文件或前移 revision。
2. 只有尾部追加且其他状态未变时生成一个 batch；其他变化生成一个完整 snapshot。
3. 从 torn tail、坏行或摘要链断裂恢复后的下一次保存强制走 snapshot 原子替换，禁止在不可验证字节后继续 append。

**验证：** `go test -json -count=1 ./internal/conversation -run TestSaveClassificationUsesNormalizedState` 通过。
**覆盖：** F20 / AC20。

### T3.6 实现 batch 崩溃安全追加

**文件：** `internal/conversation/jsonl_save.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T3.5
**步骤：**
1. 完整编码一个 batch record 后再 append、写换行、flush 和 fsync。
2. 只有全部持久化步骤成功后才更新内存 `PersistedState`。

**验证：** `go test -json -count=1 ./internal/conversation -run TestBatchSavePublishesExactlyOneRevision` 通过。
**覆盖：** F19、F20 / AC19、AC20、AC34。

### T3.7 实现 snapshot 原子替换

**文件：** `internal/conversation/jsonl_save.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T3.5
**步骤：**
1. 在同目录 staging 中复制已验证记录并追加一个 snapshot，禁止直接覆盖现有文件。
2. flush、fsync 和 atomic replace 全部成功后才更新状态；任一步失败保留旧文件。
3. 替换成功后重新创建真实 Store 并加载，必须读到完整、摘要链有效的 snapshot 及其 revision。

**验证：** `go test -json -count=1 ./internal/conversation -run TestSnapshotSaveIsAtomicAndReloadable` 通过。
**覆盖：** F19、F20 / AC19、AC20、AC34。

### T3.8 固定保存失败语义

**文件：** `internal/conversation/jsonl_store_test.go`
**依赖：** T3.6、T3.7
**步骤：**
1. 分别注入 encode、write、sync 和 rename 失败，并覆盖 batch 与 snapshot。
2. 断言磁盘最后成功状态和内存 `PersistedState` 均不前移，重试不会跳 revision。
3. 以 test-only 子进程和显式 barrier 覆盖 write-before-fsync、fsync-before-publish、staging-before-replace、replace-before-state-publish 四个提交窗口；父测试终止子进程并重新创建真实 JSONL Store，至少能读到中断前最后成功 revision；若新 revision 已跨过持久化提交点，只能接受完整且摘要链有效的下一 revision，绝不接受半条、混合或旧状态被破坏。该入口只属于测试二进制，不增加生产 CLI 模式。

**验证：** `go test -json -race -count=20 ./internal/conversation -run 'TestSaveFailurePreservesLastCommittedState|TestProcessInterruptionAtCommitBarriersPreservesLastRevision'` 通过。
**覆盖：** F19、F20 / AC19、AC20、AC34。

### T3.9 实现 v2 摘要链加载

**文件：** `internal/conversation/jsonl_load.go`, `internal/conversation/recovery_test.go`
**依赖：** T3.3、T3.4
**步骤：**
1. 验证 revision 连续、`PreviousDigest` 和重算 `Digest`，并累计消费会话预算。
2. batch 按顺序应用；snapshot 只在完整校验后原子替换内存候选状态。

**验证：** `go test -json -count=1 ./internal/conversation -run TestLoadV2ValidatesRevisionAndDigestChain` 通过。
**覆盖：** F19、F20 / AC19、AC20。

### T3.10 恢复 torn tail 与损坏记录

**文件：** `internal/conversation/jsonl_load.go`, `internal/conversation/recovery.go`, `internal/conversation/recovery_test.go`
**依赖：** T3.9、T1.8
**步骤：**
1. 忽略没有换行的尾记录；中间坏行和超限记录生成有界、安全的 `RecoveryReport`。
2. 返回最后一个可验证状态，不删除、覆盖或自动修剪原文件。

**验证：** `go test -json -count=1 ./internal/conversation -run TestRecoveryHandlesTornTailAndBoundedCorruption` 通过。
**覆盖：** F19、F21 / AC19、AC21、AC34。

### T3.11 实现有界会话列表

**文件：** `internal/conversation/jsonl_store.go`, `internal/conversation/recovery.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T3.10、T2.4
**步骤：**
1. 枚举使用 `session.max_scan_files` 和 `session.max_scan_bytes`，每次读取前检查 context。
2. 单文件损坏返回 recoverable placeholder；预算耗尽返回 `Truncated=true`；目录错误返回顶层 error。

**验证：** `go test -json -count=1 ./internal/conversation -run TestListReturnsPartialResultsPlaceholdersAndTopLevelError` 通过。
**覆盖：** F19、F21、F22 / AC19、AC21、AC22。

### T3.12 实现会话尺寸 checkpoint

**文件：** `internal/conversation/jsonl_save.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T3.7、T3.9、T2.4
**步骤：**
1. 下一 revision 将超过 `session.max_session_bytes` 时，以单 snapshot checkpoint 原子替换历史记录。
2. checkpoint 保留当前逻辑 revision 和摘要，且自身必须同时满足 record 与 session 硬上限。

**验证：** `go test -json -count=1 ./internal/conversation -run TestSessionSizeCheckpointRemainsLoadable` 通过。
**覆盖：** F19 / AC19。

### T3.13 实现保留与扫描维护

**文件：** `internal/conversation/cleanup.go`, `internal/conversation/jsonl_store_test.go`
**依赖：** T3.11、T3.12
**步骤：**
1. `Maintain` 只使用 Resolve 后的 `session.retention_days`；未设置时为 30 天，配置已在进入 Store 前受 3650 天硬上限约束，并同时服从扫描文件数与字节预算。
2. 每次枚举前检查 context；迁移未成功提交 v2 snapshot 的 legacy 文件不得删除。

**验证：** `go test -json -count=1 ./internal/conversation -run TestMaintainHonorsC8RetentionAndScanLimits` 通过。
**覆盖：** F22 / AC22、AC35。

### T3.14 接入时间跨度提醒

**文件：** `internal/conversation/recovery.go`, `internal/conversation/recovery_test.go`
**依赖：** T3.10、T2.4
**步骤：**
1. 只使用 Resolve 后的 `session.gap_reminder_days` 计算恢复提醒；未设置时为 7 天，配置已在进入 Store 前受 3650 天硬上限约束。
2. 提醒只进入安全恢复元数据，不改写历史消息正文或摘要链。

**验证：** `go test -json -count=1 ./internal/conversation -run TestGapReminderUsesConfiguredDays` 通过。
**覆盖：** F22 / AC22。

### T3.15 非破坏读取旧 JSON 与 v1 JSONL

**文件：** `internal/conversation/migration.go`, `internal/conversation/migration_test.go`
**依赖：** T3.10
**步骤：**
1. 通过 legacy DTO 加载旧 JSON 和 v1 JSONL，读取后立即转换为安全内存状态。
2. 加载阶段不写入；只有后续首次成功保存才生成 v2 snapshot，失败保留原始数据。

**验证：** `go test -json -count=1 ./internal/conversation -run TestLegacyLoadDoesNotModifySource` 通过。
**覆盖：** F19、F20 / AC19、AC20、AC35。

### T3.16 定义 C10 LegacyArtifactImporter 边界

**文件：** `internal/conversation/migration.go`, `internal/conversation/migration_test.go`
**依赖：** T3.15、T1.25
**步骤：**
1. 按 C10 定义 `LegacyArtifactImporter`，Migration 只持有该窄接口，不持有完整 Artifact Store。
2. `legacyPath` 不得进入 v2 DTO、诊断、错误、模型输入、memory 或 TUI。

**验证：** `go test -json -count=1 ./internal/conversation -run TestLegacyPathNeverCrossesMigrationBoundary` 通过。
**覆盖：** F7、F10 / AC7、AC10、AC35。

### T3.17 实现旧 ExternalPath 导入

**文件：** `internal/conversation/migration.go`, `internal/conversation/migration_test.go`, `internal/testutil/artifact.go`
**依赖：** T3.16、T1.26、T1.27
**步骤：**
1. Importer 通过旧数据 Root 的 safefs 句柄验证并流式复制；Artifact Commit 成功后才产生 opaque Ref。
2. 导入失败时保留旧记录，返回 `Available=false` 占位和安全诊断；不删除、移动或覆盖旧文件。

**验证：** `go test -json -count=1 ./internal/conversation -run TestLegacyExternalPathImportIsNonDestructive` 通过。
**覆盖：** F7、F10、F19、F20 / AC7、AC10、AC19、AC20、AC35。

### T3.18 原子切换 Conversation Store API

**文件：** `internal/conversation/store.go`, `internal/app/deps.go`, `internal/orchestrator/chat.go`, `internal/testutil/conversation_store.go`, `internal/repoaudit/conversation_refs_test.go`
**依赖：** T3.8、T3.11–T3.17
**步骤：**
1. 所有调用方一次改用 `ListResult`、`LoadResult`、`SaveResult` 和 `Maintain`，不得保留真假两套 Store 入口。
2. 删除以消息数量判断保存进度和吞掉列表错误的入口；兼容读取只能留在 Migration。

**验证：** `go test -json -count=1 ./internal/conversation ./internal/app ./internal/orchestrator` 与 `go test -json -count=1 ./internal/repoaudit -run TestLegacyStoreMethodsHaveNoProductionReference` 通过；required AST/type 审计 test 明确 pass。
**覆盖：** F19–F22 / AC19–AC22、AC34、AC35。

### T3.19 封闭 ContextManager 安全结果契约（已完成，2026-08-03）

**文件：** `internal/contextmgr/manager.go`, `internal/contextmgr/manager_test.go`, `internal/tool/capture.go`, `internal/tool/capture_test.go`, `internal/tool/result.go`, `internal/tool/result_factory.go`, `internal/tool/result_factory_test.go`, `internal/tool/executor_test.go`, `internal/repoaudit/context_result_refs_test.go`, `cmd/xagent/main.go`, `internal/app/hook_test.go`, `internal/app/commands_test.go`, `internal/orchestrator/hook_test.go`
**依赖：** T3.18、T2.17、T2.18
**步骤：**
1. 以 C18 的 `ManagerOptions`、`New(provider, options) (*Manager, error)`、`ProjectToolResult(tool.Result)` 和 `UpdateUsage(..., provider.Usage) error` 精确替换旧入口；拒绝 nil RuntimeRedactor、非法 Context 数值、越出 `1..1 MiB` 的 InlineOutputBytes、启用时 nil Provider 及非法 usage，显式禁用时只放宽 Provider。机械迁移现有构造调用点并保持编译，不创建 Store、Capture factory 或第二组装根。
2. 删除 `dataDir`、`context_blobs`、路径/文件/SHA/自建 Ref、payload codec 与候选扫描能力；ContextManager 不导入 artifact/OS 文件能力，不取得 Store、Reader、Writer、真实路径或 payload。
3. 将 `CapturedBytes int64` 沿 `CaptureResult → ResultFactoryInput → OutputMeta` 单向传递，并在 tool 包内封闭 Result/ResultFactory/OutputMeta 的四安全视图构造契约。ResultFactory 与 ProjectToolResult 落实 C18 的 8-byte ASCII/UTF-8、Ref identity/Bytes/CreatedAt/Available/Complete、截断与原因闭集；零字节 after-start write failure 和 Commit failure 不发布不一致 CaptureResult 或 unavailable Ref，后续 caller 只能构造无输出的有界 SafeError 结果。
4. `ProjectToolResult` 对同一未复制参数直接调用四个安全访问器各恰好一次，原子校验并发布 ToolResultProjection；禁止读取公开兼容字段、marshal/reflect/helper 绕行、恢复 preview/path 或部分扇出。旧 Context 三阈值的两组相反极值不得改变同一投影、Context 操作或摘要。
5. `TestContextManagerProjectionMatrix` 的 canonical subtest 闭集固定为 `no_ref_ascii_below_cap`、`no_ref_ascii_at_cap`、`no_ref_ascii_over_cap_rejected`、`no_ref_utf8_at_cap`、`no_ref_utf8_over_cap_rejected`、`no_ref_truncated_rejected`、`ref_complete_inline_limit`、`ref_incomplete_capture_hard_limit`、`ref_incomplete_artifact_hard_limit`、`ref_incomplete_write_failure`、`ref_incomplete_canceled`、`ref_id_mismatch_rejected`、`ref_bytes_mismatch_rejected`、`ref_created_at_mismatch_rejected`、`ref_available_mismatch_rejected`、`ref_complete_mismatch_rejected`、`truncated_mismatch_rejected`、`reason_mismatch_rejected`、`preview_over_cap_rejected`、`unavailable_ref_rejected`、`cancelled_before_start_rejected`、`zero_byte_write_failure_synthetic_error`、`commit_failure_synthetic_error`、`legacy_threshold_extremes_equal`；测试比较 actual seen 与该闭集完全相等。repoaudit 另以独立根证明 capability shape、唯一 Result 参数入口、四个 direct accessor 调用及 panic-on-open canary 的 open count 恒为零。八个根及上述完整 subtest paths 全部进入 T5.25b 冻结的 CommandManifest 与 T5.26 required-test gate。

**验证：** ① `go test -json -count=1 ./internal/contextmgr -run '^TestContextManagerNeverReadsRawArtifact$'`；② `go test -json -count=1 ./internal/contextmgr ./internal/tool ./internal/repoaudit -run '^(TestContextManagerConstructorIsClosed|TestContextManagerProjectionMatrix|TestContextManagerUsageSnapshotValidation|TestResultFactoryProducesFourSafeViewsOnce|TestResultFactoryCapturedArtifactMatrix|TestCaptureTerminalFailuresPublishNoUsableRef|TestContextResultRefsProductionBoundary)$'`；两条命令均通过。required 映射精确为 `xagent/internal/contextmgr` 的 `TestContextManagerNeverReadsRawArtifact`、`TestContextManagerConstructorIsClosed`、`TestContextManagerProjectionMatrix`、`TestContextManagerUsageSnapshotValidation`，`xagent/internal/tool` 的 `TestResultFactoryProducesFourSafeViewsOnce`、`TestResultFactoryCapturedArtifactMatrix`、`TestCaptureTerminalFailuresPublishNoUsableRef`，以及 `xagent/internal/repoaudit` 的 `TestContextResultRefsProductionBoundary`，八项均 `ExpectedRuns=1`；24 个 canonical Projection `Root/Subtest` paths 也各恰好一次 run+pass。regex 命中的未批准 `(Package, Test)` 或未批准 descendant subtest 计为 Excess，缺失、重复、零匹配、错包同名顶替或只有 package pass 均失败。
**覆盖：** F7、F10、F18、F20 / AC7、AC10、AC18、AC20。
**完成记录（2026-08-03）：** 两条批准验证均 exit 0，8 个 required 根与 24 个 canonical Projection 子测试各恰好一次 run+pass，三个 package 均 pass 且无 fail/skip；`go test -json -count=1 ./cmd/xagent ./internal/app ./internal/orchestrator` 亦 exit 0、三包全 pass。`internal/orchestrator/agent_loop.go` 的 `UpdateUsage` 指针参数改为值快照并处理返回错误，归类为步骤 1 精确 API 替换不可缺少的机械调用点迁移，不扩展 Orchestrator 业务范围。

### T3.19a 建立 Orchestrator 单点安全投影与临时 ModelContent 槽（已完成，2026-08-03）

**文件：** `internal/orchestrator/execution_state.go`, `internal/orchestrator/chat.go`, `internal/orchestrator/agent_loop.go`, `internal/orchestrator/tool_batches.go`, `internal/orchestrator/stream_collector.go`, `internal/orchestrator/skill_runtime.go`, `internal/orchestrator/chat_test.go`, `internal/orchestrator/hook_test.go`, `internal/orchestrator/cancellation_test.go`, `internal/conversation/message.go`, `internal/conversation/jsonl_record.go`, `internal/conversation/jsonl_record_test.go`, `internal/conversation/recovery_test.go`, `internal/events/events.go`, `internal/events/events_test.go`, `internal/hook/result_adapter.go`, `internal/hook/result_adapter_test.go`, `internal/tui/messages.go`, `internal/tui/messages_test.go`, `internal/tool/capture.go`, `internal/tool/executor.go`, `internal/tool/bash.go`, `internal/tool/read.go`, `internal/tool/grep.go`, `internal/tool/glob.go`, `internal/tool/write.go`, `internal/tool/edit.go`, `internal/tool/registry.go`, `internal/tool/load_skill.go`, `internal/mcpclient/adapter.go`, `internal/mcpclient/manager.go`, `internal/mcpclient/adapter_test.go` 及上述生产文件的直接测试/fixture
**依赖：** T3.19、T2.17–T2.20、T2.34c、T2.67、T2.69
**步骤：**
1. 消费 T3.19 已封闭的 Result/ResultFactory/OutputMeta 四安全视图契约，并把 repo-wide safe candidate 生产者收敛到该唯一构造边界；`skill_runtime.go`、`load_skill.go`、内置工具、MCP、Hook synthetic result、拒绝、未知工具、参数错误、超时和取消后结果只提交有界 factory input，不得自行 New factory、直接 Result literal 或调用 Success/Failure。CancelledBeforeStart 仍无 Result；Assembly 尚未注入的旧入口只能走显式隔离的迁移 adapter，禁止进入 ProjectToolResult、写 Conversation v2、伪造 Ref 或创建第二 factory，删除截止 T4.29a。
2. 每个字节流 producer 只接收等价于 `func(context.Context, artifact.Metadata) (*tool.Capture, error)` 的未导出窄函数依赖；闭包捕获唯一 Store、resolved inline/capture limits 与 hard cap，每次调用新建只含 Bytes 维度的 operation-local Counter 和新的 Capture/Writer。Read/Grep/Glob/Bash/MCP 在读取或接收用户输出第一字节前调用闭包一次，所有接受字节只写该 Capture，恰好执行一次 Finish 并只产生一次 Abort 或 Commit；producer 不得取得 Store/Reader/path/其他 Capture。T4.25c/d 前只建立 safe candidate、迁移隔离与测试注入，不宣称旧 main 已获得首字节 Capture，不解析 root、不创建生产 Store 或通用 CaptureFactory registry；纯 synthetic Hook adapter 只接收 ResultFactory。
3. Orchestrator 对每个可发布 Result 先且仅调用一次 `ProjectToolResult`，成功后单点扇出：UserView→Event/TUI/Hook，PersistedContent+安全状态+opaque Ref→Conversation/JSONL，ModelContent→当前 run 的 Provider 临时覆盖；投影失败不得产生部分事件、消息、保存或 Provider 副作用。生产 repo 中四访问器的唯一调用点收敛为 ProjectToolResult。
4. 单次 `executionState` 私有槽按 conversation ID、request generation、iteration、run 内单调 ordinal 和 append 后 message index 定位，call ID 只校验；单项/累计分别受 `session.max_record_bytes`/`session.max_session_bytes`，checked-add 溢出、超限或不唯一映射在副作用前失败，槽不得持有 Result/UserView/Store/path/payload，也不得升级为 Orchestrator 全局 map。
5. Provider mapper 只覆盖精确匹配的当前消息；Prepare/摘要后按唯一 survivor index 映射 rebase，淘汰即清除，不能唯一映射即 fail closed。规范化、计量并交给 StreamChat 后立即清槽，正常、Provider 错误、保存失败、取消、会话切换和所有同步出口兜底清除；JSONL、Memory、摘要、重启与历史请求不得出现 ModelContent preview。
6. `TestCaptureAbortCommitAndUTF8ThresholdMatrix` 的 canonical subtest 闭集固定为 `ascii_below_cap_abort`、`ascii_at_cap_abort`、`ascii_cap_plus_one_commit`、`utf8_at_cap_abort`、`utf8_over_cap_commit`、`capture_hard_limit_incomplete_commit`、`artifact_hard_limit_incomplete_commit`、`write_failure_after_bytes_incomplete_commit`、`canceled_after_bytes_incomplete_commit`、`zero_byte_write_failure_no_ref`、`commit_failure_no_ref`；`TestModelContentTemporarySlotLifecycleMatrix` 固定为 `success_clears_after_stream_handoff`、`provider_error_clears`、`cancel_clears`、`save_failure_clears`、`session_switch_clears`、`same_call_id_concurrent_runs_isolated`、`iteration_ordinal_reset_isolated`、`survivor_rebase_preserves_unique_mapping`、`ambiguous_rebase_fails_closed`、`record_limit_fails_before_side_effect`、`session_limit_fails_before_side_effect`、`checked_add_overflow_fails_before_side_effect`、`restart_uses_persisted_content_only`；两根均比较 actual seen 与闭集完全相等。全部 12 个根及完整 subtest paths 进入 T5.25b CommandManifest 与 T5.26 gate。
7. 按 C18c 在 `Capture` owner 内实现不完整终态标记：只允许 approved reason，first terminal error 胜出，Finish 后拒绝修改；Read/Grep/Glob/Bash 的外部 I/O、等待与取消失败统一通过该状态收敛，零接受字节及 Commit failure 仍规范化为无 Ref synthetic SafeError。
8. 按 C18c 让生产 MCP Manager 在 T4.25d 前只显式构造 legacy adapter；Manager 不获得 Capture/Store/Writer/Counter/Ref，不进入安全投影或 Conversation v2。`SafeResultProducer` 只定义在 `registry.go`，`tool.go` 与 `view.go` 不加入 T3.19a 文件面。required 测试只证明 candidate adapter 对已解码 DTO 的 fresh Capture/Writer/Counter 与终态，不宣称 wire 首字节采集。

**验证：** ① `go test -json -race -count=1 ./internal/tool ./internal/mcpclient -run '^(TestCaptureAbortCommitAndUTF8ThresholdMatrix|TestAllToolResultsRequireInjectedFactory|TestMCPAdapterUsesInjectedResultFactoryOnEveryOutcome)$'`；② `go test -json -race -count=1 ./internal/orchestrator ./internal/conversation ./internal/events ./internal/hook ./internal/tui -run '^(TestSyntheticResultsRequireInjectedResultFactory|TestToolResultProjectionFansOutOnce|TestSafeResultAccessorsHaveSingleProductionCallSite|TestModelContentTemporarySlotLifecycleMatrix|TestJSONLPersistsOnlyPersistedContentAndOpaqueRef|TestToolResultEventContainsOnlyUserView|TestToolResultHookAdapterContainsOnlyUserView|TestToolResultMessageUsesOnlyUserView|TestLegacyToolResultAdapterCannotProjectOrPersist)$'`；③ `go test -json -count=1 ./internal/tool ./internal/mcpclient`；三条命令均通过，并只读核对 `SafeResultProducer` 的生产声明仅在 `registry.go`、`view.go` 不传播 candidate 标志、Manager 的 legacy 选择只有 `buildToolCandidates` 一个调用点且没有 Capture/Store owner。required 映射精确为 `xagent/internal/tool` 的 `TestCaptureAbortCommitAndUTF8ThresholdMatrix`、`TestAllToolResultsRequireInjectedFactory`，`xagent/internal/mcpclient` 的 `TestMCPAdapterUsesInjectedResultFactoryOnEveryOutcome`，`xagent/internal/orchestrator` 的 `TestSyntheticResultsRequireInjectedResultFactory`、`TestToolResultProjectionFansOutOnce`、`TestSafeResultAccessorsHaveSingleProductionCallSite`、`TestModelContentTemporarySlotLifecycleMatrix`、`TestLegacyToolResultAdapterCannotProjectOrPersist`，`xagent/internal/conversation` 的 `TestJSONLPersistsOnlyPersistedContentAndOpaqueRef`，`xagent/internal/events` 的 `TestToolResultEventContainsOnlyUserView`，`xagent/internal/hook` 的 `TestToolResultHookAdapterContainsOnlyUserView`，以及 `xagent/internal/tui` 的 `TestToolResultMessageUsesOnlyUserView`，十二项均 `ExpectedRuns=1`；11 个 Capture 与 13 个 Slot canonical `Root/Subtest` paths 也各恰好一次 run+pass。regex 命中的未批准 tuple 或 descendant subtest 计为 Excess，缺失、重复、零匹配、错包同名顶替或只有 package pass 均失败。
**覆盖：** F7、F10、F20 / AC7、AC10、AC20。
**完成记录（2026-08-03）：** 两条批准的 `-race` 精确验证均 exit 0，12 个 required 根、11 个 Capture canonical 子测试与 13 个 ModelContent Slot canonical 子测试各恰好一次 run+pass，且无 fail、skip 或 excess。`go test -json -count=1 ./internal/tool ./internal/mcpclient ./internal/orchestrator ./internal/conversation ./internal/events ./internal/hook ./internal/tui ./cmd/xagent ./internal/app ./internal/skill` exit 0，十个相关包完整回归全部 pass；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。生产静态边界核对确认四个 Result 安全访问器仅由 `internal/contextmgr/manager.go` 的唯一 `ProjectToolResult` 读取，生产 `ProjectToolResult` 调用仅在 `internal/orchestrator/chat.go`；`SafeResultProducer` 仅声明在 `internal/tool/registry.go`，`internal/tool/view.go` 不传播 candidate 标志，`buildToolCandidates` 仅有一个 legacy constructor 调用且 MCP Manager 不持有 Capture/Store/Writer/Counter/Ref。MCP 本任务证据只覆盖 candidate adapter 对已解码 DTO 的 fresh Capture/Writer/Counter，transport/wire 首字节链仍由 T4.25d 完成。

### T3.20 收敛 Prompt、SessionContext 与 Memory（已完成，2026-08-03）

**文件：** `internal/sessionctx/manager.go`, `internal/sessionctx/manager_test.go`, `internal/prompt/dynamic.go`, `internal/prompt/dynamic_test.go`, `internal/memory/manager.go`, `internal/memory/update.go`, `internal/memory/write.go`, `internal/memory/manager_test.go`, `internal/memory/update_test.go`, `internal/memory/write_test.go`, `internal/skill/activity.go`, `internal/skill/activity_test.go`, `internal/skill/prompt.go`
**依赖：** T3.19a、T1.1、T1.5
**步骤：**
1. Prompt 和 SessionContext 只接受 `SafeText`，所有展开内容在跨边界前完成预算和脱敏。
2. Memory 只保存脱敏、有界内容和 opaque Ref，不保存 raw buffer、`legacyPath` 或真实 artifact 路径。
3. Skill Activity 与 Prompt 只发布不可变安全快照并提供显式 Clear；它们不感知 App generation 或会话切换，具体 preserve/clear 边界由 T4.2 负责。

**验证：** `go test -json -count=1 ./internal/sessionctx ./internal/prompt ./internal/memory ./internal/skill -run '^(TestSessionContextAndPromptAcceptOnlySafeText|TestMemoryStoresOnlySafeBoundedContentAndOpaqueRefs|TestActivitySnapshotIsImmutableAndClearIsExplicit|TestSkillPromptUsesOnlySafeSnapshot)$'` 通过，四个 required test 均 pass 且无 skip。
**覆盖：** F7、F10、F26 / AC7、AC10、AC26。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`xagent/internal/sessionctx` 的 `TestSessionContextAndPromptAcceptOnlySafeText`、`xagent/internal/memory` 的 `TestMemoryStoresOnlySafeBoundedContentAndOpaqueRefs`，以及 `xagent/internal/skill` 的 `TestActivitySnapshotIsImmutableAndClearIsExplicit`、`TestSkillPromptUsesOnlySafeSnapshot` 四个 required 根各恰好一次 run+pass、无 fail/skip；同一命令增加 `-race` 后亦 exit 0。SessionContext 对指令、Memory index 和固定边界统一完成 RuntimeRedactor 脱敏及 UTF-8 byte 上界，Prompt 的 `SafeDynamicRequest` 只允许 `SafeText` 动态内容并在完整展开发布前计量；Memory Manager 与直接 Writer 路径均在落盘前再次运行 RuntimeRedactor 和 note/index 上界，typed `artifact.Ref` 只投影 opaque identity/metadata，不取得 Store、Reader、Writer、payload、`legacyPath` 或 artifact path；Skill Activity 发布深复制的安全 Prompt 快照，Prompt 不读取 raw Active view，状态只由显式 `Clear` 清理且不感知 App generation/会话切换。`go test -json -count=1 ./internal/sessionctx ./internal/prompt ./internal/memory ./internal/skill ./internal/orchestrator ./internal/app ./cmd/xagent` exit 0，七包完整回归全部 pass；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.20a 建立不可替换 RequestBudgeter 类型（已完成，2026-08-03）

**文件：** `internal/contextmgr/manager.go`, `internal/contextmgr/request_budgeter_test.go`
**依赖：** T3.20
**步骤：**
1. 定义 `RequestMeasure{Bytes, PlanningTokens int64}` 与带私有计量版本/validity marker 的 concrete `RequestBudgeter`；唯一无参 `NewRequestBudgeter()` 返回有效值，不提供 interface、函数 counter、options、第二构造器或可变注册入口。
2. 实现 checked add/multiply 与 `Bytes/4 + bool(Bytes%4)` 的无溢出 ceiling helper；拒绝零值 Budgeter、负 measure 和所有算术溢出。

**验证：** `go test -json -count=1 ./internal/contextmgr -run '^(TestRequestBudgeterIsClosedConcreteValue|TestPlanningTokensAreCeilBytesOverFour|TestRequestMeasureArithmeticFailsClosed)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F9、F17、F26 / AC9、AC17、AC26。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestRequestBudgeterIsClosedConcreteValue`、`TestPlanningTokensAreCeilBytesOverFour`、`TestRequestMeasureArithmeticFailsClosed` 三个 required 根各恰好一次 run+pass、无 fail/skip；同一命令增加 `-race` 后亦 exit 0。`RequestBudgeter` 是只含私有 measurement version 与 validity marker 的 immutable concrete value，零值、负 measure、负乘数及 Bytes/PlanningTokens 任一加法或乘法溢出均返回零 measure 和错误；`PlanningTokens` 对 0、cap 边界及 `math.MaxInt64` 均按无溢出的 `Bytes/4 + bool(Bytes%4)` 计算。AST/shape 审计确认生产代码只有一个 `RequestBudgeter` 类型、唯一无参 `NewRequestBudgeter` 和构造器内唯一有效 composite literal，不存在 Options、Factory、Registry、interface、函数 counter、注册入口、第二构造器或指针可变方法。`go test -json -count=1 ./internal/contextmgr ./internal/sessionctx ./internal/orchestrator ./internal/app ./cmd/xagent` exit 0，五包完整回归全部 pass；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.20b 实现 JSON byte 上界原语（已完成，2026-08-03）

**文件：** `internal/contextmgr/manager.go`, `internal/contextmgr/request_budgeter_test.go`
**依赖：** T3.20a
**步骤：**
1. 逐字段流式计量 string、枚举、布尔、数字、slice/map/schema 及结构键/分隔符；每个原始 string byte 按 JSON 最坏 6-byte 转义计入，不用 rune count。
2. 不生成与输入大小成比例的编码副本；开始前、字段间、每处理至多 64 KiB 原始输入或 1024 个 schema 节点检查 `ctx`，取消返回 `ctx.Err()` 且不发布部分结果。

**验证：** `go test -json -count=1 ./internal/contextmgr -run '^(TestRequestByteBoundUsesWorstCaseJSONEncoding|TestRequestBytePrimitivesDoNotAllocateWholeInputCopy|TestRequestByteTraversalCancelsWithinFixedChunks)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F9、F17 / AC9、AC17、AC36。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestRequestByteBoundUsesWorstCaseJSONEncoding`、`TestRequestBytePrimitivesDoNotAllocateWholeInputCopy`、`TestRequestByteTraversalCancelsWithinFixedChunks` 三个 required 根各恰好一次 run+pass、无 fail/skip；同一命令增加 `-race` 后亦 exit 0。JSON byte counter 对原始 string byte 使用最坏 6-byte 转义上界，流式计入 nil、布尔、整数、有限浮点、`json.Number`、slice、map、结构化 schema、Raw schema 及所有键/分隔 framing；非法数字、NaN/Inf、无效 Raw JSON、未知类型、溢出与无效 Budgeter 均 fail closed。实现不调用整份 `json.Marshal`、不构造与输入大小成比例的编码副本，并在开始/发布前、字段间、每至多 64 KiB 原始输入及 1024 个 schema 节点检查 `ctx`，取消只返回零 measure 与 `ctx.Err()`。T3.20a 三项边界回归与 `internal/contextmgr`、`internal/sessionctx`、`internal/orchestrator`、`internal/app`、`cmd/xagent` 五包完整回归均 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.20c 计量请求基础字段（已完成，2026-08-03）

**文件：** `internal/contextmgr/manager.go`, `internal/contextmgr/request_budgeter_test.go`
**依赖：** T3.20b
**步骤：**
1. `MeasureRequest` 计入 stable/dynamic system blocks、Skill SOP、effective model、当前输入与已选消息、thinking、cache 及 request/message framing。
2. 空值、nil/empty 规范化和字段顺序固定为版本化 canonical encoding；出现未支持字段形态时 fail closed。

**验证：** `go test -json -count=1 ./internal/contextmgr -run '^(TestMeasureRequestCoversSystemModelInputHistoryThinkingAndCache|TestMeasureRequestCanonicalizesNilEmptyAndFieldOrder|TestMeasureRequestRejectsUnknownShape)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F17、F26 / AC7、AC17、AC26。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestMeasureRequestCoversSystemModelInputHistoryThinkingAndCache`、`TestMeasureRequestCanonicalizesNilEmptyAndFieldOrder`、`TestMeasureRequestRejectsUnknownShape` 三个 required 根各恰好一次 run+pass、无 fail/skip；同一命令增加 `-race` 后亦 exit 0。`MeasureRequest` 按固定 `version → model → system → messages → thinking → cache` canonical 顺序流式计量，覆盖 effective model、ordered 或 stable/dynamic system blocks、Skill SOP 内容、当前输入与已选基础消息、thinking/cache 值及全部对象/数组 framing；ordered `System` 按 Provider 语义覆盖 legacy stable/dynamic，model、block name 与 cache breakpoint trim 后计量，空 block 以及 nil/empty slice 归一为相同结果。未知消息 role、基础 role 携带工具字段、负 thinking budget、无效 Budgeter、nil/canceled context 及本层尚未支持的工具形态均返回零 measure 与错误；Observer 不属于 payload，不进入 byte 计量。T3.20a–c 九项联合边界测试、T3.20c race，以及 `internal/contextmgr`、`internal/sessionctx`、`internal/orchestrator`、`internal/app`、`cmd/xagent` 五包完整回归均 exit 0；canonical 路径无整份 Marshal/Builder/Buffer，`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.20d 计量工具与协议 framing（已完成，2026-08-03）

**文件：** `internal/contextmgr/manager.go`, `internal/contextmgr/request_budgeter_test.go`
**依赖：** T3.20c
**步骤：**
1. 扩展 `MeasureRequest`，计入完整 tool schema、tool call/result 安全元数据、opaque artifact Ref 与 adapter framing；禁止打开 artifact 或读取持久化内部字段。
2. 对嵌套 schema、无效 UTF-8、组合字符和 tool 链使用相同 byte 原语并保持 overflow/cancel 语义。

**验证：** `go test -json -count=1 ./internal/contextmgr -run '^(TestMeasureRequestCoversToolSchemaCallsResultsAndFraming|TestMeasureRequestNeverReadsArtifactPayload|TestMeasureRequestHandlesNestedSchemaAndInvalidUTF8)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F9、F10、F17 / AC7、AC9、AC10、AC17。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestMeasureRequestCoversToolSchemaCallsResultsAndFraming`、`TestMeasureRequestNeverReadsArtifactPayload`、`TestMeasureRequestHandlesNestedSchemaAndInvalidUTF8` 三个 required 根各恰好一次 run+pass、无 fail/skip；同一命令增加 `-race` 后亦 exit 0。`MeasureRequest` 现按显式 `Tools` 优先、否则读取 detached `ToolDefs` 定义的既有 Provider 归一规则，流式计入 tool name/description、结构化或 Raw 完整 schema、tool-call ID/name/arguments、tool-result content/status、安全 ModelContent 中的 opaque artifact Ref、OpenAI function/tool-call framing、Anthropic tool-use/result/cache framing 与固定 request framing。tool role 的缺失 identity、冲突字段、未知 status、无效 Raw schema 均返回零 measure 与错误；嵌套 schema、CJK、emoji、组合字符和无效 UTF-8 沿用同一原始 byte 上界与取消语义。`MeasureRequest` 签名及 canonical/schema helper 调用图不接收 Store/Reader/path，静态审计无 `OpenForUser`、Begin/read payload 或整份 Marshal。T3.20a–d 十二项联合边界测试、五包完整回归，以及 `internal/provider`、`internal/tool` 完整回归均 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.20e 冻结完整请求 byte oracle 与 golden（已完成，2026-08-03）

**文件：** `internal/contextmgr/request_budgeter_test.go`, `internal/contextmgr/request_budgeter_oracle_test.go`
**依赖：** T3.20d、T1.31–T1.33、T2.40、T2.43
**步骤：**
1. 用受 netpolicy 控制的本机 recorder 驱动真实 Anthropic/OpenAI request mapper，捕获最终规范化 body，并逐例断言 `measured.Bytes >= serialized payload bytes`；测试使用固定 canary/fake key、禁止公网，Provider 生产包不得反向依赖 Context Manager，也不得复制 adapter serializer。
2. 为 ASCII、CJK、emoji、组合字符、无效 UTF-8、system/SOP、任意非空 model override、tool/schema/call/result/cache/framing 固定 deterministic golden；request 字段或 adapter 规范化变化必须与 oracle/golden 同步。

**验证：** `go test -json -count=1 ./internal/contextmgr -run '^(TestRequestMeasureBoundsAnthropicAndOpenAIJSON|TestRequestMeasureGoldenCoversEveryApprovedField|TestArbitraryNonEmptyModelNamesDoNotChangeMeasurementRules)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F8、F17、F26 / AC7、AC8、AC17、AC26、AC36。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestRequestMeasureBoundsAnthropicAndOpenAIJSON`、`TestRequestMeasureGoldenCoversEveryApprovedField`、`TestArbitraryNonEmptyModelNamesDoNotChangeMeasurementRules` 三个 required 根各恰好一次 run+pass、无 fail/skip；同一命令增加 `-race` 后亦 exit 0。受 `netpolicy` 控制的 `httptest` loopback recorder 使用固定 fake key，经真实 `provider.NewWithOptions` 与 `StreamChat` 分别驱动 Anthropic/OpenAI mapper，捕获的最终 body 均为有效 JSON，effective model 与请求 override 一致，且 `RequestMeasure.Bytes` 均不低估实际序列化字节；测试未复制 adapter serializer，Provider 生产包未反向依赖 Context Manager，也未访问公网。deterministic golden 已冻结 empty、ASCII、CJK、emoji、组合字符、无效 UTF-8 与覆盖 system/SOP、model、tool/schema/call/result、opaque artifact Ref、thinking、cache、双协议 framing 的完整请求；任意 trim 后非空 model override 统一使用原始 byte 规则。`internal/contextmgr`、`internal/provider`、`internal/tool`、`internal/sessionctx`、`internal/orchestrator`、`internal/app`、`cmd/xagent` 七包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.20f 建立可相加的完整 turn measure（已完成，2026-08-03）

**文件：** `internal/contextmgr/manager.go`, `internal/contextmgr/request_budgeter_test.go`
**依赖：** T3.20e
**步骤：**
1. `MeasureConversationTurn` 只读取 index range 中最终可发送的 SafeText、工具元数据、opaque Ref 与最坏插入 framing，不调用 `ContextMessages`、不复制整段会话；结果是可与 base overflow-safe 相加的上界分量。
2. 证明最终完整请求的 Bytes/PlanningTokens 分别不高于 base＋selected-turn 累计值；最终较小允许，最终较大、取消或负值均失败且不发布部分 measure。

**验证：** `go test -json -count=1 ./internal/contextmgr -run '^(TestConversationTurnMeasureIsAdditiveUpperBound|TestFinalMeasureDoesNotExceedIncrementalBound|TestTurnMeasureCancellationReturnsNoPartialResult)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F9、F17、F26 / AC7、AC9、AC17、AC26、AC36。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestConversationTurnMeasureIsAdditiveUpperBound`、`TestFinalMeasureDoesNotExceedIncrementalBound`、`TestTurnMeasureCancellationReturnsNoPartialResult` 三个 required 根各恰好一次 run+pass、无 fail/skip；T3.20a–f 共 18 个 required 根的联合命令及同一命令增加 `-race` 后亦 exit 0。`MeasureConversationTurn` 直接遍历调用方提供的只读 `[]conversation.Message` index range，只将 user、assistant、tool-call、tool-result、context-summary、context-boundary 的最终安全投影映射到与 `MeasureRequest` 共用的 canonical message counter，thinking 不进入 Provider 上界；工具 identity、arguments、result、status 与安全结果文本中的 opaque artifact Ref 均被计入，每条可发送消息预留一个最坏插入分隔符，因此 base 与多个 turn 的 Bytes/PlanningTokens 可分别 overflow-safe 相加并覆盖最终完整请求，空 base 的最终复量允许因共享 framing 更小。未知 role、缺失或冲突 tool state、无效 status、负 measure、零值 Budgeter、nil/pre-canceled context 和长字段中途取消均 fail closed 且只返回零 measure；额外未计量消息会使最终复量超过累计 bound，从而可被发布前比较拒绝。实现不调用 `ContextMessages`，不 clone 整段会话，不读取 Artifact/Store/Reader/path/payload，并在开始、消息间、字段间及每至多 64 KiB 原始文本检查取消。`internal/contextmgr`、`internal/provider`、`internal/tool`、`internal/sessionctx`、`internal/orchestrator`、`internal/app`、`cmd/xagent` 七包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.21 原子切换 Orchestrator 授权顺序（已完成，2026-08-03）

**文件：** `internal/orchestrator/tool_scheduler.go`, `internal/orchestrator/agent_loop.go`, `internal/orchestrator/tool_batches.go`, `internal/orchestrator/authorization_test.go`, `internal/permission/authorizer.go`
**依赖：** T3.19a、T2.70、T2.15、T2.19、T2.20、T2.27、T2.34c、T2.54、T2.67
**步骤：**
1. 对内置和 MCP 工具统一执行 `Registry/schema → CheckHard/health → BeforeTool → 普通规则/确认 → TicketIssuer.Issue → Executor`。
2. Hook 拒绝时不得弹确认或签发 Ticket；任何参数变化都作废已有身份并从 schema 校验重新开始。

**验证：** `go test -json -race -count=1 ./internal/orchestrator -run TestAuthorizationStageOrder` 通过。
**覆盖：** F3–F6、F24 / AC3–AC6、AC24。
**完成记录（2026-08-03）：** 批准的精确 race 命令 exit 0，`TestAuthorizationStageOrder` 根及其内置成功、MCP 确认、schema 拒绝、hard/health 拒绝、degraded 只读 Hook 拒绝和参数/身份漂移后代全部 run+pass、无 fail/skip。Agent Loop 现只经 `scheduleToolCallsWithRegistryAndRef` 进入同一授权调度入口；普通内置与 MCP 调用统一按 `Registry/schema → Executor/Authorizer health 与 CheckHard → BeforeTool → 普通规则/可选确认 → TicketIssuer.Issue → Executor` 执行。Executor 或 Authorizer 不可用、schema 或 hard constraint 拒绝均不会进入 Hook；Hook 拒绝不会产生确认、Ticket 或工具执行。degraded 只读策略不再由 `CheckHard` 提前签票，而在 Hook 通过后的普通授权阶段签发；危险调用仍在 Hook 前 fail closed。Executor 启动边界重新走 Registry/schema 并重算身份，schema-invalid 与 schema-valid 参数漂移均不能消费旧 Ticket 或进入工具实现。`internal/orchestrator`、`internal/permission`、`internal/tool`、`internal/mcpclient` 四包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.22 定义工具调度资格（已完成，2026-08-03）

**文件：** `internal/orchestrator/tool_scheduler.go`, `internal/orchestrator/tool_scheduler_test.go`, `internal/orchestrator/tool_batches.go`
**依赖：** T3.21、T2.16
**步骤：**
1. 只有 `ReadOnly && ConcurrentSafe` 的调用进入并发批次，其他调用形成顺序屏障。
2. Risk 只参与授权，不能单独推导并发资格；远程 annotation 不能放宽本地标记。

**验证：** `go test -json -count=1 ./internal/orchestrator -run TestSchedulerSeparatesRiskFromConcurrency` 通过。
**覆盖：** F23 / AC23。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestSchedulerSeparatesRiskFromConcurrency` 根恰好一次 run+pass、无 fail/skip。`makeToolBatches` 已迁移到统一 `tool_scheduler.go`，只读取 Registry 冻结 Descriptor 的 `ExecutionPolicy.AllowsConcurrentExecution()`；只有 `ReadOnly && ConcurrentSafe` 的相邻调用合并为并发批次，任一标记缺失、未知工具或其他调用均各自形成顺序屏障并保留原始 index。测试矩阵证明零策略的 `RiskSafe` 不能获得并发资格，显式双标记的 `RiskDangerous` 可以进入并发批次，Risk 不参与调度推导；远程正向 `readOnlyHint/idempotentHint` 不能放宽零本地策略，远程否定 hint 会收紧本地双标记，本地双标记在非收紧正向 hint 下保持有效。原批次顺序与结果顺序兼容测试通过；`internal/orchestrator`、`internal/tool`、`internal/mcpclient` 三包完整回归 exit 0；调度器静态核对无 `Risk()` 判定，`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.23 实现受控并发与有序槽位

**文件：** `internal/orchestrator/tool_scheduler.go`, `internal/orchestrator/tool_scheduler_test.go`
**依赖：** T3.22
**步骤：**
1. semaphore 限制 worker 数，每个 worker 只写自己的预分配序号槽位。
2. 完成事件允许乱序；join 后结果、Conversation 回灌和下一次 Provider 输入均按原调用顺序提交。

**验证：** `go test -json -race -count=1 ./internal/orchestrator -run TestConcurrentToolsOverlapAndCommitInCallOrder` 通过。
**覆盖：** F23 / AC23。
**完成记录（2026-08-03）：** 批准的精确 race 命令 exit 0，`TestConcurrentToolsOverlapAndCommitInCallOrder` 根及“完成事件可乱序但 join 槽位有序”“共享 semaphore 限制 worker 且槽位保留 ordinal”两个子测试均 run+pass、无 fail/skip，race detector 无报告。并发批次在顺序完成授权准备后复制到固定长度槽位，只为尚无结果的调用创建最多 4 个固定 worker；worker 按槽位 index 取任务，并在实际执行前获取进程共享 semaphore，完成后只写回该 index，全部 join 后才由批次循环按原调用顺序返回。实测两个工具同时活跃且第二个可先产生完成事件，六个调用峰值活跃数严格为 4，最终 `ToolExecution.Index`、Call ID 和结果顺序仍逐项匹配原调用序列；后续 `publishToolExecutions`、Conversation 回灌及下一次 Provider 请求继续顺序消费该已排序 slice。`internal/orchestrator`、`internal/tool`、`internal/mcpclient` 三包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.24 固定工具取消状态（已完成，2026-08-03）

**文件：** `internal/orchestrator/execution_state.go`, `internal/orchestrator/cancellation_test.go`, `internal/orchestrator/tool_scheduler.go`, `internal/orchestrator/tool_batches.go`, `internal/orchestrator/chat.go`
**依赖：** T3.23、T2.20
**步骤：**
1. `ToolPending` 到真正启动边界前取消时记录 `CancelledBeforeStart` 且保持 `Result=nil`。
2. `Rejected`、`CancelledAfterStart` 和 `Completed` 只记录实际安全结果；未启动调用不得生成 tool-role 消息。

**验证：** `go test -json -race -count=1 ./internal/orchestrator -run TestCancellationBeforeStartProducesNoToolRoleMessage` 通过。
**覆盖：** F24 / AC24。
**完成记录（2026-08-03）：** 批准的精确 race 命令 exit 0，`TestCancellationBeforeStartProducesNoToolRoleMessage` 根及确认等待、semaphore 排队、启动事件 barrier 三个子测试均 run+pass、无 fail/skip，race detector 无报告。`ToolExecution` 现显式记录固定六态；`ToolPending` 后在确认、worker queue 或真正启动前取消均统一收敛为 `CancelledBeforeStart`，清空 Result、Projection、Duration 并保持 `HasResult=false`，不消费为已执行结果。实测三处取消均返回 cancellation stop reason，未产生 ToolSuccess/ToolError/ToolDenied、tool-call/tool-result Conversation 消息或文件副作用。真实拒绝结果统一记录 `Rejected`，启动后的有效 Result 按其安全状态记录 `Rejected`、`CancelledAfterStart` 或 `Completed`；发布边界在任何副作用前拒绝“启动前取消却携带 Result”“终态缺 Result”及执行状态与安全 Result 状态不一致的记录。`internal/orchestrator`、`internal/tool`、`internal/conversation`、`internal/mcpclient` 四包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.24a 建立不可绕过 SkillHistoryPolicy（已完成，2026-08-03）

**文件：** `internal/orchestrator/independent.go`, `internal/orchestrator/skill_history_policy_test.go`
**依赖：** T3.24、T3.20f、T2.31a
**步骤：**
1. 定义字段与 validity marker 均私有、只能由 `NewSkillHistoryPolicy(maxSessionBytes, modelWindowTokens, autoMarginTokens)` 产生的 value；构造器内部固定 maxTurns=1000。
2. 构造器独立验证 session bytes 为 1 B–1 GiB、model window 为 2–10000000、auto margin 为 1–1000000 且严格小于窗口；拒绝零值 policy、options、第二构造器和可提高 cap 的调用方输入，AST 禁止构造器外 struct literal。

**验证：** `go test -json -count=1 ./internal/orchestrator -run '^(TestSkillHistoryPolicyConstructorFixesAllCaps|TestSkillHistoryPolicyRejectsZeroOutOfRangeAndInvalidRelations|TestSkillHistoryPolicyCannotBeForgedOrRaised)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F9、F17、F26 / AC9、AC17、AC26。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`TestSkillHistoryPolicyConstructorFixesAllCaps`、`TestSkillHistoryPolicyRejectsZeroOutOfRangeAndInvalidRelations`、`TestSkillHistoryPolicyCannotBeForgedOrRaised` 三个 required 根及全部子测试均 run+pass、无 fail/skip。`SkillHistoryPolicy` 只含私有标量字段与 validity marker，唯一三参数构造器内部固定 `maxTurns=1000`，分别约束 session bytes 为 1 B–1 GiB、model window 为 2–10000000、auto margin 为 1–1000000 且严格小于窗口；零值、越界值、关系无效值以及 marker 或字段篡改均 fail closed。AST 审计覆盖 `internal/orchestrator` 全部生产 Go 文件，确认仅一个 policy 类型、一个三参数构造器，构造器外无 struct literal、无可变指针 receiver、无 options、setter、第二构造器或调用方 turn cap。`internal/orchestrator`、`internal/skill`、`internal/contextmgr` 三包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.24b 扫描最近完整 Conversation turn（已完成，2026-08-03）

**文件：** `internal/skill/history.go`, `internal/skill/history_test.go`, `internal/orchestrator/independent.go`
**依赖：** T3.24a
**步骤：**
1. 从最新消息向前只产生完整 user→assistant index range；tool call/result 链与所属 turn 原子纳入或舍弃，开头/结尾残缺及未闭合工具链均不返回。
2. scanner 只持有只读 index，不调用会复制整段会话的 `ContextMessages`，不 clone 全会话；每个 turn 和长 range 内按 C15 固定粒度检查 `ctx`。

**验证：** `go test -json -count=1 ./internal/skill ./internal/orchestrator -run '^(TestCompleteTurnScannerKeepsToolChainAtomic|TestCompleteTurnScannerRejectsIncompleteEdges|TestCompleteTurnScannerWalksNewestWithoutFullConversationClone)$'` 通过，三个 required test 均 pass 且无 skip。
**覆盖：** F7、F20、F26 / AC7、AC20、AC26。
**完成记录（2026-08-03）：** 批准的精确命令 exit 0，`xagent/internal/skill` 的 `TestCompleteTurnScannerKeepsToolChainAtomic`、`TestCompleteTurnScannerRejectsIncompleteEdges`、`TestCompleteTurnScannerWalksNewestWithoutFullConversationClone` 三个 required 根各恰好一次 run+pass、无 fail/skip；`xagent/internal/orchestrator` 同一精确命令编译并通过。`CompleteTurnScanner` 与 `CompleteTurnRange` 只保存私有整数游标/边界，从最新消息向前逐个识别 user 到最终可见 assistant 的完整 index range；开放、缺失、身份错配的 tool call/result 链及其所属 turn 原子舍弃，头尾孤立消息、未完成尾 turn、未知或上下文角色不会被误收，thinking 不进入可见完整性判定。扫描开始、每个候选 turn 及长 range 每至多 1024 个消息索引检查同一 `ctx`；取消返回 `ctx.Err()`、零 range 且不推进 scanner 游标。shape、源文件与别名测试确认 scanner/range 不持有消息、生产 `history.go` 不调用 `ContextMessages`，构造 scanner 后直接读取调用方只读 slice；兼容入口只克隆最终选中的完整消息，不再预先复制整段会话。`internal/skill`、`internal/orchestrator` 两包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.24c 选择有界且无别名的 Skill history（已完成，2026-08-03）

**文件：** `internal/orchestrator/independent.go`, `internal/orchestrator/independent_test.go`, `internal/orchestrator/chat.go`, `internal/contextmgr/manager.go`
**依赖：** T3.24b
**步骤：**
1. `requested<0` 或大于 1000、伪造 policy、负 measure、加减溢出均 fail closed；`requested=0` 在 policy/requested/ctx 校验后直接返回空、未截断结果，不构造或计量请求。
2. requested>0 时从完整 base measure 开始，使用唯一 Budgeter 对每个只读 turn range 计量并 overflow-safe 累加；只有 turns/bytes/planning-tokens 同时满足才深拷贝为安全 `[]provider.ModelMessage`，不得让 `conversation.Message` 进入 Provider 请求。
3. 有更旧完整 turn 且已达到 requested 时 Reason=turns；同一候选同时超 byte/planning-token 时 Reason=bytes，否则使用实际触发项，固定优先级 turns→bytes→planning-tokens。取消不发布部分选择；输出保持原顺序且不与主会话 alias。

**验证：** `go test -json -race -count=1 ./internal/orchestrator -run '^(TestSkillHistorySelectionHonorsTurnsBytesAndPlanningTokens|TestSkillHistoryLimitReasonPriorityIsTurnsBytesPlanningTokens|TestSkillHistorySelectionReturnsOnlyCompleteNonAliasedModelMessages|TestSkillHistorySelectionCancellationPublishesNothing|TestSkillHistoryZeroReturnsEmptyUntruncatedWithoutMeasurement)$'` 通过，五个 required test 均 pass 且无 skip。
**覆盖：** F7、F9、F17、F20、F26 / AC7、AC9、AC17、AC20、AC26。
**完成记录（2026-08-03）：** 批准的精确 race 命令 exit 0，五个 required 根及 turns/bytes/planning_tokens 三个完整子测试均 run+pass、无 fail/skip，race detector 无报告。Orchestrator 现持有私有 `SkillHistoryPolicy` 与唯一 concrete `RequestBudgeter` value；selector 在任何扫描前二次验证 policy、Budgeter、requested 和 `ctx`，拒绝 requested<0、requested>1000、零值/篡改 policy、零值 Budgeter、负 measure 与加法溢出。requested=0 在这些验证后直接返回空、未截断、`HistoryLimitNone`，nil Conversation 与带负 thinking budget 的不可计量 base 仍不触发构造、扫描或计量。requested>0 先计量完整 base，拒绝 byte 超限或达到 planning-token 边界，再从最新完整 turn 开始用同一 Budgeter 逐 range 计量并 overflow-safe 累积；只有 turns、session bytes、planning tokens 同时满足才保留范围，截断原因严格按 turns→bytes→planning-tokens 判定，同一候选双超限固定选择 bytes。全部决定完成且 `ctx` 仍有效后，选中范围才按原会话顺序转换为新的安全 `[]provider.ModelMessage`，thinking 不输出，tool call/result 的安全 typed 字段完整保留且输出 mutation 不影响主会话；预取消和选择完成后的晚取消都只返回零选择。`RequestBudgeter.Validate` 只提供无计量的 injected-value 完整性检查，不新增构造或替换入口；静态引用核对确认 selector 尚未接入生产路径，原子切换留给 T3.24d。`internal/orchestrator`、`internal/skill`、`internal/contextmgr` 三包完整回归 exit 0；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.24d 原子接入 isolated Skill history（已完成，2026-08-03）

**文件：** `internal/orchestrator/independent.go`, `internal/orchestrator/independent_test.go`, `internal/repoaudit/orchestrator_refs_test.go`
**依赖：** T3.24c、T2.31b
**步骤：**
1. 在一个连续切换中从 `IndependentRequest` 删除可绕过预算的 `History []conversation.Message`，移除旧 `RecentCompleteTurns` 生产入口；isolated Skill 只能使用 parser snapshot 中已验证的 requested 值。
2. requested>0 时先组装完整非历史 base→`MeasureRequest`→select→插入安全历史→最终整体 `MeasureRequest`；最终 measure 必须分别不高于增量累计且满足 session byte 与 model-window planning-token 阈值。
3. invalid/negative/overflow/cancel/超限或最终增量不一致全部在 Provider、工具和 Store 副作用前失败；任意既有合法非空 model override 保持合法且不参与计量规则。

**验证：** `go test -json -race -count=1 ./internal/orchestrator ./internal/repoaudit -run '^(TestIndependentRequestHasNoHistoryInjection|TestIndependentHistoryBuildsBaseBeforeSelection|TestIndependentHistoryFinalRemeasurePrecedesExternalEffects|TestIndependentHistoryBudgetFailureHasNoSideEffect|TestIndependentModelOverrideRemainsCompatible|TestLegacyHistorySelectorsHaveNoProductionReference)$'` 通过，六个 required test 均 pass 且无 skip。
**覆盖：** F7、F9、F17、F20、F26 / AC7、AC9、AC17、AC20、AC26、AC35。
**完成记录（2026-08-03）：** 批准的精确 race 命令 exit 0，六个 required 根各恰好一次 run+pass、无 fail/skip，race detector 无报告。`IndependentRequest` 已删除可注入的 Conversation history，isolated Skill 只把 parser snapshot 的 `PreparedInvocation.History` 通过私有绑定带到请求构造边界；旧 `RecentCompleteTurns` 生产入口及其旧测试已删除。每轮 Provider 请求均先形成包含 model override、System/SOP、当前 user、thinking、tool definitions 与 cache framing 的完整非历史 base，再用唯一 `RequestBudgeter` 计量、选择最新完整 turn 并转换为无别名的安全 `ModelMessage`；插入后执行最终整体复量，分别拒绝超过增量累计、session bytes 或 planning-token 阈值的请求。policy、Budgeter、requested、measure、overflow、取消和预算错误均在 `StreamChat` 前 fail closed；最终复量失败会释放 prompt lease，`RunIndependent` 保留 typed error，`SendSkill` 回滚主会话 user checkpoint 且跳过 Store 保存，实测 Provider、Store、Memory 与主会话均无副作用。resolved config 在唯一启动组装根构造并经 App 透传 policy/Budgeter，合法非空 child model override 继续生效。`cmd/xagent`、`internal/app`、`internal/orchestrator`、`internal/skill`、`internal/contextmgr` 五包完整回归 exit 0；新增 Repo audit 精确根通过并确认生产代码无 `RecentCompleteTurns`、`IndependentRequest.History` 或 `History []conversation.Message`。扩展运行整个 `internal/repoaudit` 仍暴露既有 `TestBuiltinToolsHaveOnlyExecutorEntryAndNoReadAllThenTruncate` 失败（当前工具执行入口计数 2、期望 1），与本任务新增 history audit 无关且未在本任务越界修改。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.25 补齐并回归 Provider 流全部退出路径（已完成，2026-08-03）

**文件：** `internal/orchestrator/stream_collector.go`, `internal/orchestrator/agent_loop.go`, `internal/orchestrator/independent.go`, `internal/orchestrator/independent_test.go`, `internal/orchestrator/chat_test.go`
**依赖：** T2.35、T2.46、T2.69、T3.24d、T1.8
**步骤：**
1. 在 T2.69 已完成接口切换的基础上，调用 `StreamChat` 取得 `ChatStream` 后立即登记 `Close(ctx)`，只从 `Events()` 消费；正常、错误、取消和提前返回都进入同一关闭路径。
2. 只消费 C5 `SafeText`、`SafeError` 和 `SafeToolCall`；`unsafe_tool_arguments` 不得进入授权或 Executor。
3. 主请求与独立 Skill 请求使用相同的 ChatStream owner/Close 规则；独立 Activity 在成功、Provider 失败、取消和无消费者退出时都清理，且不得污染主会话 Activity 或摘要事务。

**验证：** `go test -json -race -count=1 ./internal/orchestrator -run '^(TestOrchestratorClosesStreamOnEveryExitPath|TestIndependentClosesStreamOnProviderFailure|TestIndependentCancellationClosesStream|TestIndependentActivityClearsOnEveryExit|TestIndependentSummaryDoesNotMutateMainConversation|TestRunIndependentCancellationDoesNotRequireOutputConsumer)$'` 通过，六个 required test 均 pass 且无 skip。
**覆盖：** F7、F17 / AC7、AC17。
**完成记录（2026-08-03）：** 批准的精确 race 命令 exit 0，六个 required 根各恰好一次 run+pass、无 fail/skip，race detector 无报告。Orchestrator 的单次 Provider 流现由 `ownProviderStreamWithRedactor` 唯一持有：`StreamChat` 返回后立即对任意非 nil `ChatStream` 登记 defer Close，再检查 start error 或唯一一次取得的 `Events()`；正常完成、Provider SafeError、意外 EOF、nil event channel、Close 错误、请求取消、消费者提前退出以及 non-nil partial stream 与 start error 同返八条路径均实测 Close 恰好一次，partial stream 的 Events 保持零次读取。主请求与 isolated Skill 共用同一 `runAgentLoop` owner，独立 Provider 错误与取消均各自证明流恰好关闭一次；独立 Activity 在成功、Provider 失败、取消及无消费者取消出口均清空，无消费者取消不死锁。直接 `RunIndependent` 的成功摘要只存在于返回值，实测不保存、不更新 Memory、不改变主会话或摘要事务。Provider 事件消费继续只接受 `SafeText`、`SafeError` 和 `SafeToolCall`；额外三根联合 race 测试证明 `unsafe_tool_arguments` SafeError 及恶意 Provider 伪造的畸形 SafeToolCall 都不会进入确认、工具 Hook、Executor 或 tool-role history。`internal/orchestrator` 与 `internal/provider` 两包完整回归 exit 0；生产静态核对确认唯一 `StreamChat`、唯一 `Events()` 和唯一 `closeProviderStream` 调用点。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.26 统一 usage 与有序持久化（已完成，2026-08-03）

**文件：** `internal/orchestrator/agent_loop.go`, `internal/orchestrator/stream_collector.go`, `internal/orchestrator/chat.go`, `internal/orchestrator/run_tracker.go`, `internal/orchestrator/usage_commit_test.go`, `internal/orchestrator/ordered_commit_test.go`, `internal/orchestrator/tool_ordered_commit_test.go`, `internal/orchestrator/run_tracker_test.go`
**依赖：** T3.18、T3.19、T3.19a、T3.23–T3.25
**步骤：**
1. Provider 发布的唯一最终 usage 同时驱动状态事件和 ContextManager 估算，不得各自重算。
2. 工具结果只按调用顺序提交 T3.19a 已验证的安全 projection 后再保存，不得重读 Result 兼容字段；事件或保存失败保留真实执行事实，禁止重复执行工具。
3. 外层主请求与独立 Skill 在任何副作用前登记 `runTracker`，所有同步失败、取消、错误和成功出口恰好释放一次；`WaitIdle` 只在有序结果提交与保存结束后返回，不轮询、不依赖短 sleep。

**验证：** `go test -json -race -count=1 ./internal/orchestrator -run 'TestUsageAndToolResultsHaveSingleOrderedCommit|TestRunTrackerCoversMainAndIndependentExits|TestWaitIdleReturnsAfterOrderedSave'` 通过。
**覆盖：** F18、F20、F23、F24 / AC18、AC20、AC23、AC24。
**完成记录（2026-08-03）：** 批准的精确 race 命令由主 Agent 在并行分支冻结后重新执行，exit 0；三个 required 根及全部子测试均 run+pass、无 fail/skip，race detector 无报告。Provider usage 在 collector 中复制为不可别名的最终快照，collector 不再发布 usage；唯一 `commitProviderUsage` 使用同一快照更新 ContextManager、发布恰好一次 `UsageUpdated`，重复、Done 后迟到、负数及溢出 usage 均 fail closed 且不发生部分提交。工具执行事实按 Provider 调用序号稳定排序，全部只从一次安全 projection 写入 Conversation、Hook、Event 和下一轮模型内容，再进行唯一保存；兼容 `Result` canary 未被重读，Event 失败仍保存，Store 失败保留内存事实、发布终态且不重试保存、Provider 或工具。主请求与 isolated Skill 在 Hook 等副作用前登记 runTracker，同步失败、成功、Provider 错误和取消出口均恰好释放；确定性 Save barrier 分别证明两类请求保存阻塞时 active=1，只有 `save-exit` 后 `WaitIdle` 才返回，全程无轮询或短 sleep。`go test -json -count=1 ./internal/orchestrator ./internal/contextmgr` 完整回归 exit 0；生产调用点审计确认唯一 `UsageUpdated` / `UpdateUsage` 提交点以及有序 publisher 后的唯一保存边界；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。

### T3.27 建立 M3 集成门禁（已完成，2026-08-03）

**文件：** `internal/tool/executor.go`, `internal/tool/executor_test.go`, `internal/orchestrator/m3_canary_integration_test.go`, `internal/orchestrator/t327_canary_integration_test.go`, `internal/{conversation,contextmgr,sessionctx,prompt,memory,orchestrator,tool,mcpclient,events,hook,tui,repoaudit}/*_test.go`
**依赖：** T2.70、T3.13–T3.19、T3.19a、T3.20–T3.26
**步骤：**
1. 覆盖大于 64 KiB 的合法记录、原地状态修改、保存故障、部分列表、时间配置和旧格式非破坏迁移。
2. 覆盖授权阶段顺序、阻塞只读工具并发、启动前取消、Provider 提前退出、usage 单一来源和 MCP 调用 lease。
3. 将同一 canary 放入 Conversation、Context、Provider、MCP 和 Memory 输入，断言 JSONL、模型输入、事件、诊断与测试输出均无原文。

**验证：** `go test -json -count=1 ./...`、`go test -json -race -count=1 ./internal/conversation ./internal/contextmgr ./internal/sessionctx ./internal/prompt ./internal/memory ./internal/orchestrator ./internal/tool ./internal/mcpclient ./internal/events ./internal/hook ./internal/tui ./internal/repoaudit` 和 `go test -json -race -count=20 ./internal/conversation ./internal/orchestrator -run 'Recovery|SaveFailure|ConcurrentTools|Cancellation|ClosesStream|Projection|ModelContent'` 通过；T3.19/T3.19a 的全部 required `(Package, Test)` 在首两条命令各自独立的 JSON 结果中分别恰好一次 run+pass、无 skip/fail，不把两条命令合并计为一次证据。第二条命令必须包含 `internal/repoaudit`，否则 T3.19 required 根 `TestContextResultRefsProductionBoundary` 为零匹配，门禁不得通过。
**覆盖：** F3–F7、F10、F17–F24 / AC3–AC7、AC10、AC17–AC24、AC34、AC35。
**完成记录（2026-08-03）：** 三条门禁均由主 Agent 在并行诊断与修复分支冻结后串行重跑。`go test -json -count=1 ./...` exit 0；T3.19 的 8 个 required 根、T3.19a 的 12 个 required 根及新增 `TestM3CrossChannelCanaryNeverLeaks`、`TestM3CrossChannelCanaryTestOutput`、`TestM3SharedCanaryAcrossConversationContextProviderMCPAndMemory` 均各恰好一次 run+pass，required 根无 fail/skip；三个矩阵根的 canonical 子测试闭集分别为 Projection 24、Capture 11、ModelContent Slot 13，均各恰好一次 run+pass、无额外更深 descendant；仅 `internal/resources` 与 fakeprovider 两个无测试文件 package 产生 Go package-level skip。原第二条命令漏列 `internal/repoaudit`，与“前两条命令各自包含全部 required 根”矛盾，现已补包；修订后的 12 包 race 命令 exit 0，20 个 required 根、48 个 canonical 子测试及三个 canary 根再次分别各一次 run+pass、无 fail/skip，race detector 无报告。第三条 `count=20` race 命令 exit 0，实际匹配 Conversation 2 根与 Orchestrator 13 根，共 15 根；每根精确 run=20、pass=20，共 300 次根 run 与 300 次根 pass，无 fail/skip/race。集成门禁暴露的唯一全仓失败已修复：`Executor.executeValidated` 以局部闭包保留 candidate 同步与 legacy goroutine/timeout 语义，同时把底层 `Tool.Execute` 收敛为唯一生产调用点并删除同步返回后的不可达 candidate timeout 分支，`TestBuiltinToolsHaveOnlyExecutorEntryAndNoReadAllThenTruncate` 与两包完整回归通过。`TestRunIndependentCancellationDoesNotRequireOutputConsumer` 的 1 ms 轮询已替换为确定性 Provider-entered channel，专项 race `count=20` 为 20 次 run+pass。新增 M3 canary 根用同一 raw canary 非空洞触达 Conversation、Context、Provider、MCP、Memory 与 diagnostics 输入，逐字段检查 Provider/model DTO、Event、projection、重载 Conversation 和诊断，递归扫描真实 JSONL/Memory regular files；子进程扫描完整 canary 及前后半段，证明测试输出也无原文，三个根的普通及 race 验证均 exit 0。`git diff --check` 与相关 Go 文件 `gofmt -l` 无输出，生产静态核对确认底层 `validated.executor.Execute` 仅一处。

## M4：App、TUI、CLI 与唯一组装根

### T4.1 定义三层 App 状态（已完成，2026-08-03）

**文件：** `internal/app/state.go`, `internal/app/state_test.go`
**依赖：** T3.27
**步骤：**
1. 定义 `RuntimeState`、`ConversationState` 和 `RequestState`，分别承载进程级、会话级和单请求级状态。
2. RuntimeState 持有跨 reset 单调递增的 request sequence；每个 RequestState 捕获唯一 generation，并承载 duration、usage、cache、stop reason、`SafeError`、confirmation 和临时缓冲。
3. App 状态不得保存普通 error 文本或 raw payload。

**验证：** `go test -json -count=1 ./internal/app -run TestRuntimeConversationRequestStateAreSeparated` 通过。
**覆盖：** F7、F26 / AC7、AC26。
**完成记录（2026-08-03）：** 新增 `RuntimeState`、`ConversationState`、`RequestState` 及 App 自有的 `Usage`、`CacheUsage`、`ConfirmationState` 安全值类型；进程级 sequence 由包内分配原语递增并在 `uint64` 耗尽时 fail closed，request replacement 后不归零或复用 generation。RequestState 完整承载 duration、token、cache、stop reason、`*diagnostics.SafeError`、不含 decision capability/raw arguments 的安全 confirmation DTO 与 `TransientIDs`，反射门禁拒绝普通 `error`、无界 interface、raw JSON/bytes 及 raw/payload 字段。精确命令 `go test -json -count=1 ./internal/app -run TestRuntimeConversationRequestStateAreSeparated` exit 0，主根及字段归属、request replacement、SafeText canary、sequence 耗尽 4 个子场景均各一次 run+pass、无 fail/skip；`go test -json -count=1 ./internal/app` exit 0；`git diff --check` 与 `gofmt -l internal/app/state.go internal/app/state_test.go` 均无输出。现有 `Model` 消费者迁移、两个 reset 边界及旧 generation 拒绝按批准边界留给 T4.2，未提前改动 T4.11 事件 envelope 或 T4.12 ViewModel。

### T4.2 实现请求与会话 reset 边界（已完成，2026-08-03）

**文件：** `internal/app/state.go`, `internal/app/state_test.go`
**依赖：** T4.1
**步骤：**
1. 新请求清除旧 RequestState 后从 RuntimeState sequence 分配更大的 generation；sequence 不得归零或复用，并保留当前 ConversationState、Provider/MCP 和 RuntimeState。
2. 切换会话额外清除 mode、skills、输入、消息视图和会话通知，但保留运行配置与长期诊断。
3. 新请求 reset 必须保留当前会话 Skill Activity；会话切换必须显式 Clear，旧 generation 的迟到 Skill snapshot 不得回填。

**验证：** `go test -json -count=1 ./internal/app -run 'TestRequestAndConversationResetBoundaries|TestRequestGenerationNeverReusedAcrossReset|TestRequestResetPreservesSkillAndSessionResetClearsIt'` 通过。
**覆盖：** F26 / AC26。
**完成记录（2026-08-03）：** 在三层 App 状态上建立统一 reset 原语：请求 reset 先从保留的 Runtime sequence 分配更大 generation，再以新 RequestState 整体替换旧 duration、usage、cache、stop reason、SafeError、confirmation 与 transient IDs，同时保留 ActiveID、mode、Skills、安全 input/messages/notice，并将已保留 Skill Activity 重新绑定到新 generation；会话 reset 同样先完成 generation 分配，再通过窄 `Clear()` capability 恰好一次显式清理旧 Skill Activity，并原子清空旧 RequestState、mode、skills、input、messages 与 notice，只安装新的 ActiveID/generation。Skill snapshot 仅在非零且匹配 `SkillGeneration` 时深复制发布，旧 request/session generation 在任何分配和写入前拒绝；sequence 耗尽负例证明 Runtime、Conversation、Request 与 Activity 均无部分清理。批准命令 exit 0，`TestRequestAndConversationResetBoundaries`、`TestRequestGenerationNeverReusedAcrossReset`、`TestRequestResetPreservesSkillAndSessionResetClearsIt` 三根各一次 run+pass、无 fail/skip；同一命令增加 `-race` 后亦 exit 0、无 race。T4.1 精确根与 `go test -json -count=1 ./internal/app` 全量回归均 exit 0；`git diff --check` 与相关文件 `gofmt -l` 无输出。按批准文件面未提前迁移 legacy Model、导航 generation、事件 envelope 或 ViewModel。

### T4.3 冻结 App 与 TUI 边界契约（已完成，2026-08-03）

**文件：** `internal/app/deps.go`, `internal/tui/view_model.go`, `internal/app/app_test.go`, `internal/tui/program_test.go`
**依赖：** T4.1、T3.18、T3.26
**步骤：**
1. 冻结供后续 state-driven App candidate 使用的窄依赖契约：只暴露 Conversation 访问、Orchestrator 协调、artifact 用户读取和 App 所需 Hook 生命周期能力，不暴露全局容器、维护/清理/路径能力或进程关闭能力。
2. 冻结 capability-free 的不可变安全 ViewModel 与 intent 值契约；构造和读取切片均防御性复制，值中不得持有 Config、Store、Provider、MCP、Artifact Store、Orchestrator 或回调能力。
3. 本任务不迁移当前 legacy `Model`/`New(Deps)` 或既有 TUI renderer；三层状态到结构化 ViewModel 的投影和 renderer 切换由 T4.12 完成，Assembly candidate 接入与生产入口原子切换分别由 T4.25f、T4.29a 完成。

**验证：** `go test -json -count=1 ./internal/app ./internal/tui -run TestTUIHasNoDomainServiceDependency` 通过。
**覆盖：** F7、F10、F21、F25–F29 / AC7、AC10、AC21、AC25–AC29。
**完成记录（2026-08-03）：** 固定供 T4.25f Assembly 消费的 App 窄能力契约：Conversation 仅暴露 Create/List/Load/Save，Orchestrator 仅暴露 App 实际使用的六项协调能力，artifact 仅允许显式用户读取，Hook 仅允许 SystemStart/SessionStart/SessionEnd 且不下放进程 owner 的 Shutdown；编译期断言上述接口由现有真实实现满足，反射门禁同时拒绝 Config、Provider、MCP、完整 artifact Store、Registry/Executor/Tool、Manager 与全局容器泄漏。TUI 新增私有字段、构造与读取双向防御复制的安全 ViewModel，以及不携带接口、函数、指针、通道或领域服务的纯值 Intent；AST 门禁扫描全部非测试 TUI 源码，禁止直接导入 Config、Provider、MCP、Artifact、Orchestrator，并拒绝经现有 conversation/tool 兼容 DTO import 取得 Store、Conversation、Registry、Executor 等 capability。批准命令 exit 0，App/TUI 两包同名根各一次 run+pass；同命令增加 `-race` 后 exit 0 且无 race；`go test -json -count=1 ./internal/app ./internal/tui` 全量回归 exit 0；`git diff --check` 与四个批准 Go 文件的 `gofmt -l` 均无输出。按批准迁移边界，现有 legacy `New(Deps)` 与 Model 内旧生产组装仍保留，T4.25f 负责注入该窄契约，T4.29a 再原子切换生产入口；本任务未提前迁移 T4.12 的完整 TUI 渲染状态。

### T4.4 固定 C9 导航命令元数据（已完成，2026-08-03）

**文件：** `internal/command/definition.go`, `internal/command/registry.go`, `internal/command/builtins.go`, `internal/command/registry_test.go`, `internal/command/builtins_test.go`
**依赖：** T4.3
**步骤：**
1. 扩展 command metadata，使正式名称、别名、可见性、上下文快捷键、权限模式、状态/诊断入口和 intent 只有一个定义来源。
2. 注册公开 `/new`、`/sessions` 和 `/list` 别名，以及聊天页 `Esc`、列表页 `Enter`/`n`/`q`；命令与快捷键必须产生相同 intent。

**验证：** `go test -json -count=1 ./internal/command -run TestC9NavigationCommandsAndBindings` 通过。
**覆盖：** F25、F29 / AC25、AC29。
**完成记录（2026-08-03）：** 扩展唯一 command Definition 元数据，继续以 `Hidden` 作为可见性单一来源，并统一保存纯值 `IntentKind`、带状态上下文和说明的 Shortcut，以及权限模式与公开状态/诊断入口的结构化 HelpEntry；Registry 在启动时规范化命令、别名、按键与帮助条目，拒绝非法枚举、空 intent/说明、隐藏命令发布公开元数据、同定义重复和跨定义快捷键/帮助入口冲突，提供确定排序、防御复制以及按命令/别名或上下文按键解析 intent 的只读入口。新增公开 `/new` 与 `/sessions`（公开别名 `/list`），固定列表页 `n→new_conversation`、`Enter→open_conversation`、`q→quit`，空闲聊天 `Esc→show_sessions`，并为 streaming/confirmation 的 `Esc` 单独固定 `cancel`，避免导航覆盖取消语义；`/new` 与 `n`、`/sessions`/`/list` 与空闲聊天 `Esc` 实测产生相同 intent，斜杠命令只经窄 IntentSink 发值，不接触 Store、Orchestrator 或 TUI。`/permission` 是 `strict/default/permissive` 三种权限模式的唯一帮助元数据 owner，公开 `/status` 同时拥有 status 与 diagnostics 入口，隐藏 `/diagnostics` 不发布公开帮助元数据；直接受影响的既有公开命令数量断言机械更新为 11。批准命令 exit 0，`TestC9NavigationCommandsAndBindings` 恰好一次 run+pass；同命令增加 `-race` 后 exit 0 且无 race；`internal/command`、`internal/app` 与 `internal/tui` 完整回归均 exit 0。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。实际 generation、异步结果过滤和 App 导航副作用按批准边界留给 T4.5 及后续任务。

### T4.5 定义 generation 导航状态机（已完成，2026-08-03）

**文件：** `internal/app/navigation.go`, `internal/app/navigation_test.go`
**依赖：** T4.2、T4.4
**步骤：**
1. 每次导航创建递增的 `NavigationRequest.Generation`，并绑定 kind 与可选 session ID。
2. 所有异步结果提交前复验 generation；迟到结果只产生有界诊断，不覆盖当前 screen 或状态。

**验证：** `go test -json -count=1 ./internal/app -run TestNavigationDropsStaleGeneration` 通过。
**覆盖：** F25 / AC25。
**完成记录（2026-08-03）：** 在 `internal/app` 建立独立的 `NavigationKind`、`NavigationRequest{Generation, Kind, SessionID}` 与 `navigationState` 状态层；仅 new、show sessions 和 open conversation 属于导航，cancel/quit 保持为控制 intent。状态层在同一互斥边界内分配独立且严格递增的 generation、原子替换当前完整请求，并对非法 intent、session metadata 与序列耗尽 fail closed；异步提交按 generation/kind/session ID 完整身份复验，成功后消费当前请求，zero/future/mismatch/duplicate/stale 均不执行发布回调，只向 `diagnostics.BoundedSink` 聚合固定且无内容的安全记录。提交回调限定为同步、不可失败、非阻塞、不可重入的内存发布。`TestNavigationDropsStaleGeneration` 的子测试矩阵覆盖 reset 前后单调分配、非法输入与耗尽原子性、完整身份拒绝、重复结果、诊断洪泛、完整 App 状态快照零修改，以及 64 路并发分配无重复连续和并发提交仅当前请求一个副本成功；批准命令 exit 0，同根 `-race -count=1` 为 6 run/7 pass、`-race -count=20` 为 120 run/121 pass，均无 fail/skip/race；`internal/app` 全量回归 94 run/95 pass、无 fail/skip。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。本任务只验收独立 generation 状态层，尚未迁移现有生产 `Model`；后续 owner 必须以 `*navigationState` 持有且不得随 request/session reset 重建或归零。Esc 取消、WaitIdle/Save、候选构造和 Hook 生命周期不计入 T4.5 完成证据，分别由 T4.6–T4.9 验收。

### T4.6 实现 C9 取消后再导航（已完成，2026-08-03）

**文件：** `internal/app/navigation.go`, `internal/app/update.go`, `internal/app/navigation_test.go`
**依赖：** T4.5、T3.24
**步骤：**
1. streaming 或等待确认时第一次 `Esc` 只发出显式取消，screen 和会话保持不变。
2. 取消清理完成后再次 `Esc` 才产生 Sessions intent；空闲聊天页第一次 `Esc` 可直接打开列表。

**验证：** `go test -json -race -count=1 ./internal/app -run TestEscapeCancelsBeforeNavigating` 通过。
**覆盖：** F24–F26 / AC24–AC26。
**完成记录（2026-08-03）：** Esc 不再依赖硬编码页面分支，而是按当前 chat idle、streaming 或 confirmation 上下文从 C9 command metadata 解析 intent；流式执行或等待确认时解析为 `cancel`，复用共享 lifecycle 的幂等取消路径，第一次 Esc 只调用一次显式 Cancel，重复 Esc 在事件流关闭前不二次取消、不生成导航 intent，也不清除 request/confirmation 或改变 screen、active Conversation。等待确认取消不会伪造 permission decision；只有 `eventStreamClosedMsg` 完成请求清理后，再次 Esc 才生成 capability-free 的 `show_sessions` intent。空闲聊天第一次 Esc 直接生成同一 intent，`/sessions` 也经 command `IntentSink` 进入相同值交接；intent 产生阶段不访问 Store/Orchestrator、不提前切 screen 或会话。批准的精确 race 命令 exit 0，根测试及 streaming、confirmation、idle chat、slash command 四个子场景均 run+pass、无 race；相关 race 回归中的既有 `TestCancelWaitsForTurnEnd` 同样通过，`internal/app`、`internal/command`、`internal/tui` 完整回归均 exit 0。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。WaitIdle、Save 和 intent 消费后的导航事务按批准边界留给 T4.7。

### T4.7 实现导航等待与保存阶段（已完成，2026-08-03）

**文件：** `internal/app/navigation.go`, `internal/app/navigation_test.go`
**依赖：** T4.5、T4.6、T3.18、T3.26
**步骤：**
1. 固定先执行 `Orchestrator.WaitIdle`；存在 active Conversation 时再 `Store.Save`，冷启动列表等无 active 状态明确跳过 Save，禁止调用 `Save(nil)`。
2. WaitIdle 或 Save 失败时保留原 screen、ConversationState、消息视图和 Hook 生命周期，并显示安全错误。

**验证：** `go test -json -race -count=1 ./internal/app -run 'TestNavigationSaveFailurePreservesActiveConversation|TestNavigationSkipsSaveWithoutActiveConversation'` 通过。
**覆盖：** F20、F21、F25 / AC20、AC21、AC25、AC34。
**完成记录（2026-08-03）：** 导航准备阶段只接收 `WaitIdle` 与 `Save` 两项窄能力，严格执行 generation 复验 → `WaitIdle` → generation 复验 → 可选 `Save` → generation 复验；存在 active Conversation 时恰好把该非 nil 对象交给 Save，冷启动或会话列表无 active 时在 WaitIdle 后直接返回 ready preparation，实测 Save 调用为零且从不出现 `Save(nil)`。WaitIdle 失败立即停止、不会调用 Save；Save 失败不产生候选。两类失败均返回固定 code/source、可恢复且只含 `SafeText` 的错误供界面显示，原始错误仅进入注入的 `diagnostics.BoundedSink` 并经运行期脱敏，canary 未进入安全错误或诊断。失败路径对 screen、ConversationState、消息视图和 Hook 事件 sentinel 均逐字段保持不变。确定性 barrier 进一步证明旧导航阻塞在 WaitIdle 时被新 generation 替代后，不执行 Save、不回填旧安全错误，只聚合一条有界 stale 诊断且不扰动当前导航。批准的精确 race 命令 exit 0，两个 required 根及 WaitIdle 失败、Save 失败、被替代 WaitIdle、active 成功顺序四个子场景均 run+pass、无 race；覆盖 T4.5–T4.7 的导航 race 回归和 `internal/app`、`internal/command`、`internal/tui` 完整回归均 exit 0。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。List/Create/Load、完整候选和 CommitCandidate 按批准边界留给 T4.8。

### T4.8 构造导航候选并准备原子提交（已完成，2026-08-03）

**文件：** `internal/app/navigation.go`, `internal/app/navigation_test.go`
**依赖：** T4.7、T3.18
**步骤：**
1. Save 成功后按 intent 执行 List、Create 或 Load，并将完整候选状态与 generation 一起返回。
2. 只有候选成功且 generation 仍匹配时才生成不可变 CommitCandidate；本任务不得提前修改 screen、ConversationState、消息视图或 Hook 生命周期。

**验证：** `go test -json -count=1 ./internal/app -run TestNavigationBuildsCompleteCandidateWithoutEarlyCommit` 通过。
**覆盖：** F21、F25、F26 / AC21、AC25、AC26。
**完成记录（2026-08-03）：** 导航候选阶段只接收 Create/List/Load 三项窄 Store 能力，并严格按已准备 intent 恰好执行一个操作：Sessions 构造保留 active Conversation 与现有 ConversationState/消息视图的列表候选，New 构造 clean chat 候选，Open 绑定请求 SessionID、PersistedState 与 RecoveryReport 构造恢复候选；nil/空白或 ID 不匹配的 Conversation、Load unavailable、Store 顶层错误、nil Store 以及未 ready preparation 均 fail closed，不生成 CommitCandidate。候选完整携带 NavigationRequest/generation、目标 screen、ConversationState、消息投影、Save 元数据及对应 List/Create/Load 元数据；同时保存不对外暴露的 Store-tracked Conversation pointer 与冻结内容快照，避免用深复制对象替换 JSONLStore 按指针维护的身份。Conversation、Context 时间指针、Message/Tool/Artifact/SafeError、ConversationState slice、List/Recovery diagnostics 与 attributes map 均防御复制并保留 nil/empty 形态，Store 返回值、调用方输入或 snapshot 后续突变不能改写已封装候选，也不会在 T4.8 阶段改写 tracked pointer。Store 返回后先完成全部深复制，再在导航锁内同时复验 generation/kind/session ID 完整身份并封装 opaque CommitCandidate，消除了 generation check 与候选生成之间的竞态；List/Create/Load 成功及 Load 错误的确定性 channel barrier 均证明迟到结果只形成固定、有界且无内容的 stale 诊断，不能生成候选、回填旧错误或扰动新 generation。成功、失败和迟到路径对实际 navigationViewState 与 Hook sentinel 逐字段零修改，本任务未消费 current、未执行状态 reset 或 Hook。批准命令 exit 0，为 20 run/21 pass；同根 `-race -count=1` 为 20 run/21 pass，`-race -count=20` 为 400 run/401 pass，均无 fail/skip/race；覆盖 T4.5–T4.9 当前导航根的 race 回归为 46 run/47 pass，`internal/app` 全量回归为 124 run/125 pass，均无 fail/skip。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。旧 SessionEnd、Store-tracked pointer 的锁内物化、原子状态发布和新 SessionStart 的生命周期顺序不计入 T4.8 完成证据，由 T4.9 验收。

### T4.9 绑定成功导航的 Hook 生命周期（已完成，2026-08-03）

**文件：** `internal/app/navigation.go`, `internal/app/hook_test.go`
**依赖：** T4.8、T2.34
**步骤：**
1. Create/Load 候选完整、generation 匹配且 ActiveID 确实变化时，严格执行“旧 `SessionEnd` → 一次提交 screen/ConversationState/消息视图/reset → 新 `SessionStart`”。
2. `/sessions` 或 `/list` 只切换 screen，不改变 ActiveID，也不发送 SessionEnd/SessionStart；加载当前 ActiveID 同样不得伪造会话切换。
3. Save/Load 失败、取消和迟到 generation 不得发送生命周期事件；任何路径都不得在旧 SessionEnd 前切换会话状态或在提交前发送新 SessionStart。

**验证：** `go test -json -race -count=1 ./internal/app -run TestNavigationOrdersOldEndCommitAndNewStart` 通过。
**覆盖：** F25 / AC25。
**完成记录（2026-08-03）：** 新增唯一 sealed candidate 提交入口，在任何生命周期事件前复验 CommitCandidate 的完整 request/generation、intent/screen、Create/Load 标记、请求 SessionID、active Conversation ID、Store-tracked pointer 身份，以及 ConversationState/消息视图/active Conversation 三份消息投影的一致性；缺失 target、request sequence 耗尽、候选不完整、tracked pointer 缺失或 ID 不匹配、取消 nil 候选及迟到 generation 均在旧 SessionEnd 前 fail closed 且不修改状态。ActiveID 确实变化时，提交器在同一导航锁内严格执行旧 `SessionEnd(switch)` → 一次提交块 → 新 `SessionStart(new|resumed)`：提交块恰好调用一次 `resetConversation`/Skill Activity Clear，将 sealed frozen Conversation 深物化回 Create/Load 返回的原 Store-tracked pointer，再原子发布 screen、ConversationState、消息视图、该精确 active pointer 与全清 RequestState；因此后续 JSONLStore Save 继续命中按指针维护的追踪身份。封装后对 tracked pointer 的内容突变证明 SessionEnd 仍看到未物化的完整旧发布状态，SessionStart 只看到恢复后的冻结内容和精确 Store 指针，不存在提前物化或中间发布窗口；旧 ActiveID 为空时自然跳过 SessionEnd。Sessions/List 候选只切换到 list screen，ActiveID、ConversationState、RequestState、消息视图、active Conversation 与 Skill Activity 均逐字段/指针保持且 lifecycle 调用为零；加载当前 ActiveID 会采用 Load 返回的 Store-tracked pointer 并恢复 sealed 内容，但只刷新持久化消息，保留 mode、skills、input、notice、request generation 和临时请求态，同样不 reset 或伪造 SessionEnd/SessionStart。迟到候选测试使用完整有效 target，确认拒绝只能来自 generation 门禁，tracked pointer 保持封装后的外部突变值且新请求仍为 current；失败、取消和 tracked ID 伪造同样不发 Hook、不消费状态。规定精确 race 命令 exit 0，为 10 run/11 pass；同根 `-race -count=20` 为 200 run/201 pass；覆盖 T4.5–T4.9 的导航 race 回归为 47 run/48 pass，三项均无 fail/skip/race；`internal/app` 全量回归为 128 run/129 pass、无 fail/skip。`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。List 顶层错误和部分结果的用户可见投影按批准边界留给 T4.10。

### T4.10 显示会话列表错误与部分结果（已完成，2026-08-03）

**文件：** `internal/app/app.go`, `internal/app/navigation.go`, `internal/app/navigation_test.go`
**依赖：** T4.8、T3.11
**步骤：**
1. List 顶层错误显示为 `SafeError` 并保留当前页面，禁止伪装为空历史。
2. recoverable placeholder、`Truncated` 和 RecoveryReport 转成安全 ViewModel 数据，用户仍可选择可恢复会话。

**验证：** `go test -json -count=1 ./internal/app -run TestListFailureAndPartialResultsRemainVisible` 通过。
**覆盖：** F7、F19、F21、F22 / AC7、AC19、AC21、AC22。
**完成记录（2026-08-03）：** App 新增只含安全值且防御复制的会话列表状态，将 Store 的部分结果、`Truncated`、扫描计数和经运行期脱敏的 RecoveryReport 投影为可见数据；available partial 会话保持可选择，placeholder 即使被错误标记为 available 也 fail closed 为不可选择，原始 diagnostics 与 canary 不进入公开状态。导航 List 成功时，列表状态与 screen 在当前 generation 内原子发布且不改写 active session；顶层 List 错误（包括同时返回非空 entries）将整批结果视为不可信，发布固定、可恢复的 `navigation_list_failed` SafeError 并保留当前可信页面、会话、消息和列表，既不伪装为空历史，也不自动新建会话。legacy 启动路径采用相同的不可信结果丢弃规则。规定命令 exit 0，两个子场景全部 run/pass；覆盖相关导航、会话切换与列表行为的 `-race -count=1` 回归通过，`internal/app`、`internal/command`、`internal/tui` 三包全量回归通过；`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。本任务未提前实现 T4.11 的安全事件 DTO 或 T4.12 的最终纯 TUI ViewModel。

### T4.11 收敛安全事件 DTO（已完成，2026-08-03）

**文件：** `internal/events/events.go`, `internal/events/events_test.go`, `internal/app/events.go`, `internal/app/update_test.go`
**依赖：** T2.69、T3.25、T4.1
**步骤：**
1. 事件只携带 `SafeText`、`SafeError`、安全 ToolDisplay、usage 和 opaque artifact Ref，并由 App envelope 绑定 request generation 与 Conversation ID。
2. 删除 raw arguments、普通 error string、真实路径及领域对象指针；App 只把匹配当前边界的安全事件归入三层状态，迟到事件只进入有界诊断。

**验证：** `go test -json -count=1 ./internal/events ./internal/app -run 'TestEventsContainOnlySafeDTOs|TestStaleRequestEventCannotCrossResetBoundary'` 通过。
**覆盖：** F7、F10、F24、F26 / AC7、AC10、AC24、AC26。
**完成记录（2026-08-03）：** `internal/events` 将 Event 字段图收敛为精确、闭合的安全 DTO schema：正文、参数、工具/确认/诊断展示文本只允许 `SafeText`，错误精确为 `*diagnostics.SafeError`，usage 为四项纯数值，artifact 精确为 events 自有且不含路径的 opaque `*ArtifactRef`；确认 DTO 不含 decision channel，诊断不含 path，字段图不允许 raw/payload/path、普通 error、chan/func/interface/map/slice、真实 artifact 领域指针或服务能力。反射测试逐 DTO 冻结字段名与字段类型，文本回退为普通 string、新增领域 pointer 或额外字段均 fail closed。App 的 `sealedEventEnvelope` 从当前 `RequestState.Generation` 与 `ConversationState.ActiveID` 捕获身份并深复制 SafeError、Tool/Artifact、Confirmation、Diagnostic、Progress 和 Usage 指针 DTO；发布时只以 `RuntimeState.RequestSequence == RequestState.Generation == envelope.Generation`、`ConversationState.SkillGeneration == RequestState.Generation` 且 ActiveID 等于 envelope ConversationID 为当前真相，任何校验失败都在修改三层状态前拒绝。matching 事件实测可归入持久消息、request-scoped transient ID、usage/cache、duration、stop reason、SafeError 和安全 confirmation 展示；transient 正文不进入会话级 Messages。测试分别在真实 `resetRequest` 与 `resetConversation` 后注入旧 Done、Usage、Progress、Error、Confirmation、Transient Text 六类 sealed candidate，共 12 次均拒绝，Runtime/Conversation/Request 完整快照零修改，且只聚合一条固定 code/source、空 message、无 canary 的有界 stale 诊断。当前 Bubble Tea 迁移适配器的 `legacySequence/legacyCurrent` 已显式隔离，authoritative `applyStateEvent` 不读取它们；其删除留给后续三层 Model 生产迁移。规定命令 exit 0，为 2 run/4 pass；同根 `-race -count=20` 为 40 run/42 pass；`internal/events`、`internal/app`、`internal/orchestrator`、`internal/tui` 四包完整 `-race -count=1` 回归为 400 run/404 pass，均无 fail/skip/race。`git diff --check` 与批准 Go 文件 `gofmt -l` 均无输出。本任务未提前构造 T4.12 的最终纯 TUI ViewModel。

### T4.12 构造纯 TUI ViewModel（已完成，2026-08-03）

**文件：** `internal/tui/view_model.go`, `internal/app/app.go`, `internal/app/state_test.go`, `internal/tui/program_test.go`
**依赖：** T4.3、T4.10、T4.11
**步骤：**
1. App 将三层状态转换成不可变、只含安全值的 ViewModel；生成后修改 App 状态不得反向改变旧视图。
2. TUI 不缓存领域对象、context、reader、capability 或可调用 service。

**验证：** `go test -json -count=1 ./internal/app ./internal/tui -run TestViewModelIsSafeAndImmutable` 通过。
**覆盖：** F7、F10、F21、F25–F29 / AC7、AC10、AC21、AC25–AC29。
**完成记录（2026-08-03）：** 新增完整的纯值 TUI ViewModel，将 Runtime、Conversation、Request 三种生命周期状态以及可信会话列表在 App 边界一次性投影为 generation/runtime sequence、screen、会话模式/skills/input/messages/notice、request duration/usage/cache/stop reason、`SafeError`、安全 confirmation、transient IDs 与列表恢复信息；列表时间转换为 Unix milliseconds，最终快照不保留 `time.Time` 内部表示或 Store 领域值。ViewModel 及全部嵌套只读视图只含私有字段，构造时复制 lines、skills、messages、transient IDs 和列表 entries，所有 slice getter 以及 Conversation/Request/Sessions 嵌套快照再次防御复制，并保留 nil 与明确非 nil 空切片的区别；测试在构造后深改 App 状态、构造输入 slice、SafeError/confirmation 指针内容与列表条目，并在读取后篡改 renderer 所得 slice 与嵌套快照，旧 ViewModel 均保持不变。TUI 反射门禁递归检查最终 ViewModel 字段图，拒绝 exported mutable field、普通 error、interface、pointer、map、func、chan、unsafe pointer、uintptr、raw bytes 以及 artifact/config/conversation/diagnostics/events/MCP/orchestrator/provider/tool 领域类型；既有 AST 门禁继续禁止 TUI 取得领域 service capability。规定命令 exit 0，为 2 run/4 pass、无 fail/skip；同根 `-race -count=20` 为 40 run/42 pass、无 fail/skip/race；`internal/app` 与 `internal/tui` 全量普通及 race 回归均 exit 0。覆盖 ViewModel、TUI capability 与三层状态边界的专项 `-race -count=1` 为 9 run/11 pass、无 fail/skip/race；两包普通全量回归为 164 run/166 pass、无 fail/skip。全仓 `go test -json -count=1 ./...` exit 0，为 2647 run/2681 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip；`go vet ./internal/app ./internal/tui`、`git diff --check` 与四个批准 Go 文件的 `gofmt -l` 均 exit 0 且无输出。按批准边界，legacy `MessagesView`、`ConversationItem` 和 `Status.Error` 的最终 renderer 迁移分别留给 T4.14/T4.15，本任务未提前实施响应式布局或区域迁移。

### T4.13 实现统一响应式布局（已完成，2026-08-03）

**文件：** `internal/tui/layout.go`, `internal/tui/layout_test.go`
**依赖：** T4.12
**步骤：**
1. 由 `ComputeLayout(LayoutInput)` 计算所有区域，任何尺寸下宽高不得为负，也不得越过终端边界。
2. 80×24 使用完整布局，小于基线进入 compact；宽屏限制可读内容宽度并保留必要状态区域。

**验证：** `go test -json -count=1 ./internal/tui -run TestComputeLayoutForBaselineWideAndCompact` 通过。
**覆盖：** F27 / AC27。
**完成记录（2026-08-03）：** 新增无副作用且只含 TUI 自有标量值的 `ComputeLayout(LayoutInput) Layout`，统一将终端划分为连续、互不重叠的 Main、CommandMenu、Confirmation、Input、Status 五个纵向区域。负终端尺寸先归零；所有区域共享 `min(terminal width, 120)` 的可读宽度并居中，X/Y/Width/Height 均非负，区域高度总和精确等于终端高度，使用减法式边界断言证明极端整数下也不越界。80×24 为 full，79×24 与 80×23 分别证明任一维低于基线都会进入 compact；240×40 宽屏保留主内容、菜单、确认、输入和状态区域，compact 将输入限制为最多 3 行，面板不足时按确定优先级收缩，列表页则只分配主列表与状态。测试还冻结面板开关独立性、聊天输入最少一行、列表忽略聊天 flags、重复调用确定性，以及负数、单轴为零、1×1、极窄、极矮、最大 int 输入行数和最大 int 终端尺寸。规定命令 exit 0，为 10 run/11 pass、无 fail/skip；同根 `-race -count=20` 为 200 run/201 pass、无 fail/skip/race；`internal/tui` 全量普通与 race 回归均 exit 0。`go vet ./internal/tui`、`git diff --check` 与两个批准 Go 文件的 `gofmt -l` 均 exit 0 且无输出。按批准边界，本任务只交付可重复调用的统一布局纯函数；消息滚动、长输入组件及其锚点迁移留给 T4.14，列表、状态和命令菜单消费统一 Layout 留给 T4.15，确认面板内容留给 T4.16，真实 `tea.WindowSizeMsg` 优先分发与连续 resize 接线留给 T4.19。

### T4.14 迁移消息与输入区域（已完成，2026-08-03）

**文件：** `internal/tui/messages.go`, `internal/tui/messages_test.go`, `internal/tui/input.go`, `internal/tui/layout_test.go`
**依赖：** T4.13
**步骤：**
1. 消息 viewport 支持长会话可靠滚动；追加消息、切换会话和 resize 时按明确规则保持或重置锚点。
2. 长输入动态增高但不能挤掉确认、错误或状态区域；compact 模式仍可编辑和提交。

**验证：** `go test -json -count=1 ./internal/tui -run TestLongMessagesAndInputRemainOperable` 通过。
**覆盖：** F27 / AC27。
**完成记录（2026-08-03）：** `MessagesView` 接入 Bubbles viewport 并只消费统一 Layout 的 Main Region，渲染时按区域宽度对 ANSI 和宽字符安全换行；首次加载与 `SetMessages` 会话切换重置到底部，位于底部时追加、工具更新和 resize 继续追尾，用户上翻后同会话变更及 resize 保持数值 offset，PageUp/Home 可回看且 End 可重新回到最新消息。原 `[]conversation.Message` 缓存已收敛为 TUI 自有安全渲染条目，Conversation/ToolState 只在 `SetMessages` 边界投影；消息、transient trace、ToolDisplay artifact 与值复制后的工具更新均采用隔离或 copy-on-write，不反向别名源领域对象或旧 App 模型。输入组件由单行 textinput 迁移为可软换行 textarea，保留 4000 字符上限、focus/blur、SetValue/Clear/Value 与 App 现有 Enter 提交契约；`VisualLineCount` 按 Unicode 终端列宽和硬换行计算动态行数且避免整数溢出，`SetRegion` 消费统一 Input Region，full 最多 8 行、compact 最多 3 行，确认、状态与主内容仍有可用区域，40×12 compact 和多行草稿均可继续编辑并取回完整值，零/负尺寸隐藏且不 panic。规定命令 exit 0，为 4 run/5 pass、无 fail/skip；同根 `-race -count=20` 为 80 run/81 pass、无 fail/skip/race；值复制工具更新专项 `-race -count=20` 为 20 run/21 pass。最新共享状态的 `internal/tui ./internal/app` 普通与完整 `-race -count=1` 回归均 exit 0，各为 184 run/186 pass、无 fail/skip/race。`go vet ./internal/app ./internal/tui`、`git diff --check` 与四个批准 Go 文件的 `gofmt -l` 均 exit 0 且无输出。按批准边界，列表、状态和命令菜单消费统一 Layout 留给 T4.15，确认面板留给 T4.16，App 对 `tea.WindowSizeMsg` 的优先分发及真实运行期 resize 接线留给 T4.19。

### T4.15 迁移列表、状态与命令菜单（已完成，2026-08-03）

**文件：** `internal/tui/list.go`, `internal/tui/status.go`, `internal/tui/status_test.go`, `internal/tui/command_menu.go`, `internal/tui/command_menu_test.go`, `internal/tui/layout_test.go`
**依赖：** T4.13
**步骤：**
1. 会话列表、状态栏和命令菜单只使用统一 Layout，不自行假设终端宽高。
2. 动态缩放时保持选中项、菜单焦点和关键状态可见；空列表、placeholder 与截断提示可区分。

**验证：** `go test -json -count=1 ./internal/tui -run TestListStatusAndMenuResizeWithoutStateLoss` 通过。
**覆盖：** F21、F27 / AC21、AC27。
**完成记录（2026-08-03）：** 会话列表新增直接消费纯 `SessionListView` 的构造入口及统一 `ApplyConversationListLayout`，初始尺寸也由 baseline Layout 计算，不再自行写死 80×20；resize 只调用 Bubbles `SetSize` 而不重建 model，连续 wide→compact→baseline 缩放保持 selected session、过滤和分页状态。入口会扣除 Bubbles 自身 Title/Status/Pagination/Help 水平 frame，修复 40 列区域实际渲染 48 列的越界；compact 隐藏具有 48 列最小宽度的 help，列表整体始终不超过 Main Region。纯列表项将 recovery status 和更新时间收敛为本地 string 与 Unix milliseconds，不缓存 `conversation.RecoveryStatus` 或 `time.Time`；legacy Conversation 输入只在兼容构造边界投影。标题明确区分“暂无历史会话”和“结果已截断”并显示安全扫描通知，partial 描述显示“已部分恢复”及恢复提示，placeholder 显示“恢复占位 · 不可用”且 `Selectable=false`。Status 设置统一 Status Region 后按错误、确认/流式、停止原因、notice 的关键优先级限宽渲染，再显示 mode/provider/model 与 usage 等次要信息；未设置区域时保持旧输出兼容。CommandMenu 设置 CommandMenu Region 后只渲染能包含当前 selected item 的有界窗口，长描述按 ANSI/Unicode 列宽截断，连续 resize 不改变焦点。规定命令与同根精确 `-race -count=1` 均为 4 run/5 pass、无 fail/skip/race；`internal/tui` 全量回归为 54 run/55 pass、无 fail/skip。列表缓存类型收敛只需在一个既有 App 测试中加入显式 string 比较，生产 App 无改动；`internal/app` 与 `internal/tui` 普通及完整 `-race -count=1` 回归均为 184 run/186 pass、无 fail/skip/race。全仓 `go test -json -count=1 ./...` exit 0，为 2667 run/2701 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip；`go vet ./internal/app ./internal/tui`、`git diff --check` 与相关 Go 文件 `gofmt -l` 均 exit 0 且无输出。按批准边界，完整确认面板留给 T4.16，App 对真实 `tea.WindowSizeMsg` 的统一区域接线留给 T4.19。

### T4.16 实现完整确认面板（已完成，2026-08-03）

**文件：** `internal/events/events.go`, `internal/events/events_test.go`, `internal/orchestrator/chat.go`, `internal/orchestrator/chat_test.go`, `internal/app/state.go`, `internal/app/events.go`, `internal/app/app.go`, `internal/app/state_test.go`, `internal/tui/view_model.go`, `internal/tui/view_model_test.go`, `internal/tui/confirmation.go`, `internal/tui/confirmation_test.go`
**依赖：** T2.14、T4.13
**步骤：**
1. 将安全确认模型已有的实际目标、可用授权范围、规则落点和撤销方式通过 Events、App 状态与纯 TUI ViewModel 逐层投影；所有文本保持 `SafeText`，slice 采用 defensive copy，不允许 TUI 解析 Prompt、猜测路径或持有权限能力。
2. 确认面板显示实际目标、风险、授权范围、规则落点和撤销方式，并在渲染前中和 ANSI/OSC/C0 终端控制序列；任何尺寸下不得越过统一 Confirmation Region。
3. 只有确认模型明确允许时才显示 permanent 操作；含凭据或 degraded 状态时不得提供永久授权选项，永久决策仍由 Authorizer 二次校验。

**范围修订（2026-08-03，已批准）：** 原两文件边界无法满足“实际目标、规则落点、授权范围均来自安全确认模型”，因为 `ConfirmationPrompt.Target/Scopes/RuleLocation` 在 Orchestrator → Events → App → TUI 投影中丢失。本修订只补齐既有安全字段的只读传递与测试，不改变 Authorizer 决策、App 按键语义或 T4.19 的运行期布局接线。

**验证：** `go test -json -count=1 ./internal/tui -run TestConfirmationShowsCancellableAuthorizationScope` 通过。
**覆盖：** F5、F6、F27 / AC5、AC6、AC27。
**完成记录（2026-08-03）：** 既有安全确认模型的 `Target`、结构化 `Scopes` 与 `RuleLocation` 已经由 Orchestrator 投影到只含 `SafeText`/稳定标量的 Events DTO，再经 App `ConfirmationState` 和纯 TUI `ConfirmationView` 逐层复制；每个 slice 边界及 ViewModel getter 均 defensive copy，并保持 nil 与显式空 slice 的区别。Orchestrator 对新增文本字段和 scope description 再执行运行时脱敏，固定 canary 未进入任何安全确认字段。确认面板只读取结构化 `Target`、风险、模式、scope availability、规则预览、规则落点、警告和撤销提示，不解析 `Prompt`、不猜测路径或“不适用”值，也不持有权限或决策能力；渲染前剥离 ANSI/OSC 并中和 C0/C1/DEL，随后按 Unicode 终端宽度截断，40 列 compact、120 列宽屏、零/负区域均不越过统一 Confirmation Region，极矮区域始终保留拒绝和取消。`p永久` 只由 `AllowPermanent` 明确开启；含凭据或 degraded 且模型禁止永久授权时只显示本次、会话、拒绝和取消，实际永久决策仍由 Authorizer 二次校验。规定命令及同根精确 `-race -count=1` 均 exit 0，各为 5 run/6 pass、无 fail/skip/race；Events、Orchestrator、App、TUI 四包普通与完整 race 回归均 exit 0，各为 432 run/436 pass、无 fail/skip/race。全仓 `go test -json -count=1 ./...` exit 0，为 2677 run/2711 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip；`go vet ./internal/events ./internal/orchestrator ./internal/app ./internal/tui`、`git diff --check` 与 12 个任务相关 Go 文件的 `gofmt -l` 均 exit 0 且无输出。按批准边界，本任务不改变 Authorizer 决策和 App 按键语义，真实 `tea.WindowSizeMsg` 的运行期布局接线仍留给 T4.19；下一任务为 T4.17。

### T4.17 实现本地 Artifact 用户入口（已完成，2026-08-03）

**文件：** `internal/tui/artifact.go`, `internal/tui/artifact_test.go`, `internal/tui/messages.go`, `internal/tui/messages_test.go`, `internal/app/deps.go`, `internal/app/app.go`, `internal/app/update.go`, `internal/app/update_test.go`, `internal/app/commands.go`, `internal/app/commands_test.go`, `internal/app/command_controller.go`, `internal/command/controller.go`, `internal/command/builtins.go`, `internal/command/builtins_test.go`
**依赖：** T4.12、T1.25、T3.19a
**步骤：**
1. Artifact intent 只能由 App 调用窄 `OpenForUser` 接口；TUI、模型工具和 ViewModel 均不得持有 Store 或 artifact reader。
2. 常规界面只显示 ID、字节数、Available/Complete、截断原因和安全错误，不显示真实路径或把 raw 内容写入事件、会话与 memory。
3. 通过 command metadata 注册正式 `/artifact <opaque-id>` 用户动作；命令只生成纯值 intent，App 在收到显式 intent 后才调用一次窄 reader，禁止在 `commands.go` 特判输入或维护第二份命令表。
4. App 对用户页执行有界读取、运行时脱敏和终端控制字符中和，并在成功、失败、取消和关闭路径释放 reader；TUI 只接收安全页与 opaque metadata，不接收 raw、真实路径、reader、Store 或回调能力。
5. 现有工具结果行使用统一 Artifact metadata 渲染并提供正式入口提示；最终生产 Artifact Store 的创建、唯一 owner 与组装注入仍留给 T4.25b、T4.25f 和 T4.29a，不在本任务提前建立第二组装根。

**范围修订（2026-08-03，已批准）：** 原四文件边界只能产生无生产调用点的 `ArtifactOpenIntent`、formatter 与包内 helper，无法由真实命令、App Update 或现有工具结果行触发。本修订只补齐 command metadata → 纯值 intent → App 窄 `OpenForUser` → 有界安全用户页以及常规 metadata 展示链；不向 TUI/ViewModel/模型暴露 reader、Store、raw 或路径，也不提前创建或注入生产 Artifact Store。

**验证：** `go test -json -count=1 ./internal/app ./internal/tui ./internal/command -run '^TestArtifactOpenIsLocalUserOnly$'` 通过。
**覆盖：** F7、F10 / AC7、AC10。

**完成记录（2026-08-03）：** 唯一 `command.Builtins()` metadata 已正式注册 `/artifact <opaque-id>`；handler 只接受一个严格的 64 位小写十六进制 ID，并经窄 `command.Controller.OpenArtifact` 生成只含私有 ID/offset 的纯值 `ArtifactOpenIntent`，不接触 Store、reader、raw、路径或 Provider。App 只有在 `Update` 收到该显式本地 intent 后才调用唯一生产 `ArtifactUserReader.OpenForUser` 调用点；每页使用 16 KiB+1 字节有界探测，优先 `Seek` 深分页，否则以有界内存跳过 offset，所有成功、失败、取消、Esc、退出和 `Model.Close` 路径均由单次关闭 lease 释放 reader。原始页只作为函数局部值存在，经 `RuntimeRedactor` 脱敏后才进入 capability-free `ArtifactView`；request ID、offset 与单调 generation 共同拒绝迟到结果。TUI 进一步中和 ANSI/OSC/C0/C1/DEL 并限制 metadata、错误、页面行数和宽度；常规工具结果行只显示 ID、字节数、Available/Unavailable、Complete/Incomplete、截断原因和正式 open 提示，不携带用户页。精确测试普通为 37 run/40 pass，`-race -count=20` 为 740 run/743 pass，均无 fail/skip/race；App、TUI、Command 三包普通与完整 race 回归均为 268 run/271 pass，均无 fail/skip/race。全仓 `go test -json -count=1 ./...` exit 0，为 2720 run/2754 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip；`go vet ./internal/app ./internal/tui ./internal/command`、`git diff --check` 与 14 个批准 Go 文件的 `gofmt -l` 均 exit 0 且无输出。按批准边界，本任务未新增模型工具，也未创建或注入生产 Artifact Store；唯一 owner 与生产组装注入仍留给 T4.25b、T4.25f、T4.29a，当前未注入环境对 `/artifact` 安全返回 unavailable。下一任务为 T4.18。

### T4.18 生成统一帮助视图（已完成，2026-08-03）

**文件：** `internal/tui/help.go`, `internal/tui/help_test.go`, `internal/command/builtins.go`, `internal/command/registry.go`, `internal/command/registry_test.go`, `internal/app/commands.go`, `internal/app/commands_test.go`, `README.md`
**依赖：** T4.4、T4.11、T4.16、T4.17
**步骤：**
1. 从 command metadata 生成正式命令、别名、上下文快捷键、权限模式和状态/诊断入口，不维护第二份手写命令表。
2. 明确公开 `/status` 承担状态与有界、脱敏、可行动的诊断入口；隐藏 `/diagnostics` 只保留兼容，不进入 help、completion 或正式命令表。
3. `/new`、`/sessions`、`/list`、聊天 `Esc` 与列表 `Enter`/`n`/`q` 全部可发现；同步 README 公开命令/按键表，隐藏兼容命令和内部 launcher 不出现。

**验证：** `go test -json -count=1 ./internal/tui ./internal/command ./internal/app -run 'TestHelpMatchesC9PublicMetadata|TestHelpAndREADMEMatchPublicMetadata|TestStatusIsPublicDiagnosticsEntry|TestStatusOutputsBoundedActionableDiagnostics'` 通过。
**覆盖：** F25、F29 / AC25、AC29。

**完成记录（2026-08-03）：** Registry 新增 defensive-copy 的 capability-free `HelpCatalog`，从唯一 command metadata 投影全部正式命令、别名、类型、用法、参数、Badge、C9 上下文快捷键、权限模式以及公开状态/诊断入口；隐藏兼容定义不进入目录。App 的 `/help` 通过可选展示边界把目录映射到纯 TUI `HelpView`，统一视图只持有复制后的字符串值，渲染前剥离 ANSI/OSC 并中和 C0/C1/DEL；动态 Skill 仍由同一目录自动出现，并保留既有 Badge 紧邻命令名的稳定格式。公开 `/status` 同时输出运行状态、recent error 与本地诊断收集器的脱敏摘要，各文本字段及总输出均按 UTF-8 边界截断，最终输出不超过 2048 bytes，并提供根据错误和诊断检查配置或连接后重试的行动提示；隐藏 `/diagnostics` 继续兼容分发，但不进入 help、completion、README 正式命令表或公开诊断所有权。README 已与目录同步 12 个当前正式内建命令、别名、6 个 C9 上下文按键、3 种权限模式和 `/status` 双重入口，不再列出隐藏 launcher、兼容命令或并非固定内建的 Skill 示例。规定命令及同根精确 `-race -count=1` 均 exit 0，各为 4 run/7 pass、0 fail/skip/race；TUI、Command、App 三包普通与完整 race 回归均 exit 0，各为 261 run/264 pass、0 fail/skip/race；全仓 `go test -json -count=1 ./...` exit 0，为 2713 run/2747 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip；`go vet ./internal/tui ./internal/command ./internal/app`、`git diff --check` 与 7 个任务相关 Go 文件的 `gofmt -l` 均 exit 0 且无输出。最终 AC25/AC29 仍等待后续导航 E2E 和文档治理汇合，不在本任务提前勾选；下一任务为 T4.19。

### T4.19 优先处理终端尺寸消息（已完成，2026-08-03）

**文件：** `internal/app/update.go`, `internal/app/update_test.go`, `internal/tui/program.go`, `internal/tui/program_test.go`
**依赖：** T4.13–T4.18
**步骤：**
1. 在 screen、key、命令和 intent 分发前处理 `tea.WindowSizeMsg`，保存真实终端尺寸。
2. 重新计算 Layout 后再渲染当前消息、列表、菜单或确认面板；不得伪造默认尺寸覆盖运行中 resize。

**验证：** `go test -json -count=1 ./internal/app ./internal/tui -run TestWindowSizePrecedesInputDispatch` 通过。
**覆盖：** F27 / AC27。

**完成记录（2026-08-03）：** App 在 `Update` 顶层先于 Artifact、按键、命令和 intent 消费 `tea.WindowSizeMsg`，以唯一 `tui.ComputeLayout` 重新计算区域并同步应用到 Status、Conversation List、Messages、Input、Command Menu 与结构化 Confirmation Panel。TUI Program 通过私有响应式包装模型只保存 Bubble Tea 实际上报的最后尺寸：首次尺寸消息到达前不伪造 80×24，后续消息分发前后均重放真实尺寸，使菜单、确认等状态变化后立即使用当前终端尺寸重新布局；`Run` 返回前解包包装器，保留最终 App Model 的资源关闭能力。确认内容继续只通过安全 ViewModel 和 `ConfirmationPanel` 渲染，不解析旧 Prompt。测试先行阶段分别观察到 TUI 因响应式包装器尚未实现而构建失败，以及 App 未应用尺寸时输入高度仍为 1 的行为失败；实现后规定命令与同根精确 `-race -count=1` 均 exit 0，各为 2 run/4 pass、0 fail/skip/race。App/TUI 分层普通与完整 race 回归均 exit 0，各为 229 run/231 pass、0 fail/skip/race；全仓 `go test -json -count=1 ./...` exit 0，为 2720 run/2754 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip。`go vet ./internal/app ./internal/tui`、`git diff --check` 与四个任务文件的 `gofmt -l` 均 exit 0 且无输出。最终 AC27 仍等待后续完整 UI/E2E 汇合，不在本任务提前勾选；下一任务为 T4.20。

### T4.20 使计时配置与状态 reset 真实生效（已完成，2026-08-03）

**文件：** `internal/app/app.go`, `internal/app/state.go`, `internal/app/state_test.go`, `internal/app/skills.go`, `internal/app/update.go`, `internal/app/update_test.go`, `internal/tui/status.go`, `internal/tui/status_test.go`
**依赖：** T4.2、T4.12、T2.7
**步骤：**
1. `ui.show_response_timer=false` 时 App 不生成计时 ViewModel，显式 false 不被默认值覆盖。
2. `ui.start_mode` 从最终 resolved Config 决定初始 screen/mode，不允许 App/TUI 重新解释默认值。
3. 新请求和切换会话时按 T4.2 清除旧 duration、usage、cache、stop reason、错误和 confirmation。

**范围修订（2026-08-03，已批准）：** 原五文件边界只能验证三层纯状态的 reset，无法修改实际新请求入口 `beginRequest`，也无法阻止 `EventDone` 在关闭计时时继续发布 Duration。本修订只增加 `internal/app/skills.go`、`internal/app/update.go` 与 `internal/app/update_test.go`，用于让真实请求生命周期调用统一 reset、让最终 resolved `ui.show_response_timer` 控制计时发布，并从实际 Bubble Tea 事件路径验证；不修改 Config Resolve、导航事务、Provider、TUI ViewModel 数据结构或生产组装。

**验证：** `go test -json -count=1 ./internal/app ./internal/tui -run 'TestTimerAndUsageRespectResetAndConfig|TestStartModeFollowsResolvedConfig'` 通过。
**覆盖：** F26、F28 / AC26、AC28。

**完成记录（2026-08-03）：** App 进程态原样保存最终 Config 中的 `ShowResponseTimer` 与 `StartMode`，不根据零值推断 presence、不补第二份默认值，也不在 App/TUI 重复维护配置校验；`list` 直接进入列表，`new` 只有在可信列表读取与新会话 Create 成功后才发布 chat。App→TUI ViewModel 在计时关闭时不投影 duration，`EventDone` 也保持零值，但 token 与 cache usage 仍完整保留；`tui.Status` 的常规和响应式渲染都只在已解析开关开启且 duration 为正时显示耗时。新增唯一请求展示 reset，在覆盖旧 `RequestSession` 前清除 duration、token、cache、停止原因/消息、错误、确认、请求模型、流式/迭代状态、request transient 以及 assistant/thinking/tool/independent TUI 缓冲；新请求与会话切换共用该边界，保留 Provider、模型、MCP、resolved 配置及应保留的会话状态。已同步写入权威 Conversation 的用户消息经私有标记只显示一次，旧 generation 的 Done、Usage、Progress、Error、Confirmation 与 transient 事件均无法消费标记或回填；`/clear` 后的新请求只显示当前新消息，`MainTraceReset` 不恢复被显式隐藏的旧历史，显式重载会话仍按既有契约恢复。精确测试普通为 13 run/15 pass，`-race -count=20` 为 260 run/262 pass，均无 fail/skip/race；App/TUI 完整普通与 race 回归均为 242 run/244 pass，均无 fail/skip/race。全仓 `go test -json -count=1 ./...` exit 0，为 2747 run/2781 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip；`go vet ./internal/app ./internal/tui`、`git diff --check` 与 8 个批准 Go 文件的 `gofmt -l` 均 exit 0 且无输出。最终 AC26 仍等待后续完整导航/E2E 汇合，AC28 仍等待完整配置矩阵与组装根汇合，均不在本任务提前勾选；下一任务为 T4.21。

### T4.21 前置解析 CLI help 与 version（已完成，2026-08-03）

**文件：** `cmd/xagent/cli.go`, `cmd/xagent/main.go`, `cmd/xagent/cli_test.go`
**依赖：** T2.7、T3.27
**步骤：**
1. `--help`、`-h` 和 `--version` 在读取配置、展开环境变量、联网、打开会话或启动子进程前完成。
2. `--help`/`-h` 将同一有效用法写 stdout 并返回 0；`--version` 将版本写 stdout 并返回 0，stderr 为空。
3. 未知参数、缺失参数和非法 config 参数只向 stderr 写安全错误/用法摘要并返回非零，stdout 不伪装成功输出。

**验证：** `go test -json -count=1 ./cmd/xagent -run 'TestHelpAndVersionRequireNoRuntimeInitialization|TestPublicCLIOutputAndExitContract'` 通过。
**覆盖：** F7、F30 / AC7、AC30。

**完成记录（2026-08-03）：** 新增独立公共 CLI 解析层，`main` 只把 `os.Args[1:]`、stdout/stderr 和正常运行 callback 交给该层并按其 0/1/2 退出码结束；`--help`、`-h` 和 `--version` 必须单独使用，均在调用正常运行 callback、内部 launcher 检查、配置读取、环境/路径访问、网络客户端、会话、Hook/MCP/Provider 或 TUI 初始化前完成。两种 help 输出逐字相同的稳定 Usage 与 config/version 入口，version 当前输出非空 `xagent dev` 开发标识，可追溯构建信息按批准边界留给 T4.22。正常启动只接收 nil 或规范化为 `--config <path>` 的参数；`--config`/`-config`、等号形式与默认路径保持兼容，空值、缺值、重复值、未知参数、位置参数以及和 help/version 混用均在 runtime 前拒绝。usage 错误固定只向 stderr 写不含原参数的安全摘要和 Usage、返回 2，stdout 为空；运行失败只向 stderr 输出并返回 1，成功路径返回 0。旧 `parseConfigPath` 复用同一解析器，隐藏 launcher 仍不能伪装成普通参数。测试先行先取得 `runCLI` 与退出码契约缺失的构建失败；实现后规定普通与同根精确 `-race -count=1` 均 exit 0，各为 14 run/15 pass、0 fail/skip/race，`cmd/xagent` 完整普通与 race 回归均 exit 0，各为 27 run/28 pass、0 fail/skip/race。全仓 `go test -json -count=1 ./...` exit 0，为 2746 run/2780 pass、0 fail，只有 `internal/resources` 与 `internal/testutil/cmd/fakeprovider` 两个无测试包的 package-level skip。`go vet ./cmd/xagent`、`git diff --check` 与三个任务文件的 `gofmt -l` 均 exit 0 且无输出。最终 AC30 仍等待 T4.22 的可追溯版本信息及后续 CLI E2E 汇合，不在本任务提前勾选；下一任务为 T4.22。

### T4.22 提供可追溯版本信息（已完成，2026-08-03）

**文件：** `cmd/xagent/version.go`, `cmd/xagent/cli.go`, `cmd/xagent/cli_test.go`
**依赖：** T4.21
**步骤：**
1. 支持 ldflags 注入版本、revision 和构建时间，并以稳定格式输出。
2. 缺省时读取 Go build info/VCS；信息不完整仍输出非空开发版本，不回显本地敏感路径。

**范围修订（2026-08-03，已批准）：** 原两文件边界可实现版本解析与测试，但不能把 T4.21 的临时开发版本输出接入真实 `--version` 调用链。本修订只增加 `cmd/xagent/cli.go`，用于把既有 `cliVersion` 分支的一处临时输出替换为 `versionOutput()`；不修改参数识别、help、正常启动、runtime 初始化顺序或退出码契约。

**验证：** `go test -json -count=1 ./cmd/xagent -run 'TestVersionUsesInjectedTraceableBuildInfo|TestVersionUsesBuildInfoFallback'` 通过。
**覆盖：** F7、F30 / AC7、AC30。

**完成记录（2026-08-03）：** 新增唯一版本解析与格式化入口，发布构建可通过 `-ldflags '-X main.version=... -X main.revision=... -X main.buildTime=...'` 注入版本、revision 和 RFC3339 构建时间；有效注入优先于 Go build info，构建时间统一转换为 UTC。输出固定为 `xagent version=<...> revision=<...> build_time=<...> modified=<true|false|unknown>` 单行格式。缺少注入时只从 `debug.ReadBuildInfo` 白名单读取 `Main.Version`、`vcs.revision`、`vcs.time` 与 `vcs.modified`，不读取或输出 main/dependency/replace 模块路径及其他 settings。版本只接受有界 ASCII 安全标识，revision 只接受规范化的 40/64 位十六进制 OID；缺失、不完整、非法或含路径内容的元数据分别回退为 `dev`、`unknown`。`vcs.time` 与 `vcs.modified` 只在最终 revision 与 build-info revision 相同时回填，避免把异源 VCS 元数据错误绑定到注入 revision。T4.21 的临时常量通过已批准范围修订在 `cmd/xagent/cli.go` 以一处最小接线替换为该入口，`--version` 仍在 runtime 初始化前成功返回。精确普通测试为 5 run/6 pass；精确 `-race -count=20` 为 100 run/101 pass；均为 0 fail/skip/race。当前 `cmd/xagent` 完整普通与 race 回归均为 36 run/37 pass、0 fail/skip/race；全仓 `go test -json -count=1 ./...` 为 2756 run/2790 pass、0 fail，只有两个无测试包的 package-level skip。真实 ldflags 命令 exit 0，输出注入的 `v2.3.4`、40 位 revision、UTC `2026-08-03T10:30:00Z` 和同源安全的 `modified=unknown`；无注入命令 exit 0，输出 `dev/unknown` 且无本地路径。`go vet ./cmd/xagent`、任务文件 `gofmt -l` 与 `git diff --check` 均 exit 0 且无输出。最终 AC30 仍等待内部 launcher 隔离及后续 CLI E2E 汇合，不在本任务提前勾选；下一任务为 T4.23。

### T4.23 隐藏并约束内部 launcher 模式（已完成，2026-08-03）

**文件：** `cmd/xagent/internal_mode.go`, `cmd/xagent/internal_mode_test.go`, `cmd/xagent/cli_test.go`
**依赖：** T4.21、T1.21
**步骤：**
1. public help、completion 和错误建议均不显示内部 launcher 模式。
2. 内部模式只接受父进程继承句柄中的版本化 ProtectionPlan，拒绝普通 CLI 参数、环境变量或 stdin 构造计划。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestInternalLauncherCannotBeEnteredFromPublicCLI` 通过。
**覆盖：** F4、F30、F31 / AC4、AC30、AC31。

**完成记录（2026-08-03）：** `cmd/xagent` 的内部模式入口保持无调用参数、无环境 plan payload、无 stdin 的窄委托；Linux 平台只用固定 marker 选择模式，版本化 plan 仍只能由 T1.21/T1.21a 的 `internal/proctree` 从固定 sealed inherited handle 读取并完成版本、预算、握手与 scratch identity 校验，CLI 层不复制协议或增加第二来源。公共 parser 对伪内部 flag、位置参数和伪 completion 请求均在 runtime 前返回通用 usage，help 与错误不披露 `internal`、`launcher`、继承 marker、plan handle 或握手细节。修复了伪造 Linux marker 时底层 launcher 错误经公共 stderr 泄露协议术语的问题：平台内部错误现在统一映射为固定的“受保护进程启动失败”，保留 fail-closed 的 exit 1。隔离 helper 将 argv 缩为仅程序名、设置精确 marker 且不传 ExtraFiles，并同时把 canary 放入环境变量与 stdin；实际 `runCLI → runArgs → runInternalMode` 证明二者均不能替代 FD3/FD4，stdin canary 完整未消费，错误输出无 canary 或隐藏协议词。精确普通测试为 4 run/5 pass；精确 `-race -count=20` 为 80 run/81 pass；均为 0 fail/skip/race。当前 `cmd/xagent` 完整普通与 race 回归均为 37 run/38 pass、0 fail/skip/race；Darwin、Linux、Windows amd64 测试二进制均交叉编译成功。全仓 `go test -json -count=1 ./...` 为 2757 run/2791 pass、0 fail，只有两个无测试包的 package-level skip；`go vet ./cmd/xagent`、`git diff --check` 与三个任务相关 Go 文件的 `gofmt -l` 均 exit 0 且无输出。最终 AC4/AC30/AC31 仍等待生产组装、三平台原生与 CLI/E2E 汇合，不在本任务提前勾选；下一任务为 T4.24。

### T4.24 解析 C8 Artifact 根（已完成，2026-08-03）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.23、T3.27、T1.11、T1.25–T1.28、T2.7、T3.17
**步骤：**
1. 未设置 `artifact.root` 时使用 OS 用户缓存目录 `xagent/artifacts/<project-id>`；project-id 为项目 Root 稳定身份的 SHA-256，路径不含明文项目路径。
2. 显式 root 必须在工作区外、具备私有权限且不能经链接返回工作区；默认目录不可用时启动失败，不回退到项目目录。

**范围修订（2026-08-03，已批准）：** 原两文件边界无法读取 C8 已批准的公开键 `artifact.root`，因为当前 presence-aware、resolved Config 均缺少该字段；同时跨平台私有权限和链接校验已由 Artifact owner 的平台实现掌握，不应在 Assembly 复制。修订仅增加 `internal/config/config.go`、`internal/config/partial.go`、`internal/config/resolve.go`，用于让严格 decode、四层 merge 和唯一 Resolve 承载该字符串；增加 `internal/artifact/api.go`、`internal/artifact/file_store.go`、`internal/artifact/file_store_test.go`，用于提供 Assembly 唯一调用的根准备/校验入口并复用既有平台私有权限实现。根的选择、稳定 project-id 和禁止回退仍只实现在原定 `cmd/xagent/assembly.go` 与 `cmd/xagent/assembly_test.go`；不创建 Store、不接线生产组装、不修改数值预算、配置优先级或其他公开键。

**第二次范围修订（2026-08-03，已批准）：** 审计证明仅做 `EvalSymlinks → 创建 → 复检` 仍允许路径/已打开 Root 身份错配、大小写或 reparse 别名以及检查后祖先替换，随后 Store 的绝对路径 create/open/rename/remove 可能返回工作区。修订增加 `internal/artifact/writer.go`、`internal/artifact/cleanup.go`、`internal/artifact/private_posix.go`、`internal/artifact/private_windows.go`、`internal/artifact/private_unsupported.go`、`internal/artifact/private_posix_test.go` 与 `internal/artifact/private_windows_test.go`：Artifact owner 持有经对象身份验证的目录句柄，所有载荷 I/O 均改为 handle-relative 且 no-follow/reparse-safe；Assembly 在产生 project-id 或创建目录前验证 `RuntimePaths.ProjectRoot` 与传入 `safefs.Root` 为同一对象。不得把平台句柄、绝对路径或 payload 暴露到 Ref、错误、配置或 UI，也不得改变 Artifact 预算、保留策略及一次性终结语义。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestArtifactRootFollowsC8` 通过。
**覆盖：** F7、F10、F28 / AC7、AC10、AC28。

**完成记录（2026-08-03）：** presence-aware Config 已承载 `artifact.root` 的严格解码、固定优先级合并与唯一 Resolve；未配置时使用 OS 用户缓存目录及项目 Root 稳定身份 SHA-256，显式根与默认根均禁止回退到工作区。Assembly 在生成 project-id 或创建目录前重新打开声明路径并与传入 `safefs.Root` 比较版本化对象身份，错配失败无目录副作用且错误不泄露路径。Artifact Store 在 POSIX 以逐段 `openat/mkdirat + O_NOFOLLOW`、在 Windows 以 `NtCreateFile + RootDirectory + reparse-safe` 打开并持有经对象身份和私有权限验证的根句柄；create/open/rename/remove 全部改为句柄相对操作，祖先路径替换后仍绑定原目录，不进入 workspace；预算、保留策略、Ref 与 Writer 一次性终结语义保持不变。规定普通验证为 6 run/7 pass；Assembly 精确 `-race -count=20` 为 120 run/121 pass，Artifact 的句柄/链接/终结/清理专项 `-race -count=20` 也为 120 run/121 pass，均为 0 fail/skip/race（pass 均含 package-level pass）。`cmd/xagent` 完整普通与 race、`internal/config`+`internal/artifact` 完整 race、全仓 `go test -json -count=1 ./...` 均 exit 0；全仓结果为 2768 run/2802 pass、0 fail，只有两个无测试包的 package-level skip。`go vet ./cmd/xagent ./internal/artifact ./internal/config`、`git diff --check`、生产 Artifact 绝对路径 I/O 扫描及 15 个批准 Go 文件的 `gofmt -l` 均 exit 0 且无输出。Darwin、Linux、Windows amd64 的 Artifact 与 cmd 测试目标均通过交叉编译；Windows ACL/reparse 测试因当前为 Darwin 主机只完成编译，仍由后续原生 Windows CI 提供平台运行证据。最终 AC7/AC10/AC28 仍等待生产组装与跨平台交付汇合，不在本任务提前勾选；下一任务为 T4.25。

### T4.25 建立唯一生产组装根骨架（已完成，2026-08-03）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`, `cmd/xagent/lifecycle.go`, `cmd/xagent/lifecycle_test.go`, `cmd/xagent/main.go`, `cmd/xagent/main_test.go`
**依赖：** T4.3、T4.19、T4.20、T4.23、T4.24、T3.27
**步骤：**
1. 定义非导出的 `Assembly` pipeline 和唯一 ownership/close registry，按 Config、Security、Execution、Adapters、Orchestration、UI 六个显式阶段构造依赖图；每个阶段创建对象后立即登记 owner。
2. 在完整依赖图就绪前不切换 `main.go` 的生产入口；App、Orchestrator 和其他 leaf package 不得新增同级生产服务或 service locator。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestAssemblyPipelineHasSingleOwnershipRegistry` 通过。
**覆盖：** F28 / AC28。

**完成记录（2026-08-03）：** `cmd/xagent` 已建立非导出的候选 Assembly pipeline，按 Config、Security、Execution、Adapters、Orchestration、UI 六个不可由包级变量改写的固定阶段构造；pipeline 私有持有完成位与唯一 registry，builder 只能获得当前阶段的可撤销登记闭包，不能取得、替换或绕过 raw registry。builder 返回后 scope 立即失效，迟到登记会失败并阻止候选发布；每个 builder 返回后（包括最终 UI）都重新检查取消，因此阶段失败、取消、零 owner 或 scope 逃逸均不会发布半初始化 Runtime。registry 只保存关闭动作、不提供服务查询，完整依赖图完成后封存；六阶段 owner 以 UI→Orchestration→Adapters→Execution→Security→Config 的顺序幂等关闭。错误使用固定安全文本且不泄露构造错误；初始化失败后的独立 context 反向回滚仍按批准边界留给 T4.28。`main.go` 继续只走既有 `defaultStartupFactories` 生产入口，AST 测试确认未引用候选 Assembly、registry 或 `buildCandidate`，并扫描同包其他生产文件和 registry capability shape，因此没有提前执行 T4.29a 的原子切换或形成 service locator。针对性普通测试及 `-race -count=20`、`cmd/xagent` 完整普通与 race、全仓 `go test -json -count=1 ./...` 均 exit 0；Darwin、Linux、Windows amd64 的 `cmd/xagent` 测试二进制均交叉构建成功，`go vet ./...`、六个任务文件的 `gofmt -l` 与 `git diff --check` 均 exit 0 且无输出。最终 AC28 仍等待配置组装、生产切换及完整验收，不在本任务提前勾选；下一任务为 T4.25a。

### T4.25a 固定配置与秘密注册启动顺序（已完成，2026-08-03）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.25、T2.2–T2.7
**步骤：**
1. 先构造唯一 RuntimeRedactor，再严格执行 decode→merge→展开并注册秘密→resolve/hard-cap；配置错误直接返回经 Redactor 净化的安全错误。
2. Resolve 成功后才用最终 `diagnostics.max_*` 构造唯一 BoundedSink；任何错误都在创建网络 Client、打开会话或启动子进程前失败。

**验证：** `go test -json -count=1 ./cmd/xagent -run 'TestAssemblyRegistersSecretsBeforeExternalEffects|TestDiagnosticsUsesFinalResolvedLimitsOnce|TestInvalidConfigHasNoExternalSideEffect'` 通过。
**覆盖：** F7、F28 / AC7、AC28。

**完成记录（2026-08-03）：** Config 阶段现在首先创建唯一 RuntimeRedactor 并立即登记 owner，再对显式 user/project 文件执行严格 Decode、加入 runtime presence-aware layer 并按固定优先级 Merge；唯一 `ResolveConfig` 只展开最终生效值、原子注册 API key 与 MCP env/header 等秘密，并完成默认值、组合约束与 hard-cap。覆盖层 loser 不会被展开或注册，Security 及后续阶段首次运行时已能使用完整 resolved Config、同一 Redactor 与 diagnostics owner。只有 Resolve 成功后才按最终 `diagnostics.max_items/max_item_bytes/max_total_bytes` 创建并登记唯一 BoundedSink；同一已注册秘密的 Redactor 实际驱动该 Sink。decode、merge、resolve 或 sink 构造错误均转换为不拼接底层错误、配置标量、秘密或真实配置路径的固定步骤错误，候选 Runtime 不发布，网络、会话和进程等后续阶段调用次数为零；测试覆盖 unknown field、缺失有效环境变量、数值 hard-cap 和组合限制。三个规定根的针对性普通测试及 `-race -count=20`、T4.25/T4.25a 组合 race、`cmd/xagent` 完整普通与 race、全仓测试均 exit 0；Darwin、Linux、Windows amd64 的 `cmd/xagent` 测试二进制均交叉构建成功，`go vet ./...`、任务文件的 `gofmt -l` 与 `git diff --check` 均 exit 0 且无输出。最终 AC7/AC28 仍等待后续 owner 组装、生产切换和完整验收，不在本任务提前勾选；下一任务为 T4.25b。

### T4.25b 组装安全基础 owner（已完成，2026-08-03）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.25a、T4.24、T1.11、T1.18a、T1.25–T1.33
**步骤：**
1. 通过 safefs Bootstrap 构造项目/旧数据 Roots 和 C1 `Capabilities` 容器，再构造 Artifact Store、netpolicy ClientFactory、proctree Runner 与 ProtectionPlan factory。
2. Capabilities 只留在 Assembly；安全基础对象仅接收自己的窄 policy/options，不读取全局 Config。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestAssemblyKeepsRootCapabilitiesPrivate` 通过。
**覆盖：** F4、F8、F10、F11、F28 / AC4、AC8、AC10、AC11、AC28。

**完成记录（2026-08-03）：** Security 阶段已用 `tool.ProjectFilesystemPolicy` Bootstrap 项目 Root，并以独立空策略 Bootstrap 旧数据 Root；两者必须绑定不同对象。两份 C1 `safefs.Capabilities` 容器只保存在非导出的 Assembly build state，Artifact Store、netpolicy ClientFactory、proctree Runner、ProtectionPlanFactory 和候选 Runtime 均不含该容器。项目/旧数据 Root 创建后立即把 Close 登记到 T4.25 的唯一 registry；Artifact 根复用 T4.24 的工作区外解析与身份复验，并按最终 `artifact.max_*`、retention 构造唯一 FileStore 后立即登记 Close。网络基础只创建内部 Policy 和受控 ClientFactory，没有外部 transport/client 注入口；Runner 仅接收最终 cleanup timeout 与同一 BoundedSink。ProtectionPlanFactory 只持项目 Root 和用户缓存 scratch parent，旧数据 Root 保持为迁移专用读取边界，不暴露给 Bash、Hook 或 stdio MCP；各 owner 均在进入 Execution 阶段前登记。registry 关闭后项目/旧数据 Root 身份均失效，Artifact Store 也拒绝新 Begin；AST 门禁锁定唯一 Store/ProtectionPlanFactory 构造点和最小 protection roots。规定测试及专项 `-race -count=20`、`cmd/xagent` 完整普通与 race、全仓测试均 exit 0；Darwin、Linux、Windows amd64 的 `cmd/xagent` 测试二进制均交叉构建成功，`go vet ./...`、任务文件的 `gofmt -l` 与 `git diff --check` 均 exit 0 且无输出。最终 AC4/AC8/AC10/AC11/AC28 仍等待 capability 分发、生产切换、三平台原生与完整验收，不在本任务提前勾选；下一任务为 T4.25c。

### T4.25c 组装权限、工具与本地资源服务（已完成，2026-08-03）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.25b、T3.19a、T2.13、T2.17、T2.20、T2.24、T2.27、T2.30、T2.31
**步骤：**
1. 构造 Permission Health/Writer/TicketIssuer/Authorizer、Tool Registry/Executor、Instruction 与 Skill 服务；用唯一 RuntimeRedactor 创建唯一 ResultFactory，并用 T4.25b 的唯一 Artifact Store、resolved inline/capture limits 与 hard cap 创建签名等价于 `func(context.Context, artifact.Metadata) (*tool.Capture, error)` 的各内置 producer 窄闭包。每次闭包调用新建 operation-local Counter/Capture/Writer，不新增通用 CaptureFactory 类型、可替换 registry 或第二 Store owner，ResultFactory 不持有 Store。
2. 只向 Write/Edit 注入当前 Root 的 Ordinary，向 permission Writer 注入 Protected；Bash 只获得 ProtectionPlan factory，其他对象不得取得可写 capability。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestAssemblyDistributesCapabilitiesWithoutEscalation` 通过。
**覆盖：** F3–F7、F9–F13 / AC3–AC7、AC9–AC13。

**完成记录（2026-08-03）：** Execution 阶段已从唯一 RuntimeRedactor 创建唯一 ResultFactory，并把 T4.25b 的唯一 Artifact Store、最终 inline/capture limits 与不可配置 hard cap 收敛为未导出的窄 Capture 闭包；闭包只捕获 Store 和不可变数值边界，每次调用均新建 operation-local Counter、Capture 与 Writer。固定 safe candidate Registry 只注册 Read、Write、Edit、受保护 Bash、Glob、Grep 与 load-skill 七个安全 producer；Executor 与 Authorizer 共享同一 TicketAuthority，Health 是权限加载健康状态的唯一来源，原始 LoadErrors 不在 Authorizer 中形成第二份永久降级状态；用户权限、Instructions 与 Skills 均从显式 RuntimePaths 根加载，不回落到进程级 HOME/XDG。项目 Ordinary 只进入安全 Executor 的 Write/Edit 边界，Protected 只进入 permission Writer，Bash 只获得 Runner、ProtectionPlanFactory、项目工作目录、同一 ResultFactory 与 Capture 闭包；Execution state 不保留 Capabilities 容器或裸 capability。最终 Files/Tool 配置已覆盖 Executor 默认值，Instruction CachedLoader 与 Skill Manager 使用最终配置、固定根和已完成 Registry 快照。规定测试为 1 run/2 pass，专项 `-race -count=20` 为 20 run/21 pass；`cmd/xagent` 完整普通与 race 均为 62 run/63 pass，以上均为 0 fail/skip/race。全仓 `go test -json -count=1 ./...` 为 2787 run/2821 pass、0 fail，只有两个无测试包的 package-level skip；`go vet ./cmd/xagent`、相关文件 `gofmt -l` 与 `git diff --check` 均 exit 0 且无输出，Darwin/Linux/Windows amd64 的 `cmd/xagent` 应用均交叉构建成功。最终 AC3–AC7、AC9–AC13 与 I1/I2/I3/I7 仍等待后续 Adapter、Orchestrator、生产切换及完整验收，不在本任务提前勾选；下一任务为 T4.25d。

### T4.25d 组装 Provider、Hook、MCP 与 Store（已完成，2026-08-09）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.25c、T2.34、T2.45、T2.67、T2.69、T3.18
**步骤：**
1. 从最终配置构造 Provider、Hook Engine、MCP Manager 和 Conversation Store，不允许默认 Client、旧 Transport 或旧 Store fallback。
2. 把 T4.25c 的同一 ResultFactory 与 Capture 闭包注入 MCP；纯 synthetic Hook result adapter 只获得同一 ResultFactory，而不得获得 Capture/Store 能力。Hook 与 stdio MCP 只额外获得 ProtectionPlan factory，Migration 只获得 `LegacyArtifactImporter`，不得取得完整 Artifact Store 或裸旧路径访问。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestAssemblyUsesOnlyFinalProviderMCPAndStoreContracts` 通过。
**覆盖：** F4、F7、F8、F14–F20 / AC4、AC7、AC8、AC14–AC20、AC35。

**完成记录（2026-08-09）：** Assembly 已按最终 Resolve 配置组装 Provider、Hook Engine、纯 synthetic Hook result adapter、MCP Manager 与 Conversation v2 Store；Provider 统一接收 Assembly 创建的 RuntimeRedactor，MCP 与 Hook 共用最终 ResultFactory，MCP 仅通过窄 Capture 工厂取得每次调用独立的 Capture/Writer，Migration 仅通过 LegacyArtifactImporter 导入旧外部 artifact。Assembly 根测试使用真实本地 HTTP MCP fixture 连续调用工具两次，验证独立 artifact、最终 Tool/MCP/Session limits、legacy artifact 导入不改源文件、最终 session 目录及 Provider/Hook/MCP/Store/Migration 依赖流。HTTP JSON 与 stdio Transport 均在首字节链上完成增量解码和 Capture 路由；stdio 无法在读取 result 前安全解析 id 时 fail closed，HTTP SSE 在绑定 Capture 时明确 fail closed；Capture binding 在发送失败、取消、超时及连接关闭路径清理，Captured DTO 不携带原始文本、Raw 或结构化内容。修复 HTTP Receive 丢失 Captured 事件元数据的问题，并将 M3/T327 Canary 夹具迁移到 Capture-aware 调用接口。`go test -json -count=1 ./cmd/xagent -run '^TestAssemblyUsesOnlyFinalProviderMCPAndStoreContracts$'`、`go test -json -race -count=20 ./cmd/xagent -run '^TestAssemblyUsesOnlyFinalProviderMCPAndStoreContracts$'`、Provider/Hook/MCP/Protocol/Transport 专项测试及全仓 `go test -count=1 ./...` 均通过；`go vet ./cmd/xagent`、`git diff --check` 与相关 Assembly 文件 `gofmt -l` 均无输出。下一任务为 T4.25e（2026-08-09）。

### T4.25e 组装上下文与 Orchestrator（已完成，2026-08-09）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_context.go`, `cmd/xagent/assembly_test.go`, `cmd/xagent/assembly_t425e_test.go`, `internal/orchestrator/chat.go`, `internal/orchestrator/chat_test.go`
**依赖：** T4.25d、T3.19a、T3.20–T3.26、T4.4
**步骤：**
1. 构造 ContextManager、SessionContext、Prompt/Memory、command Registry 与唯一 Orchestrator，并只连接 C5 安全 DTO；注入 T3.19a 的 ProjectToolResult 通道，使 ResultFactory 四视图→ContextManager projection→Provider/Event/TUI/Hook/Conversation 的路径成为唯一生产扇出。
2. Orchestrator 只接收窄 Provider、Tool、Permission、Hook、MCP、Store 与事件接口，不接收 Assembly、Config 或 Capabilities。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestAssemblyInjectsNarrowOrchestratorDependencies` 通过。
**覆盖：** F3–F7、F10、F17–F24、F29 / AC3–AC7、AC10、AC17–AC24、AC29。

**完成记录（2026-08-09）：** Orchestration 阶段已从最终 Assembly graph 构造唯一 ContextManager、SessionContext、Memory、Prompt Resources、RequestBudgeter、SkillHistoryPolicy、command Registry 与 Orchestrator；Memory 路径只由显式 ProjectRoot/UserDataRoot 与最终配置解析，不读取进程 HOME/XDG，并拒绝相对 `../` 逃逸。Orchestrator 显式接收前序阶段的同一 Provider、Conversation Store、Tool Registry/Executor、Permission Authorizer、Hook Runtime、ContextManager、ResultFactory、SessionContext、Memory、Diagnostics、Skill Manager、RuntimeRedactor 与不可变策略/limits，不接收 Assembly、AppConfig、Capabilities、Root、Artifact Store、网络 owner 或 Capture/Reader/Writer；Assembly Authorizer、TicketAuthority、Executor verifier 与 Health 的身份关系在构造边界显式校验。注入 Registry 的只读模式从同一 Registry 派生不可变 view，兼容构造缺少 Registry 时才保留旧回退。`assemblyOrchestration` 仅保留八个窄服务字段。T4.25e 根测试以 AST 精确锁定唯一 `NewWithOptions`、共享实例路径和禁止能力，并复用真实本地 MCP/Store/Capture fixture 验证运行时 candidate 非空、SessionContext 使用同一 ContextManager、RequestBudgeter 有效且 safe execution profile 可构造；路径逃逸、权限 authority/verifier 不匹配和只读 Registry 复用负例均通过。`go test -json -count=1 ./cmd/xagent -run '^TestAssemblyInjectsNarrowOrchestratorDependencies$'`、`go test -json -race -count=20 ./cmd/xagent -run '^(TestAssemblyUsesOnlyFinalProviderMCPAndStoreContracts|TestAssemblyInjectsNarrowOrchestratorDependencies)$'`、`go test -json -count=1 ./internal/orchestrator ./internal/contextmgr ./internal/sessionctx ./internal/memory ./internal/command`、`go test -json -count=1 ./...`、`go vet ./cmd/xagent`、`git diff --check` 与相关 Go 文件 `gofmt -l` 均通过或无输出。下一任务为 T4.25f（2026-08-09）。

### T4.25f 组装 App 与 TUI 窄边界（已完成，2026-08-09）

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.25e、T4.3、T4.12、T4.19、T4.20
**步骤：**
1. App 只获得窄 Orchestrator、Store、artifact user reader 与 Hook lifecycle；TUI 只获得安全 ViewModel 和 intent sink。
2. 返回未发布的完整 Runtime candidate；本任务不修改生产 `main.go`、不启动交互循环，也不发布 App、会话、工具或 TUI。

**验证：** `go test -json -count=1 ./cmd/xagent -run 'TestAssemblyPreservesAppAndTUIBoundaries|TestAssemblyCandidateIsNotPublishedEarly'` 通过。
**覆盖：** F7、F10、F21、F25–F29 / AC7、AC10、AC21、AC25–AC29。

**完成记录（2026-08-09）：** 六阶段构造完成后，owner registry 封存并验证通过，UI 阶段只生成一个未发布的 `assemblyUI` 边界描述符；App 通过四个私有窄转发适配器获得 Conversation、Orchestration、Artifact user reader 与 Hook lifecycle 能力，禁止通过动态类型断言恢复 `conversation.Store`、具体 `*orchestrator.Orchestrator`、`artifact.Store` 或 `hook.Runtime`。候选的 TUI 侧只保留无 capability 的 `tui.ViewModel` 和保留 `Kind/Value/TargetID` 的值型 `tui.Intent` sink；未发布阶段 sink fail closed。严格校验拒绝 broad concrete 注入、typed-nil、空包装和递归包装。UI 构造 AST 门禁确认唯一 `app.AppServices` 与 `tui.NewStateViewModel`，并禁止 `app.New`、`tui.Run`、Bubble Tea `NewProgram`；生产引用扫描确认 candidate 私有符号不出现在 `assembly.go`/`lifecycle.go` 之外的非测试文件。`main.go`、真实 `ui.start_mode` 初始化、交互循环和生产 Runtime 原子切换均按边界留给后续 T4.29a，本任务不宣称已形成可运行 UI。定向命令 `go test -json -count=1 ./cmd/xagent -run '^(TestAssemblyPreservesAppAndTUIBoundaries|TestAssemblyCandidateIsNotPublishedEarly)$'`、同根 `go test -json -race -count=20 ./cmd/xagent -run '^(TestAssemblyPreservesAppAndTUIBoundaries|TestAssemblyCandidateIsNotPublishedEarly)$'`、`go test -json -count=1 ./cmd/xagent` 与全仓 `go test -json -count=1 ./...` 均 exit 0；App/TUI 窄边界测试通过，`go vet ./...`、`git diff --check` 和相关文件 `gofmt -l` 均无输出。下一任务为 T4.25g（2026-08-09）。

### T4.25g 固定三类网络 Client ownership

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.25f、T2.34、T2.45、T2.55、T2.67
**步骤：**
1. Assembly 持有 Provider 专属 Clients；Hook Engine 只持有自己创建的 Clients；MCP HTTP Transport 只持有自己的专属 Client。
2. 为每个 Client 记录唯一 owner 和 close 节点，禁止借用方关闭、重复关闭或回退到 `http.DefaultClient`。

**验证：** `go test -json -count=1 ./cmd/xagent -run TestAssemblyHasSingleOwnerForEveryPolicyClient` 通过。
**覆盖：** F7、F8、F15、F17 / AC7、AC8、AC15、AC17。

**完成记录（2026-08-09）：** Assembly 的 Provider、Hook HTTP runner 与 MCP HTTP transport 均保持独立 Client owner；Provider client 由 Assembly 创建并登记唯一 `CloseIdleConnections` 节点，Hook runner 与 MCP transport 只关闭各自创建的 client，借用方不再承担关闭。根测试锁定唯一 Policy、ClientFactory、Provider client 创建点及四个 adapter close 注册点，并检查 Assembly、Provider、Hook、MCP production 文件均无 `http.DefaultClient`/`http.DefaultTransport` 回退。定向根测试普通与 `-race -count=20`、`cmd/xagent` 完整普通测试、Provider/MCP/HTTP transport 专项测试及全仓 `go test -json -count=1 ./...` 均 exit 0；网络/Hooks/HTTP transport 的 `-race -count=20` 也 exit 0。期间发现并修复 HTTP capture 测试中测试 writer 的并发观测竞态：改用带互斥和一次性写入通知的测试 writer，继续验证正文写入先于受控 body tail，未改变生产 capture API。`go vet ./...`、`git diff --check` 与相关 Go 文件 `gofmt -l` 均无输出。最终 AC/I 仍按既定汇合门禁保留未勾选；下一任务为 T4.26（2026-08-09）。

### T4.25h 固定 Assembly 输入与唯一 Runtime registry

**文件：** `cmd/xagent/{assembly,cli,lifecycle}*.go`
**依赖：** T4.25g、T1.31–T1.33、T2.45、T2.55
**步骤：**
1. 按 C13 定义 `RuntimePaths`、`ConfigInputs` 和 `AssemblyOptions`；`Build` 拒绝缺失或相对 Root，以及 nil `LookupEnv`/stdin/stdout/stderr，`defaultAssembly` 的 factories 保持私有且只填生产构造器。
2. `AssemblyOptions.TrustedRoots` 是唯一可表达的网络定制输入；Assembly 不接受 `http.Client`、transport、proxy、dial、redirect、cookie jar 或协议 handler，并只把证书池传给 netpolicy `ClientOptions`，由 Factory 深拷贝后分别创建 Provider、Hook 与 MCP 专属 Client。
3. 正常 `RunCLI` 在前置 help/version 后解析 OS 目录和配置层，显式填满 Options 并固定 `TrustedRoots=nil` 使用系统信任根；不得增加隐藏 E2E CLI 或测试 factory 开关。
4. `Build` 成功后只发布一个 `Runtime`，其中 App/TUI 与初始化回滚、正常退出共用 T4.25 建立的同一 ownership/close registry；禁止复制 registry、关闭表或第二生产组装根。

**验证：** `go test -json -race -count=1 ./cmd/xagent -run 'TestAssemblyHasNoHTTPTransportInjection|TestTrustedRootsReachProviderHookAndMCPOnlyThroughNetpolicy|TestRunCLIUsesSystemRoots|TestDefaultAssemblyPublishesSingleRuntimeRegistry'` 通过。
**覆盖：** F7、F8、F15、F17、F28、F30、F32 / AC7、AC8、AC15、AC17、AC28、AC30、AC32。

**完成记录（2026-08-09）：** 已在 `cmd/xagent/assembly.go` 固定 C13 的 `RuntimePaths`、`ConfigInputs`、`AssemblyOptions` 与私有六阶段 `defaultAssembly`；`Assembly` 只保留私有 factories，`Build` 在进入任何 stage 前拒绝缺失/相对/非 canonical Root、非法配置路径、nil 或 typed-nil I/O 以及 nil `LookupEnv`，并对调用方 `TrustedRoots` 建立 Build 私有快照。Provider、Hook HTTP runner 与 MCP HTTP transport 的证书池只沿各自 `netpolicy.ClientOptions` 链传递，客户端/transport 仍由 netpolicy 自行创建；默认 CLI seam 在 `cmd/xagent/cli.go` 显式解析 OS 项目、用户配置、用户数据、缓存目录与配置层，固定 `TrustedRoots: nil`、`os.LookupEnv` 和三路标准 I/O。成功 Build 发布的 `Runtime` 与同一 ownership registry 共用，重复/并发 Close 只执行一次。新增四个精确规定根及输入校验、证书池隔离和 CLI seam 行为证据；旧 `runArgs`/完整 App-TUI 生产切换按批准边界保留到 T4.29a，未创建第二组装根。以下证据均在当前工作树实际通过：`go test -json -race -count=1 ./cmd/xagent -run '^(TestAssemblyHasNoHTTPTransportInjection|TestTrustedRootsReachProviderHookAndMCPOnlyThroughNetpolicy|TestRunCLIUsesSystemRoots|TestDefaultAssemblyPublishesSingleRuntimeRegistry)$'`、同命令 `-count=20`、`go test -json -count=1 ./cmd/xagent`、`go test -json -count=1 ./...`、`go vet ./...`、`git diff --check`，以及 `rg --files cmd/xagent | rg '\.go$' | xargs gofmt -l`（无输出）。初始化失败反向回滚、双 context 关闭和真实 App/TUI 发布仍留给后续 T4.26–T4.29a。

### T4.26 实现 App runtime 双 context 关闭

**文件：** `internal/app/lifecycle.go`, `internal/app/lifecycle_test.go`
**依赖：** T4.7、T4.9、T3.25、T3.26、T1.8
**步骤：**
1. 定义含 `CleanupTimeout` 与 `Diagnostics` 的 App `RuntimeOptions`；首次 Close 独立执行“停止新 intent→取消当前请求→Orchestrator.WaitIdle→确认 ChatStream/工具资源关闭”。
2. 存在 active Conversation 时才执行“保存当前 Conversation→发送 SessionEnd”；冷启动列表等无 active 状态明确跳过两者，禁止 Save(nil) 或虚假 SessionEnd。
3. 任一步失败仍继续后续清理并聚合安全诊断；SessionEnd 不得重复，保存发生在 Store/Root 仍可用时。
4. 调用者 context 只限制等待且错误不缓存；内部受 C7 硬上限约束，超限执行强制路径并诊断一次，后续 Close 返回同一最终结果。

**验证：** `go test -json -race -count=20 ./internal/app -run 'TestAppCloseOrderAndSave|TestAppCloseWithoutActiveConversationSkipsSaveAndSessionEnd|TestAppCloseContinuesAfterWaiterTimeout|TestAppCleanupTimeoutDiagnosesOnce'` 通过。
**覆盖：** F7、F15、F17、F20、F25 / AC7、AC15、AC17、AC20、AC25、AC34。

**完成记录（2026-08-09）：** App runtime 已建立共享、并发安全且幂等的双 context Close：首次关闭原子停止 intent、取消当前 request，并在独立 `Background` 派生的 C7 有界 context 中依次执行 `WaitIdle`、active Conversation 保存与唯一 `SessionEnd`；冷启动/列表态明确跳过 `Save(nil)` 和虚假退出事件。调用者 context 只限制本次等待，提前取消不缓存为最终结果；阶段失败或 panic 会安全诊断并继续后续清理，硬超时只诊断一次并缓存稳定结果。Bubble Tea value-copy 通过共享 lifecycle snapshot 保持最新 active Conversation，关闭期间的请求与导航入口 fail closed。规定四根 `go test -json -race -count=20 ./internal/app -run '^(TestAppCloseOrderAndSave|TestAppCloseWithoutActiveConversationSkipsSaveAndSessionEnd|TestAppCloseContinuesAfterWaiterTimeout|TestAppCleanupTimeoutDiagnosesOnce)$'`、完整 `go test -json -count=1 ./internal/app` 与 `go test -json -race -count=1 ./internal/app` 均 exit 0，无 race；`go vet ./...`、`git diff --check` 和相关 Go 文件 `gofmt -l` 均无输出。最终 AC/I 仍按汇合门禁保持未勾选；下一任务为 T4.27（2026-08-09）。

### T4.27 注入 C7 与公开配置 Options

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`
**依赖：** T4.25h、T4.26、T2.7
**步骤：**
1. 将 `lifecycle.cleanup_timeout_ms` 和唯一 `diagnostics.BoundedSink` 注入 ChatStream、MCP Manager/Session/Connection/Transport、Process、Hook Engine 与 App `RuntimeOptions`。
2. 精确接受 1 和 2000 毫秒并按配置生效，拒绝未被 presence 解释为“未设置”的 0、负数和 2001；任何模块不得自行放大或改用调用者 context 作为内部清理 context。
3. 为 C8 及其余公开键建立 Config→owner Options 映射测试，证明每个最终值进入唯一生产 owner，leaf module 不再补默认值或读取全局 Config。

**验证：** `go test -json -count=1 ./cmd/xagent -run 'TestCleanupTimeoutAcceptsOneAndTwoThousandRejectsZeroNegativeAndTwoThousandOne|TestEveryPublishedConfigKeyReachesOwningOptions|TestResolvedConfigReachesOwners'` 通过。
**覆盖：** F15、F17、F28 / AC15、AC17、AC28。

**完成记录（2026-08-09）：** Config stage 只解析一次 `lifecycle.cleanup_timeout_ms`，将 1–2000 毫秒转换为共享 `time.Duration`，并在同一阶段创建唯一 `diagnostics.BoundedSink`；显式 0、负数和 2001 均在启动解析阶段拒绝。该 timeout 与 sink 已沿唯一 Assembly owner 链注入 Process、Hook Engine、Provider/ChatStream、MCP Manager 及其 Session/Connection/Transport，并通过 Orchestrator 传递到具体 Provider；App 仅在候选 UI 中保留同一 `app.RuntimeOptions`，未提前构造或发布真实 App。Hook 的 legacy Collector 仅用于既有用户展示投影，与 C7 bounded sink 明确分离；兼容构造路径仍可使用旧接口，但生产组装不再读取全局 Config 或覆盖已解析值。新增 Assembly owner 映射根与 Provider/Orchestrator 转发测试。以下实证均在当前工作树通过：规定三根 `go test -json -count=1 ./cmd/xagent -run '^(TestCleanupTimeoutAcceptsOneAndTwoThousandRejectsZeroNegativeAndTwoThousandOne|TestEveryPublishedConfigKeyReachesOwningOptions|TestResolvedConfigReachesOwners)$'`；相关包 `go test -json -count=1 ./internal/provider ./internal/orchestrator ./internal/hook ./internal/mcpclient ./internal/proctree ./internal/app ./cmd/xagent`；race 根分别为 `go test -json -race -count=20 ./internal/provider -run '^(TestProviderFactoryRetainsChatStreamLifecycleOptions|TestChatStreamCleanupTimeoutForcesCloseAndDiagnosesOnce)$'`、`go test -json -race -count=20 ./internal/orchestrator -run '^(TestOrchestratorForwardsChatStreamLifecycleOptions|TestOrchestratorKeepsLegacyProviderCompatibility)$'`、`go test -json -race -count=20 ./cmd/xagent -run '^(TestCleanupTimeoutAcceptsOneAndTwoThousandRejectsZeroNegativeAndTwoThousandOne|TestEveryPublishedConfigKeyReachesOwningOptions|TestResolvedConfigReachesOwners)$'`、`go test -json -race -count=20 ./internal/app -run '^(TestAppCloseOrderAndSave|TestAppCloseWithoutActiveConversationSkipsSaveAndSessionEnd|TestAppCloseContinuesAfterWaiterTimeout|TestAppCleanupTimeoutDiagnosesOnce)$'`，以及 `go test -json -race -count=1 ./internal/hook ./internal/mcpclient ./internal/proctree`；全仓 `go test -json -count=1 ./...`、`go vet ./...`、`git diff --check` 和相关 Go 文件 `gofmt -l` 均 exit 0；`cmd/xagent` 的 Darwin/Linux/Windows amd64 测试二进制交叉编译也均 exit 0。最终 AC/I 仍按汇合门禁保持未勾选；下一任务为 T4.28（2026-08-09）。

### T4.28 实现初始化失败反向回滚

**文件：** `cmd/xagent/assembly.go`, `cmd/xagent/assembly_test.go`, `cmd/xagent/assembly_t428_test.go`, `cmd/xagent/lifecycle.go`, `cmd/xagent/lifecycle_test.go`
**依赖：** T4.25h、T4.27
**步骤：**
1. 复用 T4.25 已建立的唯一 ownership/close registry；正常退出和失败回滚必须使用同一份 owner 与关闭函数记录。
2. 构造下一对象失败时，使用不继承初始化 context 取消的内部回滚 context 按登记逆序清理。
3. 逐点注入失败，断言只关闭已创建对象且各关闭一次，不发布半初始化工具、会话、capability 或 TUI。

**验证：** `go test -json -race -count=1 ./cmd/xagent -run TestAssemblyRollsBackEveryInitializationPoint` 通过。
**覆盖：** F4、F7、F15、F17 / AC4、AC7、AC15、AC17。

**完成记录（2026-08-10）：** Assembly 失败出口统一复用同一 ownership registry 与 `closeOwnershipRegistry`，回滚从初始化 context 派生不继承取消、受最终 cleanup timeout 约束的独立 context，并严格按登记逆序调用每个 owner；调用方取消不会跳过已登记 owner，重复关闭仍由同一 `sync.Once` 结果收敛。每个 owner 调用被单独隔离，错误忽略 cleanup context 时也不会阻塞后续 owner 的启动；超时获胜后最终缓存结果固定为 `errAssemblyRollbackTimeout`，避免首次等待结果与后续 Close 不一致。回滚失败和超时分别向 Config stage 已创建的唯一 `diagnostics.BoundedSink` 写入固定 `assembly_rollback_failed` / `assembly_rollback_timeout`、`Source=assembly`、`Severity=error` 安全诊断；初始化错误仍使用固定安全文本，owner 原始错误与 canary 不进入返回值或诊断消息。`TestAssemblyRollsBackEveryInitializationPoint` 已扩展为与生产六阶段 16 个登记点（Config/Security/Execution/Adapters/Orchestration/UI = 2/7/1/4/1/1）一致的登记前/后 32 个前缀故障矩阵，并以 AST 计数绑定生产登记点，覆盖 Security 中段取消、nil Runtime、逆序及 exactly-once；新增 bounded diagnostics、超时结果稳定和 hostile owner 防死锁测试。以下实证均在当前工作树通过：`go test -json -race -count=1 ./cmd/xagent -run '^TestAssemblyRollsBackEveryInitializationPoint$'`；规定根 `-race -count=20`；rollback lifecycle 两根 `-race -count=100`；`go test -race -count=1 ./cmd/xagent`；`go test -count=1 ./...`；`go vet ./...`；`git diff --check`；`rg --files cmd/xagent -g '*.go' | xargs gofmt -l` 无输出；Darwin、Linux、Windows amd64 的 `cmd/xagent` 测试二进制交叉构建成功。最终 AC/I 仍按汇合门禁保持未勾选；下一任务为 T4.29（2026-08-10）。

### T4.29 实现进程级反向关闭

**文件：** `cmd/xagent/lifecycle.go`, `cmd/xagent/lifecycle_test.go`
**依赖：** T4.26、T4.28、T2.67、T2.34
**步骤：**
1. 使用 T4.28 的同一 ownership/close registry，按 App runtime→MCP Manager→Hook Engine→Provider netpolicy Clients→`Artifact Store.Cleanup`→`Artifact Store.Close`→safefs Roots→Diagnostics 启动幂等反向关闭；不得另建正常退出表。
2. 外层调用者 context 只限制等待，内部清理继续到 C7 timeout；保存失败、远端清理失败或单层超时均不得跳过后续 owner。
3. Diagnostics 最后关闭；最终结果缓存，重复 Close 不重复远端请求、SessionEnd、文件关闭或诊断。

**验证：** `go test -json -race -count=20 ./cmd/xagent -run 'TestLifecycleUsesC7DoubleContextAndReverseOrder|TestProviderClientsCloseAfterStreams|TestLifecycleContinuesAfterLayerFailure'` 通过。
**覆盖：** F7、F10、F15、F17 / AC7、AC10、AC15、AC17。

**完成记录（2026-08-10）：** `Runtime.Close` 复用 Assembly 创建的同一 ownership registry，通过唯一 runtime-close worker 启动独立 `context.WithoutCancel` cleanup context 和 1–2000ms bounded timeout；调用方 context 只限制本次等待，取消/超时后后台清理继续，后续 Close 等待并返回同一缓存最终结果。registry 的 reverse walk 对每个 owner 做独立等待隔离，单层失败、超时或忽略 cleanup context 都不会跳过后续 owner；每个 owner、远端 client、stream、SessionEnd 和诊断最多执行一次。Config owner 登记顺序调整为 Diagnostics 先登记、反向关闭最后处理；Provider stream 在其 client 之前关闭，MCP Manager、Hook Engine、Provider client、Artifact cleanup/close、SafeFS roots 依序收敛。新增 T4.29 三个规定根及 cleanup-timeout continuation 根，验证 caller cancellation 脱离、逆序 trace、provider client-after-stream、失败继续后续、timeout 结果缓存和 exactly-once。以下实证均在当前工作树通过：`go test -json -race -count=20 ./cmd/xagent -run 'TestLifecycleUsesC7DoubleContextAndReverseOrder|TestProviderClientsCloseAfterStreams|TestLifecycleContinuesAfterLayerFailure'`；`go test -race -count=20 ./cmd/xagent -run 'TestLifecycleUsesC7DoubleContextAndReverseOrder|TestProviderClientsCloseAfterStreams|TestLifecycleContinuesAfterLayerFailure|TestLifecycleContinuesAfterCleanupTimeout'`；`go test -race -count=1 ./cmd/xagent`；`go test -count=1 ./...`；`go vet ./...`；`git diff --check`；`rg --files cmd/xagent -g '*.go' | xargs gofmt -l` 无输出；Darwin、Linux、Windows amd64 的 `cmd/xagent` 测试二进制交叉构建成功。最终 AC/I 仍按汇合门禁保持未勾选；下一任务为 T4.29a（2026-08-10）。

### T4.29a 原子切换生产入口到唯一组装根

**文件：** `cmd/xagent/main.go`, `cmd/xagent/main_test.go`, `cmd/xagent/assembly.go`, `cmd/xagent/lifecycle.go`, `internal/tool/result.go`, `internal/tool/result_factory.go`, `internal/tool/registry.go`, `internal/tool/bash.go`, `internal/tool/load_skill.go`, `internal/orchestrator/chat.go`, `internal/orchestrator/tool_batches.go`, `internal/orchestrator/skill_runtime.go`, `internal/mcpclient/adapter.go`, `internal/hook/result_adapter.go`, `internal/repoaudit/context_result_refs_test.go` 及 T3.19a 建立的迁移 adapter/直接测试
**依赖：** T3.19a、T4.22、T4.23、T4.25h、T4.27–T4.29
**步骤：**
1. 在一个连续变更中将正常启动切为“CLI 前置解析→Assembly.Build→发布完整 Runtime→运行 App/TUI→同一 registry Close”，删除 main/App 内旧生产组装路径。
2. help/version 与隐藏 launcher 仍在正常 Assembly 之前分流；构造失败、运行错误和正常退出全部进入已验证的回滚或反向关闭路径。
3. 同一原子切换删除 `tool.Result` 公开 payload/兼容字段、直接 composite literal、`Success`/`Failure`、raw fallback、旧 Registry/Bash 构造入口及全部迁移 adapter；协议兼容只允许包内 wire DTO，不能保留真假两套结果生产或消费路径。
4. 静态断言生产服务只由 Assembly 创建，Artifact Store/Capture/ResultFactory 各有唯一 owner，四安全访问器只由 ProjectToolResult 调用且 ModelContent 无任何持久化路径；不保留第二 composition root、旧 Client/Store/Orchestrator fallback 或第二关闭表。

**验证：** `go test -json -race -count=1 ./cmd/xagent ./internal/tool ./internal/orchestrator ./internal/mcpclient ./internal/hook ./internal/repoaudit -run '^(TestMainUsesOnlyAssemblyAndLifecycle|TestMainNeverPublishesPartialRuntime|TestOnlyAssemblyConstructsProductionGraph|TestLegacyToolResultPathsHaveNoProductionReference|TestModelContentHasNoPersistencePath)$'` 通过；required 映射精确为 `(xagent/cmd/xagent, TestMainUsesOnlyAssemblyAndLifecycle, 1)`、同包 `TestMainNeverPublishesPartialRuntime`/`TestOnlyAssemblyConstructsProductionGraph`，以及 `(xagent/internal/repoaudit, TestLegacyToolResultPathsHaveNoProductionReference, 1)`/`TestModelContentHasNoPersistencePath`。五项全部进入 T5.25b CommandManifest/T5.26 gate且各恰好一次 run+pass、无 skip/fail；regex 命中的未批准 tuple 计为 Excess，零匹配、重复、错包同名顶替或只有 package pass 均失败。
**覆盖：** F7、F10、F15、F17、F20、F28、F30 / AC7、AC10、AC15、AC17、AC20、AC28、AC30。

**进度记录（2026-08-10）：** 正常入口已切为 `runCLI → buildAssemblyFromCLI → Assembly.Build → Runtime.Run → Runtime.Close`；UI stage 只在完整生产 Build 中构造真实 `app.Model`，复用 Assembly 已创建的唯一 Orchestrator，并把 App Close 登记到同一 ownership registry。旧 factory/composite closer 已从全部生产编译单元移除，仅以 `_test.go` 支撑历史回归。五个 T4.29a canonical 根在规定 package/名称下各运行并通过一次，定向 race、全仓测试、vet、格式与 diff 检查均通过。公开 Result payload、Success/Failure、raw fallback、旧 Registry/Bash 构造及 MCP migration adapter 的 repo-wide 删除仍是本任务剩余部分，因此本记录不宣称 T4.29a 完成，也不勾选最终 AC/I。

### T4.30 建立 M4 用户路径与组装门禁

**文件：** `internal/app/navigation_test.go`, `internal/tui/program_test.go`, `internal/command/registry_test.go`, `cmd/xagent/cli_test.go`, `cmd/xagent/assembly_test.go`, `cmd/xagent/lifecycle_test.go`
**依赖：** T4.6、T4.8–T4.29、T4.29a
**步骤：**
1. 从真实 parser/key update 层分别输入 `/new`、`/sessions`、`/list`、空闲聊天 `Esc`、流式/确认态双 `Esc`、列表 `Enter`/`n`/`q`，驱动“冷启动列表按 n 新建→列表→旧会话→新会话”；断言无 active 时不 Save，冷启动 q 不发送 SessionEnd，`/list` 与 `/sessions` 完全同义且已有 active 的每次切换前先 WaitIdle/Save。
2. 驱动“完成一轮→开始下一轮→切换会话”，在两个边界后注入旧 request/conversation 的迟到事件，断言 duration、usage、cache、stop reason、错误、confirmation 和临时状态均不能回填新边界。
3. 覆盖 80×24、宽屏、动态缩放、长输入、长消息、确认面板、help/version、Artifact metadata、`ui.start_mode` 与计时关闭。
4. 覆盖每个初始化故障点、App/进程关闭顺序、调用者提前超时、单层失败及所有 Client/capability 的唯一 owner。

**验证：** `go test -json -count=1 ./...`、`go test -json -race -count=1 ./internal/app ./internal/command ./internal/tui ./cmd/xagent`、`go test -json -race -count=20 ./internal/app ./cmd/xagent -run 'Navigation|Escape|Reset|StaleEvent|Close|Lifecycle|Rollback'` 和 `go test -json -count=1 ./internal/app -run TestPublicNavigationInputsEndToEnd` 通过。
**覆盖：** F4–F8、F10、F15、F17、F19–F30 / AC4–AC8、AC10、AC15、AC17、AC19–AC30、AC34。

## M5：跨平台、交付与规格治理

### T5.1 建立确定性 Clock、Gate 与 Trace

**文件：** `internal/testutil/clock.go`, `internal/testutil/*_test.go`
**依赖：** T4.30
**步骤：**
1. 在 Plan 已批准的 `clock.go` 中提供可手动推进的 timer、可取消的 ready/release Gate 和并发安全 Trace，不新增未批准的生产文件，也不用短 `sleep` 判断完成。
2. Gate 在取消、重复 Release 和等待者提前退出时解除全部资源；Trace 返回深拷贝的不可变快照并保持稳定顺序。

**验证：** `go test -json -race -count=20 ./internal/testutil -run 'TestClockGateAndTraceAreDeterministic|TestGateCancellationReleasesWaiters'` 通过。
**覆盖：** F32 / AC32、AC37。

### T5.2 实现 C5/C7 Scripted Provider fixture

**文件：** `internal/testutil/fake_provider.go`, `internal/testutil/fake_provider_test.go`
**依赖：** T5.1
**步骤：**
1. 让 fixture 支持逐事件 gate、`finish_reason → usage → done`、工具参数、扫描错误、截断、阻塞和五类 Close 路径，并只发布 SafeText/SafeError DTO。
2. 删除 Authorization、请求 body 和 secret 原文保存；观测只保留安全摘要、计数和 `SecretSeen`，失败输出不得打印 canary。
3. 生产者保持事件通道唯一关闭权，Close 使用 C7 双 context 语义并可由 Trace 验证所有权顺序。
4. E2E/performance 只复用同文件提供的 loopback HTTP/TLS scripted service，通过配置接入生产 Provider adapter；不得把 Provider 接口 fake 注入 Assembly，服务只瞬时校验请求并保留 digest、计数和 expected-auth 布尔值。

**验证：** `go test -json -race -count=20 ./internal/testutil -run 'TestScriptedProviderStateMachine|TestScriptedProviderStreamLifecycle|TestFakeProviderNeverPrintsSecret'` 通过。
**覆盖：** F7、F17、F18、F32 / AC7、AC17、AC18、AC32、AC37。

### T5.3 实现 MCP lease 与外部服务 fixture

**文件：** `internal/testutil/fake_mcp.go`, `internal/testutil/fake_mcp_test.go`
**依赖：** T5.1
**步骤：**
1. 提供只返回安全 adapter/Result 的 Manager/lease fixture，支持 Start、Call、pending、远端清理、并发 Close 和 owner Trace。
2. 提供本机 loopback HTTP/TLS JSON/SSE 服务，支持 redirect 观测、未知长度分块、超限响应、session delete 和阻塞 gate；不保存 header、env、frame 或错误原文。
3. Manager/lease fake 只用于单元故障测试；E2E 必须把 loopback 服务经配置接入生产 MCP adapter，fake Transport 不得伪造生产 raw frame 跨 Connection 边界，任何 fixture 均不得访问公网。

**验证：** `go test -json -race -count=20 ./internal/testutil -run 'TestFakeMCPLeaseAndClose|TestFakeMCPRedirectAndChunking|TestFakeMCPSessionClose'` 通过。
**覆盖：** F7、F8、F14–F16、F32 / AC7、AC8、AC14–AC16、AC32、AC37。

### T5.4 实现 Conversation 与 Artifact 故障 fixture

**文件：** `internal/testutil/conversation_store.go`, `internal/testutil/conversation_store_test.go`, `internal/testutil/artifact.go`, `internal/testutil/artifact_test.go`
**依赖：** T5.1
**步骤：**
1. 覆盖 Create、List、Load、Save、Maintain、partial ListResult、write/sync/rename 故障、重启和阶段顺序 Trace；输入输出均深拷贝。
2. Artifact fixture 只暴露 opaque Ref、字节数、完成状态和内容 hash；raw 仅保存在私有临时根，失败信息不回显正文或路径。

**验证：** `go test -json -race -count=20 ./internal/testutil -run 'TestConversationStoreFaultScript|TestFakeStoreFaultMatrix|TestFakeArtifactKeepsRawPrivate'` 通过。
**覆盖：** F7、F10、F19–F22、F32 / AC7、AC10、AC19–AC22、AC32、AC34、AC35、AC37。

### T5.5 实现 safefs、proctree 与进程 helper

**文件：** `internal/testutil/safefs.go`, `internal/testutil/proctree.go`, `internal/testutil/safefs_test.go`, `internal/testutil/proctree_test.go`, `internal/e2e/testdata/processhelper/main.go`
**依赖：** T5.1
**步骤：**
1. safefs fixture 只能包装真实 Bootstrap/Root 并注入 I/O 故障，不能构造、复制或升级 capability seal。
2. Process fixture 保持唯一 Wait、pipe 和 Close owner；原生 helper 用 pipe/gate 建立父、子、孙进程 ready/release 握手及有界副作用 marker，不依赖 sleep。
3. helper 只接受测试创建的显式临时路径，不接触仓库或用户数据。

**验证：** `go test -json -race -count=20 ./internal/testutil -run 'TestSafeFSFaultsCannotForgeCapability|TestFakeProcessAndArtifactOwnership|TestProcessHelperBarrierAndDescendants'` 通过。
**覆盖：** F4、F9–F11、F31、F32 / AC4、AC9–AC11、AC31、AC32、AC37。

### T5.6 锁定默认测试的 hermetic 与稳定性边界

**文件：** `internal/testutil/hermetic_test.go`, `internal/{provider,mcpclient,orchestrator,app,hook}/**/*_test.go`
**依赖：** T5.2–T5.5
**步骤：**
1. 将资源敏感测试中的短 `time.Sleep` 替换为 Clock/Gate/Trace；固定 MCP stdio、取消、Close、并发、burst 和 rollback 的测试名单。
2. 默认测试只使用显式临时 Root、固定 canary、loopback 服务和 helper process；不得读取真实凭据、调用真实 Provider 或访问公网。
3. 真实外部集成只能放在显式 build tag 下，普通测试发现真实 endpoint 或凭据来源时立即失败。

**验证：** `go test -json -count=3 ./...`、`go test -json -race -count=20 ./internal/provider ./internal/mcpclient/... ./internal/orchestrator ./internal/app ./internal/hook -run 'Close|Cancellation|Concurrent|Burst|Blocked|Rollback'` 和 `go test -json -count=1 ./internal/testutil -run TestDefaultTestsAreHermetic` 通过。
**覆盖：** F32 / AC32、AC37。

### T5.7 建立复用唯一 Assembly 的 hermetic E2E harness

**文件：** `internal/e2e/harness.go`, `internal/e2e/assertions.go`, `cmd/xagent/e2e_security_test.go`, `internal/e2e/testdata/{processhelper,performance}/**`, `.repoaudit.yaml`
**依赖：** T5.6、T4.25h、T4.29a
**步骤：**
1. `internal/e2e` 只提供带 `e2e` build tag 的可导入非测试 helper；所有场景测试位于 `cmd/xagent`、使用 `package main`，并经同包唯一 bridge 把 fixture 映射为 `AssemblyOptions` 后只调用 `defaultAssembly().Build`。
2. 每个场景显式注入临时绝对 RuntimePaths、配置层、固定环境查找和可选测试 CA；不得改写 HOME、覆盖 factories、恢复旧组装根或增加隐藏 E2E CLI。
3. Harness 默认使用真实 Store、Permission、Executor、safefs、artifact、netpolicy、Orchestrator、App 与关闭 registry；外部 Provider/MCP 只通过本机服务 fixture 接入生产 adapter。
4. 固定 canary 仅增加“路径＋规则 ID＋内容摘要”精确 repoaudit 豁免；禁止按目录或通用 fake-key 模式放行。
5. 静态断言生产路径只有 `RunCLI` 调用 `Build`，E2E 只有同包 bridge 和 current performance adapter 调用；`internal/e2e` 不得出现生产 Store/Executor/Provider/MCP/App 构造器。每个场景显式正常关闭 Runtime，`t.Cleanup` 只作失败兜底。

**验证：** `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run 'TestE2EHarnessIsHermetic|TestE2EHarnessUsesDefaultAssemblyOnly|TestE2EFixtureExemptionIsExact'` 通过。
**覆盖：** F2、F7、F32 / AC2、AC7、AC32、AC37。

### T5.8 建立授权碰撞 E2E

**文件：** `cmd/xagent/e2e_security_test.go`
**依赖：** T5.7
**步骤：**
1. 建立 `TestE2EAC38AuthorizationCollisionAndPermissionProtection` 根测试及授权碰撞 subtest：先授权固定单行 Bash，再提交仅增加换行、空格、引号或控制符而改变语义的样本；T5.9 只在该根中追加受保护路径 subtest。
2. 断言 once、session、permanent 均不能复用旧 Ticket，第二条调用重新确认或拒绝，原命令身份和展示脱敏互不替代。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38AuthorizationCollisionAndPermissionProtection/AuthorizationCollision$'` 通过，JSON 输出必须含该 subtest 恰好 20 次 pass。
**覆盖：** F3 / AC3。

### T5.9 建立受保护权限路径绕过 E2E

**文件：** `cmd/xagent/e2e_security_test.go`
**依赖：** T5.8
**步骤：**
1. 通过 Write、Edit、Bash 的直接路径、变量、拼接、解释器和子孙进程尝试写现有及尚不存在的 permission slots。
2. 断言全部在副作用前 fail closed，目标内容与身份摘要不变；平台能力不足时断言目标进程未启动而不是 skip。
3. 以 `TestE2EAC38AuthorizationCollisionAndPermissionProtection` 为唯一根测试，用 `t.Run` 串联 T5.8 与本任务全部样本，使一条精确命令完整执行 AC38(1)。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38AuthorizationCollisionAndPermissionProtection$'` 通过。
**覆盖：** F3、F4、F31 / AC3、AC4、AC31、AC38(1)。

### T5.10 建立大输出与私有 Artifact E2E

**文件：** `cmd/xagent/e2e_output_process_test.go`
**依赖：** T5.7
**步骤：**
1. 建立 `TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation` 根测试及大输出 subtest：产生远超 inline 限制的 stdout/stderr，断言采集期预算、有界 UTF-8 预览、工作区外私有 artifact、opaque Ref、字节数和完成状态正确；T5.11 只在该根中追加进程树 subtest。
2. 断言 raw 不进入 ViewModel、下一次模型请求、JSONL、memory、diagnostics 或测试输出；用户读取与清理只通过正式入口。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation/LargeOutputArtifact$'` 通过，JSON 输出必须含该 subtest 恰好 20 次 pass。
**覆盖：** F7、F9、F10 / AC7、AC9、AC10。

### T5.11 建立完整进程树取消 E2E

**文件：** `cmd/xagent/e2e_output_process_test.go`, `internal/e2e/testdata/processhelper/main.go`
**依赖：** T5.10
**步骤：**
1. 启动带父、子、孙进程及 marker 的真实受保护进程树，在 ready 后触发取消与超时。
2. 在 2 秒硬边界内确认全部后代退出、唯一 Wait 完成且 marker 不再变化；能力不足必须证明 target 从未启动。
3. 以 `TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation` 为唯一根测试，用 `t.Run` 串联 T5.10 与本任务全部样本，使一条精确命令完整执行 AC38(2)。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation$'` 通过。
**覆盖：** F4、F11、F31 / AC4、AC11、AC31、AC38(2)。

### T5.12 建立长会话重启、恢复与外置 E2E

**文件：** `cmd/xagent/e2e_conversation_test.go`
**依赖：** T3.8、T5.7
**步骤：**
1. 建立 `TestE2EAC38LongConversationRestartExternalizeAndSwitch` 根测试及恢复 subtest：保存超过 64 KiB 但未超系统上限的会话，并修改工具外置状态、摘要和 metadata；通过正常 Close 销毁 Runtime 后用同一配置重新 Build，T5.13 只在该根中追加切换 subtest。
2. 断言恢复后的消息、Ref、摘要、metadata、state digest 与保存前一致；torn tail 和 legacy 导入失败时保留原文件且仍可继续保存。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38LongConversationRestartExternalizeAndSwitch/RestartRestoreExternalize$'` 通过，JSON 输出必须含该 subtest 恰好 20 次 pass。
**覆盖：** F10、F19、F20 / AC10、AC19、AC20、AC34、AC35。

### T5.13 建立恢复会话导航与状态边界 E2E

**文件：** `cmd/xagent/e2e_conversation_test.go`
**依赖：** T5.12
**步骤：**
1. 驱动“新建→列表→恢复旧会话→切回新会话”，核对 WaitIdle、Save、SessionEnd、原子提交、SessionStart 的顺序和 generation。
2. 在请求与会话边界注入迟到事件，断言 request 状态、mode、skills、输入和消息视图按 C9 reset，Runtime 状态保持。
3. 以 `TestE2EAC38LongConversationRestartExternalizeAndSwitch` 为唯一根测试，用 `t.Run` 串联 T5.12 与本任务全部样本，使一条精确命令完整执行 AC38(3)。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38LongConversationRestartExternalizeAndSwitch$'` 通过。
**覆盖：** F20、F21、F25、F26 / AC20、AC21、AC25、AC26、AC34、AC38(3)。

### T5.14 建立 MCP secret、重定向、预算与关闭 E2E

**文件：** `cmd/xagent/e2e_mcp_provider_test.go`
**依赖：** T5.3、T5.7
**步骤：**
1. 建立 `TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose` 根测试及 MCP subtest；通过生产 MCP adapter 连接本机 HTTP/TLS fixture，覆盖 env/header secret 回显、恶意 redirect、未知长度超限 SSE、pending 调用和 remote session delete，Manager/Transport fake 不得进入 Assembly。
2. 断言 redirect sink 未收到认证头、未知长度 body 被取消，超限只按 framing 恢复能力影响调用或 Connection；pending、handler、body、connection、lease 全归零，远端 DELETE 恰好一次且重复 Close 不重发。
3. 外部 fixture 必须观测真实 initialize→tools/list→call→session delete wire；测试 CA 只能经 `TrustedRoots` 进入，factories/client/transport 注入在类型层无法表达。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose/MCP$'` 与 `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestMCPUsesProductionAdapterAndTrustedRoots$'` 均通过；两条命令的 required JSON 记录不得缺失或 skip。
**覆盖：** F7–F9、F14–F16 / AC7–AC9、AC14–AC16。

### T5.15 建立 Provider usage 与五类退出 E2E

**文件：** `cmd/xagent/e2e_mcp_provider_test.go`
**依赖：** T5.2、T5.7
**步骤：**
1. 通过生产 Provider adapter 连接本机 fixture，覆盖 finish→usage→done、正常、解析错误、网络错误、取消和消费者提前退出。
2. 断言 usage 同时进入状态和上下文估算，扫描错误不能伪装 EOF，所有路径关闭 body/SDK stream、生产者和 Client lease。
3. 外部 fixture 必须观测真实 Provider wire 与认证摘要；测试 CA 只能经 `TrustedRoots` 进入，Provider fake/factory/client/transport 不得注入 Assembly。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose/Provider$'` 与 `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestProviderUsesProductionAdapterAndTrustedRoots$'` 均通过；两条命令的 required JSON 记录不得缺失或 skip。
**覆盖：** F7、F8、F17、F18 / AC7、AC8、AC17、AC18。

### T5.16 建立全渠道 canary E2E

**文件：** `cmd/xagent/e2e_security_test.go`, `cmd/xagent/e2e_mcp_provider_test.go`
**依赖：** T5.9–T5.15
**步骤：**
1. 将同一固定 canary 放入 LLM key、MCP env/header、Hook、Provider/MCP error 与 result，并走完真实请求、保存、恢复和关闭。
2. 扫描 TUI/ViewModel、下一模型 body、JSONL、memory、diagnostics、日志和测试子进程输出，要求原文零命中；私有 artifact 只用 hash 证明允许的 raw 工具输出存在。
3. 失败报告只显示安全 code、渠道和摘要，不得把 canary 带入断言文本。
4. 以 `TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose` 为唯一根测试，用 `t.Run` 串联 T5.14、T5.15 与本任务全部样本，使一条精确命令完整执行 AC38(4)。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose$'` 通过。
**覆盖：** F7、F10、F14–F18 / AC7、AC10、AC14–AC18、AC38(4)。

### T5.16a 定义 GitEvidenceOwner 与净化 Git 来源

**文件：** `internal/repoaudit/git_source.go`, `internal/repoaudit/git_source_test.go`, `internal/repoaudit/git_source_unix.go`, `internal/repoaudit/git_source_windows.go`, `internal/repoaudit/git_source_unsupported.go`
**依赖：** T0.11、T0.3a、T5.16
**步骤：**
1. 实现不导出构造 marker 的 `GitEvidenceOwner`、`GitEvidenceIndex|GitEvidenceCommit` 与 Plan C17 的方法/source 组合；index 禁止 revision，commit 必须 canonical 40hex并只接受 fresh ordinary `.git` checkout。
2. 从绝对 handle identity封闭 repo/task/source，拒绝 linked worktree、submodule、alternate/replace/graft/partial clone、缺对象及 containment/alias；所有 `GIT_*` 调用者环境先清空，再只注入批准白名单和固定 absolute Git executable。
3. fresh commit checkout local config、object database 与 admin side files按 Plan 闭集净化；依赖预取必须在只读 baseline 前完成，之后 Git owner不写 worktree、index、objects、refs或 config。

**验证：** `go test -json -race -count=20 ./internal/repoaudit -run 'TestGitEvidenceOwnerRejectsWrongSourceRevisionAndAliases|TestGitEvidenceOwnerSanitizesAllGitEnvironment|TestFreshCheckoutRejectsAlternateReplaceShallowAndExtraObjects|TestGitEvidenceOwnerMethodsAreClosedBySource'` 通过。
**覆盖：** F1、F2、F31、F32 / AC1、AC2、AC31、AC32、AC37。

### T5.16b 实现 commit workspace、Git admin 与 symlink closure 证明

**文件：** `internal/repoaudit/workspace.go`, `internal/repoaudit/workspace_unix.go`, `internal/repoaudit/workspace_windows.go`, `internal/repoaudit/workspace_unsupported.go`, `internal/repoaudit/workspace_test.go`
**依赖：** T5.16a
**步骤：**
1. 实现 `VerifyCommitWorkspace`：HEAD、stage-0 index、tree、no-follow inventory与 `.git` admin tree 全部从同一 owner handle重算，按 Plan 唯一编码产生 IndexSHA256、InventorySHA256、GitAdminSHA256及精确计数。
2. 对所有 tracked symlink递归解析 closure，拒绝绝对/逃逸/`.git`/cycle/broken/untracked target、case/Unicode alias、mount/volume crossing、unexpected reparse与 identity drift；regular hard-link count 必须为 1。
3. 每个 Step 前后及 profile 结束复验；POSIX tracked/admin nodes按 executable 投影收紧为 0400/0500，Windows 使用只读 execute/traverse DACL，任一权限/bytes/type变化立即停链。

**验证：** `go test -json -race -count=20 ./internal/repoaudit -run 'TestCommitWorkspaceDigestUsesCanonicalBytes|TestGitAdminTreeRejectsEveryExtraSideFile|TestTrackedSymlinkClosureRejectsEscapeCycleBrokenAndUntrackedTargets|TestCommitWorkspaceDetectsPermissionAndIdentityDrift'` 通过。
**覆盖：** F1、F4、F31、F32 / AC1、AC4、AC31、AC32、AC37。

### T5.16c 实现 index materialization 与只读快照复验

**文件：** `internal/repoaudit/git_source.go`, `internal/repoaudit/workspace.go`, `internal/repoaudit/workspace_test.go`
**依赖：** T5.16b
**步骤：**
1. index owner只以内建 parser读取 stage-0 index/object database，按 raw path bytes materialize到 `XAGENT_TASK_TMP` 的唯一直接私有 source child；拒绝 unmerged/ITA/skip-worktree/assume-unchanged、gitlink、缺 blob、非法路径及 case/Unicode collision。
2. materialize 前验证 symlink closure，publish 后 children-first/root-last 收紧 POSIX 0400/0500或 Windows只读 DACL；不读取用户 worktree内容，也不要求修改普通 clone local config。
3. `VerifyMaterializedIndex` 在每个 index Step前后复算 source identity/权限/摘要；第二次 materialize、另一个 source、typed-nil/空 digest、task/evidence alias或节点变化均失败。

**验证：** `go test -json -race -count=20 ./internal/repoaudit -run 'TestMaterializeIndexNeverReadsUserWorktree|TestMaterializedIndexRejectsFlagsGitlinkCollisionAndMissingBlob|TestMaterializedIndexIsExactlyReadOnlyOnPOSIXAndWindows|TestMaterializedIndexDetectsEveryNodeChange'` 通过。
**覆盖：** F1、F4、F31、F32 / AC1、AC4、AC31、AC32、AC37。

### T5.16d 实现 commit archive 与单父精确 delta

**文件：** `internal/repoaudit/git_source.go`, `internal/repoaudit/git_source_test.go`, `internal/repoaudit/deletion_candidates.go`, `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.16c
**步骤：**
1. `ArchiveCommit` 只允许 commit owner和已授权 ancestry revision，逐项比较 archive与 commit tree，拒绝 attributes 导致的 export-ignore/subst、漏项、替换、逃逸或链接跟随。
2. `ReadSingleParentDelta` 只返回 canonical single-parent commit delta；change-kind、old/new mode/OID与 raw path排序按 Plan 编码，merge、额外 path、错误 parent或另一个 revision失败。
3. 为 Rpre→D01–D17→R0/E 建立 synthetic chain测试，覆盖 R0=D17 alias、仅 go.mod/go.sum tidy child和 E 仅三份 marker变化。

**验证：** `go test -json -race -count=20 ./internal/repoaudit -run 'TestArchiveCommitMatchesTreeDespiteAttributes|TestReadSingleParentDeltaCanonicalEncoding|TestDeletionChainRejectsMergeExtraPathAndWrongParent|TestR0AliasTidyChildAndEvidenceDeltaAreClosed'` 通过。
**覆盖：** F1、F2、F31–F33 / AC1、AC2、AC31–AC33、AC37。

### T5.17 定义共享性能协议与 workload

**文件：** `internal/e2e/perfprotocol/protocol.go`, `internal/e2e/perfprotocol/protocol_test.go`, `internal/e2e/perfworkload/workload.go`, `internal/e2e/perfworkload/workload_test.go`
**依赖：** T5.7、T5.16d
**步骤：**
1. 按 C12 定义 Version、Workload、Phase、Bootstrap、Begin、Command、Ready、Result、Observation 和严格 NDJSON codec；拒绝未知字段、尾随 JSON、超限帧及非法 version/ID/workload/phase/action。
2. 定义 Manifest、Driver、BuildReady、Run 与 `ValidateAndDigest(workload, phase, ...)`；共享脚本固定 ready、普通对话、会话列表和只读文件四种操作。
3. 规范摘要必须纳入 workload/phase；`PhaseReady` 只允许 ready/screen，`PhaseRun` 拒绝 StartupReady 并按 workload 精确校验消息数、assistant hash、Session IDs、读取字节数和 hash。
4. Ready/Result 均不得含 duration 或其他 adapter 自报计时字段，协议测试通过反射与非法输入证明计时只能由 coordinator 完成。

**验证：** `go test -json -race -tags='e2e performance' -count=20 ./internal/e2e/perfprotocol ./internal/e2e/perfworkload -run 'TestPerformanceProtocolStateMachineAndNoDuration|TestPerformanceWorkloadPostconditionsAndDigest'` 通过。
**覆盖：** F32 / AC32、AC36、AC37。

### T5.17a 实现严格有界 performance NDJSON codec

**文件：** `internal/e2e/perfprotocol/codec.go`, `internal/e2e/perfprotocol/codec_test.go`, `internal/e2e/perfprotocol/protocol.go`
**依赖：** T5.17
**步骤：**
1. 每帧 payload 最大 1 MiB、唯一 LF不计上限，以 cap+1读取；先验证 UTF-8，再用显式迭代 stack限制 nesting depth=64，任意 object层拒绝 duplicate/unknown key。
2. 每帧先解到对应临时 DTO并完成 schema/枚举/关系校验后才提交；拒绝 null、BOM、CRLF、前后空白、第二顶层值、尾随 bytes、错误类型、半帧和整数溢出。
3. 任一协议错误使 decoder进入 poison终态；之后所有 Decode固定返回同一安全分类，不消费后续 bytes，也不能把畸形帧恢复成下一条合法命令。

**验证：** `go test -json -race -count=20 ./internal/e2e/perfprotocol -run 'TestCodecOneMiBBoundaryAndUTF8|TestCodecRejectsDuplicateUnknownNullAndDepth65|TestCodecCommitsOnlyFullyValidatedDTO|TestCodecPoisonStateNeverResynchronizes'` 通过。
**覆盖：** F7、F9、F32 / AC7、AC9、AC32、AC36、AC37。

### T5.18 实现固定 baseline archive 与版本 adapter

**文件：** `internal/e2e/performance.go`, `internal/e2e/performance_test.go`, `internal/e2e/testdata/performance/adapter_607b_test.go`
**依赖：** T5.17a
**步骤：**
1. 固定完整 revision `607b3dd3cb9d5f01bcba9a047b816187b4512b4a`，先验证本地对象存在且类型为 commit；缺失时失败，不联网 fetch、不回退当前代码。
2. 通过只读 `git archive --format=tar` 输出安全解包到临时目录，拒绝绝对路径、`..` 和逃逸链接；操作前后当前 worktree/index 摘要保持不变。
3. 将共享 protocol/workload 复制到临时树相同 import path，把 baseline adapter 精确复制为 `cmd/xagent/e2e_performance_adapter_607b_test.go`，以同一只读 module cache 和 `GOPROXY=off` 编译。
4. baseline `_test.go` 提供 test-only `TestMain` 或等价测试二进制入口；只在 coordinator 的 adapter 标记存在时运行 NDJSON server 并在正常 Close 后直接退出，否则运行 `m.Run()`，不得新增生产 CLI 模式或输出 Go test RUN/PASS framing。
5. baseline 从旧 `defaultStartupFactories` 开始，只替换 getwd、userHomeDir、lookupEnv、runTUI 四个环境接点；旧入口在 goroutine 中运行，runTUI bridge 发布存活 Driver 后阻塞，直到 `Driver.Close` 才放行旧入口执行原有关闭；Store、Provider、Registry、MCP 与 owner 构造保持生产默认。

**验证：** `go test -json -tags='e2e performance' -count=1 ./internal/e2e -run '^TestPerformanceBaselineArchiveAndAdapterBridge$'` 通过，且 archive、逃逸拒绝、离线构建、存活 bridge 与 worktree/index 不变均为该根测试的必跑子项。
**覆盖：** F32 / AC32、AC36、AC37。

### T5.19 实现 current adapter 与严格握手

**文件：** `cmd/xagent/e2e_performance_adapter_test.go`, `internal/e2e/performance.go`, `internal/e2e/performance_test.go`
**依赖：** T5.17、T5.18、T4.25h、T4.29a
**步骤：**
1. current adapter 使用 `package main`、`defaultAssembly().Build` 和真实 Runtime，只映射共享 Driver，不覆盖 factories 或重建 owner；以 test-only `TestMain`/等价入口分流 adapter 模式，stdout 只能写 NDJSON，普通测试仍走 `m.Run()`。
2. 两个 adapter 均先读取 Bootstrap、严格加载 manifest 并准备纯 Options，再发送 `Begin(ready)`；收到唯一 build Command 后才 Build→共享 BuildReady→Ready。StartupReady 随即正常 Close/退出，其余 workload 再执行 `Begin(run) → 唯一 run Command → 共享 Run → Result → Close`。
3. Begin 前禁止创建网络 Client、Store、进程或其他 owner；EOF、重复/错序 ID/phase/action 及 Build/Run/Close 错误只产生安全 code，coordinator 仅记录 stderr 摘要，不回显原文。
4. 只有 C13 TrustedRoots 可加入本机测试 CA，任何 http.Client、transport、proxy、dialer 或 redirect 注入均无法表达；AST/协议测试拒绝 adapter 使用 `time.Now`/`time.Since` 计量或上报耗时。

**验证：** `go test -json -race -tags='e2e performance' -count=20 ./cmd/xagent -run 'TestPerformanceCurrentAdapterUsesDefaultAssembly|TestPerformanceAdaptersEmitOnlyNDJSON'` 通过。
**覆盖：** F32 / AC32、AC36、AC37。

### T5.20 建立治理前性能比较门禁

**文件：** `internal/e2e/performance.go`, `internal/e2e/performance_test.go`, `cmd/xagent/e2e_performance_test.go`, `internal/e2e/testdata/performance/**`
**依赖：** T5.12、T5.13、T5.15、T5.19
**步骤：**
1. Coordinator 为每个样本启动新进程并创建全新隔离 fixture；成对 baseline/current 的内容与 manifest digest 相同但 Root 不同，普通对话经各自生产 Provider adapter 连接同一类本机 loopback 服务，外部观测只保留请求数、body digest 和 expected-auth 布尔值。
2. 严格执行“写 Bootstrap（不计时）→收并校验 Begin(ready)→单调计时起点→一次完整写 build Command→收 Ready 并独立 `ValidateAndDigest`→停止”；非 Startup 再以相同步骤计量 `run Command → Result`，adapter 不得自报时长。
3. watchdog 覆盖 Begin/Ready/Result；异常必须 kill+Wait。每个 revision/workload 恰好执行 5 个预热和 30 个计分样本，交错并轮换先后顺序；任一预热或计分失败、超时、协议/后置条件错误、进程异常或非正时长令整项失败，不得重试、替换或筛样。
4. 将每组 30 个计分时长按无符号纳秒值非降序排为 `s[0]..s[29]`，以 `s[14]+(s[15]-s[14])/2` 得到向下取整到整纳秒的唯一中位数；测试必须拒绝下中位数、上中位数和浮点平均值替代。启动、普通对话、会话列表或只读工具任一项都用 `math/bits.Mul64` 形成 128 位乘积并精确要求 `CurrentMedianNanos*5 <= BaselineMedianNanos*6`，不能用浮点或 ppm 取整值判门禁；`RatioPPM=floor(CurrentMedianNanos*1000000/BaselineMedianNanos)` 只作可复核展示字段。报告只含批准的安全字段，且覆盖恰等于 1.20、只高于 1.20 不足 1 ppm、乘法接近 `uint64` 上限和展示商不可表示四类边界。

**验证：** `go test -json -tags='e2e performance' -count=1 ./cmd/xagent -run TestPerformanceAgainstGovernanceBaseline` 通过，四项均产生 30 个有效样本且无真实路径、凭据或原始会话内容。
**覆盖：** F32 / AC32、AC36、AC37。

### T5.21 定义三平台共同安全契约与必跑清单

**文件：** `internal/e2e/harness.go`, `internal/e2e/assertions.go`, `cmd/xagent/e2e_platform_{darwin,linux,windows}_test.go`
**依赖：** T5.5、T5.7
**步骤：**
1. 在 Plan 已批准的 harness/assertions 与三份 OS 测试中固定 `path_escape`、`link_or_reparse_race`、`protected_slot`、`process_tree_cancel`、`stdio_close` 的共同输入、后置条件及 required subtest names，不新增第二套 platform helper 文件。
2. 核心用例不得调用 `t.Skip`；能力不可用必须断言 fail-closed error、target marker 未出现且资源计数归零。
3. 三份 OS 文件各自提供同名 `TestE2EAC38NativePlatformSecurity`；开始时核对真实宿主与 runtime 均为当前 OS/amd64，非 amd64 直接失败，交叉构建只能计 build 证据。
4. 冻结 native package/test 映射：`./internal/safefs` 必须运行 `TestNativeSafeFSRejectsPathEscape`、`TestNativeSafeFSRejectsLinkOrReparseRace`；`./internal/proctree` 必须运行 `TestNativeProtectedSlotFailsClosed`、`TestNativeProcessTreeCancellation`；`./internal/tool`、`./internal/hook`、`./internal/mcpclient/transport/stdio` 必须分别运行 `TestNativeBashProtectedSlot`、`TestNativeHookProcessTreeCancellation`、`TestNativeStdioClose`。同名测试由各 OS 文件提供，required gate 按 package/test 二元组判定，不能由另一 package 的同名或相似测试顶替。

**验证：** `go test -json -race -tags=e2e -count=20 ./cmd/xagent -run '^TestE2EAC38NativePlatformSecurity$'` 及 native manifest 静态契约测试 `TestNativeRequiredPackageTestManifestIsExact` 通过；后者精确断言上述 7 个 package/test 二元组。非 amd64、翻译执行或错误宿主必须失败，不得 skip 或记作通过。
**覆盖：** F4、F11、F16、F31、F32 / AC4、AC11、AC16、AC31、AC32、AC38(5)。

### T5.22 建立 macOS amd64 原生安全门禁

**文件：** `cmd/xagent/e2e_platform_darwin_test.go`, `internal/safefs/root_darwin_test.go`, `internal/proctree/runner_darwin_test.go`, `internal/tool/bash_darwin_test.go`, `internal/hook/command_darwin_test.go`, `internal/mcpclient/transport/stdio/transport_darwin_test.go`
**依赖：** T5.9、T5.11、T5.13、T5.16、T5.21
**步骤：**
1. 在 Intel macOS runner 断言真实宿主和 runtime 均为 `darwin/amd64`，拒绝 Rosetta/ARM host，覆盖路径逃逸、symlink 竞争、Seatbelt/protected slot、子孙进程取消和 stdio Close。
2. Seatbelt 或其他能力不足时只接受目标未启动的显式 fail-closed 结果，禁止在 arm64 runner 设置 GOARCH 后冒充原生通过。
3. 同一 runner 还必须精确运行 AC38 其余四组根测试，required test 缺失、skip 或少跑均失败。

**验证：** macOS amd64 runner 精确执行 `go test -json -race -count=20 ./internal/safefs ./internal/proctree ./internal/tool ./internal/hook ./internal/mcpclient/transport/stdio -run '^(TestNativeSafeFSRejectsPathEscape|TestNativeSafeFSRejectsLinkOrReparseRace|TestNativeProtectedSlotFailsClosed|TestNativeProcessTreeCancellation|TestNativeBashProtectedSlot|TestNativeHookProcessTreeCancellation|TestNativeStdioClose)$'`，required gate 按 T5.21 的 7 个 package/test 二元组判定；随后执行 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EHarnessIsHermetic|TestE2EHarnessUsesDefaultAssemblyOnly|TestE2EFixtureExemptionIsExact|TestMCPUsesProductionAdapterAndTrustedRoots|TestProviderUsesProductionAdapterAndTrustedRoots|TestE2EAC38AuthorizationCollisionAndPermissionProtection|TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38LongConversationRestartExternalizeAndSwitch|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose|TestE2EAC38NativePlatformSecurity)$'`；10 个根测试全部恰好 pass 一次且无 skip。
**覆盖：** F4、F11、F16、F31、F32 / AC4、AC11、AC16、AC31、AC32、AC38(5)。

### T5.23 建立 Linux amd64 原生安全门禁

**文件：** `cmd/xagent/e2e_platform_linux_test.go`, `internal/safefs/root_linux_test.go`, `internal/proctree/runner_linux_test.go`, `internal/tool/bash_linux_test.go`, `internal/hook/command_linux_test.go`, `internal/mcpclient/transport/stdio/transport_linux_test.go`
**依赖：** T5.9、T5.11、T5.13、T5.16、T5.21
**步骤：**
1. 在 Linux amd64 runner 覆盖路径逃逸、symlink 竞争、Landlock/no_new_privs、protected slot、子孙进程取消和 stdio Close。
2. 内核能力不足时断言 launcher 在目标 exec 前 fail closed 且 marker 不出现，禁止退回裸进程或 skip。
3. 同一 runner 还必须精确运行 AC38 其余四组根测试，required test 缺失、skip 或少跑均失败。

**验证：** Linux amd64 runner 精确执行 `go test -json -race -count=20 ./internal/safefs ./internal/proctree ./internal/tool ./internal/hook ./internal/mcpclient/transport/stdio -run '^(TestNativeSafeFSRejectsPathEscape|TestNativeSafeFSRejectsLinkOrReparseRace|TestNativeProtectedSlotFailsClosed|TestNativeProcessTreeCancellation|TestNativeBashProtectedSlot|TestNativeHookProcessTreeCancellation|TestNativeStdioClose)$'`，required gate 按 T5.21 的 7 个 package/test 二元组判定；随后执行 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EHarnessIsHermetic|TestE2EHarnessUsesDefaultAssemblyOnly|TestE2EFixtureExemptionIsExact|TestMCPUsesProductionAdapterAndTrustedRoots|TestProviderUsesProductionAdapterAndTrustedRoots|TestE2EAC38AuthorizationCollisionAndPermissionProtection|TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38LongConversationRestartExternalizeAndSwitch|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose|TestE2EAC38NativePlatformSecurity)$'`；10 个根测试全部恰好 pass 一次且无 skip。
**覆盖：** F4、F11、F16、F31、F32 / AC4、AC11、AC16、AC31、AC32、AC38(5)。

### T5.24 建立 Windows amd64 原生安全门禁

**文件：** `cmd/xagent/e2e_platform_windows_test.go`, `internal/safefs/root_windows_test.go`, `internal/proctree/runner_windows_test.go`, `internal/tool/bash_windows_test.go`, `internal/hook/command_windows_test.go`, `internal/mcpclient/transport/stdio/transport_windows_test.go`
**依赖：** T5.9、T5.11、T5.13、T5.16、T5.21
**步骤：**
1. 在 Windows amd64 runner 核对 native machine，拒绝 WOW/ARM64 翻译环境，并覆盖路径大小写、UNC/device path、reparse point、AccessCheck、restricted token、Job Object、子孙进程取消和 stdio Close。
2. 保护能力不足时断言 suspended target 未 resume 且 marker 不出现；不得以交叉编译、unsupported stub 或 skip 代替原生结果。
3. 同一 runner 还必须精确运行 AC38 其余四组根测试，required test 缺失、skip 或少跑均失败。

**验证：** Windows amd64 runner 精确执行 `go test -json -race -count=20 ./internal/safefs ./internal/proctree ./internal/tool ./internal/hook ./internal/mcpclient/transport/stdio -run '^(TestNativeSafeFSRejectsPathEscape|TestNativeSafeFSRejectsLinkOrReparseRace|TestNativeProtectedSlotFailsClosed|TestNativeProcessTreeCancellation|TestNativeBashProtectedSlot|TestNativeHookProcessTreeCancellation|TestNativeStdioClose)$'`，required gate 按 T5.21 的 7 个 package/test 二元组判定；随后执行 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EHarnessIsHermetic|TestE2EHarnessUsesDefaultAssemblyOnly|TestE2EFixtureExemptionIsExact|TestMCPUsesProductionAdapterAndTrustedRoots|TestProviderUsesProductionAdapterAndTrustedRoots|TestE2EAC38AuthorizationCollisionAndPermissionProtection|TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38LongConversationRestartExternalizeAndSwitch|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose|TestE2EAC38NativePlatformSecurity)$'`；10 个根测试全部恰好 pass 一次且无 skip。
**覆盖：** F4、F11、F16、F31、F32 / AC4、AC11、AC16、AC31、AC32、AC38(5)。

### T5.25 定义统一检查 profile 与步骤契约

**文件：** `cmd/xagent-check/main.go`, `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`
**依赖：** T0.11、T4.30、T5.6
**步骤：**
1. 定义只含 name、固定 argv、cwd、显式 env、timeout、依赖和 required tests 的 Step；`run.go` 是 static、unit、race、coverage、cross-build、native、e2e、performance、core、final、release 唯一命令清单来源。
2. 校验 profile 图完整、无环且每个必跑步骤恰好一次；空、未知、重复或含未声明覆盖项的 profile 立即失败。
3. CLI 只选择已声明 profile 和明确的 index/commit 审计来源，不接受 shell 字符串、任意命令或隐藏跳过开关。
4. `--audit-source=commit` 必须同时收到 `--revision`，且值严格匹配 `^[0-9a-f]{40}$`；用 `git rev-parse --verify <revision>^{commit}` 解析后的 canonical OID 必须逐字节等于输入。CI/release 模式还必须独立解析实际 `HEAD^{commit}` 并要求两者相等；index 模式拒绝 `--revision`，任何多余或冲突参数 fail closed。

**验证：** `go test -json -race -count=1 ./cmd/xagent-check -run 'TestProfileGraphIsCompleteAndAcyclic|TestUnknownDuplicateAndEmptyProfileFailClosed|TestProfileIsOnlyCommandManifest|TestCommitRevisionRequiresCanonicalFullOID|TestCIAndReleaseRevisionMustEqualHEAD'` 通过。
**覆盖：** F32、F33 / AC32、AC33、AC37。

### T5.25a 接入 audit source、revision 与 workspace baseline

**文件：** `cmd/xagent-check/main.go`, `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`, `internal/repoaudit/workspace.go`
**依赖：** T5.25、T5.16d
**步骤：**
1. 按 Plan profile矩阵固定 index/commit支持；performance/final/release/attestation拒绝 index，commit入口只接受等于实际 HEAD的 canonical 40hex并构造唯一 `GitEvidenceOwner`、`AuditRevision`、`WorkspaceBaseline`。
2. 每个 Step紧邻执行前、返回后和 profile结束均复验 workspace snapshot；index Step cwd只能是 materialized source，commit Step cwd只能是 fresh read-only checkout，所有 cache/report/temp/evidence输出只能进入批准的工作区外 roots。
3. checker对 child显式注入 `XAGENT_AUDIT_SOURCE` 与 commit revision，child不得从 branch/tag/HEAD/cache重新推断。performance同时固定 AuditRevision、baseline `607b3dd3cb9d5f01bcba9a047b816187b4512b4a`与 CurrentRevision三值关系。

**验证：** `go test -json -race -count=20 ./cmd/xagent-check -run 'TestProfilesEnforceAuditSourceMatrix|TestEveryStepVerifiesWorkspaceBeforeAndAfter|TestIndexStepsUseOnlyMaterializedSource|TestCommitRevisionInjectionCannotBeOverridden|TestPerformanceRevisionTripletIsExact'` 通过。
**覆盖：** F1、F2、F31–F33 / AC1、AC2、AC31–AC33、AC36、AC37。

### T5.25b 冻结 canonical argv/env、PathBinding 与 CommandManifest

**文件：** `cmd/xagent-check/run.go`, `cmd/xagent-check/evidence.go`, `cmd/xagent-check/run_test.go`, `internal/repoaudit/evidence.go`, `internal/repoaudit/evidence_test.go`
**依赖：** T5.25a
**步骤：**
1. 实现 Plan 的 token闭集、suffix grammar、`PathBindingEvidence` identity编码和 Env白名单；真实绝对 path先经 handle/ACL/root校验，再投影为 token，record不得保存或散列用户绝对路径。
2. `CommandEvidence`记录实际 executable version/hash、safe argv/env、revision、before/after snapshot、required实际计数与 pass；`<AUDIT_REVISION>` 只在批准的完整 env/argv位置反向模板化，未知 token、错误 stage OID或从 actual临时发明模板均失败。
3. `CommandManifestRecord` 由本 Task 中每个 job的 exact Step清单与唯一 `run.go` 双向生成，compact JSON+LF；静态测试逐 byte比较 Step、executable token、argv/env、bindings、required package/test/ExpectedRuns和顺序，防止 required job替换为 trivial pass。
4. T3.19 的 8 个、T3.19a 的 12 个和 T4.29a 的 5 个 canonical 根 tuple，以及 T3.19/T3.19a 固定的 48 个完整 `Root/Subtest` paths，共 73 项，逐项加入 T5.28 `unit-1`、`unit-2`、`unit-3` 每个 Step 的 Required 闭集且各自 `ExpectedRuns=1`；三步分别记录，不能折算成 ExpectedRuns=3、只登记根、只登记一个 unit Step 或改绑到不执行全仓测试的 profile。`TestRequiredManifestCannotBeWeakened` 逐 byte冻结这三份相同的 C18 additions；某 required root 一旦存在至少一个 manifest descendant entry，其全部实际 descendant 就形成闭集，任何未登记 descendant 均计为 Excess。

**验证：** `go test -json -race -count=20 ./cmd/xagent-check ./internal/repoaudit -run '^(TestSafeArgvEnvProjectionRejectsUnknownPathsAndSecrets|TestPathBindingIdentityIsCanonical|TestAuditRevisionTemplateHasOnlyApprovedPositions|TestCommandManifestMatchesRunGoByteForByte|TestRequiredManifestCannotBeWeakened)$'` 通过；required 映射精确为 `(xagent/cmd/xagent-check, TestSafeArgvEnvProjectionRejectsUnknownPathsAndSecrets, 20)`、`(xagent/internal/repoaudit, TestPathBindingIdentityIsCanonical, 20)`、`(xagent/cmd/xagent-check, TestAuditRevisionTemplateHasOnlyApprovedPositions, 20)`、同包 `TestCommandManifestMatchesRunGoByteForByte` 与 `TestRequiredManifestCannotBeWeakened` 各 20；错包同名、missing、skip/fail、少跑或多跑均失败。
**覆盖：** F7、F31–F33 / AC7、AC31–AC33、AC37。

### T5.26 实现跨平台 runner 与 required-test 判定

**文件：** `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`
**依赖：** T5.25b、T1.24
**步骤：**
1. 以固定 argv 直接启动子命令，不经过 shell；显式传递最小环境和工作目录，流式净化并限制 stdout/stderr，区分 finding、测试失败、超时、取消和 runner 故障。
2. 取消或超时时终止并 Wait 完整子进程树，不用短 sleep 推断退出；失败摘要只能包含 step、退出分类和有界安全输出。
3. 解析 `go test -json` 并按完整 `(Package, Test/Subtest)` 计数；每项必须 `ActualRuns == ExpectedRuns` 且恰有对应 pass、无 skip/fail，missing、excess/duplicate、错包同名、只有 package pass 或进程非零都失败。若某 required root 在 manifest 中至少有一个 descendant entry，则其实际 descendant 集必须与 manifest 中该 root 的 descendant 集完全相等，任何未批准 descendant 都计为 Excess；额外普通测试只有在名称不冒充 required root/path 且不位于这种闭合 subtest tree 下时才不计 Excess。fail-closed 场景只有测试内部证明 target 未启动后正常 pass 才算通过。

**验证：** `go test -json -race -count=20 ./cmd/xagent-check -run '^(TestRunnerPreservesArgvAndDoesNotUseShell|TestRunnerCancellationReapsCommand|TestJSONGateRejectsSkipMissingExcessWrongPackageAndPackageOnlyPass|TestJSONGateAcceptsFailClosedEvidence)$'` 通过；required 映射精确为 `(xagent/cmd/xagent-check, TestRunnerPreservesArgvAndDoesNotUseShell, 20)`、同包的 `TestRunnerCancellationReapsCommand`、`TestJSONGateRejectsSkipMissingExcessWrongPackageAndPackageOnlyPass`、`TestJSONGateAcceptsFailClosedEvidence` 各 20；任何 missing、skip/fail、ActualRuns≠20、错包同名、package-only pass 或额外 required descendant 都失败。
**覆盖：** F7、F31、F32 / AC7、AC31、AC32、AC37。

### T5.26a 归一化平台依赖与模块元数据

**文件：** `go.mod`, `go.sum`
**依赖：** T4.30、T5.24、T5.26
**步骤：**
1. 从本 revision 的真实 import 图核对 Plan 批准的平台依赖；只保留实现已批准 safefs/proctree/终端路径所必需的直接与间接依赖，不顺带升级版本或引入范围外库。
2. 执行一次 `go mod tidy` 并逐项审查 `go.mod`/`go.sum` diff；不得借此修改源码、测试、配置或生成仓库内缓存文件。
3. 用只读模块模式复核三平台全部包和入口，证明后续 static profile 不会为了构建或 tidy 再写模块文件。

**验证：** `go mod verify` 与 `go mod tidy -diff` 均成功；`GOFLAGS=-mod=readonly GOOS=darwin GOARCH=amd64 go build ./...`、`GOFLAGS=-mod=readonly GOOS=linux GOARCH=amd64 go build ./...`、`GOFLAGS=-mod=readonly GOOS=windows GOARCH=amd64 go build ./...` 全部通过，且验证前后 `go.mod`/`go.sum` 摘要不变。
**覆盖：** F31、F32 / AC31、AC32、AC37。

### T5.27 接入只读格式、静态与模块门禁

**文件：** `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`
**依赖：** T5.26a、T0.11
**步骤：**
1. static profile 以 Go runner 枚举受版本控制的 Go 文件并执行只读 gofmt 比较，再执行 `go vet ./...`、`go mod verify` 和 `go mod tidy -diff`；不得自动格式化、tidy 或写源码。
2. 固定命令顺序、超时与干净环境，任一 diff、依赖校验失败或安全输出超限均返回非零。
3. profile 测试记录工作树/index 摘要，证明检查前后不变且不会接触 M0 保护的用户本地数据。
4. 基于 `go list -deps -json` 与 Go import 解析冻结 forbidden-edge：permission 不得反向依赖 tool，TUI 不得依赖 Config/Store/Provider/MCP/Artifact/Orchestrator 等领域服务，MCP transport 不得依赖 Manager，基础及领域底层包不得反向依赖 Config/App/TUI；任一新增反向边使 static profile 失败。

**验证：** `go test -json -race -count=1 ./cmd/xagent-check -run 'TestStaticProfileIsReadOnlyAndExact|TestStaticProfileRejectsFormattingVetAndModuleDrift|TestForbiddenImportEdgesMatchApprovedArchitecture'` 通过；`go run ./cmd/xagent-check --profile=static` 成功且工作树/index 摘要不变。
**覆盖：** F1、F2、F32 / AC1、AC2、AC32、AC37。

### T5.28 接入普通、race 与重复稳定性门禁

**文件：** `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`
**依赖：** T5.6、T5.26a
**步骤：**
1. unit profile 固定且只含依次执行的三个唯一 Step：`unit-1`、`unit-2`、`unit-3`；三者 argv 均逐字节等于 `go test -json -count=1 ./...`，cwd/env/timeout/required manifest 相同。每个 Step 的 Required 都逐项包含 T5.25b 冻结的 25 个 C18 根与 48 个 canonical subtest paths 且各 `ExpectedRuns=1`。每步必须启动新进程并分别解析完整 JSON；任一失败、skip、missing、excess、错包同名、package-only pass 或超时即失败，禁止合并 Step、把三次折算成 ExpectedRuns=3、提高 `-count`、省略 `-count=1` 或复用 Go test cache 结果。
2. race profile 对全仓执行一次 race，并对固定 MCP stdio、取消、Close、并发、burst、blocked write、rollback 和生命周期名单执行恰好 20 次；required 名单缺失或少跑即失败。
3. 所有默认测试移除真实凭据/Provider 和代理环境，只允许临时 Root、固定 canary、loopback 与 helper；发现公网依赖立即失败。

**验证：** `go test -json -race -count=1 ./cmd/xagent-check -run 'TestUnitRunsThreeIndependentSuites|TestRaceProfileRunsSensitiveManifestTwentyTimes|TestStableProfilesRejectSkipTimeoutAndMissingTest'` 通过；unit 与 race profile 实际成功。
**覆盖：** F7、F32 / AC7、AC32、AC37。

### T5.29 接入 80% 覆盖率硬门槛

**文件：** `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`
**依赖：** T5.28
**步骤：**
1. 从 `go list ./...` 固定本 revision 的 package universe，在检查器临时目录生成 coverage profile，不向仓库写 `coverage.out` 或上传报告。
2. 严格解析 profile 与 total，要求所有应覆盖 package 均出现且整体 statement coverage `>=80.0%`；NaN、畸形、缺包、命令失败或低于阈值均失败。
3. 授权、脱敏、持久化、预算和生命周期的 required direct tests 必须同时出现并 pass，不能只靠无关包抬高总数。

**验证：** `go test -json -race -count=1 ./cmd/xagent-check -run 'TestCoverageRejectsBelowEightyMalformedAndMissingPackage|TestCoverageRequiresSensitiveDirectTests|TestCoverageWritesOnlyTempDir'` 通过；`go run ./cmd/xagent-check --profile=coverage` 报告不低于 80%。
**覆盖：** F32 / AC32、AC37。

### T5.30 接入跨平台构建、原生身份与五组 E2E

**文件：** `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`
**依赖：** T5.9、T5.11、T5.13、T5.16、T5.21–T5.24、T5.26a
**步骤：**
1. cross-build profile 在临时目录为 darwin/linux/windows amd64 构建全部包和入口；结果只计编译证据，不能满足原生测试。
2. native profile 先核对 runtime、GOHOST 与实际宿主机器，macOS 拒绝 Rosetta/ARM host、Windows 拒绝 WOW/ARM64、Linux 要求 x86_64；随后以固定 argv `go test -json -race -count=20 ./internal/safefs ./internal/proctree ./internal/tool ./internal/hook ./internal/mcpclient/transport/stdio -run '^(TestNativeSafeFSRejectsPathEscape|TestNativeSafeFSRejectsLinkOrReparseRace|TestNativeProtectedSlotFailsClosed|TestNativeProcessTreeCancellation|TestNativeBashProtectedSlot|TestNativeHookProcessTreeCancellation|TestNativeStdioClose)$'` 执行 T5.21 的 7 个精确 package/test 二元组。错误平台/架构、任一 missing/skip/次数不足或由错误 package 顶替均失败。
3. e2e profile 的唯一测试 argv 是 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EHarnessIsHermetic|TestE2EHarnessUsesDefaultAssemblyOnly|TestE2EFixtureExemptionIsExact|TestMCPUsesProductionAdapterAndTrustedRoots|TestProviderUsesProductionAdapterAndTrustedRoots|TestE2EAC38AuthorizationCollisionAndPermissionProtection|TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38LongConversationRestartExternalizeAndSwitch|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose|TestE2EAC38NativePlatformSecurity)$'`；required manifest 精确含 T5.7 的 3 个 harness 根、T5.14/T5.15 的 2 个 production-adapter/TrustedRoots 根和 5 个 AC38 根，各恰好 pass 一次。任一 missing、skip、少跑、额外 required 替代或仅交叉编译均失败；能力不足只接受测试内部证明 target 未启动。

**验证：** `go test -json -race -count=1 ./cmd/xagent-check -run 'TestCrossBuildCannotCountAsNative|TestNativeIdentityRejectsWrongArchAndTranslation|TestNativeProfileRequiresExactPackageTestManifest|TestE2EProfileRequiresHarnessAdaptersAndFiveAC38RootsWithoutSkip'` 通过；当前机器仅在真实 amd64 时可通过 native/e2e profile。
**覆盖：** F4、F11、F16、F31、F32 / AC4、AC11、AC16、AC31、AC32、AC38。

### T5.31 接入 performance、repoaudit 与最终 profile

**文件：** `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`, `internal/repoaudit/check_profile_test.go`
**依赖：** T5.20、T5.27–T5.30
**步骤：**
1. performance profile 固定依次执行四个唯一 Step：`performance-protocol` 的 argv 为 `go test -json -race -tags=e2e,performance -count=1 ./internal/e2e/perfprotocol ./internal/e2e/perfworkload -run '^(TestPerformanceProtocolStateMachineAndNoDuration|TestPerformanceWorkloadPostconditionsAndDigest)$'`；`performance-baseline-adapter` 为 `go test -json -tags=e2e,performance -count=1 ./internal/e2e -run '^TestPerformanceBaselineArchiveAndAdapterBridge$'`；`performance-current-adapter` 为 `go test -json -race -tags=e2e,performance -count=1 ./cmd/xagent -run '^(TestPerformanceCurrentAdapterUsesDefaultAssembly|TestPerformanceAdaptersEmitOnlyNDJSON)$'`；最后 `performance-compare` 为 `go test -json -tags=e2e,performance -count=1 ./cmd/xagent -run '^TestPerformanceAgainstGovernanceBaseline$'`。required manifest 按 package/test 精确判定全部 6 个根测试；任一缺失、skip 或错序均失败。
2. performance 的 archive/coordinator 固定 baseline revision `607b3dd3cb9d5f01bcba9a047b816187b4512b4a`；预取 current/baseline module 也委托该 coordinator，随后共享 cache 只读且 adapter 固定 `GOPROXY=off`，禁止回退、重试或缓存旧性能结论。前三步先证明协议、production adapter、archive 和离线 baseline 不变量，最终比较不能替代这些不变量测试。
3. core/final/release 直接调用同一 `xagent-repo-check`；本地来源只能显式 index。每个 CI/release job 先取得实际 `HEAD^{commit}` 的 canonical 40 位小写 OID，再把该字面值同时传给 profile 与 commit repoaudit；检查器必须重新解析 HEAD 并逐字节比对，不能接受 workflow 声明值、branch、tag、短 SHA 或不同 revision。流程不依赖本地 hook，也不默认扫描会命中用户保留数据的 worktree。
4. final/release DAG 恰好包含 static、unit×3、race、敏感×20、coverage、cross-build、当前原生 native package manifest、含 harness/adapter 不变量的 E2E、四步 performance、repoaudit 和文档契约；任一步失败、skip 或超时使最终退出非零。

**验证：** `go test -json -race -count=1 ./cmd/xagent-check ./internal/repoaudit -run 'TestPerformanceProfileRequiresProtocolAdaptersBaselineAndComparison|TestPerformanceProfileUsesExactOfflineBaseline|TestFinalProfileContainsEveryGateOnce|TestCIAndReleaseRevisionMustEqualHEAD|TestCIAndReleaseAuditCommitWithoutHook|TestProfilesNeverScanUserWorktreeByDefault'` 通过。
**覆盖：** F1、F2、F31–F33 / AC1、AC2、AC31–AC33、AC36–AC38。

### T5.31a 定义 typed evidence DTO 与严格 canonical codec

**文件：** `internal/repoaudit/evidence.go`, `internal/repoaudit/evidence_test.go`, `cmd/xagent-check/evidence.go`, `cmd/xagent-check/evidence_test.go`
**依赖：** T0.3a、T5.17a、T5.25b、T5.31
**步骤：**
1. 逐字段实现 Plan C17 的 Command/Job/Provider/Performance/Deletion/R0Transition/Preservation/Attestation/Verification/WorkflowLock/MarkerRender/ArchivePreparation/ImmutableReceipt/ArchiveFinalization闭合 DTO；禁止 map、optional unknown field与 nil slice。
2. 所有 evidence payload 使用 compact JSON+唯一 LF、声明顺序、UTF-8且不 HTML escape；SHA-256只覆盖 payload。统一 decoder以 16 MiB cap+1、iterative depth=64拒绝 duplicate/unknown/null/BOM/CRLF/whitespace/第二值/trailing/错误类型与 overflow，解入临时 DTO并完成关系校验后才返回。
3. 固定 40hex OID、64hex digest、安全 ASCII/长度/计数、枚举和允许空字段全集；archive preparation/finalization必须都含 ManifestSHA256、TargetCount、TargetDigest，external none/verified字段关系逐项验证。

**验证：** `go test -json -race -count=20 ./internal/repoaudit ./cmd/xagent-check -run 'TestEveryEvidenceDTOHasCanonicalGoldenBytes|TestEvidenceCodecRejectsDuplicateUnknownNullDepthAndTrailingData|TestEvidenceCodecNeverCommitsPartialDTO|TestArchiveRecordsCarryTargetCountAndDigest|TestEvidenceEnumsLengthsAndEmptyFieldsAreClosed'` 通过。
**覆盖：** F7、F31–F33 / AC7、AC31–AC34、AC36、AC37。

### T5.31b 冻结 ExpectedEvidenceManifest 与 workflow dependency lock

**文件：** `internal/repoaudit/ci_workflow.go`, `internal/repoaudit/ci_workflow_test.go`, `internal/repoaudit/evidence.go`, `internal/repoaudit/evidence_test.go`, `cmd/xagent-check/run.go`, `cmd/xagent-check/run_test.go`
**依赖：** T5.31a
**步骤：**
1. `ExpectedEvidenceManifest` 全局恰为 97 个 job：rpre/r0/e各按 core、final、commit-repoaudit、docs、darwin-native、darwin-e2e、linux-native、linux-e2e、windows-native、windows-e2e、performance顺序各 11；D01–D17各先 deletion、required，D05/D14再加 Darwin/Linux native+e2e，D06/D15再加 Windows native+e2e，D11–D13再加三平台 native+e2e。每项 CommandManifestSHA256必须来自 T5.25b批准 manifest，缺项、额外项、换序或重复失败。
2. 实现 1 MiB/UTF-8/单文档/depth-64 的 root/local workflow YAML parser，拒绝 duplicate key、anchor/alias/merge/custom tag、dynamic/non-string uses与路径歧义；实际 `uses` 按 source context独立解析 target descriptor/五值 Kind和 SHA identity，递归 local closure并要求 lock graph完整可达、无环、无额外节点。
3. `WorkflowDependenciesSHA256` 按 Plan length-prefixed frame绑定 historical root workflow blob、全部 local blob、lock blob/OID和 canonical graph；importer只读本地 Git object database、不联网。远端 manifest内容由下表人工审核冻结，当前运行时不声称重新下载验证。
4. Task 批准即冻结 `.github/workflows/ci.yml` 的 root step布局：唯一 matrix job id=`quality`，step 0/1/2/3依次为 checkout、setup-go、运行 checker并生成 safe summary、upload artifact；RootWorkflowPath固定 `.github/workflows/ci.yml`，LocalManifests固定 `[]`，RootUses按下表三项，remote Entries按 Identity raw bytes升序且 Uses均为 `[]`。T5.36 只能据此生成 lock并填入由最终 ci.yml实际 blob计算的 RootWorkflowBlobOID，不能更换 action/ref/path/hash或移动 step index。

| SourceIdentity | Uses | TargetIdentity |
|---|---|---|
| `jobs/quality/steps/0/uses` | `actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683` | `c4d1003f0afaba66bbcb7c472a8018651d69e9429fb1fdc770c4ef4d5d109266` |
| `jobs/quality/steps/1/uses` | `actions/setup-go@0a12ed9d6a96ab950c8f026ed9f722fe0da7ef32` | `8e02403ab19c99ba9c17fe2d6b0161940dd2be1cb87b1bb76ea161684ead891e` |
| `jobs/quality/steps/3/uses` | `actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02` | `2c695fa550662134d546865a8242b71a9001ddad7fb1843f9553da55a2d89853` |

| Identity | Kind | Repository | Revision | ManifestPath | ManifestSHA256 |
|---|---|---|---|---|---|
| `2c695fa550662134d546865a8242b71a9001ddad7fb1843f9553da55a2d89853` | `remote-action` | `actions/upload-artifact` | `ea165f8d65b6e75b540449e92b4886f43607fa02` | `action.yml` | `29b21270ff44638a165ea12abb4481796770c629e513217af7b4df1aa3821177` |
| `8e02403ab19c99ba9c17fe2d6b0161940dd2be1cb87b1bb76ea161684ead891e` | `remote-action` | `actions/setup-go` | `0a12ed9d6a96ab950c8f026ed9f722fe0da7ef32` | `action.yml` | `ebb00c4462c87740b2cd811941f13ecb03a9d1b417a0db745c5c2df858cdebea` |
| `c4d1003f0afaba66bbcb7c472a8018651d69e9429fb1fdc770c4ef4d5d109266` | `remote-action` | `actions/checkout` | `11bd71901bbe5b1630ceea73d27597364c9af683` | `action.yml` | `5349b6eea0a1797a9a993c48db9d95b33d27ee1b6227ceced0c9cbf8e655c939` |

**验证：** `go test -json -race -count=20 ./internal/repoaudit ./cmd/xagent-check -run 'TestExpectedEvidenceManifestHasExactNinetySevenJobs|TestWorkflowLockRejectsMissingExtraCycleAndWrongTarget|TestWorkflowUsesResolveToFrozenTargets|TestWorkflowDependencyDigestBindsWorkflowLocalAndLockBlobs|TestFrozenRemoteActionManifestsAndIdentitiesMatchTask'` 通过。
**覆盖：** F2、F31–F33 / AC2、AC31–AC33、AC37。

### T5.31c 实现 provider safe evidence 与 pre-E/post-E 导入

**文件：** `cmd/xagent-check/evidence.go`, `cmd/xagent-check/evidence_test.go`, `internal/repoaudit/attestation.go`, `internal/repoaudit/attestation_test.go`
**依赖：** T5.31b
**步骤：**
1. CI每个 required job只上传名为 `xagent-safe-evidence-<stage>-<job>` 的单一 typed summary；performance再含单一 typed report。artifact禁止 raw stdout/stderr、coverage、binary、fixture、path、manifest、用户数据或凭据，provider页面/summary必须显示 SafeSummarySHA256、repository/workflow/job/run/attempt、checkout OID、dependency digest和 pass。
2. `import-job-evidence` stdin只接受一个 canonical `JobEvidenceImportRequest`+LF+EOF，不接受 path/URL/credential；外层 Stage/Job、Summary、Provider、safe summary digest、performance report和共有字段逐项一致，owner不联网也不保存 raw provider bytes。
3. 任一 publish前从 R0或E fresh checkout重建 stage/OID表、Rpre→D01–D17→R0 ancestry/delta、R0 transition、E marker delta、job/platform/CommandManifest与 workflow lock；wrong run/stage/checkout在写任何 temp/final前失败。
4. R0只导入 rpre/d01..d17/r0，E只导入 e且先复验全部 pre-E final；summary/report/provider三文件组按统一 publisher恢复正确子集，任何既有不一致项不可覆盖。所有导入从 ledger首读到最后 parent sync持 T0.3a同一 lease并服从 archive phase gate。

**验证：** `go test -json -race -count=20 ./cmd/xagent-check ./internal/repoaudit -run 'TestImportJobEvidenceAcceptsOnlyOneTypedFrame|TestProviderEvidenceBindsSummaryAndManualFields|TestImportValidatesAncestryStageJobAndWorkflowBeforePublish|TestPreEAndPostEImportsCannotCrossStages|TestImportRecoversOnlyExactPublishedSubset|TestSafeArtifactRejectsEveryRawOrExtraFile'` 通过。
**覆盖：** F7、F31–F33 / AC7、AC31–AC33、AC36–AC38。

### T5.31d 实现 preservation result、R0 transition 与 durable ledger phase gate

**文件：** `cmd/xagent-repo-check/main.go`, `cmd/xagent-check/evidence.go`, `cmd/xagent-check/evidence_test.go`, `internal/repoaudit/preservation.go`, `internal/repoaudit/attestation.go`, `internal/repoaudit/attestation_test.go`
**依赖：** T5.31c
**步骤：**
1. `verify-final` 只允许 after_r0/after_e两个批准调用点，分别原子发布 `preservation/after_r0.json`、`after_e.json`；二者必须同 ManifestSHA256、TargetCount=35、TargetDigest，safe argv/bindings、exit 0、result pass各自独立。
2. `record-r0-transition` 只允许 R0 pending-marker fresh checkout且在任何 pre-E import前运行，stdin必须 EOF；在 1 MiB有界捕获内用冻结 toolchain/cache离线重跑 D17/R0 tidy，验证 alias或唯一 tidy child、exact diff/delta后原子发布 typed record，raw diff只在 task temp/内存并精确清理。
3. 将 T0.3a publisher扩展到 summary/report/provider/preservation/R0/attestation全部目标；文件路径由 ExpectedEvidenceManifest展开而非 glob。prepared或其 temp出现后只允许 finalize/phase④ verify-archive，所有 ledger入口从首次观察到最后 sync都持同一 exclusive lease。
4. 冻结 ledger目录闭集、每目标唯一 temp及 POSIX/Windows权限；active/lease/control root不属于 archive且不可删除，manifest在 finalize前不得移动、覆盖或提前清理。

**验证：** `go test -json -race -count=20 ./cmd/xagent-check ./cmd/xagent-repo-check ./internal/repoaudit -run 'TestVerifyFinalPublishesExactlyAfterR0AndAfterE|TestR0TransitionRecordsAliasOrExactTidyChild|TestR0TransitionNeverPersistsRawDiff|TestEveryLedgerWriterSharesOneLeaseAndPhaseGate|TestLedgerDirectoryClosureRejectsExtraFilesAndTemps'` 通过。
**覆盖：** F1、F31–F33 / AC1、AC31–AC34、AC36、AC37。

### T5.31e 实现 marker render、attestation record 与可选 readback

**文件：** `cmd/xagent-check/evidence.go`, `cmd/xagent-check/evidence_test.go`, `internal/repoaudit/attestation.go`, `internal/repoaudit/attestation_test.go`, `internal/repoaudit/docs.go`, `internal/repoaudit/docs_test.go`
**依赖：** T5.31d
**步骤：**
1. `render-evidence-markers` 只允许 R0、持 lease只读复验完整 pre-E ledger并向 stdout输出唯一 canonical `EvidenceMarkerRenderResult`+LF+EOF；documents恰为 readme/docs-index/checklist三项 pass形态。冻结 updater只解析该 DTO、重新 canonical编码三项并替换唯一完整 pending block，marker外任一 byte变化失败。
2. `generate` 只允许 E且从全部 typed evidence和同一 handle raw manifest重建唯一 attestation；absent发布、exact final崩溃重跑幂等，later phase或不一致状态失败。`verify-record` 独立重算全部 chain/digest/performance并同样封闭 absent/exact-final恢复。
3. `verify-readback` 不联网且无 path/URL/provider/credential argv，stdin必须依次为 canonical AttestationRecord+LF、ImmutableReceiptEvidence+LF、EOF；两帧先全部验证，readback逐 byte等于 record、receipt digest/time/retention关系正确后才顺序发布 pair，正确单项崩溃状态可补齐。
4. optional external分支只存 typed safe fields；未选择时 pair必须都不存在，选择时二者必须都在 prepared前完成。任何 provider raw response、header、query、credential或日志拒绝。

**验证：** `go test -json -race -count=20 ./cmd/xagent-check ./internal/repoaudit -run 'TestMarkerRenderHasOneCanonicalOutputAndThreeDocuments|TestMarkerUpdaterChangesOnlyPendingBlocks|TestGenerateAndVerifyRecordRecoverExactFinalOnly|TestVerifyReadbackRequiresExactlyTwoCanonicalFrames|TestReadbackPairRecoversOnlyValidatedSubset|TestOptionalExternalModeIsClosed'` 通过。
**覆盖：** F29、F31–F33 / AC29、AC31–AC34、AC36–AC38。

### T5.31f 实现 archive 四阶段 finalize 与只读复验

**文件：** `cmd/xagent-check/evidence.go`, `cmd/xagent-check/evidence_test.go`, `internal/repoaudit/attestation.go`, `internal/repoaudit/attestation_test.go`, `internal/repoaudit/evidence_unix.go`, `internal/repoaudit/evidence_windows.go`
**依赖：** T5.31e
**步骤：**
1. `finalize-archive` 只接受 Plan四阶段：① manifest有/prepared无/finalization无；② manifest有/prepared有；③ manifest无/prepared有；④ manifest无/prepared/finalization有。phase-aware temp恢复优先于通用 publisher，其他组合fail closed。
2. phase①全量复验并把 Attestation/Verification/Manifest/TargetCount/TargetDigest/optional pair写入 preparation；phase②在同一 lease内再次全量复验后才精确删除 manifest并同步 preservation dir；phase③用 preparation保存的 manifest三字段复验全部剩余 archive并发布 finalization；phase④重验全部 bytes/digest/closure。
3. finalization后 children-first/root-last只读化：POSIX file=0400、dir=0500，Windows只授当前用户/必要系统主体 read/traverse/read-control并拒绝 write/append/delete/delete-child。中断可由 finalize或 `verify-archive` 幂等继续；verify-archive不能创建/改写 record或删除其他文件。
4. archive/control/lease/active不提供删除 action；用户未来请求删除必须另开 Spec。测试覆盖 phase①–④每个 file/dir sync边界、optional none/verified及 permission tightening中断。

**验证：** `go test -json -race -count=20 ./cmd/xagent-check ./internal/repoaudit -run 'TestFinalizeArchiveAcceptsOnlyFourPhases|TestFinalizeNeverDeletesManifestBeforeSecondFullValidation|TestPhaseThreeUsesPreparationManifestFields|TestArchiveFinalizationBindsEveryCorrespondingField|TestVerifyArchiveOnlyContinuesReadOnlyTightening|TestFinalizeCrashMatrixHasNoSplitLeaseOrUnrecoverableLegalState'` 通过。
**覆盖：** F1、F31–F33 / AC1、AC31–AC34、AC36–AC38。

### T5.32 同步完整安全配置示例

**文件：** `config.example.yaml`, `internal/config/example_test.go`
**依赖：** T2.7、T4.24、T4.25h、T4.27、T4.30
**步骤：**
1. 让示例覆盖 C8、C14 与现有 schema 的每个公开键恰好一次，并逐项准确说明本文件数值快照中的默认值、有效最小值、硬上限、显式 0 语义、四层优先级、同名 MCP 整项覆盖和 legacy 冲突。
2. 凭据仅使用 `${ENV}` 安全示例，不含可用 token、私有路径或真实 endpoint；Artifact 默认位置、显式 root 约束和迁移行为与生产契约一致。
3. 建立 schema↔example 双向检查：示例必须严格加载，公开键增删时测试失败，未知/null/重复键和 literal credential 均拒绝。

**验证：** `go test -json -count=1 ./internal/config -run 'TestExampleConfigStrictlyLoads|TestExampleCoversEveryPublicKeyExactlyOnce|TestExampleContainsNoLiteralCredential|TestExampleDocumentsLegacyConflict'` 通过。
**覆盖：** F28、F33 / AC28、AC33、AC35。

### T5.33 实现 C11 显式 pre-commit 门禁

**文件：** `.githooks/pre-commit`, `README.md`, `internal/repoaudit/hook_test.go`
**依赖：** T0.9、T5.31f
**步骤：**
1. 受版本控制的 hook 除 shebang 外只 `exec go run ./cmd/xagent-repo-check --source=index`，不复制规则、不自动修复，文件 mode 为可执行。
2. README 明示安装 `git config core.hooksPath .githooks`、查询和撤销命令；任何程序、测试和 CI 都不得静默修改用户 Git 配置。
3. 证明未安装 hook 时 CI/release 仍无条件执行 commit repoaudit，安装 hook 也只审计 index 且不读取工作区用户数据。

**验证：** `go test -json -count=1 ./internal/repoaudit ./cmd/xagent-check -run 'TestPreCommitHookIsSingleDelegation|TestHookInstallAndRemovalAreExplicit|TestChecksNeverMutateGitConfig|TestCIAndReleaseAuditWithoutHook'` 通过，并确认 index mode 为 `100755`。
**覆盖：** F2、F32、F33 / AC2、AC32、AC33。

### T5.34 同步 README 的真实公开事实

**文件：** `README.md`, `internal/repoaudit/readme_test.go`
**依赖：** T4.18、T4.22、T5.16、T5.20、T5.24、T5.31f、T5.32、T5.33
**步骤：**
1. 从 command metadata 同步公开命令、别名、快捷键、权限模式、`/status`、help/version 和 checker profiles，不公开隐藏 launcher、兼容 diagnostics 或测试入口。
2. 准确写明三个 amd64 平台与 fail-closed 行为、配置优先级/硬上限、Artifact 私有边界、Provider/MCP 网络策略、旧配置/权限/会话/ExternalPath 非破坏迁移及 C11 安装/撤销。
3. 只陈述已有可复验证据；未取得三平台或性能结果时标明待验证，不得把 task/checklist 计划写成完成事实。

**验证：** `go test -json -count=1 ./internal/repoaudit ./internal/command ./internal/config -run 'TestREADMEMatchesCommandMetadataConfigSchemaProfilesAndCurrentStatus|TestREADMEContainsNoHiddenOrUnsupportedInterface'` 通过。
**覆盖：** F29–F33 / AC29–AC33。

### T5.35 建立文档索引与机器追溯审计

**文件：** `docs/index.md`, `internal/repoaudit/docs.go`, `internal/repoaudit/docs_test.go`, `docs/**/{spec,plan,task,checklist}.md`
**依赖：** T0.11、T5.34
**步骤：**
1. 为每套规格记录 current、historical 或 superseded；`superseded_by` 必须是存在的仓库相对路径且关系无环，历史四件套不得删除或批量误标。
2. 实施时 checklist 已批准，审计器必须校验 F1–F33→AC→task→checklist→当前证据；已勾选项需含 exact revision、可复现命令和结果，旧勾选无当前证据时只能标历史/被替代。
3. 当前四件套的编号、覆盖声明和替代链必须双向一致；计划文本不能充当实施证据，审计输出不得包含文档中的 secret 样本文本。
4. 机器审计显式实现 T5.53a–T5.54 的 R0/E/Archive 证据规则：仓库内已勾选 checklist 只绑定通过全部候选门禁的40位`R0`，evidence commit`E`只能修改`README.md`、`docs/index.md`和当前`checklist.md`各自唯一marker结果；仓库外attestation绑定40位`E`并进入只读archive。`E`不得把自身OID写回自身，也不得以`E`替换checklist内嵌的`R0`；审计器根据`R0..E`diff拒绝任何源码、测试、配置、CI、Plan或Task变化。

**验证：** `go test -json -count=1 ./internal/repoaudit -run 'TestDocsIndexHasEveryQuartetAndNoDanglingSupersession|TestCurrentDocsHaveCompleteTraceabilityAndEvidence|TestCheckedItemsRequireCurrentReproducibleEvidence|TestHistoricalSpecsRemainPresent|TestEvidenceProtocolBindsChecklistToR0AndAttestationToE|TestEvidenceCommitChangesOnlyApprovedEvidenceDocs'` 通过。
**覆盖：** F29、F33 / AC29、AC33。

### T5.35a 建立三份证据文档的唯一 pending marker

**文件：** `README.md`, `docs/index.md`, `docs/security-reliability-delivery-quality-2026-07-30/checklist.md`, `internal/repoaudit/docs.go`, `internal/repoaudit/docs_test.go`
**依赖：** T5.35
**步骤：**
1. 在 README、docs 索引和当前 checklist 各建立一个 canonical pending evidence block；marker 名、顺序、换行和 pending payload 必须固定，全文每类 marker 恰好出现一次且首尾成对。
2. pending block 只声明尚待 R0 实际证据，不伪造 revision、job、结果或完成状态；marker 外的公开事实、历史规格与用户数据保持不变。
3. 实现冻结 updater 的结构预检：只有 T5.31e 的三项 canonical render result 能把三个完整 pending block 一次性替换为 pass block，缺失、重复、嵌套、错序、部分替换或 marker 外 byte 变化均拒绝。

**验证：** `go test -json -race -count=20 ./internal/repoaudit -run 'TestEvidenceDocsHaveExactlyOneCanonicalPendingBlockEach|TestPendingMarkersContainNoClaimedResult|TestMarkerUpdaterRejectsMissingDuplicateNestedAndOutsideChanges'` 通过，并确认三份文档仍通过当前追溯审计。
**覆盖：** F29、F33 / AC29、AC33、AC37。

### T5.36 建立 CI 原生矩阵与删除前资格门禁

**文件：** `.github/workflows/ci.yml`, `.github/xagent-actions.lock.json`, `internal/repoaudit/ci_workflow_test.go`, `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.22–T5.24、T5.31–T5.35a
**步骤：**
1. 生成唯一 `quality` matrix job；step 0/1/2/3 严格为冻结 checkout、setup-go、运行 checker并生成单一 typed safe summary、upload-artifact，所有远端 `uses` 的仓库、40hex revision、SourceIdentity、TargetIdentity 与 manifest SHA-256 必须逐项等于 T5.31b，禁止新增 remote/local action、移动 step index或动态 `uses`。workflow 对所有分支 `push`、`pull_request` 自动触发，可加 `workflow_dispatch`，但不得以路径或条件跳过批准的 97 个 stage/job。
2. 从最终 `ci.yml` 的实际 Git blob生成 canonical `.github/xagent-actions.lock.json`：RootWorkflowPath、RootWorkflowBlobOID、RootUses、空 LocalManifests、三项 remote Entries与 canonical dependency graph全部满足 T5.31b；离线 importer从本地 Git ODB复算 `WorkflowDependenciesSHA256`，运行时不联网且不把人工冻结声明冒充重新下载验证。
3. 在测试中冻结 Plan 的17个删除候选、顺序、替代实现和状态 `present_no_live_references`；拒绝缺项、增项、重复、越序、init/注册副作用，以及把 legacy fixture、历史规格或用户数据列为候选。状态根固定 `TestDeletionCandidateState` 及 `Prefix00PreDelete` 到 `Prefix17ConversationFileStore` 的18个精确 subtest，每个同时断言文件存在性、候选状态、零生产引用和已删除项恰为批准顺序前缀。
4. 在仅含批准实现、最终 workflow/lock、候选表和三个 pending marker的单父提交上形成 `Rpre`；每个 matrix cell从 fresh checkout独立取得 actual `HEAD^{commit}`，验证为与 `Rpre` 相同的40位小写 OID，再按 ExpectedEvidenceManifest运行 rpre 的 core、final、commit-repoaudit、docs、Darwin/Linux/Windows native+e2e和performance共11个job。performance使用完整历史；所有job固定只读权限/toolchain，禁止 secrets、发布、raw/private artifact、arm64、翻译执行、skip、缺失或不同 revision拼接。
5. 只收集并校验 rpre 的 typed safe artifact与 provider identity；每组 summary/report/provider按 T5.31c导入 evidence ledger，拒绝跨stage、未在manifest内、wrong checkout/dependency digest/CommandManifest、额外文件或raw输出。11项全部导入且 preservation freeze/remove-index/verify-immediate记录齐全后，才允许进入 T5.37。

**验证：** `go test -json -count=1 ./internal/repoaudit ./cmd/xagent-check -run '^(TestCIHasExactFrozenQualityMatrixAndFourSteps|TestWorkflowLockMatchesFinalBlobAndFrozenActions|TestCITriggersOnEveryPushAndPullRequestWithoutUnsafePathExclusion|TestCIJobsPassCanonicalActualHEADToProfiles|TestDeletionCandidatesExactlyMatchPlanAndArePresentUnreferenced|TestRpreHasExactElevenImportedJobs)$'` 与 `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix00PreDelete$'` 均通过；`Rpre` 为其前驱的唯一单父，11个job的checkout OID、dependency digest和provider identity一致且全部pass，ledger无额外项或temp。
**覆盖：** F1–F33 / AC1–AC38。

#### T5.37–T5.53 逐项提交与 evidence 约束

> 本小节逐行构成对应删除任务的第4步及验证组成部分；每项均须在该任务原有定向验证通过后完成，不得把多个删除合入同一提交或复用其他 stage 的证据。

| 任务 | 提交 | 唯一父提交 | ExpectedEvidenceManifest job |
|---|---|---|---|
| T5.37 | D01 | Rpre | deletion、required |
| T5.38 | D02 | D01 | deletion、required |
| T5.39 | D03 | D02 | deletion、required |
| T5.40 | D04 | D03 | deletion、required |
| T5.41 | D05 | D04 | deletion、required、Darwin native/e2e、Linux native/e2e |
| T5.42 | D06 | D05 | deletion、required、Windows native/e2e |
| T5.43 | D07 | D06 | deletion、required |
| T5.44 | D08 | D07 | deletion、required |
| T5.45 | D09 | D08 | deletion、required |
| T5.46 | D10 | D09 | deletion、required |
| T5.47 | D11 | D10 | deletion、required、Darwin/Linux/Windows native/e2e |
| T5.48 | D12 | D11 | deletion、required、Darwin/Linux/Windows native/e2e |
| T5.49 | D13 | D12 | deletion、required、Darwin/Linux/Windows native/e2e |
| T5.50 | D14 | D13 | deletion、required、Darwin native/e2e、Linux native/e2e |
| T5.51 | D15 | D14 | deletion、required、Windows native/e2e |
| T5.52 | D16 | D15 | deletion、required |
| T5.53 | D17 | D16 | deletion、required |

每个 `Dn` 都必须是表中父提交的唯一单父，commit delta仅含该任务批准的单文件删除及候选状态单步前移；fresh checkout先证明 exact parent/delta、stage/OID、CommandManifest和workflow dependency digest，再运行该行全部job。只有同一`Dn`的全部job通过后，才可收集typed safe summary/provider evidence并导入ledger；导入必须在下一删除任务开始前完成，禁止跨阶段预取、提前导入、raw artifact、缺项、额外项、skip或不同revision拼接。验证必须证明表中17个stage合计64个job全部恰好出现一次。

### T5.37 精确删除旧 Provider SSE 文件

**文件：** 删除 `internal/provider/sse.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.36、T2.39–T2.46、T3.25、T5.15、T5.16
**步骤：**
1. 复核该文件仍处于 `present_no_live_references`，新同步 SSE decoder、两类生产 stream、五类退出及 Provider E2E 均已通过，候选集合外无旧入口引用、init 或注册副作用。
2. 只用精确单文件补丁删除该源码，并把清单中这一项改为 `deleted`；不得使用 glob/递归删除或触碰 fixture、历史规格和用户数据。
3. 断言清单状态仍是批准顺序的前缀，运行 Provider/Orchestrator 定向 race、五类退出和全仓普通测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix01ProviderSSE$'`、`go test -json -race -count=20 ./internal/provider ./internal/orchestrator -run 'SSE|EveryExit|Usage|Close'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestProviderUsesProductionAdapterAndTrustedRoots|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F7、F8、F17、F18、F31、F32 / AC7、AC8、AC17、AC18、AC31、AC32、AC38(4)。

### T5.38 精确删除旧 MCP HTTP 文件

**文件：** 删除 `internal/mcpclient/http.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.37、T2.55–T2.60、T2.67、T5.14、T5.16
**步骤：**
1. 证明所有 HTTP 构造均进入 `transport/http` 与 netpolicy，body/SSE/redirect/预算/双 context Close 门禁通过，候选集合外无旧 HTTP 入口引用。
2. 精确删除该文件并只前移对应清单状态；保留新 transport、协议 fixture、canary 和旧数据兼容样本。
3. 运行 HTTP Transport、MCP Manager、AC38(4) 与全仓普通测试，任何旧 fallback 或清单越序均失败。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix02MCPHTTP$'`、`go test -json -race -count=20 ./internal/mcpclient/... -run 'HTTP|Body|SSE|Redirect|Close'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestMCPUsesProductionAdapterAndTrustedRoots|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F7、F8、F14、F15、F31、F32 / AC7、AC8、AC14、AC15、AC31、AC32、AC38(4)。

### T5.39 精确删除旧 MCP HTTP closer 文件

**文件：** 删除 `internal/mcpclient/http_closer.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.38、T2.59、T2.60、T2.67、T5.14
**步骤：**
1. 证明远端 session delete 只由新 `RemoteSessionCloser` 在 Manager Close 中调用，重复 Close 不重发，旧 closer 类型与构造器零生产引用。
2. 精确删除该文件并将该项状态改为 `deleted`，不得删除新远端清理测试或兼容 fixture。
3. 运行远端清理次数、并发 Close、Manager 生命周期、AC38(4) 与全仓测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix03MCPHTTPCloser$'`、`go test -json -race -count=20 ./internal/mcpclient/... -run 'RemoteSession|ManagerClose|ConcurrentClose'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestMCPUsesProductionAdapterAndTrustedRoots|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F15、F16、F31、F32 / AC15、AC16、AC31、AC32、AC38(4)。

### T5.40 精确删除旧 MCP stdio 主文件

**文件：** 删除 `internal/mcpclient/stdio.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.39、T2.61–T2.68、T5.11、T5.14
**步骤：**
1. 证明全部 stdio 调用方已迁至 `transport/stdio`、受保护 Process、唯一 writer/Wait 与有界 framing/stderr，旧主入口零生产引用。
2. 精确删除该文件并只更新对应状态；不得删除新 transport、process helper 或 secret canary。
3. 运行 stdio Close/取消/burst/blocked write、进程树与全仓普通测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix04MCPStdio$'`、`go test -json -race -count=20 ./internal/mcpclient/... ./internal/proctree -run 'Stdio|Cancellation|Burst|Blocked|SingleProcess'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestMCPUsesProductionAdapterAndTrustedRoots|TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'`、`go run ./cmd/xagent-check --profile=cross-build` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F4、F7、F11、F14–F16、F31、F32 / AC4、AC7、AC11、AC14–AC16、AC31、AC32、AC38(2)、AC38(4)。

### T5.41 精确删除旧 MCP stdio Unix 文件

**文件：** 删除 `internal/mcpclient/stdio_unix.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.40、T1.19–T1.21a、T2.61–T2.68、T5.22、T5.23
**步骤：**
1. 证明 Darwin/Linux 只使用新 stdio Transport 与 proctree 保护实现，旧 Unix build-tag 文件和裸进程入口零引用。
2. 精确删除该文件并前移单项状态；保留 POSIX 原生测试、launcher 和 fail-closed 实现。
3. 运行 Darwin/Linux 交叉构建和同一 post-change revision 的原生相关测试；原生证据不能由当前非目标宿主冒充。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix05MCPStdioUnix$'` 与 `go run ./cmd/xagent-check --profile=cross-build` 通过；Darwin、Linux amd64 runner 在同一 post-change revision 各执行 `go run ./cmd/xagent-check --profile=native` 及 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^TestE2EAC38NativePlatformSecurity$'`，最后 `go run ./cmd/xagent-check --profile=unit` 通过。所有 required package/test 均满足且无 skip。
**覆盖：** F4、F11、F16、F31、F32 / AC4、AC11、AC16、AC31、AC32、AC38(5)。

### T5.42 精确删除旧 MCP stdio other 文件

**文件：** 删除 `internal/mcpclient/stdio_other.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.41、T1.22–T1.24、T2.61–T2.68、T5.24
**步骤：**
1. 证明 Windows 使用新 restricted-token/Job Object stdio 路径，unsupported 只显式拒绝；旧 `!unix` fallback 零引用且不能静默裸启动。
2. 精确删除该文件并更新单项状态，保留 Windows/unsupported 生产实现与原生测试。
3. 运行 Windows 交叉构建及原生 stdio/proctree/AC38(5)；能力不足只能证明 target 未启动。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix06MCPStdioOther$'` 与 `go run ./cmd/xagent-check --profile=cross-build` 通过；Windows amd64 runner 在同一 post-change revision 执行 `go run ./cmd/xagent-check --profile=native` 及 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^TestE2EAC38NativePlatformSecurity$'`，最后 `go run ./cmd/xagent-check --profile=unit` 通过。所有 required package/test 均满足且无 skip。
**覆盖：** F4、F11、F16、F31、F32 / AC4、AC11、AC16、AC31、AC32、AC38(5)。

### T5.43 精确删除旧 MCP JSON-RPC 文件

**文件：** 删除 `internal/mcpclient/jsonrpc.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.42、T2.47–T2.54、T2.68、T5.14、T5.16
**步骤：**
1. 证明 numeric/string ID、pending、唯一 receive loop 和 codec 全由新 `internal/mcpclient/protocol/**` 与 Connection 实现，旧 JSON-RPC 符号零引用。
2. 精确删除该文件并更新单项状态；禁止删除新 protocol 目录、adapter 测试或 canary。
3. 运行 ID round-trip、竞态、预算、关闭、AC38(4) 和全仓测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix07MCPJSONRPC$'`、`go test -json -race -count=20 ./internal/mcpclient/... -run 'JSONRPC|Numeric|string|Pending|Receive|Close'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestMCPUsesProductionAdapterAndTrustedRoots|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F7、F14–F16、F31、F32 / AC7、AC14–AC16、AC31、AC32、AC38(4)。

### T5.44 精确删除旧 MCP protocol 文件

**文件：** 删除 `internal/mcpclient/protocol.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.43、T2.47–T2.54、T2.68
**步骤：**
1. 证明所有 MCP DTO 和状态转换只使用新 protocol 包，旧导出类型、alias、注册与兼容 shim 均零生产引用。
2. 精确删除该文件并更新单项状态；旧 wire fixture 仍保留用于新 codec 的兼容验证。
3. 运行协议 round-trip、未知字段/超限、Manager adapter 和全仓普通测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix08MCPProtocol$'`、`go test -json -race -count=20 ./internal/mcpclient/... -run 'Protocol|Codec|RoundTrip|Adapter'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestMCPUsesProductionAdapterAndTrustedRoots|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F14–F16、F31、F32 / AC14–AC16、AC31、AC32、AC38(4)。

### T5.45 精确删除旧 MCP limits 文件

**文件：** 删除 `internal/mcpclient/limits.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.44、T2.47、T2.53、T2.57、T2.58、T2.62、T2.68
**步骤：**
1. 证明配置后的共享 Counter 覆盖 JSON、SSE、stdio framing、工具发现和响应，旧限制常量/计数器零引用且无宽松 fallback。
2. 精确删除该文件并更新单项状态；保留所有超限 fixture 与 C8 兼容测试。
3. 运行多维累计预算、未知长度流、framing 恢复边界与全仓测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix09MCPLimits$'`、`go test -json -race -count=20 ./internal/mcpclient/... -run 'Limit|Budget|Oversized|Chunk|Framing'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestMCPUsesProductionAdapterAndTrustedRoots|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F9、F14–F16、F32 / AC9、AC14–AC16、AC32、AC38(4)。

### T5.46 精确删除旧 MCP redact 文件

**文件：** 删除 `internal/mcpclient/redact.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.45、T1.1–T1.10、T2.68、T5.16
**步骤：**
1. 证明 MCP 所有边界使用唯一 RuntimeRedactor、SafeText/SafeError 与 BoundedSink，旧局部脱敏函数零引用且没有 raw fallback。
2. 精确删除该文件并更新单项状态；保留 secret canary 和精确 repoaudit fixture 豁免。
3. 运行 MCP 全链路 canary、错误/diagnostics 安全输出、AC38(4) 和全仓测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix10MCPRedact$'`、`go test -json -race -count=20 ./internal/mcpclient/... ./internal/redact -run 'Canary|Redact|SafeError|Diagnostic'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestMCPUsesProductionAdapterAndTrustedRoots|TestE2EAC38MCPProviderSecretsRedirectBudgetAndClose)$'` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip，输出无 canary。
**覆盖：** F7、F14–F16、F32 / AC7、AC14–AC16、AC32、AC38(4)。

### T5.47 精确删除旧 Permission sandbox 文件

**文件：** 删除 `internal/permission/sandbox.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.46、T1.11–T1.24、T2.11–T2.15、T5.9、T5.22–T5.24
**步骤：**
1. 证明规则加载/写入只使用 safefs capability，危险执行只使用 Ticket 与 ProtectionPlan；旧字符串 sandbox API 零引用，旧规则非破坏迁移通过。
2. 精确删除该文件并更新单项状态；不得删除权限规则 fixture、迁移逻辑或用户权限文件。
3. 运行 Permission/safefs/proctree、AC38(1)、三平台 protected-slot 原生门禁与全仓测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix11PermissionSandbox$'`、`go test -json -race -count=20 ./internal/permission ./internal/safefs ./internal/proctree -run 'Legacy|Protected|Ticket|Capability'` 与 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EAC38AuthorizationCollisionAndPermissionProtection|TestE2EAC38NativePlatformSecurity)$'` 通过；macOS/Linux/Windows amd64 runner 各执行 `go run ./cmd/xagent-check --profile=native`，随后 `go run ./cmd/xagent-check --profile=unit` 通过，required JSON 记录无 missing/skip。
**覆盖：** F3–F6、F31、F32 / AC3–AC6、AC31、AC32、AC35、AC38(1)。

### T5.48 精确删除旧 Tool path 文件

**文件：** 删除 `internal/tool/path.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.47、T2.21–T2.27、T5.9–T5.11、T5.22–T5.24
**步骤：**
1. 证明 Read/Grep/Glob/Write/Edit 均使用句柄相对 safefs、累计预算和 Ordinary capability，旧 clean/join/EvalSymlinks 路径入口零引用。
2. 精确删除该文件并更新单项状态；保留路径逃逸、symlink/reparse、预算和 legacy fixture。
3. 运行 Tool/safefs、AC38(1)/(2)、三平台路径原生门禁与全仓测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix12ToolPath$'`、`go test -json -race -count=20 ./internal/tool ./internal/safefs -run 'Path|Escape|Symlink|Reparse|Budget|Protected'` 与 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EAC38AuthorizationCollisionAndPermissionProtection|TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38NativePlatformSecurity)$'` 通过；macOS/Linux/Windows amd64 runner 各执行 `go run ./cmd/xagent-check --profile=native`，随后 `go run ./cmd/xagent-check --profile=unit` 通过，required JSON 记录无 missing/skip。
**覆盖：** F4、F9、F12、F31、F32 / AC4、AC9、AC12、AC31、AC32、AC38(1)、AC38(2)。

### T5.49 精确删除旧 Instruction sandbox 文件

**文件：** 删除 `internal/instructions/sandbox.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.48、T2.28–T2.31、T5.22–T5.24
**步骤：**
1. 证明 Instruction/Skill 发现与 include 只使用 safefs 稳定身份和累计预算，旧 Unix 直接路径/sandbox 入口零引用。
2. 精确删除该文件并更新单项状态；保留重复/菱形/深层 include 与链接竞态 fixture。
3. 运行 Instruction/Skill、三平台链接原生门禁和全仓测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix13InstructionSandbox$'`、`go test -json -race -count=20 ./internal/instructions ./internal/skill ./internal/safefs -run 'Include|Budget|Symlink|Reparse|Discovery'` 与 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^TestE2EAC38NativePlatformSecurity$'` 通过；macOS/Linux/Windows amd64 runner 各执行 `go run ./cmd/xagent-check --profile=native`，随后 `go run ./cmd/xagent-check --profile=unit` 通过，required JSON 记录无 missing/skip。
**覆盖：** F7、F13、F31、F32 / AC7、AC13、AC31、AC32、AC38(5)。

### T5.50 精确删除旧 Hook Unix 命令文件

**文件：** 删除 `internal/hook/command_unix.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.49、T2.32–T2.34、T5.11、T5.22、T5.23
**步骤：**
1. 证明 Darwin/Linux Hook 只使用 `command_shell_posix.go`、ProtectionPlan 与共享 proctree，旧裸 exec/只杀父进程入口零引用。
2. 精确删除该文件并更新单项状态；保留 POSIX shell 选择、生命周期和原生 fail-closed 测试。
3. 运行 Hook/proctree race、AC38(2) 与 Darwin/Linux 原生门禁。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix14HookCommandUnix$'`、`go test -json -race -count=20 ./internal/hook ./internal/proctree -run 'Command|Cancellation|Close|Protected'` 与 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38NativePlatformSecurity)$'` 通过；Darwin/Linux amd64 runner 各执行 `go run ./cmd/xagent-check --profile=native`，随后 `go run ./cmd/xagent-check --profile=unit` 通过，required JSON 记录无 missing/skip。
**覆盖：** F4、F7–F9、F11、F31、F32 / AC4、AC7–AC9、AC11、AC31、AC32、AC38(2)、AC38(5)。

### T5.51 精确删除旧 Hook other 命令文件

**文件：** 删除 `internal/hook/command_other.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.50、T2.32–T2.34、T5.11、T5.24
**步骤：**
1. 证明 Windows 使用 `command_shell_windows.go` 和受保护 Runner，unsupported 使用显式拒绝实现；旧 `!unix` no-op/裸执行入口零引用。
2. 精确删除该文件并更新单项状态；保留 Windows/unsupported 实现和测试。
3. 运行 Hook/proctree、AC38(2)/(5)、Windows 原生门禁和全仓普通测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix15HookCommandOther$'`、`go run ./cmd/xagent-check --profile=cross-build` 与 `go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^(TestE2EAC38LargeOutputArtifactAndProcessTreeCancellation|TestE2EAC38NativePlatformSecurity)$'` 通过；Windows amd64 runner 执行 `go run ./cmd/xagent-check --profile=native`，随后 `go run ./cmd/xagent-check --profile=unit` 通过，required JSON 记录无 missing/skip。
**覆盖：** F4、F7–F9、F11、F31、F32 / AC4、AC7–AC9、AC11、AC31、AC32、AC38(2)、AC38(5)。

### T5.52 精确删除旧 Orchestrator tool batches 文件

**文件：** 删除 `internal/orchestrator/tool_batches.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.51、T3.21–T3.27、T5.6、T5.16
**步骤：**
1. 证明所有工具调用只由资格判断、共享 semaphore、序号槽和取消状态机调度，旧 RiskSafe 批处理与无界 goroutine 入口零引用。
2. 精确删除该文件并更新单项状态；保留排序、取消、迟到事件、Provider/MCP 生命周期和 canary fixture。
3. 运行 Orchestrator/App 定向 race、全渠道 AC38 和全仓普通测试。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix16OrchestratorToolBatches$'`、`go test -json -race -count=20 ./internal/orchestrator ./internal/app -run 'Scheduler|Ordering|Cancellation|Stale|EveryExit'`、`go run ./cmd/xagent-check --profile=e2e` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；e2e profile 的 10 个 required 根和 unit 三步记录均无 missing/skip。
**覆盖：** F3–F7、F17–F24、F32 / AC3–AC7、AC17–AC24、AC32、AC38。

### T5.53 精确删除旧 Conversation file store 文件

**文件：** 删除 `internal/conversation/file_store.go`；修改 `internal/repoaudit/deletion_candidates_test.go`
**依赖：** T5.52、T3.15–T3.18、T5.12、T5.13
**步骤：**
1. 在删除前再次证明旧 JSON、v1 JSONL 和 C10 ExternalPath 成功/失败路径均非破坏迁移，原文件原位保留，新生产入口只使用 v2 Store，旧 FileStore 零引用。
2. 最后精确删除该文件并将第 17 项改为 `deleted`；不得删除 migration DTO、legacy fixture、旧会话、数据根或历史规格。
3. 运行 Conversation 崩溃恢复/迁移、AC38(3)、三平台构建和全仓普通测试，断言 17 项状态全部 deleted。

**验证：** `go test -json -count=1 ./internal/repoaudit -run '^TestDeletionCandidateState$/^Prefix17ConversationFileStore$'`、`go test -json -race -count=20 ./internal/conversation -run 'Legacy|Migration|ExternalPath|Recovery|Atomic|Restart|TestProcessInterruptionAtCommitBarriersPreservesLastRevision'`、`go test -json -race -tags=e2e -count=1 ./cmd/xagent -run '^TestE2EAC38LongConversationRestartExternalizeAndSwitch$'`、`go run ./cmd/xagent-check --profile=cross-build` 与 `go run ./cmd/xagent-check --profile=unit` 均通过；required JSON 记录无 missing/skip。
**覆盖：** F7、F10、F19–F22、F31、F32 / AC7、AC10、AC19–AC22、AC31、AC32、AC34、AC35、AC38(3)。

### T5.53a 形成 R0、导入 pre-E 证据并渲染 marker result

**文件：** `go.mod`, `go.sum`, evidence ledger；只读检查全部批准实现、测试、配置、CI 与文档
**依赖：** T5.53
**步骤：**
1. 在D17 fresh checkout中固定`GOPROXY=off`、`GOSUMDB=off`、`GOENV=off`、`GOWORK=off`、`GOTOOLCHAIN=local`、空`GOFLAGS`及已冻结只读cache/toolchain，运行唯一`go mod tidy -diff`：exit 0且stdout/stderr均空时`R0=D17`；只有exit 1、stderr空且stdout为可严格解析、仅触及`go.mod`/`go.sum`非空子集的单一diff时，才可逐byte应用捕获diff一次并形成唯一单父tidy child`R0`，且R0复跑必须无差异。禁止gofmt、源码重构、候选外删除或文档证据变化。
2. 立即运行 `record-r0-transition`，从fresh checkout复演并记录 D17→R0 为 alias或唯一 tidy child；typed transition必须精确绑定OID、tree、delta和module digest，raw diff不得落盘。
3. 在同一R0运行 ExpectedEvidenceManifest的core、final、commit-repoaudit、docs、Darwin/Linux/Windows native+e2e和performance共11个job；另复核17项在文件系统、index与commit tree均不存在，兼容数据、历史规格、M0本地保留文件、替代实现和35个preservation target完整。
4. 运行after_r0 `verify-final`，随后才逐项导入rpre、D01–D17、r0全部pre-E typed evidence；每次导入从fresh R0重建ancestry/delta/stage表，最终恰为86个pre-E job且无extra/temp/cross-stage。
5. 在完整pre-E ledger上运行`render-evidence-markers`，只接收stdout唯一canonical `EvidenceMarkerRenderResult`+LF+EOF；结果恰含README、docs index、checklist三项pass文档及R0可复现证据，保存于任务临时内存/文件供下一任务精确应用，不写ledger或仓库。

**验证：** `record-r0-transition`、`verify-final --stage=after_r0`和`render-evidence-markers`均exit 0；R0的11个job全部绑定同一OID，pre-E ledger恰含rpre 11＋D01–D17 64＋r0 11＝86个job，transition为alias或唯一tidy child，render result canonical且三个document之外无输出。
**覆盖：** F1–F33 / AC1–AC38。

### T5.53b 精确应用 marker result、形成 E 并导入 post-E 证据

**文件：** `README.md`, `docs/index.md`, `docs/security-reliability-delivery-quality-2026-07-30/checklist.md`, evidence ledger
**依赖：** T5.53a
**步骤：**
1. 从clean R0 checkout读取T5.53a的canonical render result；冻结updater逐byte替换三份文档各自唯一pending block，重编码结果必须等于DTO中的完整document，marker外任一byte、文件mode或路径变化即失败。
2. 形成唯一单父evidence commit `E`，`R0..E` delta恰好只有README、docs index和当前checklist三份marker结果；内容继续绑定R0，不写E自身OID，不修改spec/plan/task、源码、测试、配置、workflow或lock。
3. 在同一E运行ExpectedEvidenceManifest的core、final、commit-repoaudit、docs、Darwin/Linux/Windows native+e2e和performance共11个job；fresh checkout逐项证明E的唯一父、三文件delta、R0内嵌证据和workflow dependency digest。
4. 全部E job到齐后才导入post-E typed safe summary/provider evidence；运行after_e `verify-final`，证明pre-E ledger、E的11项、R0 transition、两个preservation final result均完整且无cross-stage、extra或temp。

**验证：** `git diff-tree --no-commit-id --name-only -r E`精确列出三份批准文档；E为R0唯一单父，11个E job均绑定同一OID且pass，post-E导入后ExpectedEvidenceManifest全部97个job恰好一次，`verify-final --stage=after_e` exit 0。
**覆盖：** F1–F33 / AC1–AC38。

### T5.54 执行删除后最终门禁并固化证据

**文件：** evidence ledger 与仓库外只读 archive；不修改仓库、Git index、`.gitmodules` 或用户数据
**依赖：** T5.53b
**步骤：**
1. 在E且完整after_e ledger上运行`generate`，从97个job、Rpre→D01–D17→R0→E链、workflow lock、performance、preservation和marker delta重建并以no-replace publisher发布唯一canonical AttestationRecord；随后运行`verify-record`独立重算全部关系与digest。
2. 若批准external readback，则仅通过stdin输入canonical AttestationRecord与ImmutableReceiptEvidence两帧运行`verify-readback`并发布verified pair；未选择时pair必须均不存在。不得联网、传path/URL/credential或保存provider raw response。
3. 运行`finalize-archive`完成Plan C17四阶段：全量复验并发布preparation，再次全量复验后才删除manifest，使用preparation固定三字段复验剩余archive并发布finalization，最后children-first/root-last收紧为跨平台只读；任一中断只能恢复到批准的四种状态。
4. 运行`verify-archive`只读复验全部bytes、digest、target count/closure、optional mode、权限和phase④幂等性；它不得创建/改写record、删除文件或清理active/lease/control root。报告archive精确位置、ManifestSHA256、TargetCount、TargetDigest、AttestationSHA256与FinalizationSHA256，但不删除ledger/archive。

**验证：** `generate`、`verify-record`、可选`verify-readback`、`finalize-archive`和`verify-archive`按所选分支全部exit 0；manifest已按phase②精确移除，preparation/finalization存在且互相绑定，archive闭集无temp/extra、权限只读，97个job与AC1–AC38均由同一attestation覆盖，仓库仍精确停留在E且ledger/archive未被删除。
**覆盖：** F1–F33 / AC1–AC38。

## 执行 DAG

### 记号与调度规则

- `A → B`：A 的验证实际通过后才能开始 B。
- `[A ∥ B] → C`：A、B 可在文件与集成面不冲突时并行；两者均通过后才能开始 C。
- `Ta–Tb`：按本文出现顺序包含两个端点及其间全部任务，包括 `T1.18a`、`T4.25h`、`T5.26a` 等带后缀任务。
- 各任务的“依赖”字段是硬边；本节只汇总主链、并行分支、汇合点和互斥约束，不削弱任何任务内依赖。
- `cmd/xagent/**`、`internal/events/events.go`、`internal/app/**` 与 `internal/orchestrator/**` 共用一个集成写锁。逻辑上可并行但触及这些路径的任务必须串行修改；测试执行仍可在不同 runner 并行。
- 任意两个任务修改同一文件时必须串行。原子切换和删除任务执行期间不得穿插其他生产接口、注册表或调用方修改。
- 后一里程碑不得在前一门禁通过前开始。checklist 获批准前，所有任务保持未开始状态。

### 里程碑主链

```text
T0.1 → … → T0.11       G0：仓库门禁
  → M1 并行安全基座
  → T1.34               G1：安全基础门禁
  → M2 并行运行时迁移
  → T2.70               G2：运行时安全门禁
  → M3 数据/编排双分支
  → T3.27               G3：数据与编排门禁
  → M4 App/CLI/Assembly 分支
  → T4.30               G4：用户路径与组装门禁
  → M5 fixture/E2E/performance/platform/check/docs 分支
  → T5.36               Gpre：删除前资格门禁
  → T5.37 → … → T5.53  17 个精确删除，严格串行
  → T5.53a → T5.53b
  → T5.54               Gpost：R0/E 证据归档门禁
```

### M0：仓库止血

```text
T0.1 → T0.2
          ├─→ T0.3 ─┬─→ T0.3a ─┐
          │          ├─→ T0.4 ──┤
          │          ├─→ T0.6 ──┤
          │          └─→ T0.7 ──┤
          └────────────→ T0.5 ─┘
                               → T0.8 → T0.9
T0.9 → T0.10 → T0.11
```

T0.10 是唯一 Git index 变更点；进入前必须完成目标核对，退出后必须证明本地文件仍存在且未创建 `.gitmodules`。

### M1：安全基础能力

可并行推进以下能力分支：

- 脱敏与诊断：`T1.1 → T1.2 → [T1.3 ∥ T1.7]`，`[T1.7 ∥ T1.5] → [T1.8 ∥ T1.9] → T1.10`。
- 预算：`T1.4 → T1.5 → T1.6`。
- safefs：`T1.5 → T1.11 → [T1.12 ∥ T1.13]`；两平台实现汇合到 T1.14、T1.17，原子写分别进入 T1.15、T1.16。
- proctree：`[T1.8 ∥ T1.11] → T1.18`，随后完成 T1.18a、T1.18b；POSIX、Darwin、Linux、Windows 分支最终汇合到 T1.24。
- Artifact：`[T1.5 ∥ T1.11] → T1.25 → T1.26 → T1.27 → T1.28`。
- netpolicy：`T1.2 → T1.29 → T1.30 → T1.31 → [T1.32 ∥ T1.33]`。

```text
JOIN(
  T1.3, T1.6, T1.10, T1.14, T1.17,
  T1.24, T1.28, T1.32, T1.33
) → T1.34
```

### M2：运行时安全边界

G1 后可启动配置、Instruction、HTTP Hook 和 MCP 协议基础分支：

- 配置：`T2.1 → T2.2 → T2.3 → T2.4 → T2.5 → T2.6 → T2.7`。
- 权限：T2.8–T2.14 完成身份、Ticket、健康状态、规则和确认模型，汇合到原子切换 T2.15。
- 工具：T2.16–T2.20 建立结果、Registry 和唯一 Executor；随后 Read/Grep/Glob、Write/Edit、Shell/Bash 分支汇合到 T2.27。
- Instruction/Skill：`T2.28 → T2.29 → T2.30 → T2.31 → T2.31a → T2.31b`。
- Hook：`[T2.32 ∥ T2.33] → T2.34 → T2.34a → T2.34b → T2.34c`。
- Provider：T2.35–T2.39 建立公共流；OpenAI T2.40–T2.42 与 Anthropic T2.43–T2.44 可并行，汇合到 `T2.45 → T2.46`。
- MCP core：T2.47–T2.54。
- MCP HTTP：T2.55–T2.60。
- MCP stdio：T2.61–T2.66。
- MCP 三个分支完成后进入 `T2.67 → T2.68`；Provider 与 MCP 再汇合到原子切换 T2.69。

```text
JOIN(
  T2.7, T2.27, T2.30, T2.31, T2.31a, T2.31b,
  T2.34, T2.34a, T2.34b, T2.34c, T2.51, T2.69
) → T2.70
```

### M3：Conversation 与 Orchestrator

G2 后可并行推进：

- Conversation：T3.1–T3.18；保存分支 T3.5–T3.8、加载恢复分支 T3.9–T3.14、旧格式迁移分支 T3.15–T3.17 汇合到原子切换 T3.18。
- C18 安全结果：`T3.18 → T3.19 → T3.19a`；随后安全上下文链从 T3.20 推进，Orchestrator 链从 T3.21 推进，两条分支修改共享集成面时仍串行持锁。
- Orchestrator：`T3.19a → T3.21 → T3.22 → T3.23 → T3.24`，该分支持有 `internal/orchestrator/**` 集成写锁。
- Provider 完整退出路径：T3.25 在 T3.20 后执行。
- `T3.26` 汇合 Store、调度、取消、Provider usage 与持久化顺序。

```text
JOIN(T3.13–T3.19, T3.19a, T3.20–T3.26) → T3.27
```

### M4：App、TUI、CLI 与唯一组装根

G3 后形成两条主分支：

- App/TUI：T4.1–T4.20，按各任务依赖内部并行，但修改 `internal/app/**` 时必须持有集成写锁。
- CLI：`T4.21 → [T4.22 ∥ T4.23] → T4.24`。

两条分支汇合后：

```text
T4.25 → T4.25a → T4.25b → T4.25c
      → T4.25d → T4.25e → T4.25f
      → T4.25g → T4.25h
```

其中 T4.25c 用唯一 RuntimeRedactor 创建 ResultFactory，并从唯一 Store 创建内置 producer 的 Capture 闭包；T4.25d 向 MCP 注入同一 ResultFactory/Capture 闭包、向纯 synthetic Hook adapter 只注入 ResultFactory；T4.25e 注入 ContextManager 并封闭唯一安全扇出；T4.29a 才删除 T3.19a 的迁移 adapter 和全部旧 Result/raw fallback。

T4.25h 只锁定 Assembly 输入、TrustedRoots 传递和唯一 Runtime registry，不发布第二生产组装根。随后：

```text
[T4.25h ∥ T4.26] → T4.27 → T4.28
[T4.26 ∥ T4.28] → T4.29
[T4.22 ∥ T4.23 ∥ T4.25h ∥ T4.27–T4.29] → T4.29a
[T4.6 ∥ T4.8–T4.29 ∥ T4.29a] → T4.30
```

### M5：跨平台、交付与规格治理

fixture 基座：

```text
T5.1 → [T5.2 ∥ T5.3 ∥ T5.4 ∥ T5.5] → T5.6 → T5.7
```

T5.7 后形成以下逻辑分支：

- 授权与权限：`T5.8 → T5.9`。
- Artifact 与进程树：`T5.10 → T5.11`。
- Conversation 与导航：`T5.12 → T5.13`。
- MCP：T5.14。
- Provider：T5.15。
- 全渠道 canary：`[T5.9–T5.15] → T5.16 → T5.16a → T5.16b → T5.16c → T5.16d`。
- 性能：`T5.17 → T5.17a → T5.18 → T5.19`，再与 T5.12、T5.13、T5.15 汇合到 T5.20。
- 平台：`T5.21 → [T5.22 ∥ T5.23 ∥ T5.24]`。三个原生证据 job 必须在不同 amd64 runner 上并行取得；源文件修改仍服从集成写锁。
- 检查器：`T5.25 → T5.25a → T5.25b → T5.26`；`[T5.24 ∥ T5.26] → T5.26a → T5.27`，随后测试 T5.28→T5.29、平台/E2E T5.30 汇合到 `T5.31 → T5.31a → T5.31b → T5.31c → T5.31d → T5.31e → T5.31f`。
- 配置示例：T5.32 可与 fixture、E2E 和检查器分支并行。
- 提交门禁与文档：`T5.31f → T5.33`；相关结果汇合到 `T5.34 → T5.35 → T5.35a`。

```text
JOIN(
  T5.22–T5.24,
  T5.31–T5.35a
) → T5.36
```

T5.8–T5.24 中涉及 `cmd/xagent/**` 的源码修改必须串行提交；其 hermetic fixture、独立包测试和三平台运行证据可以并行。

### 原子切换点

| 切换点 | 进入条件 | 退出后唯一状态 | 禁止的中间状态 |
|---|---|---|---|
| T2.15 | T2.8–T2.14 通过 | Authorizer 只签发一次性 Ticket | Grant/Ticket 双入口并存 |
| T2.20 | Ticket、状态、ResultFactory、Registry 完成 | 所有执行只经唯一 Executor | 直接 Tool.Run 或旧 Grant fallback |
| T2.69 | Provider T2.46、MCP T2.51/T2.68 均完成 | 全部生产调用方使用安全 DTO、ChatStream、Manager 与新 Transport | 新旧 Provider/MCP 入口或 owner 并存 |
| T3.18 | v2 保存、恢复、列表、兼容迁移完成 | 所有调用方使用新版 Store API | 旧 FileStore 与 v2 生产入口并存 |
| T3.21 | G2 通过 | schema→硬约束→Hook→确认→Ticket→Executor 的唯一顺序 | MCP、内置工具使用不同授权路径 |
| T4.29a | T3.19a 安全投影与 T4.25c–e 注入完成；Assembly、Runtime registry、回滚和反向关闭完成 | RunCLI 只发布 defaultAssembly 构造的完整 Runtime；唯一 Store/Capture/ResultFactory/ProjectToolResult 路径生效 | 第二组装根、旧 factory/raw fallback、公开 Result payload、迁移 adapter 或两份关闭表 |

原子切换由同一执行者连续完成；切换期间独占相关包及共享集成面，验证失败必须停留在切换前状态，不能提交可编译但安全边界不完整的中间版本。

### 删除前、删除中与删除后门禁

T5.36 是唯一删除前门禁。必须在同一 exact pre-delete revision 上同时证明：

1. 17 个候选均为 `present_no_live_references`，路径、顺序和替代实现与 Plan 完全一致。
2. 所有生产引用、init 和注册副作用为零。
3. 旧配置、权限、JSON、v1 JSONL 与 ExternalPath 非破坏兼容测试通过。
4. macOS、Linux、Windows 原生 amd64 的 required tests 和五组 E2E 全部通过，无 skip。
5. core、coverage、repoaudit、文档追溯和固定性能基线均通过。

只有 T5.36 通过后，才允许执行严格串行链：

```text
T5.37 → T5.38 → T5.39 → T5.40 → T5.41 → T5.42
      → T5.43 → T5.44 → T5.45 → T5.46 → T5.47
      → T5.48 → T5.49 → T5.50 → T5.51 → T5.52
      → T5.53 → T5.53a → T5.53b → T5.54
```

每个删除任务必须依次完成“当前项仍无生产引用→精确单文件删除→只前移一个状态→定向验证→唯一单父Dn提交→manifest指定job→typed evidence导入”。已删除项必须始终形成批准顺序的前缀；不得并行删除、合并删除、跨stage预取/导入或触碰候选外文件。

T5.53a、T5.53b、T5.54 构成唯一删除后门禁，必须按 R0/E/Archive 协议证明：

1. `R0` 中 17 项在文件系统、Git index 和 commit tree 中全部不存在，替代实现、legacy DTO/fixture/会话、历史规格和 M0 用户本地数据仍完整。
2. 兼容测试、unit×3、race、敏感场景×20、coverage≥80%、repoaudit 和五组 E2E 在同一 `R0` 上通过。
3. 三个平台均为绑定 `R0` 的原生 amd64 证据；performance 仍使用固定 baseline、每项 5 次预热与 30 次计分，按固定偶数样本中位数公式计算，并以精确 `CurrentMedianNanos*5 <= BaselineMedianNanos*6` 判定通过；ppm 仅供展示。
4. evidence commit `E` 只精确应用三份canonical marker结果并记录`R0`证据，不把自身SHA写回自身；随后`E`的11个job全部通过并导入，97个job恰好一次。
5. attestation生成、独立复验并完成四阶段只读archive；任何后续仓库写入都会使证据失效，计划文本或CI配置本身不能充当完成证据，ledger/archive不得在当前Spec中删除。

## F1–F33 追溯矩阵

> “主要实现”记录需求落点与原子切换任务；“专项验证”记录该需求的直接验收及回归门禁。所有需求先在 T5.36 取得同一Rpre删除资格证据，再经过D01–D17、T5.53a的R0、T5.53b的E，最终汇合到T5.54只读archive。表内“`T5.36 → T5.54`”是这条完整不可跳步链的缩写，不表示可越过中间任务。

| 需求 / 验收 | 主要实现 | 专项验证 | 最终汇合 |
|---|---|---|---|
| F1 / AC1 | T0.1、T0.3、T0.3a、T0.4、T0.6、T0.10 | T0.11、T5.27、T5.31–T5.31f、T5.53a–T5.54 | T5.36 → T5.54 |
| F2 / AC2 | T0.2–T0.9 | T0.11、T5.7、T5.27、T5.31、T5.33 | T5.36 → T5.54 |
| F3 / AC3 | T2.8、T2.8a、T2.9、T2.12、T2.15、T2.20、T2.25–T2.27、T3.21 | T2.70、T3.27、T5.8–T5.9 | T5.36 → T5.54 |
| F4 / AC4 | T1.11–T1.13、T1.15–T1.16、T1.18a、T1.20、T1.21、T1.21a、T1.23–T1.24、T2.13、T2.15、T2.20、T2.24、T2.26、T2.32、T2.34、T3.21 | T1.34、T2.70、T3.27、T4.30、T5.9、T5.11、T5.21–T5.24、T5.30 | T5.36 → T5.54 |
| F5 / AC5 | T1.8、T1.15–T1.16、T2.8–T2.11、T2.13、T2.15、T2.19–T2.20、T2.54、T3.21、T4.16 | T2.70、T3.27、T4.16、T4.30、T5.47、T5.52 | T5.36 → T5.54 |
| F6 / AC6 | T2.8、T2.8a、T2.9、T2.11–T2.15、T2.20、T3.21、T4.16 | T4.16、T4.30；T5.8 仅证明 F3/AC3 的授权碰撞，不作为 F6/AC6 证据 | T5.36 → T5.54 |
| F7 / AC7 | T0.5；T1.1–T1.3、T1.7–T1.10；T2.5、T2.17、T2.31–T2.35、T2.40、T2.43–T2.46、T2.54–T2.55、T2.63、T2.68–T2.69；T3.1、T3.16–T3.17、T3.19、T3.19a、T3.20、T3.25；T4.17、T4.25a、T4.25c–T4.25h、T4.26、T4.28–T4.29a | T1.10、T2.46、T2.68、T3.19、T3.19a、T3.27、T4.30、T5.7、T5.10、T5.14–T5.16、T5.26、T5.28 | T5.36 → T5.54 |
| F8 / AC8 | T1.9、T1.29–T1.33、T2.33、T2.45、T2.55、T2.68–T2.69、T4.25b、T4.25d、T4.25g–T4.25h | T1.34、T2.70、T4.30、T5.3、T5.14–T5.16、T5.30 | T5.36 → T5.54 |
| F9 / AC9 | T1.4–T1.6、T1.25–T1.26、T2.4、T2.18、T2.21–T2.24、T2.26–T2.27、T2.32–T2.34、T2.38、T2.58 | T1.34、T2.70、T5.5、T5.10、T5.14、T5.45、T5.48、T5.50–T5.51 | T5.36 → T5.54 |
| F10 / AC10 | T1.25–T1.28、T2.17–T2.18、T3.1、T3.16–T3.17、T3.19、T3.19a、T3.20、T4.17、T4.24、T4.25b–T4.25f、T4.29–T4.29a | T3.19、T3.19a、T3.27、T4.30、T5.4、T5.10、T5.12–T5.13、T5.16、T5.53 | T5.36 → T5.54 |
| F11 / AC11 | T1.18、T1.18a、T1.18b、T1.19–T1.23、T1.21a、T2.26–T2.27、T2.32、T2.34、T4.25b | T1.34、T2.70、T5.5、T5.11、T5.21–T5.24、T5.30 | T5.36 → T5.54 |
| F12 / AC12 | T1.4–T1.5、T1.11–T1.14、T2.21–T2.23、T2.27 | T1.34、T2.70、T5.48 | T5.36 → T5.54 |
| F13 / AC13 | T1.4–T1.5、T1.11–T1.14、T2.28–T2.31 | T1.34、T2.70、T5.49 | T5.36 → T5.54 |
| F14 / AC14 | T1.4–T1.5、T1.7–T1.9、T2.4、T2.6、T2.47–T2.48、T2.50、T2.53、T2.57–T2.58、T2.62、T2.68–T2.69 | T2.70、T5.3、T5.14、T5.16、T5.38、T5.40、T5.43–T5.46 | T5.36 → T5.54 |
| F15 / AC15 | T2.48–T2.53、T2.55–T2.56、T2.58–T2.60、T2.66–T2.69、T4.25d、T4.25g–T4.25h、T4.26–T4.29a | T2.70、T4.30、T5.3、T5.14、T5.16、T5.38–T5.46 | T5.36 → T5.54 |
| F16 / AC16 | T1.8、T1.18、T1.18b、T1.19、T1.22、T2.48–T2.51、T2.61–T2.69 | T2.70、T5.3、T5.14、T5.16、T5.21–T5.24、T5.30、T5.39–T5.46 | T5.36 → T5.54 |
| F17 / AC17 | T1.8–T1.9、T2.35–T2.39、T2.42–T2.46、T2.69、T3.25、T4.25d、T4.25e、T4.25g–T4.25h、T4.26–T4.30 | T2.46、T2.70、T3.27、T4.30、T5.2、T5.15–T5.16、T5.37、T5.52 | T5.36 → T5.54 |
| F18 / AC18 | T1.8、T2.40–T2.42、T2.46、T2.69、T3.19、T3.26、T4.25d–T4.25e | T2.70、T3.19、T3.27、T5.2、T5.15–T5.16、T5.37、T5.52 | T5.36 → T5.54 |
| F19 / AC19 | T1.8、T3.1、T3.3–T3.4、T3.6–T3.12、T3.15、T3.17–T3.18、T4.10、T4.25d | T3.6–T3.10、T3.27、T4.30、T5.4、T5.12、T5.53 | T5.36 → T5.54 |
| F20 / AC20 | T1.8、T3.1–T3.3、T3.5–T3.9、T3.15、T3.17–T3.19、T3.19a、T3.26、T4.7、T4.25d–T4.25e、T4.26、T4.29a | T3.6–T3.8、T3.19、T3.19a、T3.27、T4.30、T5.4、T5.12–T5.13、T5.53 | T5.36 → T5.54 |
| F21 / AC21 | T1.8、T3.10–T3.11、T3.18、T4.3、T4.7–T4.8、T4.10、T4.12、T4.15、T4.25f | T3.27、T4.30、T5.4、T5.13、T5.53 | T5.36 → T5.54 |
| F22 / AC22 | T2.1、T2.4、T3.11、T3.13–T3.14、T3.18、T4.10 | T3.27、T4.30、T5.4、T5.53；T5.32 只验证配置示例与兼容说明，不作为 F22 生产行为证据 | T5.36 → T5.54 |
| F23 / AC23 | T2.16、T2.19、T2.27、T2.54、T3.22–T3.23、T3.26、T4.25e | T2.70、T3.27、T5.52 | T5.36 → T5.54 |
| F24 / AC24 | T2.9、T2.16–T2.17、T2.19–T2.20、T2.27、T3.1、T3.21、T3.24、T3.26、T4.6、T4.11、T4.25e | T2.70、T3.27、T4.30、T5.52 | T5.36 → T5.54 |
| F25 / AC25 | T4.3–T4.9、T4.12、T4.18、T4.25f、T4.26 | T4.30、T5.13 | T5.36 → T5.54 |
| F26 / AC26 | T3.20、T4.1–T4.3、T4.6、T4.8、T4.11–T4.12、T4.20、T4.25f | T3.20、T4.30、T5.13 | T5.36 → T5.54 |
| F27 / AC27 | T4.3、T4.12–T4.16、T4.19、T4.25f | T4.30 | T5.36 → T5.54 |
| F28 / AC28 | T2.1–T2.4、T2.6–T2.7、T4.3、T4.12、T4.20、T4.24、T4.25、T4.25a、T4.25b、T4.25f、T4.25h、T4.27、T4.29a | T2.70、T4.30、T5.32 | T5.36 → T5.54 |
| F29 / AC29 | T4.3–T4.4、T4.12、T4.18、T4.25e–T4.25f | T4.30、T5.34–T5.35a、T5.53a–T5.53b | T5.36 → T5.54 |
| F30 / AC30 | T4.21–T4.23、T4.25h、T4.29a | T4.30、T5.34 | T5.36 → T5.54 |
| F31 / AC31 | T1.11–T1.13、T1.15–T1.18、T1.18a、T1.19–T1.24、T1.21a、T1.27、T2.25–T2.26、T2.30–T2.32、T2.61、T2.69、T4.23、T5.21–T5.24、T5.26a | T1.34、T2.70、T5.5、T5.9、T5.11、T5.21–T5.24、T5.26–T5.31f、T5.36–T5.54 | T5.36 → T5.54 |
| F32 / AC32 | T0.9、T2.51、T2.66、T2.70、T5.1–T5.7、T5.17–T5.31f、T5.33、T5.36–T5.54 | T5.6、T5.20、T5.25–T5.31f、T5.33、T5.36–T5.54 | T5.36 → T5.54 |
| F33 / AC33 | T0.2、T5.25–T5.25b、T5.31–T5.35a、T5.53a–T5.54 | T5.31–T5.31f、T5.34–T5.36、T5.53a–T5.54 | T5.36 → T5.54 |

## AC34–AC38 横向质量门槛

| 门槛 | 主要实现与约束 | 专项验证 | 最终汇合 |
|---|---|---|---|
| AC34 / N9 | T3.6 的 batch 完整追加、T3.7 的 snapshot 原子替换、T3.8 的失败与 test-only 子进程中断矩阵、T3.9–T3.10 的摘要链加载与非破坏恢复、T3.18 的 Store 原子切换，以及 T4.7、T4.26 的导航/runtime 保存边界。T3.7 与 T3.8 的覆盖声明均按 F19、F20 / AC19、AC20、AC34 对齐。 | T3.6–T3.10；`TestProcessInterruptionAtCommitBarriersPreservesLastRevision` 覆盖 write-before-fsync、fsync-before-publish、staging-before-replace、replace-before-state-publish；T5.4、T5.12–T5.13、T5.53 复核故障注入、重启与删除旧 Store 后的最后成功 revision。 | T5.36 → T5.54 |
| AC35 / N10、N16 | T2.7 保留旧配置入口，T2.11 非破坏读取权限规则，T3.13、T3.15–T3.18 处理旧 JSON、v1 JSONL 与 ExternalPath，T4.25d 将兼容 Store 纳入唯一 Assembly，T5.32 同步 schema 与迁移说明。迁移失败不得删除、覆盖或截断原文件。 | T2.70、T3.27、T5.4、T5.12、T5.32、T5.47、T5.53；分别验证旧配置、权限规则、JSON、v1 JSONL、ExternalPath 及删除旧实现后的兼容路径。 | T5.36 → T5.54 |
| AC36 / N12 | T5.17 固定共享协议与四个 workload，T5.18 固定治理前 revision `607b3dd3cb9d5f01bcba9a047b816187b4512b4a` 及 baseline adapter，T5.19 使用唯一 Assembly 实现 current adapter，T5.20 由 coordinator 以单调时钟独立计时，并把各 30 个有效样本排序后以 `s[14]+(s[15]-s[14])/2` 计算唯一中位数。 | T5.31 的 performance profile 依次要求协议/workload、baseline archive/adapter、current production-adapter/NDJSON 和最终四 workload 比较共 6 个根测试；T5.36、T5.54 分别在删除前后以 overflow-safe 精确 `CurrentMedianNanos*5 <= BaselineMedianNanos*6` 判门禁，`RatioPPM` 仅作展示。 | T5.36 → T5.54 |
| AC37 / N18–N21 | T5.1–T5.5 提供确定性 Clock/Gate/Trace 和 hermetic fixture，T5.6 禁止普通测试读取真实凭据、真实 Provider 或公网并移除脆弱短 sleep，T5.7 复用唯一 Assembly，T5.17–T5.20 固定可复现性能环境，T5.25–T5.26 建立统一 profile 与 required-test 判定，T5.26a 固定模块元数据，T5.27–T5.29 建立只读静态、重复稳定性和 80% 覆盖率硬门槛。 | T5.6 的普通测试连续 3 次及敏感场景 race `count=20`；T5.7 的 hermetic E2E；T5.20 的固定性能样本；T5.26a 的只读 module 验证；T5.27–T5.29 的只读静态、普通/race 和整体 coverage `>=80.0%` 门禁。 | T5.36 → T5.54 |
| AC38 / 端到端 | T5.7 提供只调用唯一 `defaultAssembly().Build` 的 hermetic harness；T5.8–T5.9 组成授权碰撞与权限保护根测试；T5.10–T5.11 组成大输出、私有 Artifact 与进程树取消根测试；T5.12–T5.13 组成长会话重启、恢复、外置与切换根测试；T5.14–T5.16 组成 MCP/Provider secret、重定向、预算、usage、关闭及全渠道 canary 根测试；T5.21–T5.24 提供三平台同名原生安全根测试。 | T5.30 通过 JSON 结果精确要求 3 个 harness 根、2 个 production-adapter/TrustedRoots 根和 5 个 `TestE2EAC38...` 根各恰好执行一次且无 skip，并另要求 native profile 的 7 个 package/test 二元组各执行 20 次；T5.22–T5.24 在 macOS、Linux、Windows 原生 amd64 runner分别执行完整清单。T5.37–T5.53 在每项删除后运行指定状态前缀、对应根测试和固定 profile。能力不足只接受测试证明目标未启动，不能以 fake、skip 或交叉构建代替。 | T5.36 → T5.54 |

## Task 文档静态自检

- 任务总数：281；编号唯一。分布为 M0=12、M1=37、M2=84、M3=38、M4=39、M5=71。
- 281 个任务均具有文件、依赖、步骤、验证和覆盖字段。
- 所有精确依赖与范围端点均存在；所有依赖均指向当前任务之前，无自依赖、无指向后续任务的边，因此 DAG 无环。
- G0、G1、G2、G3、G4、Gpre 和 Gpost 的祖先闭包（含门禁自身）分别为 12、49、133、171、210、261、281，精确覆盖对应累计里程碑全部任务。
- Plan 文件组织中所有批准修改路径均有 Task 归属；Task 不修改 Plan 明确要求保持不动的 `schema.go`、`run_request.go`、`command_controller.go` 或 completion 实现。
- T5.37–T5.53 与 Plan 的 17 个删除候选路径和顺序完全一致。
- F1–F33 每条均有主要实现、专项验证和Rpre→D01–D17→R0→E→Archive最终汇合；AC34–AC38均有独立横向质量门槛。
- ExpectedEvidenceManifest恰为97个job：rpre/r0/e各11个共33个，D01–D17共64个；逐stage无缺项、额外项、重复或顺序漂移。
- 本节数据必须由审批前静态脚本复算；若后续再增删任务或依赖，本文数字与结论必须同步更新后重新审批。
