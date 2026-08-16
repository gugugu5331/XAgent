package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"xagent/internal/hook"
	"xagent/internal/safefs"
	"xagent/internal/sessionctx"
	"xagent/internal/tool"
	"xagent/internal/worktree"
)

type runtimeState uint8

const (
	runtimeAccepting runtimeState = iota + 1
	runtimeStopping
	runtimeClosed
)

type taskRuntime struct {
	mu sync.Mutex

	lease         worktree.Lease
	protection    *Protection
	resources     []Resource
	registry      *tool.Registry
	tools         *tool.Executor
	executor      *tool.ScopedExecutor
	capabilities  *tool.CapabilitySwitch
	bindingReport tool.WorkspaceBindingReport
	hookView      hook.Runtime
	contextView   sessionctx.StablePreparer
	onClosed      func()
	ctx           context.Context
	cancel        context.CancelFunc
	state         runtimeState
	active        uint64
	idle          chan struct{}

	stopOnce  sync.Once
	stopDone  chan struct{}
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func NewRuntime(parent context.Context, options RuntimeOptions) (Runtime, error) {
	if parent == nil || options.Protection == nil || !validRuntimeLease(options.Lease, options.Protection.Working().Path) {
		rollbackRuntimeOptions(options)
		return nil, ErrRuntimeInvalid
	}
	if err := parent.Err(); err != nil {
		rollbackRuntimeOptions(options)
		return nil, err
	}
	if err := options.Protection.Verify(); err != nil {
		rollbackRuntimeOptions(options)
		return nil, ErrRuntimeInvalid
	}
	resources := append([]Resource(nil), options.Resources...)
	for _, resource := range resources {
		if resource.Close == nil || resource.Name == "" || !utf8.ValidString(resource.Name) {
			rollbackRuntimeOptions(options)
			return nil, ErrRuntimeInvalid
		}
	}
	idle := make(chan struct{})
	close(idle)
	ctx, cancel := context.WithCancel(parent)
	runtime := &taskRuntime{
		lease:         options.Lease,
		protection:    options.Protection,
		resources:     resources,
		registry:      options.Registry,
		tools:         options.Tools,
		executor:      options.Executor,
		capabilities:  options.Capabilities,
		bindingReport: cloneWorkspaceBindingReport(options.BindingReport),
		onClosed:      options.OnClosed,
		ctx:           ctx,
		cancel:        cancel,
		state:         runtimeAccepting,
		idle:          idle,
		stopDone:      make(chan struct{}),
		closeDone:     make(chan struct{}),
	}
	runtime.hookView = admittedHookRuntime{runtime: runtime, target: options.Hooks}
	runtime.contextView = admittedStablePreparer{runtime: runtime, target: options.SessionContext}
	context.AfterFunc(ctx, runtime.StopAccepting)
	return runtime, nil
}

func rollbackRuntimeOptions(options RuntimeOptions) {
	for index := len(options.Resources) - 1; index >= 0; index-- {
		if options.Resources[index].Stop != nil {
			options.Resources[index].Stop()
		}
		if options.Resources[index].Close != nil {
			_ = options.Resources[index].Close(context.Background())
		}
	}
	if options.Protection != nil {
		_ = options.Protection.Close()
	}
}

func validRuntimeLease(lease worktree.Lease, root string) bool {
	return worktree.ValidWorkspaceID(lease.WorkspaceID) && worktree.ValidWorkspaceID(lease.OwnerID) &&
		lease.Root == root && filepath.IsAbs(lease.Root) && filepath.Clean(lease.Root) == lease.Root &&
		lease.Branch == "xagent/worktree/"+lease.WorkspaceID && validOID(lease.BaseOID) && !lease.AcquiredAt.IsZero()
}

func validOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func (r *taskRuntime) Root() string {
	if r == nil || r.protection == nil {
		return ""
	}
	return r.protection.Working().Path
}

func (r *taskRuntime) ScratchRoot() string {
	if r == nil || r.protection == nil {
		return ""
	}
	return r.protection.Scratch().Path
}

func (r *taskRuntime) ArtifactRoot() string {
	if r == nil || r.protection == nil {
		return ""
	}
	return r.protection.Artifact().Path
}

func (r *taskRuntime) RootHandle() *safefs.Root {
	if r == nil || r.protection == nil {
		return nil
	}
	return r.protection.Working().Root
}

func (r *taskRuntime) Lease() worktree.Lease {
	if r == nil {
		return worktree.Lease{}
	}
	return r.lease
}

func (r *taskRuntime) Protection() *Protection {
	if r == nil {
		return nil
	}
	return r.protection
}

func (r *taskRuntime) Registry() *tool.Registry {
	if r == nil {
		return nil
	}
	return r.registry
}

func (r *taskRuntime) ToolExecutor() *tool.Executor {
	if r == nil {
		return nil
	}
	return r.tools
}

func (r *taskRuntime) Executor() *tool.ScopedExecutor {
	if r == nil {
		return nil
	}
	return r.executor
}

func (r *taskRuntime) Capabilities() *tool.CapabilitySwitch {
	if r == nil {
		return nil
	}
	return r.capabilities
}

func (r *taskRuntime) ToolBindingReport() tool.WorkspaceBindingReport {
	if r == nil {
		return tool.WorkspaceBindingReport{}
	}
	return cloneWorkspaceBindingReport(r.bindingReport)
}

func (r *taskRuntime) Hooks() hook.Runtime {
	if r == nil || r.hookView == nil {
		return hook.Noop()
	}
	return r.hookView
}

func (r *taskRuntime) SessionContext() sessionctx.StablePreparer {
	if r == nil || r.contextView == nil {
		return admittedStablePreparer{}
	}
	return r.contextView
}

func cloneWorkspaceBindingReport(report tool.WorkspaceBindingReport) tool.WorkspaceBindingReport {
	clone := tool.WorkspaceBindingReport{Filtered: make(map[string]tool.WorkspaceFilterReason, len(report.Filtered))}
	for name, reason := range report.Filtered {
		clone.Filtered[name] = reason
	}
	return clone
}

func (r *taskRuntime) Context() context.Context {
	if r == nil || r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

func (r *taskRuntime) BeginCall(parent context.Context) (context.Context, func(), error) {
	if r == nil || parent == nil {
		return nil, func() {}, ErrRuntimeUnavailable
	}
	if err := parent.Err(); err != nil {
		return nil, func() {}, err
	}
	if r.ctx.Err() != nil {
		r.StopAccepting()
		return nil, func() {}, ErrRuntimeUnavailable
	}
	r.mu.Lock()
	if r.state != runtimeAccepting || r.active == ^uint64(0) || r.ctx.Err() != nil {
		r.mu.Unlock()
		r.StopAccepting()
		return nil, func() {}, ErrRuntimeUnavailable
	}
	if r.active == 0 {
		r.idle = make(chan struct{})
	}
	r.active++
	r.mu.Unlock()

	callContext, cancel := context.WithCancel(parent)
	stopCancellation := context.AfterFunc(r.ctx, cancel)
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			stopCancellation()
			cancel()
			r.releaseCall()
		})
	}
	if err := r.protection.Verify(); err != nil {
		release()
		r.StopAccepting()
		return nil, func() {}, ErrRuntimeUnavailable
	}
	return callContext, release, nil
}

