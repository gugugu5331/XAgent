# System Prompt Tasks

## 文件清单

| 操作 | 文件 | 职责 |
|------|------|------|
| 新建 | `internal/prompt/section.go` | 定义 Section、RunMode、BuildRequest、Block、Bundle |
| 新建 | `internal/prompt/builder.go` | 稳定模块排序、空行拼装、Build 主入口 |
| 新建 | `internal/prompt/sections.go` | 七个固定稳定模块文本 |
| 新建 | `internal/prompt/dynamic.go` | 动态系统补充、特殊标签、轮次策略、动态值转义 |
| 新建 | `internal/prompt/prompt_test.go` | Prompt 稳定性、模块顺序、动态隔离和注入防护测试 |
| 修改 | `internal/provider/provider.go` | 扩展 ChatRequest、SystemBlock、ToolDefinition、CachePolicy、Usage |
| 修改 | `internal/provider/anthropic.go` | 映射 system/tool cache_control，解析 cache usage |
| 修改 | `internal/provider/openai.go` | 结构化 system blocks 退化为普通 OpenAI system message |
| 修改 | `internal/provider/tool_parse_test.go` | Provider 请求形状、cache_control、usage、OpenAI 退化测试 |
| 修改 | `internal/tool/registry.go` | 生成按工具名排序的工具定义快照输入 |
| 修改 | `internal/tool/*` | 强化各工具 Description 关键规则 |
| 修改 | `internal/tool/tool_test.go` | 工具描述、定义顺序、schema 稳定性测试 |
| 修改 | `internal/orchestrator/chat.go` | 构造 prompt bundle、工具快照和结构化 ChatRequest |
| 修改 | `internal/orchestrator/agent_loop.go` | 向 stream 传递 Agent Loop iteration |
| 修改 | `internal/orchestrator/run_request.go` | 移除旧 systemPrompt 拼接或改为兼容包装 |
| 修改 | `internal/orchestrator/chat_test.go` | 动态注入、Plan Mode 多轮、历史不污染、安全边界测试 |
| 修改 | `internal/events/events.go` | UsageDisplay 增加 cache 字段并改为 int64 |
| 修改 | `internal/app/update.go` | 请求级累计四类 usage，新请求清零 |
| 修改 | `internal/tui/status.go` | 展示 cache creation/read token |
| 修改 | `internal/tui/messages_test.go` | 状态栏 usage/cache 展示测试 |
| 修改 | `.claude/agent_loop_smoke.go` | 如 provider fake usage 字段变化导致编译失败则同步调整 |

## T1: 固定包依赖契约

**文件：** `docs/system-prompt/task.md`、后续代码 review 检查项
**依赖：** 无

**步骤：**
1. 明确 `internal/prompt` 只能依赖标准库。
2. 明确 `internal/provider` 不导入 `internal/prompt`。
3. 明确 `internal/tool` 不导入 `internal/provider`。
4. 明确转换职责在 `internal/orchestrator`：prompt block → provider system block，tool registry → provider tool definition。
5. 后续每次实现任务后用 `go test ./...` 或编译错误检查是否出现 import cycle。

**验证：** `go test ./...` 不出现 import cycle。

## T2: 定义 prompt 纯数据模型

**文件：** `internal/prompt/section.go`
**依赖：** T1

**步骤：**
1. 新建 `internal/prompt` 包。
2. 定义 `Section`，包含 `Name`、`Priority`、`Content`、`Stable`。
3. 定义 `RunMode` 及 `default`、`plan`、`do` 三个常量。
4. 定义 `BuildRequest`，包含 `Mode`、`Iteration`、`ProjectRoot`、`OptionalStableSections`。
5. 定义 `Block` 和 `Bundle`。
6. 确保该文件只依赖标准库或不依赖任何包。

**验证：** `go test ./internal/prompt` 编译通过。

## T3: 实现七个固定稳定模块

