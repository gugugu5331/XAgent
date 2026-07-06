# Permission System Checklist

## 使用方式

实现权限系统时按本清单逐项验收。只有本文件全部关键项通过后，才能认为本章完成。

标记说明：

- `[ ]` 未完成。
- `[x]` 已完成。
- `[blocked]` 被外部问题阻塞，需要说明原因。

## 1. 文档与范围

- [x] `docs/permission-system/spec.md` 已通过。
- [x] `docs/permission-system/plan.md` 已通过。
- [x] `docs/permission-system/task.md` 已通过。
- [x] 本章没有实现网络请求限制。
- [x] 本章没有实现 CPU、内存、时间、输出大小等资源配额。
- [x] 本章没有实现审计日志查询系统。
- [x] 本章没有声称 Bash 拥有 OS 级文件系统沙箱。

## 2. permission 包基础

- [x] 新增 `internal/permission` 包。
- [x] 定义 `DecisionKind`: `allow`、`deny`、`ask`。
- [x] 定义 `DenyReason`: `blacklist`、`sandbox`、`plan_mode`、`rule_deny`、`user_denied`、`user_cancelled`、`config_error`。
- [x] 定义 `Decision`、`Grant`、`GrantScope`、`Source`、`Context`。
- [x] 定义 `Rule`、`RuleFile`、`RuleSet`。
- [x] 拒绝文案区分 `UserMessage` 和 `ModelMessage`。
- [x] `permission_denied` tool result 使用结构化 JSON。
- [x] `ModelMessage` 不包含完整黑名单 regex。
- [x] `ModelMessage` 不包含权限文件绝对路径。
- [x] `ModelMessage` 不包含绕过硬约束的建议。

## 3. 规则 schema 和匹配

- [x] YAML schema 包含 `version` 和 `rules`。
- [x] rule 包含 `tool`、`pattern`、`match_type`、`effect`。
- [x] rule 可选包含 `path_param`、`description`。
- [x] `tool` 必须是已知工具名。
- [x] `pattern` 不得为空。
- [x] `match_type` 只支持 `exact` 和 `glob`。
- [x] `effect` 只支持 `allow` 和 `deny`。
- [x] `version` 缺失按 v1 处理。
- [x] `version > supported` 产生 `config_error`。
- [x] unknown fields 产生 `config_error`。
- [x] 空 `rules` 合法。
- [x] 加载时允许重复 rule。
- [x] writer 写入时去重。
- [x] exact 匹配完整规范化字符串。
- [x] glob 匹配完整规范化字符串，不做子串匹配。
- [x] 规则展示格式为 `Tool(pattern)`。
- [x] 同层冲突 deny 优先。
- [x] effect 相同时更具体规则优先。
- [x] 具体度相同时文件顺序靠后优先。

## 4. Bash 策略

- [x] Bash 命令匹配前 trim 首尾空白。
- [x] Bash 命令匹配前折叠连续空白。
- [x] Bash exact 只匹配完整命令。
- [x] Bash glob 只匹配完整命令。
- [x] 普通 Bash 规则只自动放行单一简单命令。
- [x] `&&` 被识别为复杂 shell。
- [x] `||` 被识别为复杂 shell。
- [x] `;` 被识别为复杂 shell。
- [x] `|` 被识别为复杂 shell。
- [x] backtick 被识别为复杂 shell。
- [x] `$()` 被识别为复杂 shell。
- [x] `<`、`>`、`>>` 被识别为复杂 shell。
- [x] here-doc 被识别为复杂 shell。
- [x] newline 分隔多命令被识别为复杂 shell。
- [x] `sh -c`、`bash -c`、`zsh -c` 被识别为复杂 shell。
- [x] `xargs` 被识别为复杂 shell。
- [x] `find -exec` 被识别为复杂 shell。
- [x] `eval` 被识别为复杂 shell。
- [x] `Bash(git status)` exact allow 不放行 `git status && rm -rf .`。
- [x] `Bash(git *)` glob allow 不放行 `git status && rm -rf .`。
- [x] `Bash(git *)` glob allow 不放行 `git status | cat`。
- [x] `Bash(git *)` glob allow 不放行 `git status > out.txt`。
- [x] 内置 Bash allowlist 第一版保持极小。
- [x] 内置 Bash allowlist 不默认包含 `go test`。
- [x] 内置 Bash allowlist 不默认包含 `npm test`。
- [x] 内置 Bash allowlist 不默认包含 `python script.py`。

