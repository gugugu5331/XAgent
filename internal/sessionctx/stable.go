package sessionctx

import "context"

// StablePreparer 为一个工作区加载稳定的项目级和用户级上下文。
// 实现不得修改或压缩父会话。
type StablePreparer interface {
	PrepareStable(context.Context) PreparedContext
}
