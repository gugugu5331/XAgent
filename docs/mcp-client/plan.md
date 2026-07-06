# MCP Client Plan

## 1. 总体方案

本章实现 MCP 工具接入链路：配置加载后创建 MCP Manager，Manager 初始化多个 MCP Server，发现工具后包装成现有 `Tool` 接口并注册进主工具中心。Agent Loop、Executor、Permission、TUI 和 conversation 仍沿用现有工具调用路径。

启动链路：

```text
config.Load
  -> merge MCP user/project config
  -> create tool.Registry with built-ins
  -> create mcp.Manager
  -> manager.Start(ctx)
       -> connect server
       -> initialize
       -> notifications/initialized
       -> tools/list pages
       -> create MCP tool adapters
  -> register MCP tools into registry
  -> create Executor/Orchestrator/App
```

调用链路：

```text
Agent tool call: mcp__server__tool
  -> Permission Authorizer
  -> Executor.ExecuteAuthorized
  -> MCPTool.Execute
  -> Manager.CallTool(server, originalTool, arguments)
  -> JSON-RPC tools/call
  -> convert MCP result to tool.Result
  -> Agent Loop continues
```

关闭链路：

```text
TUI quit / app shutdown
  -> Manager.Close(ctx)
       -> close stdio stdin / terminate process group
       -> close HTTP sessions
```

## 2. 包结构

新增 `internal/mcpclient` 包：

```text
internal/mcpclient/
  config.go          # MCP config structs, merge, env expansion, validation
  manager.go         # Manager lifecycle, server registry, diagnostics
  server.go          # Server session state and tool metadata
  jsonrpc.go         # JSON-RPC types, id, pending request map
  transport.go       # Transport interface
  stdio.go           # stdio transport and subprocess lifecycle
  http.go            # Streamable HTTP transport
  protocol.go        # initialize/tools/list/tools/call DTOs
  adapter.go         # MCP Tool adapter implementing tool.Tool
  schema.go          # raw JSON Schema handling and provider schema mapping
  names.go           # sanitize server/tool names and collision handling
  redact.go          # env/header/argument/result redaction helpers
  diagnostics.go     # per-server diagnostic model
  *_test.go
```

需要改动现有模块：

```text
internal/config/
  config.go          # AppConfig 增加 MCP config
  load.go            # 用户级/项目级加载与 unknown-field 校验
  validate.go        # MCP config validate

internal/tool/
  tool.go            # Schema 增加 raw JSON Schema 表达能力
  registry.go        # provider definitions 输出 raw schema

internal/permission/
  rule.go/matcher.go # 支持动态 MCP 工具名和 fingerprint

internal/app/
  app.go/update.go   # 持有 Manager，退出时 Close
  deps.go            # Deps 增加 MCP Manager/Closer

cmd/xagent/main.go
  启动时创建 MCP Manager、注册工具、关闭生命周期
```

## 3. 配置设计

### 3.1 Config struct

```go
type MCPConfig struct {
    DefaultTimeoutMS int64                       `yaml:"default_timeout_ms"`
    Servers          map[string]MCPServerConfig  `yaml:"servers"`
}

type MCPServerConfig struct {
    Disabled  bool              `yaml:"disabled"`
    Type      string            `yaml:"type"`
    Command   string            `yaml:"command,omitempty"`
    Args      []string          `yaml:"args,omitempty"`
    Env       map[string]string `yaml:"env,omitempty"`
    URL       string            `yaml:"url,omitempty"`
    Headers   map[string]string `yaml:"headers,omitempty"`
    TimeoutMS int64             `yaml:"timeout_ms,omitempty"`
    Source    string            `yaml:"-"`
}
```

`AppConfig` 增加：

```go
type AppConfig struct {
    ...
    MCP MCPConfig `yaml:"mcp"`
}
```

### 3.2 用户级和项目级路径

本轮复用现有配置体系，但扩展为两层：

