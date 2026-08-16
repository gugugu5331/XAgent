package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"xagent/internal/agentrole"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

// t35ParallelProvider keeps the first two requests concurrently in flight.
// This makes model/request identity crossover observable instead of merely
// checking two sequential snapshots.
type t35ParallelProvider struct {
	mu       sync.Mutex
	requests []provider.ChatRequest
	twoReady chan struct{}
	once     sync.Once
}

type t35CancelOnCloseProvider struct {
	mu     sync.Mutex
	calls  int
	cancel context.CancelCauseFunc
}

func (*t35CancelOnCloseProvider) Name() string { return "t35-cancel-on-close" }

func (fixture *t35CancelOnCloseProvider) StreamChat(context.Context, provider.ChatRequest) (provider.ChatStream, error) {
	fixture.mu.Lock()
	fixture.calls++
	cancel := fixture.cancel
	fixture.mu.Unlock()
	eventStream := make(chan provider.StreamEvent, 2)
	eventStream <- provider.StreamEvent{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-after-cancel", "Echo", `{"value":"must-not-run"}`)}
	eventStream <- provider.StreamEvent{Type: provider.StreamEventDone}
	close(eventStream)
	return newOrchestratorTestChatStreamFromChannel(eventStream, func() {
		if cancel != nil {
			cancel(errors.New("application closed after Provider stream"))
		}
	}), nil
}

func (fixture *t35CancelOnCloseProvider) setCancel(cancel context.CancelCauseFunc) {
	fixture.mu.Lock()
	fixture.cancel = cancel
	fixture.mu.Unlock()
}

func (fixture *t35CancelOnCloseProvider) Calls() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.calls
}

func newT35ParallelProvider() *t35ParallelProvider {
	return &t35ParallelProvider{twoReady: make(chan struct{})}
}

func (*t35ParallelProvider) Name() string { return "t35-parallel" }

func (fixture *t35ParallelProvider) StreamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	fixture.mu.Lock()
	fixture.requests = append(fixture.requests, request)
	index := len(fixture.requests)
	if index == 2 {
		fixture.once.Do(func() { close(fixture.twoReady) })
	}
	fixture.mu.Unlock()
	if index <= 2 {
		select {
		case <-fixture.twoReady:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	return newOrchestratorTestChatStream(
		provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: testSafeText("done")},
		provider.StreamEvent{Type: provider.StreamEventDone},
	), nil
}

func (fixture *t35ParallelProvider) Requests() []provider.ChatRequest {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return append([]provider.ChatRequest(nil), fixture.requests...)
}

