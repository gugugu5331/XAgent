# Lifecycle Hook System Spec

## 背景

XAgent 已具备命令分流、Conversation 生命周期、多轮 Agent Loop、上下文压缩、工具注册与权限控制、MCP、Skill 以及结构化诊断，但格式化、审计、上下文提醒和外部通知等固定工作仍需要用户重复触发或持续盯守。现有 UI 事件流主要服务渲染，既不是完整的生命周期总线，也不具备同步拦截、稳定规则顺序、进程内一次性状态和受管异步执行能力。

本功能引入独立的 Lifecycle Hook Engine。用户通过 YAML 声明“事件 + 可选条件 + 动作”，XAgent 在确定的生命周期接点运行规则。工具执行前 Hook 可以返回显式结构化决策，在普通权限确认之前拒绝调用，并把原因作为可恢复工具结果反馈模型；其他运行失败只写脱敏诊断并 fail-open，不改变 Agent 主流程。

## 目标

- 使用声明式 YAML 描述事件、条件、动作和执行控制。
- 覆盖系统、Conversation、完整用户轮次、完整消息、工具执行和上下文压缩生命周期。
- 支持 Command、Prompt、HTTP 和 SubAgent 占位四类动作。
- 支持基于规范事件字段的 `exact`、`glob`、`regex` 和叶子取反条件。
- 在不可绕过安全检查之后、普通权限确认之前提供确定性的工具拦截。
- 支持稳定执行顺序、进程内 `once`、受管异步和动作超时。
- 通过动态 Prompt 状态实现 next、turn、session 三种上下文注入寿命。
- 将 Hook 运行失败隔离为有界、脱敏、可定位的诊断，不污染 Conversation 或模型上下文。
- 保持现有权限、命令、Skill、Memory、MCP、Provider 和 TUI 行为向后兼容。

## 术语与边界

- **Hook Rule**：由必填 `event`、可选 `if`、必填 `action` 以及可选执行控制组成的一条规则。
- **System**：一次 XAgent 应用进程生命周期。
- **Session**：一段 Conversation；新建、恢复、切换和关闭 Conversation 构成其边界。
- **Turn**：一条普通用户消息触发的完整 Agent Run，包含其中全部 Provider iteration 和工具调用。
- **Message**：写入 Conversation 的完整 user 或 assistant 消息，不包含流式 delta、thinking、工具记录或内部摘要。
- **EventContext**：一次 Hook 触发时生成的只读、规范化、可 JSON 序列化事件载荷。
- **Hook failure**：条件求值、模板、进程、网络、超时或响应解析等技术失败；必须记录诊断并 fail-open。
- **Hook deny**：`tool_before` 动作成功返回的合法业务决策；它不是 Hook failure。

## 功能需求

### 配置发现与集中校验

- F1：系统必须在启动时依次读取以下两个可选文件：
  1. 用户级 `~/.config/xagent/hooks.yaml`
  2. 项目级 `<project>/.xagent/hooks.yaml`
  文件不存在时忽略；两层规则累加，不做同名或位置覆盖。
- F2：Hook 文件必须使用以下顶层结构，并以 `version: 1` 标识本规格：

  ```yaml
  version: 1
  hooks:
    - event: tool_before
      if:
        all:
          - field: tool.name
            match: exact
            value: Bash
          - field: tool.arguments.command
            match: regex
            value: 'rm\s+.*'
            negate: true
      timeout: 5s
      action:
        type: command
        command: ./scripts/check-tool.sh
        decision: true
  ```

- F3：解析必须启用严格未知字段检查并进行集中校验。任一现存文件出现 YAML 错误、未知版本、未知字段、未知事件、非法路径、错误类型、非法正则或不兼容选项时，启动整体失败。文件或顶层错误必须包含文件路径、行列和字段路径；规则级错误还必须包含从 1 开始的规则序号。缺少文件不算错误。
- F4：Hook 配置只在启动时加载。修改文件后必须重启 XAgent；本阶段不实现文件监听或交互边界热更新。
- F5：一条规则的稳定身份由规范配置来源与声明序号组成，用于诊断和进程内 `once`。加载器还必须按“用户文件 → 项目文件”的合并顺序分配进程内唯一的 `effective_rule_ordinal`，用于跨文件稳定排序。本阶段不增加用户可配置 rule ID 或显式 priority。

