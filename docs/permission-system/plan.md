# Permission System Plan

## 1. 目标与边界

本章在现有工具系统、Agent Loop、路径沙箱、Plan Mode 和 TUI 确认流程之上，增加统一权限系统。权限系统负责在每次工具执行前给出 `allow`、`deny` 或 `ask` 决策，并保证拒绝不会终止 Agent Loop，而是作为结构化 tool result 回灌给模型。

本轮实现范围：

- 高危 Bash 命令硬拦截。
- 文件工具路径沙箱统一纳入权限决策。
- 用户级、项目级、本地级、会话级规则。
- `strict`、`default`、`permissive` 三档权限模式。
- TUI 人在回路确认：拒绝、本次允许、本会话允许、永久允许、取消。
- 结构化 `permission_denied` 工具结果。
- 权限决策异常和规则文件损坏时 fail closed。

不做：

- 网络请求限制。
- CPU、内存、时间、输出大小等资源配额。
- 审计日志查询系统。
- 远程策略同步。
- 多用户隔离。
- OS 级 Bash 文件系统沙箱。

## 2. 总体架构

新增 `internal/permission` 包，Orchestrator 和 Tool Executor 都通过该包完成权限判断。

```text
Provider ToolCall
      │
      ▼
Orchestrator
      │
      ▼
permission.Authorizer.Decide(call, context)
      │
      ├─ deny ─► 生成 permission_denied tool result ─► Agent Loop 继续
      │
      ├─ allow ─► Executor.ExecuteAuthorized(call, grant)
      │
      └─ ask  ─► TUI 确认 ─► once/session/permanent/deny/cancel
                         │
                         ├─ allow ─► Executor.ExecuteAuthorized(call, grant)
                         └─ deny  ─► permission_denied tool result
```

关键原则：

1. 权限决策集中在 `internal/permission`。
2. Orchestrator 负责处理 `ask` 的 UI 往返。
3. Executor 执行前必须校验授权凭证，避免绕过 Orchestrator 直接执行。
4. 硬约束先于任何规则、模式和用户确认。
5. 拒绝是可恢复 tool result，不是 Agent Loop 致命错误。

## 3. 包与文件拆分

```text
internal/permission/
  decision.go      # Decision、Reason、Source、Grant、Context
  mode.go          # strict/default/permissive 策略
  rule.go          # Rule schema、RuleSet、校验
  matcher.go       # exact/glob 匹配、Bash 命令规范化、路径 glob
  blacklist.go     # Bash 硬黑名单 regex
  sandbox.go       # 文件工具路径提取、真实路径校验封装
  loader.go        # 用户级/项目级/本地级 YAML 加载
  session.go       # 会话级临时规则
  writer.go        # 本地级永久规则安全写入
  authorizer.go    # 统一决策链
  result.go        # permission_denied tool result 构造
  permission_test.go
```

需要改动的现有模块：

```text
internal/tool/
  executor.go      # 增加授权执行入口，保留底层执行能力但不暴露给 Orchestrator 直接绕过
  path.go          # 复用或导出真实路径规范化能力

internal/orchestrator/
  tool_batches.go  # 接入 Authorizer、处理 ask 和 permission_denied
  chat.go          # 确保拒绝结果回灌后 Agent Loop 继续

internal/events/
  events.go        # 扩展确认请求和确认决策事件

internal/app/
  update.go        # 支持多种确认动作快捷键

internal/tui/
  messages.go      # 展示等待确认/拒绝/完成状态
  status.go        # 展示等待权限确认状态

internal/config/
  config.go        # 增加 permission mode 与规则文件路径配置
```

## 4. 核心数据结构

### 4.1 决策

