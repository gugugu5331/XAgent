package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/testutil"
	"xagent/internal/tool"
)

type orderedCommitTool struct {
	name       string
	preview    string
	factory    *tool.ResultFactory
	wait       <-chan struct{}
	signal     chan struct{}
	signalOnce *sync.Once
	executions atomic.Int32
}

func (t *orderedCommitTool) Name() string        { return t.name }
func (t *orderedCommitTool) Description() string { return "ordered commit test tool" }
func (t *orderedCommitTool) Schema() tool.Schema { return tool.Schema{Type: "object"} }
func (t *orderedCommitTool) Risk() tool.Risk     { return tool.RiskSafe }
func (t *orderedCommitTool) UsesSafeResultBoundary() bool {
	return t != nil && t.factory != nil
}

func (t *orderedCommitTool) Execute(ctx context.Context, input tool.Input) tool.Result {
	t.executions.Add(1)
	if t.signal != nil && t.signalOnce != nil {
		t.signalOnce.Do(func() { close(t.signal) })
	}
	if t.wait != nil {
		select {
		case <-t.wait:
		case <-ctx.Done():
			result, _ := t.factory.Build(tool.ResultFactoryInput{
				CallID: input.CallID, Name: input.Name, State: tool.CancelledAfterStart, Status: tool.StatusTimeout,
				Summary: "ordered tool canceled", Error: &tool.Error{Code: tool.ErrTimeout, Message: "ordered tool canceled", Recoverable: true},
			})
			return result
		}
	}
	result, _ := t.factory.Build(tool.ResultFactoryInput{
		CallID: input.CallID, Name: input.Name, State: tool.Completed, Status: tool.StatusSuccess,
		Summary: t.preview, Preview: t.preview, CapturedBytes: int64(len(t.preview)),
	})
	return result
}

type cancelAfterToolHook struct {
	hook.Runtime
	cancel context.CancelFunc
	once   sync.Once
}

func (h *cancelAfterToolHook) AfterTool(context.Context, hook.ExecutionRef, hook.ToolInput, hook.ToolOutput, time.Duration) {
	h.once.Do(h.cancel)
}

type orderedCommitFixture struct {
	orch     *Orchestrator
	conv     *conversation.Conversation
	provider *scriptedSkillProvider
	store    *testutil.ScriptedStore
	first    *orderedCommitTool
	second   *orderedCommitTool
}

func newOrderedCommitFixture(t *testing.T, hooks hook.Runtime, save func(context.Context, *conversation.Conversation) (conversation.SaveResult, error)) orderedCommitFixture {
	t.Helper()
	template, factory := newProjectionCandidate(t, hook.Noop())
	gate := make(chan struct{})
	gateOnce := &sync.Once{}
	first := &orderedCommitTool{name: "OrderedFirst", preview: "first-result", factory: factory, wait: gate}
	second := &orderedCommitTool{name: "OrderedSecond", preview: "second-result", factory: factory, signal: gate, signalOnce: gateOnce}
	registry := tool.NewSafeCandidateRegistry()
	policy := tool.ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}
	if err := registry.RegisterWithOptions(first, tool.RegistrationOptions{Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterWithOptions(second, tool.RegistrationOptions{Policy: policy}); err != nil {
		t.Fatal(err)
	}
	providerImpl := &scriptedSkillProvider{events: [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-first", first.Name(), `{}`)},
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-second", second.Name(), `{}`)},
			{Type: provider.StreamEventDone},
		},
		{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("final answer")},
			{Type: provider.StreamEventDone},
		},
	}}
	store := &testutil.ScriptedStore{SaveFunc: save}
	executor := tool.NewExecutor(registry, t.TempDir(), time.Second, 4096)
	orch := NewWithOptions(OrchestratorOptions{
		Provider: providerImpl, Store: store, Resources: resources.New(), Thinking: config.ThinkingConfig{},
		Registry: registry, Executor: executor, ContextManager: template.contextManager, ResultFactory: factory,
		MaxRecordBytes: 4096, MaxSessionBytes: 16384, RuntimeRedactor: template.runtimeRedactor, Hooks: hooks,
	})
	return orderedCommitFixture{
		orch: orch, conv: conversation.NewConversation("ordered-tool-commit", time.Unix(1_700_000_000, 0)),
		provider: providerImpl, store: store, first: first, second: second,
	}
}

