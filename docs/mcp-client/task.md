# MCP Client Task

## 任务总览

基于已通过的 `spec.md` 和 `plan.md`，本阶段将 MCP Client 实现拆成可顺序推进、可独立验证的任务。原则是先补现有架构的承载能力，再实现协议层和 transport，最后接入工具注册、权限、生命周期和端到端 smoke。

## Milestone A：基础承载能力

目标：让现有 XAgent 能表达 MCP 工具所需的配置、schema、命名和权限身份。

覆盖任务：T1–T4。

验收：

- 配置结构可表达 MCP servers。
- tool schema 能保真传递 raw JSON Schema。
- MCP 工具名 sanitize 稳定。
- 权限系统能识别动态 `mcp__` 工具名。

## Milestone B：协议与传输

目标：完成 JSON-RPC、stdio transport、Streamable HTTP transport 和 MCP protocol DTO。

覆盖任务：T5–T8。

验收：

- JSON-RPC 并发 pending request 正确配对。
- fake stdio server 可 initialize/list/call。
- fake HTTP server 可 initialize/list/call。

## Milestone C：工具注册与执行

目标：实现 Manager 和 ToolAdapter，把 MCP 工具注册进现有工具中心并可被 Agent 调用。

覆盖任务：T9–T12。

验收：

- MCP 工具注册进 Registry。
- MCP 工具默认 dangerous。
- MCP 工具调用返回 tool.Result。
- 单个 Server 失败不影响其他工具。

## Milestone D：生命周期、诊断和 E2E

目标：补齐 app 生命周期、TUI 诊断、敏感信息脱敏和端到端测试。

覆盖任务：T13–T16。

验收：

- XAgent 退出时关闭 MCP Manager。
- stdio 子进程不残留。
- TUI/status 能看到 MCP 诊断摘要。
- fake stdio/http E2E 通过。

## T1. 扩展 Tool Schema raw JSON 支持

目标：让 MCP `inputSchema` 能保真进入 provider tool definitions。

修改范围：

- `internal/tool/tool.go`
- `internal/tool/registry.go`
- provider tool definition 相关测试

实现内容：

- `tool.Schema` 增加 `Raw json.RawMessage`。
- Anthropic definition 生成时，如果 Raw 非空，使用 raw input_schema。
- OpenAI function parameters 生成时，如果 Raw 非空，使用 raw parameters。
- 内置工具保持现有结构化 schema。
- Raw schema 不参与普通 json marshal 的字段污染。

验收：

- 内置工具 provider schema 不变。
- Raw schema 可包含 nested object、array、number、additionalProperties、oneOf/anyOf。
- Raw schema 不被静默降级。

## T2. 增加 MCP 配置结构、加载、合并和校验

目标：支持用户级/项目级 MCP config，整体覆盖和 disabled。

修改范围：

- `internal/config/config.go`
- `internal/config/load.go`
- `internal/config/validate.go`
- 新增或复用 `internal/mcpclient/config.go`
- config 测试

实现内容：

- `AppConfig` 增加 `MCP MCPConfig`。
- 定义 `MCPConfig`、`MCPServerConfig`。
- 支持 `mcp.default_timeout_ms`。
- 支持 `mcp.servers` map。
- 支持 `disabled: true`。
- 用户级和项目级同名 server 整体覆盖。
- 项目级 disabled 禁用用户级同名 server。
- unknown field 校验。
- env/header `${VAR}` 展开。
- 敏感变量缺失报错。
- 未定义变量默认报配置错误。
- 诊断不包含 env/header value。

验收：

- 不同名合并。
- 同名整体覆盖。
- disabled 生效。
- unknown field 报错。
- 敏感变量缺失报错。
- canary secret 不出现在错误中。

## T3. 实现 MCP 名称 sanitize 和 metadata 安全处理

目标：生成稳定、provider-safe 的 MCP 注册工具名。

修改范围：

- `internal/mcpclient/names.go`
- `internal/mcpclient/redact.go`
- `internal/mcpclient/*_test.go`

实现内容：

- sanitize server name。
- sanitize remote tool name。
- 注册名格式：`mcp__<server>__<tool>`。
- 非法字符转 `_`。
- 连续 `_` 折叠。
- 空名使用 hash。
- 超长截断并追加 hash。
- 冲突时稳定追加 hash 后缀。
- metadata 去控制字符、长度限制。

验收：

- 换行、ANSI、路径分隔符被清理。
- 冲突结果稳定。
- 内置工具不会被覆盖。
- 原始 server/tool name 保留用于 `tools/call`。

## T4. 扩展权限系统支持动态 MCP 工具

目标：让权限系统能识别和授权 `mcp__` 工具，且不同 server 不复用授权。

修改范围：

- `internal/permission/rule.go`
- `internal/permission/matcher.go`
- `internal/permission/authorizer.go`
- `internal/permission/*_test.go`

实现内容：

