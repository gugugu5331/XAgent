package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/command"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/orchestrator"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/testutil"
	"xagent/internal/tool"
)

func TestTUIAgentInvalidArgumentsReturnRecoverableStructuredErrorsWithoutSubmit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		code    subagent.ErrorCode
	}{
		{name: "empty task", command: "/agent defined", code: subagent.ErrInvalidTask},
		{name: "invalid type", command: "/agent sideways inspect", code: subagent.ErrInvalidType},
		{name: "invalid placement", command: "/agent defined --placement=sideways inspect", code: subagent.ErrInvalidPlacement},
		{name: "unknown field", command: "/agent defined --unknown=value inspect", code: subagent.ErrInvalidTask},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			service := &taskCommandServiceFake{}
			model := Model{
				deps:         Deps{Tasks: service},
				conversation: conversation.NewConversation("conversation-t37-invalid", time.Unix(1_725_000_000, 0).UTC()),
			}
			controller := &commandController{model: &model}
			result := command.MustNew(command.Builtins()...).Dispatch(test.command, controller)

			var safe *diagnostics.SafeError
			if !errors.As(result.Err, &safe) || safe.Code != string(test.code) || !safe.Recoverable {
				t.Fatalf("Dispatch error=%T %#v, want recoverable SafeError %q", result.Err, result.Err, test.code)
			}
			var displayed *diagnostics.SafeError
			if !errors.As(model.status.Error, &displayed) || displayed.Code != string(test.code) || !displayed.Recoverable {
				t.Fatalf("displayed error=%T %#v, want recoverable SafeError %q", model.status.Error, model.status.Error, test.code)
			}
			if len(service.submitInputs) != 0 {
				t.Fatalf("invalid TUI input crossed Submit boundary: %#v", service.submitInputs)
			}
		})
	}
}

func TestModelAndTUIAgentEntriesProduceEquivalentDefinedAndForkTasks(t *testing.T) {
	tests := []struct {
		name      string
		taskType  subagent.ExecutionType
		placement subagent.Placement
	}{
		{name: "defined foreground", taskType: subagent.TypeDefined, placement: subagent.Foreground},
		{name: "fork requested foreground is background", taskType: subagent.TypeFork, placement: subagent.Background},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			modelEntry := runT37ModelEntry(t, test.taskType)
			tuiEntry := runT37TUIEntry(t, test.taskType)

			if modelEntry.submitCalls != 1 || tuiEntry.submitCalls != 1 {
				t.Fatalf("Submit calls model=%d TUI=%d, want one each", modelEntry.submitCalls, tuiEntry.submitCalls)
			}
			if modelEntry.input.Origin != subagent.OriginModel || modelEntry.input.Invocation.ToolCallID != t37ModelCallID {
				t.Fatalf("model trusted invocation = %#v", modelEntry.input)
			}
			if tuiEntry.input.Origin != subagent.OriginTUI || tuiEntry.input.Invocation != (subagent.InvocationRef{}) {
				t.Fatalf("TUI trusted invocation = %#v", tuiEntry.input)
			}
			if modelEntry.input.Task != tuiEntry.input.Task || modelEntry.input.Type != tuiEntry.input.Type ||
				modelEntry.input.Role != tuiEntry.input.Role || modelEntry.input.Placement != tuiEntry.input.Placement {
				t.Fatalf("untrusted values diverged: model=%#v TUI=%#v", modelEntry.input, tuiEntry.input)
			}
			if modelEntry.submission.Placement != test.placement || tuiEntry.submission.Placement != test.placement {
				t.Fatalf("placement model=%q TUI=%q, want %q", modelEntry.submission.Placement, tuiEntry.submission.Placement, test.placement)
			}

			if got, want := normalizeT37Submission(modelEntry.submission), normalizeT37Submission(tuiEntry.submission); !reflect.DeepEqual(got, want) {
				t.Fatalf("submission schema diverged:\nmodel=%#v\nTUI=%#v", got, want)
			}
			if got, want := normalizeT37Snapshot(modelEntry.detail.Task), normalizeT37Snapshot(tuiEntry.detail.Task); !reflect.DeepEqual(got, want) {
				t.Fatalf("task record diverged:\nmodel=%#v\nTUI=%#v", got, want)
			}
			if !reflect.DeepEqual(modelEntry.completion, tuiEntry.completion) {
				t.Fatalf("terminal fields diverged:\nmodel=%#v\nTUI=%#v", modelEntry.completion, tuiEntry.completion)
			}
			assertT37EquivalentEvents(t, modelEntry.detail.RecentEvents, tuiEntry.detail.RecentEvents)
		})
	}
}

