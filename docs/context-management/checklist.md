# Context Management Checklist

## 自动压缩

- [x] 每次 API 请求前先执行轻量压缩。
- [x] 单个大型工具结果会外置到磁盘。
- [x] 多个工具结果合计超阈值时会逐个外置。
- [x] 会话上下文只保留外置预览和路径。
- [x] 外置消息重复压缩不会重复写入。

## 重量摘要

- [x] 上下文估算接近窗口时触发摘要。
- [x] 摘要请求不带工具定义。
- [x] 摘要输出按固定结构保存。
- [x] 摘要后保留近期原文消息。
- [x] 摘要后插入边界消息，提醒重新读取细节。
- [x] 连续失败达到限制后熔断。

## 手动压缩

- [x] `/compact` 被本地处理，不发送给模型。
- [x] 无会话时显示无需压缩。
- [x] 成功后刷新消息视图。
- [x] 失败后显示错误。

## 安全与正确性

- [x] 用户原始消息不会被轻量压缩改写。
- [x] 外置文件使用私有目录/文件权限。
- [x] 摘要边界明确禁止根据摘要脑补代码。
- [x] provider 可接收 context summary/boundary 角色。

## 验证命令

- [x] `go test ./internal/contextmgr`
- [x] `go test ./internal/conversation ./internal/provider ./internal/orchestrator ./internal/app ./internal/tui`
- [x] `go test ./...`
- [x] `go build ./cmd/xagent`
- [x] tmux 启动 XAgent，验证长工具结果外置和 `/compact`
