package orchestrator

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

const isolatedReviewSkill = `---
name: review
description: Review in isolation
allowed_tools: [Read, Glob, Grep, Bash]
mode: isolated
history: 1
---
ISOLATED REVIEW {{args}}
`

type gatedSkillProvider struct {
	release chan struct{}
}

func (*gatedSkillProvider) Name() string { return "gated-skill" }

func (p *gatedSkillProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (provider.ChatStream, error) {
	out := make(chan provider.StreamEvent)
	go func() {
		defer close(out)
		select {
		case out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: testSafeText(strings.Repeat("visible progress ", 128))}:
		case <-ctx.Done():
			return
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		select {
		case out <- provider.StreamEvent{Type: provider.StreamEventDone}:
		case <-ctx.Done():
		}
	}()
	return newOrchestratorTestChatStreamFromChannel(out, nil), nil
}

type failingIndependentProvider struct {
	calls atomic.Int32
}

func (*failingIndependentProvider) Name() string { return "failing-independent" }

func (p *failingIndependentProvider) StreamChat(context.Context, provider.ChatRequest) (provider.ChatStream, error) {
	p.calls.Add(1)
	return nil, errors.New("independent provider failed")
}

type fixedIndependentStreamProvider struct {
	stream  *orchestratorTestChatStream
	entered chan struct{}
	calls   atomic.Int32
}

func (*fixedIndependentStreamProvider) Name() string { return "fixed-independent-stream" }

func (p *fixedIndependentStreamProvider) StreamChat(context.Context, provider.ChatRequest) (provider.ChatStream, error) {
	p.calls.Add(1)
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	return p.stream, nil
}

type contextBoundIndependentProvider struct {
	calls   atomic.Int32
	started chan struct{}
	stopped chan struct{}
}

func newContextBoundIndependentProvider() *contextBoundIndependentProvider {
	return &contextBoundIndependentProvider{started: make(chan struct{}, 1), stopped: make(chan struct{}, 1)}
}

func (*contextBoundIndependentProvider) Name() string { return "context-bound-independent" }

func (p *contextBoundIndependentProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (provider.ChatStream, error) {
	p.calls.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	out := make(chan provider.StreamEvent)
	go func() {
		defer close(out)
		<-ctx.Done()
		select {
		case p.stopped <- struct{}{}:
		default:
		}
	}()
	return newOrchestratorTestChatStreamFromChannel(out, nil), nil
}

func TestIndependentRequestHasNoHistoryInjection(t *testing.T) {
	typeOfRequest := reflect.TypeOf(IndependentRequest{})
	wantFields := []string{"Invocation", "Main", "Profile", "UserText", "Mode"}
	if typeOfRequest.NumField() != len(wantFields) {
		t.Fatalf("IndependentRequest fields = %d, want closed set %v", typeOfRequest.NumField(), wantFields)
	}
	for index, want := range wantFields {
		field := typeOfRequest.Field(index)
		if field.Name != want {
			t.Fatalf("IndependentRequest field %d = %q, want %q", index, field.Name, want)
		}
		if field.Name == "History" || field.Type == reflect.TypeOf([]conversation.Message(nil)) {
			t.Fatalf("IndependentRequest exposes history injection field %#v", field)
		}
	}
}

func TestIndependentHistoryBuildsBaseBeforeSelection(t *testing.T) {
	base := provider.ChatRequest{
		Model: "base-model",
		DynamicSystem: []provider.SystemBlock{{
			Name: "complete-base", Content: testSafeText(strings.Repeat("complete isolated base ", 256)),
		}},
		Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: testSafeText("current isolated request")}},
	}
	main := &conversation.Conversation{Messages: []conversation.Message{
		{Role: conversation.RoleUser, Content: testSafeText("older user")},
		{Role: conversation.RoleAssistant, Content: testSafeText("older answer")},
		{Role: conversation.RoleUser, Content: testSafeText("newest user")},
		{Role: conversation.RoleAssistant, Content: testSafeText("newest answer")},
	}}
	budgeter := contextmgr.NewRequestBudgeter()
	baseMeasure, err := budgeter.MeasureRequest(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	newestMeasure, err := budgeter.MeasureConversationTurn(context.Background(), main.Messages[2:4])
	if err != nil {
		t.Fatal(err)
	}
	olderMeasure, err := budgeter.MeasureConversationTurn(context.Background(), main.Messages[0:2])
	if err != nil {
		t.Fatal(err)
	}
	maxSessionBytes := baseMeasure.Bytes + newestMeasure.Bytes
	emptyMeasure, err := budgeter.MeasureRequest(context.Background(), provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	withoutBase, err := addSkillHistoryMeasures(emptyMeasure, newestMeasure)
	if err != nil {
		t.Fatal(err)
	}
	withoutBase, err = addSkillHistoryMeasures(withoutBase, olderMeasure)
	if err != nil {
		t.Fatal(err)
	}
	if withoutBase.Bytes > maxSessionBytes {
		t.Fatalf("fixture cannot distinguish base-first selection: no-base=%d cap=%d", withoutBase.Bytes, maxSessionBytes)
	}

	orch := newSkillHistorySelectorForTest(t, maxSessionBytes, 10_000_000, 1)
	request, err := orch.applyIndependentSkillHistory(withIndependentSkillHistory(context.Background(), main, 2), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 3 || request.Messages[0].Content.Text() != "newest user" ||
		request.Messages[1].Content.Text() != "newest answer" || request.Messages[2].Content.Text() != "current isolated request" {
		t.Fatalf("base-first history request messages = %#v", request.Messages)
	}
	if request.Model != base.Model || !reflect.DeepEqual(request.DynamicSystem, base.DynamicSystem) {
		t.Fatalf("history assembly changed the complete base: got=%#v base=%#v", request, base)
	}
	if len(base.Messages) != 1 || base.Messages[0].Content.Text() != "current isolated request" {
		t.Fatalf("history assembly mutated its base request: %#v", base.Messages)
	}
}

func TestIndependentHistoryFinalRemeasurePrecedesExternalEffects(t *testing.T) {
	orch := newSkillHistorySelectorForTest(t, 1<<30, 10_000_000, 1)
	temporary := conversation.NewConversation("independent-final-remeasure", time.Unix(1_700_000_000, 0))
	temporary.Messages = append(temporary.Messages, conversation.Message{
		Role: conversation.RoleUser, Content: testSafeText("current isolated request"),
	})
	profile := skill.ExecutionProfile{}
	capture := &captureProvider{}
	orch.provider = capture
	stream, err := orch.streamWithExecutionState(
		context.Background(), temporary, RunModeDefault, profile, false, 1, hook.ExecutionRef{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProviderStream(stream); err != nil {
		t.Fatal(err)
	}

	main := &conversation.Conversation{Messages: []conversation.Message{
		{Role: conversation.RoleUser, Content: testSafeText("history user")},
		{Role: conversation.RoleAssistant, Content: testSafeText("history answer")},
	}}
	probe := &historySelectionCancelContext{}
	if _, err := orch.selectMeasuredSkillHistory(probe, main, 1, capture.request); err != nil {
		t.Fatal(err)
	}
	if probe.checks == 0 {
		t.Fatal("history-selection probe observed no context checks")
	}

	failing := &failingIndependentProvider{}
	orch.provider = failing
	// selectMeasuredSkillHistory consumes exactly probe.checks checks for this
	// immutable request. The next check is the post-selection barrier; canceling
	// on the following check enters the mandatory final whole-request measure.
	canceling := &historySelectionCancelContext{cancelAfterChecks: probe.checks + 2}
	ctx := withIndependentSkillHistory(canceling, main, 1)
	stream, err = orch.streamWithExecutionState(ctx, temporary, RunModeDefault, profile, false, 1, hook.ExecutionRef{}, nil)
	if stream != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("final remeasure cancellation = (%v,%v), want nil,context.Canceled", stream, err)
	}
	if canceling.checks != canceling.cancelAfterChecks {
		t.Fatalf("cancellation checks = %d, want final-measure boundary %d", canceling.checks, canceling.cancelAfterChecks)
	}
	if calls := failing.calls.Load(); calls != 0 {
		t.Fatalf("Provider observed request before final remeasure: calls=%d", calls)
	}
}

func TestIndependentHistoryBudgetFailureHasNoSideEffect(t *testing.T) {
	orch, main, _, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("must not run")},
		{Type: provider.StreamEventDone},
	}})
	conversation.AppendUserMessage(main, "previous request")
	conversation.AppendAssistantMessage(main, "previous answer")
	before := cloneMessages(main.Messages)
	policy, err := NewSkillHistoryPolicy(1, 16, 1)
	if err != nil {
		t.Fatal(err)
	}
	orch.skillHistoryPolicy = policy
	store := &countingSaveStore{Store: orch.store}
	memoryRecorder := &countingMemory{}
	orch.store = store
	orch.memory = memoryRecorder
	prepared, err := orch.PrepareSkill(skill.Invocation{Name: "review", Origin: skill.OriginSlash}, skill.NewActivity())
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan events.Event, 32)
	result, runErr := orch.RunIndependent(context.Background(), IndependentRequest{
		Invocation: prepared,
		Main:       main,
		Mode:       RunModeDefault,
	}, out)
	if runErr == nil || result.Reason != StopReasonProviderError {
		t.Fatalf("budget failure result = %#v err=%v", result, runErr)
	}
	if requests := scripted.Requests(); len(requests) != 0 {
		t.Fatalf("budget failure reached Provider: %#v", requests)
	}
	if store.saves != 0 || memoryRecorder.updates != 0 {
		t.Fatalf("budget failure produced Store/Memory effects: saves=%d updates=%d", store.saves, memoryRecorder.updates)
	}
	if !reflect.DeepEqual(main.Messages, before) {
		t.Fatalf("budget failure mutated main conversation: before=%#v after=%#v", before, main.Messages)
	}
}