func TestModelAndTUIAgentEntriesRejectUnknownRoleWithoutTaskRecord(t *testing.T) {
	modelService := newT37EntryService(t, subagent.TypeDefined, "task-model-unknown", true)
	modelResult := routeT37ModelCall(t, modelService, `{"task":"inspect unified entry","type":"defined","role":"missing","placement":"background"}`)
	if modelResult.Error == nil || modelResult.Error.Code != string(subagent.ErrUnknownRole) || !modelResult.Error.Recoverable {
		t.Fatalf("model unknown-role result = %#v", modelResult)
	}
	if calls, _, _ := modelService.observed(); calls != 1 {
		t.Fatalf("model unknown role called Submit %d times, want 1", calls)
	}
	assertT37NoTaskRecord(t, modelService)

	tuiService := newT37EntryService(t, subagent.TypeDefined, "task-tui-unknown", true)
	model, controller := newT37TUIController(t, tuiService)
	dispatched := command.MustNew(command.Builtins()...).Dispatch(
		"/agent defined --role=missing --background inspect unified entry", controller,
	)
	var safe *diagnostics.SafeError
	if !errors.As(dispatched.Err, &safe) || safe.Code != string(subagent.ErrUnknownRole) || !safe.Recoverable {
		t.Fatalf("TUI unknown-role error = %T %#v", dispatched.Err, dispatched.Err)
	}
	if model.status.Error == nil {
		t.Fatal("TUI unknown-role error was not projected")
	}
	if calls, _, _ := tuiService.observed(); calls != 1 {
		t.Fatalf("TUI unknown role called Submit %d times, want 1", calls)
	}
	assertT37NoTaskRecord(t, tuiService)
}

