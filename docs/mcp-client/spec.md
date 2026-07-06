# MCP Client Spec

## 背景

XAgent 已具备内置工具系统、权限系统、Agent Loop 和工具注册中心，但当前工具来源主要是本地内置实现。为了让用户把外部能力无缝接入 XAgent，需要实现一个 MCP 客户端，在启动时根据配置连接多个 MCP Server，发现其暴露的工具，并通过适配层注册成现有 `Tool` 接口。Agent 调用时不需要区分内置工具和 MCP 工具。

MCP 客户端本章只覆盖工具能力：初始化握手、列出工具、调用工具。不实现 MCP resource、prompt、sampling、roots、elicitation 等其他能力，也不做健康检查和自动重连。

## 目标

- 支持从配置文件声明多个 MCP Server。
- 支持本地 stdio 子进程传输。
- 支持远程 Streamable HTTP 传输。
- 支持 JSON-RPC 2.0 请求/响应收发和按 id 异步配对。
- 启动时初始化 MCP Server、列出工具、将工具注册到现有工具中心。
- Agent 调用 MCP 工具时与调用内置工具体验一致。
- 多个 Server 连接可缓存和统一管理。
- 单个 Server 初始化、列工具或调用失败不影响其他 Server。
- 支持用户级和项目级 MCP 配置合并，项目级整体覆盖用户级同名 Server。
- 支持项目级配置禁用用户级 Server。

## 非目标

- 不实现 MCP resources。
- 不实现 MCP prompts。
- 不实现 MCP sampling。
- 不实现 MCP roots。
- 不实现 MCP elicitation。
- 不实现 Server 健康检查。
- 不实现自动重连。
- 不实现动态热加载配置。
- 不实现 OAuth 或复杂认证流程。
- 不把 MCP Server 当作安全沙箱；MCP 工具仍必须经过 XAgent 权限系统。

## 功能需求

### F1. 配置加载与合并

- 系统必须支持用户级和项目级两层 MCP 配置。
- 配置以 map 声明 Server 列表，每个 key 是 Server 名字。
- 用户级作为全局默认。
- 项目级与用户级不同名 Server 合并保留。
- 项目级同名 Server 必须整体覆盖用户级 Server，而不是字段级合并。
- 项目级同名 Server 即使缺少用户级中的 env/header/timeout 字段，也不能继承这些字段。
- 项目级 Server 可通过 `disabled: true` 禁用用户级同名 Server。
- 合并后 disabled Server 不启动、不 initialize、不注册工具。
- 同名覆盖、禁用和配置来源必须可诊断。
- Server 名必须稳定，用于工具命名、日志和错误展示。
- Server 名必须经过字符集和长度校验，不能包含换行、ANSI 控制字符、路径分隔符或 provider 不支持的工具名字符。
- 未知字段必须报配置错误，避免用户拼错字段后静默失效。

### F2. 配置格式

MCP 配置必须支持 stdio 和 HTTP 两种 Server 类型。

示例：

```yaml
mcp:
  default_timeout_ms: 30000
  servers:
    filesystem:
      type: stdio
      command: npx
      args:
        - -y
        - '@modelcontextprotocol/server-filesystem'
        - .
      env:
        NODE_ENV: production
        API_KEY: ${FILESYSTEM_API_KEY}
      timeout_ms: 30000
    github:
      type: http
      url: https://example.com/mcp
      headers:
        Authorization: Bearer ${GITHUB_MCP_TOKEN}
      timeout_ms: 30000
    old-global-server:
      disabled: true
```

字段要求：

- `mcp.default_timeout_ms`: 可选，全局默认超时。
- `mcp.servers`: Server map。
- `disabled`: 可选 bool；为 true 时该 Server 不启动。
- `type`: enabled Server 必填，取值 `stdio` 或 `http`。
- `timeout_ms`: 可选，覆盖全局默认超时。
- stdio:
  - `command`: 必填。
  - `args`: 可选字符串数组。
  - `env`: 可选 string map。
- http:
  - `url`: 必填。
  - `headers`: 可选 string map。
- 顶层、Server 层、stdio/http 字段中的未知字段都必须报配置错误。
- disabled Server 除 `disabled` 和诊断所需字段外，不要求具备完整 transport 字段。

### F3. 环境变量展开和敏感字段