**文件：** `internal/prompt/sections.go`
**依赖：** T2

**步骤：**
1. 定义固定模块优先级常量或内部列表。
2. 实现身份模块，声明 XAgent 是终端 AI 编程助手。
3. 实现系统约束模块，声明遵守安全边界和系统优先级。
4. 实现任务模式模块，说明默认、Plan、Do 的职责差异。
5. 实现动作执行模块，说明先观察再行动，遇到阻塞要说明。
6. 实现工具使用模块，说明优先专用工具、编辑前先读、路径限制、危险工具谨慎。
7. 实现语气风格模块，要求中文、简洁、直接。
8. 实现文本输出模块，要求结果优先、必要时给验证证据。
9. 避免引入“主动执行危险操作”“绕过确认”等扩大权限语义。

**验证：** `go test ./internal/prompt` 编译通过。

## T4: 实现稳定模块拼装

**文件：** `internal/prompt/builder.go`
**依赖：** T2, T3

**步骤：**
1. 实现 `StableSections(optional []Section) []Section`。
2. 固定模块始终排在可选稳定模块之前。
3. 可选稳定模块按 `Priority` 再按 `Name` 稳定排序。
4. 过滤空白 `Content`，避免产生空占位符。
5. 处理重复可选模块名，采用确定性策略保留一个结果。
6. 实现稳定 block 生成，模块之间用单个空行分隔。
7. 实现 `Build(req BuildRequest) Bundle`，组合 stable 和 dynamic blocks。

**验证：** 新增测试后运行 `go test ./internal/prompt`。

## T5: 实现动态系统补充和轮次策略

**文件：** `internal/prompt/dynamic.go`
**依赖：** T2

**步骤：**
1. 实现 `DynamicBlocks(req BuildRequest) []Block`。
2. 动态 block 使用 `<system-reminder>` 标签包裹。
3. 注入 `ProjectRoot` 等环境信息，动态值必须转义或结构化格式化。
4. 处理 XML 闭合标签、Markdown fence、ANSI escape、Unicode 双向控制字符等特殊输入。
5. iteration 1 输出完整模式说明。
6. iteration 可被 3 整除时输出关键约束重复。
7. 其他 iteration 输出精简模式提醒。
8. Plan Mode 动态内容必须包含只读约束。
9. Do Mode 动态内容必须说明允许执行工具推进任务。
10. 不允许用户输入、工具输出或文件内容进入动态系统补充。

**验证：** 新增测试后运行 `go test ./internal/prompt`。

## T6: 补 prompt 单元测试

**文件：** `internal/prompt/prompt_test.go`
**依赖：** T2-T5

**步骤：**
1. 测试七个固定模块顺序稳定。
2. 测试固定模块包含核心句义：XAgent、Plan Mode、优先专用工具、编辑前先读、路径限制、中文简洁。
3. 测试可选稳定模块为空时无占位符。
4. 测试可选稳定模块按 priority/name 稳定追加。
5. 测试重复可选模块名结果确定。
6. 测试连续两次 `Build` 输出 stable blocks 字节一致。
7. 测试 `ProjectRoot` 只出现在 dynamic blocks，不出现在 stable blocks。
8. 测试动态值转义，覆盖闭合标签、ANSI escape、Markdown fence、Unicode 双向控制字符。
9. 测试用户输入形态的注入文本不会进入 dynamic blocks。
10. 测试 iteration 1/2/3 的完整/精简/重复策略。

**验证：** `go test ./internal/prompt` 通过。

## T7: 扩展 provider DTO 与 Usage，保留兼容字段

**文件：** `internal/provider/provider.go`
**依赖：** T1

