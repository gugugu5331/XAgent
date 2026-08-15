package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestSyntheticResultsRequireInjectedResultFactory(t *testing.T) {
	orch, factory := newProjectionCandidate(t, hook.Noop())
	call := tool.Call{ID: "missing-1", Name: "Missing", ArgumentsJSON: `{}`}
	loadCall := tool.Call{ID: "skill-1", Name: tool.LoadSkillToolName, ArgumentsJSON: `{}`}
	loadTool, err := tool.NewLoadSkillToolWithResultFactory(factory)
	if err != nil {
		t.Fatal(err)
	}
	results := []tool.Result{
		orch.unknownToolResult(call),
		orch.invalidToolArgumentsResult(call, context.Canceled),
		orch.unavailablePermissionTicketResult(call),
		orch.loadSkillSuccess(loadCall, "safe-skill"),
		orch.loadSkillFailure(loadCall, context.Canceled),
		loadTool.Execute(context.Background(), tool.Input{Name: tool.LoadSkillToolName, CallID: "internal-route"}),
	}
	for index, result := range results {
		if _, err := orch.contextManager.ProjectToolResult(result); err != nil {
			t.Fatalf("factory-backed synthetic result %d did not project: %v", index, err)
		}
	}
	if results[4].ExecutionState() != tool.CancelledAfterStart || results[4].Status != tool.StatusTimeout {
		t.Fatalf("load-skill cancel-after-start result = %#v", results[4])
	}
	legacy := (&Orchestrator{}).unknownToolResult(call)
	if _, err := orch.contextManager.ProjectToolResult(legacy); err == nil {
		t.Fatal("synthetic result without injected factory crossed the projection boundary")
	}
	if factory == nil || orch.resultFactory != factory {
		t.Fatal("candidate did not retain the injected result factory identity")
	}
}

