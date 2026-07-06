# Permission System Task

## 任务总览

基于已通过的 `spec.md` 和 `plan.md`，本阶段将权限系统实现拆成可顺序推进、可独立验证的任务。原则是先建立权限核心，再接入执行链路，最后补齐 TUI 交互和端到端验收。

## 里程碑

### Milestone A：权限核心离线可测

覆盖 T1–T8，但不接入 Orchestrator 主执行链路。

验收：

- `go test ./internal/permission` 通过。
- 不影响现有工具执行链路。
- 黑名单、sandbox、规则优先级、权限模式和配置错误策略均可离线测试。

### Milestone B：执行链路接入

覆盖 T9–T10 和旧确认逻辑迁移。

验收：

- 权限拒绝能作为 tool result 回灌给模型。
- 工具不能绕过 Executor grant 执行。
- 旧 `NeedsConfirmation` 和新 permission ask 不会双重确认。

### Milestone C：交互体验补齐

覆盖 T11–T12。

验收：

- TUI 能展示等待确认、拒绝、取消、执行中、完成、失败。
- 本次、本会话、永久允许行为正确。
- 历史恢复能显示权限拒绝状态。

### Milestone D：端到端验收

覆盖 T13。

验收：

- 全量测试通过。
- tmux E2E 通过。
- `checklist.md` 逐项通过。

## T1. 建立 permission 包基础类型

目标：创建权限系统核心数据结构和最小包骨架。

修改范围：

- `internal/permission/decision.go`
- `internal/permission/mode.go`
- `internal/permission/rule.go`
- `internal/permission/result.go`

实现内容：

- 定义 `DecisionKind`: `allow`、`deny`、`ask`。
- 定义 `DenyReason`: `blacklist`、`sandbox`、`plan_mode`、`rule_deny`、`user_denied`、`user_cancelled`、`config_error`。
- 定义 `Decision`、`Grant`、`GrantScope`、`Source`、`Context`。
- 定义 `Rule`、`RuleFile`、`RuleSet`。
- 定义用户侧和模型侧拒绝文案结构。
- 实现 `permission_denied` tool result 构造函数。

验收：

- `go test ./internal/permission` 通过。
- `permission_denied` JSON 包含结构化 reason、recoverable 和 model-safe message。

## T2. 实现规则校验与匹配器

目标：支持 exact/glob 规则匹配，并明确 Bash 与文件路径的匹配语义。

修改范围：

- `internal/permission/rule.go`
- `internal/permission/matcher.go`
- `internal/permission/permission_test.go`

实现内容：

- 校验 rule schema：已知工具名、非空 pattern、合法 match_type、合法 effect。
- 校验 YAML schema：
  - `version` 缺失按 v1 处理。
  - `version > supported` 视为 `config_error`。
  - unknown fields 第一版视为 `config_error`，避免字段拼错后静默失效。
  - 空 `rules` 合法。
  - 重复 rule 允许加载，但 writer 写入时去重。
- exact 匹配完整规范化字符串。
- glob 匹配完整规范化字符串。
- 同层规则冲突处理：deny 优先；effect 相同时更具体优先；具体度相同后者优先。
- 实现规则展示格式：`Tool(pattern)`。
- 为文件工具实现默认路径参数映射：
  - `Read`: `path`
  - `Write`: `path`
  - `Edit`: `path`
  - `Glob`: `path/root`，缺省为项目根
  - `Grep`: `path/root`，缺省为项目根

验收：

- exact allow 不匹配追加参数或组合命令。
- glob 匹配完整字符串而非子串。
- deny 优先规则测试通过。
- 文件工具默认路径参数映射测试通过。

## T3. 实现 Bash 命令规范化、复杂 shell 检测和黑名单

目标：为 Bash 建立不可覆盖的硬拦截和保守的普通规则匹配前置检查。

修改范围：

- `internal/permission/blacklist.go`
- `internal/permission/matcher.go`
- `internal/permission/permission_test.go`

实现内容：