**步骤：**
1. 新增 `SystemBlock`，包含 `Name`、`Content`、`Cacheable`。
2. 新增 `ToolDefinition`，包含 `Name`、`Description`、`Schema`。
3. 新增 `CachePolicy`，包含 `EnablePromptCache`。
4. 扩展 `ChatRequest`，新增 `StableSystem`、`DynamicSystem`、`Tools`、`Cache`。
5. 保留 `SystemPrompt` 和 `ToolDefs` 兼容字段，并明确新字段优先级高于旧字段。
6. 将 `Usage` 字段改为 `int64`，新增 `CacheCreationInputTokens`、`CacheReadInputTokens`。
7. 不把 Anthropic 私有字段放入通用 DTO。

**验证：** `go test ./internal/provider` 编译通过。

## T8: 增加 provider 兼容 helper

**文件：** `internal/provider/provider.go` 或 provider 内部 helper 文件
**依赖：** T7

**步骤：**
1. 实现 system blocks 兼容 helper：结构化字段非空时使用结构化字段，否则使用旧 `SystemPrompt`。
2. 实现工具定义兼容 helper：`Tools` 非空时使用快照，否则临时从旧 `ToolDefs` 生成。
3. 明确 helper 不导入 `internal/prompt`。
4. 增加旧字段不重复注入的测试入口。

**验证：** `go test ./internal/provider` 通过。

## T9: 验证 Anthropic SDK cache_control 类型

**文件：** `internal/provider/anthropic.go`、`internal/provider/tool_parse_test.go`
**依赖：** T7

**步骤：**
1. 用实际 SDK 类型编译验证 `anthropic.TextBlockParam.CacheControl`。
2. 用实际 SDK 类型编译验证 `anthropic.ToolParam.CacheControl`。
3. 用实际 SDK 构造函数验证 ephemeral cache control 创建方式。
4. 确认是否需要额外 beta header；如需要，记录并在 T13 实现。
5. 用最小单测防止字段名猜错。

**验证：** `go test ./internal/provider` 通过。

## T10: 实现工具定义快照和稳定排序

**文件：** `internal/orchestrator/chat.go` 或 `internal/orchestrator/tool_definitions.go`、必要时 `internal/tool/registry.go`
**依赖：** T7

**步骤：**
1. 在 orchestrator 层实现 registry → `[]provider.ToolDefinition` 转换。
2. 按工具名排序，生成不可变快照。
3. 保留 `tool.Registry.List()` 注册顺序行为，避免破坏执行逻辑。
4. 对 nil registry 返回空列表。
5. 不让 `internal/tool` 导入 `internal/provider`。

**验证：** `go test ./internal/orchestrator ./internal/tool` 编译通过。

## T11: 强化工具描述关键规则

**文件：** `internal/tool/read.go`、`write.go`、`edit.go`、`bash.go`、`glob.go`、`grep.go`
**依赖：** 无

**步骤：**
1. Read/Glob/Grep 描述中强调优先使用专用工具读取、搜索项目内容。
2. Write/Edit 描述中强调编辑或写入前应先读取相关文件。
3. Bash 描述中强调危险命令谨慎、优先使用专用工具。
4. 所有涉及路径的工具描述中强调路径限制在项目内。
5. 避免写成“主动执行危险操作”或“绕过确认”的语义。
6. 保持描述简洁，不写长篇策略文档。

**验证：** `go test ./internal/tool` 通过。

## T12: 补工具定义稳定性和描述测试

**文件：** `internal/tool/tool_test.go`、必要时 `internal/orchestrator/chat_test.go`
**依赖：** T10, T11

**步骤：**
1. 测试排序定义按工具名稳定输出。
2. 测试连续两次生成定义顺序一致。
3. 测试连续两次生成 provider 请求体中工具 JSON 字节一致。
4. 测试关键工具描述包含“读取”“专用工具”“项目内”等核心句义。
5. 测试危险工具描述不包含绕过确认或破坏性暗示。
6. 测试 schema 仍可正常 JSON marshal。

**验证：** `go test ./internal/tool ./internal/orchestrator` 通过。

## T13: 更新 Anthropic provider 的系统块映射

**文件：** `internal/provider/anthropic.go`
**依赖：** T7-T9

