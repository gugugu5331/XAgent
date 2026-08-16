package workspace

import (
	"errors"

	"xagent/internal/tool"
)

type taskToolBinding struct {
	registry     *tool.Registry
	base         *tool.Executor
	scoped       *tool.ScopedExecutor
	capabilities *tool.CapabilitySwitch
	cache        *tool.ReadCache
	report       tool.WorkspaceBindingReport
}

// bindTaskTools 的安全顺序固定为：先按 Workspace 重建执行对象，再做角色和
// placement 的单调过滤，最后才构造 task base/scoped executor。Bash 是否可信
// 完全由 source Registry 的本地 WorkspacePolicy 和 binder 决定；这里不制造或
// 提升 Bash capability。
func bindTaskTools(options FactoryOptions, request BindRequest, protection *Protection) (taskToolBinding, error) {
	if protection == nil {
		return taskToolBinding{}, ErrRuntimeInvalid
	}
	binding := tool.WorkspaceBinding{
		WorkspaceID: request.Lease.WorkspaceID,
		Root:        request.Root,
		ScratchRoot: request.ScratchRoot,
	}
	registry, report, err := tool.BindWorkspaceRegistry(options.SourceRegistry, binding)
	if err != nil {
		return taskToolBinding{}, errors.New("workspace tool binding failed")
	}
	foreground, background, err := tool.BuildCapabilityViews(
		registry,
		request.Role,
		options.BackgroundPolicy,
		options.GlobalDenied,
		request.PlanMode,
	)
	if err != nil {
		return taskToolBinding{}, errors.New("workspace capability views failed")
	}
	capabilities, err := tool.NewCapabilitySwitch(foreground, background)
	if err != nil {
		return taskToolBinding{}, errors.New("workspace capability switch failed")
	}
	working := protection.Working()
	base, err := tool.NewExecutorWithWriteAccessAndResultFactory(
		registry,
		request.Root,
		options.ExecutorTimeout,
		options.MaxOutputBytes,
		working.Root,
		working.Capabilities.Ordinary(),
		options.ResultFactory,
	)
	if err != nil {
		return taskToolBinding{}, errors.New("workspace tool executor failed")
	}
	cache, err := tool.NewReadCache(options.ReadCacheLimits, options.ResultFactory)
	if err != nil {
		return taskToolBinding{}, errors.New("workspace read cache failed")
	}
	scoped, err := tool.NewScopedExecutor(base, request.Verifier, request.TaskID, capabilities, cache)
	if err != nil {
		cache.Close()
		return taskToolBinding{}, errors.New("workspace scoped executor failed")
	}
	return taskToolBinding{
		registry: registry, base: base, scoped: scoped, capabilities: capabilities,
		cache: cache, report: cloneWorkspaceBindingReport(report),
	}, nil
}
