package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/provider"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type cancellationTurnEnd struct {
	kind   hook.ExecutionKind
	status hook.TurnStatus
	detail string
}

type cancellationHookRecorder struct {
	hook.Runtime
	mu      sync.Mutex
	nextID  int
	turnEnd chan cancellationTurnEnd
}

func newCancellationHookRecorder() *cancellationHookRecorder {
	return &cancellationHookRecorder{Runtime: hook.Noop(), turnEnd: make(chan cancellationTurnEnd, 8)}
}

func (r *cancellationHookRecorder) BeginTurn(_ context.Context, sessionID string, kind hook.ExecutionKind, mode hook.HookMode) hook.ExecutionRef {
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.mu.Unlock()
	return hook.ExecutionRef{
		SessionID: sessionID, ExecutionID: fmt.Sprintf("cancel-execution-%d", id),
		TurnID: fmt.Sprintf("cancel-turn-%d", id), Kind: kind, Mode: mode,
	}
}

func (r *cancellationHookRecorder) EndTurn(_ context.Context, ref hook.ExecutionRef, status hook.TurnStatus, detail string) {
	r.turnEnd <- cancellationTurnEnd{kind: ref.Kind, status: status, detail: detail}
}

type cancelDuringStreamStartProvider struct {
	entered chan struct{}
	once    sync.Once
}

func newCancelDuringStreamStartProvider() *cancelDuringStreamStartProvider {
	return &cancelDuringStreamStartProvider{entered: make(chan struct{})}
}

func (*cancelDuringStreamStartProvider) Name() string { return "cancel-during-stream-start" }