- 用户级：`~/.config/xagent/config.yaml`，如果存在则先加载。
- 项目级：当前项目目录下 `config.yaml`，后加载。

现有 `--config` 继续作为显式项目级配置路径；如果用户传入 `--config`，它替代默认项目级路径，但仍可与用户级合并。

### 3.3 合并语义

- 用户级先加载。
- 项目级后加载。
- 非 MCP 普通配置仍按现有 AppConfig 语义处理，具体兼容策略在实现时保持现状。
- MCP `servers` map 按 server 名合并。
- 同名 server 项目级整体覆盖用户级。
- 项目级 `disabled: true` 禁用用户级同名 server。
- 不做字段级继承，避免 env/header 混合导致歧义。

### 3.4 unknown fields

配置加载使用 `yaml.Decoder.KnownFields(true)` 或等价机制。

要求：

- 顶层 unknown field 报错。
- `mcp` unknown field 报错。
- server unknown field 报错。
- 单个 MCP server 配置错误不影响其他 server，但整体 AppConfig 的基础字段错误仍按现有逻辑处理。

### 3.5 env 展开

展开字段：

- stdio: `command`、`args`、`env`。
- http: `url`、`headers`。

规则：

- `${VAR}` 从当前进程环境读取。
- `$$\{VAR}` 或计划中固化的转义格式表示字面量 `${VAR}`。
- 未定义变量默认报配置错误。
- env/header 中敏感 key 的缺失一定报错。
- 诊断只展示 key，不展示 value。

敏感 key 判断：大小写不敏感，包含以下片段视为敏感：

```text
token
key
secret
password
authorization
credential
```

## 4. Tool Schema 扩展

当前 `tool.Schema` 表达力不足，不能保真 MCP `inputSchema`。本章先扩展 schema：

```go
type Schema struct {
    Type       string                    `json:"type,omitempty"`
    Properties map[string]SchemaProperty `json:"properties,omitempty"`
    Required   []string                  `json:"required,omitempty"`
    Raw         json.RawMessage          `json:"-"`
}
```

Provider definition 生成时：

- 如果 `Schema.Raw` 非空，Anthropic/OpenAI 使用 Raw schema。
- 否则使用现有结构化 schema。

计划中同时调整 provider tool definition 结构，使 input schema/parameters 支持 raw JSON，而不是只能 marshal `tool.Schema` 当前字段。

MCP 工具 schema 策略：

- MCP `inputSchema` 原样保存到 `Schema.Raw`。
- 如果 schema 不是 object 或 JSON 无效，跳过该工具并记录诊断。
- 不静默丢弃 nested object、array、number/integer、additionalProperties、oneOf/anyOf。

## 5. 工具命名与 metadata 安全

### 5.1 注册名

格式：

```text
mcp__<sanitized-server>__<sanitized-tool>
```

sanitize 规则：

- 只保留 `[A-Za-z0-9_-]`。
- 其他字符转为 `_`。
- 连续 `_` 折叠。
- 去掉首尾 `_`。
- 空名变成稳定 hash 前缀。
- 总长度限制由 provider 限制决定，超长时截断并追加 hash。

保留字段：

- `RegisteredName`: 给模型/工具中心使用。
- `ServerName`: 配置中的原始 server key。
- `RemoteToolName`: MCP 返回的原始 tool name。

`tools/call` 必须使用 `RemoteToolName`，不能使用 sanitized registered name。

### 5.2 metadata 不可信

来自 MCP Server 的以下字段均视为不可信：

- tool name
- title
- description
- inputSchema
- outputSchema
- annotations
- _meta
- result content
- result structuredContent

进入 provider tool definition 前：

- 控制字符移除或转义。
- 长度限制。
- 结构化包裹为工具描述数据，不允许覆盖 system/developer 指令。

## 6. JSON-RPC 设计

### 6.1 数据结构