func (r *taskRuntime) releaseCall() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == 0 {
		return
	}
	r.active--
	if r.active == 0 {
		close(r.idle)
	}
}

func (r *taskRuntime) StopAccepting() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.mu.Lock()
		if r.state == runtimeAccepting {
			r.state = runtimeStopping
		}
		r.mu.Unlock()
		r.cancel()
		// Stop callbacks are owner cleanup, not part of the caller's synchronous
		// admission transition. A callback has no context contract and may block;
		// running it here would let it defeat every bounded Close caller.
		go func() {
			defer close(r.stopDone)
			for index := len(r.resources) - 1; index >= 0; index-- {
				if r.resources[index].Stop != nil {
					r.resources[index].Stop()
				}
			}
		}()
	})
}

func (r *taskRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return ErrRuntimeInvalid
	}
	r.StopAccepting()
	r.closeOnce.Do(func() {
		// Caller cancellation bounds only this wait. Owner cleanup must keep its
		// own lifetime so a timed-out Hook/resource cannot outlive borrowed roots.
		go r.closeOwned(context.Background())
	})
	select {
	case <-r.closeDone:
		return r.closeErr
	default:
	}
	select {
	case <-r.closeDone:
		return r.closeErr
	case <-ctx.Done():
		select {
		case <-r.closeDone:
			return r.closeErr
		default:
			return ctx.Err()
		}
	}
}