```go
type DecisionKind string

const (
    DecisionAllow DecisionKind = "allow"
    DecisionDeny  DecisionKind = "deny"
    DecisionAsk   DecisionKind = "ask"
)

type DenyReason string

const (
    ReasonBlacklist     DenyReason = "blacklist"
    ReasonSandbox       DenyReason = "sandbox"
    ReasonPlanMode      DenyReason = "plan_mode"
    ReasonRuleDeny      DenyReason = "rule_deny"
    ReasonUserDenied    DenyReason = "user_denied"
    ReasonUserCancelled DenyReason = "user_cancelled"
    ReasonConfigError   DenyReason = "config_error"
)

type Decision struct {
    Kind         DecisionKind
    Reason       DenyReason
    Source       Source
    Rule         *Rule
    Grant        *Grant
    Prompt       *ConfirmationPrompt
    ModelMessage string
    Recoverable  bool
}
```

`ModelMessage` 只包含必要原因和安全替代方向，不包含完整黑名单正则、权限文件绝对路径、规则优先级细节或绕过建议。

拒绝信息需要分为用户侧和模型侧两套文案：

```go
type DenialMessage struct {
    UserMessage  string
    ModelMessage string
}
```

- `UserMessage` 用于 TUI，可包含规则展示名、权限模式、目标路径摘要和配置错误层级。
- `ModelMessage` 用于回灌给模型，只包含原因类别、是否可恢复和安全替代方向。
- 两者都不得包含 API key、完整敏感命令输出或可用于绕过硬约束的细节。

### 4.2 授权凭证

```go
type Grant struct {
    CallID      string
    Tool        string
    Scope       GrantScope
    Source      Source
    IssuedAt    time.Time
    Fingerprint string
}

type GrantScope string

const (
    GrantOnce      GrantScope = "once"
    GrantSession   GrantScope = "session"
    GrantPermanent GrantScope = "permanent"
    GrantRule      GrantScope = "rule"
    GrantMode      GrantScope = "mode"
)
```

Executor 只接受带 `Grant` 的执行入口：

```go
func (e *Executor) ExecuteAuthorized(ctx context.Context, call tool.Call, grant permission.Grant) tool.Result
```

Executor 内部校验：

- `grant.CallID` 必须匹配当前 call，除非 grant 来自规则/模式并带有匹配 fingerprint。
- `grant.Tool` 必须匹配当前工具。
- fingerprint 必须由规范化后的工具名和关键参数生成。
- 校验失败返回 `permission_denied` 风格结果，不执行工具。

### 4.3 规则

```go
type Rule struct {
    Tool        string `yaml:"tool"`
    Pattern     string `yaml:"pattern"`
    MatchType   string `yaml:"match_type"`
    Effect      string `yaml:"effect"`
    PathParam   string `yaml:"path_param,omitempty"`
    Description string `yaml:"description,omitempty"`
}

type RuleFile struct {
    Version int    `yaml:"version"`
    Rules   []Rule `yaml:"rules"`
}
```

字段约束：

- `tool`: 必须是已知工具名，例如 `Bash`、`Read`、`Write`、`Edit`、`Glob`、`Grep`。
- `pattern`: exact 或 glob 使用的模式。
- `match_type`: 本轮只支持 `exact` 和 `glob`。
- `effect`: 只能是 `allow` 或 `deny`。
- `path_param`: 文件类工具用于指定匹配哪个路径参数；默认根据工具内置映射推断。
- `description`: 展示用途，不参与匹配。

展示格式由规则转换得到：

```text
Bash(git status)
Bash(git *)
Read(docs/**/*.md)
Edit(internal/**/*.go)
```

## 5. 规则文件位置

采用三层持久化文件加一层内存规则。

```text
用户级：  $XDG_CONFIG_HOME/xagent/permissions.yaml
          若 XDG_CONFIG_HOME 为空，则使用 ~/.config/xagent/permissions.yaml

项目级：  <project>/.xagent/permissions.yaml
          可提交到仓库，表达项目默认策略。

本地级：  <project>/.xagent/permissions.local.yaml
          仅本机使用，保存“永久允许”，默认不提交。

会话级：  运行期内存 RuleSet
          不写入 conversation history，不写入文件。
```

需要更新 `.gitignore`，确保：

```text
.xagent/permissions.local.yaml
```

默认不提交。

如果仓库已有其他配置目录约定，优先复用现有 XAgent 项目内配置目录，但仍要保持“项目级可提交、本地级不提交”的语义。

## 6. 决策优先级

