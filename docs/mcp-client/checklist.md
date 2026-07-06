# MCP Client Checklist

## 使用方式

实现 MCP Client 时按本清单逐项验收。只有关键项全部通过后，才能认为本章完成。

标记说明：

- `[ ]` 未完成。
- `[x]` 已完成。
- `[blocked]` 被外部问题阻塞，需要说明原因。

## 1. 文档与范围

- [x] `docs/mcp-client/spec.md` 已通过。
- [x] `docs/mcp-client/plan.md` 已通过。
- [x] `docs/mcp-client/task.md` 已通过。
- [x] 本章只实现 MCP tools 能力。
- [x] 本章不实现 MCP resources。
- [x] 本章不实现 MCP prompts。
- [x] 本章不实现 MCP sampling。
- [x] 本章不实现 health check。
- [x] 本章不实现自动重连。
- [x] 本章不声称 MCP Server 是安全沙箱。

## 2. Tool Schema raw JSON 支持

- [x] `tool.Schema` 支持 raw JSON Schema。
- [x] 内置工具 schema 输出保持兼容。
- [x] Anthropic tool definition 可使用 raw input_schema。
- [x] OpenAI tool definition 可使用 raw parameters。
- [x] MCP nested object schema 不被丢弃。
- [x] MCP array schema 不被丢弃。
- [x] MCP number/integer schema 不被丢弃。
- [x] MCP additionalProperties 不被丢弃。
- [x] MCP oneOf/anyOf 不被静默变宽。
- [x] 非 object 或非法 inputSchema 会跳过对应工具并记录诊断。

## 3. MCP 配置结构

- [x] `AppConfig` 增加 `MCP` 配置。
- [x] 支持 `mcp.default_timeout_ms`。
- [x] 支持 `mcp.servers` map。
- [x] server 支持 `disabled`。
- [x] stdio server 支持 `type: stdio`。
- [x] stdio server 支持 `command`。
- [x] stdio server 支持 `args`。
- [x] stdio server 支持 `env`。
- [x] HTTP server 支持 `type: http`。
- [x] HTTP server 支持 `url`。
- [x] HTTP server 支持 `headers`。
- [x] server 支持 `timeout_ms`。
- [x] enabled server 缺少必填字段会报配置错误。
- [x] disabled server 不要求完整 transport 字段。

## 4. 配置加载、合并和校验

- [x] 支持用户级配置。
- [x] 支持项目级配置。
- [x] 用户级先加载。
- [x] 项目级后加载。
- [x] 不同名 MCP server 合并保留。
- [x] 同名 MCP server 项目级整体覆盖用户级。
- [x] 同名覆盖不做字段级继承。
- [x] 项目级 `disabled: true` 可禁用用户级同名 server。
- [x] disabled server 不启动。
- [x] disabled server 不注册工具。
- [x] unknown top-level field 报错。
- [x] unknown mcp field 报错。
- [x] unknown server field 报错。
- [x] 配置错误可诊断并包含 server 名。
- [x] 一个 server 配置错误不影响其他合法 server。

## 5. 环境变量展开与敏感字段

- [x] stdio `command` 支持 `${VAR}` 展开。
- [x] stdio `args` 支持 `${VAR}` 展开。
- [x] stdio `env` 支持 `${VAR}` 展开。
- [x] HTTP `url` 支持 `${VAR}` 展开。
- [x] HTTP `headers` 支持 `${VAR}` 展开。
- [x] 支持字面量 `${VAR}` 的转义机制。
- [x] 未定义变量按 spec/plan 策略报错。
- [x] 未定义变量会报配置错误。
- [x] 敏感 env/header 变量缺失一定报错。
- [x] 敏感 key 判断大小写不敏感。
- [x] token/key/secret/password/authorization/credential 被识别为敏感。
- [x] 错误信息不包含敏感 value。
- [x] 诊断只展示 env/header key。
- [x] canary secret 不出现在 TUI。
- [x] canary secret 不出现在 conversation。
- [x] canary secret 不出现在模型上下文。
- [x] canary secret 不出现在 debug/diagnostic 输出。