func TestIndependentModelOverrideRemainsCompatible(t *testing.T) {
	orch, main, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"modeled.md": `---
name: modeled
description: Isolated model override with history
mode: isolated
history: 1
model: child-model
---
MODELED CHILD
`}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("child result")},
		{Type: provider.StreamEventDone},
	}})
	conversation.AppendUserMessage(main, "previous request")
	conversation.AppendAssistantMessage(main, "previous answer")
	eventStream, _, err := orch.SendSkill(context.Background(), main, skill.Invocation{
		Name: "modeled", Raw: "/modeled", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 1 || requests[0].Model != "child-model" {
		t.Fatalf("isolated model override request = %#v", requests)
	}
	if len(requests[0].Messages) < 3 || requests[0].Messages[0].Content.Text() != "previous request" ||
		requests[0].Messages[1].Content.Text() != "previous answer" || requests[0].Messages[2].Content.Text() != "/modeled" {
		t.Fatalf("model override history assembly = %#v", requests[0].Messages)
	}
}

func TestIndependentClosesStreamOnProviderFailure(t *testing.T) {
	orch, main, _, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	prepared, err := orch.PrepareSkill(skill.Invocation{Name: "review", Origin: skill.OriginSlash}, skill.NewActivity())
	if err != nil {
		t.Fatal(err)
	}
	providerStream := newOrchestratorTestChatStream(provider.StreamEvent{
		Type: provider.StreamEventError, Error: testSafeProviderError(errors.New("independent stream failed")),
	})
	tracked := &fixedIndependentStreamProvider{stream: providerStream}
	orch.provider = tracked
	result, runErr := orch.RunIndependent(context.Background(), IndependentRequest{
		Invocation: prepared,
		Main:       main,
		Mode:       RunModeDefault,
	}, make(chan events.Event, 16))
	if runErr == nil || result.Reason != StopReasonProviderError {
		t.Fatalf("independent Provider failure = %#v err=%v", result, runErr)
	}
	if calls := tracked.calls.Load(); calls != 1 {
		t.Fatalf("independent Provider calls = %d, want 1", calls)
	}
	if closes := providerStream.CloseCalls(); closes != 1 {
		t.Fatalf("independent Provider failure stream closes = %d, want 1", closes)
	}
}

func TestIndependentCancellationClosesStream(t *testing.T) {
	orch, main, _, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	prepared, err := orch.PrepareSkill(skill.Invocation{Name: "review", Origin: skill.OriginSlash}, skill.NewActivity())
	if err != nil {
		t.Fatal(err)
	}
	providerEvents := make(chan provider.StreamEvent)
	providerStream := newOrchestratorTestChatStreamFromChannel(providerEvents, nil)
	tracked := &fixedIndependentStreamProvider{stream: providerStream, entered: make(chan struct{}, 1)}
	orch.provider = tracked
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result RunResult
		err    error
	}, 1)
	go func() {
		result, runErr := orch.RunIndependent(ctx, IndependentRequest{
			Invocation: prepared,
			Main:       main,
			Mode:       RunModeDefault,
		}, make(chan events.Event, 16))
		done <- struct {
			result RunResult
			err    error
		}{result: result, err: runErr}
	}()
	select {
	case <-tracked.entered:
	case <-time.After(time.Second):
		t.Fatal("independent Provider did not start")
	}
	cancel()
	select {
	case outcome := <-done:
		if outcome.err == nil || outcome.result.Reason != StopReasonCancelled {
			t.Fatalf("independent cancellation = %#v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent cancellation did not return")
	}
	if closes := providerStream.CloseCalls(); closes != 1 {
		t.Fatalf("independent cancellation stream closes = %d, want 1", closes)
	}
}