- `${VAR}` 展开只应用于 stdio `command`、`args`、`env`，HTTP `url`、`headers`，以及 timeout 不参与展开。
- env/header 中包含 token、key、secret、password、authorization 等敏感字段时，引用的环境变量未定义必须报配置错误。
- 普通字段引用未定义环境变量也必须报配置错误，避免静默得到错误命令、URL 或 header。
- 必须支持转义机制表达字面量 `${VAR}`，具体格式由 `plan.md` 固化。
- 展开后的敏感值不得进入 TUI、conversation、模型上下文、诊断、panic、debug dump 或 JSON-RPC 日志。
- 诊断信息只允许展示 env/header key，不展示 value。
- 测试必须使用 canary token 断言敏感值不出现在错误、日志、会话文件、权限提示或工具摘要中。

### F4. stdio 传输

- stdio Server 必须作为本地子进程启动。
- stdio command 不得经 shell 执行，必须使用 argv 形式启动。
- 项目级 stdio Server 第一版默认不静默启动；除非存在明确本地信任记录或用户本次确认，否则该 Server 应禁用并显示诊断。
- 项目级 stdio Server 首次启用或覆盖用户级同名 stdio Server 时，必须有明确用户确认或等价信任机制；不得在打开未信任仓库时静默执行任意项目配置命令。
- 启动确认或诊断必须展示 Server 名、command、args 摘要、env key、配置来源和 PATH 解析结果；敏感值必须脱敏。
- 客户端通过 stdin/stdout 与 Server 交换 JSON-RPC 消息。
- stdio framing 使用 UTF-8、newline-delimited JSON-RPC，每条 JSON-RPC 消息一行。
- stdout 只允许协议消息。
- stderr 只用于诊断，不作为协议消息；stderr 摘要必须截断并脱敏。
- malformed stdout line 是协议错误，不能导致 XAgent panic。
- 子进程启动失败只禁用该 Server，不影响其他 Server。
- XAgent 退出时必须关闭 stdio 连接并终止对应子进程。
- 退出流程应先关闭 stdin 或发送终止信号，再等待，超时后 kill；需要处理进程组，避免子进程残留。
- 单个 Server 的 stdout 消息读取必须串行解析，不得因为并发请求打乱响应配对。

### F5. Streamable HTTP 传输

- HTTP Server 必须通过配置中的 URL 连接。
- 默认只允许 `https://` URL。
- `http://localhost`、`http://127.0.0.1`、`http://[::1]` 可作为本地开发例外。
- 禁止 `file://`、`ftp://` 等非 HTTP(S) scheme。
- 客户端必须向 MCP endpoint 发送 POST 请求。
- 请求 `Content-Type` 必须为 `application/json`。
- 请求 `Accept` 必须包含 `application/json, text/event-stream`。
- 必要时发送 `MCP-Protocol-Version`。
- 必须处理服务端返回的 `Mcp-Session-Id`，并在后续请求中复用。
- Session id 失效或服务端拒绝时，该 Server 应进入可诊断错误状态；本章不做自动重连。
- 必须支持 JSON 响应和 SSE/stream 响应。
- notification 或 response 类 POST 返回 `202 Accepted` 时应按 MCP Streamable HTTP 语义处理。
- 禁止跨域重定向时携带 Authorization 等敏感 header。
- 禁止用户配置覆盖 `Host`、`Content-Length` 等由 HTTP client 管理的敏感协议头。
- HTTP 请求失败只影响对应 Server 或对应工具调用。
- HTTP 请求必须支持 ctx 取消。

### F6. JSON-RPC 2.0

- 所有 MCP 消息必须按 JSON-RPC 2.0 处理。
- 请求必须带唯一 id。
- id 必须支持 string 和 number；同一连接内 pending id 必须唯一。
- 内部 pending key 必须区分 id 类型，例如 number `1` 和 string `"1"` 不能互相覆盖。
- 响应必须按 id 关联到对应 pending request。
- 响应处理后必须清理 pending request。
- 必须支持并发 pending request。
- 必须处理乱序响应。
- 必须处理 success/error 混排。
- 必须校验 `jsonrpc: "2.0"`。
- 必须处理 JSON-RPC error object，error 至少包含 `code` 和 `message`，可包含 `data`。
- 必须区分 transport error、JSON-RPC error 和 MCP tool-level error。
- 收到未知 id 响应时不能 panic，应记录为协议错误。
- 收到 Server notification 时可以忽略或记录；本章不要求消费非工具 notification。
- 应列明并安全忽略或记录 `notifications/tools/list_changed`、`notifications/progress`、`notifications/message` 等 notification。
- ctx 取消或超时后必须停止等待；可尽量发送 `notifications/cancelled`，但不得取消 `initialize`。
- 迟到响应必须丢弃并清理关联状态。

### F7. 初始化握手

每个 Server 启动流程必须至少包含：

1. 建立 transport。
2. 发送 `initialize`。
3. 收到 initialize response。
4. 发送 `notifications/initialized`。
5. 调用 `tools/list`。
6. 注册工具。