- Bash 命令规范化：trim、折叠空白、完整字符串匹配。
- 检测复杂 shell：`&&`、`||`、`;`、`|`、backtick、`$()`、重定向、here-doc、newline、`sh -c`、`bash -c`、`zsh -c`、`xargs`、`find -exec`、`eval`。
- 普通 glob allow 不放行复杂 shell。
- 实现硬黑名单类别：强制删除、格式化磁盘、权限破坏、关机重启、fork bomb、提权、写系统目录、破坏 git 历史。
- 黑名单命中直接 deny，不生成 ask。
- 内置 Bash allowlist 第一版保持极小，不默认放行测试/脚本命令。

验收：

- `Bash(git status)` exact allow 不放行 `git status && rm -rf .`。
- `Bash(git *)` 不放行管道、重定向、`sh -c`、`xargs`、`find -exec`。
- 黑名单命中不能被规则、permissive 或用户确认覆盖。

## T4. 接入路径沙箱和文件规则匹配

目标：统一文件工具路径安全判断，并让规则匹配规范化后的项目相对真实路径。

修改范围：

- `internal/permission/sandbox.go`
- `internal/tool/path.go`
- `internal/tool/grep.go`
- `internal/tool/glob.go`
- `internal/permission/permission_test.go`
- `internal/tool/tool_test.go`

实现内容：

- 复用或导出真实路径规范化能力。
- 对不存在深层路径查找最深已存在祖先，解析 symlink 后再拼接剩余路径。
- sandbox 决策返回展示用原始路径和规则用规范化相对真实路径。
- `Glob`、`Grep` 先校验搜索根，再对结果逐项真实路径校验。
- sandbox 失败直接 deny，不进入规则、模式或 ask。

验收：

- 项目外路径被拒绝。
- symlink 逃逸被拒绝。
- `../`、重复分隔符、symlink 别名不能绕过规则。
- `Glob` 不返回项目外 symlink 结果。
- `Grep` 不读取项目外 symlink 文件。

## T5. 实现规则加载、层级优先级和配置错误策略

目标：支持用户级、项目级、本地级和会话级规则，并保证加载确定性。

修改范围：

- `internal/permission/loader.go`
- `internal/permission/session.go`
- `internal/permission/authorizer.go`
- `internal/config/config.go`
- `.gitignore`
- `internal/permission/permission_test.go`

实现内容：

- 加载用户级：`$XDG_CONFIG_HOME/xagent/permissions.yaml` 或 `~/.config/xagent/permissions.yaml`。
- 加载项目级：`<project>/.xagent/permissions.yaml`。
- 加载本地级：`<project>/.xagent/permissions.local.yaml`。
- 规则加载不主动创建目录；目录或文件不存在时视为空规则。
- 只有永久允许写入本地级规则时才创建 `<project>/.xagent/`。
- 本地目录权限使用 `0700` 或平台安全默认权限，权限文件使用 `0600`。
- 会话级规则保存在内存。
- 优先级：session > local > project > user。
- 配置损坏返回 `config_error`。
- 用户级损坏：危险操作 fail closed，低风险只读可保守继续。
- 项目级损坏：写工具和 Bash fail closed。
- 本地级损坏：写工具、Bash、永久允许写入 fail closed。
- `.gitignore` 加入 `.xagent/permissions.local.yaml`。

验收：

- 四层优先级测试通过。
- 同层冲突确定性测试通过。
- 损坏 YAML 对危险操作 fail closed。
- 本地权限文件默认不进入 git。

## T6. 实现权限模式默认策略

目标：在无显式规则命中时，根据 strict/default/permissive 决定 allow、deny 或 ask。

修改范围：

- `internal/permission/mode.go`
- `internal/permission/authorizer.go`
- `internal/config/config.go`
- `internal/permission/permission_test.go`

实现内容：

- 定义 `strict`、`default`、`permissive`。
- 配置字段使用 `permission.mode`。
- 模式来源优先级：runtime/CLI > project config > user config > default。
- 本轮如暂不实现 runtime 切换，必须明确只读取配置和默认值。
- 非法 mode 值产生 `config_error`；危险操作 fail closed，TUI 展示配置错误摘要。
- 当前模式必须传入确认 prompt，用于 TUI 展示。
- 权限模式只处理无规则命中的默认策略。
- 模式不能覆盖硬约束或显式 deny。
- Plan Mode 不属于权限模式，作为硬约束单独处理。
- default 下文件只读工具在沙箱内 allow，写工具和 Bash ask。
- permissive 可减少 ask，但不绕过复杂 shell、黑名单、sandbox、Plan Mode 和权限配置保护。