最终优先级固定为：

```text
硬约束
  > 会话级规则
  > 本地级规则
  > 项目级规则
  > 用户级规则
  > 权限模式默认策略
  > ask
```

硬约束包括：

1. Bash 高危黑名单。
2. 文件工具路径沙箱失败。
3. Plan Mode 只读限制。
4. 权限配置文件保护。
5. 明显提权或系统破坏操作。

同一层级多条规则命中时采用：

1. deny 优先于 allow。
2. 如果 effect 相同，选择更具体规则。
3. 具体度相同，按文件顺序靠后的规则优先。

具体度计算：

- exact 高于 glob。
- pattern 字面字符越多越具体。
- 通配符越少越具体。

这样允许先写宽规则，再用后续规则做局部覆盖，同时 deny 仍优先保证安全。

## 7. Bash 匹配策略

### 7.1 命令规范化

Bash 规则匹配前先规范化命令字符串：

- 去掉首尾空白。
- 连续空白折叠为单个空格。
- 保留引号内内容语义，不做 shell 展开。
- 只对完整命令字符串匹配，不做子串匹配。

exact：规范化后完全相等。

glob：必须匹配完整规范化命令。

### 7.2 shell 组合命令处理

在进入普通规则匹配前，先做轻量 shell 组合检测。

普通 Bash 规则只匹配单一简单命令。单一简单命令允许：

- 命令名加普通参数，例如 `git status`。
- 引号包裹的单个参数。
- 不改变执行结构的环境变量前缀是否允许由实现阶段决定；默认不作为 glob allow 的自动放行条件。

以下情况视为复合命令或复杂 shell，普通 `Bash(git *)` 不直接放行：

- `&&`
- `||`
- `;`
- `|`
- backtick
- `$()`
- `<`、`>`、`>>` 等重定向
- here-doc
- newline 分隔多命令
- `sh -c`、`bash -c`、`zsh -c`
- `xargs`
- `find -exec`
- 通过 shell 函数、alias 或 eval 间接执行

复合命令默认进入 `ask` 或按模式 deny，不被宽 glob allow 自动放行。这样 `Bash(git *)` 可以放行单一 git 命令族，但不能放行 `git status && rm -rf .`。

常见但仍属于复杂 shell 的命令，例如 `cd dir && go test`、`git status | cat`、`go test ./... > test.log`，第一版不被 glob allow 自动放行；如果用户需要，可以本次确认或写 exact 规则。

### 7.3 allowlist 风格

Bash 黑名单不是唯一安全边界。普通 Bash 自动允许应优先依赖明确安全命令族，例如：

```yaml
rules:
  - tool: Bash
    pattern: git status
    match_type: exact
    effect: allow
  - tool: Bash
    pattern: go test ./...
    match_type: exact
    effect: allow
  - tool: Bash
    pattern: go test *
    match_type: glob
    effect: allow
```

内置 Bash allowlist 第一版应保持极小：

- 可考虑内置 `pwd`、`git status`、`git diff`、`git log`、`git branch` 这类只读查询。
- 不默认内置 `go test`、`npm test`、`python script.py` 等会执行项目代码的命令；这些命令应通过规则或确认允许。
- 不因为命令看似只读就绕过复杂 shell 检测。

永久允许由 TUI 生成时，Bash 默认生成 exact 规则，不自动泛化成 `Bash(git *)`。

## 8. Bash 硬黑名单

`blacklist.go` 内维护不可配置覆盖的 regex 列表。命中后直接 deny，禁止用户确认放行。

覆盖类别：

- 强制删除：`rm -rf /`、删除项目根以外敏感目录等。
- 磁盘格式化和分区破坏：`mkfs`、`diskutil eraseDisk`、`dd if=... of=/dev/...`。
- 权限破坏：递归 `chmod 777 /`、`chown -R` 系统路径。
- 系统关机重启：`shutdown`、`reboot`、`halt`。
- fork bomb 和资源炸弹。
- 提权：`sudo`、`su`、修改 sudoers。
- 写系统目录：`/System`、`/usr/bin`、`/bin`、`/sbin`、`/etc` 等。
- 破坏 git 历史：`git reset --hard`、`git push --force`、`git clean -fd` 等。