func (p *cancelDuringStreamStartProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (provider.ChatStream, error) {
	p.once.Do(func() { close(p.entered) })
	<-ctx.Done()
	return nil, errors.New("provider returned after request cancellation")
}

type cancelThenCloseProvider struct {
	entered chan struct{}
	once    sync.Once
}

func newCancelThenCloseProvider() *cancelThenCloseProvider {
	return &cancelThenCloseProvider{entered: make(chan struct{})}
}

func (*cancelThenCloseProvider) Name() string { return "cancel-then-close" }

func (p *cancelThenCloseProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (provider.ChatStream, error) {
	p.once.Do(func() { close(p.entered) })
	out := make(chan provider.StreamEvent)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return newOrchestratorTestChatStreamFromChannel(out, nil), nil
}

type immediateProviderError struct{ err error }

func (*immediateProviderError) Name() string { return "immediate-provider-error" }
func (p *immediateProviderError) StreamChat(context.Context, provider.ChatRequest) (provider.ChatStream, error) {
	return nil, p.err
}

func TestMainTurnCancellationDuringProviderStartIsCanceled(t *testing.T) {
	providerImpl := newCancelDuringStreamStartProvider()
	hooks := newCancellationHookRecorder()
	orch := NewWithOptions(OrchestratorOptions{Provider: providerImpl, Hooks: hooks})
	conv := conversation.NewConversation("main-cancel-session", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := orch.SendRequest(ctx, conv, RunRequest{UserText: "cancel while starting", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	drained := drainCancellationEvents(stream)
	waitCancellationBarrier(t, providerImpl.entered, "main Provider start")
	cancel()
	waitCancellationBarrier(t, drained, "main event drain")
	ended := waitTurnEnd(t, hooks)
	if ended.kind != hook.ExecutionMain || ended.status != hook.TurnCanceled || ended.detail != "request canceled" {
		t.Fatalf("main turn end = %#v, want main/canceled", ended)
	}
}

func TestIsolatedTurnCancellationWinsProviderClose(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"cancel.md": `---
name: cancel
description: Cancellation lifecycle fixture
mode: isolated
history: 0
---
CANCEL TEST
`}, nil)
	providerImpl := newCancelThenCloseProvider()
	hooks := newCancellationHookRecorder()
	orch.provider = providerImpl
	orch.hooks = hooks
	ctx, cancel := context.WithCancel(context.Background())
	stream, _, err := orch.SendSkill(ctx, conv, skill.Invocation{
		Name: "cancel", Raw: "/cancel", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	drained := drainCancellationEvents(stream)
	waitCancellationBarrier(t, providerImpl.entered, "isolated Provider start")
	cancel()
	waitCancellationBarrier(t, drained, "isolated event drain")
	ended := waitTurnEnd(t, hooks)
	if ended.kind != hook.ExecutionIsolatedSkill || ended.status != hook.TurnCanceled || ended.detail != "request canceled" {
		t.Fatalf("isolated turn end = %#v, want isolated_skill/canceled", ended)
	}
}

func TestProviderErrorWithoutRequestCancellationRemainsError(t *testing.T) {
	wantErr := errors.New("real provider failure")
	hooks := newCancellationHookRecorder()
	orch := NewWithOptions(OrchestratorOptions{Provider: &immediateProviderError{err: wantErr}, Hooks: hooks})
	conv := conversation.NewConversation("provider-error-session", time.Now())
	stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "fail normally", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	ended := waitTurnEnd(t, hooks)
	if ended.kind != hook.ExecutionMain || ended.status != hook.TurnError || ended.detail != "agent run failed" {
		t.Fatalf("provider failure turn end = %#v, want main/error", ended)
	}
}

func TestCollectProviderStreamCancellationWinsClosedOrErrorRace(t *testing.T) {
	for _, mode := range []string{"closed", "error"} {
		t.Run(mode, func(t *testing.T) {
			for iteration := 0; iteration < 128; iteration++ {
				ctx, cancel := context.WithCancel(context.Background())
				stream := make(chan provider.StreamEvent, 1)
				if mode == "error" {
					stream <- provider.StreamEvent{Type: provider.StreamEventError, Error: testSafeProviderError(errors.New("provider race failure"))}
				}
				close(stream)
				cancel()
				_, reason, err := collectProviderStreamWithRedactor(ctx, newOrchestratorTestChatStreamFromChannel(stream, nil), make(chan events.Event), nil, 64)
				if reason != StopReasonCancelled || !errors.Is(err, context.Canceled) {
					t.Fatalf("iteration %d: reason=%s err=%v, want canceled", iteration, reason, err)
				}
			}
		})
	}
}

func TestCancellationBeforeStartProducesNoToolRoleMessage(t *testing.T) {
	t.Run("confirmation barrier records cancellation without a result", func(t *testing.T) {
		fixture := newToolCancellationFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan events.Event, 16)
		done := make(chan concurrentScheduleResult, 1)
		go func() {
			executions, reason, err := fixture.orch.scheduleToolCallsWithRegistryAndRef(ctx, RunModeDefault, fixture.registry, []tool.Call{{
				ID: "confirm-cancel", Name: "Write", ArgumentsJSON: `{"path":"blocked.txt","content":"must not be written"}`,
			}}, hook.ExecutionRef{}, out)
			done <- concurrentScheduleResult{executions: executions, reason: reason, err: err}
		}()
		waitForCancellationToolEvent(t, out, events.ToolWaitingConfirmation)
		cancel()
		result := waitForConcurrentSchedule(t, done)
		assertCancelledBeforeStart(t, result, "confirm-cancel")
		assertCancellationNotPublished(t, fixture, result.executions, out)
		if _, err := os.Stat(filepath.Join(fixture.root, "blocked.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pre-start cancellation changed the filesystem: %v", err)
		}
	})

	t.Run("semaphore queue records cancellation without a result", func(t *testing.T) {
		fixture := newToolCancellationFixture(t)
		fixture.orch.SetPermissionMode(permission.ModePermissive)
		out := make(chan events.Event, 16)
		prepared := fixture.orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, fixture.registry, indexedToolCall{
			Call: tool.Call{ID: "queue-cancel", Name: "Read", ArgumentsJSON: `{"path":"queued.txt"}`}, Index: 0,
		}, hook.ExecutionRef{}, out)
		if prepared.State != tool.Prepared || prepared.HasResult || prepared.Err != nil {
			t.Fatalf("prepared execution = %#v", prepared)
		}
		for len(concurrentToolWorkerSemaphore) < cap(concurrentToolWorkerSemaphore) {
			concurrentToolWorkerSemaphore <- struct{}{}
		}
		defer func() {
			for len(concurrentToolWorkerSemaphore) > 0 {
				<-concurrentToolWorkerSemaphore
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		done := make(chan []ToolExecution, 1)
		go func() {
			close(entered)
			done <- fixture.orch.executeConcurrentPreparedTools(ctx, []ToolExecution{prepared}, out)
		}()
		waitCancellationBarrier(t, entered, "concurrent queue")
		cancel()
		executions := <-done
		result := concurrentScheduleResult{executions: executions, reason: executions[0].StopReason, err: executions[0].Err}
		assertCancelledBeforeStart(t, result, "queue-cancel")
		assertCancellationNotPublished(t, fixture, executions, out)
	})

	t.Run("start event barrier records cancellation without a result", func(t *testing.T) {
		fixture := newToolCancellationFixture(t)
		fixture.orch.SetPermissionMode(permission.ModePermissive)
		preparedOut := make(chan events.Event, 4)
		prepared := fixture.orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, fixture.registry, indexedToolCall{
			Call: tool.Call{ID: "start-cancel", Name: "Read", ArgumentsJSON: `{"path":"queued.txt"}`}, Index: 0,
		}, hook.ExecutionRef{}, preparedOut)
		if prepared.State != tool.Prepared || prepared.HasResult || prepared.Err != nil {
			t.Fatalf("prepared execution = %#v", prepared)
		}

		ctx, cancel := context.WithCancel(context.Background())
		blockedOut := make(chan events.Event)
		entered := make(chan struct{})
		done := make(chan ToolExecution, 1)
		go func() {
			close(entered)
			done <- fixture.orch.executePreparedTool(ctx, prepared, blockedOut)
		}()
		waitCancellationBarrier(t, entered, "tool start event barrier")
		cancel()
		execution := <-done
		result := concurrentScheduleResult{executions: []ToolExecution{execution}, reason: execution.StopReason, err: execution.Err}
		assertCancelledBeforeStart(t, result, "start-cancel")
		assertCancellationNotPublished(t, fixture, result.executions, preparedOut)
	})
}

type toolCancellationFixture struct {
	orch     *Orchestrator
	registry *tool.Registry
	state    *executionState
	conv     *conversation.Conversation
	root     string
}

func newToolCancellationFixture(t *testing.T) toolCancellationFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "queued.txt"), []byte("safe fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := newProjectionCandidate(t, hook.Noop())
	executor := tool.NewExecutor(registry, root, time.Second, 1024)
	orch := NewWithOptions(OrchestratorOptions{
		Registry: registry, Executor: executor, ContextManager: candidate.contextManager,
		ResultFactory: candidate.resultFactory, MaxRecordBytes: 4096, MaxSessionBytes: 16384,
		RuntimeRedactor: candidate.runtimeRedactor, Hooks: hook.Noop(),
	})
	conv := conversation.NewConversation("pre-start-cancellation", time.Now())
	state := &executionState{}
	if err := orch.configureCandidateState(state, conv.ID); err != nil {
		t.Fatal(err)
	}
	return toolCancellationFixture{orch: orch, registry: registry, state: state, conv: conv, root: root}
}

func waitForCancellationToolEvent(t *testing.T, out <-chan events.Event, want events.Type) {
	t.Helper()
	for {
		select {
		case event := <-out:
			if event.Type == want {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func assertCancelledBeforeStart(t *testing.T, result concurrentScheduleResult, callID string) {
	t.Helper()
	if !errors.Is(result.err, context.Canceled) || result.reason != StopReasonCancelled || len(result.executions) != 1 {
		t.Fatalf("cancellation result = (%#v, %q, %v)", result.executions, result.reason, result.err)
	}
	execution := result.executions[0]
	if execution.Call.ID != callID || execution.State != tool.CancelledBeforeStart || execution.HasResult || execution.Result.CallID != "" {
		t.Fatalf("pre-start execution = %#v", execution)
	}
}

func assertCancellationNotPublished(t *testing.T, fixture toolCancellationFixture, executions []ToolExecution, out <-chan events.Event) {
	t.Helper()
	published, err := fixture.orch.publishToolExecutions(context.Background(), fixture.conv, fixture.state, 1, executions, make(chan events.Event, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != len(executions) {
		t.Fatalf("published executions = %#v", published)
	}
	for _, message := range fixture.conv.Messages {
		if message.Role == conversation.RoleToolCall || message.Role == conversation.RoleToolResult {
			t.Fatalf("pre-start cancellation produced tool-role message: %#v", fixture.conv.Messages)
		}
	}
	for {
		select {
		case event := <-out:
			if event.Type == events.ToolSuccess || event.Type == events.ToolError || event.Type == events.ToolDenied {
				t.Fatalf("pre-start cancellation produced result event: %#v", event)
			}
		default:
			return
		}
	}
}

func drainCancellationEvents(stream <-chan events.Event) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for range stream {
		}
		close(done)
	}()
	return done
}

func waitCancellationBarrier(t *testing.T, barrier <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitTurnEnd(t *testing.T, recorder *cancellationHookRecorder) cancellationTurnEnd {
	t.Helper()
	select {
	case ended := <-recorder.turnEnd:
		return ended
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn_end")
		return cancellationTurnEnd{}
	}
}