func TestIndependentActivityClearsOnEveryExit(t *testing.T) {
	testCases := []struct {
		name       string
		events     []provider.StreamEvent
		cancel     bool
		noConsumer bool
	}{
		{
			name: "success",
			events: []provider.StreamEvent{
				{Type: provider.StreamEventTextDelta, Delta: testSafeText("independent summary")},
				{Type: provider.StreamEventDone},
			},
		},
		{
			name:   "provider_failure",
			events: []provider.StreamEvent{{Type: provider.StreamEventError, Error: testSafeProviderError(errors.New("failed"))}},
		},
		{name: "cancellation", cancel: true},
		{name: "no_consumer_cancellation", cancel: true, noConsumer: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			orch, main, _, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
			prepared, err := orch.PrepareSkill(skill.Invocation{Name: "review", Origin: skill.OriginSlash}, skill.NewActivity())
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Activity == nil || len(prepared.Activity.Snapshot().Active) != 1 {
				t.Fatalf("isolated Activity was not activated: %#v", prepared.Activity)
			}
			providerEvents := make(chan provider.StreamEvent, len(testCase.events))
			for _, event := range testCase.events {
				providerEvents <- event
			}
			if !testCase.cancel {
				close(providerEvents)
			}
			providerStream := newOrchestratorTestChatStreamFromChannel(providerEvents, nil)
			tracked := &fixedIndependentStreamProvider{stream: providerStream, entered: make(chan struct{}, 1)}
			orch.provider = tracked
			ctx, cancel := context.WithCancel(context.Background())
			out := make(chan events.Event, 16)
			if testCase.noConsumer {
				out = make(chan events.Event)
			}
			done := make(chan error, 1)
			go func() {
				_, runErr := orch.RunIndependent(ctx, IndependentRequest{
					Invocation: prepared,
					Main:       main,
					Mode:       RunModeDefault,
				}, out)
				done <- runErr
			}()
			select {
			case <-tracked.entered:
			case <-time.After(time.Second):
				t.Fatal("independent Provider did not start")
			}
			if testCase.cancel {
				cancel()
			}
			select {
			case runErr := <-done:
				if testCase.name == "success" && runErr != nil {
					t.Fatalf("successful independent run: %v", runErr)
				}
				if testCase.name != "success" && runErr == nil {
					t.Fatal("non-success independent run returned no error")
				}
			case <-time.After(time.Second):
				t.Fatal("independent run did not finish")
			}
			cancel()
			if active := prepared.Activity.Snapshot().Active; len(active) != 0 {
				t.Fatalf("independent Activity remained active after %s: %#v", testCase.name, active)
			}
		})
	}
}