```go
type Request struct {
    JSONRPC string          `json:"jsonrpc"`
    ID      any             `json:"id,omitempty"`
    Method  string          `json:"method"`
    Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
    JSONRPC string          `json:"jsonrpc"`
    ID      any             `json:"id,omitempty"`
    Result  json.RawMessage `json:"result,omitempty"`
    Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
    Code    int             `json:"code"`
    Message string          `json:"message"`
    Data    json.RawMessage `json:"data,omitempty"`
}
```

### 6.2 Pending map

每个 connection 维护：

```go
type pendingRequest struct {
    id     string
    method string
    ch     chan Response
}
```

- id 内部统一成带类型前缀的 string key，例如 `number:1` 与 `string:1` 必须区分。
- 支持 JSON number/string id。
- id 在连接内单调递增。
- pending map 由 mutex 保护。
- 响应后删除 pending。
- ctx cancel/timeout 后删除 pending。
- 迟到响应丢弃并记录诊断。
- 未知 id 响应记录协议错误，不 panic。

## 7. Transport 接口

```go
type Transport interface {
    Start(ctx context.Context) error
    Send(ctx context.Context, msg JSONRPCMessage) error
    Recv() <-chan JSONRPCMessage
    Close(ctx context.Context) error
    Diagnostics() Diagnostics
}
```

更高层 `Connection` 基于 Transport 实现：

- `Request(ctx, method, params, result)`
- `Notify(ctx, method, params)`
- pending request 管理
- JSON-RPC 校验

## 8. stdio transport

### 8.1 启动

- 使用 `exec.CommandContext` 或等价 API。
- 不经 shell。
- command/args 来自展开后的配置。
- env 为当前环境 + 配置 env 覆盖。
- stderr 独立读取，截断保存诊断。
- stdout 按行读取 UTF-8 JSON-RPC。

### 8.2 信任边界

项目级 stdio server 不得静默执行。

本轮策略：

- 用户级 stdio 配置默认可信，启动时不额外确认。
- 项目级 stdio 配置第一版默认不启动，记录诊断并提示用户需要显式信任。
- 后续可实现本地信任记录或每次启动确认；本轮不做静默执行项目级 stdio。
- 项目级 stdio 配置首次出现、覆盖用户级同名 server、或 command/args 改变时，都必须被视为未信任。
- 如果本轮不实现持久信任记录，则项目级 stdio 可在启动时禁用并显示诊断，直到用户显式允许。

### 8.3 关闭

Close 顺序：

1. 关闭 stdin。
2. 等待进程退出。
3. 超时后终止进程组。
4. 再超时则 kill。
5. 清理 goroutine 和管道。

## 9. Streamable HTTP transport

### 9.1 请求

- POST 到配置 URL。
- `Content-Type: application/json`。
- `Accept: application/json, text/event-stream`。
- 如果已有 session id，带上 `Mcp-Session-Id`。
- 必要时带上 `MCP-Protocol-Version`。
- 用户配置 headers 合并到请求，但禁止覆盖 Host、Content-Length 等受控 header。

### 9.2 响应

支持：

- JSON response。
- SSE / text-event-stream response。
- `202 Accepted` notification acknowledgement。

Session：

- 读取 response header `Mcp-Session-Id`。
- 后续请求复用。
- session 失效时 Server 标记为错误；本章不自动重连。

Redirect：

- 默认不向跨 host redirect 传递 Authorization 等敏感 header。
- 非 http/https redirect 拒绝。

## 10. MCP 协议流程

### 10.1 initialize

请求包含：

- `protocolVersion`
- `clientInfo`
- `capabilities`

客户端 capabilities 只声明本章需要的能力，不声明 tools capability。

响应校验：

- `protocolVersion` 必须在支持列表中。
- 保存 `serverInfo`。
- 保存 `capabilities`。

### 10.2 notifications/initialized

initialize 成功后发送：

```text
notifications/initialized
```

