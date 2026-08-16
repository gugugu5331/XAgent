package workspace

import (
	"context"
	"errors"

	"xagent/internal/hook"
	"xagent/internal/safefs"
	"xagent/internal/sessionctx"
	"xagent/internal/tool"
	"xagent/internal/worktree"
)

var (
	ErrRuntimeInvalid     = errors.New("workspace runtime configuration is invalid")
	ErrRuntimeUnavailable = errors.New("workspace runtime is not accepting calls")
	ErrProtectionInvalid  = errors.New("workspace protection is invalid")
)

// ProtectionPaths 是一个任务的全部绝对文件系统边界。Working、Scratch 和
// Artifact 可写；Readonly 中的主工作区、其他 Worktree 和共享依赖只读。
type ProtectionPaths struct {
	Working  string
	Scratch  string
	Artifact string
	Readonly []string
}

// WritableRoot 把已打开的目录句柄和仅对当前保护计划有效的 capability 绑定。
type WritableRoot struct {
	Path         string
	Root         *safefs.Root
	Capabilities safefs.Capabilities
}

// ReadonlyRoot 只公开读句柄，不提供写 capability。
type ReadonlyRoot struct {
	Path string
	Root *safefs.Root
}

// Resource 是 Runtime 所有的任务级对象。资源按注册顺序创建，并按逆序停止和
// 关闭。Stop 可省略；Close 必须提供且必须遵守传入的 context。
type Resource struct {
	Name  string
	Stop  func()
	Close func(context.Context) error
}

type RuntimeOptions struct {
	Lease          worktree.Lease
	Protection     *Protection
	Resources      []Resource
	Registry       *tool.Registry
	Tools          *tool.Executor
	Executor       *tool.ScopedExecutor
	Capabilities   *tool.CapabilitySwitch
	BindingReport  tool.WorkspaceBindingReport
	Hooks          hook.Runtime
	SessionContext sessionctx.StablePreparer
	OnClosed       func()
}

// Runtime 是任务级生命周期边界。调用方必须通过 BeginCall 登记所有可能访问
// 工作区的在途操作；Close 会先停止准入和取消调用，再等待全部 release。
type Runtime interface {
	Root() string
	ScratchRoot() string
	ArtifactRoot() string
	RootHandle() *safefs.Root
	Lease() worktree.Lease
	Protection() *Protection
	Registry() *tool.Registry
	ToolExecutor() *tool.Executor
	Executor() *tool.ScopedExecutor
	Capabilities() *tool.CapabilitySwitch
	ToolBindingReport() tool.WorkspaceBindingReport
	Hooks() hook.Runtime
	SessionContext() sessionctx.StablePreparer
	Context() context.Context
	BeginCall(context.Context) (context.Context, func(), error)
	StopAccepting()
	Close(context.Context) error
}