func runToolResultsOrderedCommitChecks(t *testing.T) {
	t.Helper()
	t.Run("tool_results_are_projected_saved_and_emitted_in_call_order", func(t *testing.T) {
		fixture := newOrderedCommitFixture(t, hook.Noop(), nil)
		resultEvents, _ := runOrderedCommitRequest(t, context.Background(), fixture)
		if len(resultEvents) != 2 || resultEvents[0] != "call-first" || resultEvents[1] != "call-second" {
			t.Fatalf("tool result event order = %v", resultEvents)
		}
		assertOrderedToolsExecutedOnce(t, fixture)
		assertOrderedExecutionFacts(t, fixture.conv)
		calls := fixture.store.Calls()
		if len(calls.Save) != 2 {
			t.Fatalf("ordered tool saves = %d, want tool commit plus final response", len(calls.Save))
		}
		assertOrderedExecutionFacts(t, calls.Save[0])
		if requests := fixture.provider.Requests(); len(requests) != 2 {
			t.Fatalf("Provider requests = %d, want tool request plus final request", len(requests))
		}
	})

	t.Run("store_failure_keeps_execution_facts_without_retry", func(t *testing.T) {
		fixture := newOrderedCommitFixture(t, hook.Noop(), func(context.Context, *conversation.Conversation) (conversation.SaveResult, error) {
			return conversation.SaveResult{}, errors.New("ordered tool save failed")
		})
		_, terminalProgress := runOrderedCommitRequest(t, context.Background(), fixture)
		assertOrderedToolsExecutedOnce(t, fixture)
		assertOrderedExecutionFacts(t, fixture.conv)
		if saves := len(fixture.store.Calls().Save); saves != 1 {
			t.Fatalf("failed ordered tool save attempts = %d, want exactly 1", saves)
		}
		if requests := fixture.provider.Requests(); len(requests) != 1 {
			t.Fatalf("save failure retried Provider/tool iteration: requests=%d", len(requests))
		}
		if !terminalProgress {
			t.Fatal("save failure omitted terminal progress after the ordered commit attempt")
		}
	})

	t.Run("event_failure_keeps_execution_facts_and_saves_once", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		hooks := &cancelAfterToolHook{Runtime: hook.Noop(), cancel: cancel}
		fixture := newOrderedCommitFixture(t, hooks, nil)
		_, _ = runOrderedCommitRequest(t, ctx, fixture)
		assertOrderedToolsExecutedOnce(t, fixture)
		assertOrderedExecutionFacts(t, fixture.conv)
		if saves := len(fixture.store.Calls().Save); saves != 1 {
			t.Fatalf("event-failed ordered tool saves = %d, want exactly 1", saves)
		}
		if requests := fixture.provider.Requests(); len(requests) != 1 {
			t.Fatalf("event failure retried Provider/tool iteration: requests=%d", len(requests))
		}
	})
}

func runOrderedCommitRequest(t *testing.T, ctx context.Context, fixture orderedCommitFixture) ([]string, bool) {
	t.Helper()
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := fixture.orch.SendRequest(requestCtx, fixture.conv, RunRequest{UserText: "run ordered tools", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	var resultOrder []string
	var terminalProgress bool
	for event := range stream {
		if event.Type == events.ToolWaitingConfirmation {
			if event.Confirmation == nil || event.Confirmation.ConfirmationID == "" {
				t.Fatal("tool confirmation omitted its opaque identity")
			}
			if !fixture.orch.ResolveToolConfirmation(events.ToolConfirmationDecision{
				ConfirmationID: event.Confirmation.ConfirmationID,
				CallID:         event.Confirmation.CallID,
				Allowed:        true,
				Action:         events.PermissionAllowOnce,
			}) {
				t.Fatal("tool confirmation decision was not accepted")
			}
		}
		if event.Type == events.ToolSuccess && event.Tool != nil {
			resultOrder = append(resultOrder, event.Tool.CallID)
		}
		if event.Type == events.AgentProgressed && event.Progress != nil && event.Progress.StopReason != "" {
			terminalProgress = true
		}
	}
	return resultOrder, terminalProgress
}

func assertOrderedToolsExecutedOnce(t *testing.T, fixture orderedCommitFixture) {
	t.Helper()
	if first, second := fixture.first.executions.Load(), fixture.second.executions.Load(); first != 1 || second != 1 {
		t.Fatalf("ordered tool executions = (%d,%d), want (1,1)", first, second)
	}
}

func assertOrderedExecutionFacts(t *testing.T, conv *conversation.Conversation) {
	t.Helper()
	var callIDs []string
	for _, message := range conv.Messages {
		if (message.Role == conversation.RoleToolCall || message.Role == conversation.RoleToolResult) && message.Tool != nil {
			callIDs = append(callIDs, message.Tool.CallID)
		}
	}
	want := []string{"call-first", "call-first", "call-second", "call-second"}
	if len(callIDs) != len(want) {
		t.Fatalf("ordered execution facts = %v, want %v", callIDs, want)
	}
	for index := range want {
		if callIDs[index] != want[index] {
			t.Fatalf("ordered execution facts = %v, want %v", callIDs, want)
		}
	}
}