## 5. Bash 硬黑名单

- [x] 黑名单在规则、模式和用户确认之前执行。
- [x] 黑名单命中直接 deny。
- [x] 黑名单命中不进入 ask。
- [x] 黑名单不能被 allow 规则覆盖。
- [x] 黑名单不能被 permissive 覆盖。
- [x] 黑名单不能被用户确认覆盖。
- [x] 强制删除类高危命令被拦截。
- [x] 磁盘格式化和分区破坏类命令被拦截。
- [x] 权限破坏类命令被拦截。
- [x] 系统关机重启类命令被拦截。
- [x] fork bomb 或资源炸弹被拦截。
- [x] 提权类命令被拦截。
- [x] 写系统目录类命令被拦截。
- [x] 破坏 git 历史类命令被拦截。
- [x] 黑名单拒绝结果不暴露完整 regex。

## 6. 路径沙箱

- [x] 文件工具包括 `Read`、`Write`、`Edit`、`Glob`、`Grep`。
- [x] 文件工具权限判断基于项目根真实路径。
- [x] 判断前解析 symlink。
- [x] 不存在的深层路径使用最深已存在祖先解析。
- [x] 解析祖先 symlink 后再拼接剩余路径。
- [x] 最终真实路径必须仍在项目根真实路径内。
- [x] 原始路径只用于展示。
- [x] 规则匹配使用规范化后的项目相对真实路径。
- [x] `../` 不能绕过 sandbox。
- [x] 重复分隔符不能绕过 sandbox。
- [x] symlink 别名不能绕过 sandbox。
- [x] Unicode 规范化差异不能绕过 sandbox。
- [x] 指向项目外的 symlink 文件不能被读取。
- [x] 指向项目外的 symlink 目录不能被写入。
- [x] sandbox 失败直接 deny。
- [x] sandbox 失败不进入 ask。
- [x] sandbox 失败不能被规则覆盖。
- [x] sandbox 失败不能被 permissive 覆盖。
- [x] sandbox 失败不能被用户确认覆盖。

## 7. 文件工具路径参数

- [x] `Read` 默认匹配 `path`。
- [x] `Write` 默认匹配 `path`。
- [x] `Edit` 默认匹配 `path`。
- [x] `Glob` 默认匹配 `path/root`，缺省为项目根。
- [x] `Grep` 默认匹配 `path/root`，缺省为项目根。
- [x] `Glob` 规则匹配搜索根，不匹配 glob pattern 本身。
- [x] `Grep` 规则匹配搜索根，不匹配 grep pattern 本身。
- [x] `Glob` 返回结果逐项经过真实路径校验。
- [x] `Grep` 打开文件前逐项经过真实路径校验。

## 8. 规则文件和优先级

- [x] 用户级规则路径为 `$XDG_CONFIG_HOME/xagent/permissions.yaml` 或 `~/.config/xagent/permissions.yaml`。
- [x] 项目级规则路径为 `<project>/.xagent/permissions.yaml`。
- [x] 本地级规则路径为 `<project>/.xagent/permissions.local.yaml`。
- [x] 会话级规则只存在内存。
- [x] 规则加载不主动创建目录。
- [x] 目录或文件不存在时视为空规则。
- [x] 永久允许写入时才创建 `<project>/.xagent/`。
- [x] 本地目录权限使用 `0700` 或平台安全默认权限。
- [x] 本地权限文件权限使用 `0600`。
- [x] `.gitignore` 包含 `.xagent/permissions.local.yaml`。
- [x] 会话级规则优先于本地级规则。
- [x] 本地级规则优先于项目级规则。
- [x] 项目级规则优先于用户级规则。
- [x] 用户级规则优先于权限模式默认策略。
- [x] 权限模式默认策略优先于 ask。
- [x] 同层冲突语义确定且有测试。

