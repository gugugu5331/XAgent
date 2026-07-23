# Lifecycle Hook System Tasks

> 本文只拆解已批准的 `spec.md` 与 `plan.md`。每个任务是一个聚焦、可独立验证的工作单元；四份文档全部批准前不得执行这些实现任务。

## 文件清单

### 新建生产文件

| 文件 | 职责 |
| --- | --- |
| `internal/matcher/matcher.go` | 权限与 Hook 共用的区分大小写、完整值 exact/glob 核心 |
| `internal/hook/api.go` | Runtime、事件/状态枚举、token、输入输出和工具决策类型 |
| `internal/hook/config.go` | YAML DTO、字段 presence、编译后规则和来源类型 |
| `internal/hook/limits.go` | F59–F60 全部硬上限的单一来源 |
| `internal/hook/loader.go` | 两个固定文件的有界读取、严格解析、环境展开和稳定合并 |
| `internal/hook/validate.go` | schema、字段目录、action union、组合矩阵和限制校验 |
| `internal/hook/event.go` | EventContext、12 类事件 builder、冻结与规范 JSON |
| `internal/hook/fields.go` | 点路径编译、事件字段目录和动态参数读取 |
| `internal/hook/condition.go` | all/any、typed exact、glob/regex 和 negate |
| `internal/hook/template.go` | 严格标量占位符编译与原子渲染 |
| `internal/hook/diagnostic.go` | 稳定 Hook 诊断码和无载荷诊断构造 |
| `internal/hook/decision.go` | allow/deny 严格 JSON 协议解析 |
| `internal/hook/command.go` | 受控环境、stdin、输出限制和 Command Runner 公共逻辑 |
| `internal/hook/command_unix.go` | Unix 进程组、TERM/KILL 和收束实现 |
| `internal/hook/command_other.go` | 非 Unix 平台可编译的受限取消实现 |
| `internal/hook/http.go` | URL/Header、loopback、redirect、响应限制和 HTTP Runner |
| `internal/hook/prompt_state.go` | next/turn/session Prompt、预算、排序、lease 和 send gate |
| `internal/hook/once.go` | idle/pending/done 进程内状态机 |
| `internal/hook/async.go` | 4 worker、64 queue、admission 和有界关闭 |
| `internal/hook/engine.go` | 稳定 dispatch、动作路由、生命周期状态和失败隔离 |
| `internal/hook/noop.go` | 无配置时的零行为 Runtime |
| `internal/tool/validation.go` | `ValidatedCall` 与注册成员/JSON object 一次性预检 |
| `docs/hook-system/example.yaml` | 可解析的完整 Hook 示例 |

### 新建测试文件

| 文件 | 职责 |
| --- | --- |
| `internal/matcher/matcher_test.go` | exact/glob 完整值回归 |
| `internal/hook/api_test.go` | 事件枚举、Runtime/no-op 和 token API |
| `internal/hook/loader_test.go` | 路径、严格 YAML、合并、位置、环境与配置边界 |
| `internal/hook/event_test.go` | 12 类载荷、深复制、sequence 和 JSON 上限 |
| `internal/hook/fields_test.go` | 字段目录、点路径和动态参数读取 |
| `internal/hook/condition_test.go` | typed matcher、逻辑组合、negate 和缺失字段 |
| `internal/hook/template_test.go` | 严格 token、标量格式和原子失败 |
| `internal/hook/diagnostic_test.go` | Hook 诊断码、属性和敏感 canary |
| `internal/hook/decision_test.go` | 精确决策协议与安全 reason |
| `internal/hook/command_test.go` | cwd/env/stdin、输出、取消、timeout 和进程组 |
| `internal/hook/http_test.go` | URL/Header、DNS、redirect、body、status 和 timeout |
| `internal/hook/prompt_state_test.go` | scope、预算、lease、generation 和并发 barrier |
| `internal/hook/once_test.go` | reserve/commit/release 并发状态机 |
| `internal/hook/async_test.go` | 容量、排队 timeout、close race 和泄漏 |
| `internal/hook/engine_test.go` | 顺序、fail-open、deny、生命周期和 SubAgent no-op |
| `internal/hook/limits_test.go` | F59–F60 的 limit/limit+1 |
| `internal/hook/docs_test.go` | example 解析与 README 风险警告 |
| `internal/provider/openai_hook_test.go` | OpenAI ordered blocks 与 request attempt |
| `internal/provider/anthropic_hook_test.go` | Anthropic cache boundary 与异步 attempt |
| `internal/provider/request_observer_test.go` | `MarkSent`/`Finish` 终态握手 barrier |
| `internal/orchestrator/hook_test.go` | Turn/Message/Tool/Prompt 综合接线 |
| `internal/orchestrator/independent_hook_test.go` | 独立 Skill 生命周期和状态隔离 |
| `internal/contextmgr/hook_test.go` | 真实压缩尝试和 observer 边界 |
| `internal/app/hook_test.go` | System/Session、取消、切换与关闭 |
| `internal/app/hook_e2e_test.go` | `integration` build tag 的 scripted TUI 端到端场景 |
| `internal/tui/program_test.go` | 返回最终 Model 的兼容测试 |
| `cmd/xagent/hook_startup_test.go` | 早期加载、start/stop 和组合 closer 顺序 |

### 修改现有文件

| 文件 | 职责 |
| --- | --- |
| `internal/permission/matcher.go`、`permission_test.go` | 复用共享 matcher 并锁定旧 YAML/评分语义 |
| `internal/permission/authorizer.go` | 拆分 Normalize、Plan/hard、ordinary 和已规范化确认入口 |
| `internal/diagnostics/diagnostic.go`、`collector.go`、`diagnostic_test.go` | 有界 attributes、深复制、稳定清理和旧输出兼容 |
| `internal/redact/redact.go`、`redact_test.go` | Hook 展开秘密注册和短 secret 安全匹配 |
| `internal/tool/executor.go`、`tool.go`、`tool_test.go` | 授权执行、兼容回归和 `hook_denied` |
| `internal/prompt/section.go`、`builder.go`、`prompt_test.go` | 固定安全块后的 Hook ordered blocks |
| `internal/provider/provider.go`、`openai.go`、`anthropic.go` | ordered System、缓存策略和 attempt observer |
| `internal/provider/tool_parse_test.go`、`anthropic_schema_test.go` | Provider legacy/ordered/cache 回归 |
| `internal/orchestrator/run_request.go` | `RunResult.Err` 与 Hook turn status 映射 |
| `internal/orchestrator/chat.go`、`execution_state.go`、`agent_loop.go` | execution ref、统一 run/message wrapper、Prompt lease 和 run tracker |
| `internal/orchestrator/tool_batches.go`、`skill_runtime.go` | 公共工具安全链、deny 回流和 `load_skill` system route |
| `internal/orchestrator/independent.go` | isolated turn/message/tool/compact 生命周期 |
| `internal/orchestrator/chat_test.go`、`skill_runtime_test.go`、`independent_test.go` | 既有行为与新 wrapper 的回归适配 |
| `internal/contextmgr/manager.go`、`manager_test.go` | 非变异 preflight、PrepareOptions 和 CompactionObserver |
| `internal/sessionctx/manager.go`、`manager_test.go` | persist/transient prepare 透传 |
| `internal/app/deps.go`、`app.go`、`update.go`、`skills.go` | Hook 注入、共享 lifecycleState、Session 和取消收尾 |
| `internal/app/update_test.go`、`commands_test.go`、`skills_test.go` | App 取消、命令排除和直接 isolated 回归 |
| `internal/tui/program.go` | 返回最终 Model 与 error |
| `cmd/xagent/main.go`、`main_test.go` | `run() error`、早期严格加载和幂等组合关闭 |
| `.gitignore` | 允许提交项目级 `.xagent/hooks.yaml` |
| `README.md` | 配置、重启、受信代码和 fail-open 风险说明 |

## 任务执行约定

- 每个任务按 2–5 分钟的单一行为增量执行；若实际改动超出该范围，先按步骤继续拆分，不把多个任务一次合并完成。
- 每个实现任务必须同步增加或更新其“验证”中命名的最小测试；测试文件落在上方文件清单对应包内，即使任务的“文件”字段只列出主要生产文件。
- 并发顺序用 channel/barrier 驱动，不用 `time.Sleep` 证明时序；网络测试只使用注入 transport 或本机 `httptest`，绝不访问公网或真实 Provider。
- 每个任务先运行验证并保存证据，再标记完成；验证失败时修实现或任务设计，不放宽已批准 spec 的断言。

## A. 向后兼容基础

### T001：实现共享 exact/glob matcher

**文件：** `internal/matcher/matcher.go`

**依赖：** 无

**步骤：**

1. 提供区分大小写、无隐式 trim、完整值匹配的 string exact 与 glob API。
2. 固定空字符串、换行、`*`/`?` 和非法 pattern 的返回语义，不加入 regex 或 negate。

**验证：** 运行 `go test -count=1 ./internal/matcher -run TestMatch`，期望新包编译且 matcher 边界全部通过。

### T002：锁定共享 matcher 测试

**文件：** `internal/matcher/matcher_test.go`

**依赖：** T001

**步骤：**

1. 表驱动覆盖 exact、glob、大小写、首尾空白、换行和完整值负例。
2. 重复运行相同用例，断言结果不依赖 map 或环境状态。

**验证：** 运行 `go test -count=20 ./internal/matcher`，期望每次都返回 `ok`。

### T003：让权限模块委托共享 matcher

**文件：** `internal/permission/matcher.go`

**依赖：** T001

**步骤：**

1. 只把权限的 string exact/glob 底层比较改为调用 `internal/matcher`。
2. 保留命令/路径规范化、规则评分、匹配层级和权限公开 schema，不暴露 regex/negate。

**验证：** 运行 `go test -count=1 ./internal/permission -run 'Test.*Match|Test.*Rule'`，期望旧 matcher 结果不变。

### T004：增加权限 matcher 兼容回归

**文件：** `internal/permission/permission_test.go`

**依赖：** T003

**步骤：**

1. 锁定 exact/glob 的规范化、评分和层级选择结果。
2. 增加权限 YAML 拒绝 regex、negate 和未知字段的负例。

**验证：** 运行 `go test -count=1 ./internal/permission`，期望全部既有与新增回归通过。

### T005：扩展 Diagnostic attributes 模型

**文件：** `internal/diagnostics/diagnostic.go`

**依赖：** 无

**步骤：**

1. 为 `Diagnostic` 增加可选 `Attributes map[string]string` 和复制式构造方法。
2. 保持无 attributes 时的 JSON 字段、`Text()` 内容和旧调用方式不变。

**验证：** 运行 `go test -count=1 ./internal/diagnostics -run TestDiagnostic`，期望旧序列化断言继续通过。

### T006：实现 attributes 有界收集

**文件：** `internal/diagnostics/collector.go`

**依赖：** T005

**步骤：**

1. 在 `Add` 和 `List` 两侧深复制 attributes，并按 key 稳定排序展示。
2. 清理无效 UTF-8、ANSI/终端控制字符并执行 8 项、64 B key、256 B value、2 KiB 总量限制和统一脱敏；任一 attributes 边界超限时整体丢弃该 map，保留基础 Diagnostic，不截成部分属性。

**验证：** 运行 `go test -count=1 ./internal/diagnostics -run TestCollectorAttributes`，期望合法属性稳定保留、任一超限属性整体丢弃且内部状态不可被调用方修改。

### T007：覆盖 diagnostics 兼容与并发测试

**文件：** `internal/diagnostics/diagnostic_test.go`

**依赖：** T006

**步骤：**

1. 覆盖 attributes 深复制、排序、限长、控制字符和 secret canary。
2. 并发 Add/List 后修改返回 map，断言 Collector 内部数据不变且旧无属性输出逐字保持。

**验证：** 运行 `go test -race -count=1 ./internal/diagnostics`，期望无竞态并全部通过。

### T008：实现 Hook secret 注册语义

**文件：** `internal/redact/redact.go`

**依赖：** 无

**步骤：**

1. 保留非空秘密去重和最长优先，并支持 Loader 在启动编译期注册展开值。
2. 对很短秘密只做完整 token/字段值匹配，不做破坏普通文本的单字符全局替换。

**验证：** 运行 `go test -count=1 ./internal/redact -run 'TestRuntimeRedactor.*Secret'`，期望长短秘密均按约定脱敏。

### T009：覆盖短 secret 与并发回归

**文件：** `internal/redact/redact_test.go`

**依赖：** T008

**步骤：**

1. 覆盖重复、空值、重叠长短秘密、单字符秘密和敏感字段完整值。
2. 并发 Register/Text/MaxSecretBytes，断言输出确定且无竞态。

**验证：** 运行 `go test -race -count=1 ./internal/redact`，期望所有 canary 被安全处理且普通文本不被误伤。

### T010：定义一次性 ValidatedCall

**文件：** `internal/tool/validation.go`

**依赖：** 无

**步骤：**

1. 定义携带原 Call、注册 Tool 和 `map[string]any` 参数的 `ValidatedCall`。
2. `Registry.ValidateCall` 先验证注册成员，再把空白参数归一为 `{}`；其他输入只接受单个 JSON object、EOF 且使用 `UseNumber`。

**验证：** 运行 `go test -count=1 ./internal/tool -run TestRegistryValidateCall`，期望新 API 可编译。

### T011：覆盖 ValidatedCall 结构预检

**文件：** `internal/tool/tool_test.go`

**依赖：** T010

**步骤：**

1. 覆盖未知工具、空白、object、array/scalar、trailing JSON、重复顶层值和非法 JSON。
2. 断言大整数和十进制保持 `json.Number`，不会转换为 `float64`。

