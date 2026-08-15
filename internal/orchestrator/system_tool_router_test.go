package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

func TestSystemToolRouterForegroundUsesSingleSubmitAndOriginalToolCallID(t *testing.T) {
	factory, validated := systemRouterAgentCall(t, "agent-call-1", `{"task":"inspect the bounded change","type":"defined","role":"reviewer","placement":"foreground"}`)
	parent := subagent.ParentRef{ConversationID: "conversation-1", ExecutionID: "execution-1", RequestGeneration: 7}
	service := &recordingSystemSubagentService{
		submission: subagent.Submission{
			ID: "task-1", Type: subagent.TypeDefined, Role: "reviewer", Origin: subagent.OriginModel,
			Parent: parent, Placement: subagent.Foreground, Status: subagent.StatusQueued, CreatedAt: time.Unix(1, 0),
		},
		foreground: subagent.ForegroundOutcome{Completion: &subagent.Completion{
			ID: "task-1", Status: subagent.StatusCompleted, Summary: redact.NewRuntimeRedactor().Redact("bounded child summary"),
			StopReason: subagent.StopCompleted, EndedAt: time.Unix(2, 0),
		}},
	}
	router, err := NewSystemToolRouter(SystemToolRouterOptions{
		Service: service, ResultFactory: factory, Limits: subagent.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}

	result := router.RouteSystem(context.Background(), SystemToolRouteRequest{
		Call: validated, Parent: parent, Invocation: subagent.InvocationRef{ToolCallID: "agent-call-1"},
		RequestGeneration: 7, Depth: 0,
	})

	if result.CallID != "agent-call-1" || result.Name != tool.AgentToolName || result.Status != tool.StatusSuccess {
		t.Fatalf("foreground result identity/status = %#v", result)
	}
	if result.Summary != "bounded child summary" || !strings.Contains(result.ModelContent().Text(), "bounded child summary") {
		t.Fatalf("foreground result did not use authoritative completion summary: %#v", result)
	}
	submissions, awaited := service.observed()
	if len(submissions) != 1 || len(awaited) != 1 || awaited[0] != "task-1" {
		t.Fatalf("routing calls = submit:%#v await:%#v", submissions, awaited)
	}
	want := subagent.SubmitInput{
		Task: "inspect the bounded change", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginModel, Parent: parent,
		Invocation: subagent.InvocationRef{ToolCallID: "agent-call-1"},
	}
	if submissions[0] != want {
		t.Fatalf("Submit input = %#v, want %#v", submissions[0], want)
	}
}

func TestSystemToolRouterBackgroundAndForkReturnBoundedAcceptanceWithoutAwait(t *testing.T) {
	tests := []struct {
		name       string
		arguments  string
		submission subagent.Submission
	}{
		{
			name:      "defined background",
			arguments: `{"task":"inspect later","type":"defined","placement":"background"}`,
			submission: subagent.Submission{ID: "task-background", Type: subagent.TypeDefined, Origin: subagent.OriginModel,
				Placement: subagent.Background, Status: subagent.StatusQueued, CreatedAt: time.Unix(1, 0)},
		},
		{
			name:      "fork forced background",
			arguments: `{"task":"inspect frozen parent","type":"fork","placement":"foreground"}`,
			submission: subagent.Submission{ID: "task-fork", Type: subagent.TypeFork, Origin: subagent.OriginModel,
				Placement: subagent.Background, Status: subagent.StatusQueued, CreatedAt: time.Unix(1, 0)},
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			callID := "agent-background-call-" + string(rune('1'+index))
			factory, validated := systemRouterAgentCall(t, callID, test.arguments)
			parent := subagent.ParentRef{ConversationID: "conversation-bg", ExecutionID: "execution-bg", RequestGeneration: 9}
			test.submission.Parent = parent
			service := &recordingSystemSubagentService{submission: test.submission}
			router, err := NewSystemToolRouter(SystemToolRouterOptions{Service: service, ResultFactory: factory, Limits: subagent.DefaultLimits()})
			if err != nil {
				t.Fatal(err)
			}

			result := router.RouteSystem(context.Background(), SystemToolRouteRequest{
				Call: validated, Parent: parent, Invocation: subagent.InvocationRef{ToolCallID: callID},
				RequestGeneration: 9,
			})
			if result.CallID != callID || result.Status != tool.StatusSuccess ||
				!strings.Contains(result.Content, `"status":"accepted"`) ||
				!strings.Contains(result.Content, string(test.submission.ID)) {
				t.Fatalf("background acceptance = %#v (%s)", result, result.Content)
			}
			submissions, awaited := service.observed()
			if len(submissions) != 1 || len(awaited) != 0 {
				t.Fatalf("background routing calls = submit:%d await:%d", len(submissions), len(awaited))
			}
		})
	}
}