注意：黑名单只拦明显高危模式，不声称覆盖所有风险；其他未明确安全的 Bash 仍由规则、模式和确认控制。

## 9. 文件路径沙箱

文件工具包括：

- `Read`
- `Write`
- `Edit`
- `Glob`
- `Grep`

权限系统复用并强化 `internal/tool` 的路径解析能力：

1. 原始路径只用于展示。
2. 决策前转换为项目根真实路径下的规范化相对路径。
3. 对不存在的深层路径，查找最深已存在祖先。
4. 解析该祖先 symlink 后再拼接剩余路径。
5. 最终真实路径必须仍在项目根真实路径内。
6. 规则匹配使用规范化后的项目相对真实路径。

禁止绕过方式：

- `../`
- 重复分隔符
- symlink 别名
- Unicode 规范化差异
- 指向项目外的 symlink 文件或目录

路径沙箱失败直接 deny，不能由规则、模式或用户确认覆盖。

### 9.1 文件工具默认路径参数

文件类工具默认使用以下参数做 sandbox 和规则匹配：

```text
Read:  path
Write: path
Edit:  path
Glob:  path/root，取实际搜索根；缺省时使用项目根
Grep:  path/root，取实际搜索根；缺省时使用项目根
```

`Glob` 和 `Grep` 的规则匹配搜索根路径，不匹配 glob pattern 或 grep pattern 本身。搜索执行过程中仍要对每个候选结果逐项做真实路径校验，指向项目外的 symlink 文件或目录不得返回、读取或编辑。

## 10. 权限配置文件保护

权限系统自身配置文件受硬约束保护：

- `.xagent/permissions.yaml`
- `.xagent/permissions.local.yaml`
- 用户级 `permissions.yaml`

默认策略：

- 普通工具调用不能直接编辑权限配置文件。
- 永久允许只能通过 `permission.Writer` 写入本地级规则文件。
- 写入前必须 validate。
- 写入必须使用安全权限和原子写入。
- 不写入 `config.yaml`。
- 不记录完整敏感命令输出。

如果用户明确要求手工编辑权限文件，仍需要专门确认路径，但不得绕过 schema 校验；更推荐通过权限确认流生成规则。

## 11. 权限模式

模式只处理未命中显式规则后的默认策略，不覆盖硬约束和显式 deny。权限模式不参与规则冲突计算，永远不能把已经命中的 deny 改成 allow。

Plan Mode 是硬约束，不属于权限模式：

- Plan Mode 下 `Read`、`Glob`、`Grep` 可在路径沙箱内执行。
- Plan Mode 下 `Write`、`Edit`、`Bash` 一律 deny。
- Plan Mode deny 不进入 ask，即使存在 allow 规则或用户愿意确认也不能执行。

### strict

- 文件只读工具在沙箱内可 allow。
- 写工具默认 ask。
- Bash 默认 ask 或 deny，只对非常明确的安全命令可由内置默认 allow。
- 高风险工具不允许永久允许。

### default

- 文件只读工具在沙箱内 allow。
- `Glob`、`Grep` 在沙箱内 allow。
- `Write`、`Edit`、`Bash` 默认 ask。
- 简单只读 Bash 命令可 ask 或由内置安全规则 allow，例如 `git status`、`pwd`。

### permissive

- 沙箱内文件读写可减少 ask。
- 简单单一 Bash 命令可减少 ask。
- 复合 shell、高风险命令、显式 deny、黑名单、Plan Mode 仍不能绕过。
- 权限配置文件保护仍生效。

模式来源优先级：

1. CLI flag 或运行期设置。
2. 项目配置。
3. 用户配置。
4. 默认 `default`。

## 12. 人在回路确认

当决策为 `ask` 时，Orchestrator 发送确认事件给 TUI。

确认提示必须展示：

- 工具名。
- 风险等级。
- 关键参数摘要。
- 工作目录或目标路径。
- 触发原因。
- 匹配规则来源或未命中说明。
- 当前权限模式。
- 允许范围影响。
- 将生成的 session/permanent 规则预览。