## 6. 名称 sanitize 和工具命名

- [x] server 名经过 sanitize。
- [x] remote tool 名经过 sanitize。
- [x] 注册名格式为 `mcp__<server>__<tool>`。
- [x] 注册名满足 provider 工具名字符集限制。
- [x] 注册名满足 provider 工具名长度限制。
- [x] 换行被移除或替换。
- [x] ANSI/control chars 被移除或替换。
- [x] 路径分隔符被替换。
- [x] 空名使用稳定 hash。
- [x] 超长名截断并追加 hash。
- [x] sanitize 冲突使用稳定后缀。
- [x] 原始 server 名被保留。
- [x] 原始 remote tool 名被保留。
- [x] `tools/call` 使用原始 remote tool 名。
- [x] MCP 工具不能覆盖内置工具。

## 7. MCP metadata 安全

- [x] tool name 视为不可信。
- [x] title 视为不可信。
- [x] description 视为不可信。
- [x] inputSchema 视为不可信。
- [x] outputSchema 视为不可信。
- [x] annotations 视为不可信。
- [x] `_meta` 视为不可信。
- [x] metadata 进入 provider 前做长度限制。
- [x] metadata 进入 provider 前处理控制字符。
- [x] metadata 不能覆盖 system/developer 指令。
- [x] MCP result output 视为不可信工具输出。

## 8. JSON-RPC 连接层

- [x] 定义 JSON-RPC request DTO。
- [x] 定义 JSON-RPC response DTO。
- [x] 定义 JSON-RPC notification DTO。
- [x] 定义 JSON-RPC error DTO。
- [x] 校验 `jsonrpc: "2.0"`。
- [x] id 支持 string。
- [x] id 支持 number。
- [x] 内部 pending key 区分 number `1` 与 string `"1"`。
- [x] 同一连接内 pending id 唯一。
- [x] pending map 线程安全。
- [x] response 按 id 关联 request。
- [x] response 后清理 pending。
- [x] ctx timeout 后清理 pending。
- [x] ctx cancel 后清理 pending。
- [x] 迟到 response 丢弃。
- [x] unknown id 记录协议错误且不 panic。
- [x] 支持乱序 response。
- [x] 支持 success/error 混排。
- [x] JSON-RPC error 转换为可恢复错误。
- [x] race 测试通过。

## 9. stdio transport

- [x] stdio server 使用 argv 启动。
- [x] stdio server 不经 shell。
- [x] 项目级 stdio 未确认前不静默启动。
- [x] 项目级 stdio 第一版默认禁用并显示诊断，除非存在显式信任机制。
- [x] 启动诊断展示 command/args/env key/source。
- [x] 启动诊断脱敏。
- [x] stdin 写 newline-delimited JSON-RPC。
- [x] stdout 按 UTF-8 行解析 JSON-RPC。
- [x] stdout malformed line 记录协议错误且不 panic。
- [x] stderr 作为诊断读取。
- [x] stderr 摘要截断。
- [x] stderr 摘要脱敏。
- [x] 子进程启动失败只影响该 server。
- [x] Close 关闭 stdin。
- [x] Close 等待进程退出。
- [x] Close 超时后 terminate process group。
- [x] Close 再超时后 kill。
- [x] XAgent 退出后 stdio 子进程不残留。

## 10. Streamable HTTP transport