**验证：** 运行 `go test -count=1 ./internal/tool -run TestRegistryValidateCall`，期望全部正反用例通过。

### T012：增加 validated 授权执行入口

**文件：** `internal/tool/executor.go`

**依赖：** T010–T011

**步骤：**

1. 增加 `ExecuteValidatedAuthorized`，用同一个 ValidatedCall 校验 Grant fingerprint 并调用 Tool。
2. 保留 `ExecuteAuthorized`/`Execute` 兼容 wrapper，但只预检一次后委托，实际执行不再普通 `json.Unmarshal`。

**验证：** 运行 `go test -count=1 ./internal/tool -run TestExecuteValidatedAuthorized`，期望参数类型和授权指纹贯穿不变。

### T013：锁定 Executor 兼容行为

**文件：** `internal/tool/tool_test.go`

**依赖：** T012

**步骤：**

1. 覆盖 grant tool/call ID/fingerprint 不匹配、timeout、输出限长和兼容旧入口。
2. 用记录型 Tool 断言执行时收到的参数 map 与 ValidatedCall 相同。

**验证：** 运行 `go test -count=1 ./internal/tool`，期望旧 Tool 行为和新 validated 路径全部通过。

### T014：增加 Hook deny 错误码

**文件：** `internal/tool/tool.go`、`internal/tool/tool_test.go`

**依赖：** 无

**步骤：**

1. 定义 `ErrHookDenied = "hook_denied"`，不改变 `permission_denied`。
2. 锁定 denied result 的 code、model content 与 `recoverable: true` 序列化。

**验证：** 运行 `go test -count=1 ./internal/tool -run TestHookDeniedError`，期望新旧拒绝码互不混淆。

## B. 配置、严格加载与编译

### T015：集中定义 Hook 硬上限

**文件：** `internal/hook/limits.go`、`internal/hook/limits_test.go`

**依赖：** 无

**步骤：**

1. 将 F59–F60 的文件、规则、条件、Command、HTTP、Prompt、SubAgent、decision 和诊断上限集中为默认 Limits。
2. 提供测试可注入的副本/校验入口，生产默认值不可被调用方修改。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestDefaultLimits`，期望每个常量与 spec 一致。

### T016：定义 Hook 配置 DTO 与 presence

**文件：** `internal/hook/config.go`

**依赖：** T015

**步骤：**

1. 定义 File、RuleConfig、ConditionGroup、Predicate、ActionConfig、Source 和 Duration。
2. 记录 `timeout`、`decision`、`send_event` 等字段是否显式出现，不能用 Go 零值猜测配置意图。
3. Duration 只接受 YAML string scalar 的 Go duration 语法；拒绝错误节点类型、空串、裸数字、未知单位和非法字符串。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestConfigPresence`，期望未写与显式 false/zero 可区分。

### T017：定义 Runtime API 与 no-op

**文件：** `internal/hook/api.go`、`internal/hook/noop.go`、`internal/hook/api_test.go`

**依赖：** T015

**步骤：**

1. 定义 12 个事件、Execution/Session/Turn/Message/Tool/Compact 类型、Runtime、PromptLease 与 ToolDecision。
2. 提供所有方法 nil-safe、无 goroutine、无 Prompt、只返回 Continue 的 no-op Runtime。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestAPIEnums|TestNoopRuntime'`，期望 API 完整且 no-op 无副作用。

### T018：实现固定路径和有界读取

**文件：** `internal/hook/loader.go`

**依赖：** T016

**步骤：**

1. 只读取 `~/.config/xagent/hooks.yaml` 与 `<project>/.xagent/hooks.yaml`，缺失文件忽略。
2. 对同一次打开的 reader 使用 `io.LimitReader(max+1)`，在解析前拒绝超过 256 KiB 的内容。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestLoadDiscovery`，期望路径顺序固定且 size check 无 stat/read TOCTOU。

### T019：实现严格 YAML 结构扫描

**文件：** `internal/hook/loader.go`

**依赖：** T018

**步骤：**

1. 解码为 `yaml.Node` 并显式遍历顶层、规则、条件和 action 对象。
2. 拒绝第二 document、重复/非标量 key、anchor、alias、merge key、custom tag 和错误节点类型。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestLoadYAMLStructure`，期望每种非法结构启动失败。

### T020：校验顶层、必填项和枚举

**文件：** `internal/hook/validate.go`

**依赖：** T016、T019

**步骤：**

1. 严格校验 `version: 1`、`hooks`、必填 event/action 和所有未知字段。
2. 只接受 12 个事件、4 个 action、合法 match/scope/method 和已声明的 schema 值。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestValidateRequiredFields`，期望未知版本/事件/action/字段均失败。

### T021：校验条件结构

**文件：** `internal/hook/validate.go`

**依赖：** T019–T020

**步骤：**

1. 要求 `if` 只含一个非空 all 或 any，不允许嵌套或混用。
2. 校验每个 predicate 的 field/match/value/negate presence；value 只接受 string、有限十进制 number 或 bool，glob/regex 只接受 string，并拒绝 null、NaN/Inf、时间、对象和数组。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestValidateConditionSchema`，期望所有非法逻辑结构被集中报告。

### T022：校验 action union 并应用默认值

**文件：** `internal/hook/validate.go`

**依赖：** T020

**步骤：**

1. 按 action type 应用字段白名单，显式出现的不适用字段即使为 false/空值也拒绝。
2. 明确要求非空/非纯空白的 Command `command`、HTTP `url`、Prompt `content`、SubAgent `agent` 与 `input`，并覆盖缺失、空值和错误类型。
3. 只在字段未配置时应用 Command 30s、HTTP 10s、POST、`send_event=true` 和 Prompt `scope=turn`。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestValidateActionUnion`，期望 union 和默认值用例全部通过。

### T023：校验事件与执行控制组合

**文件：** `internal/hook/validate.go`

**依赖：** T021–T022

**步骤：**

1. 实现 F38 Prompt event/scope 完整矩阵，拒绝 Prompt timeout 和所有 Prompt async。
2. 拒绝 `tool_before` async、非法 decision 位置以及非正数/超过 10 分钟的适用 timeout。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestValidateCombinationMatrix`，期望矩阵内组合通过、矩阵外组合失败。

### T024：校验配置容器硬上限

**文件：** `internal/hook/validate.go`、`internal/hook/limits_test.go`

**依赖：** T023

**步骤：**

1. 对单文件 YAML、每文件规则数和每规则 predicate 数量应用 T015 的限制。
2. 为 256 KiB、256 rules、32 predicates 写精确 limit 与 limit+1；各 action 自身边界由对应编译任务验证。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestConfigContainerLimits`，期望三个配置容器边界都有正反证据。

### T025：实现环境变量严格展开

**文件：** `internal/hook/loader.go`、`internal/hook/validate.go`

**依赖：** T008、T022

**步骤：**

1. 只展开 `${ENV_NAME}`，支持 `$${...}` 字面量；拒绝未定义变量、NUL 和事件占位符。
2. 依据变量名、env key 或 Header name 的敏感性向 RuntimeRedactor 注册非空展开值；完全静态的敏感字段也注册完整值，并在展开后检查大小。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestEnvironmentExpansion`，期望定义/未定义/转义/敏感注册用例全部通过。

### T026：稳定合并候选规则并分配身份

**文件：** `internal/hook/loader.go`

**依赖：** T018–T025

**步骤：**

1. 先完整解析/结构校验用户文件，再处理项目文件；任一现存文件非法时整体失败，不跳过坏规则。
2. 生成 canonical absolute source、1-based declared ordinal 和连续 effective ordinal，形成供 T084 最终编译的稳定候选序列。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestLoadMergeOrder`，期望用户→项目→文件内声明顺序稳定。

### T027：覆盖发现、缺失和 no-op 加载

**文件：** `internal/hook/loader_test.go`

**依赖：** T018、T026

**步骤：**

1. 用临时 home/project 覆盖两个文件均缺失、单侧存在、空 hooks 和双侧累加。
2. 断言缺失/空规则产生空候选且不访问网络、启动进程或调用 Provider；worker 的 no-op 行为由 T084/T164 验证。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestLoadDiscovery|TestLoadNoop'`，期望全部路径用例通过。

### T028：覆盖严格 YAML 负例

**文件：** `internal/hook/loader_test.go`

**依赖：** T019–T021

**步骤：**

1. 覆盖顶层/规则/if/action 未知字段、重复 key、非标量 key、错误 node type 和缺失字段。
2. 覆盖第二 document、anchor/alias/merge/custom tag、未知 version/event/action/match/scope。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestLoadYAMLStructure`，期望所有输入均以配置错误失败。

### T029：覆盖默认值和 presence 负例

**文件：** `internal/hook/loader_test.go`

**依赖：** T016、T022–T023

**步骤：**

1. 断言未写字段获得默认值，而 `timeout: 0s`、不适用 action 的 `decision: false` 仍报错。
2. 断言 `send_event` 未写与显式 false 被正确区分，Prompt/event/scope/async 矩阵完整覆盖。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestConfigPresence|TestValidateCombinationMatrix'`，期望全部通过。

### T030：实现安全、可定位的加载错误

**文件：** `internal/hook/loader.go`、`internal/hook/loader_test.go`

**依赖：** T008、T026

**步骤：**

1. 错误包含 source path、line、column、字段点路径和 1-based 规则序号。
2. 错误在 stderr 前再次脱敏，禁止回显 command、Prompt、regex、Header/env 值或模拟 secret。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestLoadErrorLocationAndRedaction`，期望定位字段齐全且 canary 不出现。

## C. EventContext、字段、条件和模板

### T031：定义 EventContext 数据模型

**文件：** `internal/hook/event.go`

**依赖：** T017

**步骤：**

1. 定义顶层和 project/execution/session/turn/message/tool/compact 子对象。
2. 对不适用对象使用 nil/omitempty，禁止为缺失阶段填充空对象。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestEventJSONOmission`，期望 JSON 只包含事件适用对象。

### T032：实现 sequence、时间和身份注入

**文件：** `internal/hook/event.go`

**依赖：** T031

**步骤：**

1. 为 Engine 注入 clock/ID source，并以 atomic counter 分配进程内单调 sequence。
2. 输出 RFC3339Nano 时间、绝对 clean project root，并把 Do Mode 映射为 Hook `default`。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestEventSequenceAndClock`，期望并发 sequence 唯一且测试时间确定。

### T033：冻结动态值并生成规范 JSON

**文件：** `internal/hook/event.go`

**依赖：** T031–T032

**步骤：**

1. 深复制 map/slice，保留 `json.Number`，通过私有状态或 copy-on-read 防止 action 修改快照。
2. 每个 dispatch 只生成一次规范 JSON；超过 1 MiB 时保留内存快照并标记 payload 不可用。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestEventSnapshotImmutability`，期望外部变更不污染任何消费者。

### T034：实现 system/session/turn/message builder

**文件：** `internal/hook/event.go`

**依赖：** T033

**步骤：**

1. 构造 system start/stop、session new/resumed/end reason 和 turn 四种终态载荷。
2. `BeginMessage` 生成一次 ID 并冻结 role/content，`EndMessage` 只能复用同一 token。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestLifecycleEventBuilders`，期望 before/after 身份和正文完全一致。

### T035：实现 tool/compact builder

**文件：** `internal/hook/event.go`

**依赖：** T033

**步骤：**

1. 构造 tool before/after，after 只接收已限长脱敏的模型可见结果和纯执行 duration。
2. 构造 compact token，让 after 复用同一份 reason/before stats，并按可用性附加状态、after stats 和安全 error。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestToolEventBuilder|TestCompactEventBuilder'`，期望阶段字段准确。

### T036：锁定 12 类事件对象存在矩阵

**文件：** `internal/hook/event_test.go`

**依赖：** T034–T035

**步骤：**

1. 用 12 行表驱动用例只锁定每个事件应出现/省略的顶层对象。
2. 每行校验 event、sequence 和一个代表性身份字段；终态与 before/after token 细节保持在 T034–T035 的小测试中。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestEventObjectPresenceMatrix`，期望 12 行对象矩阵全部通过。

### T037：覆盖 EventContext 并发和 1 MiB 分流

**文件：** `internal/hook/event_test.go`

**依赖：** T033、T036

**步骤：**

1. 并发构造事件并尝试修改原始/返回 map，断言 sequence 和冻结快照不受影响。
2. 在 1 MiB 与 1 MiB+1 覆盖：条件/点路径仍可读，事件对象明确报告规范 JSON payload 不可用；Runner 拒绝的集成证据由 T160 提供。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run 'TestEvent.*Concurrent|TestEventJSONLimit'`，期望无竞态且分流正确。

### T038：建立事件字段目录

**文件：** `internal/hook/fields.go`

**依赖：** T021、T031

**步骤：**

1. 为 12 个事件列出所有稳定 fixed path 及阶段可用性。
2. 只允许 tool 事件的 `tool.arguments.*` 作为动态扩展，其他未知路径在加载期失败。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestEventFieldCatalog`，期望合法/非法路径与 spec 表一致。

### T039：实现点路径读取和规范数值

**文件：** `internal/hook/fields.go`

**依赖：** T033、T038

**步骤：**

1. 加载期把点路径编译成 segments，运行期只读遍历对象且不支持数组下标。
2. 返回 value/exists/scalarKind；穿过 scalar、缺失、最终 object/array 均按不存在或非标量处理，并规范 decimal。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestFieldLookup`，期望动态参数与缺失路径行为确定。

### T040：覆盖字段目录和 copy-on-read

**文件：** `internal/hook/fields_test.go`

**依赖：** T038–T039

**步骤：**