func TestT35ParallelRoleModelsAndHookSessionsStayTaskLocal(t *testing.T) {
	factory, roles := newTaskRuntimeFactoryFixture(t, 2)
	catalog, err := agentrole.NewModelCatalog(agentrole.ModelCatalogOptions{
		ProviderID: "fixture", DefaultModel: "model-default",
		Aliases:       agentrole.ModelAliases{Haiku: "model-fast", Sonnet: "model-quality"},
		MaxModelBytes: 128,
		Validate: func(model string) error {
			if !strings.HasPrefix(model, "model-") {
				return errors.New("unsupported")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	factory.options.Models = catalog
	addRole := func(name string, model agentrole.ModelAlias) {
		resolved := roles.roles["reviewer"]
		resolved.Generation++
		resolved.Definition = resolved.Definition.Clone()
		resolved.Definition.Name = name
		resolved.Definition.Model = model
		roles.roles[name] = resolved
	}
	addRole("fast", agentrole.ModelHaiku)
	addRole("quality", agentrole.ModelSonnet)

	providerFixture := newT35ParallelProvider()
	hooks := newSubagentLifecycleRecorder()
	factory.options.Provider = providerFixture
	factory.options.Hooks = hooks

	prepare := func(id subagent.ID, role, taskText string) subagent.PreparedTask {
		t.Helper()
		prepared, prepareErr := factory.Prepare(context.Background(), id, subagent.SubmitInput{
			Task: taskText, Type: subagent.TypeDefined, Role: role,
			Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
			Parent: subagent.ParentRef{ConversationID: "parent"},
		})
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		return prepared
	}
	fast := prepare("task-model-fast", "fast", "FAST-TASK")
	quality := prepare("task-model-quality", "quality", "QUALITY-TASK")
	completions := make(chan subagent.Completion, 2)
	for _, task := range []subagent.PreparedTask{fast, quality} {
		task := task
		go func() {
			result := task.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
			completions <- task.Settle(context.Background(), result)
		}()
	}
	for range 2 {
		completion := <-completions
		if completion.Status != subagent.StatusCompleted {
			t.Fatalf("parallel completion = %#v", completion)
		}
	}

	requests := providerFixture.Requests()
	if len(requests) != 2 {
		t.Fatalf("parallel Provider requests = %d, want 2", len(requests))
	}
	modelsByTask := make(map[string]string, 2)
	for _, request := range requests {
		for _, message := range request.Messages {
			if message.Role == provider.ModelMessageRoleUser {
				modelsByTask[message.Content.Text()] = request.Model
			}
		}
	}
	if modelsByTask["FAST-TASK"] != "model-fast" || modelsByTask["QUALITY-TASK"] != "model-quality" {
		t.Fatalf("parallel model routing crossed tasks: %#v", modelsByTask)
	}

	// A later inherited task must still use the captured parent model; aliases
	// are per-role resolution, never a mutation of the catalog/default path.
	inherited := prepare("task-model-inherit", "reviewer", "INHERIT-TASK")
	if completion := inherited.Run(context.Background(), func(subagent.AgentEvent) error { return nil }); completion.Status != subagent.StatusCompleted {
		t.Fatalf("inherited completion = %#v", completion)
	}
	requests = providerFixture.Requests()
	if len(requests) != 3 || requests[2].Model != "model-parent" || factory.options.Models.Default != "model-default" {
		t.Fatalf("role model leaked into default path: requests=%#v default=%q", requests, factory.options.Models.Default)
	}

	calls := hooks.snapshot()
	for _, id := range []string{"task-model-fast", "task-model-quality", "task-model-inherit"} {
		start, end := 0, 0
		for _, call := range calls {
			if call == "session_start:subagent:"+id+":new" {
				start++
			}
			if call == "session_end:subagent:"+id+":exit" {
				end++
			}
		}
		if start != 1 || end != 1 {
			t.Fatalf("task %s Hook lifecycle pairing = %d/%d in %#v", id, start, end, calls)
		}
	}
}

func TestT35CancellationAtProviderStartBoundaryDoesNotStartProvider(t *testing.T) {
	providerFixture := &subagentScriptProvider{scripts: [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("must not run")},
		{Type: provider.StreamEventDone},
	}}}
	factory, _ := newTaskLoopToolFactory(t, providerFixture, hook.Noop(), 2)
	prepared, err := factory.Prepare(context.Background(), "task-cancel-provider-boundary", subagent.SubmitInput{
		Task: "cancel at handoff", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	task.SetFirstProviderRequestObserver(func(context.Context) {
		task.runtime.Cancel(errors.New("application closed at Provider boundary"))
	})
	completion := task.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
	if completion.Status != subagent.StatusCancelled || completion.StopReason != subagent.StopCancelled {
		t.Fatalf("boundary cancellation completion = %#v", completion)
	}
	if got := len(providerFixture.Requests()); got != 0 {
		t.Fatalf("cancellation started %d Provider requests after the boundary", got)
	}
}

func TestT35CancellationAfterProviderStreamDoesNotStartReturnedTools(t *testing.T) {
	providerFixture := &t35CancelOnCloseProvider{}
	hooks := newSubagentLifecycleRecorder()
	factory, echo := newTaskLoopToolFactory(t, providerFixture, hooks, 2)
	prepared, err := factory.Prepare(context.Background(), "task-cancel-before-tools", subagent.SubmitInput{
		Task: "cancel before returned tools", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	providerFixture.setCancel(task.runtime.Cancel)
	var emitted []subagent.AgentEvent
	completion := task.Run(context.Background(), func(event subagent.AgentEvent) error {
		emitted = append(emitted, event.Clone())
		return nil
	})
	if completion.Status != subagent.StatusCancelled || completion.StopReason != subagent.StopCancelled {
		t.Fatalf("post-stream cancellation completion = %#v", completion)
	}
	if providerFixture.Calls() != 1 || echo.Calls() != 0 {
		t.Fatalf("post-stream cancellation continued work: Provider=%d tools=%d", providerFixture.Calls(), echo.Calls())
	}
	for _, event := range emitted {
		if event.Kind == events.ToolPending || event.Kind == events.ToolRunning {
			t.Fatalf("cancelled returned tool crossed its start boundary: %#v", emitted)
		}
	}
	wantTurnEnd := "turn_end:subagent:task-cancel-before-tools:canceled:request canceled"
	if calls := hooks.snapshot(); !containsT35String(calls, wantTurnEnd) {
		t.Fatalf("post-stream cancellation Hook terminal = %#v, want %q", calls, wantTurnEnd)
	}
}

func TestT35FatalToolResultStopsBatchAndNextProviderRequest(t *testing.T) {
	providerFixture := &subagentScriptProvider{scripts: [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-fatal", "Echo", `{"value":"fatal"}`)},
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-after-fatal", "Echo", `{"value":"must-not-run"}`)},
			{Type: provider.StreamEventDone},
		},
		{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("must not continue")},
			{Type: provider.StreamEventDone},
		},
	}}
	factory, echo := newTaskLoopToolFactory(t, providerFixture, hook.Noop(), 3)
	fatal, err := echo.factory.Build(tool.ResultFactoryInput{
		CallID: "call-fatal", Name: "Echo", State: tool.Completed, Status: tool.StatusError,
		Summary: "fatal tool failure", Preview: "bounded failure",
		Error: &tool.Error{Code: "fatal_fixture", Message: "fatal tool failure", Recoverable: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	echo.execute = func(context.Context, tool.Input) tool.Result { return fatal }
	prepared, err := factory.Prepare(context.Background(), "task-fatal-tool", subagent.SubmitInput{
		Task: "stop after fatal tool", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var emitted []subagent.AgentEvent
	completion := prepared.Run(context.Background(), func(event subagent.AgentEvent) error {
		emitted = append(emitted, event.Clone())
		return nil
	})
	if completion.Status != subagent.StatusFailed || completion.StopReason != subagent.StopToolError ||
		completion.Error == nil || completion.Error.Code != string(subagent.ErrToolFailed) {
		t.Fatalf("fatal tool completion = %#v", completion)
	}
	if echo.Calls() != 1 || len(providerFixture.Requests()) != 1 {
		t.Fatalf("fatal tool continued work: tools=%d Provider=%d", echo.Calls(), len(providerFixture.Requests()))
	}
	sawSafeFailure := false
	for _, event := range emitted {
		if event.Kind == events.ToolError && event.Payload.Tool != nil && event.Payload.Tool.CallID == "call-fatal" {
			sawSafeFailure = true
		}
	}
	if !sawSafeFailure {
		t.Fatalf("fatal tool safe result was not retained: %#v", emitted)
	}
}

var _ provider.Provider = (*t35ParallelProvider)(nil)
var _ provider.Provider = (*t35CancelOnCloseProvider)(nil)

func containsT35String(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