### 生命周期事件

- F6：版本 1 必须且只能支持以下 12 个事件：

  | 层级 | before/start | after/end | 精确边界 |
  | --- | --- | --- | --- |
  | 系统 | `system_start` | `system_stop` | 运行依赖就绪后；Session 结束后、资源关闭前 |
  | 会话 | `session_start` | `session_end` | Conversation 新建或恢复后；切换或退出前的最终保存完成后 |
  | 轮次 | `turn_start` | `turn_end` | 普通用户输入验证后、写消息前；完整 Agent Run 结束后 |
  | 消息 | `message_before` | `message_after` | 完整 user/assistant 消息写入 Conversation 前后 |
  | 工具 | `tool_before` | `tool_after` | 工具安全链中规定的前置点；实际工具执行产生结果后 |
  | 压缩 | `compact_before` | `compact_after` | 一次真实上下文压缩尝试前后 |

- F7：`session_start` 载荷必须区分 `new` 与 `resumed`；`session_end` 必须携带切换或退出等结束原因。Session 等同 Conversation，不等同应用进程。`session_end` 必须在最终保存后、Session 状态清理前分发；分发完成后再清除 session Prompt 并释放 Conversation。
- F8：一个 Turn 从 `turn_start` 开始，到 `turn_end` 结束。`turn_end` 必须携带 `completed`、`error`、`canceled` 或 `max_iterations` 状态。一次 Turn 内任意数量的 Provider iteration 都不得额外产生 turn 事件。`turn_end` 必须在 Turn 状态清理前分发；分发完成后再清除 turn Prompt 和 execution-bound next Prompt。
- F9：普通用户请求的基本时序必须是：`turn_start` → `message_before(user)` → 写入用户消息 → `message_after(user)` → Agent Loop → 每条完整 assistant 消息的 before/写入/after → `turn_end`。消息 Hook 不得修改或拒绝消息。
- F10：Message 事件只覆盖完整 user/assistant 消息。流式文本、thinking、工具调用、工具结果、压缩边界、Memory 内部消息和其他内部摘要不得触发 Message 事件。
- F11：所有已注册模型工具都必须覆盖 Tool 事件，包括内置工具、MCP 工具和系统工具 `load_skill`。未知或伪造工具在注册表检查时先拒绝，不触发 Hook。
- F12：`tool_after` 只在工具确实进入执行后触发，并携带 `success`、`error` 或 `timeout` 结果。注册表、Plan 模式、硬安全、Hook 或普通权限拒绝的调用都不得触发 `tool_after`。
- F13：`compact_before/after` 必须包围一次真实压缩尝试。before 只能携带压缩原因和压缩前统计；after 必须携带成功或错误状态，并同时携带同一份压缩前统计及可用的压缩后统计。
- F14：独立模式 Skill 必须触发自己的 turn、message、tool 和 compact 事件，但不得创建额外 session。其 EventContext 必须使用 `execution.kind: isolated_skill`，并与主对话共享 Hook 配置和进程级状态。
- F15：本地斜杠命令本身不得产生 turn/message 事件；命令造成的真实生命周期操作仍须触发对应事件，例如 `/compact` 触发 compact 事件、Conversation 切换触发 session 事件。

### 规范事件载荷