1. 覆盖每个事件的 fixed path、动态多层 arguments、数组下标和穿越非对象。
2. 修改 lookup 返回的复合值，断言 EventContext 内部快照不变。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run 'TestEventFieldCatalog|TestFieldLookup'`，期望全部通过。

### T041：实现类型化 exact 与逻辑组合

**文件：** `internal/hook/condition.go`

**依赖：** T001、T039

**步骤：**

1. 实现 string/bool/decimal exact，保证数字 `1 == 1.0` 而字符串 `"1" != 1`。
2. 实现 all/any 左到右短路；技术错误返回规则不匹配并交给 Dispatcher 诊断。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestConditionExact|TestConditionGroups'`，期望类型和短路断言通过。

### T042：实现 glob、regex 与 negate

**文件：** `internal/hook/condition.go`

**依赖：** T041

**步骤：**

1. glob 委托共享 matcher；regex 在启动期编译为 `\A(?:pattern)\z`，保持 RE2 完整值语义。
2. 只有字段存在且类型合法后才应用 negate；缺失字段即使 negate=true 也返回 false。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestConditionFullValueMatch`，期望 multiline、尾换行和缺失字段用例通过。

### T043：覆盖条件求值边界

**文件：** `internal/hook/condition_test.go`

**依赖：** T041–T042

**步骤：**

1. 覆盖 all/any、typed exact、case/no-trim、glob、regex、negate 和动态字段。
2. 注入 matcher panic/error，断言当前规则不匹配且同一 dispatch 可继续后续规则。

**验证：** 运行 `go test -count=20 ./internal/hook -run TestCondition`，期望结果确定且无偶发失败。

### T044：严格编译占位符模板

**文件：** `internal/hook/template.go`

**依赖：** T038

**步骤：**

1. 只识别完整 `{{a.b.c}}` token，预编译 literal 与 field segment。
2. 拒绝函数、管道、循环、条件、默认值、畸形括号和目标事件不可能提供的 fixed path。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCompileTemplate`，期望严格语法正反例通过。

### T045：原子渲染标量模板

**文件：** `internal/hook/template.go`

**依赖：** T039、T044

**步骤：**

1. 将 string/bool/number 以固定格式写入临时缓冲区，完成后统一脱敏并检查动作上限。
2. 缺失、nil、object、array 或超限时整次失败，不返回或提交部分内容。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestRenderTemplate`，期望标量格式稳定且失败原子。

### T046：覆盖模板确定性和原子失败

**文件：** `internal/hook/template_test.go`

**依赖：** T044–T045

**步骤：**

1. 覆盖重复 token、相邻 literal、bool/decimal、动态 arguments 和特殊字符原样插入。
2. 覆盖缺失/object/array、64 KiB 模板、128 KiB 片段和 secret canary，断言无部分状态。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestTemplate`，期望全部语法、边界和原子性用例通过。

## D. 诊断、Decision、Command 与 HTTP

### T047：定义 Hook 稳定诊断

**文件：** `internal/hook/diagnostic.go`

**依赖：** T005–T008、T015

**步骤：**

1. 定义 condition/template/command/http/decision/timeout/queue/shutdown/SubAgent/scope/limit/panic 诊断码。
2. 构造器只写 event、rule/effective ordinal、action、stage、duration 和最多 2 KiB 的安全类别摘要，不接收原始 payload。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHookDiagnostic`，期望所有要求码稳定可定位。

### T048：覆盖 Hook 诊断无泄漏边界

**文件：** `internal/hook/diagnostic_test.go`

**依赖：** T047

**步骤：**

1. 为消息、工具参数、Header、URL query、stdout/stderr、HTTP body/response 和结果放置不同 canary。
2. 断言 Collector 与 `Text()` 只含固定属性和脱敏摘要，单条摘要不超过 2 KiB。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHookDiagnostic`，期望所有 canary 都不出现在诊断中。

### T049：实现 token 级 decision 协议解析

**文件：** `internal/hook/decision.go`

**依赖：** T014、T047

**步骤：**

1. 先要求有效 UTF-8，再以 token 级 decoder 记录 key 并拒绝重复/未知 key、非 string value 和额外 JSON/token。
2. 只接受无 reason 的 allow，或带唯一非空 reason 的 deny；前后 JSON whitespace 可存在。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestParseDecisionProtocol`，期望精确协议正反例通过。

### T050：实现 deny reason 安全处理

**文件：** `internal/hook/decision.go`

**依赖：** T008、T049

**步骤：**

1. 在任何清理前拒绝超过 2 KiB 的 reason，不允许截断后成为合法 deny。
2. 移除 ANSI、NUL 和非法终端控制字符后统一脱敏；安全化后为空则 decision 失败。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestDecisionReasonSafety`，期望超限和清空场景 fail-open。

### T051：覆盖 decision 精确负例

**文件：** `internal/hook/decision_test.go`

**依赖：** T049–T050

**步骤：**

1. 覆盖无效 UTF-8、重复/额外 key、非 string、额外 object/正文、未知值、allow+reason、deny 缺 reason。
2. 覆盖空白/超长 reason、控制字符清理后为空和 secret canary；断言错误不复制输入。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestDecision`，期望仅两个规范形态被接受。

### T052：构造 Command 受控环境

**文件：** `internal/hook/command.go`

**依赖：** T025

**步骤：**

1. 只从父环境复制 PATH、HOME、TMPDIR、LANG、LC_ALL、LC_CTYPE、TERM。
2. 校验 POSIX env key/NUL、64 项、128 B key 和展开后 8 KiB value；应用静态 env 后强制 `PWD=project root`，拒绝覆盖 PWD。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandEnvironment`，期望未允许的父环境不会进入进程。

### T053：启动 Command 并写入同一事件 JSON

**文件：** `internal/hook/command.go`

**依赖：** T033、T052

**步骤：**

1. 只接收启动编译期已确认不超过 16 KiB 的 static command，在绝对 project root 使用 `/bin/sh -c`，不经 Tool Executor/permission/Hook Dispatcher。
2. 把同一份有界规范 EventContext JSON 写入 stdin；payload 不可用时在启动进程前失败，并在其他终态关闭 stdin。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandCWDAndStdin`，期望 cwd、stdin 与 EventContext 快照一致。

### T054：实现 Command 输出 hard limit

**文件：** `internal/hook/command.go`

**依赖：** T053

**步骤：**

1. 从进程启动起并行 drain stdout/stderr，各使用 32 KiB hard-limit writer。
2. 任一超限立即取消整个动作并等待收束；始终分离两条流，只有完整 stdout 可进入 decision，stderr 只作安全诊断。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandOutputHardLimit`，期望 limit 成功、limit+1 整体失败。

### T055：建立 Command 取消平台 shim

**文件：** `internal/hook/command.go`、`internal/hook/command_unix.go`、`internal/hook/command_other.go`

**依赖：** T054

**步骤：**

1. 在公共 Command runner 定义最小 setup/terminate/join 平台接口，用 build tag 分离 Unix 与其他平台。
2. 先提供两侧可编译 shim，非 Unix 文件不引用任何 Unix syscall。

**验证：** 运行 `GOOS=windows GOARCH=amd64 go test -c ./internal/hook -o /private/tmp/xagent-hook.test.exe`，期望非 Unix shim 独立编译成功。

### T056：实现 Unix process-group 收束

**文件：** `internal/hook/command_unix.go`、`internal/hook/command_test.go`

**依赖：** T055

**步骤：**

1. Unix 启动时创建独立 process group，取消时只对该 group 发 TERM。
2. 可注入 grace 到期后发 KILL，最后 join Wait 和 pipe goroutine；无取消路径不发任何 signal。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandProcessGroupSignals`，期望 TERM→KILL→join 顺序精确。

### T057：验证 Command 取消收束

**文件：** `internal/hook/command_test.go`

**依赖：** T053–T056

**步骤：**

1. 用三行表分别触发 rule timeout、caller cancel 和 Engine shutdown，断言三者都映射到同一 terminate 原语。
2. 对其中一行加入孙进程与 channel barrier，断言 Wait/pipe 完全 join 且无残留 goroutine/process；输出边界仍由 T054 验证。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run 'TestCommand.*Cancel|TestCommand.*ProcessGroup'`，期望无竞态和泄漏。

### T058：编译 HTTP 静态 URL 与 method

**文件：** `internal/hook/http.go`

**依赖：** T022、T025

**步骤：**

1. 校验静态绝对 URL、2 KiB 上限和 GET/POST/PUT/PATCH/DELETE；拒绝 fragment、userinfo、opaque、空 host 和控制字符。
2. 只允许 HTTPS，或 host 精确为 localhost/loopback literal 的 HTTP；加载期不解析 DNS，不接受环境/事件模板 URL。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHTTPURLValidation`，期望协议和解析歧义负例全部失败。

### T059：校验 HTTP Header

**文件：** `internal/hook/http.go`

**依赖：** T025、T058

**步骤：**

1. 校验 token name、field value、CR/LF/NUL、大小写不敏感重复、32 项、128 B name 及展开后 8 KiB value。
2. 拒绝 Host、Content-Length，以及 `send_event=true` 时用户配置 Content-Type；只对 Header value 做环境展开。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHTTPHeaderValidation`，期望所有 header 边界和 secret 注册用例通过。

### T060：实现禁代理的 loopback resolver/dialer

**文件：** `internal/hook/http.go`

**依赖：** T058–T059

**步骤：**

1. 构造禁用环境 proxy 的专用 client，并为 resolver/dialer 提供测试注入 seam。
2. localhost 运行时要求全部候选地址为 loopback，且只拨经过验证的 IP，防止混合解析与 DNS rebinding。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHTTPLoopbackDialer`，期望纯 loopback 成功、任一非 loopback 候选整体失败。

### T061：实现受控 HTTP redirect

**文件：** `internal/hook/http.go`

**依赖：** T060

**步骤：**

1. 自管最多 3 次重定向，每跳重新校验协议、loopback、same-origin 和 HTTPS 不降级。
2. 对 301/302/303/307/308 都保持原 method/body，并从有界 `GetBody` 重建请求。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHTTPRedirectPolicy`，期望第 3 跳成功、第 4 跳及跨 origin/降级失败。

### T062：实现 HTTP body、状态和响应限制

**文件：** `internal/hook/http.go`

**依赖：** T033、T049、T061

**步骤：**

1. `send_event=true` 发送同一规范 JSON 与唯一 Content-Type，payload 不可用时在 transport 前失败；false 不构造事件 body，request body 不超过 1 MiB。
2. 限制 response header 64 KiB、解压后 body 64 KiB；非 2xx、网络、timeout、redirect 或超限均失败，decision 只读完整成功 body。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHTTPRequestAndResponseLimits`，期望 limit 通过、limit+1 fail-open。

### T063：覆盖 HTTP 请求构造

**文件：** `internal/hook/http_test.go`

**依赖：** T058–T062

**步骤：**

1. 用 `httptest` 覆盖 method、Header、默认/false send_event、JSON body、Content-Type 和默认/显式 timeout。
2. 断言 URL/Header 保持静态，加载期不调用 resolver，环境 proxy 不接收 loopback 请求。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestHTTPURL|TestHTTPHeader|TestHTTPRequest'`，期望全部通过；环境禁止 loopback 监听时使用获批的测试权限重跑。

### T064：锁定 HTTP redirect 安全矩阵

**文件：** `internal/hook/http_test.go`

**依赖：** T060–T063

**步骤：**

1. 用表驱动用例锁定 301/302/303/307/308 的 same-origin method/body 保留。
2. 锁定跨 origin、HTTPS 降级和第 4 跳失败；resolver/DNS rebinding 边界专属 T060。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHTTPRedirectPolicy`，期望合法跳转保持请求，所有越界跳转失败。

### T065：覆盖 HTTP 失败、decision 与 hard limit

**文件：** `internal/hook/http_test.go`

**依赖：** T049–T051、T062

**步骤：**

1. 覆盖非 2xx、网络错误、timeout、header/body limit+1、压缩响应和非法 decision。
2. 在认证 Header、body、response 和 URL query 放 canary，断言诊断和模型结果不泄漏原值。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestHTTP.*Failure|TestHTTP.*Decision|TestHTTP.*Limit'`，期望失败只产安全诊断并继续主流程。

### T066：锁定 Action 超限的统一结算

**文件：** `internal/hook/limits_test.go`

**依赖：** T051、T054、T062

**步骤：**

1. 用共享 ActionOutcome 表驱动测试只比较 Command/HTTP/decision 在代表性 limit+1 时的 failure 形状。
2. 断言三者都整体 fail-open，且影响 deny 的内容绝不截断后判定；每个字节上限的 limit/+1 证据留在 T037/T046/T051/T054/T062/T065。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCrossActionLimitOutcome`，期望 Command/HTTP/decision 三个代表性 outcome 完全一致。

## E. PromptState、once、异步和 Engine

### T067：定义 Prompt entry、owner 与 scope binding

**文件：** `internal/hook/prompt_state.go`

**依赖：** T017、T045

**步骤：**

1. 定义保存 scope、session/execution/turn owner、sequence、effective ordinal、source 和 content 的不可变 entry。
2. 按 F38 解析 active binding；next 优先绑定 execution，无 execution 时绑定 session，turn/session 缺 owner 时失败。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestPromptScopeBinding`，期望每种 scope 的 owner 选择准确。

### T068：实现 Prompt 原子预算与稳定追加

**文件：** `internal/hook/prompt_state.go`

**依赖：** T067

**步骤：**

