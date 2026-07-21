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
- 可复用 Skill：按需加载 Markdown SOP，支持共享会话、独立执行、工具白名单和动态斜杠命令。
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

斜杠命令会由本地注册中心直接分发。基础设施命令在本地执行；Skill 命令会保留原始输入并启动对应 AI 工作流。命令名和别名不区分大小写。输入命令前缀后可按 `Tab` 补全，单个匹配直接写回，多个匹配显示选择菜单。

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
| `/commit` | — | Skill/shared | 在当前会话激活提交工作流 |
| `/review` | `/rv`（隐藏兼容入口） | Skill/isolated | 在独立上下文审查当前改动并回流摘要 |
| `/test` | — | Skill/isolated | 在独立上下文运行相关测试并回流摘要 |

状态栏中的 `[DEFAULT]` 和 `[PLAN]` 表示当前会话模式。`/plan` 会让后续普通请求持续使用只读计划模式，直到执行 `/do`；新建、切换或重新打开会话时恢复 `[DEFAULT]`。模式本身不写入会话存储。

为保持兼容，`/permissions status`、`/mcp status`、`/diagnostics` 以及 `/memory status|index|off|delete|rebuild` 仍可使用，但不出现在帮助和补全中。

## Skill

Skill 把可复用的 AI 操作保存为 Markdown SOP。XAgent 启动时只把有效 Skill 的名称和一句说明告诉模型；用户调用短命令或 Agent 调用系统级 `load_skill` 后，完整 SOP 才进入当前请求的系统上下文。

Skill 按以下优先级发现，同名时高层有效定义覆盖低层定义：

1. 项目级：`.xagent/skills/`
2. 用户级：`~/.config/xagent/skills/`
3. 程序内置：`commit`、`review`、`test`

既可以用单个 Markdown 文件，也可以使用带辅助资源的目录：

```text
.xagent/skills/
├── explain.md
└── release/
    ├── SKILL.md
    ├── template.md
    ├── examples/
    └── scripts/
```

单文件和目录入口都使用 YAML frontmatter：

```markdown
---
name: release
description: 检查并准备一次发布
allowed_tools:
  - Read
  - Glob
  - Grep
  - Bash
mode: isolated
history: 1
model: optional-model-name
---

检查版本、变更和测试状态。用户补充要求：{{args}}
```

字段语义：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `name` | 是 | 唯一规范名称，也是斜杠命令名；支持小写字母、数字、`_`、`-` |
| `description` | 是 | 启动阶段提供给 Agent 的一句说明 |
| `allowed_tools` | 否 | 模型可见和可调用工具的白名单；省略或空列表表示不额外限制 |
| `mode` | 是 | `shared` 保持在当前会话，`isolated` 使用临时上下文运行 |
| `history` | 独立模式可选 | 独立执行携带最近多少个完整用户轮次，默认 0 |
| `model` | 否 | 仅覆盖当前 Skill 请求的模型，不改变全局 Provider 配置 |

正文只支持 `{{args}}` 占位符，以调用时的完整参数做一次字面替换；不支持条件、循环、命名参数或可执行模板。

shared Skill 会立即执行并保持活动，后续每轮都携带其 SOP。多个活动 Skill 的非空工具白名单取交集，非空模型必须一致。isolated Skill 只激活目标 Skill，按 `history` 复制完整历史轮次，复用正常权限确认和实时工具进度；完成后主历史只保存原始命令和最终回复，不保存临时工具轨迹，也不会额外调用一次模型做摘要。

目录型 Skill 只自动加载 `SKILL.md`。允许的 `Read`、`Glob`、`Grep` 可以按需访问包内辅助资源，但这个额外根始终只读，不会扩大 `Write`、`Edit`、`Bash` 或权限系统的边界。系统级 `load_skill` 始终可见，但只能激活已发现的有效 Skill。

新增、修改或删除 Skill 后，下一次输入、Tab 补全或 Agent 请求前会原子刷新目录和动态命令。已活动 Skill 继续使用激活时的快照，重新调用后才采用新版本。`/clear` 保留会话历史，但会清除全部活动 Skill；新建、切换会话和重启也从无活动 Skill 开始。

首版不提供 Skill 市场、远程安装、版本/依赖管理、自定义权限规则、底层工具注册、命令别名、逐个卸载或后台文件监听。

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
| `.xagent/skills/` | 项目级 Skill 文件和能力包 |
| `.xagent/conversations/` | 上下文管理产生的外置工具结果 |
| `.mewcode/sessions/` | 可恢复的 JSONL 会话数据 |
| `.mewcode/memory/` | 项目级长期记忆与索引 |
| `MEWCODE.md` | 默认项目指令文件 |
| `~/.config/xagent/config.yaml` | 用户级配置 |
| `~/.config/xagent/skills/` | 用户级 Skill 文件和能力包 |

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
internal/skill/         Skill 发现、快照、活动状态与执行配置
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