func TestSkillHistorySelectionHonorsTurnsBytesAndPlanningTokens(t *testing.T) {
	base := provider.ChatRequest{}
	budgeter := contextmgr.NewRequestBudgeter()
	baseMeasure, err := budgeter.MeasureRequest(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	main := &conversation.Conversation{Messages: []conversation.Message{
		{Role: conversation.RoleUser, Content: testSafeText("user 1")},
		{Role: conversation.RoleAssistant, Content: testSafeText("answer 1")},
		{Role: conversation.RoleUser, Content: testSafeText("user 2")},
		{Role: conversation.RoleAssistant, Content: testSafeText("answer 2")},
		{Role: conversation.RoleUser, Content: testSafeText("user 3")},
		{Role: conversation.RoleAssistant, Content: testSafeText("answer 3")},
	}}
	newestMeasure, err := budgeter.MeasureConversationTurn(context.Background(), main.Messages[4:6])
	if err != nil {
		t.Fatal(err)
	}

	t.Run("turns", func(t *testing.T) {
		selector := newSkillHistorySelectorForTest(t, 1<<30, 10_000_000, 1)
		selection, err := selector.selectSkillHistory(context.Background(), main, 2, base)
		if err != nil {
			t.Fatal(err)
		}
		if selection.Turns != 2 || !selection.Truncated || selection.Reason != HistoryLimitTurns || len(selection.Messages) != 4 ||
			selection.Messages[0].Content.Text() != "user 2" || selection.Messages[3].Content.Text() != "answer 3" {
			t.Fatalf("turn-limited selection = %#v", selection)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		selector := newSkillHistorySelectorForTest(t, baseMeasure.Bytes+newestMeasure.Bytes, 10_000_000, 1)
		selection, err := selector.selectSkillHistory(context.Background(), main, 3, base)
		if err != nil {
			t.Fatal(err)
		}
		if selection.Turns != 1 || !selection.Truncated || selection.Reason != HistoryLimitBytes || len(selection.Messages) != 2 ||
			selection.Messages[0].Content.Text() != "user 3" {
			t.Fatalf("byte-limited selection = %#v", selection)
		}
	})

	t.Run("planning_tokens", func(t *testing.T) {
		planningLimit := baseMeasure.PlanningTokens + newestMeasure.PlanningTokens
		selector := newSkillHistorySelectorForTest(t, 1<<30, planningLimit+1, 1)
		selection, err := selector.selectSkillHistory(context.Background(), main, 3, base)
		if err != nil {
			t.Fatal(err)
		}
		if selection.Turns != 1 || !selection.Truncated || selection.Reason != HistoryLimitPlanningTokens || len(selection.Messages) != 2 ||
			selection.Messages[0].Content.Text() != "user 3" {
			t.Fatalf("planning-token-limited selection = %#v", selection)
		}
	})

	if measure, err := addSkillHistoryMeasures(contextmgr.RequestMeasure{Bytes: -1}, contextmgr.RequestMeasure{}); err == nil || measure != (contextmgr.RequestMeasure{}) {
		t.Fatalf("negative measure was accepted: measure=%#v err=%v", measure, err)
	}
	if measure, err := addSkillHistoryMeasures(contextmgr.RequestMeasure{Bytes: math.MaxInt64, PlanningTokens: math.MaxInt64}, contextmgr.RequestMeasure{Bytes: 1, PlanningTokens: 1}); err == nil || measure != (contextmgr.RequestMeasure{}) {
		t.Fatalf("overflowing measure was accepted: measure=%#v err=%v", measure, err)
	}
}

func TestSkillHistoryLimitReasonPriorityIsTurnsBytesPlanningTokens(t *testing.T) {
	base := provider.ChatRequest{}
	budgeter := contextmgr.NewRequestBudgeter()
	baseMeasure, err := budgeter.MeasureRequest(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	main := &conversation.Conversation{Messages: []conversation.Message{
		{Role: conversation.RoleUser, Content: testSafeText("older")},
		{Role: conversation.RoleAssistant, Content: testSafeText("older answer")},
		{Role: conversation.RoleUser, Content: testSafeText("newer")},
		{Role: conversation.RoleAssistant, Content: testSafeText("newer answer")},
	}}
	newestMeasure, err := budgeter.MeasureConversationTurn(context.Background(), main.Messages[2:4])
	if err != nil {
		t.Fatal(err)
	}

	turnSelector := newSkillHistorySelectorForTest(
		t, baseMeasure.Bytes+newestMeasure.Bytes, baseMeasure.PlanningTokens+newestMeasure.PlanningTokens+1, 1,
	)
	turnSelection, err := turnSelector.selectSkillHistory(context.Background(), main, 1, base)
	if err != nil || !turnSelection.Truncated || turnSelection.Reason != HistoryLimitTurns || turnSelection.Turns != 1 {
		t.Fatalf("turn priority selection = %#v err=%v", turnSelection, err)
	}

	bothSelector := newSkillHistorySelectorForTest(t, baseMeasure.Bytes+1, baseMeasure.PlanningTokens+2, 1)
	bothSelection, err := bothSelector.selectSkillHistory(context.Background(), main, 2, base)
	if err != nil || !bothSelection.Truncated || bothSelection.Reason != HistoryLimitBytes || bothSelection.Turns != 0 {
		t.Fatalf("byte priority selection = %#v err=%v", bothSelection, err)
	}

	tokenSelector := newSkillHistorySelectorForTest(t, 1<<30, baseMeasure.PlanningTokens+2, 1)
	tokenSelection, err := tokenSelector.selectSkillHistory(context.Background(), main, 2, base)
	if err != nil || !tokenSelection.Truncated || tokenSelection.Reason != HistoryLimitPlanningTokens || tokenSelection.Turns != 0 {
		t.Fatalf("planning priority selection = %#v err=%v", tokenSelection, err)
	}
}

func TestSkillHistorySelectionReturnsOnlyCompleteNonAliasedModelMessages(t *testing.T) {
	main := &conversation.Conversation{Messages: []conversation.Message{
		{Role: conversation.RoleUser, Content: testSafeText("request")},
		{Role: conversation.RoleThinking, Content: testSafeText("private thinking")},
		{Role: conversation.RoleAssistant, Content: testSafeText("calling")},
		{Role: conversation.RoleToolCall, Content: testSafeText("Read"), Tool: &conversation.ToolState{
			CallID: "call-1", Name: "Read", ArgumentsJSON: testSafeText(`{"path":"safe.txt"}`), State: tool.Prepared,
		}},
		{Role: conversation.RoleToolResult, Content: testSafeText("persisted result"), Tool: &conversation.ToolState{
			CallID: "call-1", Name: "Read", Result: testSafeText("persisted result"), State: tool.Completed, Status: tool.StatusSuccess,
		}},
		{Role: conversation.RoleAssistant, Content: testSafeText("done")},
	}}
	selector := newSkillHistorySelectorForTest(t, 1<<30, 10_000_000, 1)
	selection, err := selector.selectSkillHistory(context.Background(), main, 1, provider.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	wantRoles := []provider.ModelMessageRole{
		provider.ModelMessageRoleUser,
		provider.ModelMessageRoleAssistant,
		provider.ModelMessageRoleToolCall,
		provider.ModelMessageRoleToolResult,
		provider.ModelMessageRoleAssistant,
	}
	if selection.Turns != 1 || selection.Truncated || selection.Reason != HistoryLimitNone || len(selection.Messages) != len(wantRoles) {
		t.Fatalf("complete selection = %#v", selection)
	}
	for index, role := range wantRoles {
		if selection.Messages[index].Role != role {
			t.Fatalf("message %d role = %q, want %q", index, selection.Messages[index].Role, role)
		}
	}
	if selection.Messages[3].ToolResult.Text() != "persisted result" || selection.Messages[3].ToolResultStatus != string(tool.StatusSuccess) {
		t.Fatalf("tool result projection = %#v", selection.Messages[3])
	}
	selection.Messages[0].Content = testSafeText("mutated")
	selection.Messages[2].ToolName = "mutated"
	selection.Messages[3].ToolResult = testSafeText("mutated")
	if main.Messages[0].Content.Text() != "request" || main.Messages[3].Tool.Name != "Read" || main.Messages[4].Tool.Result.Text() != "persisted result" {
		t.Fatalf("selected Provider DTO aliases main conversation: %#v", main.Messages)
	}
}

func TestSkillHistorySelectionCancellationPublishesNothing(t *testing.T) {
	main := &conversation.Conversation{Messages: []conversation.Message{
		{Role: conversation.RoleUser, Content: testSafeText(strings.Repeat("older ", 32*1024))},
		{Role: conversation.RoleAssistant, Content: testSafeText(strings.Repeat("answer ", 32*1024))},
		{Role: conversation.RoleUser, Content: testSafeText("newer")},
		{Role: conversation.RoleAssistant, Content: testSafeText("newer answer")},
	}}
	selector := newSkillHistorySelectorForTest(t, 1<<30, 10_000_000, 1)
	probe := &historySelectionCancelContext{}
	selection, err := selector.selectSkillHistory(probe, main, 2, provider.ChatRequest{})
	if err != nil || selection.Turns != 2 || probe.checks < 4 {
		t.Fatalf("probe selection = %#v checks=%d err=%v", selection, probe.checks, err)
	}
	canceling := &historySelectionCancelContext{cancelAfterChecks: probe.checks}
	selection, err = selector.selectSkillHistory(canceling, main, 2, provider.ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation error = %v", err)
	}
	assertZeroSkillHistorySelection(t, selection)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	selection, err = selector.selectSkillHistory(canceled, main, 2, provider.ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled selection error = %v", err)
	}
	assertZeroSkillHistorySelection(t, selection)
}

func TestSkillHistoryZeroReturnsEmptyUntruncatedWithoutMeasurement(t *testing.T) {
	selector := newSkillHistorySelectorForTest(t, 4096, 4096, 512)
	invalidBase := provider.ChatRequest{Thinking: config.ThinkingConfig{BudgetTokens: -1}}
	selection, err := selector.selectSkillHistory(context.Background(), nil, 0, invalidBase)
	if err != nil {
		t.Fatalf("zero history measured or constructed its base: %v", err)
	}
	assertZeroSkillHistorySelection(t, selection)

	for _, requested := range []int{-1, skillHistoryMaxTurns + 1} {
		selection, err := selector.selectSkillHistory(context.Background(), nil, requested, invalidBase)
		if err == nil {
			t.Fatalf("invalid requested=%d was accepted", requested)
		}
		assertZeroSkillHistorySelection(t, selection)
	}

	invalidPolicy := &Orchestrator{skillHistoryBudgeter: contextmgr.NewRequestBudgeter()}
	selection, err = invalidPolicy.selectSkillHistory(context.Background(), nil, 0, invalidBase)
	if err == nil {
		t.Fatal("zero SkillHistoryPolicy was accepted")
	}
	assertZeroSkillHistorySelection(t, selection)

	policy, err := NewSkillHistoryPolicy(4096, 4096, 512)
	if err != nil {
		t.Fatal(err)
	}
	invalidBudgeter := &Orchestrator{skillHistoryPolicy: policy}
	selection, err = invalidBudgeter.selectSkillHistory(context.Background(), nil, 0, invalidBase)
	if err == nil {
		t.Fatal("zero RequestBudgeter was accepted")
	}
	assertZeroSkillHistorySelection(t, selection)

	forgedPolicy := policy
	forgedPolicy.maxTurns++
	forged := &Orchestrator{skillHistoryPolicy: forgedPolicy, skillHistoryBudgeter: contextmgr.NewRequestBudgeter()}
	selection, err = forged.selectSkillHistory(context.Background(), nil, 0, invalidBase)
	if err == nil {
		t.Fatal("forged SkillHistoryPolicy was accepted")
	}
	assertZeroSkillHistorySelection(t, selection)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	selection, err = selector.selectSkillHistory(canceled, nil, 0, invalidBase)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("zero history ignored cancellation: %v", err)
	}
	assertZeroSkillHistorySelection(t, selection)
	selection, err = selector.selectSkillHistory(nil, nil, 0, invalidBase)
	if err == nil {
		t.Fatal("nil context was accepted")
	}
	assertZeroSkillHistorySelection(t, selection)
}

func newSkillHistorySelectorForTest(t *testing.T, maxSessionBytes, modelWindowTokens, autoMarginTokens int64) *Orchestrator {
	t.Helper()
	policy, err := NewSkillHistoryPolicy(maxSessionBytes, modelWindowTokens, autoMarginTokens)
	if err != nil {
		t.Fatalf("construct history policy: %v", err)
	}
	return &Orchestrator{skillHistoryPolicy: policy, skillHistoryBudgeter: contextmgr.NewRequestBudgeter()}
}

func assertZeroSkillHistorySelection(t *testing.T, selection SkillHistorySelection) {
	t.Helper()
	if len(selection.Messages) != 0 || selection.Turns != 0 || selection.Truncated || selection.Reason != HistoryLimitNone {
		t.Fatalf("selection published partial state: %#v", selection)
	}
}

type historySelectionCancelContext struct {
	checks            int
	cancelAfterChecks int
}

func (c *historySelectionCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *historySelectionCancelContext) Done() <-chan struct{}       { return nil }
func (c *historySelectionCancelContext) Value(any) any               { return nil }

func (c *historySelectionCancelContext) Err() error {
	c.checks++
	if c.cancelAfterChecks > 0 && c.checks >= c.cancelAfterChecks {
		return context.Canceled
	}
	return nil
}

func TestExecuteIsolatedSkillCommandReturnsOnlySummary(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("review summary")},
		{Type: provider.StreamEventDone},
	}})
	conversation.AppendUserMessage(conv, "previous request")
	conversation.AppendAssistantMessage(conv, "previous answer")

	eventStream, prepared, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Args: "workspace", Raw: "/review workspace", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Mode != skill.ModeIsolated {
		t.Fatalf("unexpected mode: %s", prepared.Mode)
	}
	var transientText strings.Builder
	var finalText bool
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		if event.Type != events.TextDelta {
			continue
		}
		if event.Transient {
			if event.IndependentID == "" {
				t.Fatal("transient independent event is missing its identifier")
			}
			transientText.WriteString(event.Text.Text())
		} else if event.Text.Text() == "review summary" {
			finalText = true
		}
	}
	if transientText.String() != "review summary" || !finalText {
		t.Fatalf("missing transient or final text: transient=%q final=%v", transientText.String(), finalText)
	}
	requests := scripted.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected one provider call without second summarizer, got %d", len(requests))
	}
	if !systemBlocksContain(requests[0].DynamicSystem, "ISOLATED REVIEW workspace") {
		t.Fatalf("isolated SOP missing: %#v", requests[0].DynamicSystem)
	}
	if len(requests[0].Messages) != 3 || requests[0].Messages[0].Content.Text() != "previous request" || requests[0].Messages[2].Content.Text() != "/review workspace" {
		t.Fatalf("unexpected independent history: %#v", requests[0].Messages)
	}
	if len(conv.Messages) != 4 || conv.Messages[2].Content.Text() != "/review workspace" || conv.Messages[3].Content.Text() != "review summary" {
		t.Fatalf("unexpected main history: %#v", conv.Messages)
	}
	for _, message := range conv.Messages {
		if message.Role == conversation.RoleToolCall || message.Role == conversation.RoleToolResult || strings.Contains(message.Content.Text(), "ISOLATED REVIEW") {
			t.Fatalf("independent internals leaked into main history: %#v", conv.Messages)
		}
	}
	if len(activity.Snapshot().Active) != 0 {
		t.Fatalf("isolated skill polluted main activity: %#v", activity.Snapshot())
	}
}