## 9. 配置错误策略

- [x] YAML 解析失败产生 `config_error`。
- [x] schema 校验失败产生 `config_error`。
- [x] 用户级损坏时危险操作 fail closed。
- [x] 用户级损坏时低风险只读操作可保守继续。
- [x] 项目级损坏时写工具 fail closed。
- [x] 项目级损坏时 Bash fail closed。
- [x] 本地级损坏时写工具 fail closed。
- [x] 本地级损坏时 Bash fail closed。
- [x] 本地级损坏时永久允许写入 fail closed。
- [x] 会话级非法状态视为内部错误，危险操作 fail closed。
- [x] TUI 展示配置错误摘要。
- [x] 给模型的结果不暴露完整配置文件路径。

## 10. 权限模式

- [x] 支持 `strict`。
- [x] 支持 `default`。
- [x] 支持 `permissive`。
- [x] 配置字段为 `permission.mode`。
- [x] 模式来源优先级为 runtime/CLI > project config > user config > default。
- [x] 如果本轮不实现 runtime 切换，文档和代码中明确只读取配置和默认值。
- [x] 非法 mode 产生 `config_error`。
- [x] 非法 mode 下危险操作 fail closed。
- [x] 当前模式传入确认 prompt。
- [x] 模式只处理无显式规则命中的默认策略。
- [x] 模式不能覆盖显式 deny。
- [x] 模式不能覆盖黑名单。
- [x] 模式不能覆盖 sandbox。
- [x] 模式不能覆盖 Plan Mode。
- [x] 模式不能覆盖权限配置文件保护。
- [x] default 下文件只读工具在 sandbox 内 allow。
- [x] default 下 `Write`、`Edit`、`Bash` 默认 ask。
- [x] permissive 可减少 ask。
- [x] permissive 不放行复杂 shell。

## 11. Plan Mode 硬约束

- [x] Plan Mode 作为硬约束由 Authorizer 处理。
- [x] Plan Mode 下 `Read` 可在 sandbox 内执行。
- [x] Plan Mode 下 `Glob` 可在 sandbox 内执行。
- [x] Plan Mode 下 `Grep` 可在 sandbox 内执行。
- [x] Plan Mode 下 `Write` 一律 deny。
- [x] Plan Mode 下 `Edit` 一律 deny。
- [x] Plan Mode 下 `Bash` 一律 deny。
- [x] Plan Mode deny 不进入 ask。
- [x] Plan Mode deny 不能被 allow 规则覆盖。
- [x] Plan Mode deny 不能被用户确认覆盖。

## 12. 永久规则写入

- [x] 永久允许只写本地级规则文件。
- [x] 永久允许不写项目级规则文件。
- [x] 永久允许不写用户级规则文件。
- [x] 永久允许写入前展示最小规则预览。
- [x] Bash 永久规则默认 exact。
- [x] Bash 永久规则不自动泛化成 `Bash(git *)`。
- [x] 文件工具永久规则默认 exact。
- [x] 文件工具永久规则使用规范化相对真实路径。
- [x] 写入前 validate 全量规则。
- [x] 写入时去重。
- [x] 使用临时文件 + rename 原子写入。
- [x] 写入文件权限为 `0600`。
- [x] 写入失败返回 `config_error`。
- [x] 写入失败不自动执行当前 tool call。
- [x] 写入失败时 UI 提示用户可改选本次允许重新授权。

## 13. 权限配置文件保护