验收：

- 三档模式对同一未命中危险调用产生不同决策。
- permissive 不覆盖显式 deny。
- Plan Mode 下 Bash、Write、Edit 一律 deny。

## T7. 实现永久规则写入器

目标：支持 TUI 的“永久允许”写入本地级规则文件。

修改范围：

- `internal/permission/writer.go`
- `internal/permission/session.go`
- `internal/permission/permission_test.go`

实现内容：

- 根据当前 call 生成最小规则。
- Bash 默认 exact，不自动泛化为 glob。
- 文件工具默认 exact，使用规范化相对真实路径。
- 写入前 validate 全量规则。
- 去重。
- 使用临时文件 + rename 原子写入。
- 文件权限使用 `0600`。
- 永久写入失败时不自动执行当前 tool call，返回 `config_error`。

验收：

- 成功写入本地级规则。
- 重复规则不重复写入。
- 写入失败时当前工具不执行。
- 生成规则预览与最终写入一致。

## T8. 实现 Authorizer 统一决策链

目标：把硬约束、规则、模式和 ask 组合成唯一执行前决策入口。

修改范围：

- `internal/permission/authorizer.go`
- `internal/permission/result.go`
- `internal/permission/permission_test.go`

子任务：

- T8a：实现硬约束决策链：Plan Mode、Bash 黑名单、路径沙箱、权限配置文件保护。
- T8b：实现规则与模式决策链：session、local、project、user、mode、ask。
- T8c：实现用户决策解析：deny、cancel、once、session、permanent。

实现内容：

决策顺序：

1. Plan Mode 硬约束。
2. Bash 黑名单。
3. 路径沙箱。
4. 权限配置文件保护。
5. 会话级规则。
6. 本地级规则。
7. 项目级规则。
8. 用户级规则。
9. 权限模式默认策略。
10. ask。

实现 `ResolveUserDecision`：

- deny -> `user_denied`。
- cancel -> `user_cancelled`。
- allow once -> once grant。
- allow session -> 添加 session 规则并生成 grant。
- allow permanent -> 写入本地规则并生成 grant；写入失败返回 `config_error`。

验收：

- 所有优先级测试通过。
- ask prompt 包含规则预览和风险摘要。
- model-safe denial 不暴露内部细节。

## T9. 改造 Executor 授权执行入口

目标：确保工具执行前必须持有有效 grant，防止绕过 Orchestrator。

修改范围：

- `internal/tool/executor.go`
- `internal/tool/tool_test.go`

实现内容：

- 新增 `ExecuteAuthorized(ctx, call, grant)`。
- grant 校验 call ID、工具名、fingerprint。
- 无 grant 或 grant 不匹配时返回 `permission_denied` 风格结果，不执行底层工具。
- 保留底层执行方法但不再供 Orchestrator 直接调用。
- `NeedsConfirmation` 不再作为最终权限来源。

验收：

- 无 grant 不执行。
- grant 不能复用于不同参数调用。
- fake tool 记录拒绝路径执行次数为 0。

## T10. 迁移旧确认逻辑

目标：移除旧 `RiskSafe` / `RiskDangerous` 确认路径作为最终权限来源，避免新旧权限系统双轨运行。

修改范围：

- `internal/orchestrator/tool_batches.go`
- `internal/tool/executor.go`
- `internal/events/events.go`
- `internal/app/update.go`
- 相关测试

实现内容：

- 找出所有调用 `NeedsConfirmation` 的地方。
- Orchestrator 不再直接依据 `RiskSafe` / `RiskDangerous` 决定是否确认。
- Plan Mode 只读限制从旧 filter 迁移到 permission hard constraint。
- 旧危险工具确认 UI 不再和新 permission ask 并行存在。
- 如保留兼容层，只能作为 permission prompt 的展示数据来源，不能作为授权来源。

验收：