敏感参数需要脱敏。

### 12.1 确认动作

```go
type PermissionAction string

const (
    PermissionDeny      PermissionAction = "deny"
    PermissionAllowOnce PermissionAction = "allow_once"
    PermissionAllowSession PermissionAction = "allow_session"
    PermissionAllowPermanent PermissionAction = "allow_permanent"
    PermissionCancel    PermissionAction = "cancel"
)
```

行为：

- 拒绝：不执行，返回 `permission_denied`，reason=`user_denied`。
- 本次允许：只给当前 call 生成 once grant。
- 本会话允许：生成内存规则，后续匹配 call 生效。
- 永久允许：写入本地级权限规则文件。
- 取消：不执行，返回 `permission_denied`，reason=`user_cancelled`。

### 12.2 TUI 快捷键

建议快捷键：

```text
Esc / n / Enter: 拒绝或取消，默认安全路径
 y: 本次允许
 s: 本会话允许
 p: 永久允许
 ?: 展开规则预览和风险说明
```

`p` 只在允许永久保存的风险等级下显示。高风险工具或命令可以禁用永久允许，只允许本次或本会话。

永久允许需要先展示将写入的最小规则预览。若用户按下 `p`，TUI 至少要要求用户明确确认预览内容；第一版可以通过二次确认状态实现，避免误触把一次授权变成长期授权。

工具行状态区分：

- 等待确认。
- 已拒绝。
- 已取消。
- 执行中。
- 完成。
- 失败。

用户拒绝不显示为工具执行失败，因为工具没有执行；取消在 UI 上显示“已取消”，tool result reason 使用 `user_cancelled`。

## 13. 永久规则写入

永久允许写入本地级文件：

```text
<project>/.xagent/permissions.local.yaml
```

写入流程：

1. 根据当前 call 生成最小规则。
2. TUI 展示规则预览。
3. 用户确认永久允许。
4. 加载现有本地规则。
5. validate 全量规则。
6. 去重。
7. 原子写入临时文件再 rename。
8. 文件权限使用 `0600`。

Bash 规则默认 exact：

```yaml
version: 1
rules:
  - tool: Bash
    pattern: git status
    match_type: exact
    effect: allow
    description: Permanent allow from confirmation
```

文件工具规则默认使用当前规范化相对路径 exact；只有用户在 UI 中明确选择泛化时才生成 glob，本轮可以先不做 UI 泛化。

永久写入失败时不得自动执行当前 tool call。系统应返回可恢复的 `permission_denied`，reason=`config_error`，并在 UI 提示用户可以改选“本次允许”重新授权当前调用。

## 14. permission_denied tool result

拒绝时工具不得执行，系统生成结构化结果。

示例：

```json
{
  "status": "error",
  "summary": "Permission denied before executing Bash",
  "error": {
    "code": "permission_denied",
    "reason": "blacklist",
    "message": "This command is blocked by a non-overridable safety rule.",
    "recoverable": true,
    "suggestion": "Choose a safer read-only command or ask the user for a different approach."
  }
}
```

会话保存要求：

- role 仍为 tool result。
- content 保存结构化 JSON。
- `ToolResultData` 保留结构化字段。
- 不暴露完整黑名单 regex。
- 不暴露权限文件绝对路径。
- 不给出绕过建议。

连续重复相同被拒绝操作时，Orchestrator 根据 call fingerprint 限制重复确认：

- 同一 fingerprint 被用户拒绝后，本轮 Agent Loop 内再次请求可直接返回拒绝结果。
- 避免无限确认/拒绝循环。

## 15. 并发工具批次

多个工具调用并发到达时，策略为逐个确认、逐个回灌：

1. 先对所有 call 做硬约束和规则决策。
2. `allow` 的 call 可执行。
3. `deny` 的 call 生成对应结果。
4. `ask` 的 call 按风险从高到低逐个确认。
5. 每个 tool call 都必须按原 call ID 生成结果。
6. 最终回灌顺序保持 provider tool call 顺序。