`tools/call` 是运行期工具调用，不属于启动握手。

初始化要求：

- 连接建立后必须先发送 `initialize`。
- initialize 请求必须包含客户端支持的 `protocolVersion`、`clientInfo` 和客户端 `capabilities`。
- 客户端 capabilities 只声明本章需要的能力；不得把 tools 当作客户端 capability。
- 必须校验 Server 返回的 `protocolVersion` 是否受支持。
- 协议版本不受支持时必须关闭该 Server，且不注册工具。
- initialize 成功后必须发送 `notifications/initialized`。
- `notifications/initialized` 后才能进入正常请求阶段并调用 `tools/list`。
- initialize 失败则该 Server 不注册任何工具。
- 必须保存 Server 返回的协议版本、serverInfo 和 capabilities，用于诊断和兼容判断。

### F8. 工具发现

- initialize 和 `notifications/initialized` 成功后必须调用 `tools/list`。
- `tools/list` 必须支持 `cursor` / `nextCursor` 分页，循环拉取全部工具。
- `tools/list` 必须检测重复 cursor，避免无限循环。
- `tools/list` 必须限制最大页数。
- 必须解析 Server 返回的工具名、title、description、inputSchema、outputSchema、annotations、_meta。
- 本章只使用工具名、description、inputSchema 和调用所需元数据；其他字段需保留到诊断或未来扩展结构中，不要求暴露给 Agent。
- 工具名为空或 schema 无效时跳过该工具，并记录该 Server 的诊断信息。
- 单个工具解析失败不能影响同一 Server 的其他工具。
- 单个 Server `tools/list` 失败不能影响其他 Server。
- 每个 Server 可注册的工具数量必须有上限，防止恶意 Server 返回过多工具。

### F9. 工具注册和命名

- MCP 工具必须通过适配层实现现有 `Tool` 接口。
- Agent 调用时不需要知道工具来自 MCP。
- 为避免命名冲突，MCP 工具注册名必须包含 Server 名。
- 注册名格式建议为：`mcp__<server>__<tool>`。
- Server 名和远端工具名必须经过确定性 sanitize。
- 注册名必须满足 Anthropic/OpenAI 工具名字符集和长度限制。
- 原始 Server 名和原始工具名必须保留，用于真正的 MCP `tools/call`。
- 如果 sanitize 后冲突，必须使用稳定后缀策略，并记录诊断。
- 内置工具优先；MCP 工具不得伪装成内置工具。
- 工具名、description、inputSchema 均来自不可信 Server，进入模型上下文前必须结构化包裹、长度限制、控制字符处理，不能覆盖 system/developer 指令。

### F10. MCP 工具适配和 Schema

MCP 工具适配层必须：

- 实现 `Name()`。
- 实现 `Description()`。
- 实现 `Schema()`。
- 实现 `Risk()`。
- 实现 `Execute(ctx, input)`，内部调用对应 Server 的 `tools/call`。

Schema 要求：

- MCP `inputSchema` 是 JSON Schema，表达力可能超过当前 XAgent `tool.Schema`。
- 本章必须选择一种保真策略：扩展 XAgent Schema 支持 raw JSON Schema，或在 provider tool definition 生成层保留 raw schema。
- 不得静默丢弃嵌套对象、数组、number/integer、additionalProperties、oneOf/anyOf 等关键约束。
- 如果某个 schema 无法安全表达，必须跳过该工具并记录诊断，而不是注册一个过宽 schema。

风险等级：

- MCP 工具默认 dangerous。
- 配置不得把远端 MCP 工具降级为 safe；未来如需 safe，必须有独立可信来源和安全评审。
- MCP 工具调用必须经过现有权限系统。
- 权限身份必须包含 sanitized 注册名、原始 Server 名、原始工具名和关键参数摘要。
- 权限规则、fingerprint、prompt summary 和 redaction 必须支持动态 MCP 工具名。
- 不同 Server 的同名远端工具不能复用授权。
- 永久授权 MCP 工具的策略必须在 `plan.md` 固化；默认建议先不允许 MCP 工具永久授权，或只允许精确到 server+tool+完整参数 fingerprint。

### F11. 工具调用