func TestIndependentTextIsVisibleBeforeProviderCompletes(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	gated := &gatedSkillProvider{release: make(chan struct{})}
	orch.provider = gated
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-eventStream:
			if !ok {
				if !released {
					t.Fatal("independent output completed without a live transient text event")
				}
				return
			}
			if event.Type == events.Error {
				t.Fatal(event.Err)
			}
			if event.Type == events.TextDelta && event.Transient && event.Text.Text() != "" && !released {
				close(gated.release)
				released = true
			}
		case <-timer.C:
			if !released {
				close(gated.release)
			}
			t.Fatal("independent text was buffered until provider completion")
		}
	}
}

func TestIndependentDoesNotInheritMainSharedActivity(t *testing.T) {
	orch, conv, mainActivity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"review.md": isolatedReviewSkill,
		"shared.md": `---
name: shared
description: Shared workflow
mode: shared
---
MAIN SHARED SOP
`,
	}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("isolated result")},
		{Type: provider.StreamEventDone},
	}})
	if _, err := orch.PrepareSkill(skill.Invocation{Name: "shared", Origin: skill.OriginSlash}, mainActivity); err != nil {
		t.Fatal(err)
	}
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, mainActivity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 1 || !systemBlocksContain(requests[0].DynamicSystem, "ISOLATED REVIEW") || systemBlocksContain(requests[0].DynamicSystem, "MAIN SHARED SOP") {
		t.Fatalf("isolated request inherited the main shared activity: %#v", requests)
	}
	if active := mainActivity.Snapshot().Active; len(active) != 1 || active[0].Name != "shared" {
		t.Fatalf("isolated run mutated the main activity: %#v", active)
	}
}