**步骤：**
1. 新增 `toAnthropicSystemBlocks(req ChatRequest)`。
2. 将 `StableSystem` blocks 转成 `anthropic.TextBlockParam`。
3. 将 `DynamicSystem` blocks 追加在 stable blocks 后。
4. 如果启用 cache 且存在 stable blocks，给最后一个 stable block 设置 `CacheControl`。
5. 兼容旧 `SystemPrompt`：结构化字段为空时生成一个 stable block。
6. 确保 dynamic blocks 不设置 cache control。
7. 确保 user messages、tool results、工具输出不进入 stable system blocks。

**验证：** `go test ./internal/provider` 通过。

## T14: 更新 Anthropic provider 的工具映射和 cache usage

**文件：** `internal/provider/anthropic.go`
**依赖：** T7-T10, T13

**步骤：**
1. 将 `toAnthropicTools` 参数从 registry 改为 `[]ToolDefinition`。
2. 映射 `Name`、`Description`、`Schema`。
3. 如果启用 cache 且工具非空，给最后一个工具设置 `CacheControl`。
4. `MessageDeltaEvent.Usage` 映射 input/output/cache creation/cache read 四类 token。
5. 如 SDK/API 需要 prompt caching beta header，补最小 request option；否则不加。
6. 不把 cache_control 放到 user/history/tool_result blocks。

**验证：** `go test ./internal/provider` 通过。

## T15: 更新 OpenAI provider 兼容映射

**文件：** `internal/provider/openai.go`
**依赖：** T7, T8

**步骤：**
1. 新增 system blocks 合并 helper。
2. OpenAI 请求首条 system message 包含 stable blocks 和 dynamic blocks，按顺序空行拼接。
3. 不发送 cache policy、cache_control、anthropic_beta 或其他 Anthropic 专属字段。
4. 将工具映射改为使用 `[]ToolDefinition`，兼容旧 `ToolDefs` 直到调用迁移完成。
5. usage 普通字段改为 int64，cache 字段保持 0。
6. 保持 `stream_options.include_usage=true`。

**验证：** `go test ./internal/provider` 通过。

## T16: 补 provider 请求形状和 cache 测试

**文件：** `internal/provider/tool_parse_test.go`
**依赖：** T13-T15

**步骤：**
1. 用 httptest 捕获 OpenAI 请求体，验证 system message 包含 stable 和 dynamic 内容。
2. 验证 OpenAI 请求体不包含 `cache_control`、`anthropic_beta`、Anthropic thinking 私有字段。
3. 增加 Anthropic helper 级测试，验证 stable blocks 在 dynamic blocks 前。
4. 验证最后一个 stable block 有 cache control。
5. 验证 dynamic blocks 没有 cache control。
6. 验证最后一个 tool definition 有 cache control。
7. 验证 Anthropic usage cache creation/read 字段解析。
8. 验证旧 `SystemPrompt` 兼容路径不会和新 blocks 重复注入。
9. 覆盖纯对话、带工具定义、tool_use/tool_result 历史、多工具调用场景的请求转换。

**验证：** `go test ./internal/provider` 通过。

## T17: Orchestrator 接入 prompt builder

**文件：** `internal/orchestrator/chat.go`
**依赖：** T6-T16

**步骤：**
1. 导入 `internal/prompt`。
2. 将 `stream` 签名改为接收 `iteration int`。
3. 在 `stream` 中构造 `prompt.BuildRequest`。
4. 将 `prompt.Bundle` 转为 `provider.SystemBlock`。
5. 构造按 mode 选择的工具定义快照。
6. 设置 `CachePolicy{EnablePromptCache: true}`。
7. 不再使用旧 `systemPrompt(mode)` 拼接模式说明。
8. 确保动态补充不调用 `conversation.Append*`。

**验证：** `go test ./internal/orchestrator` 编译通过。

## T18: Agent Loop 传递 iteration