- [x] 默认只允许 `https://`。
- [x] 允许 localhost HTTP 例外。
- [x] 禁止非 HTTP(S) scheme。
- [x] 使用 POST 请求 MCP endpoint。
- [x] `Content-Type` 为 `application/json`。
- [x] `Accept` 包含 `application/json, text/event-stream`。
- [x] 支持 `MCP-Protocol-Version`。
- [x] 读取 `Mcp-Session-Id`。
- [x] 后续请求复用 session id。
- [x] session 失效时 server 进入可诊断错误状态。
- [x] 支持 JSON response。
- [x] 支持 SSE response。
- [x] 支持 `202 Accepted`。
- [x] 禁止跨 host redirect 泄露 Authorization。
- [x] 禁止配置覆盖 Host。
- [x] 禁止配置覆盖 Content-Length。
- [x] HTTP 401/500 转成可诊断错误。
- [x] HTTP ctx cancel 生效。

## 11. MCP initialize 流程

- [x] 先建立 transport。
- [x] 发送 `initialize`。
- [x] initialize 请求包含 `protocolVersion`。
- [x] initialize 请求包含 `clientInfo`。
- [x] initialize 请求包含客户端 capabilities。
- [x] 客户端 capabilities 不把 tools 当客户端 capability。
- [x] 校验 server 返回 protocolVersion。
- [x] 不支持 protocolVersion 时禁用该 server。
- [x] 保存 serverInfo。
- [x] 保存 server capabilities。
- [x] initialize 成功后发送 `notifications/initialized`。
- [x] initialized 后才发送 `tools/list`。
- [x] initialize 失败不注册该 server 的工具。

## 12. tools/list

- [x] 发送 `tools/list`。
- [x] 支持 cursor。
- [x] 支持 nextCursor。
- [x] 循环拉取全部工具。
- [x] 重复 cursor 会中止并记录诊断。
- [x] 最大分页次数限制生效。
- [x] 解析 tool name。
- [x] 解析 title。
- [x] 解析 description。
- [x] 解析 inputSchema。
- [x] 保留 outputSchema。
- [x] 保留 annotations。
- [x] 保留 `_meta`。
- [x] 工具名为空跳过该工具。
- [x] schema 无效跳过该工具。
- [x] 单个工具解析失败不影响同 server 其他工具。
- [x] tools/list 失败不影响其他 server。
- [x] max tools 限制生效。

## 13. tools/call

- [x] 请求 method 为 `tools/call`。
- [x] params 包含原始 remote tool name。
- [x] params 包含 arguments。
- [x] JSON-RPC error 转可恢复 tool error。
- [x] result.isError=true 转工具级可恢复 error。
- [x] JSON-RPC error 和 isError 区分。
- [x] content[] text block 转模型可见 Content。
- [x] 非 text block 进入 Data。
- [x] 非 text block 在 Content 中只放摘要。
- [x] structuredContent 进入 Data。
- [x] 长 result 截断。
- [x] 单个 MCP 工具调用失败不终止 Agent Loop。
- [x] ctx timeout 返回 timeout。

## 14. MCP Manager

- [x] Manager 按 server name 管理 Server。
- [x] Manager Start 初始化 enabled servers。
- [x] disabled server 状态正确。
- [x] server 状态包含 configured/starting/initialized/listed/ready/failed/closed。
- [x] 单个 server 启动失败不影响其他 server。
- [x] 单个 server initialize 失败不影响其他 server。
- [x] 单个 server tools/list 失败不影响其他 server。
- [x] Manager 收集 per-server diagnostics。
- [x] Manager 限制 max response size。
- [x] Manager 限制 max tools。
- [x] Manager 默认 max tools 为 128 或文档化等价值。
- [x] Manager 限制 max concurrent calls。
- [x] Manager 默认 max concurrent calls 为 4 或文档化等价值。
- [x] Manager Tools() 返回 adapter 列表。
- [x] Manager Close() 关闭所有 transport。
- [x] Manager Close() 可重复调用且不 panic。

## 15. MCP ToolAdapter