func TestIndependentModelOverrideDoesNotLeakToMainRequests(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"modeled.md": `---
name: modeled
description: Isolated model override
mode: isolated
history: 0
model: child-model
---
MODELED CHILD
`}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("child result")}, {Type: provider.StreamEventDone}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("main result")}, {Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "modeled", Raw: "/modeled", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	mainStream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "ordinary request", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range mainStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 2 || requests[0].Model != "child-model" || requests[1].Model != "default-model" {
		t.Fatalf("isolated model leaked into main requests: %#v", requests)
	}
}

func TestAgentTriggeredIsolatedSkillEndsParentLoop(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("parent draft must disappear")}, {Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("load-review", tool.LoadSkillToolName, `{"name":"review","args":"changes"}`)}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("agent review summary")}, {Type: provider.StreamEventDone}},
	})

	eventStream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "review this", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if len(scripted.Requests()) != 2 {
		t.Fatalf("expected parent plus child provider calls only, got %d", len(scripted.Requests()))
	}
	if len(conv.Messages) != 2 || conv.Messages[0].Content.Text() != "review this" || conv.Messages[1].Content.Text() != "agent review summary" {
		t.Fatalf("unexpected main history: %#v", conv.Messages)
	}
	for _, message := range conv.Messages {
		if message.Tool != nil && message.Tool.Name == tool.LoadSkillToolName {
			t.Fatalf("isolated system tool call leaked into main history: %#v", conv.Messages)
		}
	}
}

func TestIndependentUsesOnlyCompletedChildIterationAsSummary(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("draft before tool")}, {Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("read", "Read", `{"path":"missing.txt"}`)}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("final child summary")}, {Type: provider.StreamEventDone}},
	})

	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if len(scripted.Requests()) != 2 {
		t.Fatalf("expected child tool iteration and final iteration, got %d", len(scripted.Requests()))
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Content.Text() != "final child summary" {
		t.Fatalf("intermediate child text was mistaken for the summary: %#v", conv.Messages)
	}
}

func TestRunIndependentCancellationDoesNotRequireOutputConsumer(t *testing.T) {
	orch, conv, _, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	providerImpl := &fixedIndependentStreamProvider{
		stream: newOrchestratorTestChatStream(
			provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: testSafeText("streaming child output")},
			provider.StreamEvent{Type: provider.StreamEventDone},
		),
		entered: make(chan struct{}, 1),
	}
	orch.provider = providerImpl
	definition, ok := orch.skillManager.Resolve("review")
	if !ok {
		t.Fatal("review skill not found")
	}
	activity := skill.NewActivity()
	activated, err := activity.Activate(definition, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := orch.RunIndependent(ctx, IndependentRequest{
			Invocation: skill.PreparedInvocation{Definition: definition, Activated: activated, Activity: activity, Mode: skill.ModeIsolated, History: 1},
			Main:       conv,
			Mode:       RunModeDefault,
		}, make(chan events.Event))
		done <- runErr
	}()
	select {
	case <-providerImpl.entered:
	case <-time.After(time.Second):
		t.Fatal("independent run did not enter Provider before cancellation")
	}
	cancel()
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("independent run deadlocked after cancellation with no output consumer")
	}
}

func TestIndependentProviderFailureDoesNotCreateSummary(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	failing := &failingIndependentProvider{}
	orch.provider = failing
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawError, sawSuccess bool
	for event := range eventStream {
		sawError = sawError || event.Type == events.Error
		sawSuccess = sawSuccess || event.Type == events.Done || event.Type == events.TextDelta && !event.Transient
	}
	if !sawError || sawSuccess {
		t.Fatalf("provider failure events reported the wrong terminal state: error=%v success=%v", sawError, sawSuccess)
	}
	assertIndependentHasNoSuccessSummary(t, conv, "/review")
	if got := failing.calls.Load(); got != 1 {
		t.Fatalf("provider failure made %d calls, want 1", got)
	}
}

func TestIndependentTimeoutStopsProviderWithoutSummary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	assertIndependentContextStop(t, ctx, nil)
}

func TestIndependentCancelStopsProviderWithoutSummary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assertIndependentContextStop(t, ctx, cancel)
}