- F16：每次触发必须创建不可变的 `EventContext`。所有载荷至少包含 `schema_version: 1`、事件名、进程内单调递增 `sequence`、RFC3339 时间和绝对项目根。存在 Agent Run 时还必须包含不透明 execution ID、`main | isolated_skill` kind 和 `default | plan` mode；纯 system/session 事件没有 execution。其他不适用于当前事件的对象必须省略，不得填充空对象。
- F17：版本 1 的稳定字段必须符合下表；表中 optional 字段只在对应阶段出现：

  | 对象 | 稳定字段 | 出现范围 |
  | --- | --- | --- |
  | 顶层 | `schema_version`、`event`、`sequence`、`occurred_at`、`project.root` | 所有事件 |
  | `execution` | `id`、`kind`、`mode` | 存在 Agent Run 的 turn/message/tool/compact |
  | `session` | `id`；optional `state`、`end_reason` | session 及处于 Conversation 中的其他事件 |
  | `turn` | `id`；optional `status`、`error` | turn 及发生在 Turn 内的 message/tool/compact |
  | `message` | `id`、`role`、`content` | message before/after |
  | `tool` | `call_id`、`name`、`arguments`；after 增加 `status`、`duration_ms`、`result.content`、optional `result.error.code/message/recoverable` | tool before/after |
  | `compact` | `reason`、`before.messages`、`before.estimated_tokens`；after 增加 `status`、optional `after.messages/estimated_tokens`、optional `error` | compact before/after |
- F18：`tool.arguments` 必须保持原 JSON 标量、对象和数组类型，不得依赖对象键顺序。`tool.result` 只能暴露已经过执行器限长和脱敏的模型可见内容、错误码和错误信息，不得暴露内部对象或未截断原始输出。
- F19：条件、Shell stdin、默认 HTTP JSON body 和 Prompt 占位符必须读取同一份 EventContext 快照。EventContext 不得自动写入 Conversation、模型上下文或诊断。
- F20：加载器必须校验条件和模板字段是否可能由目标事件提供。`tool.arguments.*` 是唯一允许动态扩展的字段路径；运行时不存在的动态路径按各自规则处理。

### 条件表达式

- F21：`if` 可省略，表示无条件触发。存在时必须且只能包含一个非空 `all` 或一个非空 `any` 列表；版本 1 不允许嵌套、同时出现或混用两种逻辑组合。
- F22：每个叶子条件必须包含 `field`、`match`、`value`，并可包含默认 false 的 `negate`。`field` 使用点路径读取 EventContext。
- F23：`exact` 必须支持字符串、数字和布尔标量，并要求事件值与配置值类型一致；所有 YAML/JSON 数字使用无精度损失的规范数值比较，因此数值 `1` 与 `1.0` 相等，但字符串 `"1"` 不相等。`glob` 和 `regex` 只允许字符串。对象和数组不得直接参与匹配。
- F24：字符串匹配必须区分大小写且不得隐式 trim。注册工具名使用注册中心的规范名称。`glob` 与 `regex` 都采用完整值匹配；需要正则子串搜索时，作者必须显式使用 `.*`。正则必须在启动时预编译。
- F25：只有字段存在且类型合法时才应用 `negate`。字段缺失、路径不存在或值类型不支持时，叶子结果必须为 false，不得因 `negate: true` 变成 true。
- F26：实现必须抽取权限模块可复用的底层 exact/glob 匹配核心，并在 Hook 层增加 regex/negate 组合。现有权限 YAML 仍只公开 exact/glob，不得因本功能接受 regex 或 negate。

### 动作模型

- F27：版本 1 必须支持 `command`、`http`、`prompt`、`subagent` 四种 action type。每条规则只能声明一个动作；动作字段必须使用严格 schema 校验。

#### Command

- F28：Command 动作必须包含静态非空 `command`，并通过独立受信 Runner 在项目根目录使用 `/bin/sh -c` 执行。它不得进入 Agent 工具权限系统，也不得再次触发 Lifecycle Hook。
- F29：Command 的基础环境只能继承 `PATH`、`HOME`、`TMPDIR`、`LANG`、`LC_ALL`、`LC_CTYPE`、`TERM`，并把 `PWD` 设置为项目根；规则可通过静态 `env` 增加或覆盖值。`env` 支持启动期 `${ENV_NAME}` 展开；展开后的秘密必须注册到统一脱敏器。事件字段不得插入 command 或 env。
- F30：Runner 必须把规范 EventContext JSON 写入 stdin，分别限制 stdout/stderr 大小，并支持上下文取消与规则 timeout。输出必须在进入任何诊断、决策结果或返回值前执行限长与脱敏，原始输出不得持久化。启动失败、非零退出、超时或 I/O 失败属于运行期 Hook failure。