- [x] ToolAdapter 实现 `tool.Tool`。
- [x] `Name()` 返回 registeredName。
- [x] `Description()` 返回安全描述。
- [x] `Schema()` 返回 raw inputSchema。
- [x] `Risk()` 返回 `tool.RiskDangerous`。
- [x] `Execute()` 调用 Manager.CallTool。
- [x] success result 转 `tool.StatusSuccess`。
- [x] isError result 转 `tool.StatusError`。
- [x] JSON-RPC error 转 `tool.StatusError`。
- [x] timeout 转 `tool.StatusTimeout`。
- [x] Data 保留 MCP 结构化内容。

## 16. Permission 集成

- [x] permission rule 校验允许 `mcp__` 前缀工具名。
- [x] MCP 工具默认 dangerous。
- [x] MCP 工具默认 ask。
- [x] MCP fingerprint 包含 registeredName。
- [x] MCP fingerprint 包含 server identity。
- [x] MCP fingerprint 包含 remote tool identity。
- [x] MCP fingerprint 包含参数 hash。
- [x] 不同 server 同名 remote tool 不复用授权。
- [x] MCP 权限提示展示 server/tool 信息。
- [x] MCP 参数摘要脱敏。
- [x] MCP 工具 permanent allow 默认禁用，或按 plan 的 exact 策略实现。

## 17. Registry / cmd / app lifecycle

- [x] main 创建内置 registry。
- [x] main 创建 MCP Manager。
- [x] Manager Start 在 Executor/Orchestrator 前完成。
- [x] MCP tools 注册进主 registry。
- [x] 只读 registry 策略明确，不把 dangerous MCP 工具放入 Plan Mode 只读工具集。
- [x] app 持有 MCP closer。
- [x] q 退出调用 Manager.Close。
- [x] ctrl+c 退出调用 Manager.Close。
- [x] TUI 正常退出调用 Manager.Close。
- [x] ctx cancel 调用 Manager.Close。
- [x] Close 错误可诊断。

## 18. TUI / conversation / diagnostics

- [x] TUI 工具行展示 MCP registeredName。
- [x] 权限提示展示 MCP server name。
- [x] 权限提示展示 remote tool name。
- [x] 权限提示展示脱敏参数摘要。
- [x] conversation 保存脱敏 MCP arguments。
- [x] MCP diagnostics 脱敏。
- [x] MCP diagnostics 截断。
- [x] TUI/status 或日志展示 MCP ready/failed 摘要。
- [x] MCP diagnostics 默认不进入模型上下文。
- [x] 长 description 截断。
- [x] 长 result 截断。
- [x] 控制字符不进入 TUI。

## 19. fake stdio smoke

- [x] fake stdio server 支持 initialize。
- [x] fake stdio server 接收 initialized notification。
- [x] fake stdio server 支持 paginated tools/list。
- [x] fake stdio server 支持 tools/call success。
- [x] fake stdio server 支持 tools/call isError。
- [x] fake stdio server 支持 JSON-RPC error。
- [x] fake stdio server 输出 malformed stdout line 不导致 panic。
- [x] fake stdio server 输出 stderr diagnostic。
- [x] XAgent 发现 fake stdio tool。
- [x] Agent 可调用 fake stdio tool。
- [x] 退出后 fake stdio server 进程清理。

## 20. fake HTTP smoke

- [x] fake HTTP server 支持 initialize。
- [x] fake HTTP server 返回 session id。
- [x] fake HTTP server 支持 JSON response。
- [x] fake HTTP server 支持 SSE response。
- [x] fake HTTP server 支持 202 Accepted。
- [x] fake HTTP server 支持 tools/list。
- [x] fake HTTP server 支持 tools/call success。
- [x] fake HTTP server 支持 401。
- [x] fake HTTP server 支持 500。
- [x] XAgent 发现 fake HTTP tool。
- [x] Agent 可调用 fake HTTP tool。
- [x] ctx cancel 生效。

## 21. 全量验证