func assertIndependentContextStop(t *testing.T, ctx context.Context, stop func()) {
	t.Helper()
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	blocking := newContextBoundIndependentProvider()
	orch.provider = blocking
	eventStream, _, err := orch.SendSkill(ctx, conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	outcome := make(chan bool, 1)
	go func() {
		sawSuccess := false
		for event := range eventStream {
			sawSuccess = sawSuccess || event.Type == events.Done || event.Type == events.TextDelta && !event.Transient
		}
		outcome <- sawSuccess
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("independent Provider did not start")
	}
	if stop != nil {
		stop()
	}
	select {
	case sawSuccess := <-outcome:
		if sawSuccess {
			t.Fatal("context termination emitted a successful independent summary")
		}
	case <-time.After(time.Second):
		t.Fatal("independent event stream did not stop after context termination")
	}
	select {
	case <-blocking.stopped:
	case <-time.After(time.Second):
		t.Fatal("Provider continued running after independent context termination")
	}
	assertIndependentHasNoSuccessSummary(t, conv, "/review")
	if got := blocking.calls.Load(); got != 1 {
		t.Fatalf("context-terminated run made %d Provider calls, want 1", got)
	}
}

func assertIndependentHasNoSuccessSummary(t *testing.T, conv *conversation.Conversation, raw string) {
	t.Helper()
	if len(conv.Messages) != 1 || conv.Messages[0].Role != conversation.RoleUser || conv.Messages[0].Content.Text() != raw {
		t.Fatalf("failed independent run wrote a success summary: %#v", conv.Messages)
	}
}

func TestIndependentStreamingRedactionSpansProviderChunks(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("api_key ")},
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("= split-secret;\n-----BEGIN PRIVATE ")},
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("KEY-----\nSUPERSECRETBASE64\n")},
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("-----END PRIVATE KEY-----\nreview complete")},
			{Type: provider.StreamEventDone},
		},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		output.WriteString(event.Text.Text())
	}
	for _, secret := range []string{"split-secret", "SUPERSECRETBASE64"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("streaming output leaked %q across provider chunks: %q", secret, output.String())
		}
		if len(conv.Messages) > 1 && strings.Contains(conv.Messages[1].Content.Text(), secret) {
			t.Fatalf("main summary leaked %q: %#v", secret, conv.Messages)
		}
	}
	if !strings.Contains(output.String(), "[redacted]") {
		t.Fatalf("expected redaction marker in output: %q", output.String())
	}
}

func TestIndependentUsesRuntimeRedactorAcrossPromptEventsAndHistory(t *testing.T) {
	const opaqueSecret = "opaque-runtime-value"
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"secure.md": `---
name: secure
description: Workflow containing opaque-runtime-value
mode: isolated
history: 0
---
Never expose opaque-runtime-value or {{args}}.
`}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("result contains opaque-runtime-value")}, {Type: provider.StreamEventDone}},
	})
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(opaqueSecret)
	orch.redact = runtimeRedactor.Text
	orch.redactionLookbehind = max(64, runtimeRedactor.MaxSecretBytes())

	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "secure", Args: opaqueSecret, Raw: "/secure " + opaqueSecret, Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var visible strings.Builder
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		visible.WriteString(event.Text.Text())
		if event.Tool != nil {
			visible.WriteString(event.Tool.Arguments.Text())
			visible.WriteString(event.Tool.Summary.Text())
		}
	}
	if strings.Contains(visible.String(), opaqueSecret) {
		t.Fatalf("runtime secret leaked in events: %q", visible.String())
	}
	for _, message := range conv.Messages {
		arguments := ""
		if message.Tool != nil {
			arguments = message.Tool.ArgumentsJSON.Text()
		}
		if strings.Contains(message.Content.Text(), opaqueSecret) || strings.Contains(arguments, opaqueSecret) {
			t.Fatalf("runtime secret leaked in main history: %#v", conv.Messages)
		}
	}
	for _, request := range scripted.Requests() {
		for _, block := range append(append([]provider.SystemBlock(nil), request.StableSystem...), request.DynamicSystem...) {
			if strings.Contains(block.Content.Text(), opaqueSecret) {
				t.Fatalf("runtime secret leaked in provider prompt: %q", block.Content.Text())
			}
		}
		for _, message := range request.Messages {
			if strings.Contains(message.Content.Text(), opaqueSecret) {
				t.Fatalf("runtime secret leaked in provider history: %#v", request.Messages)
			}
		}
	}
}

type failingSaveStore struct {
	conversation.Store
}

func (failingSaveStore) Save(context.Context, *conversation.Conversation) (conversation.SaveResult, error) {
	return conversation.SaveResult{}, errors.New("save failed")
}

type countingSaveStore struct {
	conversation.Store
	saves int
}

func (s *countingSaveStore) Save(context.Context, *conversation.Conversation) (conversation.SaveResult, error) {
	s.saves++
	return conversation.SaveResult{Kind: conversation.SaveNoop}, nil
}

type countingMemory struct {
	updates int
}

func (m *countingMemory) UpdateAsync(memory.UpdateInput) {
	m.updates++
}

func TestIndependentSummaryDoesNotMutateMainConversation(t *testing.T) {
	orch, conv, _, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("standalone result")}, {Type: provider.StreamEventDone}},
	})
	conversation.AppendUserMessage(conv, "existing request")
	conversation.AppendAssistantMessage(conv, "existing answer")
	before := cloneMessages(conv.Messages)
	store := &countingSaveStore{Store: orch.store}
	memoryRecorder := &countingMemory{}
	orch.store = store
	orch.memory = memoryRecorder
	prepared, err := orch.PrepareSkill(skill.Invocation{Name: "review", Origin: skill.OriginSlash}, skill.NewActivity())
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan events.Event, 32)
	result, err := orch.RunIndependent(context.Background(), IndependentRequest{
		Invocation: prepared,
		Main:       conv,
		Mode:       RunModeDefault,
	}, out)
	if err != nil || result.FinalText != "standalone result" {
		t.Fatalf("unexpected independent result: %#v %v", result, err)
	}
	if store.saves != 0 || memoryRecorder.updates != 0 {
		t.Fatalf("temporary run produced side effects: saves=%d memory=%d", store.saves, memoryRecorder.updates)
	}
	if !reflect.DeepEqual(conv.Messages, before) {
		t.Fatalf("temporary run mutated main messages: before=%#v after=%#v", before, conv.Messages)
	}
}

