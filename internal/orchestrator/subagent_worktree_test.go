package orchestrator

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/provider"
	"xagent/internal/subagent"
	"xagent/internal/worktree"
)

func TestWorktreeE2ECancellingOnePreparedTaskDoesNotAffectSibling(t *testing.T) {
	manager := &runnerIsolationManager{}
	alphaRuntime := &runnerIsolationRuntime{manager: manager, stable: &runnerIsolationStableContext{}}
	betaRuntime := &runnerIsolationRuntime{manager: manager, stable: &runnerIsolationStableContext{}}
	binder := &runnerIsolationBinder{manager: manager, runtime: alphaRuntime}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)
	factory.options.WorktreeManager = &orchestratorWorktreeE2EManager{runnerIsolationManager: manager}
	script := newOrchestratorWorktreeE2EProvider()
	factory.options.Provider = script

	current := alphaRuntime
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		lease := runnerIsolationLease(t, request)
		if err := os.MkdirAll(lease.Root, 0o700); err != nil {
			return worktree.Lease{}, err
		}
		current.lease = lease
		return lease, nil
	}
	alpha, err := factory.Prepare(context.Background(), "worktree-e2e-alpha", runnerIsolationInputWithTask("alpha sibling"))
	if err != nil {
		t.Fatal(err)
	}
	current = betaRuntime
	binder.runtime = betaRuntime
	beta, err := factory.Prepare(context.Background(), "worktree-e2e-beta", runnerIsolationInputWithTask("beta cancel"))
	if err != nil {
		t.Fatal(err)
	}
	if alphaRuntime.lease.WorkspaceID == betaRuntime.lease.WorkspaceID || alphaRuntime.lease.Root == betaRuntime.lease.Root ||
		alphaRuntime.lease.Branch == betaRuntime.lease.Branch {
		t.Fatalf("prepared siblings shared workspace ownership: alpha=%#v beta=%#v", alpha.Metadata(), beta.Metadata())
	}

	alphaCtx, cancelAlpha := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelAlpha()
	betaCtx, cancelBeta := context.WithCancel(context.Background())
	type runOutcome struct {
		name   string
		result subagent.RunResult
	}
	results := make(chan runOutcome, 2)
	go func() { results <- runOutcome{name: "alpha", result: alpha.Run(alphaCtx, nil)} }()
	go func() { results <- runOutcome{name: "beta", result: beta.Run(betaCtx, nil)} }()
	select {
	case <-script.ready:
		cancelBeta()
	case <-alphaCtx.Done():
		t.Fatalf("parallel runner barrier timed out: %v", context.Cause(alphaCtx))
	}
	runs := make(map[string]subagent.RunResult, 2)
	for range 2 {
		select {
		case outcome := <-results:
			runs[outcome.name] = outcome.result
		case <-alphaCtx.Done():
			t.Fatalf("parallel runner completion timed out: %v", context.Cause(alphaCtx))
		}
	}
	if runs["alpha"].Status != subagent.StatusCompleted || runs["beta"].Status != subagent.StatusCancelled {
		t.Fatalf("run statuses = alpha:%s beta:%s", runs["alpha"].Status, runs["beta"].Status)
	}
	alphaCompletion := alpha.Settle(context.Background(), runs["alpha"])
	betaCompletion := beta.Settle(context.Background(), runs["beta"])
	for label, completion := range map[string]subagent.Completion{"alpha": alphaCompletion, "beta": betaCompletion} {
		if err := completion.Validate(); err != nil {
			t.Fatalf("%s completion invalid: %v", label, err)
		}
		if completion.Workspace.Isolation != "worktree" || completion.Workspace.State != "retained" || completion.Workspace.Cleanup != "retained" {
			t.Fatalf("%s settlement = %#v", label, completion.Workspace)
		}
		projection := completion.Workspace.WorkspaceID + completion.Workspace.Branch + completion.Workspace.BaseOID + completion.Workspace.RetentionCause
		if strings.Contains(projection, alphaRuntime.lease.Root) || strings.Contains(projection, betaRuntime.lease.Root) {
			t.Fatalf("%s workspace projection leaked an absolute root: %q", label, projection)
		}
	}
	if alphaCompletion.Workspace.WorkspaceID == betaCompletion.Workspace.WorkspaceID ||
		alphaCompletion.Workspace.Branch == betaCompletion.Workspace.Branch {
		t.Fatalf("settled siblings shared workspace identity: alpha=%#v beta=%#v", alphaCompletion.Workspace, betaCompletion.Workspace)
	}
}

func runnerIsolationInputWithTask(task string) subagent.SubmitInput {
	input := runnerIsolationInput()
	input.Task = task
	return input
}

type orchestratorWorktreeE2EProvider struct {
	mu      sync.Mutex
	arrived int
	ready   chan struct{}
}

func newOrchestratorWorktreeE2EProvider() *orchestratorWorktreeE2EProvider {
	return &orchestratorWorktreeE2EProvider{ready: make(chan struct{})}
}

func (*orchestratorWorktreeE2EProvider) Name() string { return "orchestrator-worktree-e2e" }

func (script *orchestratorWorktreeE2EProvider) StreamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	marker := ""
	for _, message := range request.Messages {
		if strings.Contains(message.Content.Text(), "alpha sibling") {
			marker = "alpha"
		}
		if strings.Contains(message.Content.Text(), "beta cancel") {
			marker = "beta"
		}
	}
	if marker == "" {
		return nil, errors.New("worktree e2e marker is unavailable")
	}
	script.mu.Lock()
	script.arrived++
	if script.arrived == 2 {
		close(script.ready)
	}
	script.mu.Unlock()
	select {
	case <-script.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if marker == "beta" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return newOrchestratorTestChatStream(
		provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: testSafeText("alpha complete")},
		provider.StreamEvent{Type: provider.StreamEventDone},
	), nil
}

var _ provider.Provider = (*orchestratorWorktreeE2EProvider)(nil)

type orchestratorWorktreeE2EManager struct {
	*runnerIsolationManager
}

func (manager *orchestratorWorktreeE2EManager) Settle(_ context.Context, _ worktree.Lease, request worktree.SettleRequest) (worktree.Settlement, error) {
	manager.calls = append(manager.calls, "settle")
	manager.settleRequests = append(manager.settleRequests, request)
	return worktree.Settlement{State: worktree.SettlementRetained, Dirty: true, ReasonCode: "protected_changes"}, nil
}