该 notification 不等待 response。

### 10.3 tools/list

- 发送 `tools/list`。
- 支持 `cursor`。
- 如果 response 有 `nextCursor`，继续请求直到为空。
- 记录已见过的 cursor；重复 cursor 视为协议错误并停止该 Server。
- 限制最大分页次数，默认建议 32 页。
- 累计工具数超过上限时停止该 Server 并记录诊断。

### 10.4 tools/call

请求 params：

```json
{
  "name": "remote_tool_name",
  "arguments": {}
}
```

响应处理：

- JSON-RPC error -> 协议/请求错误。
- result.isError=true -> 工具执行错误。
- result.content[] -> 转为模型可见文本和 Data。
- result.structuredContent -> Data。

## 11. Manager 设计

```go
type Manager struct {
    servers map[string]*Server
    tools   []tool.Tool
}
```

职责：

- 加载已合并 config。
- 为每个 enabled server 创建 transport。
- 初始化 server。
- list tools。
- 创建 tool adapters。
- 暴露 `Tools() []tool.Tool` 给 registry 注册。
- 暴露 `Close(ctx)` 给 app 退出调用。
- `Close(ctx)` 必须幂等，重复调用不能 panic，不能重复 kill 已释放进程。
- 暴露 diagnostics。
- 默认 max tools: 128。
- 默认 max response size: 1MiB 或沿用工具输出限制，由实现时统一。
- 默认 max concurrent calls: 4。

Server 状态：

```text
disabled
configured
starting
initialized
listed
ready
failed
closed
```

局部失败：

- Server 失败只影响该 server。
- 单个 tool schema 失败只跳过该 tool。
- Manager 汇总 diagnostics。
- diagnostics 默认不进入模型上下文；只有用户明确询问 MCP 状态时才提供脱敏摘要。

## 12. Tool Adapter 设计

```go
type ToolAdapter struct {
    registeredName string
    serverName     string
    remoteName     string
    description    string
    schema         tool.Schema
    manager        *Manager
}
```

实现：

- `Name()` 返回 registeredName。
- `Description()` 返回安全处理后的 description。
- `Schema()` 返回 raw MCP inputSchema。
- `Risk()` 永远返回 `tool.RiskDangerous`。
- `Execute(ctx, input)` 调用 Manager 的 `CallTool`。

结果转换：

- content text blocks 拼接为 Content。
- 非 text blocks 进入 Data，并在 Content 中放摘要。
- structuredContent 进入 Data。
- isError=true 返回 `tool.StatusError` + recoverable error。
- JSON-RPC error 返回 `tool.StatusError` + recoverable error。
- 超时返回 `tool.StatusTimeout`。

## 13. Permission 集成

现有 permission 需要支持动态 MCP 工具：

- `knownTools` 改为支持 registry 或允许 `mcp__` 前缀。
- MCP 工具 fingerprint 包含 registeredName + serverName + remoteName + 脱敏参数摘要 hash。
- MCP 工具默认 dangerous。
- MCP 工具永久授权默认禁用，除非 plan 后续明确最小 exact 规则格式。
- 权限提示展示：registeredName、serverName、remoteName、参数摘要。
- 不同 server 的同名 remote tool 不能复用授权。

## 14. App / lifecycle 集成

`app.Deps` 增加：

```go
MCPManager io.Closer 或 interface{ Close(context.Context) error }
```

退出路径：

- `q`
- `ctrl+c`
- TUI 正常退出
- app context cancel

都必须调用 Manager Close。Manager Close 必须幂等。

`cmd/xagent/main.go` 启动顺序：

1. load config。
2. create built-in registry。
3. create MCP manager。
4. manager start/list tools。
5. register MCP tools into registry。
6. create executor。
7. create app。
8. run TUI。
9. defer manager close。

## 15. TUI / diagnostics

本轮不做完整诊断 UI，但至少：