#### HTTP

- F31：HTTP 动作必须包含静态 URL，可选 method、静态 headers、`send_event` 和 `decision`。method 默认 `POST`，且只能是 `GET`、`POST`、`PUT`、`PATCH`、`DELETE`；`send_event` 默认 true，并使用 `application/json` 发送 EventContext。`Host` 和 `Content-Length` Header 禁止配置。
- F32：URL 和 Header 不得使用事件占位符；Header 可使用启动期环境变量展开并参与统一脱敏。HTTP 目标只允许 HTTPS，或 host 为精确 `localhost`/loopback IP literal 的 HTTP；其他明文 HTTP 在加载期拒绝。加载期不得做 DNS 请求；运行时连接 `localhost` 时，解析出的每个候选地址都必须是 loopback。
- F33：HTTP Client 最多跟随 3 次重定向，禁止跨 Origin 重定向和 HTTPS 降级，并限制响应体大小。每次重定向都必须重新验证目标协议与 loopback 约束。非 2xx、网络错误、超时、非法重定向或响应过大属于运行期 Hook failure。

#### Prompt

- F34：Prompt 动作必须包含非空 `content`，可选 `scope`，默认 `turn`。模板只支持 `{{tool.arguments.file_path}}` 这类点路径标量占位符，不支持函数、循环、条件或任意表达式。
- F35：占位字段必须是当前事件中实际存在的标量。字段缺失或值为对象/数组时，本次动作失败并记录诊断；不得注入部分渲染结果。
- F36：多条 Prompt 必须按规则执行顺序追加并保留来源边界。注入位置固定在不可变系统/安全指令之后、Skill 与普通运行时上下文之前，不得替换已有系统指令。Prompt 只进入主 Agent Loop 与独立 Skill Agent Loop 的请求，不进入上下文摘要、Memory 更新或其他内部 Provider 调用。
- F37：Prompt 作用域必须遵守以下语义：
  - `next`：有活动 execution 时优先绑定该 execution，否则绑定活动 session；只注入下一次实际发送给 Agent Loop Provider 的请求，附加到请求后消费。execution 绑定内容在 `turn_end` 丢弃，session 绑定内容在 `session_end` 丢弃。
  - `turn`：要求已有活动 Turn，从触发后持续注入该 Turn 剩余的所有 Provider 请求，在 `turn_end` 清除。
  - `session`：要求已有活动 Session，持续注入该 Conversation 及其独立 Skill 请求，在 `session_end` 清除。
- F38：Prompt 的 event/scope 兼容矩阵必须固定如下，矩阵外组合在加载期拒绝。Prompt 是同步状态更新，不允许 async。

  | 事件 | 允许的 scope |
  | --- | --- |
  | `system_start`、`system_stop`、`session_end` | 无 |
  | `session_start` | `next`、`session` |
  | `turn_start`、`message_before`、`message_after`、`tool_before`、`tool_after`、`compact_before`、`compact_after` | `next`、`turn`、`session` |
  | `turn_end` | `session` |

  对 `compact_*` 声明 turn scope 在加载期合法；若它由 `/compact` 在 Turn 外触发，则因没有活动 Turn 而成为运行期 Hook failure。每个 Prompt 条目必须按 `(EventContext.sequence, effective_rule_ordinal)` 排序，使并发事件写入同一 scope 时仍有唯一、可观察的稳定顺序。

#### SubAgent 占位

- F39：SubAgent 动作必须包含非空 `agent` 和 `input`，其模板按 Prompt 的严格占位符规则校验。
- F40：本阶段触发 SubAgent 动作时不得创建 Agent，只能记录结构化 `hook_subagent_not_implemented` 诊断并返回成功 no-op。它不得产生新的 lifecycle 事件或参与工具决策；成功 no-op 可以消耗 once。