func TestModelAgentFourFieldContractRejectsInvalidCallsBeforeSubmit(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
	}{
		{name: "empty task", arguments: `{"task":"","type":"defined"}`},
		{name: "invalid type", arguments: `{"task":"inspect","type":"sideways"}`},
		{name: "unknown fifth field", arguments: `{"task":"inspect","type":"defined","role":"reviewer","placement":"background","extra":"forged"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newT37EntryService(t, subagent.TypeDefined, "task-model-invalid", false)
			registry, _ := newT37AgentRegistry(t)
			if _, err := registry.ValidateCall(tool.Call{ID: t37ModelCallID, Name: tool.AgentToolName, ArgumentsJSON: test.arguments}); err == nil {
				t.Fatalf("invalid model Agent arguments validated: %s", test.arguments)
			}
			if calls, _, _ := service.observed(); calls != 0 {
				t.Fatalf("invalid model call crossed Submit boundary %d times", calls)
			}
			assertT37NoTaskRecord(t, service)
		})
	}
}

const (
	t37TaskText    = "inspect unified entry"
	t37Role        = "reviewer"
	t37ModelCallID = "agent-call-t37"
)

type t37EntryOutcome struct {
	input       subagent.SubmitInput
	submission  subagent.Submission
	detail      subagent.TaskDetailSnapshot
	completion  subagent.Completion
	submitCalls int
}

type t37EntryService struct {
	subagent.Service

	mu          sync.Mutex
	submitCalls int
	inputs      []subagent.SubmitInput
	submissions []subagent.Submission
}

func (service *t37EntryService) Submit(ctx context.Context, input subagent.SubmitInput) (subagent.Submission, error) {
	service.mu.Lock()
	service.submitCalls++
	service.inputs = append(service.inputs, input)
	service.mu.Unlock()

	submission, err := service.Service.Submit(ctx, input)
	if err == nil {
		service.mu.Lock()
		service.submissions = append(service.submissions, submission)
		service.mu.Unlock()
	}
	return submission, err
}

func (service *t37EntryService) observed() (int, []subagent.SubmitInput, []subagent.Submission) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.submitCalls,
		append([]subagent.SubmitInput(nil), service.inputs...),
		append([]subagent.Submission(nil), service.submissions...)
}

type t37KnownRoleRunner struct {
	delegate subagent.RunnerFactory
}

func (runner t37KnownRoleRunner) Prepare(ctx context.Context, id subagent.ID, input subagent.SubmitInput) (subagent.PreparedTask, error) {
	if input.Role != t37Role {
		return nil, subagent.SafeError(subagent.ErrUnknownRole, redact.NewRuntimeRedactor().Redact("subagent role is unknown"), true)
	}
	return runner.delegate.Prepare(ctx, id, input)
}

func runT37ModelEntry(t *testing.T, taskType subagent.ExecutionType) t37EntryOutcome {
	t.Helper()
	service := newT37EntryService(t, taskType, "task-t37", false)
	arguments := t37AgentArguments(t, taskType, t37Role, subagent.PlacementForeground)
	result := routeT37ModelCall(t, service, arguments)
	if result.Error != nil || result.Status != tool.StatusSuccess || result.CallID != t37ModelCallID {
		t.Fatalf("model route result = %#v", result)
	}
	return collectT37EntryOutcome(t, service)
}

func runT37TUIEntry(t *testing.T, taskType subagent.ExecutionType) t37EntryOutcome {
	t.Helper()
	service := newT37EntryService(t, taskType, "task-t37", false)
	_, controller := newT37TUIController(t, service)
	result := command.MustNew(command.Builtins()...).Dispatch(
		"/agent "+string(taskType)+" --role="+t37Role+" --foreground "+t37TaskText,
		controller,
	)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	return collectT37EntryOutcome(t, service)
}

func routeT37ModelCall(t *testing.T, service subagent.Service, arguments string) tool.Result {
	t.Helper()
	registry, factory := newT37AgentRegistry(t)
	validated, err := registry.ValidateCall(tool.Call{ID: t37ModelCallID, Name: tool.AgentToolName, ArgumentsJSON: arguments})
	if err != nil {
		t.Fatal(err)
	}
	router, err := orchestrator.NewSystemToolRouter(orchestrator.SystemToolRouterOptions{
		Service: service, ResultFactory: factory, Limits: subagent.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := subagent.ParentRef{
		ConversationID: "conversation-t37", ExecutionID: "execution-t37", RequestGeneration: 1,
	}
	return router.RouteSystem(context.Background(), orchestrator.SystemToolRouteRequest{
		Call: validated, Parent: parent, Invocation: subagent.InvocationRef{ToolCallID: t37ModelCallID}, RequestGeneration: 1,
	})
}

func newT37AgentRegistry(t *testing.T) (*tool.Registry, *tool.ResultFactory) {
	t.Helper()
	factory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
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
	return registry, factory
}

func newT37TUIController(t *testing.T, service subagent.Service) (*Model, *commandController) {
	t.Helper()
	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	preparer := orchestrator.NewWithOptions(orchestrator.OrchestratorOptions{
		Registry: registry, DefaultModel: "fixture-model",
	})
	model := &Model{
		deps: Deps{
			Tasks: service, TaskSubmitPreparer: preparer, RuntimeRedactor: redact.NewRuntimeRedactor(),
		},
		conversation: conversation.NewConversation("conversation-t37", time.Unix(1_725_000_000, 0).UTC()),
		mode:         orchestrator.RunModeDefault,
	}
	return model, &commandController{model: model}
}

func newT37EntryService(t *testing.T, taskType subagent.ExecutionType, taskID string, rejectUnknownRole bool) *t37EntryService {
	t.Helper()
	clock := testutil.NewManualSubagentClock(time.Unix(1_725_000_100, 0).UTC())
	ids := testutil.NewScriptedSubagentIDGenerator(
		testutil.SubagentIDResult{ID: subagent.ID(taskID)},
		testutil.SubagentIDResult{ID: subagent.ID("notification-" + taskID)},
	)
	limits := subagent.DefaultLimits()
	limits.MaxConcurrent = 1
	limits.MaxQueued = 2
	limits.MaxRetainedTasks = 4
	limits.MaxTaskTombstones = 4
	limits.MaxGlobalEvents = 64
	limits.MaxEventsPerTask = 32
	limits.MaxSubscriberBuffer = 16
	limits.AutoBackgroundAfter = time.Hour
	inbox, err := subagent.NewResultInbox(subagent.ResultInboxOptions{Limits: limits, IDGenerator: ids.Generate})
	if err != nil {
		t.Fatal(err)
	}
	step := testutil.SubagentRunStep{
		Metadata: subagent.PreparedMetadata{
			Type: taskType, Role: t37Role, RoleSource: agentrole.SourceProject,
			RoleSourceID: "project/reviewer.md", RoleGeneration: 3,
			RoleOrigin: redact.NewRuntimeRedactor().Redact("project reviewer"), MaxIterations: 4,
		},
		BeforeWait: []subagent.AgentEvent{
			{Kind: events.TextDelta, Payload: events.Event{Type: events.TextDelta, Text: redact.NewRuntimeRedactor().Redact("bounded progress")}},
			{Kind: events.AgentProgressed, Payload: events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{Iteration: 2, Max: 4}}},
			{Kind: events.UsageUpdated, Payload: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{InputTokens: 7, OutputTokens: 5}}},
		},
		Completion: subagent.Completion{
			Status: subagent.StatusCompleted, Summary: redact.NewRuntimeRedactor().Redact("equivalent completion"),
			StopReason: subagent.StopCompleted, Usage: subagent.Usage{InputTokens: 7, OutputTokens: 5}, EndedAt: clock.Now(),
		},
	}
	scripted := testutil.NewScriptedSubagentRunner(clock.Now, step)
	var runner subagent.RunnerFactory = scripted
	if rejectUnknownRole {
		runner = t37KnownRoleRunner{delegate: scripted}
	}
	manager, err := subagent.NewManager(subagent.ManagerOptions{
		Runner: runner, Limits: limits, Inbox: inbox, Redactor: redact.NewRuntimeRedactor(),
		Clock: clock.Now, IDGenerator: ids.Generate, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Errorf("subagent manager shutdown: %v", err)
		}
	})
	return &t37EntryService{Service: manager}
}

func collectT37EntryOutcome(t *testing.T, service *t37EntryService) t37EntryOutcome {
	t.Helper()
	calls, inputs, submissions := service.observed()
	if calls != 1 || len(inputs) != 1 || len(submissions) != 1 {
		t.Fatalf("entry observations calls=%d inputs=%#v submissions=%#v", calls, inputs, submissions)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	completion, err := service.Await(ctx, submissions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := service.Get(context.Background(), submissions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return t37EntryOutcome{
		input: inputs[0], submission: submissions[0], detail: detail, completion: completion, submitCalls: calls,
	}
}

func t37AgentArguments(t *testing.T, taskType subagent.ExecutionType, role string, placement subagent.PlacementIntent) string {
	t.Helper()
	payload, err := json.Marshal(tool.AgentToolInput{
		Task: t37TaskText, Type: string(taskType), Role: role, Placement: string(placement),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func assertT37NoTaskRecord(t *testing.T, service *t37EntryService) {
	t.Helper()
	snapshot, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 0 {
		t.Fatalf("rejected input created task records: %#v", snapshot.Tasks)
	}
}

func normalizeT37Submission(value subagent.Submission) subagent.Submission {
	value.Origin = ""
	value.Parent = subagent.ParentRef{}
	return value
}

func normalizeT37Snapshot(value subagent.TaskSnapshot) subagent.TaskSnapshot {
	value.Origin = ""
	value.Parent = subagent.ParentRef{}
	return value
}

func normalizeT37Event(value subagent.Event) subagent.Event {
	value = value.Clone()
	if value.Snapshot != nil {
		normalized := normalizeT37Snapshot(*value.Snapshot)
		value.Snapshot = &normalized
	}
	if value.Result != nil {
		value.Result.Parent = subagent.ParentRef{}
	}
	return value
}

func assertT37EquivalentEvents(t *testing.T, modelEvents, tuiEvents []subagent.Event) {
	t.Helper()
	if len(modelEvents) == 0 || len(modelEvents) != len(tuiEvents) {
		t.Fatalf("event counts model=%d TUI=%d", len(modelEvents), len(tuiEvents))
	}
	for index := range modelEvents {
		if err := modelEvents[index].ValidateOneOf(); err != nil {
			t.Fatalf("model event %d invalid: %v", index, err)
		}
		if err := tuiEvents[index].ValidateOneOf(); err != nil {
			t.Fatalf("TUI event %d invalid: %v", index, err)
		}
		if modelEvents[index].Sequence != uint64(index+1) || tuiEvents[index].Sequence != uint64(index+1) {
			t.Fatalf("event sequence %d model=%d TUI=%d", index, modelEvents[index].Sequence, tuiEvents[index].Sequence)
		}
		if got, want := normalizeT37Event(modelEvents[index]), normalizeT37Event(tuiEvents[index]); !reflect.DeepEqual(got, want) {
			t.Fatalf("event %d diverged:\nmodel=%#v\nTUI=%#v", index, got, want)
		}
	}
}