1. 在同一锁内检查 128 KiB fragment、256 KiB execution/session owner 和当前请求跨 owner 可见聚合预算。
2. 预算通过后一次性追加，按 `(sequence, effective_rule_ordinal)` 排序并为每条规则保留独立来源边界。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestPromptOrdering|TestPromptBudgets'`，期望并发追加也得到唯一顺序且失败不部分提交。

### T069：实现 Prompt lease 获取

**文件：** `internal/hook/prompt_state.go`

**依赖：** T068

**步骤：**

1. `AcquirePrompts` 原子选择 session、turn、execution-next 和 session-next 可见条目，并返回内容副本。
2. 把本次 next generation 标为带唯一 lease ID 的 leased；并发 main/isolated 不能取得同一 session-next。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestAcquirePromptLease`，期望一个 generation 只有一个 lease owner。

### T070：实现 lease Commit、Release 与过期 no-op

**文件：** `internal/hook/prompt_state.go`

**依赖：** T069

**步骤：**

1. Commit 消费 leased next，Release 使同 generation 回到 available；Blocks/Commit/Release 全部幂等。
2. EndTurn/SessionEnd 已清 owner 后，迟到 lease/observer 操作只能 no-op，不能重新创建 entry。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestPromptLeaseTerminalOperations`，期望重复与迟到操作均不复活 Prompt。

### T071：实现 send gate 与 generation 线性化

**文件：** `internal/hook/prompt_state.go`

**依赖：** T070

**步骤：**

1. execution-next 阻挡同 execution 后续发送，session-next 阻挡该 session 的 main/isolated 竞争者；等待可被 request context 取消。
2. Acquire 后新增 Prompt 进入下一 generation；pre-send Release 后旧条目与新增条目按 sequence/ordinal 合并给下个 waiter。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestPromptSendGateGeneration`，期望 gate 无旁路且 generation 归属确定。

### T072：覆盖 Prompt scope、预算和清理

**文件：** `internal/hook/prompt_state_test.go`

**依赖：** T067–T071

**步骤：**

1. 覆盖 next/turn/session 可见性、来源 block、排序、fragment/owner/visible aggregate limit/+1。
2. 覆盖 turn/session end、会话切换等价清理和 owner 结束后迟到操作；`/clear` 的 App 行为留到 T141。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestPromptScope|TestPromptBudget|TestPromptCleanup'`，期望全部通过。

### T073：覆盖 Prompt 并发 barrier

**文件：** `internal/hook/prompt_state_test.go`

**依赖：** T071–T072

**步骤：**

1. 用 channel/barrier 覆盖 main/isolated 同抢 session-next、同 execution 后续发送、并发插入和 pre-send rollback。
2. 断言 completion 调度顺序不会改变 entry 全序，且 cancel waiter 不泄漏 gate 或 goroutine。

**验证：** 运行 `go test -race -count=20 ./internal/hook -run TestPromptConcurrent`，期望无重复租借、无竞态和死锁。

### T074：实现 once 状态机

**文件：** `internal/hook/once.go`

**依赖：** T016

**步骤：**

1. 以 canonical source + declared ordinal 为 key，实现 idle→pending→done 原子转换。
2. 成功/合法 allow/deny/SubAgent no-op 提交 done；failure/timeout/cancel/queue reject/panic 释放到 idle。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestOnceTransitions`，期望所有转换符合状态图。

### T075：覆盖 once 并发与重启语义

**文件：** `internal/hook/once_test.go`

**依赖：** T074

**步骤：**

1. barrier 触发同一规则，断言只有一个调用取得 pending，其他调用不等待并跳过。
2. 覆盖各失败释放、成功固定 done 和新 Engine 状态为空，证明不持久化。

**验证：** 运行 `go test -race -count=20 ./internal/hook -run TestOnce`，期望无重复成功和数据竞争。

### T076：冻结异步有界 payload

**文件：** `internal/hook/async.go`

**依赖：** T015、T033、T045

**步骤：**

1. 入队前为 Command/`send_event` HTTP 共享一次最多 1 MiB JSON，为 SubAgent 保存最多 64 KiB 完整渲染 input。
2. 静态 HTTP 不复制事件正文；job 只保存有界 payload 与不可变 rule，不引用 Conversation/Turn 可变对象。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestAsyncPayloadBounds`，期望超限在入队前失败且无大对象复制。

### T077：实现异步 worker 与非阻塞容量

**文件：** `internal/hook/async.go`

**依赖：** T076

**步骤：**

1. 生产默认启动 4 个 worker 和只容纳 64 waiting jobs 的 channel；无异步规则时不启动 worker。
2. enqueue 使用非阻塞 send，队列满立即返回拒绝，不等待 Agent 主流程。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestAsyncCapacity`，期望最多 4 running、64 queued，第 65 个等待任务立即失败。

### T078：从入队时计算 timeout 并结算 once

**文件：** `internal/hook/async.go`

**依赖：** T074、T077

**步骤：**

1. 成功入队时即创建 Engine root + rule timeout deadline，排队时间计入预算。
2. worker 对过期、runner failure、cancel 和 panic 释放 once；成功及 SubAgent no-op 提交 once。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestAsyncDeadlineAndOnce`，期望过期 job 不执行且可再次触发。

### T079：实现无 send-on-closed 的 admission 状态机

**文件：** `internal/hook/async.go`

**依赖：** T078

**步骤：**

1. 用同一 RWMutex/状态机保护 admission 检查、nonblocking send 和 closing 转换。
2. Close 在写锁内阻止新 sender 后关闭 queue；worker 不关闭共享 channel，重复 Close 幂等。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run TestAsyncAdmissionCloseRace`，期望 enqueue-vs-close 无 panic 或丢失结算。

### T080：实现有界异步关闭

**文件：** `internal/hook/async.go`

**依赖：** T079

**步骤：**

1. `system_stop` 同步 dispatch 完成后关 admission，正常 drain 最多 2 秒，再取消 Engine root。
2. 取消 Command/HTTP 后最多再 join 1 秒，记录未完成数量的安全汇总诊断并返回，不无限等待。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestAsyncBoundedShutdown`，期望异步关闭总窗口有界且诊断准确。

### T081：覆盖异步容量、排队和 once

**文件：** `internal/hook/async_test.go`

**依赖：** T076–T080

**步骤：**

1. barrier 覆盖 4 running/64 queued、满队列、入队顺序、排队 timeout、worker panic 和 once 结算。
2. 断言 queue reject 立即返回，turn/session 结束不取消已接受 job，但 Engine root shutdown 会取消。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run 'TestAsyncCapacity|TestAsyncDeadline|TestAsyncOnce'`，期望全部通过。

### T082：覆盖异步 close race 与 goroutine 收束

**文件：** `internal/hook/async_test.go`

**依赖：** T079–T081

**步骤：**

1. 并发 enqueue、重复 Close、runner panic 和取消，禁止使用 `time.Sleep` 判断顺序。
2. 验证 2s drain/1s join 的可注入短窗口、send-on-closed 防护和 worker/goroutine 全部退出。

**验证：** 运行 `go test -race -count=20 ./internal/hook -run 'TestAsync.*Close|TestAsync.*Shutdown'`，期望无竞态、panic、泄漏或死锁。

### T083：实现 SubAgent 占位动作

**文件：** `internal/hook/engine.go`

**依赖：** T045、T047、T077

**步骤：**

1. 启动编译期先检查 agent 64 B 与 input template 64 KiB；运行期原子渲染并再检查完整 agent/input 64 B/64 KiB，随后记录 `hook_subagent_not_implemented`。
2. 不创建 Agent、不产生嵌套 lifecycle 或 decision，动作 adapter 只返回成功 no-op；async/once 结算由 T088/T091 验证。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestSubAgentPlaceholder`，期望只出现占位诊断，Agent/lifecycle/decision recorder 均为零。

### T084：构造不可变 Engine 快照

**文件：** `internal/hook/loader.go`、`internal/hook/validate.go`、`internal/hook/engine.go`

**依赖：** T026、T030、T042、T045、T047、T057、T065、T068、T083

**步骤：**

1. 在 Loader/Validator 最终编译候选：字段目录、regex/template、Command 16 KiB 等静态 action 上限、环境展开、URL/Header、默认 timeout 和 runner 均成功后才发布 snapshot。
2. Engine 接收 snapshot、PromptState、runners、Diagnostics、clock/ID 和 async seam并复制；零规则 snapshot 直接返回 no-op，不创建 async worker。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestCompileRuleSnapshot|TestEngineSnapshot'`，期望非法编译带定位错误、成功 snapshot 不受外部修改，且零规则 worker 计数为零。

### T085：实现同步稳定 dispatch

**文件：** `internal/hook/engine.go`

**依赖：** T084

**步骤：**

1. 按 effective ordinal 扫描，依次过滤 event、condition，再执行同步 action，不按类型重排。
2. 普通失败写诊断并继续；同步 action 完成前不执行下一条同步规则。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestDispatchStableOrder`，期望 user→project 声明顺序可观测。

### T086：接入 once、panic 隔离与 fail-open

**文件：** `internal/hook/engine.go`

**依赖：** T074、T085

**步骤：**

1. condition 匹配后、执行/入队前 reserve once，并按 ActionOutcome 统一 commit/release。
2. 每条同步/异步 action 单独 recover panic；除合法 deny 外都 fail-open 并继续后续规则。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestDispatchFailOpen`，期望 failure/timeout/panic 不改变主流程结果。

### T087：实现工具 allow/deny 短路

**文件：** `internal/hook/engine.go`

**依赖：** T049–T051、T086

**步骤：**

1. 只有同步 `tool_before` Command/HTTP 的合法 decision 可影响返回；allow 继续后续规则。
2. 首个 deny 提交 once 并立即停止剩余 before；非法 decision 只诊断并继续。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestDispatchToolDecision`，期望 allow/deny/failure 三条路径准确。

### T088：统一动作路由与异步入队

**文件：** `internal/hook/engine.go`

**依赖：** T057、T065、T068、T077、T083、T087

**步骤：**

1. 用统一 `ActionOutcome` 路由 Command、HTTP、Prompt 和 SubAgent；Prompt 必须同步原子写入。
2. 异步规则按扫描顺序 nonblocking 入队；拒绝时释放 once 并写 queue-full 诊断。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestDispatchActions|TestDispatchAsync'`，期望各动作结算一致。

### T089：实现 Session/Turn/Message/Tool/Compact Runtime

**文件：** `internal/hook/engine.go`

**依赖：** T034–T035、T067、T088

**步骤：**

1. SessionStart/BeginTurn 先建立 binding 再 dispatch；Begin/EndMessage 与 Before/AfterTool/Compact 使用不可变 token。
2. EndTurn/SessionEnd 先 dispatch 再清理对应 turn/execution-next/session Prompt；技术错误不向调用方返回。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestEngineLifecycleState`，期望事件可见状态和清理顺序正确。

### T090：实现 System 原子生命周期

**文件：** `internal/hook/engine.go`

**依赖：** T080、T089

**步骤：**

1. 实现 `not_started → started → stopped`；只有完整完成一次 system_start 后才允许 system_stop。
2. Shutdown 先同步 dispatch stop（仍可入 async），再执行 T080 关闭；并发/重复 start/stop 只生效一次。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run TestSystemLifecycle`，期望 fallback close 不误发 stop。

### T091：锁定 system_stop 异步 admission 边界

**文件：** `internal/hook/engine_test.go`

**依赖：** T083–T090

**步骤：**

1. 用 channel barrier 卡住 system_stop 异步 action，断言规则在 admission 关闭前已成功入队。
2. 分别驱动 drain 成功与 deadline cancel 两行，断言 once/outcome 都完成结算；排序、fail-open、deny 和其他 lifecycle 已由 T085–T090 锁定。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run TestSystemStopAsyncAdmission`，期望入队、drain 和 cancel 结算都通过。

## F. Turn/Message 统一执行生命周期

### T092：向 Orchestrator 注入 Hook 与 ExecutionRef

**文件：** `internal/orchestrator/chat.go`、`internal/orchestrator/execution_state.go`

**依赖：** T017、T090

**步骤：**

1. `OrchestratorOptions` 接受 nil-safe `hook.Runtime`，旧构造函数自动注入 no-op。
2. `executionState` 从成功创建起持有不可变 ExecutionRef，并显式传给 stream、tool、Skill 和 compact adapter。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestExecutionStateHookRef`，期望旧调用者无需提供 Hook 也可编译运行。

### T093：完善 RunResult 与 Hook 状态映射

**文件：** `internal/orchestrator/run_request.go`

**依赖：** T092

**步骤：**

1. 为 `RunResult` 增加 Err/安全终止信息，并保留 FinalText、Usage、Duration 和 Reason。
2. 固定 completed/max_iterations/canceled/provider-tool-internal error 到四种 Hook turn status 的映射。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestRunResultStatusMapping`，期望每个 stop reason 只有一个状态。

### T094：让内层 Agent Loop 只返回结果

**文件：** `internal/orchestrator/agent_loop.go`

**依赖：** T093

**步骤：**

1. 移除内层各分支直接发送 terminal Done/Error 的职责，统一返回完整 RunResult。
2. 保留 progress、usage、tool 等非 terminal event，所有退出分支都收束到一个结果构造路径。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestAgentLoopReturnsRunResult`，期望每次 run 只产生一个最终结果。

### T095：包围主 Turn 和用户消息

**文件：** `internal/orchestrator/chat.go`

**依赖：** T092–T094

**步骤：**

