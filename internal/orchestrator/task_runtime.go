package orchestrator

import (
	"context"
	"errors"
	"math"
	"sync"

	"xagent/internal/agentrole"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

// RuntimeProfile is the immutable-by-convention execution policy captured for
// one child task. It deliberately does not embed skill.ExecutionProfile: a
// child never inherits mutable Skill Activity from its parent.
type RuntimeProfile struct {
	Model                    string
	PermissionMode           permission.Mode
	PlanMode                 bool
	Depth                    int
	ReadRoots                []string
	Persist                  bool
	UpdateMemory             bool
	MaxUnknownToolCalls      int
	ForegroundTools          tool.CapabilitySet
	BackgroundTools          tool.CapabilitySet
	MaxIterations            int
	MaxRequestBytes          int64
	MaxRequestPlanningTokens int64
}

// CancelBridge owns the optional propagation edge from a foreground parent
// request into a task. Detach removes only that edge; it does not cancel the
// task-owned context.
type CancelBridge interface {
	Detach() bool
	Close()
}

// CapabilitySwitch is the narrow placement boundary stored in TaskRuntimeState.
type CapabilitySwitch interface {
	tool.CapabilitySource
	MoveToBackground() (changed bool, current tool.CapabilitySet)
}

// TaskRuntimeState contains every mutable execution dependency owned by one
// subagent task. Provider, Hook runtime, filesystem metadata and the sealed
// base registry may be shared by the runner factory; these fields may not.
type TaskRuntimeState struct {
	TaskID          subagent.ID
	Parent          subagent.ParentRef
	Role            *agentrole.ResolvedRole
	Conversation    *conversation.Conversation
	Profile         RuntimeProfile
	ActiveTools     CapabilitySwitch
	Authorizer      *permission.Authorizer
	Executor        *tool.ScopedExecutor
	Confirm         subagent.ConfirmationBroker
	ReadCache       *tool.ReadCache
	Usage           provider.Usage
	Iteration       int
	Prompt          provider.PromptPrefixSnapshot
	RequestBudgeter contextmgr.RequestBudgeter
	HookSessionID   string
	HookExecution   hook.ExecutionRef
	Context         context.Context
	Cancel          context.CancelCauseFunc
	ParentBridge    CancelBridge
	// modelToolResults retains only the transient Provider projection for tool
	// result messages. Conversation itself keeps the persistence projection and
	// is never saved for child tasks.
	modelToolResults map[string]redact.SafeText

	mu sync.Mutex
}

func (state *TaskRuntimeState) setModelToolResult(callID string, content redact.SafeText) error {
	if state == nil || callID == "" || content.Text() == "" {
		return errors.New("task model tool result is invalid")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.modelToolResults == nil {
		state.modelToolResults = make(map[string]redact.SafeText)
	}
	if _, exists := state.modelToolResults[callID]; exists {
		return errors.New("task model tool result is duplicated")
	}
	state.modelToolResults[callID] = content
	return nil
}

func (state *TaskRuntimeState) modelToolResult(callID string) (redact.SafeText, bool) {
	if state == nil || callID == "" {
		return redact.SafeText{}, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	content, ok := state.modelToolResults[callID]
	return content, ok
}

func (state *TaskRuntimeState) snapshotUsage() provider.Usage {
	if state == nil {
		return provider.Usage{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.Usage
}

func (state *TaskRuntimeState) snapshotIteration() int {
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.Iteration
}

func (state *TaskRuntimeState) beginIteration(iteration int) error {
	if state == nil || iteration <= 0 {
		return errors.New("task iteration is invalid")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if iteration != state.Iteration+1 || iteration > state.Profile.MaxIterations {
		return errors.New("task iteration is not monotonic")
	}
	state.Iteration = iteration
	return nil
}

func (state *TaskRuntimeState) addUsage(usage *provider.Usage) (provider.Usage, error) {
	if state == nil {
		return provider.Usage{}, errors.New("task usage is unavailable")
	}
	if usage == nil {
		return state.snapshotUsage(), nil
	}
	if err := validateProviderUsageSnapshot(*usage); err != nil {
		return provider.Usage{}, err
	}
	values := [4]int64{usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens}
	state.mu.Lock()
	defer state.mu.Unlock()
	totalValues := [4]int64{
		state.Usage.InputTokens, state.Usage.OutputTokens,
		state.Usage.CacheCreationInputTokens, state.Usage.CacheReadInputTokens,
	}
	for index, value := range values {
		if value > math.MaxInt64-totalValues[index] {
			return provider.Usage{}, errors.New("task usage overflow")
		}
		totalValues[index] += value
	}
	candidate := provider.Usage{
		InputTokens: totalValues[0], OutputTokens: totalValues[1],
		CacheCreationInputTokens: totalValues[2], CacheReadInputTokens: totalValues[3],
	}
	if err := validateProviderUsageSnapshot(candidate); err != nil {
		return provider.Usage{}, err
	}
	state.Usage = candidate
	return candidate, nil
}

func (state *TaskRuntimeState) close(cause error) {
	if state == nil {
		return
	}
	if state.ParentBridge != nil {
		state.ParentBridge.Close()
	}
	if state.Confirm != nil {
		state.Confirm.Close(cause)
	}
	if state.ReadCache != nil {
		state.ReadCache.Close()
	}
	if state.Cancel != nil {
		state.Cancel(cause)
	}
}

type parentCancelBridge struct {
	mu       sync.Mutex
	stop     func() bool
	detached bool
	closed   bool
}

func newParentCancelBridge(parent context.Context, cancel context.CancelCauseFunc) CancelBridge {
	bridge := &parentCancelBridge{detached: parent == nil || cancel == nil}
	if bridge.detached {
		return bridge
	}
	bridge.stop = context.AfterFunc(parent, func() {
		cancel(context.Cause(parent))
	})
	return bridge
}

func (bridge *parentCancelBridge) Detach() bool {
	if bridge == nil {
		return false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed || bridge.detached {
		return false
	}
	// context.AfterFunc returns false once the cancellation callback has
	// started. In that case parent cancellation won the linearization race: do
	// not report a successful detach and do not let placement switch views.
	if bridge.stop == nil || !bridge.stop() {
		return false
	}
	bridge.stop = nil
	bridge.detached = true
	return true
}

func (bridge *parentCancelBridge) Close() {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed {
		return
	}
	bridge.closed = true
	if bridge.stop != nil {
		bridge.stop()
		bridge.stop = nil
	}
}

var _ CancelBridge = (*parentCancelBridge)(nil)