func TestIsolatedSummaryRollsBackWhenMainSaveFails(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("summary that must roll back")}, {Type: provider.StreamEventDone}},
	})
	orch.store = failingSaveStore{Store: orch.store}
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for event := range eventStream {
		if event.Type == events.Error {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("expected save failure event")
	}
	if len(conv.Messages) != 1 || conv.Messages[0].Content.Text() != "/review" {
		t.Fatalf("failed summary save left a hidden assistant message: %#v", conv.Messages)
	}
}

func TestIndependentNestedIsolatedSkillIsRecoverable(t *testing.T) {
	orch, conv, _, scripted := newSkillRuntimeFixture(t, map[string]string{
		"review.md": isolatedReviewSkill,
		"other.md": `---
name: other
description: Other isolated workflow
mode: isolated
history: 0
---
OTHER
`,
	}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("nested", tool.LoadSkillToolName, `{"name":"other"}`)}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("handled nested rejection")}, {Type: provider.StreamEventDone}},
	})
	definition, ok := orch.skillManager.Resolve("review")
	if !ok {
		t.Fatal("review skill not found")
	}
	temporaryActivity := skill.NewActivity()
	activated, err := temporaryActivity.Activate(definition, "")
	if err != nil {
		t.Fatal(err)
	}
	prepared := skill.PreparedInvocation{Definition: definition, Activated: activated, Mode: skill.ModeIsolated, History: 1}
	out := make(chan events.Event, 32)
	result, err := orch.RunIndependent(context.Background(), IndependentRequest{
		Invocation: prepared,
		Main:       conv,
		Mode:       RunModeDefault,
	}, out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText != "handled nested rejection" || len(scripted.Requests()) != 2 {
		t.Fatalf("unexpected nested result=%#v calls=%d", result, len(scripted.Requests()))
	}
	var recoverable bool
	for event := range out {
		if event.Tool != nil && event.Tool.Name == tool.LoadSkillToolName && event.Tool.Recoverable {
			recoverable = true
		}
	}
	if !recoverable {
		t.Fatal("nested isolated load did not return a recoverable tool error")
	}
}

func TestIndependentSharedLoadStaysInTemporaryActivity(t *testing.T) {
	orch, conv, mainActivity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"review.md": isolatedReviewSkill,
		"helper.md": `---
name: helper
description: Temporary shared helper
mode: shared
---
TEMPORARY HELPER SOP
`,
	}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("helper", tool.LoadSkillToolName, `{"name":"helper"}`)}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("temporary helper result")}, {Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, mainActivity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 2 || !systemBlocksContain(requests[1].DynamicSystem, "TEMPORARY HELPER SOP") || !systemBlocksContain(requests[1].DynamicSystem, "ISOLATED REVIEW") {
		t.Fatalf("shared helper did not activate in the child context: %#v", requests)
	}
	if len(mainActivity.Snapshot().Active) != 0 {
		t.Fatalf("temporary shared helper polluted main activity: %#v", mainActivity.Snapshot())
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Content.Text() != "temporary helper result" {
		t.Fatalf("temporary tool history leaked into main conversation: %#v", conv.Messages)
	}
}

func TestIndependentEmptyFinalReplyDoesNotCreateSummary(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for event := range eventStream {
		if event.Type == events.Error {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("empty independent reply did not report a failure")
	}
	if len(conv.Messages) != 1 || conv.Messages[0].Content.Text() != "/review" {
		t.Fatalf("empty independent reply created a success summary: %#v", conv.Messages)
	}
}

func TestIndependentSkillReusesPermissionConfirmation(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("bash", "Bash", `{"command":"printf ok"}`)}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("permission-aware summary")}, {Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawConfirmation bool
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		if event.Confirmation != nil {
			if !event.Transient || event.IndependentID == "" {
				t.Fatalf("independent confirmation was not marked transient: %#v", event)
			}
			sawConfirmation = true
			if !orch.ResolveToolConfirmation(events.ToolConfirmationDecision{
				ConfirmationID: event.Confirmation.ConfirmationID,
				CallID:         event.Confirmation.CallID,
				Action:         events.PermissionAllowOnce,
				Allowed:        true,
			}) {
				t.Fatal("independent confirmation decision was not accepted")
			}
		}
	}
	if !sawConfirmation {
		t.Fatal("dangerous independent tool did not request confirmation")
	}
	if len(scripted.Requests()) != 2 || len(conv.Messages) != 2 || conv.Messages[1].Content.Text() != "permission-aware summary" {
		t.Fatalf("unexpected independent permission flow: calls=%d history=%#v", len(scripted.Requests()), conv.Messages)
	}
}

func TestIndependentPermissionDenialAndCancelRemainRecoverable(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		action     events.PermissionAction
		wantStatus events.ToolDisplayStatus
		wantEvent  bool
	}{
		{name: "deny", action: events.PermissionDeny, wantStatus: events.ToolDisplayDenied, wantEvent: true},
		{name: "cancel", action: events.PermissionCancel, wantStatus: events.ToolDisplayCancelled, wantEvent: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
				{{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("bash", "Bash", `{"command":"printf blocked"}`)}},
				{{Type: provider.StreamEventTextDelta, Delta: testSafeText("handled permission decision")}, {Type: provider.StreamEventDone}},
			})
			eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
				Name: "review", Raw: "/review", Origin: skill.OriginSlash,
			}, activity, RunModeDefault)
			if err != nil {
				t.Fatal(err)
			}
			var sawDecision bool
			for event := range eventStream {
				if event.Type == events.Error {
					t.Fatal(event.Err)
				}
				if event.Confirmation != nil {
					if !orch.ResolveToolConfirmation(events.ToolConfirmationDecision{
						ConfirmationID: event.Confirmation.ConfirmationID,
						CallID:         event.Confirmation.CallID,
						Action:         testCase.action,
					}) {
						t.Fatal("independent confirmation decision was not accepted")
					}
				}
				if event.Type == events.ToolDenied && event.Tool != nil && event.Tool.Status == testCase.wantStatus {
					sawDecision = true
				}
			}
			if sawDecision != testCase.wantEvent || len(conv.Messages) != 2 || conv.Messages[1].Content.Text() != "handled permission decision" {
				t.Fatalf("permission decision did not preserve recoverable semantics: saw=%v history=%#v", sawDecision, conv.Messages)
			}
		})
	}
}