1. 在输入、mode、Conversation、profile 全部验证后 BeginTurn；验证失败不触发 lifecycle。
2. 用 BeginMessage/append/EndMessage 包围用户写入，再发送 UserSubmitted UI event 和进入 Agent Loop。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestUserLifecycleOrder`，期望顺序为 turn_start→message_before→append→message_after。

### T096：包围完整 assistant 消息

**文件：** `internal/orchestrator/agent_loop.go`

**依赖：** T094–T095

**步骤：**

1. 每次 Provider stream 收束并实际 append assistant text 时，使用同一 message token 分发 before/after。
2. thinking、delta、tool call/result、summary、Memory 和 UI 临时 copy 不触发 Message Hook。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestAssistantMessageLifecycle`，期望只有完整 user/assistant append 产生事件。

### T097：统一 Turn finalizer 与 terminal 顺序

**文件：** `internal/orchestrator/chat.go`、`internal/orchestrator/agent_loop.go`

**依赖：** T093–T096

**步骤：**

1. run wrapper 按 assistant 收尾→EndTurn→terminal UI event→关闭 stream 的顺序完成。
2. EndTurn 使用不随请求取消的有界 lifecycle context，确保 canceled/error 分支仍发送一次 turn_end。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestTurnEndBeforeTerminalEvent`，期望所有退出分支顺序一致且不重复。

### T098：实现活动 run tracker 与 WaitIdle

**文件：** `internal/orchestrator/chat.go`

**依赖：** T097

**步骤：**

1. 为 main run 的统一 wrapper 登记 begin/end；tracker API 本身支持后续 isolated 调用方登记。
2. 实现 context-aware `WaitIdle`，只有 EndTurn 和 wrapper 收尾完成后才解除等待。

**验证：** 运行 `go test -race -count=1 ./internal/orchestrator -run TestWaitIdle`，期望取消/并发 run 都不会提前报告 idle。

### T099：覆盖 Turn/Message 综合事件

**文件：** `internal/orchestrator/hook_test.go`、`internal/orchestrator/chat_test.go`

**依赖：** T095–T098

**步骤：**

1. 覆盖多 Provider iteration 仅一组 turn、四种终态、message token 复用和 turn.error 脱敏。
2. 负断言 delta/thinking/tool records/summary/Memory 不触发 Turn/Message；本地命令分流留到 T141 的 App 测试，无 Hook 路径保持旧 Conversation 行为。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run 'TestHookTurn|TestHookMessage|TestNoHook'`，期望全部通过。

## G. 有序 Prompt、Provider 与 request-attempt 终态握手

### T100：构建统一 ordered Prompt blocks

**文件：** `internal/prompt/section.go`、`internal/prompt/builder.go`

**依赖：** T067

**步骤：**

1. 让 BuildRequest 接受通用 Hook blocks，Bundle 提供明确 ordered blocks API，不导入 `hook` 包。
2. 精确组装 fixed safety→Hook→optional instructions/memory→catalog→active SOP→runtime reminder，Hook 不能替换 fixed block；block name/source 只作本地排序与定位元数据。

**验证：** 运行 `go test -count=1 ./internal/prompt -run TestHookBlockOrder`，期望每个来源边界和顺序可定位。

### T101：锁定 Prompt legacy/ordered 回归

**文件：** `internal/prompt/prompt_test.go`

**依赖：** T100

**步骤：**

1. 对有 Hook blocks 断言精确顺序、non-cacheable 和每条 entry 独立边界。
2. 对无 Hook blocks 断言原 Stable/Dynamic 内容与顺序逐字不变。

**验证：** 运行 `go test -count=1 ./internal/prompt -run 'TestHookBlockOrder|TestLegacyPromptUnchanged'`，期望两条路径互不影响。

### T102：扩展 Provider 请求兼容 API

**文件：** `internal/provider/provider.go`

**依赖：** T100

**步骤：**

1. `ChatRequest` 增加首选 `System []SystemBlock`，保留现有 StableSystem、DynamicSystem、SystemPrompt fallback 优先级。
2. CachePolicy 增加 ordered path 的 SystemBreakpointName 与 CacheTools；默认旧路径语义不变。

**验证：** 运行 `go test -count=1 ./internal/provider -run TestSystemBlockSelection`，期望 ordered 与三层 legacy fallback 选择正确。

### T103：实现 OpenAI ordered system messages

**文件：** `internal/provider/openai.go`、`internal/provider/tool_parse_test.go`

**依赖：** T102

**步骤：**

1. ordered path 为每个 SystemBlock 生成一条按序 system message，不合并 Hook entry。
2. wire 只序列化 role/content，绝不序列化 `SystemBlock.Name`、Hook source 或用户/项目绝对路径；legacy path 和普通消息不变。

**验证：** 运行 `go test -count=1 ./internal/provider -run TestOpenAIOrderedSystemBlocks`，期望 wire payload 精确匹配且 source/name/path canary 全部不存在。

### T104：实现 Anthropic ordered blocks 与 cache boundary

**文件：** `internal/provider/anthropic.go`、`internal/provider/anthropic_schema_test.go`

**依赖：** T102

**步骤：**

1. ordered path 保留每个 content block，只在指定的最后一个 fixed block 设置 system cache breakpoint。
2. Hook path 设置 `CacheTools=false`，且 wire block 只含 content/cache 字段，不含本地 Name/source/绝对路径；legacy 默认不变。

**验证：** 运行 `go test -count=1 ./internal/provider -run TestAnthropicHookCacheBoundary`，期望只有目标 fixed block 可缓存，且 source/name/path canary 不在 request JSON。

### T105：定义 request-local attempt observer 契约

**文件：** `internal/provider/provider.go`

**依赖：** T070、T102

**步骤：**

1. 增加 nil-safe `RequestObserver`，包含 `MarkSent()` 与 `Finish(sent bool)`，并放入 ChatRequest。
2. 文档化强契约：每个 attempt 恰好一次 Finish；只有 transport/callback 完全静止后才可 Finish(false)，之后禁止迟到 MarkSent。

**验证：** 运行 `go test -count=1 ./internal/provider -run TestRequestObserverContract`，期望 nil observer 安全且重复/非法时序可由测试捕获。

### T106：接入 OpenAI attempt 终态

**文件：** `internal/provider/openai.go`、`internal/provider/openai_hook_test.go`

**依赖：** T105

**步骤：**

1. 用 request context/httptrace 在不可逆写出边界锁存 sent；同步构造/Do/HTTP status/流错误/取消各路径都结算。
2. 保证 Finish 恰好一次；写出后错误/取消使用 sent=true，pre-send 失败等待 transport callback 静止后 Finish(false)。

**验证：** 运行 `go test -count=1 ./internal/provider -run TestOpenAIRequestAttempt`，期望 pre-send/post-write 分支终态准确。

### T107：接入 Anthropic 异步 attempt 终态

**文件：** `internal/provider/anthropic.go`、`internal/provider/anthropic_hook_test.go`

**依赖：** T105

**步骤：**

1. 把 observer 接到 SDK 实际 transport attempt，使用单调 sent latch 覆盖 goroutine 内请求。
2. 在所有 return/error/cancel 分支先 Finish，再发送 terminal stream event 和关闭 output channel。

**验证：** 运行 `go test -count=1 ./internal/provider -run TestAnthropicRequestAttempt`，期望 StreamChat 提前返回 channel 不会被误判为已发送或未发送。

### T108：锁定 Provider 取消与写出 barrier

**文件：** `internal/provider/request_observer_test.go`

**依赖：** T106–T107

**步骤：**

1. 用本地 RoundTripper/httptrace 的一个 channel barrier 交错 cancel 与实际 write。
2. 断言 Finish(false) 前 callback 已完全静止、之后绝无 MarkSent，且 Finish 先于 terminal close；各返回路径的 exactly-once 属于 T106–T107。

**验证：** 运行 `go test -race -count=20 ./internal/provider -run TestRequestObserverBarrier`，期望无迟到 callback 或重复终态。

### T109：实现 PromptLease observer adapter

**文件：** `internal/orchestrator/chat.go`

**依赖：** T070–T071、T105

**步骤：**

1. MarkSent 原子锁存并 Commit；Finish(true) 幂等 Commit；Finish(false) 仅在从未观察 sent 时 Release。
2. 请求交给 Provider 后，caller cleanup、stream error/close 和 context cancel 都不得自行 Release；仅本地交付前错误可直接回滚。

**验证：** 运行 `go test -race -count=1 ./internal/orchestrator -run TestPromptLeaseObserver`，期望竞态下 lease 只完成一次。

### T110：在线性化边界获取 Prompt

**文件：** `internal/orchestrator/chat.go`

**依赖：** T100、T109

**步骤：**

1. 在 Session/Context prepare 和其他请求构造完成后、Provider 调用前 AcquirePrompts。
2. 有 Hook blocks 才走 ordered path并设置 fixed breakpoint/CacheTools=false；无 Hook 继续逐字使用 legacy path。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestAcquirePromptsBeforeProvider`，期望 compact_after Prompt 能进入紧接请求且 summary/Memory 不取得 lease。

### T111：覆盖 next Prompt 发送/取消竞争

**文件：** `internal/orchestrator/hook_test.go`、`internal/provider/request_observer_test.go`

**依赖：** T103–T110

**步骤：**

1. barrier 覆盖 pre-send Finish(false) 释放后 waiter 获得 next，以及 post-write commit 后 waiter 不再获得已消费 next。
2. 覆盖 Anthropic 异步发送、cancel/WroteRequest 交错、并发 session main/isolated 和 Acquire 后新增 generation。
3. 驱动同一 Turn 多次 Provider iteration/context rebuild，断言 turn/session Prompt 每次出现、next 只出现一次；turn_end 丢 execution-next，session_end/切换丢 session-next。

**验证：** 运行 `go test -race -count=20 ./internal/provider ./internal/orchestrator -run 'Test.*RequestAttempt|Test.*NextPrompt'`，期望无重复注入、gate 泄漏或死锁。

## H. Permission、公共工具安全链与 `load_skill`

### T112：让 Permission 接收已解析参数

**文件：** `internal/permission/authorizer.go`

**依赖：** T003、T010

**步骤：**

1. 增加接收 `permission.Call + map[string]any + Context` 的 NormalizeArguments，不重新解析 JSON。
2. 保持 `permission` 只依赖 matcher 等基础包，禁止导入 `tool`，避免现有 `tool → permission` 形成环。

**验证：** 运行 `go test -count=1 ./internal/permission -run TestNormalizeArgumentsMap`，期望 json.Number 和规范化结果无损。

### T113：提取不可绕过 CheckHard

**文件：** `internal/permission/authorizer.go`

**依赖：** T112

**步骤：**

1. 把配置损坏保护、权限文件保护、路径 sandbox 和 Bash blacklist 提取为 `CheckHard`。
2. 从该层移除 Plan 和普通规则/ask；hard deny 只接收已规范化调用。

**验证：** 运行 `go test -count=1 ./internal/permission -run TestCheckHard`，期望每个不可绕过约束独立拒绝。

### T114：提取普通权限决策并保留旧入口

**文件：** `internal/permission/authorizer.go`

**依赖：** T113

**步骤：**

1. 提取 `DecideOrdinary`，保留 layer 匹配、mode 默认、ask 和 Grant 生成。
2. 让旧 `Decide` 按 Normalize→兼容 Plan→CheckHard→ordinary 委托，旧调用结果不变。

**验证：** 运行 `go test -count=1 ./internal/permission -run TestDecideCompatibility`，期望既有表驱动结果逐项一致。

### T115：复用 NormalizedCall 处理用户确认

**文件：** `internal/permission/authorizer.go`

**依赖：** T114

**步骤：**

1. 增加基于既有 NormalizedCall 的 ResolveUserDecision 入口。
2. allow once/session/permanent 与 deny/cancel 都不重新解析参数或改变 fingerprint；旧入口仅作兼容委托。

**验证：** 运行 `go test -count=1 ./internal/permission -run TestResolveNormalizedUserDecision`，期望确认前后指纹和类型不变。

### T116：锁定 Permission 新旧入口等价

**文件：** `internal/permission/permission_test.go`

**依赖：** T112–T115

**步骤：**

1. 对同一组代表性 deny/ask/allow 调用，比较旧 Decide 与分阶段组合的 decision、reason 和 Grant。
2. 加一个大数参数确认 fingerprint 不变；config/Plan/hard/ordinary 各自的边界保持在 T112–T115。

**验证：** 运行 `go test -count=1 ./internal/permission -run TestDecideStagedCompatibility`，期望新旧入口对照逐行相同。

### T117：构建 preflight Registry view

**文件：** `internal/orchestrator/execution_state.go`、`internal/orchestrator/tool_batches.go`

**依赖：** T010、T092、T115

**步骤：**

1. 为当前 profile 构造只应用 Skill allowlist、并保留系统工具的 preflight view，不提前应用 Plan read-only 过滤。
2. 本任务只产出 ValidatedCall；不得在 Plan policy 前调用 permission Normalize。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestPrepareRegisteredCall`，期望未知/Skill 隐藏与 Plan 拒绝可区分。

### T118：固定 Tool 前置安全链顺序

**文件：** `internal/orchestrator/tool_batches.go`

**依赖：** T113、T117

**步骤：**

1. 固定执行 registry/profile→Plan policy→Normalize/CheckHard→Hook BeforeTool；Plan allow 后才形成携带 ValidatedCall 与 NormalizedCall 的 prepared execution。
2. unknown、Skill hidden、Plan deny、hard deny 都在 Hook 前停止且不触发 tool_after；Plan policy 保留 `load_skill` 既有系统工具例外，并断言 Plan deny 时 Normalize recorder 未调用。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestToolSafetyChainOrder`，期望 recorder 顺序严格一致。

### T119：接入 Hook deny 回流

**文件：** `internal/orchestrator/tool_batches.go`

**依赖：** T014、T087、T118

**步骤：**

1. Hook allow 继续；首个 deny 不进入普通权限、不弹确认、不执行，也不触发 after。
2. 构造 `StatusDenied`、`hook_denied`、`recoverable=true` 和安全 reason 的工具结果，但不写任何权限规则。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestHookDenyShortCircuit`，期望拒绝结果可沿 tool history 反馈模型。