- 调用 MCP 工具时必须向对应 Server 发送 `tools/call`。
- `tools/call` 请求参数必须为 `{ name, arguments }`，其中 name 是原始远端工具名。
- 调用参数来自 Agent tool call arguments。
- Server 返回结果必须转换成 XAgent `tool.Result`。
- MCP result 的 `content[]` 必须转换成模型可见文本，并保留结构化表示到 Data。
- `structuredContent` 必须进入 Data；是否进入模型可见文本由 `plan.md` 固化。
- `isError=true` 表示工具级执行失败，应转换成可恢复 tool error，但不同于 JSON-RPC error。
- JSON-RPC error 表示协议/请求错误，也应转换成可恢复 tool error。
- text/image/resource 等 content block 的处理、截断、摘要策略必须在 `plan.md` 固化。
- 单个 MCP 工具调用失败不得终止 Agent Loop。
- ctx 取消或超时时必须取消等待；stdio 子进程是否中断该请求由 `plan.md` 决策。

### F12. 连接缓存和生命周期

- MCP Client Manager 必须按 Server 名缓存连接。
- 同一个 Server 的多个工具共用同一个连接。
- XAgent 启动顺序必须是：加载并合并配置 → 创建 MCP Manager → 初始化 Server 并 list tools → 将 MCP 工具注册进主 Registry/只读 Registry 适配层 → 创建 Executor/Orchestrator。
- XAgent 退出、TUI quit、ctx cancel 时必须调用 MCP Manager Close。
- Manager Close 必须幂等，重复调用不能 panic，不能重复 kill 已释放进程。
- Close 必须关闭 stdio 子进程和 HTTP session。
- 单个 Server 崩溃或退出后，对应工具调用应返回可恢复错误。
- 本章不做自动重连；错误提示可以建议用户重启 XAgent。
- Manager 必须维护每个 Server 的状态和诊断摘要，便于启动后查看哪些 Server/工具被跳过。

### F13. 局部失败隔离和资源限制

- 一个 Server 配置错误不影响其他 Server。
- 一个 Server 启动失败不影响其他 Server。
- 一个 Server initialize 失败不影响其他 Server。
- 一个 Server tools/list 失败不影响其他 Server。
- 一个 MCP 工具调用失败不影响其他工具。
- 必须支持 per-server timeout。
- 默认 timeout 建议为 30 秒，可由配置覆盖。
- 必须限制单个 JSON-RPC 响应大小。
- 默认 max response size 建议沿用工具输出限制或不低于 1MiB，具体值由 `plan.md` 固化。
- 必须限制单个 Server 工具数量。
- 默认 max tools 建议为 128。
- 必须限制单个 Server 并发调用数。
- 默认 max concurrent calls 建议为 4。
- 超时、响应过大、工具过多或并发超限必须可诊断，并且只影响对应 Server 或调用。

### F14. 安全和权限

- MCP 工具必须经过 XAgent 权限系统。
- MCP 工具默认 dangerous。
- MCP Server 名、工具名、description、schema、annotations、_meta、调用结果全部视为不可信数据。
- MCP metadata 和 output 不能覆盖 system/developer 指令。
- 配置中的 env、headers 不得写入 conversation。
- 配置中的 env、headers 不得出现在 TUI 工具参数摘要中。
- JSON-RPC request/response 日志不得包含敏感 header/env 值。
- MCP 参数、结果摘要、错误、诊断必须统一脱敏。
- conversation 只保存脱敏后的 MCP tool arguments。
- stdio command 来自配置，启动前必须经过配置来源和信任边界处理；项目级 stdio 不得静默自动执行未确认命令。
- HTTP Authorization 等敏感 header 不得因 redirect、错误或诊断泄露。

### F15. 错误处理和诊断

- 配置错误必须可诊断。
- Server 启动失败必须可诊断。
- initialize 失败必须可诊断。
- tools/list 失败必须可诊断。
- tools/call 失败必须转换为可恢复 tool result。
- JSON-RPC 协议错误不能导致 XAgent panic。
- 单个 Server 错误不影响其他 Server。
- 诊断应包含 Server 名、配置来源、transport 类型、命令或 URL 摘要、退出码、stderr 摘要、HTTP 状态码、协议错误摘要。
- 所有诊断必须脱敏并截断。
- 启动后应能在 TUI status、日志或诊断摘要中看到 MCP Server 初始化结果；具体展示位置由 `plan.md` 固化。
- 诊断摘要默认不得进入模型上下文；只有用户明确询问 MCP 状态时，才可用脱敏摘要回答。

## 非功能需求