### 执行控制与顺序

- F41：规则级必须支持可选 `once`、`async` 和 `timeout`：
  - `once` 默认 false。
  - `async` 默认 false。
  - `timeout` 使用 Go duration 格式，只适用于 Command、HTTP 和未来 SubAgent，必须大于 0 且不超过 10 分钟。
  - Command 默认 timeout 30 秒，HTTP 默认 timeout 10 秒。
- F42：所有 `tool_before` 规则和所有 Prompt 动作都禁止 `async: true`，违反时启动失败。SubAgent 占位可以保留 async 配置兼容性，但仍只执行受管 no-op。
- F43：`once: true` 表示同一规则在本次 XAgent 进程内最多成功执行一次，应用重启后重置。同步动作执行前必须原子地从 idle 转为 pending；并发看到 pending/done 时跳过。失败或超时从 pending 释放为 idle，成功时提交 done。
- F44：异步 once 必须在入队前执行同样的原子保留，防止并发重复入队。队列拒绝或运行失败时释放，成功时提交；合法 allow/deny 和 SubAgent 成功 no-op 都算成功。
- F45：规则稳定顺序必须是用户文件声明顺序，再到项目文件声明顺序。同步规则逐条完成；普通失败不阻止后续规则。异步规则按该顺序入队，但不保证完成顺序。本阶段不得按动作类型重排。
- F46：异步执行器必须最多并行 4 个任务、排队 64 个任务。队列满时立即丢弃本次动作、释放 once 保留并记录诊断，不得阻塞 Agent。
- F47：后台任务不得因 Turn 或 Session 结束自动取消，仍受自身 timeout。退出时必须先同步分发 `system_stop`，再停止接收新任务，最多等待 2 秒，随后取消未完成任务并记录汇总诊断。

### 工具拦截与权限顺序

- F48：普通工具安全链必须固定为：
  1. 注册表与结构校验
  2. Plan 模式限制
  3. 不可绕过的硬安全检查
  4. 同步 `tool_before` Hook
  5. 普通权限规则和用户确认
  6. 工具执行
  7. `tool_after` Hook
  Hook 不得放行或绕过前后任一安全层。`load_skill` 继续保留既有系统级权限豁免，但在 Hook allow 后才能进入其专用执行路径。
- F49：只有同步 `tool_before` 的 Command/HTTP 动作可以设置 `decision: true`。Command 从 stdout、HTTP 从成功响应 body 读取决策；前后 JSON 空白允许，除此之外不得有额外 stdout/body 内容。Command stderr 不参与决策，但必须限长、脱敏且不得反馈模型。决策内容必须是且只能是以下 JSON 之一：

  ```json
  {"decision":"allow"}
  ```

  ```json
  {"decision":"deny","reason":"拒绝原因"}
  ```

- F50：deny reason 必须非空并限长。非零退出、非 2xx、超时、非法 JSON、额外非空输出、未知 decision 或非法 reason 都属于 Hook failure，只记录诊断并继续普通工具安全链。
- F51：合法 allow 必须继续执行后续 Hook 和普通权限链。首个合法 deny 必须立即停止剩余 `tool_before` 规则，不弹普通权限确认，不执行工具，也不触发 `tool_after`。
- F52：合法 deny 必须生成状态为 denied、错误码为 `hook_denied`、`recoverable: true` 的工具结果。模型可见内容必须包含安全化拒绝原因，并沿现有 tool_call/tool_result 回流链进入下一次模型请求，使模型能够调整方案。
- F53：Hook allow、失败、未匹配或队列行为不得写入持久权限规则；Hook deny 也不得改变权限记忆。

### 失败隔离与诊断