func TestToolResultProjectionFansOutOnce(t *testing.T) {
	hooks := newToolHookRecorder(hook.Continue())
	orch, factory := newProjectionCandidate(t, hooks)
	state := &executionState{}
	if err := orch.configureCandidateState(state, "conversation-1"); err != nil {
		t.Fatal(err)
	}
	result, err := factory.Build(tool.ResultFactoryInput{
		CallID: "call-1", Name: "Read", State: tool.Completed, Status: tool.StatusSuccess,
		Summary: "read complete", Preview: "model-preview-canary", CapturedBytes: int64(len("model-preview-canary")),
	})
	if err != nil {
		t.Fatal(err)
	}
	conv := conversation.NewConversation("conversation-1", time.Now())
	out := make(chan events.Event, 1)
	published, err := orch.publishToolExecutions(context.Background(), conv, state, 1, []ToolExecution{{
		Call:   tool.Call{ID: "call-1", Name: "Read", ArgumentsJSON: `{"path":"safe.txt"}`},
		Result: result, HasResult: true, HookInput: hook.NewToolInput("call-1", "Read", map[string]any{"path": "safe.txt"}),
	}}, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || !published[0].Projected {
		t.Fatalf("publication did not retain its single projection: %#v", published)
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Tool == nil {
		t.Fatalf("projected conversation messages = %#v", conv.Messages)
	}
	if strings.Contains(conv.Messages[1].Content.Text(), "model-preview-canary") || strings.Contains(conv.Messages[1].Tool.Result.Text(), "model-preview-canary") {
		t.Fatal("ModelContent leaked into Conversation persistence fields")
	}
	modelContent, found, err := state.lookupModelContentForMessage(conv.ID, 2, 1, "call-1")
	if err != nil || !found || !strings.Contains(modelContent.Text(), "model-preview-canary") {
		t.Fatalf("temporary model projection = (%q,%t,%v)", modelContent.Text(), found, err)
	}
	event := <-out
	if event.Tool == nil || event.Tool.Stdout.Text() != "model-preview-canary" {
		t.Fatalf("Event did not receive UserView: %#v", event)
	}
	_, after := hooks.counts()
	if after != 1 || len(hooks.after) != 1 || hooks.after[0].Content.Text() != "model-preview-canary" {
		t.Fatalf("Hook did not receive the same UserView: %#v", hooks.after)
	}

	failedState := &executionState{}
	if err := orch.configureCandidateState(failedState, "conversation-failed-projection"); err != nil {
		t.Fatal(err)
	}
	failedConversation := conversation.NewConversation("conversation-failed-projection", time.Now())
	failedEvents := make(chan events.Event, 1)
	_, beforeFailureHooks := hooks.counts()
	if _, err := orch.publishToolExecutions(context.Background(), failedConversation, failedState, 1, []ToolExecution{{
		Call: tool.Call{ID: "invalid-result", Name: "Read", ArgumentsJSON: `{}`}, Result: tool.Result{}, HasResult: true,
	}}, failedEvents); err == nil {
		t.Fatal("invalid projection unexpectedly published")
	}
	_, afterFailureHooks := hooks.counts()
	if len(failedConversation.Messages) != 0 || len(failedEvents) != 0 || afterFailureHooks != beforeFailureHooks {
		t.Fatalf("failed projection produced partial side effects: messages=%d events=%d hooks=%d/%d", len(failedConversation.Messages), len(failedEvents), afterFailureHooks, beforeFailureHooks)
	}
	assertModelContentSlotsEmpty(t, failedState)
}

func TestSafeResultAccessorsHaveSingleProductionCallSite(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(current), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	accessors := []string{".ModelContent()", ".UserView()", ".PersistedContent()", ".OutputMeta()"}
	projectCalls := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, accessor := range accessors {
			if strings.Contains(text, accessor) {
				t.Fatalf("orchestrator production directly calls safe accessor %s in %s", accessor, filepath.Base(path))
			}
		}
		projectCalls += strings.Count(text, ".ProjectToolResult(")
	}
	if projectCalls != 1 {
		t.Fatalf("ProjectToolResult production calls = %d, want 1", projectCalls)
	}
}

func TestLegacyToolResultAdapterCannotProjectOrPersist(t *testing.T) {
	hooks := newToolHookRecorder(hook.Continue())
	orch := NewWithOptions(OrchestratorOptions{Hooks: hooks})
	conv := conversation.NewConversation("legacy", time.Now())
	state := &executionState{}
	out := make(chan events.Event, 1)
	execution := ToolExecution{
		Call:      tool.Call{ID: "legacy-call", Name: "Read", ArgumentsJSON: `{}`},
		Result:    tool.Result{CallID: "legacy-call", Name: "Read", Status: tool.StatusSuccess, Summary: "legacy", Content: "bounded legacy display"},
		HasResult: true,
		HookInput: hook.NewToolInput("legacy-call", "Read", nil),
	}
	if !orch.publishLegacyToolResult(context.Background(), execution, out) {
		t.Fatal("legacy bounded Event/Hook adapter failed")
	}
	if _, err := orch.publishToolExecutions(context.Background(), conv, state, 1, []ToolExecution{execution}, out); err != nil {
		t.Fatal(err)
	}
	if len(conv.Messages) != 0 {
		t.Fatalf("legacy adapter wrote Conversation v2: %#v", conv.Messages)
	}
	assertModelContentSlotsEmpty(t, state)
	if orch.contextManager != nil || orch.resultFactory != nil {
		t.Fatal("legacy adapter acquired candidate projection capabilities")
	}
	_, after := hooks.counts()
	if after != 1 {
		t.Fatalf("legacy Hook display count = %d, want 1", after)
	}
}

func newProjectionCandidate(t *testing.T, hooks hook.Runtime) (*Orchestrator, *tool.ResultFactory) {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	factory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := contextmgr.New(nil, contextmgr.ManagerOptions{
		Context: hookTestContextConfig(false), InlineOutputBytes: 1024, RuntimeRedactor: redactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewWithOptions(OrchestratorOptions{
		ContextManager: manager, ResultFactory: factory, MaxRecordBytes: 4096, MaxSessionBytes: 16384,
		RuntimeRedactor: redactor, Hooks: hooks,
	}), factory
}
