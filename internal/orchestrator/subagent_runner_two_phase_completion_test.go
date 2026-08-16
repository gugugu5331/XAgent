package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/hook"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/workspace"
	"xagent/internal/worktree"
)

type twoPhaseManager struct {
	mu sync.Mutex

	calls          []string
	settleRequests []worktree.SettleRequest
	settlement     worktree.Settlement
	settleErr      error
	releaseErr     error
	settleEntered  chan struct{}
	settleContinue chan struct{}
	settleSignal   sync.Once
}

func (*twoPhaseManager) Acquire(context.Context, worktree.AcquireRequest) (worktree.Lease, error) {
	return worktree.Lease{}, errors.New("two-phase test does not acquire")
}

func (manager *twoPhaseManager) record(call string) {
	manager.mu.Lock()
	manager.calls = append(manager.calls, call)
	manager.mu.Unlock()
}

func (manager *twoPhaseManager) Settle(_ context.Context, _ worktree.Lease, request worktree.SettleRequest) (worktree.Settlement, error) {
	manager.mu.Lock()
	manager.calls = append(manager.calls, "settle")
	manager.settleRequests = append(manager.settleRequests, request)
	manager.mu.Unlock()
	if manager.settleEntered != nil {
		manager.settleSignal.Do(func() { close(manager.settleEntered) })
	}
	if manager.settleContinue != nil {
		<-manager.settleContinue
	}
	return manager.settlement, manager.settleErr
}

func (manager *twoPhaseManager) Release(context.Context, worktree.Lease) error {
	manager.record("release")
	return manager.releaseErr
}

func (manager *twoPhaseManager) snapshot() ([]string, []worktree.SettleRequest) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]string(nil), manager.calls...), append([]worktree.SettleRequest(nil), manager.settleRequests...)
}

type twoPhaseRuntime struct {
	*runnerIsolationRuntime
	manager  *twoPhaseManager
	closeErr error
}

func (runtime *twoPhaseRuntime) StopAccepting() {
	runtime.manager.record("runtime-stop")
}

func (runtime *twoPhaseRuntime) Close(context.Context) error {
	runtime.manager.record("runtime-close")
	return runtime.closeErr
}

func newTwoPhaseTask(t *testing.T, manager *twoPhaseManager) (*preparedSubagentTask, *twoPhaseRuntime) {
	t.Helper()
	factory, _ := newTaskRuntimeFactoryFixture(t, 0)
	ownedContext, cancel := context.WithCancelCause(context.Background())
	lease := worktree.Lease{
		WorkspaceID: strings.Repeat("a", 32),
		OwnerID:     strings.Repeat("b", 32),
		Root:        t.TempDir(),
		Branch:      "xagent/worktree/" + strings.Repeat("a", 32),
		BaseOID:     strings.Repeat("c", 40),
	}
	baseRuntime := &runnerIsolationRuntime{manager: &runnerIsolationManager{}, lease: lease}
	ownedRuntime := &twoPhaseRuntime{runnerIsolationRuntime: baseRuntime, manager: manager}
	task := &preparedSubagentTask{
		factory: factory,
		runtime: &TaskRuntimeState{
			TaskID: "two-phase-task", Profile: RuntimeProfile{MaxIterations: 0},
			Hooks: hook.Noop(), Context: ownedContext, Cancel: cancel,
		},
		workspaceRuntime: ownedRuntime,
		workspaceLease:   lease,
		worktreeManager:  manager,
	}
	return task, ownedRuntime
}

func twoPhaseRunResult(redactor *redact.RuntimeRedactor, summary string) subagent.RunResult {
	return subagent.RunResult{
		Status: subagent.StatusCompleted, StopReason: subagent.StopCompleted,
		Summary: redactor.Redact(summary),
	}
}

func awaitTwoPhaseCompletion(t *testing.T, results <-chan subagent.Completion) subagent.Completion {
	t.Helper()
	select {
	case completion := <-results:
		return completion
	case <-time.After(time.Second):
		t.Fatal("Settle did not complete within the test bound")
		return subagent.Completion{}
	}
}

func TestTwoPhaseCompletionRunDoesNotCloseOrSettleWorkspace(t *testing.T) {
	manager := &twoPhaseManager{settlement: worktree.Settlement{State: worktree.SettlementDeleted, ReasonCode: "clean"}}
	task, _ := newTwoPhaseTask(t, manager)

	result := task.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
	if calls, _ := manager.snapshot(); len(calls) != 0 {
		t.Fatalf("Run touched workspace settlement: %v", calls)
	}
	if result.Status != subagent.StatusLimitReached {
		t.Fatalf("RunResult status = %q, want limit_reached", result.Status)
	}

	completion := task.Settle(context.Background(), result)
	if calls, _ := manager.snapshot(); !reflect.DeepEqual(calls, []string{"runtime-stop", "runtime-close", "settle", "release"}) {
		t.Fatalf("Settle order = %v", calls)
	}
	if completion.Workspace.State != "deleted" || completion.Workspace.Cleanup != "deleted" {
		t.Fatalf("workspace summary = %#v", completion.Workspace)
	}
}