- F54：配置期错误必须阻止启动；运行期条件求值、模板、Command、HTTP、决策解析、队列或 SubAgent 占位诊断不得中断 Agent 主流程。`tool_before` 技术失败必须默认放行。
- F55：Dispatcher 必须隔离单条规则错误并恢复意外 panic。除合法 deny 外，一条规则失败不得阻止后续规则、消息写入、工具执行、Turn 结束或应用关闭。
- F56：运行期诊断必须写入现有有界 Diagnostics Collector。与单条规则关联的诊断至少包含稳定诊断码、事件名、配置来源、规则序号、动作类型、失败阶段、耗时和安全摘要；跨规则的异步关闭汇总诊断改为包含事件名、关闭阶段、真实耗时和被取消/未完成数量，并省略不适用的伪规则来源、序号和动作类型。
- F57：诊断码至少覆盖条件求值、模板展开、命令失败、HTTP 失败、非法决策、超时、异步队列满、退出取消和 `hook_subagent_not_implemented`。
- F58：Hook 诊断不得进入普通 TUI 消息、Conversation 或 Provider 上下文；必须可以通过现有 `/diagnostics` 查看。

### 资源上限

- F59：版本 1 必须采用以下硬上限；超过配置期上限时启动失败，超过运行期上限时本次动作失败并按 fail-open 处理，不得静默截断后继续作出 deny 决策：

  | 资源 | 上限 |
  | --- | --- |
  | 单个 Hook YAML / 每文件规则数 / 每规则条件数 | 256 KiB / 256 / 32 |
  | Command 字符串 / env 数量 / env key / 展开后 env value | 16 KiB / 64 / 128 B / 8 KiB |
  | EventContext 规范 JSON | 1 MiB |
  | Command stdout / stderr | 各 32 KiB |
  | HTTP URL / Header 数量 / Header name / 展开后 value | 2 KiB / 32 / 128 B / 8 KiB |
  | HTTP request body / response body / redirects | 1 MiB / 64 KiB / 3 |
  | Prompt 模板 / 单次渲染片段 / 每 execution 或 session 活动 Prompt 总量 | 64 KiB / 128 KiB / 256 KiB |
  | SubAgent agent / input 模板 | 64 B / 64 KiB |
  | deny reason / 单条诊断安全摘要 | 2 KiB / 2 KiB |

- F60：EventContext 超过序列化上限时，条件仍可读取内存快照，但 Command/HTTP payload 动作必须失败；Prompt 渲染或聚合超过上限时不得注入部分内容。Command stdout/stderr 或 HTTP response 超限时必须终止读取并把动作判为失败。所有超限都必须使用不复制原始内容的诊断。

## 非功能需求