**文件：** `internal/orchestrator/agent_loop.go`、`internal/orchestrator/chat.go`
**依赖：** T17

**步骤：**
1. `runAgentLoop` 调用 `stream` 时传入当前 iteration。
2. `finalReply` 等旧路径传入 iteration 1。
3. 确保所有 `stream` 调用点编译通过。

**验证：** `go test ./internal/orchestrator` 通过。

## T19: 清理旧 systemPrompt 模式拼接

**文件：** `internal/orchestrator/run_request.go`、必要时 `internal/resources/prompts.go`
**依赖：** T17, T18

**步骤：**
1. 删除 `systemPrompt(mode)` 或改为仅返回 legacy fallback。
2. 保留 `registryForMode(mode)` 行为。
3. 确保模式约束只来自 dynamic prompt blocks。
4. 更新旧 `resources.SystemPrompt()` 中“不支持 tool use”的过期描述，避免与现有工具能力冲突。

**验证：** `go test ./internal/orchestrator ./internal/resources` 编译通过。

## T20: 补 orchestrator 集成和安全边界测试

**文件：** `internal/orchestrator/chat_test.go`
**依赖：** T17-T19

**步骤：**
1. 更新捕获 provider 测试，从 `SystemPrompt` 改为检查 `StableSystem` 和 `DynamicSystem`。
2. 测试 `/plan` 同一请求内多次 provider 调用都包含 Plan Mode dynamic block。
3. 测试 iteration 1 和 iteration 3 动态内容符合完整/重复策略。
4. 测试 dynamic blocks 不进入 conversation history。
5. 测试 Plan Mode 下 Write/Edit/Bash 不暴露或被拒绝。
6. 测试用户输入中的“忽略系统提示”等文本不会进入 dynamic system blocks。
7. 测试工具结果中的伪系统指令只作为 tool result 历史，不进入 stable/dynamic system blocks。
8. 测试 stable blocks 不包含用户文本、工具输出、环境变量值等动态内容。

**验证：** `go test ./internal/orchestrator` 通过。

## T21: 扩展 usage 事件和 app 累计

**文件：** `internal/events/events.go`、`internal/orchestrator/stream_collector.go`、`internal/app/update.go`
**依赖：** T7

**步骤：**
1. `events.UsageDisplay` 增加四类 int64 字段。
2. `collectProviderStream` 转发四类 usage 字段。
3. `app.Update` 收到 usage 时累计四类字段。
4. 新请求提交时清零四类字段。
5. usage 展示事件不包含 prompt 片段、工具参数、用户原文或 provider 原始响应。

**验证：** `go test ./internal/orchestrator ./internal/app` 编译通过。

## T22: 更新 TUI status 展示

**文件：** `internal/tui/status.go`、`internal/tui/messages_test.go`
**依赖：** T21

**步骤：**
1. `Status` 增加 cache creation/read 字段并改 token 字段为 int64。
2. 普通 token 仍显示 `Tokens: X in / Y out`。
3. cache 字段非零时显示 `Cache: C create / R read`。
4. cache 字段全为 0 时不显示 Cache 段。
5. 测试状态栏不回显 prompt 内容、工具参数或用户原文。
6. 更新或新增状态栏测试。

**验证：** `go test ./internal/tui` 通过。

## T23: 补 usage 传递和重置测试

**文件：** `internal/orchestrator/chat_test.go`、必要时 `internal/app` 测试文件、`internal/tui/messages_test.go`
**依赖：** T21, T22

**步骤：**
1. 测试 provider usage 四类字段能传到 events。
2. 测试多次 provider 调用在同一用户请求内累计。
3. 测试新用户请求提交时 usage 清零。
4. 测试 OpenAI cache 字段为 0 时普通 usage 正常。
5. 测试 cache 字段为 0 时 TUI 不显示 Cache 段。

**验证：** `go test ./internal/orchestrator ./internal/app ./internal/tui` 通过。