func TestSystemToolRouterForegroundDetachReturnsBackgroundAcceptance(t *testing.T) {
	factory, validated := systemRouterAgentCall(t, "agent-detached-call", `{"task":"continue after detach","type":"defined","role":"reviewer","placement":"foreground"}`)
	parent := subagent.ParentRef{ConversationID: "conversation-detached", ExecutionID: "execution-detached", RequestGeneration: 10}
	service := &recordingSystemSubagentService{
		submission: subagent.Submission{
			ID: "task-detached", Type: subagent.TypeDefined, Role: "reviewer", Origin: subagent.OriginModel,
			Parent: parent, Placement: subagent.Foreground, Status: subagent.StatusQueued, Revision: 1, CreatedAt: time.Unix(1, 0),
		},
		foreground: subagent.ForegroundOutcome{Detached: true},
	}
	router, err := NewSystemToolRouter(SystemToolRouterOptions{Service: service, ResultFactory: factory, Limits: subagent.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	result := router.RouteSystem(context.Background(), SystemToolRouteRequest{
		Call: validated, Parent: parent, Invocation: subagent.InvocationRef{ToolCallID: "agent-detached-call"}, RequestGeneration: 10,
	})
	if result.Status != tool.StatusSuccess || !strings.Contains(result.Content, `"placement":"background"`) ||
		!strings.Contains(result.Content, `"status":"accepted"`) {
		t.Fatalf("detached acceptance = %#v (%s)", result, result.Content)
	}
	submissions, awaited := service.observed()
	if len(submissions) != 1 || len(awaited) != 1 {
		t.Fatalf("detached route calls = submit:%d await:%d", len(submissions), len(awaited))
	}
}

func TestSystemToolRouterRejectsRecursiveAndForgedIdentityBeforeSubmit(t *testing.T) {
	factory, validated := systemRouterAgentCall(t, "agent-call-secure", `{"task":"do not enqueue","type":"defined","role":"reviewer","placement":"default"}`)
	validParent := subagent.ParentRef{ConversationID: "conversation-secure", ExecutionID: "execution-secure", RequestGeneration: 11}
	tests := []struct {
		name     string
		request  SystemToolRouteRequest
		wantCode string
	}{
		{
			name: "recursive", wantCode: string(subagent.ErrRecursiveDelegate),
			request: SystemToolRouteRequest{Call: validated, Parent: validParent,
				Invocation: subagent.InvocationRef{ToolCallID: "agent-call-secure"}, RequestGeneration: 11, Depth: 1},
		},
		{
			name: "stale generation", wantCode: string(subagent.ErrInvalidParent),
			request: SystemToolRouteRequest{Call: validated, Parent: validParent,
				Invocation: subagent.InvocationRef{ToolCallID: "agent-call-secure"}, RequestGeneration: 12},
		},
		{
			name: "forged tool call", wantCode: string(subagent.ErrInvalidParent),
			request: SystemToolRouteRequest{Call: validated, Parent: validParent,
				Invocation: subagent.InvocationRef{ToolCallID: "different-call"}, RequestGeneration: 11},
		},
		{
			name: "missing execution", wantCode: string(subagent.ErrInvalidParent),
			request: SystemToolRouteRequest{Call: validated,
				Parent:     subagent.ParentRef{ConversationID: "conversation-secure", RequestGeneration: 11},
				Invocation: subagent.InvocationRef{ToolCallID: "agent-call-secure"}, RequestGeneration: 11},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &recordingSystemSubagentService{}
			router, err := NewSystemToolRouter(SystemToolRouterOptions{Service: service, ResultFactory: factory, Limits: subagent.DefaultLimits()})
			if err != nil {
				t.Fatal(err)
			}
			result := router.RouteSystem(context.Background(), test.request)
			if result.Error == nil || result.Error.Code != test.wantCode || result.CallID != "agent-call-secure" {
				t.Fatalf("rejection = %#v, want code %q", result, test.wantCode)
			}
			submissions, awaited := service.observed()
			if len(submissions) != 0 || len(awaited) != 0 {
				t.Fatalf("rejected request crossed Submit boundary: submit:%d await:%d", len(submissions), len(awaited))
			}
		})
	}
}

func TestSystemToolRouterModelLoopCapturesExactParentAndPublishesOneOriginalResult(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	factory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewSafeCandidateRegistry()
	agent, err := tool.NewAgentToolWithResultFactory(factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(agent); err != nil {
		t.Fatal(err)
	}
	manager, err := contextmgr.New(nil, contextmgr.ManagerOptions{
		Context: hookTestContextConfig(false), InlineOutputBytes: 1024, RuntimeRedactor: redactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := newConversationTestStore("")
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	providerScript := &scriptedSkillProvider{events: [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("agent-model-call", tool.AgentToolName, `{"task":"inspect exact parent","type":"defined","role":"reviewer","placement":"foreground"}`)}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("parent resumed")}, {Type: provider.StreamEventDone}},
	}}
	service := &recordingSystemSubagentService{
		submission: subagent.Submission{ID: "task-model-loop", Placement: subagent.Foreground},
		foreground: subagent.ForegroundOutcome{Completion: &subagent.Completion{
			ID: "task-model-loop", Status: subagent.StatusCompleted, Summary: redactor.Redact("child finished once"),
			StopReason: subagent.StopCompleted, EndedAt: time.Unix(2, 0),
		}},
	}
	orch := NewWithOptions(OrchestratorOptions{
		Provider: providerScript, Store: store, Resources: resources.New(), Thinking: config.ThinkingConfig{},
		Registry: registry, ContextManager: manager, ResultFactory: factory,
		MaxRecordBytes: 4096, MaxSessionBytes: 16384, RuntimeRedactor: redactor,
		DefaultModel: "model-parent", Hooks: hook.Noop(), Subagents: service, SubagentLimits: subagent.DefaultLimits(),
	})

	stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "delegate exactly once", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Type == events.Error {
			t.Fatalf("model route failed: %v", event.Err)
		}
	}

	submissions, awaited := service.observed()
	if len(submissions) != 1 || len(awaited) != 1 || awaited[0] != "task-model-loop" {
		t.Fatalf("integrated route calls = submit:%#v await:%#v", submissions, awaited)
	}
	input := submissions[0]
	if input.Invocation.ToolCallID != "agent-model-call" || input.Parent.ConversationID != conv.ID ||
		input.Parent.ExecutionID == "" || input.Parent.RequestGeneration == 0 {
		t.Fatalf("integrated model identity = %#v", input)
	}

	requests := providerScript.Requests()
	if len(requests) != 2 {
		t.Fatalf("Provider requests = %d, want 2", len(requests))
	}
	wantPrompt, err := provider.CapturePromptPrefix(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	runtimes := service.capturedRuntimes()
	if len(runtimes) != 1 || runtimes[0].Prompt.Fingerprint != wantPrompt.Fingerprint || runtimes[0].Registry == nil || !runtimes[0].Registry.IsSealed() {
		t.Fatalf("captured parent runtime = %#v", runtimes)
	}
	capturedConversation := runtimes[0].Conversation.MaterializeEphemeral("captured-parent", time.Unix(3, 0))
	if capturedConversation == nil || len(capturedConversation.Messages) != 1 || capturedConversation.Messages[0].Role != conversation.RoleUser {
		t.Fatalf("captured ordered parent conversation = %#v", capturedConversation)
	}

	var toolCalls, toolResults int
	for _, message := range conv.Messages {
		if message.Tool == nil || message.Tool.CallID != "agent-model-call" {
			continue
		}
		switch message.Role {
		case conversation.RoleToolCall:
			toolCalls++
		case conversation.RoleToolResult:
			toolResults++
			if message.Tool.Name != tool.AgentToolName {
				t.Fatalf("Agent result tool name = %q", message.Tool.Name)
			}
		}
	}
	if toolCalls != 1 || toolResults != 1 {
		t.Fatalf("original ToolCallID publication count = calls:%d results:%d messages:%#v", toolCalls, toolResults, conv.Messages)
	}
}

func TestSystemRouteDescriptorControlsPreparation(t *testing.T) {
	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterWithOptions(descriptorSystemTool{}, tool.RegistrationOptions{Route: tool.RouteSystem}); err != nil {
		t.Fatal(err)
	}
	orch := NewWithOptions(OrchestratorOptions{Registry: registry, Hooks: hook.Noop()})
	out := make(chan events.Event, 4)
	execution := orch.prepareToolExecutionWithRegistryAndRef(
		context.Background(), RunModeDefault, registry,
		indexedToolCall{Call: tool.Call{ID: "descriptor-route", Name: "descriptor_system", ArgumentsJSON: `{}`}},
		hook.ExecutionRef{ExecutionID: "execution-descriptor"}, out,
	)
	if !execution.SystemRoute || execution.HasResult || execution.Validated.Call.Name != "descriptor_system" {
		t.Fatalf("descriptor system route preparation = %#v", execution)
	}
}

func systemRouterAgentCall(t *testing.T, callID, arguments string) (*tool.ResultFactory, tool.ValidatedCall) {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	factory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewSafeCandidateRegistry()
	agent, err := tool.NewAgentToolWithResultFactory(factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(agent); err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	validated, err := registry.ValidateCall(tool.Call{ID: callID, Name: tool.AgentToolName, ArgumentsJSON: arguments})
	if err != nil {
		t.Fatal(err)
	}
	return factory, validated
}

type recordingSystemSubagentService struct {
	subagent.Service

	mu         sync.Mutex
	submission subagent.Submission
	submitErr  error
	foreground subagent.ForegroundOutcome
	awaitErr   error
	submits    []subagent.SubmitInput
	awaits     []subagent.ID
	runtimes   []ParentRuntimeSnapshot
}

func (service *recordingSystemSubagentService) Submit(ctx context.Context, input subagent.SubmitInput) (subagent.Submission, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.submits = append(service.submits, input)
	if runtime, err := ParentRuntimeSnapshotFromSubmitContext(ctx, input.Parent); err == nil {
		service.runtimes = append(service.runtimes, runtime)
	}
	submission := service.submission
	if submission.ID == "" {
		submission.ID = "task-recorded"
	}
	if submission.Type == "" {
		submission.Type = input.Type
	}
	if submission.Role == "" {
		submission.Role = input.Role
	}
	if submission.Origin == "" {
		submission.Origin = input.Origin
	}
	if submission.Parent == (subagent.ParentRef{}) {
		submission.Parent = input.Parent
	}
	if submission.Placement == "" {
		if input.Type == subagent.TypeFork || input.Placement == subagent.PlacementBackground {
			submission.Placement = subagent.Background
		} else {
			submission.Placement = subagent.Foreground
		}
	}
	if submission.Status == "" {
		submission.Status = subagent.StatusQueued
	}
	if submission.Revision == 0 {
		submission.Revision = 1
	}
	if submission.CreatedAt.IsZero() {
		submission.CreatedAt = time.Unix(1, 0)
	}
	return submission, service.submitErr
}

func (service *recordingSystemSubagentService) AwaitForeground(_ context.Context, id subagent.ID) (subagent.ForegroundOutcome, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.awaits = append(service.awaits, id)
	return service.foreground.Clone(), service.awaitErr
}

func (service *recordingSystemSubagentService) observed() ([]subagent.SubmitInput, []subagent.ID) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]subagent.SubmitInput(nil), service.submits...), append([]subagent.ID(nil), service.awaits...)
}

func (service *recordingSystemSubagentService) capturedRuntimes() []ParentRuntimeSnapshot {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]ParentRuntimeSnapshot(nil), service.runtimes...)
}

type descriptorSystemTool struct{}

func (descriptorSystemTool) Name() string        { return "descriptor_system" }
func (descriptorSystemTool) Description() string { return "test descriptor-routed system tool" }
func (descriptorSystemTool) Schema() tool.Schema {
	return tool.Schema{Raw: []byte(`{"type":"object","additionalProperties":false}`)}
}
func (descriptorSystemTool) Risk() tool.Risk { return tool.RiskSafe }
func (descriptorSystemTool) Execute(context.Context, tool.Input) tool.Result {
	panic("descriptor system tool must not execute")
}
