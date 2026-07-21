# XAgent

XAgent 是一个使用 Go 构建的终端 AI 编程助手。它在交互式 TUI 中连接大语言模型，能够读取和修改项目文件、搜索代码、执行命令，并通过权限确认机制控制高风险操作。

> 项目仍在持续开发中，接口和配置格式可能发生变化。

## 核心能力

- 终端对话界面：支持会话列表、流式回复、思考过程展示和请求取消。
- 多模型协议：支持 Anthropic API 和 OpenAI-compatible API。
- Agent 工具循环：内置 `Read`、`Write`、`Edit`、`Glob`、`Grep`、`Bash` 工具。
- 权限控制：提供 `strict`、`default`、`permissive` 三种模式，高风险操作需要用户确认。
- MCP 扩展：支持通过 stdio 或 HTTP 连接 MCP Server，并将远程工具注册到 Agent。
- 上下文管理：自动外置大型工具结果，并支持手动压缩长会话。
- 会话恢复：使用 JSONL 持久化会话，可跳过损坏记录并恢复未正常结束的会话。
- 指令与记忆：加载项目级/用户级指令，维护可检索的长期记忆。
- 安全诊断：对 API Key、Token 等敏感内容进行运行时脱敏。

## 环境要求

- Go 1.26.4 或更高版本
- 一个可用的 Anthropic 或 OpenAI-compatible API
- 支持 ANSI 的终端

## 快速开始

```bash
git clone https://github.com/gugugu5331/XAgent.git
cd XAgent
cp config.example.yaml config.yaml
```

编辑 `config.yaml`，建议通过环境变量提供 API Key：

```yaml
llm:
  protocol: anthropic
  model: your-model
  base_url: https://api.anthropic.com
  api_key: ${ANTHROPIC_API_KEY}
  thinking:
    enabled: true
    show: false
    budget_tokens: 4096

ui:
  show_response_timer: true
  start_mode: list

storage:
  data_dir: .xagent/conversations

permission:
  mode: default
```

设置环境变量并运行：

```bash
export ANTHROPIC_API_KEY="your-api-key"
go run ./cmd/xagent
```

也可以先编译二进制：

```bash
go build -o xagent ./cmd/xagent
./xagent
```

使用其他配置文件时，通过 `-config` 指定路径：

```bash
./xagent -config /path/to/config.yaml
```

## 使用 OpenAI-compatible API

将 `llm` 配置改为：

```yaml
llm:
  protocol: openai
  model: your-model
  base_url: https://api.openai.com/v1
  api_key: ${OPENAI_API_KEY}
  request_timeout_ms: 120000
```

```bash
export OPENAI_API_KEY="your-api-key"
go run ./cmd/xagent
```

`api_key`、MCP 环境变量、HTTP Header 等配置项均支持 `${ENV_NAME}` 形式的环境变量展开。未定义的变量会在启动时产生明确错误。

## 交互操作

在会话列表中按 `Enter` 新建或打开会话。进入对话后，输入任务并按 `Enter` 提交。

| 按键 | 作用 |
| --- | --- |
| `Enter` | 提交输入或选择会话 |
| `Tab` | 补全斜杠命令；多项匹配时打开候选菜单 |
| `↑` / `↓` | 在命令候选菜单中移动选择 |
| `Esc` / `Ctrl+C` | 取消正在生成的请求 |
| `q` / `Ctrl+C` | 空闲时退出程序 |
| `y` | 本次允许工具操作 |
| `s` | 当前会话内允许同类操作 |
| `p` | 永久允许（仅在该操作支持时可用） |
| `n` / `Esc` | 拒绝或取消工具操作 |

斜杠命令会由本地注册中心直接分发。除 `/review` 外，它们不会作为用户消息发送给模型；命令名和别名不区分大小写。输入命令前缀后可按 `Tab` 补全，单个匹配直接写回，多个匹配显示选择菜单。