### T120：复用同一调用完成普通权限与执行

**文件：** `internal/orchestrator/tool_batches.go`、`internal/tool/executor.go`

**依赖：** T012、T115、T118–T119

**步骤：**

1. ordinary permission 与用户确认始终复用 prepared NormalizedCall，不再解析原始 JSON。
2. deny/cancel 停在 handler 之前且不触发 after；allow 才把同一 ValidatedCall 和 Grant 交给 `ExecuteValidatedAuthorized`，Hook allow 不能伪造 Grant。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestValidatedCallFlowsEndToEnd`，期望 Hook、权限、fingerprint 与 Tool 收到同一类型和值。

### T121：在实际执行后分发 ToolAfter

**文件：** `internal/orchestrator/tool_batches.go`

**依赖：** T120

**步骤：**

1. duration 只包围 executor/system handler，不包含 preflight、Hook 或用户确认等待。
2. handler 返回后先执行既有限长和 RuntimeRedactor，再映射 success/error/timeout 并同步 AfterTool，最后写 UI/Conversation。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestToolAfterPayload`，期望 payload 安全且时间边界准确。

### T122：把 `load_skill` 并入公共 gate

**文件：** `internal/orchestrator/skill_runtime.go`、`internal/orchestrator/tool_batches.go`

**依赖：** T117–T121

**步骤：**

1. 从 ValidatedCall.Arguments 做严格 name/args/未知 key 校验，移除第三次 JSON 解析。
2. Hook allow 后才进入 system route；保留普通 permission 豁免，成功、业务参数错误、Skill 语义失败和 timeout 都触发 after。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestLoadSkillUsesHookGate`，期望 Hook deny 不激活 Skill，handler 错误有 after。

### T123：锁定三类 Tool 正向 before/after 矩阵

**文件：** `internal/orchestrator/hook_test.go`

**依赖：** T118–T122

**步骤：**

1. 用 built-in、MCP、`load_skill` 三行表断言进入公共 gate 后都恰好一次 before。
2. 每行分别返回 success/error/timeout，断言都有一次对应 after；六类 pre-handler 拒绝的无 after 证据已分别落在 T118–T120。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestHookToolPositiveMatrix`，期望 3×3 正向矩阵的 before/after 计数精确。

### T124：覆盖 Hook 决策与普通权限隔离

**文件：** `internal/orchestrator/hook_test.go`、`internal/orchestrator/skill_runtime_test.go`、`internal/permission/permission_test.go`

**依赖：** T119–T123

**步骤：**

1. 用 recorder 分别让 Hook allow、技术失败和 no-match，断言随后 ordinary deny/ask 与用户确认仍生效；只有 Hook deny 跳过确认。
2. 断言所有路径都不写 session/local/project/user 权限，不扩大 Skill allowlist；Plan 下 `load_skill` 仍通过 Plan 后触发 Hook，Hook deny 时不激活。

**验证：** 运行 `go test -count=1 ./internal/orchestrator ./internal/permission -run 'TestHookDenyFeedback|TestHookPermissionIsolation|TestLoadSkill'`，期望全部通过。

## I. Context compression

### T125：定义 request-local 压缩观察 API

**文件：** `internal/contextmgr/manager.go`

**依赖：** T031

**步骤：**

1. 在 contextmgr 内定义不导入 hook 的 CompactionObserver、Attempt、Result、PrepareOptions。
2. 增加 `PrepareWithOptions`，并让现有 Prepare/CompactNow 保持兼容 wrapper。

**验证：** 运行 `go test -count=1 ./internal/contextmgr -run TestPrepareCompatibilityWrapper`，期望旧入口行为不变。

### T126：实现非变异 compact preflight

**文件：** `internal/contextmgr/manager.go`

**依赖：** T125

**步骤：**

1. 用不可变消息视图计算消息数、字符/token 估计、外置候选、摘要阈值和熔断状态。
2. preflight 不调用会 `EnsureContext` 的旧估算路径，不写 metadata、blob 或 Conversation。

**验证：** 运行 `go test -count=1 ./internal/contextmgr -run TestCompactionPreflightDoesNotMutate`，期望调用前后 Conversation 深度相等。

### T127：只包围真实压缩 attempt

**文件：** `internal/contextmgr/manager.go`

**依赖：** T126

**步骤：**

1. auto 仅在将外置结果或尝试摘要时调用 Before；manual 即使 Changed=false 也算真实 attempt。
2. mutation 完成后调用 After，并用 opaque token 复用相同 before stats、reason，按可用性补 after stats。

**验证：** 运行 `go test -count=1 ./internal/contextmgr -run TestCompactionObserverBoundaries`，期望非真实 auto 不触发，manual 总触发。

### T128：报告 attemptErr 并支持 transient prepare

**文件：** `internal/contextmgr/manager.go`

**依赖：** T127

**步骤：**

1. 单独维护 attemptErr，使 auto summary 被现有返回策略吞掉时 After 仍报告 error。
2. `PersistArtifacts=false` 禁止外置 blob/保存临时 Conversation，但允许阈值触发内存摘要；observer failure 始终 fail-open。

**验证：** 运行 `go test -count=1 ./internal/contextmgr -run 'TestCompactionAttemptError|TestTransientPrepare'`，期望错误可见且无孤儿文件。

### T129：透传 Session PrepareOptions

**文件：** `internal/sessionctx/manager.go`、`internal/sessionctx/manager_test.go`

**依赖：** T125–T128

**步骤：**

1. 增加 PrepareWithOptions 透传；main auto/manual 使用 persist=true，isolated transient 使用 false。
2. 保留 instructions、memory 和 restore boundary 加载；PrepareStable 兼容行为不变。

**验证：** 运行 `go test -count=1 ./internal/sessionctx -run TestPrepareWithOptions`，期望三种模式传参和产物准确。

### T130：接入 compact Hook adapter

**文件：** `internal/orchestrator/chat.go`、`internal/contextmgr/hook_test.go`

**依赖：** T092、T111、T129

**步骤：**

1. 用 request-local 通用 adapter 把 main ExecutionRef 转成 Hook Compact binding；`/compact` 只带 active session/manual，不伪造 turn，isolated ref 接线留到 T134。
2. 保证 context prepare 完成后才 AcquirePrompts，使 compact_after Prompt 可进入紧接请求；summary Provider 不取 lease或触发 Message/Turn。

**验证：** 运行 `go test -count=1 ./internal/contextmgr ./internal/orchestrator -run TestHookCompact`，期望统计、身份、顺序和摘要隔离正确。

### T131：覆盖 compact 错误与 Prompt scope

**文件：** `internal/contextmgr/hook_test.go`、`internal/orchestrator/hook_test.go`

**依赖：** T127–T130

**步骤：**

1. 覆盖 success/error、after stats 可选、同一 before token、非真实 auto、本地 manual 和 observer failure。
2. `/compact` Turn 外命中 turn Prompt 时只记录 scope-unavailable，压缩继续；next/session Prompt 按 owner 生效。

**验证：** 运行 `go test -count=1 ./internal/contextmgr ./internal/orchestrator -run 'TestHookCompact|TestCompactPromptScope'`，期望全部 fail-open 边界通过。

## J. 独立 Skill 生命周期

### T132：为 isolated run 创建独立 execution/turn

**文件：** `internal/orchestrator/independent.go`

**依赖：** T098、T122

**步骤：**

1. `RunIndependent` 创建 `execution.kind=isolated_skill` 的 ExecutionRef/turn，复用主 session ID但不发送 session start/end。
2. direct 与 agent-triggered isolated 都使用统一 run wrapper，显式向 T098 的 tracker 登记 begin/end，并只发送一次 EndTurn。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestIndependentHookTurn`，期望 child turn 与 parent turn 身份独立。

### T133：消除 isolated Message 重复

**文件：** `internal/orchestrator/independent.go`

**依赖：** T096、T132

**步骤：**

1. 临时 Conversation 中驱动 Agent 的 user 和最终 assistant 作为唯一权威逻辑 Message Hook。
2. 主 Conversation raw command/final summary bridge copy 标为 internal copy，不再发送第二组 message before/after。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestIndependentHookMessagesNotDuplicated`，期望每个逻辑消息恰好一组事件。

### T134：接入 isolated Tool、Compact 与 Prompt 状态

**文件：** `internal/orchestrator/independent.go`

**依赖：** T111、T124、T130、T133

**步骤：**

1. tool/compact 显式传 isolated ref；临时 context 走 `PersistArtifacts=false`，不产生额外 session。
2. 与 main 共享 rule/diagnostics/async/process once/session Prompt，严格隔离 execution-next/turn Prompt 和父子 turn status。

**验证：** 运行 `go test -race -count=1 ./internal/orchestrator -run TestIndependentHookStateIsolation`，期望共享项共享、隔离项无串扰。

### T135：覆盖独立 Skill Hook 综合场景

**文件：** `internal/orchestrator/independent_hook_test.go`、`internal/orchestrator/independent_test.go`

**依赖：** T132–T134

**步骤：**

1. 覆盖无额外 session、kind、父子 turn 嵌套、消息去重、tool/compact 和取消终态。
2. barrier 覆盖共享 once/session-next 与 execution/turn 隔离，并确认 direct isolated 也被 WaitIdle 跟踪。

**验证：** 运行 `go test -race -count=1 ./internal/orchestrator -run TestIndependentHook`，期望 AC19–AC20 的隔离断言全部通过。

## K. App、TUI 与启动关闭

### T136：注入 Hook 并建立共享 lifecycleState

**文件：** `internal/app/deps.go`、`internal/app/app.go`、`internal/app/skills.go`

**依赖：** T090、T098

**步骤：**

1. Deps 注入 nil-safe Hooks，并传给唯一 Orchestrator；不传时使用 no-op。
2. 把 close-once、active/canceling request、run 等待状态放入所有 Bubble Tea Model 值副本共享的 `*lifecycleState`；`beginRequest` 同步登记。

**验证：** 运行 `go test -count=1 ./internal/app -run TestLifecycleStateSharedAcrossCopies`，期望复制 Model 后仍共享同一生命周期。

### T137：接入 SystemStart 与初始 SessionStart

**文件：** `internal/app/app.go`

**依赖：** T136

**步骤：**

1. App/Orchestrator/命令/状态依赖装配完成后调用 SystemStart。
2. 初始 Conversation 新建或成功恢复后调用 SessionStart(new/resumed)，失败候选不改变 active session。

**验证：** 运行 `go test -count=1 ./internal/app -run TestSystemAndInitialSessionStart`，期望 system 永远先于 session 且状态准确。

### T138：实现事务式 Session 切换

**文件：** `internal/app/app.go`

**依赖：** T137

**步骤：**

1. 抽取单一 transition helper：先 load/create candidate；失败时旧 Conversation、Skill、mode、Prompt 完全不动。
2. 成功时 WaitIdle→final save old→SessionEnd(switch)→清旧状态→activate candidate→SessionStart(new/resumed)；save 失败仍继续 end 并报告。

**验证：** 运行 `go test -count=1 ./internal/app -run TestSessionSwitchTransaction`，期望成功和失败路径均满足顺序。

### T139：让取消等待真实 Turn 收尾

**文件：** `internal/app/update.go`、`internal/app/app.go`、`internal/app/skills.go`

**依赖：** T098、T136

**步骤：**

1. cancelRequest 只发送 cancel、设置 canceling 并保持 request/输入禁用，不立即清 streaming、transient 或确认状态。
2. `listen` channel 无 terminal event 时返回显式 `eventStreamClosedMsg`；收到 terminal/closed 且 run tracker 已收尾后才清 request。

**验证：** 运行 `go test -count=1 ./internal/app -run TestCancelWaitsForTurnEnd`，期望第二次退出不会早于 canceled turn_end。

### T140：实现幂等 App Session 收尾

**文件：** `internal/app/app.go`

**依赖：** T138–T139

**步骤：**

1. App Close 只负责取消并 WaitIdle、final save active Conversation、SessionEnd(exit) 和清 Skill/mode/session 状态。
2. App 不关闭 Hook 或 MCP；重复/并发调用只执行一次 session 收尾，进程级资源唯一由 T144 的 main coordinator 关闭。

**验证：** 运行 `go test -count=1 ./internal/app -run TestAppCloseOrder`，期望 turn_end→save→session_end→状态清理，且 Hook/MCP close recorder 始终为零。

### T141：锁定 App 本地命令与诊断隔离

**文件：** `internal/app/hook_test.go`、`internal/app/commands_test.go`

**依赖：** T111、T131、T137–T140

**步骤：**

1. 用 `/clear`、`/plan`、`/do`、`/compact`、`/diagnostics` 五行表锁定是否产生 turn/message/compact，并断言 `/clear` 不清 Hook Prompt。
2. 表中制造一次实际 Hook failure，断言它只能经 `/diagnostics` 读取，不进入普通 TUI message、Conversation 或下一次 Provider request；App lifecycle 由 T137–T140 验证。

**验证：** 运行 `go test -race -count=1 ./internal/app -run 'TestLocalCommandHookExclusion|TestHookDiagnosticIsolation'`，期望命令分流和诊断可见性精确通过。

### T142：让 TUI 返回最终 Model

**文件：** `internal/tui/program.go`、`internal/tui/program_test.go`

**依赖：** T140

**步骤：**