- 同一个工具调用不会出现两次确认。
- `NeedsConfirmation` 不再影响最终 allow/deny。
- Plan Mode deny 由 Authorizer 返回。
- 旧确认路径相关测试迁移到 permission ask。

## T11. 接入 Orchestrator 和 Agent Loop

目标：把权限决策接入工具批次执行，并确保拒绝可恢复回灌。

修改范围：

- `internal/orchestrator/tool_batches.go`
- `internal/orchestrator/chat.go`
- `internal/conversation/message.go`
- `internal/conversation/conversation.go`
- `internal/orchestrator/*_test.go`
- `internal/conversation/*_test.go`

子任务：

- T11a：单工具调用接入权限决策和授权执行。
- T11b：`permission_denied` 结构化回灌与 conversation 保存。
- T11c：多工具批次、重复拒绝 fingerprint 和防循环。

实现内容：

- Orchestrator 调用 Authorizer，而不是直接调用 `NeedsConfirmation`。
- `allow` 调用 `ExecuteAuthorized`。
- `deny` 生成结构化 `permission_denied` tool result。
- `ask` 发起 TUI 确认并处理用户决策。
- 拒绝结果写入 conversation，Agent Loop 继续下一轮。
- 同一 fingerprint 被拒绝后，本轮 Agent Loop 内再次请求直接拒绝，避免确认循环。
- 多工具批次第一版采用保守顺序策略：先对所有 call 完成决策和必要确认，再按 provider 原始顺序执行允许项并回灌所有结果。
- 批次中存在 ask 时，不提前执行其他 allow 工具，避免用户拒绝后已有副作用发生。
- 每个 call 都必须有对应结果，结果按 call ID 回灌。

验收：

- 用户拒绝后模型能继续调整方案。
- Plan Mode 硬拒绝不进入 ask。
- 多工具部分拒绝不丢结果。
- 批次中 ask 未完成前，不执行其他 allow 工具。
- 最终回复阶段再次请求工具不递归失控。

## T12. 扩展事件、TUI 和 app 确认交互

目标：把原有 y/n 确认扩展为权限确认动作，并正确展示工具状态。

修改范围：

- `internal/events/events.go`
- `internal/app/update.go`
- `internal/tui/messages.go`
- `internal/tui/status.go`
- `internal/app/*_test.go`
- `internal/tui/*_test.go`

子任务：

- T12a：扩展事件结构，区分 permission ask 和最终用户决策。
- T12b：改造 app 快捷键和确认状态机。
- T12c：改造 TUI 工具行展示、状态文案和历史恢复。

实现内容：

- 扩展确认请求，包含工具名、风险、参数摘要、路径、触发原因、权限模式、规则预览。
- 扩展确认决策：deny、allow once、allow session、allow permanent、cancel。
- 快捷键：
  - `Enter` / `n` / `Esc`: 默认安全拒绝或取消。
  - `y`: 本次允许。
  - `s`: 本会话允许。
  - `p`: 永久允许。
  - `?`: 展开风险和规则预览。
- 永久允许需要展示最小规则预览并明确确认。
- 高风险命令禁用永久允许。
- 工具行状态区分等待确认、已拒绝、已取消、执行中、完成、失败。

验收：

- Enter 默认拒绝。
- y/s/p 分别产生正确动作。
- p 显示规则预览并确认后才写入。
- 拒绝和取消不显示为执行失败。
- Bash 命令含 `API_KEY=...`、`TOKEN=...` 等敏感片段时，确认 prompt 脱敏。
- `permission_denied` 的 model message 不包含完整敏感命令、完整命令输出、配置文件绝对路径或黑名单 regex。

## T13. 补齐 conversation 结构化结果保存

目标：确保权限拒绝和普通工具结果都能结构化保存。

修改范围：

- `internal/conversation/message.go`
- `internal/conversation/conversation.go`
- `internal/conversation/*_test.go`

实现内容：

- 保存 tool result 的结构化 data/error 字段。
- 如现有结构不足，新增 `ToolResultStatus`、`ToolResultError`、`ToolResultData`、`ToolResultTruncated` 等字段。
- `permission_denied` 保存 reason 和 recoverable。
- 历史恢复时 TUI 能显示拒绝状态。
- 旧历史文件缺少结构化字段时应能向后读取，不做迁移写回。
- TUI 历史恢复要区分工具执行失败、权限拒绝、用户取消和工具结果截断。