## T24: 更新 smoke 脚本和 fake provider

**文件：** `.claude/agent_loop_smoke.go`、必要的测试 fake provider
**依赖：** T21-T23

**步骤：**
1. 修复因 `Usage` 字段 int64 或 `ChatRequest` 字段变化导致的 fake provider 编译问题。
2. 确保 smoke 仍覆盖多轮工具、Plan Mode 阻止写入、迭代上限。
3. 如 smoke provider 记录 ChatRequest，确保其不要求真实网络或真实 cache 命中。

**验证：** `go run .claude/agent_loop_smoke.go` 输出 `AGENT_LOOP_SMOKE_OK`。

## T25: 增加请求体快照/负向安全测试

**文件：** `internal/provider/tool_parse_test.go`、`internal/orchestrator/chat_test.go`、`internal/prompt/prompt_test.go`
**依赖：** T16, T20, T23

**步骤：**
1. 对相同 stable prompt + tools 连续构造请求体，断言稳定部分字节一致。
2. 对动态环境变化构造请求体，断言 stable 部分不变、dynamic 部分变化。
3. 测试用户粘贴 `.env` 或 token-like 文本不会进入 cacheable system blocks。
4. 测试工具输出包含伪 system 指令不会进入 system blocks。
5. 测试 cache 关闭时 Anthropic 请求不设置 cache_control。
6. 测试 provider 不支持 cache 时普通流式路径不失败。

**验证：** `go test ./internal/prompt ./internal/provider ./internal/orchestrator` 通过。

## T26: 运行分层回归

**文件：** 无
**依赖：** T1-T25

**步骤：**
1. 运行 `go test ./internal/prompt`。
2. 运行 `go test ./internal/provider`。
3. 运行 `go test ./internal/tool`。
4. 运行 `go test ./internal/orchestrator ./internal/app ./internal/tui`。
5. 修复失败后重复对应测试。

**验证：** 以上命令全部通过。

## T27: 运行全量回归和构建

**文件：** 无
**依赖：** T26

**步骤：**
1. 运行 `go test ./...`。
2. 运行 `go build ./cmd/xagent`。
3. 运行 `go run .claude/tool_smoke.go`。
4. 运行 `go run .claude/agent_loop_smoke.go`。
5. 运行 `git diff --check`。

**验证：** 所有命令通过，两个 smoke 分别输出 `TOOL_SMOKE_OK` 和 `AGENT_LOOP_SMOKE_OK`。

## T28: 准备 checklist 人工验收字段

**文件：** `docs/system-prompt/checklist.md`（下一阶段创建）
**依赖：** T25

**步骤：**
1. 记录 checklist 需要包含的字段：场景、输入、模式、fixture 文件、期望工具序列、禁止动作、期望最终输出特征、实际工具序列、是否触发确认、usage/cache 指标、通过/失败原因。
2. 记录必须覆盖的负向场景：Plan Mode 请求写入、用户 prompt injection、工具输出伪系统指令、密钥不进 cacheable prompt、动态补充特殊字符。
3. 本任务只为下一阶段 checklist 做准备，不提前创建 checklist 内容。

**验证：** checklist 阶段能直接使用这些字段生成验收清单。

## 执行顺序

```text
T1
 ↓
T2 → T3 → T4 → T5 → T6
                         ↓
                       T7 → T8 → T9
                         ↓
              T10 → T11 → T12
                         ↓
              T13 → T14 → T15 → T16
                         ↓
              T17 → T18 → T19 → T20
                         ↓
              T21 → T22 → T23 → T24
                         ↓
                    T25 → T26 → T27
                         ↓
                         T28
```

可并行项：

- T11 可与 T13-T15 并行，但必须在 T12/T16/T25 前完成。
- T21-T22 可在 provider 请求形状稳定后并行推进。
- T28 属于 checklist 准备项，不阻塞代码实现，但必须在 checklist 阶段前完成。