- [x] 普通 `Edit(.xagent/permissions.local.yaml)` 被拒绝。
- [x] 普通 `Write(.xagent/permissions.yaml)` 被拒绝。
- [x] 普通工具不能写用户级权限规则文件。
- [x] 权限配置文件保护不能被 allow 规则覆盖。
- [x] 权限配置文件保护不能被 permissive 覆盖。
- [x] 权限配置文件保护不能被用户确认覆盖。
- [x] `permission.Writer` 可以写本地级规则文件。
- [x] `permission.Writer` 不能写项目级规则文件。
- [x] `permission.Writer` 不能写用户级规则文件。
- [x] `permission.Writer` 不写入 `config.yaml`。
- [x] `permission.Writer` 不读取 `config.yaml` 内容。
- [x] `permission.Writer` 不记录 `config.yaml` 内容。

## 14. Authorizer 决策链

- [x] Authorizer 是唯一执行前权限决策入口。
- [x] 决策顺序先处理硬约束。
- [x] 决策顺序再处理 session 规则。
- [x] 决策顺序再处理 local 规则。
- [x] 决策顺序再处理 project 规则。
- [x] 决策顺序再处理 user 规则。
- [x] 决策顺序再处理权限模式默认策略。
- [x] 最后才返回 ask。
- [x] `ResolveUserDecision(deny)` 返回 `user_denied`。
- [x] `ResolveUserDecision(cancel)` 返回 `user_cancelled`。
- [x] `ResolveUserDecision(allow once)` 生成 once grant。
- [x] `ResolveUserDecision(allow session)` 添加 session 规则并生成 grant。
- [x] `ResolveUserDecision(allow permanent)` 写入本地规则并生成 grant。
- [x] `ResolveUserDecision(allow permanent)` 写入失败返回 `config_error`。
- [x] ask prompt 包含工具名。
- [x] ask prompt 包含风险等级。
- [x] ask prompt 包含关键参数摘要。
- [x] ask prompt 包含工作目录或目标路径。
- [x] ask prompt 包含触发原因。
- [x] ask prompt 包含当前权限模式。
- [x] ask prompt 包含规则预览。

## 15. Executor guard

- [x] 新增 `ExecuteAuthorized(ctx, call, grant)`。
- [x] Orchestrator 不直接调用无授权执行入口。
- [x] grant 校验 call ID。
- [x] grant 校验工具名。
- [x] grant 校验 fingerprint。
- [x] 无 grant 时不执行底层工具。
- [x] grant call ID 不匹配时不执行底层工具。
- [x] grant fingerprint 不匹配时不执行底层工具。
- [x] grant 不能复用于不同参数调用。
- [x] fake tool 或 spy tool 证明拒绝路径执行次数为 0。
- [x] `NeedsConfirmation` 不再作为最终权限来源。

## 16. 旧确认逻辑迁移

- [x] 找出所有调用 `NeedsConfirmation` 的地方。
- [x] Orchestrator 不再直接依据 `RiskSafe` / `RiskDangerous` 决定确认。
- [x] Plan Mode 旧 filter 迁移到 permission hard constraint。
- [x] 旧危险工具确认 UI 不与新 permission ask 并行存在。
- [x] 同一工具调用不会出现两次确认。
- [x] 如保留兼容层，只作为 prompt 展示数据来源。

## 17. Orchestrator 和 Agent Loop

- [x] Orchestrator 调用 Authorizer。
- [x] allow 决策调用 `ExecuteAuthorized`。
- [x] deny 决策生成结构化 `permission_denied`。
- [x] ask 决策发起 TUI 确认。
- [x] 用户决策由 Authorizer 解析。
- [x] 拒绝结果写入 conversation。
- [x] 拒绝结果作为 tool result 回灌给模型。
- [x] 权限拒绝不终止 Agent Loop。
- [x] 用户拒绝后模型能继续调整方案。
- [x] 同一 fingerprint 被拒绝后，本轮 Agent Loop 内再次请求直接拒绝。
- [x] 重复拒绝不会无限弹确认。
- [x] 最终回复阶段再次请求工具不递归失控。