验收：

- 会话文件中可看到结构化权限拒绝。
- 历史恢复显示已拒绝/已取消。
- 旧会话文件仍能读取。
- 不丢失普通工具 stderr、exit_code、timed_out、truncated 等字段。

## T14. 权限配置文件保护专项测试

目标：把权限配置文件保护作为硬约束单独验收。

修改范围：

- `internal/permission/authorizer.go`
- `internal/permission/writer.go`
- `internal/permission/permission_test.go`
- `internal/orchestrator/*_test.go`

实现内容：

- 普通 `Edit(.xagent/permissions.local.yaml)` 被拒绝。
- 普通 `Write(.xagent/permissions.yaml)` 被拒绝。
- 普通工具不能写用户级权限规则文件。
- 永久允许只能通过 `permission.Writer` 写本地级规则文件。
- 永久允许不能写项目级或用户级规则文件。
- `permission.Writer` 不写入、不读取、不记录 `config.yaml` 内容。

验收：

- 权限配置文件保护不能被 allow 规则、permissive 或用户确认绕过。
- 本地 `config.yaml` 不出现在权限规则、tool result、TUI prompt 或测试快照中。

## T15. 错误路径与降级行为测试

目标：覆盖权限系统异常路径，保证危险操作 fail closed。

修改范围：

- `internal/permission/*_test.go`
- `internal/orchestrator/*_test.go`
- `internal/app/*_test.go`

实现内容：

- TUI confirmation channel 关闭时，当前 ask 视为取消，返回 `user_cancelled`。
- 本轮不实现确认 timeout；若未来加入 timeout，timeout 等价于取消。
- writer 写入失败返回 `config_error`，当前工具不自动执行。
- loader 报错但当前工具是低风险只读时按保守策略继续。
- Authorizer 内部决策错误对危险操作 fail closed；不吞掉应暴露给开发者的 panic，测试只覆盖可预期错误。

验收：

- 所有错误路径都有结构化结果。
- 危险操作在错误路径下底层执行次数为 0。
- 给模型的信息不暴露内部细节。

## T16. 全量测试与端到端验证

目标：完成所有自动化测试和 tmux 端到端验收。

验证命令：

```text
go test ./internal/permission
go test ./internal/tool
go test ./internal/orchestrator ./internal/conversation
go test ./internal/tui ./internal/app
go test ./...
go build ./cmd/xagent
```

端到端场景：

1. `Read(go.mod)` 在沙箱内自动执行。
2. 项目内写文件触发确认，本次允许后执行。
3. 用户拒绝 Bash 或 Edit 后，模型继续给出安全替代方案。
4. 明显危险 Bash 被硬拒绝，不出现允许选项。
5. Plan Mode 下写工具或 Bash 被硬拒绝。
6. 本会话允许对后续匹配调用生效。
7. 永久允许写入本地规则后对后续调用生效。
8. 损坏本地规则文件时危险操作 fail closed。

E2E 临时文件策略：

- 写入测试文件使用 `.xagent/tmp-permission-e2e.txt` 或 `docs/permission-system-e2e.tmp`。
- 测试后清理临时文件，或确保临时文件在 `.gitignore` 中。
- 永久允许会生成 `.xagent/permissions.local.yaml`，测试后按场景说明保留或清理。
- E2E 不读取、不打印、不修改 `config.yaml`。

验收：

- 自动化测试全部通过。
- tmux E2E 场景通过。
- 对照 `checklist.md` 完成逐项验收。

## 推荐执行顺序

1. T1 基础类型。
2. T2 规则匹配。
3. T3 Bash 策略。
4. T4 路径沙箱。
5. T5 规则加载。
6. T6 权限模式。
7. T7 永久规则写入。
8. T8 Authorizer。
9. T9 Executor guard。
10. T10 迁移旧确认逻辑。
11. T11 Orchestrator/Agent Loop。
12. T12 TUI/app。
13. T13 conversation 保存。
14. T14 权限配置文件保护专项测试。
15. T15 错误路径与降级行为测试。
16. T16 全量验证。
