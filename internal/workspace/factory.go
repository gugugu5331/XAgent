package workspace

import (
	"context"
	"errors"
	"sync"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/tool"
	"xagent/internal/worktree"
)

type Factory interface {
	Bind(context.Context, BindRequest) (Runtime, error)
	Close(context.Context) error
}

type FactoryOptions struct {
	SourceRegistry   *tool.Registry
	ResultFactory    *tool.ResultFactory
	ReadCacheLimits  tool.ReadCacheLimits
	BackgroundPolicy tool.BackgroundPolicy
	GlobalDenied     map[string]struct{}
	ExecutorTimeout  time.Duration
	MaxOutputBytes   int
	ContextFactory   *ContextFactory
	HookFactory      *hook.WorkspaceFactory
}

type BindRequest struct {
	TaskID        string
	Isolation     agentrole.IsolationMode
	Root          string
	ScratchRoot   string
	ArtifactRoot  string
	ReadonlyRoots []string
	Lease         worktree.Lease
	Role          *agentrole.Definition
	PlanMode      bool
	Verifier      permission.TicketVerifier
}

type workspaceFactory struct {
	mu sync.Mutex

	options       FactoryOptions
	contextBinder contextBinder
	hookBinder    hookBinder
	accepting     bool
	runtimes      map[string]runtimeRegistration
	activeBind    uint64
	bindIdle      chan struct{}

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

type registrationToken struct{ identity byte }

type runtimeRegistration struct {
	token   *registrationToken
	runtime Runtime
	closed  bool
}

func NewFactory(options FactoryOptions) (Factory, error) {
	if options.SourceRegistry == nil || !options.SourceRegistry.IsSealed() || options.ResultFactory == nil ||
		options.ExecutorTimeout <= 0 || options.MaxOutputBytes <= 0 ||
		options.ContextFactory == nil || options.HookFactory == nil {
		return nil, ErrRuntimeInvalid
	}
	probe, err := tool.NewReadCache(options.ReadCacheLimits, options.ResultFactory)
	if err != nil {
		return nil, ErrRuntimeInvalid
	}
	probe.Close()
	options.GlobalDenied = cloneDeniedTools(options.GlobalDenied)
	options.BackgroundPolicy.AllowedNames = append([]string(nil), options.BackgroundPolicy.AllowedNames...)
	idle := make(chan struct{})
	close(idle)
	return &workspaceFactory{
		options: options, accepting: true, runtimes: make(map[string]runtimeRegistration),
		contextBinder: options.ContextFactory, hookBinder: &workspaceHookBinder{factory: options.HookFactory},
		bindIdle: idle, closeDone: make(chan struct{}),
	}, nil
}

func cloneDeniedTools(source map[string]struct{}) map[string]struct{} {
	if source == nil {
		return nil
	}
	clone := make(map[string]struct{}, len(source))
	for name := range source {
		clone[name] = struct{}{}
	}
	return clone
}

func (f *workspaceFactory) Bind(ctx context.Context, request BindRequest) (Runtime, error) {
	if f == nil || ctx == nil || ctx.Err() != nil || request.TaskID == "" || request.Isolation != agentrole.IsolationWorktree {
		return nil, ErrRuntimeInvalid
	}
	token, ok := f.beginBind(request.TaskID)
	if !ok {
		return nil, ErrRuntimeUnavailable
	}
	completed := false
	request = cloneBindRequest(request)
	defer func() {
		if !completed {
			f.finishBind(request.TaskID, token, nil)
		}
	}()
	if !validRuntimeLease(request.Lease, request.Root) {
		return nil, ErrRuntimeInvalid
	}

	protection, err := OpenProtection(ProtectionPaths{
		Working: request.Root, Scratch: request.ScratchRoot, Artifact: request.ArtifactRoot,
		Readonly: request.ReadonlyRoots,
	})
	if err != nil {
		return nil, err
	}
	binding, err := bindTaskTools(f.options, request, protection)
	if err != nil {
		_ = protection.Close()
		return nil, err
	}
	boundContext, err := f.contextBinder.Bind(ctx, ContextBindRequest{
		Root: request.Root, WorkspaceID: request.Lease.WorkspaceID, WorkingDirectory: protection.Working().Root,
	})
	if err != nil || boundContext == nil {
		if err == nil {
			err = ErrRuntimeInvalid
		}
		var contextCloseErr error
		if boundContext != nil {
			contextCloseErr = boundContext.Close()
		}
		binding.cache.Close()
		return nil, errors.Join(err, contextCloseErr, protection.Close())
	}
	boundHooks, err := f.hookBinder.Bind(ctx, HookBindRequest{
		Root: request.Root, WorkspaceID: request.Lease.WorkspaceID, WorkingDirectory: protection.Working().Root,
		Plans: protection.ProcessPlans(),
	})
	if err != nil || boundHooks == nil {
		if err == nil {
			err = ErrRuntimeInvalid
		}
		var hookCloseErr error
		if boundHooks != nil {
			hookCloseErr = boundHooks.Close(context.WithoutCancel(ctx))
		}
		contextCloseErr := boundContext.Close()
		binding.cache.Close()
		return nil, errors.Join(err, hookCloseErr, contextCloseErr, protection.Close())
	}
	runtime, err := NewRuntime(ctx, RuntimeOptions{
		Lease: request.Lease, Protection: protection,
		Resources: []Resource{
			{Name: "read-cache", Close: func(context.Context) error { binding.cache.Close(); return nil }},
			{Name: "session-context", Close: func(context.Context) error { return boundContext.Close() }},
			{Name: "hooks", Close: boundHooks.Close},
		},
		Registry: binding.registry, Tools: binding.base, Executor: binding.scoped,
		Capabilities: binding.capabilities, BindingReport: binding.report,
		Hooks: boundHooks, SessionContext: boundContext,
		OnClosed: func() {
			f.unregister(request.TaskID, token)
		},
	})
	if err != nil {
		return nil, err
	}
	if !f.finishBind(request.TaskID, token, runtime) {
		_ = runtime.Close(context.Background())
		f.finishBind(request.TaskID, token, nil)
		completed = true
		return nil, ErrRuntimeUnavailable
	}
	completed = true
	return runtime, nil
}

func cloneBindRequest(source BindRequest) BindRequest {
	clone := source
	clone.ReadonlyRoots = append([]string(nil), source.ReadonlyRoots...)
	if source.Role != nil {
		role := *source.Role
		role.ToolAllow = append([]string(nil), source.Role.ToolAllow...)
		role.ToolDeny = append([]string(nil), source.Role.ToolDeny...)
		if source.Role.MaxIterations != nil {
			iterations := *source.Role.MaxIterations
			role.MaxIterations = &iterations
		}
		clone.Role = &role
	}
	return clone
}

func (f *workspaceFactory) beginBind(taskID string) (*registrationToken, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.accepting || taskID == "" {
		return nil, false
	}
	if _, exists := f.runtimes[taskID]; exists {
		return nil, false
	}
	if f.activeBind == 0 {
		f.bindIdle = make(chan struct{})
	}
	f.activeBind++
	token := &registrationToken{}
	f.runtimes[taskID] = runtimeRegistration{token: token}
	return token, true
}

func (f *workspaceFactory) finishBind(taskID string, token *registrationToken, runtime Runtime) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	registration, exists := f.runtimes[taskID]
	if !exists || registration.token != token {
		return false
	}
	if runtime != nil && registration.closed {
		if f.activeBind > 0 {
			f.activeBind--
			if f.activeBind == 0 {
				close(f.bindIdle)
			}
		}
		delete(f.runtimes, taskID)
		return false
	}
	// Factory 已停止准入时，调用方必须先完整关闭刚构造的 Runtime，再以 nil
	// 再次进入这里释放 activeBind；否则 Factory.Close 会早于回滚完成返回。
	if runtime != nil && !f.accepting {
		return false
	}
	if f.activeBind > 0 {
		f.activeBind--
		if f.activeBind == 0 {
			close(f.bindIdle)
		}
	}
	if runtime == nil {
		delete(f.runtimes, taskID)
		return false
	}
	registration.runtime = runtime
	f.runtimes[taskID] = registration
	return true
}