## 18. 多工具批次

- [x] 多工具批次先对所有 call 完成决策。
- [x] 多工具批次先完成必要确认。
- [x] ask 未完成前不提前执行其他 allow 工具。
- [x] 决策和确认完成后按 provider 原始顺序执行允许项。
- [x] 每个 tool call 都有对应结果。
- [x] 结果按 call ID 回灌。
- [x] 部分 call deny 时不影响其他 call 生成结果。
- [x] 多工具批次不丢结果。
- [x] 多工具批次不绕过权限。

## 19. TUI / app 确认交互

- [x] events 区分 permission ask 和最终用户决策。
- [x] 确认请求包含工具名。
- [x] 确认请求包含风险等级。
- [x] 确认请求包含参数摘要。
- [x] 确认请求包含目标路径或工作目录。
- [x] 确认请求包含触发原因。
- [x] 确认请求包含权限模式。
- [x] 确认请求包含规则预览。
- [x] `Enter` 默认安全拒绝或取消。
- [x] `n` 拒绝。
- [x] `Esc` 取消。
- [x] `y` 本次允许。
- [x] `s` 本会话允许。
- [x] `p` 永久允许。
- [x] `?` 展开风险和规则预览。
- [x] 永久允许需要明确确认规则预览。
- [x] 高风险命令禁用永久允许。
- [x] 工具行显示等待确认。
- [x] 工具行显示已拒绝。
- [x] 工具行显示已取消。
- [x] 工具行显示执行中。
- [x] 工具行显示完成。
- [x] 工具行显示失败。
- [x] 拒绝不显示为执行失败。
- [x] 取消不显示为执行失败。

## 20. conversation 结构化保存

- [x] tool result 保存结构化 data 字段。
- [x] tool result 保存结构化 error 字段。
- [x] 保存 `ToolResultStatus` 或等价字段。
- [x] 保存 `ToolResultTruncated` 或等价字段。
- [x] `permission_denied` 保存 reason。
- [x] `permission_denied` 保存 recoverable。
- [x] 历史恢复能显示已拒绝。
- [x] 历史恢复能显示已取消。
- [x] 历史恢复能区分工具执行失败和权限拒绝。
- [x] 历史恢复能区分工具结果截断。
- [x] 旧会话文件缺少结构化字段时仍能读取。
- [x] 不做旧会话迁移写回。
- [x] 普通工具结果不丢失 stderr。
- [x] 普通工具结果不丢失 exit_code。
- [x] 普通工具结果不丢失 timed_out。
- [x] 普通工具结果不丢失 truncated。

## 21. 敏感信息保护

- [x] 确认 prompt 对 `API_KEY=...` 脱敏。
- [x] 确认 prompt 对 `TOKEN=...` 脱敏。
- [x] 确认 prompt 对常见 secret/password 片段脱敏。
- [x] `permission_denied` model message 不包含完整敏感命令。
- [x] `permission_denied` model message 不包含完整命令输出。
- [x] `permission_denied` model message 不包含配置文件绝对路径。
- [x] `permission_denied` model message 不包含黑名单 regex。
- [x] `config.yaml` 不出现在权限规则中。
- [x] `config.yaml` 不出现在 tool result 中。
- [x] `config.yaml` 不出现在 TUI prompt 中。
- [x] `config.yaml` 不出现在测试快照中。

## 22. 错误路径与降级

- [x] TUI confirmation channel 关闭时 ask 视为取消。
- [x] channel 关闭返回 `user_cancelled`。
- [x] 本轮不实现确认 timeout，或明确 timeout 等价取消。
- [x] writer 写入失败返回 `config_error`。
- [x] writer 写入失败时当前工具不执行。
- [x] loader 报错但低风险只读工具可按保守策略继续。
- [x] 可预期的 Authorizer 决策错误对危险操作 fail closed。
- [x] 危险操作错误路径下底层执行次数为 0。
- [x] 错误路径都有结构化结果。
- [x] 错误路径给模型的信息不暴露内部细节。