1. 调整 `tui.Run` 返回最终 `tea.Model` 与 error，不在 Update 的退出键路径同步关闭资源。
2. 测试正常与 error 返回都保留最终 Model，使外层 coordinator 可统一 Close。

**验证：** 运行 `go test -count=1 ./internal/tui -run TestRunReturnsFinalModel`，期望调用方总能取得最终状态。

### T143：重构 main 为可测试的早期启动流程

**文件：** `cmd/xagent/main.go`

**依赖：** T030、T090、T142

**步骤：**

1. 抽 `run() error` 和可注入 startup factories/buildRuntime seam，避免中途 `os.Exit` 绕过 defer。
2. 主配置/RuntimeRedactor/project root 后立即严格加载 Hook；成功后才创建 store/provider/registry/MCP/Skill，且必须早于 `mcpManager.Start`。

**验证：** 运行 `go test -count=1 ./cmd/xagent -run TestHookConfigLoadsBeforeDependencies`，期望坏 Hook 时所有 recording factory 副作用计数为零。

### T144：实现幂等组合关闭协调器

**文件：** `cmd/xagent/main.go`

**依赖：** T080、T140、T143

**步骤：**

1. main coordinator 是进程资源的唯一 owner：正常时先调用最终 App Model Close，再依次 Hook Shutdown、MCP Close；defer 用同一幂等对象覆盖 App 未创建或 TUI error。
2. Hook 与 MCP 各使用新的独立 context；TUI 已退出后的安全 shutdown 诊断同时写 Collector 和无 payload 的 stderr 摘要，App 不重复关闭二者。

**验证：** 运行 `go test -count=1 ./cmd/xagent -run TestCompositeCloseOrder`，期望 context 预算互不吞噬且资源只关一次。

### T145：锁定 system_start 前构造失败

**文件：** `cmd/xagent/hook_startup_test.go`

**依赖：** T143–T144

**步骤：**

1. 在 Hook Runtime 构造完成、system_start 之前注入下游 factory 失败。
2. 断言不发 system_stop、已创建资源各关闭一次、失败点后 factory 计数为零；正常与并发 coordinator 由 T144 验证。

**验证：** 运行 `go test -count=1 ./cmd/xagent -run TestFallbackBeforeSystemStart`，期望 stop、close 和 factory recorder 精确匹配。

## L. 文档、端到端与质量门禁

### T146：提交可解析示例并调整忽略规则

**文件：** `.gitignore`、`docs/hook-system/example.yaml`

**依赖：** T030、T090

**步骤：**

1. 精确允许版本控制项目级 `.xagent/hooks.yaml`，不意外放开其他本地状态或秘密文件。
2. 示例覆盖四种 action、all/any 中的合法一种、once/async/timeout、decision 和 Prompt scope，并只使用安全占位值。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestExampleConfig`，期望 example 通过真实 Loader；运行 `test -z "$(git check-ignore --no-index .xagent/hooks.yaml)"` 与 `test -n "$(git check-ignore --no-index .xagent/other-local-state)"`，期望只放行 Hook 文件而非整个目录。

### T147：补充 Hook 用户文档与信任警告

**文件：** `README.md`

**依赖：** T146

**步骤：**

1. 说明两固定路径、用户→项目累加、version/schema、四 action、条件、once/async/timeout、三种 Prompt scope 和修改后重启。
2. 显著说明项目 Hook 是受信代码：可在普通权限确认前执行 Shell/HTTP、HTTP 可外传事件、动态 system Prompt 有注入风险、技术失败时 deny fail-open；打开不可信项目先审查。
3. 披露大量同步规则及 10 分钟单规则 timeout 可能长时间阻塞 dispatch；本版本不提供 aggregate timeout 或热更新。

**验证：** 运行 `rg -n 'hooks.yaml|受信|不可信|重启|fail-open|Shell|HTTP|Prompt' README.md`，期望每个主题有可定位说明。

### T148：自动验证 example 与文档警告

**文件：** `internal/hook/docs_test.go`

**依赖：** T146–T147

**步骤：**

1. 通过真实固定 schema 解析 example，断言四 action 和执行控制均出现。
2. 读取 README 并断言受信、确认前副作用、外传、Prompt injection、fail-open、不可信项目检查和重启警告齐全。

**验证：** 运行 `go test -count=1 ./internal/hook -run 'TestExample|TestHookDocumentation'`，期望文档缺少任何关键警告都会失败。

### T149：锁定无 Hook 的核心线协议等价

**文件：** `internal/provider/openai_hook_test.go`、`internal/provider/anthropic_hook_test.go`、`internal/orchestrator/hook_test.go`

**依赖：** T091、T111、T145

**步骤：**

1. 对无文件和空规则各跑一个 scripted main turn，将 Provider system/cache wire 与 Conversation JSON 逐字节比较 legacy golden。
2. 断言两条路径都选择 legacy Provider API，wire 无 Hook 元数据，Conversation 也不新增任何普通消息。

**验证：** 运行 `go test -count=1 ./internal/provider ./internal/orchestrator -run TestNoHookWireCompatibility`，期望两份 golden 和 Provider path 选择全部不变。

### T150：建立 tagged TUI E2E 驱动器

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T145、T148

**步骤：**

1. 建立 `integration` build tag 的 scripted Provider、fake store/tool 和最小 App input/terminal-event recorder，禁止公网和真实 Provider。
2. 用空 Hook 配置跑一次启动→单轮→关闭 smoke；loopback HTTP helper 留给 T152，并发 barrier 留给 T154。

**验证：** 运行 `go test -tags=integration -count=1 ./internal/app -run TestHookE2EDriverSmoke`，期望驱动器的最小 smoke 通过。

### T151：增加 E2E 生命周期与 Prompt 场景

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T111、T141、T150

**步骤：**

1. 跑一个 main turn，断言 system→session→turn→message 事件顺序与 before/after 身份。
2. 在同一 turn 进入两次 Provider iteration，断言 next 仅一次、turn/session 每次出现，关闭后按 scope 清理。

**验证：** 运行 `go test -tags=integration -count=1 ./internal/app -run TestHookE2ELifecyclePrompt`，期望事件顺序和 Prompt 次数通过。

### T152：增加 E2E Command/HTTP 传输场景

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T066、T150

**步骤：**

1. 为 driver 增加仅绑定 loopback 的 `httptest` helper，各执行一个 Command 和 HTTP action。
2. 断言两者收到同一 EventContext、使用稳定规则顺序，成功输出/响应不进入 Conversation，且 recorder 只有原触发事件、无任何嵌套 Hook dispatch。

**验证：** 运行 `go test -tags=integration -count=1 ./internal/app -run TestHookE2EActionTransport`，期望 Command/HTTP 两条传输路径通过。

### T153：增加 E2E allow/deny 决策场景

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T124、T150

**步骤：**

1. 在同一工具调用前分别返回 allow 与 deny，断言 allow 继续普通权限链。
2. 断言 deny 只以 `hook_denied` 工具结果回流，不弹用户确认、不执行工具、不产生 after。

**验证：** 运行 `go test -tags=integration -count=1 ./internal/app -run TestHookE2EToolDecision`，期望 allow/deny 两条安全链分支通过。

### T154：增加 E2E 异步与 once 场景

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T091、T153

**步骤：**

1. 用 barrier 并发触发 async+once 规则，断言只保留一次副作用。
2. 按 main coordinator 的 owner 顺序先 App.Close 再 Hook.Shutdown，断言已接受动作在受管 drain/cancel 中结算，无 worker 泄漏。

**验证：** 运行 `go test -tags=integration -race -count=1 ./internal/app -run TestHookE2EAsyncOnce`，期望无重复副作用或竞态。

### T155：增加 E2E isolated Skill 场景

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T135、T150

**步骤：**

1. 运行一个 isolated Skill，断言 execution/turn/Prompt 状态与 main 完全隔离。
2. 断言摘要回流只是 main conversation 内部写入，不创建额外 turn/message 生命周期。

**验证：** 运行 `go test -tags=integration -count=1 ./internal/app -run TestHookE2EIsolatedSkill`，期望无状态串扰或重复生命周期。

### T156：锁定 Command 配置字节上限

**文件：** `internal/hook/limits_test.go`

**依赖：** T052、T084

**步骤：**

1. 对 Command 字符串 16 KiB、env 数量 64、env key 128 B 逐项写 limit 与 limit+1。
2. 对环境展开后 env value 8 KiB 写 limit/+1，断言按 UTF-8 字节计算且越界在启动期拒绝。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandConfigLimits`，期望四类边界的 limit 通过、limit+1 失败。

### T157：锁定 HTTP Header 配置上限

**文件：** `internal/hook/limits_test.go`

**依赖：** T059、T084

**步骤：**

1. 对 Header 数量 32 和 name 128 B 各写 limit 与 limit+1。
2. 对环境展开后 value 8 KiB 写 limit/+1，断言大小写重复检查先于发布不可变配置。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestHTTPHeaderConfigLimits`，期望三类边界的 limit 通过、limit+1 失败。

### T158：锁定 SubAgent 模板与渲染上限

**文件：** `internal/hook/limits_test.go`

**依赖：** T083–T086

**步骤：**

1. 对 agent 64 B 与 input template 64 KiB 各写启动期 limit/+1 用例。
2. 用占位符使 input 完整渲染结果分别为 64 KiB 和 +1，断言越界不提交 once 也不启动 Agent。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestSubAgentLimits`，期望三个上限都有精确正反证据。

### T159：锁定诊断安全摘要上限

**文件：** `internal/hook/limits_test.go`、`internal/hook/diagnostic_test.go`

**依赖：** T047–T048

**步骤：**

1. 构造 2 KiB 与 2 KiB+1 的安全摘要，断言 Collector JSON/Text 中的 message 始终不超过 2 KiB。
2. 在截断边界放置多字节 UTF-8 与 secret canary，断言输出有效、结果稳定且不复制原始尾部。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestDiagnosticSummaryLimit`，期望 limit/+1 均产生有界、无泄漏诊断。

### T160：锁定超大 EventContext 的 Action 分流

**文件：** `internal/hook/limits_test.go`

**依赖：** T037、T053、T062、T084

**步骤：**

1. 用 1 MiB+1 的事件断言 condition/点路径仍可读，但 Command recorder 不启动、`send_event=true` HTTP transport 不发请求。
2. 以 `send_event=false` 的静态 HTTP 作对照，断言它不依赖事件 JSON；失败诊断不包含 payload。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestOversizeEventActionRouting`，期望两个 payload action 失败、静态 HTTP 对照成功。

### T161：覆盖 Command Runner I/O 失败

**文件：** `internal/hook/command.go`、`internal/hook/command_test.go`

**依赖：** T053–T057

**步骤：**

1. 增加最小 pipe factory seam，表驱动注入 stdin write、stdout read 和 stderr read 错误。
2. 断言每行都收束进程/pipe goroutine，返回同一技术失败类别，不带原始 I/O 内容。

**验证：** 运行 `go test -race -count=1 ./internal/hook -run TestCommandRunnerIOFailure`，期望三行用例均无泄漏或残留进程。

### T162：锁定 Command I/O 的 Engine 结算

**文件：** `internal/hook/engine_test.go`

**依赖：** T086、T161

**步骤：**

1. 注入返回 I/O 技术失败的 Command runner，断言 Engine 写安全诊断并 fail-open 执行后续规则。
2. 断言 once reservation 被 release，下次同事件可重试，且失败绝不产生 deny。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandIOFailureSettlement`，期望诊断、后续规则、once 和 decision 断言通过。

### T163：锁定无 Hook 的 App 命令/Skill 等价

**文件：** `internal/app/commands_test.go`、`internal/app/skills_test.go`

**依赖：** T141、T149

**步骤：**

1. 通过 nil 和显式 no-op Runtime 各跑一组本地命令与 Skill 激活/清理 fixture。
2. 比较命令结果、mode、Skill allowlist 和可见 App 消息与 legacy golden。

**验证：** 运行 `go test -count=1 ./internal/app -run TestNoHookCommandSkillCompatibility`，期望 nil/no-op/legacy 三方一致。

### T164：锁定无 Hook 的 Orchestrator 工具链等价

**文件：** `internal/orchestrator/skill_runtime_test.go`、`internal/orchestrator/hook_test.go`

**依赖：** T124、T149

**步骤：**

1. 通过 nil/no-op Runtime 各跑 built-in、MCP 和 `load_skill` 三行既有 fixture。
2. 逐行比较 permission decision/Grant、工具输入输出和 Skill system-route 效果与 legacy golden。

**验证：** 运行 `go test -count=1 ./internal/orchestrator -run TestNoHookToolChainCompatibility`，期望三类工具的 nil/no-op 路径不变。

### T165：锁定无 Hook 的 Context/Memory/Compact 等价

**文件：** `internal/contextmgr/hook_test.go`、`internal/sessionctx/manager_test.go`

**依赖：** T131、T149

**步骤：**

1. 通过 nil/no-op observer 各跑 Memory/instructions 注入和 auto/manual compact 既有 fixture。
2. 比较 prompt/context、summary、外置产物与 legacy golden，断言无 Hook event/Prompt/diagnostic。

**验证：** 运行 `go test -count=1 ./internal/contextmgr ./internal/sessionctx -run TestNoHookContextCompatibility`，期望 Context/Memory/Compact 产物不变。

### T166：锁定无 Hook 的 Session/App 状态等价

**文件：** `internal/sessionctx/manager_test.go`、`internal/app/hook_test.go`

**依赖：** T140、T149

**步骤：**

1. 通过 nil/no-op Runtime 各跑 session new/resume/save/switch/close 的既有 fixture。
2. 比较 Conversation JSON、持久化产物和 App 收尾状态，断言无额外生命周期输出。

**验证：** 运行 `go test -count=1 ./internal/sessionctx ./internal/app -run TestNoHookSessionCompatibility`，期望会话状态与 legacy golden 一致。

### T167：锁定无 Hook 的 App/TUI 可见等价

**文件：** `internal/app/hook_test.go`、`internal/tui/program_test.go`

**依赖：** T149、T163–T166

**步骤：**

1. 对无文件与空规则各跑一个 scripted App/TUI 会话，比较消息、状态栏、模式和终态 Model。
2. 断言无 Hook notice，并与 T149/T163–T166 的 wire、工具和状态 golden 组成可见等价矩阵。

**验证：** 运行 `go test -count=1 ./internal/app ./internal/tui -run TestNoHookVisibleCompatibility`，期望两条 no-op 路径的 UI 输出不变。

### T168：锁定无 Hook 的进程资源等价

**文件：** `cmd/xagent/hook_startup_test.go`

**依赖：** T084、T145、T149

**步骤：**

1. 对无文件与空规则分别记录 worker、system event、stderr 和 closer 调用。
2. 断言无 worker/event/stderr，既有 App/MCP 资源仍各关闭一次，启动顺序与 legacy 一致。

**验证：** 运行 `go test -count=1 ./cmd/xagent -run TestNoHookProcessCompatibility`，期望两条 no-op 启动路径无额外资源或输出。

### T169：覆盖 Command 启动与退出失败

**文件：** `internal/hook/command_test.go`

**依赖：** T052–T057、T161

**步骤：**

1. 注入 process start 失败，再运行一个非零退出 shell，断言两者都返回技术失败且完成资源收束。
2. 在 start error、stdout 和 stderr 放置不同 canary，断言 RuntimeRedactor 后的诊断不含原值，也不把非零输出当成 decision。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandExecutionFailure`，期望 start/non-zero 两行的 outcome、收束和脱敏断言通过。