- 规则校验允许 `mcp__` 前缀工具名。
- MCP fingerprint 包含 registeredName + 参数 hash。
- 权限提示可展示 MCP server/tool 元数据。
- MCP 工具默认 dangerous。
- MCP 工具默认禁用 permanent allow，除非 plan 后续细化 exact 策略。
- redaction 支持任意 MCP 参数。

验收：

- `mcp__a__tool` 可被规则匹配。
- `mcp__a__tool` 和 `mcp__b__tool` fingerprint 不同。
- MCP 工具默认 ask。
- MCP 工具不能误用内置工具规则。

## T5. 实现 JSON-RPC 连接层

目标：提供线程安全的 JSON-RPC request/notify/response 配对能力。

修改范围：

- `internal/mcpclient/jsonrpc.go`
- `internal/mcpclient/transport.go`
- JSON-RPC 测试

实现内容：

- 定义 Request/Response/Error/Notification DTO。
- id 支持 string/number，内部 pending key 必须区分 number `1` 与 string `"1"`。
- 连接内生成唯一 id。
- pending map + mutex。
- Request(ctx, method, params, result)。
- Notify(ctx, method, params)。
- 响应后清理 pending。
- ctx timeout/cancel 清理 pending。
- unknown id 记录协议错误。
- 迟到 response 丢弃。
- JSON-RPC error 转换为 Go error。

验收：

- 并发 N 个 request 乱序响应不串包。
- success/error 混排正确。
- unknown id 不 panic。
- timeout 清理 pending。
- race 测试通过。

## T6. 实现 stdio transport

目标：支持本地子进程 MCP Server。

修改范围：

- `internal/mcpclient/stdio.go`
- stdio fake server 测试

实现内容：

- 使用 argv 启动，不经 shell。
- stdin 写 newline-delimited JSON。
- stdout 按行解析 UTF-8 JSON-RPC。
- stderr 独立读取诊断，截断和脱敏。
- malformed stdout line 记录协议错误。
- 项目级 stdio 第一版默认不启动，记录诊断并提示需要显式信任。
- Close 时关闭 stdin、等待、超时 terminate process group、kill。
- 子进程启动失败只影响该 Server。

验收：

- fake stdio server 可收发消息。
- malformed line 不 panic。
- stderr 摘要脱敏。
- Close 后进程退出。

## T7. 实现 Streamable HTTP transport

目标：支持远程 MCP Server。

修改范围：

- `internal/mcpclient/http.go`
- HTTP fake server 测试

实现内容：

- 只允许 https，localhost HTTP 例外。
- POST MCP endpoint。
- `Content-Type: application/json`。
- `Accept: application/json, text/event-stream`。
- 支持 `MCP-Protocol-Version`。
- 读取/复用 `Mcp-Session-Id`。
- 支持 JSON response。
- 支持 SSE response。
- 支持 202 Accepted。
- redirect 不泄露 Authorization。
- 禁止覆盖 Host/Content-Length 等受控 header。
- ctx cancel 生效。

验收：

- JSON response 正常。
- SSE response 正常。
- session id 复用。
- 401/500 转成可诊断错误。
- ctx cancel 终止请求。
- Authorization 不跨 host redirect。

## T8. 实现 MCP protocol DTO 和流程

目标：封装 initialize、initialized、tools/list、tools/call。

修改范围：

- `internal/mcpclient/protocol.go`
- `internal/mcpclient/server.go`
- protocol 测试

实现内容：

- initialize request 包含 protocolVersion、clientInfo、capabilities。
- 校验 server protocolVersion。
- 保存 serverInfo/capabilities。
- initialize 后发送 `notifications/initialized`。
- tools/list 支持 cursor/nextCursor。
- tools/list 检测重复 cursor 并中止。
- tools/list 限制最大分页次数。
- tools/list 保留 tool metadata。
- tools/call params 为 `{name, arguments}`。
- 区分 JSON-RPC error 和 result.isError。

验收：

- initialize 顺序正确。
- 不支持协议版本时禁用 Server。
- tools/list 分页完整。
- tools/call success/isError/error 区分正确。

## T9. 实现 MCP Manager

目标：管理多个 Server 的连接、工具发现和诊断。

修改范围：

- `internal/mcpclient/manager.go`
- `internal/mcpclient/server.go`
- `internal/mcpclient/diagnostics.go`
- manager 测试

实现内容：

- 按 server name 管理 Server。
- Start(ctx) 初始化所有 enabled Server。
- 单个 Server 失败不影响其他 Server。
- 每个 Server 状态：disabled/configured/starting/initialized/listed/ready/failed/closed。
- 保存 per-server diagnostics。
- 限制 max tools、max response size、max concurrent calls。
- Tools() 返回所有 adapter。
- Close(ctx) 关闭所有 Server。
- Close(ctx) 必须幂等。

验收：

- 一个 Server 失败不影响另一个 Server。
- disabled Server 不启动。
- max tools 生效。
- Close 调用所有 transport。