func (f *workspaceFactory) unregister(taskID string, token *registrationToken) {
	if f == nil || token == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	registration, exists := f.runtimes[taskID]
	if exists && registration.token == token {
		if registration.runtime == nil {
			registration.closed = true
			f.runtimes[taskID] = registration
		} else {
			delete(f.runtimes, taskID)
		}
	}
}

func (f *workspaceFactory) Close(ctx context.Context) error {
	if f == nil {
		return nil
	}
	if ctx == nil {
		return ErrRuntimeInvalid
	}
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.accepting = false
		idle := f.bindIdle
		for _, registration := range f.runtimes {
			if registration.runtime != nil {
				registration.runtime.StopAccepting()
			}
		}
		f.mu.Unlock()
		// Caller context bounds only this invocation. Factory-owned cleanup must
		// wait for child Runtime settlement even after the first caller times out.
		go f.closeOwned(context.Background(), idle)
	})
	select {
	case <-f.closeDone:
		return f.closeErr
	default:
	}
	select {
	case <-f.closeDone:
		return f.closeErr
	case <-ctx.Done():
		select {
		case <-f.closeDone:
			return f.closeErr
		default:
			return ctx.Err()
		}
	}
}

func (f *workspaceFactory) closeOwned(ctx context.Context, bindIdle <-chan struct{}) {
	defer close(f.closeDone)
	// Close 的 caller 可以有界返回，但 owner cleanup 必须继续等待正在构造的
	// Bind 退出；否则 closeOnce 已消耗后，这些资源将永远没有第二个回收者。
	<-bindIdle
	f.mu.Lock()
	runtimes := make([]Runtime, 0, len(f.runtimes))
	for _, registration := range f.runtimes {
		if registration.runtime != nil {
			runtimes = append(runtimes, registration.runtime)
		}
	}
	f.runtimes = make(map[string]runtimeRegistration)
	f.mu.Unlock()
	var failed bool
	for index := len(runtimes) - 1; index >= 0; index-- {
		if err := runtimes[index].Close(ctx); err != nil {
			failed = true
		}
	}
	if failed {
		f.closeErr = errors.New("workspace factory close failed")
	}
}