### T170：锁定 Command stdout-only decision 接线

**文件：** `internal/hook/command_test.go`、`internal/hook/engine_test.go`

**依赖：** T049–T051、T054、T087、T169

**步骤：**

1. 用真实 Command runner 分别输出精确 allow、deny、带额外内容的 stdout，以及只在 stderr 中出现的伪 decision。
2. 断言只有完整 stdout 的两个规范形状生效，stderr 永不参与判定，额外 stdout 整体 fail-open，两条流中的 canary 均不泄漏。

**验证：** 运行 `go test -count=1 ./internal/hook -run TestCommandDecisionIntegration`，期望四行 decision 矩阵精确通过。

### T171：恢复 E2E timeout 与 diagnostics 隔离场景

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T141、T150、T154、T169

**步骤：**

1. 在 tagged driver 中用 context/barrier 触发一次可控 Command timeout，断言 Agent 主流程 fail-open 继续并完成 terminal event。
2. 断言安全诊断只在 `/diagnostics` 可见，不进入 Conversation、普通 TUI 或后续 Provider request。

**验证：** 运行 `go test -tags=integration -count=1 ./internal/app -run TestHookE2EFailureDiagnostics`，期望 timeout 和可见性断言通过。

### T172：运行 Hook 单包完整边界测试

**文件：** `internal/hook` 全部生产与测试文件

**依赖：** T091、T148、T156–T162、T169–T170

**步骤：**

1. 运行全部 Loader/Event/Condition/Template/Command/HTTP/Prompt/once/async/Engine 测试。
2. 对照 F59–F60 表核对 limit/limit+1 和 secret canary；失败时返回 T024/T037/T046/T051/T054/T058–T065/T068–T072/T156–T162/T169–T170 的 owner task。

**验证：** 运行 `go test -count=1 ./internal/hook`，期望返回 `ok`。

### T173：运行跨模块定向集成测试

**文件：** 全部 Hook 相关集成文件

**依赖：** T001–T172

**步骤：**

1. 运行 matcher、permission、tool、prompt、provider、orchestrator、contextmgr、sessionctx、app、tui 和 main 测试。
2. 确认测试只使用本地 fake/httptest，不访问公网、真实 Provider 或交互式 TUI。

**验证：** 运行 `go test -count=1 ./internal/matcher ./internal/permission ./internal/tool ./internal/prompt ./internal/provider ./internal/orchestrator ./internal/contextmgr ./internal/sessionctx ./internal/app ./internal/tui ./cmd/xagent`，期望全部返回 `ok`。

### T174：运行 tagged E2E 集成门禁

**文件：** `internal/app/hook_e2e_test.go`

**依赖：** T150–T155、T171、T173

**步骤：**

1. 一次性运行全部 `TestHookE2E*`，确认 smoke、lifecycle/Prompt、Action、decision、async/once、isolated 和 failure/diagnostics 都被选中。
2. 检查 recorder 证明用例只使用 scripted Provider、fake 依赖和 loopback `httptest`，不访问公网。

**验证：** 运行 `go test -tags=integration -count=1 ./internal/app -run '^TestHookE2E'`，期望全部 tagged 场景通过。

### T175：执行竞态检查

**文件：** 并发相关包

**依赖：** T174

**步骤：**

1. 对 once、Prompt lease/gate、Provider observer、async admission/close、run tracker 和 App lifecycle 执行 race。
2. 若出现偶发失败，用 barrier/channel 固定事件边界，不用 `time.Sleep` 掩盖竞态。

**验证：** 运行 `go test -race -count=1 ./internal/hook ./internal/provider ./internal/orchestrator ./internal/contextmgr ./internal/app`，期望无 race report。

### T176：执行顺序 shuffle 压测

**文件：** `internal/hook`、`internal/orchestrator` 测试

**依赖：** T175

**步骤：**

1. shuffle 重复运行规则排序、once、Prompt、工具链和 Turn lifecycle 测试。
2. 断言任何可观察顺序都来自 sequence/effective ordinal/显式链，而非 map 迭代或 goroutine 偶然调度。

**验证：** 运行 `go test -shuffle=on -count=20 ./internal/hook ./internal/orchestrator`，期望 20 轮全部通过。

### T177：执行全仓格式、静态检查、测试与构建

**文件：** 全部变更文件

**依赖：** T001–T176

**步骤：**

1. 只对变更 Go 文件执行 gofmt，再检查全仓 `gofmt -l` 无输出及 `git diff --check` 无错误。
2. 依次运行 vet、全项目测试和当前源码构建，保留所有既有命令、Skill、Memory、MCP、权限、会话和 Agent Loop 回归。

**验证：** 依次运行 `test -z "$(gofmt -l $(rg --files -g '*.go'))"`、`git diff --check`、`go vet ./...`、`go test -count=1 ./...`、`go build -o /private/tmp/xagent-hook-system ./cmd/xagent`，期望全部成功。

## Task 文档生成期自检

- 已在本阶段将 F1–F60、N1–N17、AC1–AC25 映射到下方任务范围和具体验证入口，不把需求审计延后到实现之后。
- `task.md` 获批后才立即生成 `checklist.md`；Checklist 获批前不执行 T001 或任何实现任务。

## 包依赖边界

```text
internal/hook → internal/diagnostics + internal/redact + internal/matcher
internal/permission → internal/matcher（不得导入 internal/tool）
internal/contextmgr → 本地 CompactionObserver（不得导入 internal/hook）
internal/prompt → 通用 Block（不得导入 internal/hook）
internal/provider → 自有 RequestObserver（不得导入 internal/hook）
internal/orchestrator → 负责 hook/tool/permission/contextmgr/prompt/provider 之间的适配
```

`internal/hook` 不得导入 App、TUI、Orchestrator、Tool、Context Manager、Prompt 或 Provider；Hook 事件输入使用自己的窄类型。这样保留 `tool → permission` 的现有方向，不产生反向依赖。

## 执行顺序

```text
兼容基础：
T001 → T002 → T003 → T004
T005 → T006 → T007
T008 → T009
T010 → T011 → T012 → T013
T014

Hook 纯领域：
T015 → T016 → T017 → T018 → T019 → T020 → T021 → T022 → T023 → T024
                                  └──────────────→ T025 → T026 → T027–T030
T017 → T031 → T032 → T033 → T034–T037 → T038 → T039 → T040
T001 + T039 → T041 → T042 → T043
T038 → T044 → T045 → T046

动作与 Engine（Command/HTTP 两支可并行）：
T005 + T015 → T047 → T048 → T049 → T050 → T051
T025 → T052 → T053 → T054 → T055 → T056 → T057
T025 → T058 → T059 → T060 → T061 → T062 → T063 → T064 → T065
T051 + T054 + T062 → T066
T017 + T045 → T067 → T068 → T069 → T070 → T071 → T072 → T073
T016 → T074 → T075
T015 + T033 + T045 → T076 → T077 → T078 → T079 → T080 → T081 → T082
T083 → T084 → T085 → T086 → T087 → T088 → T089 → T090 → T091

运行、Provider 与工具链：
T017 + T090 → T092 → T093 → T094 → T095 → T096 → T097 → T098 → T099
T067 → T100 → T101 → T102 → T103–T104 → T105 → T106–T108
T070 + T105 → T109 → T110 → T111
T003 + T010 → T112 → T113 → T114 → T115 → T116
T010 + T092 + T115 → T117 → T118 → T119 → T120 → T121 → T122 → T123 → T124

Context、isolated、App 与启动：
T031 → T125 → T126 → T127 → T128 → T129
T092 + T111 + T129 → T130 → T131
T098 + T122 → T132 → T133 → T134 → T135
T090 + T098 → T136 → T137 → T138 → T139 → T140
T111 + T131 + T140 → T141 → T142
T030 + T090 + T142 → T143 → T144 → T145

文档和质量门禁：
T030 + T090 → T146 → T147 → T148
T091 + T111 + T145 → T149
T145 + T148 → T150
T111 + T141 + T150 → T151
T066 + T150 → T152
T124 + T150 → T153
T091 + T153 → T154
T135 + T150 → T155
T052 + T084 → T156
T059 + T084 → T157
T083–T086 → T158
T047 + T048 → T159
T037 + T053 + T062 + T084 → T160
T053–T057 → T161
T086 + T161 → T162
T141 + T149 → T163
T124 + T149 → T164
T131 + T149 → T165
T140 + T149 → T166
T149 + T163–T166 → T167
T084 + T145 + T149 → T168
T052–T057 + T161 → T169
T049–T051 + T054 + T087 + T169 → T170
T141 + T150 + T154 + T169 → T171
T091 + T148 + T156–T162 + T169–T170 → T172
T001–T172 → T173
T150–T155 + T171 + T173 → T174 → T175 → T176
T001–T176 → T177
```

关键串行路径是：

```text
严格 Loader 与纯领域
→ Command/HTTP/Prompt/once/async Engine
→ 统一 Run 生命周期
→ Provider request-attempt 终态握手
→ 公共 Tool gate 与 load_skill
→ Compact 与 isolated
→ App/Session/TUI/main 关闭
→ 文档、E2E、race 与全仓门禁
```

## 需求覆盖映射

| 任务范围 | Plan 章节 | Spec / 验收覆盖 |
| --- | --- | --- |
| T001–T014 | §3.2、§11.1–§11.2、§13、§14 | F18、F26、F48、F52、F56–F58；N6、N8、N13、N17；AC5、AC6、AC11、AC18、AC21、AC22 |
| T015–T030 | §4.1、§5、§13.2 | F1–F5、F20、F27、F38、F41–F42、F59；N5、N7、N17；AC1、AC9、AC10、AC15、AC22、AC23 |
| T031–T046 | §4.2、§6 | F6、F16–F26、F34–F35、F39、F60；N1、N4、N7；AC2、AC5、AC6、AC9、AC10、AC23 |
| T047–T066 | §7.1–§7.3、§7.6、§13 | F28–F33、F49–F50、F54、F56–F60；N7–N10；AC7、AC8、AC11、AC12、AC17、AC18、AC23 |
| T067–T091 | §4.4、§7.4–§8 | F34–F47、F49–F55；N1、N3、N4、N7、N10、N12–N14、N16；AC9、AC10、AC12–AC17、AC20、AC21、AC23 |
| T092–T099 | §9.2 | F6、F8–F10、F14、F16–F17；N2、N4、N11；AC2、AC3、AC19、AC20 |
| T100–T111 | §4.4、§10 | F36–F37；N6、N12–N14；AC9、AC20、AC22、AC23 |
| T112–T124 | §11 | F11–F12、F18、F48–F53；N3、N6、N8、N13、N17；AC4、AC5、AC11、AC12、AC21、AC22 |
| T125–T131 | §9.3 | F6、F13–F15、F17、F19、F37–F38；N2、N11–N13；AC2、AC5、AC9、AC19、AC20 |
| T132–T135 | §12 | F14、F36–F38；N11–N13；AC2、AC4、AC19–AC21 |
| T136–T145 | §5.1、§8.3、§9.1 | F1、F4、F6–F8、F15、F37、F47；N2、N4–N6、N12、N14；AC1、AC2、AC16、AC18、AC20、AC22、AC23 |
| T146–T155、T171 | §14–§15 | N5、N6、N9、N15、N17；AC1、AC18、AC22、AC24、AC25 |
| T156–T162、T169–T170 | §13.2、§15 | F30、F49–F50、F59–F60；N7–N10；AC7、AC8、AC12、AC17、AC18、AC23 |
| T163–T168 | §14–§15 | N6；AC22 的命令/Skill、工具链、Context/Memory、Session/App、Provider/Conversation、UI 与进程资源等价矩阵 |
| T172–T177 | §15–§18 | F1–F60、N1–N17、AC1–AC25 的最终自动化证据与质量门禁 |