## T10. 实现 MCP ToolAdapter

目标：把 MCP tools 包装成现有 `tool.Tool`。

修改范围：

- `internal/mcpclient/adapter.go`
- adapter 测试

实现内容：

- `Name()` 返回 registeredName。
- `Description()` 返回安全描述。
- `Schema()` 返回 raw inputSchema。
- `Risk()` 返回 `tool.RiskDangerous`。
- `Execute(ctx, input)` 调用 Manager.CallTool。
- content[] 转 Content/Data。
- structuredContent 转 Data。
- image/resource block 进入 Data，Content 中放摘要。
- isError=true 返回可恢复 error。
- JSON-RPC error 返回可恢复 error。
- 超时返回 timeout。

验收：

- adapter 可注册进 Registry。
- Risk 是 dangerous。
- Execute success/error/timeout 转换正确。
- 非 text block 不污染模型正文。

## T11. 接入 Registry、cmd 和 app lifecycle

目标：启动时发现 MCP 工具并注册，退出时关闭 Manager。

修改范围：

- `cmd/xagent/main.go`
- `internal/app/deps.go`
- `internal/app/app.go`
- `internal/app/update.go`
- app/cmd 测试

实现内容：

- main 创建内置 registry。
- 创建 MCP Manager。
- Manager Start。
- 将 Manager.Tools 注册进 registry。
- 创建 Executor/Orchestrator。
- app 持有 Manager closer。
- q/ctrl+c/正常退出调用 Close。
- 启动诊断进入 status/log。

验收：

- MCP 工具出现在 registry。
- 内置工具仍可用。
- Close 被调用。
- stdio 子进程不残留。

## T12. TUI、conversation 和敏感信息处理

目标：确保 MCP 信息展示安全且可诊断。

修改范围：

- `internal/tui/*`
- `internal/orchestrator/*`
- `internal/conversation/*`
- `internal/mcpclient/redact.go`
- 相关测试

实现内容：

- 工具行展示 registeredName 和安全摘要。
- 权限提示展示 server/tool 信息。
- conversation 只保存脱敏 MCP arguments。
- MCP diagnostics 脱敏。
- MCP tool result 截断。
- metadata/control chars 清理。

验收：

- canary secret 不出现在 TUI。
- canary secret 不出现在 conversation。
- canary secret 不出现在 diagnostics。
- 长 description/result 被截断。

## T13. 端到端 fake stdio server

目标：用真实 XAgent 流程验证 stdio MCP 工具调用。

修改范围：

- `.claude` 或测试目录中的 fake server 辅助程序
- E2E/smoke 测试脚本

实现内容：

- fake stdio server 支持 initialize。
- 支持 initialized notification。
- 支持 tools/list。
- 支持 tools/call。
- 支持 isError。
- 支持 stderr diagnostic。
- XAgent 启动时注册 fake tool。
- Agent 调用 fake tool 成功。

验收：

- fake stdio smoke 通过。
- 退出后 fake server 进程清理。

## T14. 端到端 fake HTTP server

目标：用真实 XAgent 流程验证 Streamable HTTP MCP 工具调用。

修改范围：

- fake HTTP server 测试辅助
- E2E/smoke 测试脚本

实现内容：

- fake HTTP server 支持 initialize。
- 支持 session id。
- 支持 JSON response。
- 支持 SSE response。
- 支持 202 Accepted。
- 支持 tools/list/tools/call。
- 支持 401/500。
- XAgent 启动时注册 fake tool。
- Agent 调用 fake tool 成功。

验收：

- fake HTTP smoke 通过。
- ctx cancel 生效。
- Authorization 不跨 host redirect。

## T15. 全量验证和跨平台检查

目标：完成全部测试、构建和 checklist 验收。

验证命令：

```text
go test ./internal/mcpclient
go test ./internal/tool ./internal/permission
go test ./internal/orchestrator ./internal/app ./internal/tui ./internal/conversation
go test ./...
go build ./cmd/xagent
```

额外验证：

- race 测试 JSON-RPC pending map。
- macOS/Linux stdio 行为。
- Windows 进程启动/终止差异，如本轮不能完整验证，需在 checklist 标注平台限制。
- tmux E2E：fake stdio MCP 工具和 fake HTTP MCP 工具。

验收：

- 所有测试通过。
- fake stdio/http smoke 通过。
- `docs/mcp-client/checklist.md` 全部关键项通过。

## 推荐执行顺序

1. T1 raw schema。
2. T2 config。
3. T3 name/redaction。
4. T4 permission 动态工具。
5. T5 JSON-RPC。
6. T6 stdio。
7. T7 HTTP。
8. T8 protocol。
9. T9 Manager。
10. T10 ToolAdapter。
11. T11 app/cmd lifecycle。
12. T12 TUI/conversation 安全展示。
13. T13 fake stdio E2E。
14. T14 fake HTTP E2E。
15. T15 全量验证。
