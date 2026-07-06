# Context Management Plan

## Milestone A：配置与会话结构

- 新增 `config.ContextConfig`。
- 给上下文管理配置补默认值和校验。
- 扩展 `conversation.Message`，记录外置状态、路径、大小和预览。
- 扩展 `conversation.Conversation`，记录摘要、边界、估算锚点和摘要失败次数。
- `ContextMessages` 输出模型安全内容：摘要、边界、外置预览和重新读取提示。

## Milestone B：轻量预防压缩

- 新增 `internal/contextmgr.Manager`。
- 实现单个大型工具结果外置。
- 实现多个工具结果合计超阈值时按大小外置。
- 外置结果保存完整结构化 JSON。
- 外置操作幂等。

## Milestone C：重量兜底摘要

- 基于 usage 锚点和字符数增量估算上下文 token。
- 自动触发使用 13K 安全余量。
- 手动触发使用 3K 安全余量。
- 摘要请求不携带工具定义。
- 摘要输出固定结构。
- 摘要成功后插入摘要消息和边界消息。
- 连续摘要失败 3 次后熔断。

## Milestone D：请求链路接入

- 在 `orchestrator.stream` 构造 `ChatRequest` 前调用 `contextmgr.Prepare`。
- 如果压缩改变会话，立即保存。
- provider usage 回传后更新估算锚点。
- `finalReply` 和 Agent Loop 共享同一接入点。

## Milestone E：手动压缩入口

- 在 app 输入层识别 `/compact`。
- `/compact` 不发送给模型。
- 成功后刷新当前消息视图并显示状态提示。
- 失败后显示错误。

## Milestone F：验证

- 单元测试覆盖外置、摘要、熔断、上下文输出、provider 角色映射和手动入口。
- 全量运行 `go test ./...`。
- 构建 `go build ./cmd/xagent`。
- tmux 端到端验证长工具结果外置和 `/compact`。