如果当前 Orchestrator 仍不支持多工具执行，本轮至少要保证：

- 多工具不直接执行。
- 每个 tool call 都有结构化不支持或拒绝结果。
- Agent Loop 不崩溃、不递归失控。

## 16. Orchestrator 集成

`executeOneTool` 调整为：

```go
decision := authorizer.Decide(ctx, call, permission.Context{
    ProjectRoot: root,
    Mode: currentMode,
    PlanMode: planMode,
    ConversationID: id,
})

switch decision.Kind {
case permission.DecisionAllow:
    return executor.ExecuteAuthorized(ctx, call, *decision.Grant)
case permission.DecisionDeny:
    return permission.ToolResult(call, decision)
case permission.DecisionAsk:
    userDecision := confirmation.Ask(decision.Prompt)
    followUp := authorizer.ResolveUserDecision(call, decision, userDecision)
    if followUp.Kind == permission.DecisionAllow {
        return executor.ExecuteAuthorized(ctx, call, *followUp.Grant)
    }
    return permission.ToolResult(call, followUp)
}
```

Plan Mode 不再只散落在 Orchestrator 过滤逻辑中，而是作为 `permission.Context.PlanMode` 输入，由硬约束统一处理。

## 17. Executor 集成

现有 `NeedsConfirmation` 逐步废弃：

- 第一阶段保留但不再作为最终权限来源。
- 新增 `ExecuteAuthorized`。
- Orchestrator 不再直接调用 `NeedsConfirmation` 决定危险工具确认，也不直接调用无授权执行入口，而是调用 Authorizer。
- Executor 内仍保留最终 guard，防止误用。
- 测试必须覆盖绕过 Orchestrator 直接调用 Executor 的场景：无 grant、grant call ID 不匹配、grant fingerprint 不匹配都不能执行底层工具。

底层工具实现不直接知道权限规则，只依赖 Executor 传入已授权调用。

## 18. 配置加载与错误策略

规则文件加载：

- 不存在：视为空规则。
- YAML 解析失败：记录 config_error。
- schema 校验失败：记录 config_error。
- 用户级损坏：TUI 提示用户级配置错误；危险操作 fail closed；低风险只读操作可按保守策略继续。
- 项目级损坏：TUI 提示项目级配置错误；写工具和 Bash fail closed；低风险只读操作按保守策略继续。
- 本地级损坏：TUI 提示本地级配置错误；写工具、Bash、永久允许写入 fail closed。
- 会话级规则只存在内存中，若发生非法状态视为内部错误，危险操作 fail closed。

给模型的 tool result 只说明配置错误导致无法安全执行，不暴露完整路径和内部细节。

## 19. 测试计划

### 19.1 permission 包单元测试

- 黑名单命中后 deny，不能被 allow 规则、permissive、用户确认覆盖。
- Bash exact allow 只匹配完整规范化命令。
- `Bash(git *)` 不匹配 `git status && rm -rf .`。
- `Bash(git *)` 不匹配管道、重定向、`sh -c`、`xargs`、`find -exec` 等复杂 shell。
- 内置 Bash allowlist 不包含会执行项目代码的测试/脚本命令。
- 文件 glob 匹配规范化项目相对真实路径。
- `Read`、`Write`、`Edit`、`Glob`、`Grep` 默认路径参数映射正确。
- 同层冲突 deny 优先。
- 规则层级 session > local > project > user。
- 模式只处理未命中规则的默认策略，不能覆盖显式 deny。
- Plan Mode 下写工具和 Bash deny，且不进入 ask。
- 配置损坏时危险操作 fail closed。
- 永久规则写入 validate、去重、原子写入。

所有 deny 类测试都必须断言底层工具没有执行；只断言返回 deny 不够。

### 19.2 tool/executor 测试

- 无 grant 调用执行入口不能执行危险工具。
- grant call ID 不匹配时拒绝。
- grant fingerprint 不匹配时拒绝。
- grant 不能被复用到不同参数的工具调用。
- 沙箱失败时不进入底层工具执行。
- 使用 fake executor 或 spy tool 记录底层执行次数，确保权限拒绝路径执行次数为 0。