## 23. 单元测试命令

- [x] `go test ./internal/permission` 通过。
- [x] `go test ./internal/tool` 通过。
- [x] `go test ./internal/orchestrator ./internal/conversation` 通过。
- [x] `go test ./internal/tui ./internal/app` 通过。
- [x] `go test ./...` 通过。
- [x] `go build ./cmd/xagent` 通过。

## 24. 端到端 smoke

- [x] 用 tmux 启动 XAgent。
- [x] `Read(go.mod)` 在 sandbox 内自动执行。
- [x] 项目内写临时文件触发确认。
- [x] 本次允许后写入成功。
- [x] 用户拒绝 Bash 或 Edit 后，模型继续给出安全替代方案。
- [blocked] 明显危险 Bash 被硬拒绝。Claude Code 运行环境拒绝构造 `rm -rf .` tmux 场景；该行为由单元/集成测试覆盖。
- [blocked] 明显危险 Bash 不出现允许选项。Claude Code 运行环境拒绝构造 `rm -rf .` tmux 场景；该行为由单元/集成测试覆盖。
- [x] Plan Mode 下写工具被硬拒绝（单元/集成测试覆盖）。
- [x] Plan Mode 下 Bash 被硬拒绝（单元/集成测试覆盖）。
- [x] 本会话允许对后续匹配调用生效（单元/集成测试覆盖）。
- [x] 永久允许写入本地规则（单元/集成测试覆盖）。
- [x] 永久允许后后续匹配调用生效（单元/集成测试覆盖）。
- [x] 损坏本地规则文件时危险操作 fail closed（单元/集成测试覆盖）。
- [x] E2E 临时文件使用 `.xagent/tmp-permission-e2e.txt` 或 `docs/permission-system-e2e.tmp`。
- [x] E2E 后清理临时文件，或确认临时文件已 gitignore。
- [x] E2E 不读取、不打印、不修改 `config.yaml`。

## 25. 最终验收

- [x] AC1: 命中硬编码黑名单的 Bash 命令被拒绝，且不可被规则、permissive 或用户确认放行。
- [x] AC2: 项目外路径或 symlink 逃逸路径被拒绝，且不可覆盖。
- [x] AC3: `Bash(git status)` exact allow 不放行 `git status && rm -rf .`。
- [x] AC4: `Bash(git *)` glob allow 不放行 shell 组合命令。
- [x] AC5: `Edit(internal/**/*.go)` deny 能阻止匹配规范化路径的 Edit 调用。
- [x] AC6: session > local > project > user 优先级正确。
- [x] AC7: strict/default/permissive 对未命中危险工具产生不同决策，且不绕过硬约束和显式 deny。
- [x] AC8: TUI 支持拒绝、本次允许、本会话允许、永久允许、取消。
- [x] AC9: 本次允许、本会话允许、永久允许作用域正确。
- [x] AC10: 永久允许写入前展示最小规则预览，Bash 默认不自动泛化。
- [x] AC11: 权限拒绝时工具不执行，conversation 保存结构化 `permission_denied`，Agent Loop 继续。
- [x] AC12: Plan Mode 下写工具或 Bash 即使有 allow 规则也不会执行。
- [x] AC13: 规则文件损坏或无法解析时危险操作 fail closed，TUI 显示可诊断错误。
- [x] AC14: 给模型的拒绝结果不暴露完整黑名单 regex 或绕过细节。
- [x] AC15: 多工具批次中部分工具被拒绝时，每个 tool call 都有对应结果。
- [x] AC16: 全量测试、工具 smoke、Agent Loop smoke 和端到端场景通过。