func TestTwoPhaseCompletionMapsRetainedSettlementWithoutPaths(t *testing.T) {
	manager := &twoPhaseManager{settlement: worktree.Settlement{
		State: worktree.SettlementRetained, Dirty: true, ReasonCode: "protected_changes",
	}}
	task, _ := newTwoPhaseTask(t, manager)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result := twoPhaseRunResult(task.factory.options.RuntimeRedactor, "finished")

	completion := task.Settle(cancelled, result)
	want := subagent.WorkspaceSummary{
		WorkspaceID: strings.Repeat("a", 32), Isolation: "worktree",
		State: "retained", BaseOID: strings.Repeat("c", 40),
		Branch: "xagent/worktree/" + strings.Repeat("a", 32), Dirty: true,
		Cleanup: "retained", RetentionCause: "protected_changes",
	}
	if !reflect.DeepEqual(completion.Workspace, want) {
		t.Fatalf("workspace summary = %#v, want %#v", completion.Workspace, want)
	}
	if err := completion.Validate(); err != nil {
		t.Fatalf("completion is invalid: %v", err)
	}
	if strings.Contains(strings.Join([]string{
		completion.Workspace.WorkspaceID, completion.Workspace.State, completion.Workspace.BaseOID,
		completion.Workspace.Branch, completion.Workspace.RetentionCause,
	}, " "), task.workspaceLease.Root) {
		t.Fatal("workspace summary leaked the absolute root")
	}
	calls, requests := manager.snapshot()
	if !reflect.DeepEqual(calls, []string{"runtime-stop", "runtime-close", "settle", "release"}) ||
		len(requests) != 1 || !requests[0].RuntimeStopped {
		t.Fatalf("settlement order/request = %v / %#v", calls, requests)
	}
}

func TestTwoPhaseCompletionConcurrentSettleIsExactlyOnceAndKeepsFirstResult(t *testing.T) {
	manager := &twoPhaseManager{
		settlement:    worktree.Settlement{State: worktree.SettlementDeleted, ReasonCode: "clean"},
		settleEntered: make(chan struct{}), settleContinue: make(chan struct{}),
	}
	task, _ := newTwoPhaseTask(t, manager)
	first := twoPhaseRunResult(task.factory.options.RuntimeRedactor, "first-result")
	second := twoPhaseRunResult(task.factory.options.RuntimeRedactor, "second-result")

	firstDone := make(chan subagent.Completion, 1)
	go func() { firstDone <- task.Settle(context.Background(), first) }()
	select {
	case <-manager.settleEntered:
	case <-time.After(time.Second):
		t.Fatal("first Settle did not reach the lifecycle manager")
	}

	const concurrent = 16
	results := make(chan subagent.Completion, concurrent)
	var started sync.WaitGroup
	started.Add(concurrent)
	for index := 0; index < concurrent; index++ {
		go func() {
			started.Done()
			results <- task.Settle(context.Background(), second)
		}()
	}
	started.Wait()
	close(manager.settleContinue)
	want := awaitTwoPhaseCompletion(t, firstDone)
	if want.Summary.Text() != "first-result" {
		t.Fatalf("first settlement result = %q", want.Summary.Text())
	}
	for index := 0; index < concurrent; index++ {
		if got := awaitTwoPhaseCompletion(t, results); !reflect.DeepEqual(got, want) {
			t.Fatalf("concurrent result %d = %#v, want %#v", index, got, want)
		}
	}
	if calls, requests := manager.snapshot(); !reflect.DeepEqual(calls, []string{"runtime-stop", "runtime-close", "settle", "release"}) || len(requests) != 1 {
		t.Fatalf("concurrent settlement calls = %v requests=%d", calls, len(requests))
	}
}

func TestTwoPhaseCompletionCloseFailureRetainsAndDoesNotRelease(t *testing.T) {
	secret := t.TempDir()
	manager := &twoPhaseManager{
		settlement: worktree.Settlement{State: worktree.SettlementDeleted, ReasonCode: "clean"},
		settleErr:  errors.New("settle exposed " + secret),
	}
	task, runtime := newTwoPhaseTask(t, manager)
	runtime.closeErr = errors.New("close exposed " + secret)

	completion := task.Settle(context.Background(), twoPhaseRunResult(task.factory.options.RuntimeRedactor, "finished"))
	if calls, requests := manager.snapshot(); !reflect.DeepEqual(calls, []string{"runtime-stop", "runtime-close", "settle"}) ||
		len(requests) != 1 || requests[0].RuntimeStopped {
		t.Fatalf("failed-close settlement calls = %v requests=%#v", calls, requests)
	}
	workspaceSummary := completion.Workspace
	if workspaceSummary.State != "retained" || workspaceSummary.Cleanup != "retained" ||
		workspaceSummary.RetentionCause != "runtime_active" || workspaceSummary.Error == nil {
		t.Fatalf("failed-close workspace summary = %#v", workspaceSummary)
	}
	if strings.Contains(workspaceSummary.Error.Error(), secret) || workspaceSummary.Error.Error() != "workspace runtime did not stop" {
		t.Fatalf("workspace error was not fixed and path-free: %#v", workspaceSummary.Error)
	}
	if err := completion.Validate(); err != nil {
		t.Fatalf("failed-close completion is invalid: %v", err)
	}
}