- N1：相同配置和同一 EventContext 必须产生相同规则选择与单次 dispatch 内同步执行顺序，不依赖 map 迭代或文件系统遍历。并发 dispatch 可以交错，但必须以进程内 `sequence` 提供全序；所有持久到 Prompt 状态的条目按 sequence 与规则序号稳定排序。
- N2：Hook Engine 必须与现有 UI 事件流和渲染框架解耦。App、Conversation、上下文管理器和 Orchestrator 通过显式生命周期接口调用 Engine。
- N3：工具拦截必须使用同步决策接口；非拦截生命周期使用通知接口。异步动作不得改变已经返回的事件或工具决策。
- N4：规则快照、sequence 分配、once 状态、Prompt 队列和异步执行器必须线程安全，不得依赖当前工具批次偶然串行。并发事件不得造成重复 once、数据竞争或未定义的 Prompt 顺序。
- N5：Hook 加载和集中校验不得访问网络、启动进程、调用 Provider 或消耗对话 Token。
- N6：没有 Hook 文件时，不得改变 Provider 请求、Conversation 历史、权限判断、命令结果或可见 TUI 行为，只允许固定且可忽略的初始化开销。
- N7：所有配置、条件、Command/HTTP 输入输出、Prompt 内容、deny reason、错误文本和诊断字段必须遵守 F59–F60 的硬边界，防止无界内存、日志或模型上下文增长。
- N8：环境变量展开值、认证 Header、Command stdout/stderr、HTTP body/response、工具结果和错误在进入诊断、决策结果或用户可见错误前必须经过统一 RuntimeRedactor。诊断不得复制消息正文、工具参数、Shell 输出、HTTP body 或响应内容。
- N9：用户级和项目级 Hook 都属于受信任配置。XAgent 不为每次动作重复确认；用户文档必须明确项目 Hook 可执行任意静态 Shell，并提示在打开不受信任项目之前检查 `.xagent/hooks.yaml`。
- N10：Hook 内部 Shell/HTTP 不得产生嵌套 Hook，避免递归、重复通知和无限执行。
- N11：独立 Skill 与主对话共享安全 Hook、Prompt session 状态和进程级 once，但必须通过 execution ID/kind 隔离 Turn 状态，不能混淆两个并行执行。
- N12：Prompt 注入必须在同一作用域的每次 Prompt 重建中稳定存在，并在 next 消费、turn_end、session_end 或会话切换时准确清除，不得跨边界残留。
- N13：Hook allow、Prompt 或其他动作不得覆盖系统安全指令、扩大 Skill 工具白名单、绕过 Plan Mode、持久权限或工具注册中心。
- N14：异步执行器关闭必须有界，不得发生 goroutine 泄漏、重复 Close、send-on-closed-channel 或退出死锁。
- N15：所有新增行为必须可以在不启动真实 TUI、不连接真实 Provider、不访问公网的情况下通过单元和集成测试验证。
- N16：Hook 包及其共享状态必须通过 race 测试；全项目格式检查、静态检查、自动化测试和源码构建必须保持通过。
- N17：版本 1 遇到未知 schema version、未知事件、未知 action 或未知字段必须拒绝启动。未来新增事件、动作或匹配器必须提升 schema version 或提供明确兼容策略。

## 不做的事

- 不实现 SubAgent 动作的真实运行；只保留可校验、可诊断的占位结构。
- 不持久化 once 标记；应用重启后全部重置。
- 不支持规则显式 priority、依赖关系、before/after 排序声明或按动作类型重排。
- 不实现 Hook 市场、远程下载、发布、签名、版本管理或依赖解析。
- 不实现 Hook 文件监听、运行期热更新或候选快照回滚。
- 不扩展现有权限 YAML 的公开 matcher；权限规则仍只支持 exact/glob。
- 不允许嵌套 all/any、任意布尔表达式、脚本条件或动态求值语言。
- 不允许事件字段插入 Shell command、HTTP URL、HTTP Header 或环境变量。
- 不允许 Message Hook 修改、替换、拒绝或重新排序 Conversation 消息。
- 不让本地斜杠命令自动变成 Agent Turn 或 Message。
- 不让 Hook action 自动注册新工具、命令、Skill、权限规则或 Provider。
- 不提供图形化 Hook 编辑器、项目 Hook 信任管理界面或逐次动作确认。

## 验收标准