### 19.3 orchestrator 测试

- 权限 deny 生成结构化 tool result，Agent Loop 继续。
- 权限 deny 时底层 executor 调用次数为 0。
- 用户拒绝返回 reason=`user_denied`，底层工具不执行。
- 用户取消返回 reason=`user_cancelled`，底层工具不执行。
- 本次允许只影响当前 call。
- 本会话允许对后续匹配 call 生效。
- 永久允许写入本地规则并对后续 call 生效。
- 永久允许写入失败时当前 call 不自动执行，返回 `config_error`。
- 重复拒绝同一 fingerprint 不无限弹确认。
- 多工具批次部分 allow、部分 deny 时每个 call 都有结果。

### 19.4 TUI/app 测试

- 确认提示展示工具名、风险、参数摘要、触发原因、模式、规则预览。
- `Enter` 默认安全拒绝。
- `y` 本次允许。
- `s` 本会话允许。
- `p` 永久允许。
- 永久允许展示最小规则预览，并需要明确确认。
- 高风险命令不显示或禁用永久允许。
- 工具时间线显示等待确认、已拒绝、已取消、执行中、完成、失败。
- 拒绝和取消不显示为执行失败。

### 19.5 端到端 smoke

按 `CLAUDE.md` 要求用 tmux 启动 XAgent：

1. 输入读取文件请求，确认 Read 在沙箱内自动执行。
2. 输入写文件请求，确认出现权限提示，本次允许后执行。
3. 输入 Bash `git status`，验证 exact allow 或确认流。
4. 输入明显危险命令，验证硬拒绝且不能确认放行。
5. 输入 Plan Mode 下写入请求，验证硬拒绝。
6. 用户拒绝后，验证模型继续给出安全替代方案。

基础命令：

```text
go test ./internal/permission
go test ./internal/tool
go test ./internal/orchestrator ./internal/conversation
go test ./internal/tui ./internal/app
go test ./...
go build ./cmd/xagent
```

## 20. 实施顺序

1. 建立 `internal/permission` 基础类型、规则 schema、matcher 和测试。
2. 实现 Bash 黑名单和 Bash 命令规范化。
3. 接入文件路径沙箱规范化和权限路径匹配。
4. 实现规则加载、层级优先级和模式默认策略。
5. 实现 session 规则和本地永久规则 writer。
6. 在 Executor 增加 `ExecuteAuthorized` guard。
7. 在 Orchestrator 接入 Authorizer 和 `permission_denied` tool result。
8. 扩展 events/app/TUI 确认动作。
9. 增加配置项和本地规则 gitignore。
10. 跑单元测试、全量测试和 tmux 端到端 smoke。

## 21. 风险与取舍

- Bash 无 OS 级沙箱：本章只通过规则、黑名单、模式和确认降低风险，不声称 Bash 不能访问项目外路径。
- shell 解析复杂：本轮不实现完整 shell AST，只做保守复杂命令检测；检测不清的命令进入 ask 或 deny。
- 永久规则泛化风险：默认只生成 exact 规则，避免一次确认变成宽授权。
- 配置损坏可用性下降：危险操作 fail closed 是预期行为，只读低风险操作尽量保持可用。
- 多工具批次支持若当前架构不足，本轮先保证不绕过权限、不丢结果、不阻断 Agent Loop。

## 22. 验收对应关系

- AC1、AC3、AC4：由 Bash blacklist、命令规范化和 matcher 测试覆盖。
- AC2、AC5：由 sandbox 和文件规则测试覆盖。
- AC6：由多层 RuleSet 优先级测试覆盖。
- AC7：由 mode 策略测试覆盖。
- AC8、AC9、AC10：由 TUI/app/orchestrator 确认流测试覆盖。
- AC11、AC14：由 `permission_denied` 结构化结果测试覆盖。
- AC12：由 Plan Mode 硬约束测试覆盖。
- AC13：由 loader config_error fail-closed 测试覆盖。
- AC15：由多工具批次测试覆盖。
- AC16：由全量测试、tool smoke、Agent Loop smoke、tmux E2E 覆盖。