| 命令 | 别名 | 类型 | 作用 |
| --- | --- | --- | --- |
| `/help` | `/h`、`/?` | 本地 | 显示公开命令、用法和参数提示 |
| `/compact` | `/ctx` | 本地 | 手动压缩当前会话上下文 |
| `/clear` | `/cls` | 界面 | 只清空当前消息显示，保留会话历史和 Agent 上下文 |
| `/plan` | `/p` | 界面 | 进入当前会话的计划模式 |
| `/do` | `/d` | 界面 | 退出计划模式，恢复默认执行模式 |
| `/session` | `/sess` | 本地 | 显示会话 ID、消息数、模式和流式状态 |
| `/memory` | `/mem` | 本地 | 显示用户级和项目级记忆状态及条目数 |
| `/permission` | `/perm` | 本地 | 显示权限模式和各层规则数量 |
| `/status` | `/st` | 本地 | 显示 Provider、模型、模式、Token、Cache、MCP 和最近错误 |
| `/review` | `/rv` | 提示词 | 展开固定审查提示并交给 AI 审查未提交改动 |

状态栏中的 `[DEFAULT]` 和 `[PLAN]` 表示当前会话模式。`/plan` 会让后续普通请求持续使用只读计划模式，直到执行 `/do`；新建、切换或重新打开会话时恢复 `[DEFAULT]`。模式本身不写入会话存储。

为保持兼容，`/permissions status`、`/mcp status`、`/diagnostics` 以及 `/memory status|index|off|delete|rebuild` 仍可使用，但不出现在帮助和补全中。

## 权限模式

在 `config.yaml` 中设置：

```yaml
permission:
  mode: default
```

| 模式 | 行为 |
| --- | --- |
| `strict` | 只读文件工具自动允许，其他工具调用均需确认 |
| `default` | 只读文件工具自动允许，写入和命令执行需确认 |
| `permissive` | 文件工具自动允许；除少量只读命令外，Bash 仍需确认 |

权限规则可放在以下位置，越靠近项目的配置可以覆盖更通用的配置：

- 用户级：`~/.config/xagent/permissions.yaml`
- 项目级：`.xagent/permissions.yaml`
- 本地项目级：`.xagent/permissions.local.yaml`

## MCP 配置

XAgent 支持 HTTP 和 stdio 两种 MCP 传输方式。HTTP 示例：

```yaml
mcp:
  default_timeout_ms: 30000
  servers:
    example:
      type: http
      url: https://example.com/mcp
      headers:
        Authorization: Bearer ${MCP_TOKEN}
```

stdio 示例：

```yaml
mcp:
  servers:
    local-tools:
      type: stdio
      command: npx
      args:
        - -y
        - your-mcp-server
      env:
        API_TOKEN: ${MCP_TOKEN}
```

出于安全考虑，项目配置中的 stdio MCP Server 默认不会直接启动。需要信任的 stdio Server 应配置在用户级文件 `~/.config/xagent/config.yaml` 中。项目配置与用户配置会在启动时合并，项目中的同名 MCP 配置优先。

## 数据与配置

| 路径 | 内容 |
| --- | --- |
| `config.yaml` | 项目配置，已被 Git 忽略 |
| `.xagent/conversations/` | 上下文管理产生的外置工具结果 |
| `.mewcode/sessions/` | 可恢复的 JSONL 会话数据 |
| `.mewcode/memory/` | 项目级长期记忆与索引 |
| `MEWCODE.md` | 默认项目指令文件 |
| `~/.config/xagent/config.yaml` | 用户级配置 |

不要把真实 API Key、Token 或本地权限文件提交到版本库。推荐始终使用环境变量引用敏感配置。

## 项目结构

```text
cmd/xagent/             程序入口
internal/app/           TUI 应用状态与交互逻辑
internal/orchestrator/  Agent 循环与模型编排
internal/provider/      Anthropic/OpenAI provider
internal/tool/          内置工具及执行器
internal/permission/    权限判断与规则加载
internal/mcpclient/     MCP 客户端与传输层
internal/conversation/  会话持久化与恢复
internal/contextmgr/    上下文压缩和大型结果外置
internal/instructions/  项目/用户指令加载
internal/memory/        长期记忆管理
internal/tui/           终端界面组件
docs/                   功能规格、计划与验收记录
```

## 开发与测试

```bash
go test ./...
go vet ./...
```

格式化代码：

```bash
go fmt ./...
```