- N1: MCP 客户端必须兼容现有工具注册中心和 Agent Loop。
- N2: MCP 客户端必须兼容现有权限系统。
- N3: MCP 工具默认 dangerous，不能绕过权限确认。
- N4: JSON-RPC 请求/响应配对必须线程安全。
- N5: stdio 子进程生命周期必须可控，避免 XAgent 退出后残留。
- N6: HTTP 请求必须支持 ctx 取消。
- N7: 配置合并必须确定性。
- N8: 错误必须结构化并可恢复，不应让 Agent Loop 崩溃。
- N9: 不得把 env/header 敏感值写入 conversation、TUI、模型上下文、诊断或 debug dump。
- N10: 本章实现应尽量复用现有 Tool/Registry/Executor/Permission 架构。
- N11: MCP 工具 schema 不能静默变宽。
- N12: Server/tool metadata 进入模型前必须长度限制和控制字符处理。

## 配置文件范围

- 用户级：用户配置目录中的 XAgent 配置文件。
- 项目级：项目目录中的 XAgent 配置文件。
- 合并策略：用户级先加载，项目级整体覆盖同名 Server。
- 项目级可用 `disabled: true` 禁用用户级同名 Server。
- 用户级和项目级路径、是否使用单独 MCP 配置文件或并入现有 config，由 `plan.md` 固化。
- 配置加载必须启用 unknown-field 校验或等价机制。

## 不做的事

- 不实现 MCP resource。
- 不实现 MCP prompt。
- 不实现 MCP sampling。
- 不实现 MCP health check。
- 不实现自动重连。
- 不实现 Server 热插拔。
- 不实现 OAuth。
- 不实现远程配置同步。
- 不实现 MCP Server 权限隔离或沙箱。

## 验收标准

- AC1: 配置中声明的 stdio Server 能在启动时启动并完成 initialize。
- AC2: 配置中声明的 HTTP Server 能完成 initialize。
- AC3: initialize 成功后发送 `notifications/initialized`，再调用 `tools/list`。
- AC4: `tools/list` 支持 cursor 分页并发现全部工具，重复 cursor 或超过最大页数会中止并记录诊断。
- AC5: 发现的 MCP 工具被注册到现有工具中心。
- AC6: MCP 工具名称包含 sanitized Server 名和工具名，且不与内置工具冲突。
- AC7: Agent 能像调用内置工具一样调用 MCP 工具。
- AC8: MCP 工具调用会转换成 `tools/call` JSON-RPC 请求，参数为 `{name, arguments}`。
- AC9: JSON-RPC 响应能按 string/number id 正确回到对应请求。
- AC10: 并发 pending request 乱序返回不串包。
- AC11: JSON-RPC error 转换成可恢复 tool result。
- AC12: MCP `isError=true` 转换成工具级可恢复错误，并区别于 JSON-RPC error。
- AC13: 单个 Server 启动失败不影响其他 Server 工具注册。
- AC14: 单个 Server tools/list 失败不影响其他 Server。
- AC15: XAgent 退出时 stdio 子进程和 HTTP session 被关闭，Manager Close 可重复调用且不会 panic。
- AC16: HTTP 请求支持 ctx 取消，并处理 JSON/SSE/202 Accepted 响应。
- AC17: 用户级和项目级 Server 配置按整体覆盖规则合并。
- AC18: 项目级 `disabled: true` 可禁用用户级同名 Server。
- AC19: env 和 headers 支持 `${VAR}` 展开，敏感变量缺失产生配置错误。
- AC20: unknown fields 产生可诊断配置错误，且不影响其他合法 Server。
- AC21: env/header 敏感值不会进入 conversation、TUI、模型上下文、错误、诊断或 debug dump。
- AC22: stdio 项目级 Server 不会在未确认信任边界时静默执行。
- AC23: HTTP 默认只允许 HTTPS，localhost HTTP 例外，redirect 不泄露 Authorization。
- AC24: MCP 工具默认 dangerous，并经过现有权限系统。
- AC25: MCP 权限身份包含 server 名、原始工具名和参数 fingerprint，不同 Server 不复用授权。
- AC26: MCP Server 返回的工具 metadata 和 output 不会被当作系统指令。
- AC27: MCP inputSchema 能保真传递；无法安全表达的 schema 会跳过工具并记录诊断。
- AC28: per-server timeout、max response size、max tools、max concurrent calls 生效。
- AC29: fake stdio server smoke 覆盖 initialize、initialized、tools/list、tools/call、JSON-RPC error、stderr 诊断和退出清理。
- AC30: fake HTTP server smoke 覆盖 session id、JSON 响应、SSE 响应、202 Accepted、HTTP 401/500 和 ctx cancel。
- AC31: 并发 JSON-RPC 测试覆盖乱序响应、未知 id、success/error 混排，且 race 测试通过。
- AC32: 跨平台测试或平台说明覆盖 stdio 进程启动、环境变量、进程终止、换行解析差异。
- AC33: 全量测试、MCP stdio fake server smoke、MCP HTTP fake server smoke 通过。