func TestTwoPhaseCompletionUnknownSettlementFailsClosed(t *testing.T) {
	secret := t.TempDir()
	manager := &twoPhaseManager{
		settlement: worktree.Settlement{State: worktree.SettlementState("future"), ReasonCode: secret},
		settleErr:  errors.New("unknown settlement " + secret),
	}
	task, _ := newTwoPhaseTask(t, manager)

	completion := task.Settle(context.Background(), twoPhaseRunResult(task.factory.options.RuntimeRedactor, "finished"))
	if completion.Workspace.State != "manual_attention" || completion.Workspace.Cleanup != "manual_attention" ||
		completion.Workspace.RetentionCause != "settlement_failed" || completion.Workspace.Error == nil {
		t.Fatalf("unknown settlement summary = %#v", completion.Workspace)
	}
	if strings.Contains(completion.Workspace.Error.Error(), secret) || completion.Workspace.Error.Error() != "workspace settlement requires attention" {
		t.Fatalf("unknown settlement error leaked detail: %#v", completion.Workspace.Error)
	}
	if calls, _ := manager.snapshot(); !reflect.DeepEqual(calls, []string{"runtime-stop", "runtime-close", "settle", "release"}) {
		t.Fatalf("unknown settlement calls = %v", calls)
	}
}

func TestTwoPhaseCompletionReleaseFailureDoesNotClaimDeleted(t *testing.T) {
	secret := t.TempDir()
	manager := &twoPhaseManager{
		settlement: worktree.Settlement{State: worktree.SettlementDeleted, ReasonCode: "clean"},
		releaseErr: errors.New("release exposed " + secret),
	}
	task, _ := newTwoPhaseTask(t, manager)

	completion := task.Settle(context.Background(), twoPhaseRunResult(task.factory.options.RuntimeRedactor, "finished"))
	if completion.Workspace.State != "manual_attention" || completion.Workspace.Cleanup != "manual_attention" ||
		completion.Workspace.RetentionCause != "lease_release_failed" || completion.Workspace.Error == nil {
		t.Fatalf("release-failure summary = %#v", completion.Workspace)
	}
	if strings.Contains(completion.Workspace.Error.Error(), secret) || completion.Workspace.Error.Error() != "workspace lease release requires attention" {
		t.Fatalf("release-failure error leaked detail: %#v", completion.Workspace.Error)
	}
	if calls, _ := manager.snapshot(); !reflect.DeepEqual(calls, []string{"runtime-stop", "runtime-close", "settle", "release"}) {
		t.Fatalf("release-failure calls = %v", calls)
	}
}

func TestTwoPhaseCompletionContradictoryDeletedSettlementFailsClosed(t *testing.T) {
	manager := &twoPhaseManager{settlement: worktree.Settlement{
		State: worktree.SettlementDeleted, Dirty: true, Unpushed: true, ReasonCode: "clean",
	}}
	task, _ := newTwoPhaseTask(t, manager)

	completion := task.Settle(context.Background(), twoPhaseRunResult(task.factory.options.RuntimeRedactor, "finished"))
	if completion.Workspace.State != "manual_attention" || completion.Workspace.Cleanup != "manual_attention" ||
		completion.Workspace.RetentionCause != "settlement_failed" {
		t.Fatalf("contradictory deleted settlement was trusted: %#v", completion.Workspace)
	}
}

func TestTwoPhaseCompletionSharedSettleHasNoWorkspaceSideEffects(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 0)
	task := &preparedSubagentTask{
		factory: factory,
		runtime: &TaskRuntimeState{TaskID: "shared-two-phase", Hooks: hook.Noop()},
	}
	result := twoPhaseRunResult(factory.options.RuntimeRedactor, "shared")

	first := task.Settle(context.Background(), result)
	second := task.Settle(context.Background(), twoPhaseRunResult(factory.options.RuntimeRedactor, "changed"))
	if first.Workspace != (subagent.WorkspaceSummary{}) || !reflect.DeepEqual(first, second) || first.Summary.Text() != "shared" {
		t.Fatalf("shared settlement changed behavior: first=%#v second=%#v", first, second)
	}
}

var _ WorktreeLifecycleManager = (*twoPhaseManager)(nil)
var _ workspace.Runtime = (*twoPhaseRuntime)(nil)