- 启动时 Server 失败应记录并可在 status/log 中看到摘要。
- TUI status 可显示 MCP ready/failed server 数，具体样式由实现决定。
- 工具确认提示展示 MCP server/tool 信息。
- 敏感 env/header/参数不出现在工具行。
- conversation 只保存脱敏后的 MCP tool arguments。

## 16. 测试计划

### 16.1 config 测试

- 用户级 + 项目级不同名合并。
- 项目级同名整体覆盖。
- 项目级 disabled 禁用用户级 server。
- 未知字段报错。
- env/header `${VAR}` 展开。
- 敏感变量缺失报错。
- 敏感值不出现在错误字符串。

### 16.2 JSON-RPC 测试

- string id / number id。
- 并发 N 个 pending request。
- 乱序 response 不串包。
- unknown id 记录错误不 panic。
- JSON-RPC error object。
- success/error 混排。
- ctx timeout 清理 pending。
- 迟到 response 丢弃。
- race test 通过。

### 16.3 stdio fake server smoke

fake server 覆盖：

- initialize。
- notifications/initialized。
- paginated tools/list。
- tools/call success。
- tools/call isError。
- JSON-RPC error。
- malformed stdout line。
- stderr diagnostic。
- process close cleanup。

### 16.4 HTTP fake server smoke

fake server 覆盖：

- initialize。
- session id。
- JSON response。
- SSE response。
- 202 Accepted。
- HTTP 401/500。
- redirect Authorization 不泄露。
- ctx cancel。

### 16.5 adapter/registry 测试

- MCP tool 注册名 sanitize。
- sanitize 冲突稳定后缀。
- 内置工具优先。
- raw inputSchema 保真。
- 无法表达/非法 schema 跳过。
- MCP tool Risk 为 dangerous。
- ToolAdapter Execute 转换 content/structuredContent/isError。

### 16.6 permission 测试

- mcp__ 工具可通过权限规则识别。
- 不同 server 同名 remote tool fingerprint 不同。
- MCP 默认 dangerous 触发确认。
- MCP 工具默认不能 permanent allow，或只允许 plan 固化的 exact 策略。
- 权限提示和 conversation 不泄露敏感参数。

### 16.7 E2E

- 启动 fake stdio MCP server，发现工具并注册。
- Agent 调用 fake stdio MCP 工具成功。
- 启动 fake HTTP MCP server，发现工具并注册。
- Agent 调用 fake HTTP MCP 工具成功。
- 单个 server 启动失败不影响内置工具和其他 MCP server。
- 退出 XAgent 后 stdio 子进程不残留。

## 17. 实施顺序

1. 扩展 tool.Schema/provider definitions 支持 raw JSON Schema。
2. 增加 MCP config struct、merge、env expansion、validation。
3. 实现 JSON-RPC connection/pending map。
4. 实现 stdio transport。
5. 实现 Streamable HTTP transport。
6. 实现 protocol initialize/initialized/tools/list/tools/call DTO。
7. 实现 Manager 和 diagnostics。
8. 实现 ToolAdapter 和 schema/name sanitize。
9. 接入 registry 和 cmd/app lifecycle。
10. 接入 permission 动态 MCP 工具。
11. 补 TUI/conversation 脱敏和诊断摘要。
12. 跑 fake server smoke 和全量测试。

## 18. 风险与取舍

- MCP schema 表达力高于当前 Tool schema，因此必须先做 raw schema 支持，否则会产生过宽工具参数。
- stdio 项目级配置自动启动风险高，必须建立信任边界；如果实现复杂，本轮可先禁用未信任项目级 stdio 并提示用户。
- Streamable HTTP 细节多，计划阶段应优先实现标准最小兼容路径，再扩展边缘场景。
- MCP metadata prompt injection 需要和 system prompt/工具描述一起防护，不能只处理 tool result。
- 自动重连不在本章范围内，Server 崩溃后返回可恢复错误即可。