- AC1（F1–F5）：`~/.config/xagent/hooks.yaml` 与项目 `.xagent/hooks.yaml` 按约定顺序累加；任一文件缺失可正常启动，任一现存文件非法则启动失败，并准确报告文件、行列、规则和字段。
- AC2（F6–F15）：12 个事件均在规定边界触发；一次用户请求无论包含多少 Provider iteration 都只有一组 turn start/end；本地命令、独立 Skill 和会话切换行为符合定义。
- AC3（F9–F10）：只有完整 user/assistant 消息触发 before/after；delta、thinking、工具记录、Memory 和内部摘要不触发。
- AC4（F11–F12）：内置、MCP 和 `load_skill` 均触发工具事件；未知工具、Plan 拒绝、硬安全拒绝、Hook 拒绝和权限拒绝都不触发 tool_after。
- AC5（F16–F20）：同一事件供条件、Shell、HTTP 和 Prompt 使用的 EventContext 内容一致；动态工具参数保持 JSON 类型，不适用对象缺省，敏感内部数据不泄露。
- AC6（F21–F26）：省略 if 无条件触发；all/any、类型化 exact、完整值 glob/regex 和 negate 均有正反测试；缺失字段即使 negate 也不匹配；现有权限 YAML 行为不变。
- AC7（F28–F30）：Command 在项目根执行，收到规范 JSON stdin 和受控环境；退出、超时、取消、输出限长和 stdout/stderr 秘密 canary 脱敏可重复验证，且不会触发嵌套 Hook。
- AC8（F31–F33）：HTTP 的 method、协议、loopback 解析、3 次重定向、静态 URL/Header、环境变量、JSON body、状态码、响应上限和超时限制均被验证。
- AC9（F34–F38）：Prompt 严格占位符、稳定拼接顺序、固定注入位置及 next/turn/session 三种清理边界均可验证；它不进入摘要或 Memory Provider 请求，非法事件/作用域和 async 组合启动失败。
- AC10（F39–F40）：合法 SubAgent 占位配置可以启动，触发时只产生 `hook_subagent_not_implemented`，不会创建 Agent 或新的 lifecycle 事件。
- AC11（F48–F53）：合法 deny 在权限确认前短路，拒绝原因以 `hook_denied` 可恢复工具结果反馈模型；allow 继续执行且任何决策都不修改权限记忆。
- AC12（F49–F50、F54）：非零退出、非 2xx、超时、非法 JSON、额外输出、未知 decision 或空 deny reason 均只记录诊断并继续普通工具安全链。
- AC13（F45、F51）：同步规则严格按“用户文件 → 项目文件 → 文件内声明顺序”运行；普通失败继续后续规则，首个合法 deny 才短路。
- AC14（F43–F44）：同步和异步 once 都在执行或入队前原子保留，成功提交、失败释放；并发触发不会同时运行或重复成功执行同一 once 规则。
- AC15（F41–F47）：最多 4 个异步任务运行、64 个排队；队列满立即丢弃并诊断；默认 timeout、正数校验、10 分钟上限及 async 禁用组合均生效。
- AC16（F47）：`system_stop` 在停止接收异步任务前完成分发；随后等待不超过 2 秒并取消剩余任务，不存在 goroutine 泄漏、重复关闭或关闭死锁。
- AC17（F54–F55）：条件、模板、Command、HTTP、决策解析、队列和意外 panic 的失败不改变 Agent 主流程结果，后续规则仍可运行。
- AC18（F56–F58）：每类失败都有稳定诊断码及来源定位；环境秘密、认证 Header、消息正文、工具参数和 HTTP 内容不会出现在诊断中。
- AC19（F14、N11）：独立 Skill 触发自己的 turn/message/tool/compact、不触发 session；execution.kind 正确，安全 Hook 与进程级 once 和主会话共享，执行状态不串扰。
- AC20（F37、N12）：turn/session 结束和会话切换后无过期 Prompt；未消费的 execution-bound next 在 turn_end 清除，session-bound next 在 session_end 清除；同一作用域 Prompt 在每次上下文重建中稳定存在。
- AC21（F48、F53、N13）：Hook allow、失败或未匹配不能绕过硬安全、Plan、持久权限、用户确认或 Skill 工具白名单；deny 不写入持久权限。
- AC22（N6、N13）：没有 Hook 文件时，现有命令、Skill、Memory、上下文、权限、MCP、Provider 和会话测试行为不变；权限 YAML 不接受新增 matcher。
- AC23（F59–F60、N4、N14、N16）：Hook 包和集成路径通过全部资源边界、并发 once、异步队列、关闭、事件顺序、Prompt 清理及拒绝回流的 race 测试。
- AC24（N9）：示例配置与用户文档明确项目 Hook 是受信代码、修改后需重启，以及打开不可信项目时的检查责任。
- AC25（F1–F60、N1–N17）：格式检查、静态检查、全项目测试和当前源码构建成功；真实 TUI 端到端验证覆盖生命周期触发、Prompt 注入、Command/HTTP、工具 allow/deny、超时、异步、once、诊断和独立 Skill。
