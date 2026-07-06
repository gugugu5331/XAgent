# Context Management Tasks

## A. 配置与会话结构

- [x] 新增 `ContextConfig`。
- [x] 增加默认阈值、模型窗口、安全余量和熔断次数。
- [x] 校验上下文管理配置。
- [x] 扩展消息外置字段。
- [x] 扩展会话上下文元数据。
- [x] 增加摘要和边界消息角色。

## B. 轻量预防压缩

- [x] 单个工具结果超过阈值时外置。
- [x] 多个工具结果合计超过阈值时外置。
- [x] 外置完整结构化工具结果 JSON。
- [x] 会话内保留预览、路径、大小和重新读取提示。
- [x] 已外置消息重复压缩时保持幂等。

## C. 重量兜底摘要

- [x] 近似估算上下文 token。
- [x] 支持自动触发阈值。
- [x] 支持手动 `/compact` 触发。
- [x] 摘要请求禁止工具。
- [x] 摘要成功后插入结构化摘要和边界消息。
- [x] 摘要失败计数和熔断。

## D. 请求链路接入

- [x] 每次 provider 请求前运行轻量压缩和必要摘要。
- [x] 压缩改变会话后立即保存。
- [x] usage 回写估算锚点。
- [x] Agent Loop 和 final reply 共享接入。

## E. 手动入口与 UI

- [x] app 输入层识别 `/compact`。
- [x] `/compact` 不发送给 LLM。
- [x] 成功后刷新消息视图。
- [x] 成功/失败后显示状态反馈。

## F. 验证

- [x] `go test ./internal/contextmgr`
- [x] `go test ./internal/conversation ./internal/provider ./internal/orchestrator ./internal/app ./internal/tui`
- [x] `go test ./...`
- [x] `go build ./cmd/xagent`
- [x] tmux 端到端验证