func (r *taskRuntime) closeOwned(closeContext context.Context) {
	defer func() {
		if r.onClosed != nil {
			r.onClosed()
		}
		close(r.closeDone)
	}()
	<-r.stopDone
	r.mu.Lock()
	idle := r.idle
	r.mu.Unlock()
	<-idle

	failed := false
	for index := len(r.resources) - 1; index >= 0; index-- {
		if err := r.resources[index].Close(closeContext); err != nil {
			failed = true
		}
	}
	if err := r.protection.Close(); err != nil {
		failed = true
	}
	r.mu.Lock()
	r.state = runtimeClosed
	r.mu.Unlock()
	if failed {
		r.closeErr = errors.New("workspace runtime close failed")
	}
}

type admittedStablePreparer struct {
	runtime *taskRuntime
	target  sessionctx.StablePreparer
}

func (p admittedStablePreparer) PrepareStable(ctx context.Context) sessionctx.PreparedContext {
	if p.runtime == nil || p.target == nil {
		return sessionctx.PreparedContext{}
	}
	callContext, release, err := p.runtime.BeginCall(ctx)
	if err != nil {
		return sessionctx.PreparedContext{}
	}
	defer release()
	return p.target.PrepareStable(callContext)
}

type admittedHookRuntime struct {
	runtime *taskRuntime
	target  hook.Runtime
}

func (h admittedHookRuntime) admit(ctx context.Context) (context.Context, func(), bool) {
	if h.runtime == nil || h.target == nil {
		return nil, func() {}, false
	}
	callContext, release, err := h.runtime.BeginCall(ctx)
	return callContext, release, err == nil
}

func (h admittedHookRuntime) SystemStart(ctx context.Context) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return
	}
	defer release()
	h.target.SystemStart(callContext)
}

func (h admittedHookRuntime) Shutdown(ctx context.Context) error {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return ErrRuntimeUnavailable
	}
	defer release()
	return h.target.Shutdown(callContext)
}

func (h admittedHookRuntime) SessionStart(ctx context.Context, id string, state hook.SessionState) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return
	}
	defer release()
	h.target.SessionStart(callContext, id, state)
}

func (h admittedHookRuntime) SessionEnd(ctx context.Context, id string, reason hook.SessionEndReason) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return
	}
	defer release()
	h.target.SessionEnd(callContext, id, reason)
}

func (h admittedHookRuntime) BeginTurn(ctx context.Context, id string, kind hook.ExecutionKind, mode hook.HookMode) hook.ExecutionRef {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return hook.ExecutionRef{}
	}
	defer release()
	return h.target.BeginTurn(callContext, id, kind, mode)
}

func (h admittedHookRuntime) EndTurn(ctx context.Context, ref hook.ExecutionRef, status hook.TurnStatus, message string) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return
	}
	defer release()
	h.target.EndTurn(callContext, ref, status, message)
}

func (h admittedHookRuntime) BeginMessage(ctx context.Context, ref hook.ExecutionRef, role hook.MessageRole, content string) hook.MessageToken {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return hook.MessageToken{}
	}
	defer release()
	return h.target.BeginMessage(callContext, ref, role, content)
}

func (h admittedHookRuntime) EndMessage(ctx context.Context, token hook.MessageToken) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return
	}
	defer release()
	h.target.EndMessage(callContext, token)
}

func (h admittedHookRuntime) BeforeTool(ctx context.Context, ref hook.ExecutionRef, input hook.ToolInput) hook.ToolDecision {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return hook.Deny("workspace runtime unavailable")
	}
	defer release()
	return h.target.BeforeTool(callContext, ref, input)
}

func (h admittedHookRuntime) AfterTool(ctx context.Context, ref hook.ExecutionRef, input hook.ToolInput, output hook.ToolOutput, elapsed time.Duration) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return
	}
	defer release()
	h.target.AfterTool(callContext, ref, input, output, elapsed)
}

func (h admittedHookRuntime) BeforeCompact(ctx context.Context, binding hook.CompactBinding, input hook.CompactInput) hook.CompactToken {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return hook.CompactToken{}
	}
	defer release()
	return h.target.BeforeCompact(callContext, binding, input)
}

func (h admittedHookRuntime) AfterCompact(ctx context.Context, token hook.CompactToken, output hook.CompactOutput) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return
	}
	defer release()
	h.target.AfterCompact(callContext, token, output)
}

func (h admittedHookRuntime) AcquirePrompts(ctx context.Context, ref hook.ExecutionRef) (hook.PromptLease, error) {
	callContext, release, ok := h.admit(ctx)
	if !ok {
		return nil, ErrRuntimeUnavailable
	}
	defer release()
	return h.target.AcquirePrompts(callContext, ref)
}

var _ hook.Runtime = admittedHookRuntime{}
var _ sessionctx.StablePreparer = admittedStablePreparer{}