- [x] `go test ./internal/mcpclient` 通过。
- [x] `go test ./internal/tool ./internal/permission` 通过。
- [x] `go test ./internal/orchestrator ./internal/app ./internal/tui ./internal/conversation` 通过。
- [x] `go test ./...` 通过。
- [x] `go build ./cmd/xagent` 通过。
- [x] JSON-RPC pending map race 测试通过。
- [x] tmux E2E fake stdio MCP 工具通过。
- [x] tmux E2E fake HTTP MCP 工具通过。
- [x] 平台差异已验证或在文档中标注限制（macOS 已验证；非 Unix 进程组终止走 kill 兜底）。

## 22. 最终验收 AC

- [x] AC1: 配置中声明的 stdio Server 能在启动时启动并完成 initialize。
- [x] AC2: 配置中声明的 HTTP Server 能完成 initialize。
- [x] AC3: initialize 成功后发送 initialized，再调用 tools/list。
- [x] AC4: tools/list 支持 cursor 分页并发现全部工具，重复 cursor 或超过最大页数会中止并记录诊断。
- [x] AC5: 发现的 MCP 工具被注册到现有工具中心。
- [x] AC6: MCP 工具名称包含 sanitized Server 名和工具名，且不与内置工具冲突。
- [x] AC7: Agent 能像调用内置工具一样调用 MCP 工具。
- [x] AC8: MCP 工具调用转换成 tools/call JSON-RPC 请求。
- [x] AC9: JSON-RPC 响应能按 string/number id 正确配对。
- [x] AC10: 并发 pending request 乱序返回不串包。
- [x] AC11: JSON-RPC error 转换成可恢复 tool result。
- [x] AC12: MCP isError=true 转换成工具级可恢复错误。
- [x] AC13: 单个 Server 启动失败不影响其他 Server 工具注册。
- [x] AC14: 单个 Server tools/list 失败不影响其他 Server。
- [x] AC15: XAgent 退出时 stdio 子进程和 HTTP session 被关闭。
- [x] AC16: HTTP 请求支持 ctx 取消，并处理 JSON/SSE/202。
- [x] AC17: 用户级和项目级 Server 配置按整体覆盖规则合并。
- [x] AC18: 项目级 disabled 可禁用用户级同名 Server。
- [x] AC19: env/headers 支持变量展开，敏感变量缺失报错。
- [x] AC20: unknown fields 产生可诊断配置错误，且不影响其他合法 Server。
- [x] AC21: env/header 敏感值不会进入 conversation、TUI、模型上下文、错误、诊断或 debug dump。
- [x] AC22: stdio 项目级 Server 不会在未确认信任边界时静默执行。
- [x] AC23: HTTP 默认只允许 HTTPS，localhost HTTP 例外，redirect 不泄露 Authorization。
- [x] AC24: MCP 工具默认 dangerous，并经过现有权限系统。
- [x] AC25: MCP 权限身份包含 server 名、原始工具名和参数 fingerprint，不同 Server 不复用授权。
- [x] AC26: MCP Server 返回的工具 metadata 和 output 不会被当作系统指令。
- [x] AC27: MCP inputSchema 能保真传递；无法安全表达的 schema 会跳过工具并记录诊断。
- [x] AC28: per-server timeout、max response size、max tools、max concurrent calls 生效。
- [x] AC29: fake stdio server smoke 覆盖 initialize、initialized、tools/list、tools/call、JSON-RPC error、stderr 诊断和退出清理。
- [x] AC30: fake HTTP server smoke 覆盖 session id、JSON 响应、SSE 响应、202 Accepted、HTTP 401/500 和 ctx cancel。
- [x] AC31: 并发 JSON-RPC 测试覆盖乱序响应、未知 id、success/error 混排，且 race 测试通过。
- [x] AC32: 跨平台测试或平台说明覆盖 stdio 进程启动、环境变量、进程终止、换行解析差异。
- [x] AC33: 全量测试、MCP stdio fake server smoke、MCP HTTP fake server smoke 通过。
