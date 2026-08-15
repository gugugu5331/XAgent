package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

type taskCommandServiceFake struct {
	subagent.Service
	submitInputs  []subagent.SubmitInput
	submitResult  subagent.Submission
	submitErr     error
	listCalls     int
	listResult    subagent.TaskListSnapshot
	listErr       error
	getIDs        []subagent.ID
	getResult     subagent.TaskDetailSnapshot
	getErr        error
	cancelIDs     []subagent.ID
	cancelErr     error
	backgroundIDs []subagent.ID
	backgroundErr error
	resolved      []taskResolvedConfirmation
	resolveErr    error
	awaitIDs      []subagent.ID
	awaitResult   subagent.ForegroundOutcome
	awaitErr      error
	awaitStarted  chan struct{}
	awaitRelease  <-chan struct{}
	shutdownCalls int
	cancelCalls   int
}

type taskResolvedConfirmation struct {
	taskID   subagent.ID
	decision events.ToolConfirmationDecision
}

func (fake *taskCommandServiceFake) Submit(_ context.Context, input subagent.SubmitInput) (subagent.Submission, error) {
	fake.submitInputs = append(fake.submitInputs, input)
	return fake.submitResult, fake.submitErr
}

func (fake *taskCommandServiceFake) List(context.Context) (subagent.TaskListSnapshot, error) {
	fake.listCalls++
	return fake.listResult.Clone(), fake.listErr
}

func (fake *taskCommandServiceFake) Get(_ context.Context, taskID subagent.ID) (subagent.TaskDetailSnapshot, error) {
	fake.getIDs = append(fake.getIDs, taskID)
	return fake.getResult.Clone(), fake.getErr
}

func (fake *taskCommandServiceFake) Cancel(_ context.Context, taskID subagent.ID) error {
	fake.cancelCalls++
	fake.cancelIDs = append(fake.cancelIDs, taskID)
	return fake.cancelErr
}

func (fake *taskCommandServiceFake) MoveToBackground(_ context.Context, taskID subagent.ID) error {
	fake.backgroundIDs = append(fake.backgroundIDs, taskID)
	return fake.backgroundErr
}

func (fake *taskCommandServiceFake) ResolveConfirmation(_ context.Context, taskID subagent.ID, decision events.ToolConfirmationDecision) error {
	fake.resolved = append(fake.resolved, taskResolvedConfirmation{taskID: taskID, decision: decision})
	return fake.resolveErr
}

func (fake *taskCommandServiceFake) AwaitForeground(ctx context.Context, taskID subagent.ID) (subagent.ForegroundOutcome, error) {
	fake.awaitIDs = append(fake.awaitIDs, taskID)
	if fake.awaitStarted != nil {
		select {
		case <-fake.awaitStarted:
		default:
			close(fake.awaitStarted)
		}
	}
	if fake.awaitRelease != nil {
		select {
		case <-fake.awaitRelease:
		case <-ctx.Done():
			return subagent.ForegroundOutcome{}, context.Cause(ctx)
		}
	}
	return fake.awaitResult.Clone(), fake.awaitErr
}

func (fake *taskCommandServiceFake) Shutdown(context.Context) error {
	fake.shutdownCalls++
	return nil
}

func TestAgentIntentAppFillsTrustedRoutingAtServiceBoundary(t *testing.T) {
	fake := &taskCommandServiceFake{submitResult: subagent.Submission{ID: "task-1", Status: subagent.StatusQueued, Placement: subagent.Background}}
	model := Model{
		deps:         Deps{Tasks: fake},
		conversation: &conversation.Conversation{ID: "conversation-1"},
	}
	controller := &commandController{model: &model}
	intent := command.TaskIntent{
		Kind: command.TaskIntentSubmit,
		Submit: subagent.SubmitInput{
			Task: "inspect", Type: subagent.TypeDefined, Role: "explore", Placement: subagent.PlacementBackground,
			Origin:     subagent.OriginModel,
			Parent:     subagent.ParentRef{ConversationID: "forged", ExecutionID: "forged", RequestGeneration: 99},
			Invocation: subagent.InvocationRef{ToolCallID: "forged"},
		},
	}
	if err := controller.HandleTaskIntent(intent); err != nil {
		t.Fatal(err)
	}
	if len(fake.submitInputs) != 1 {
		t.Fatalf("Submit calls=%d, want 1", len(fake.submitInputs))
	}
	got := fake.submitInputs[0]
	want := intent.Submit
	want.Origin = subagent.OriginTUI
	want.Parent = subagent.ParentRef{ConversationID: "conversation-1"}
	want.Invocation = subagent.InvocationRef{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Submit input=%#v, want trusted routing %#v", got, want)
	}
	if !strings.Contains(model.status.Notice, "task-1") || model.status.Error != nil {
		t.Fatalf("successful submission was not safely projected: notice=%q error=%v", model.status.Notice, model.status.Error)
	}
}

func TestAgentIntentForegroundAwaitReturnsTypedTerminalOrDetach(t *testing.T) {
	now := time.Unix(700, 0)
	t.Run("terminal", func(t *testing.T) {
		release := make(chan struct{})
		started := make(chan struct{})
		fake := &taskCommandServiceFake{
			submitResult: subagent.Submission{ID: "task-foreground", Status: subagent.StatusRunning, Placement: subagent.Foreground},
			awaitStarted: started, awaitRelease: release,
			awaitResult: subagent.ForegroundOutcome{Completion: &subagent.Completion{
				ID: "task-foreground", Status: subagent.StatusCompleted, Summary: redact.NewRuntimeRedactor().Redact("foreground summary"),
				StopReason: subagent.StopCompleted, EndedAt: now,
			}},
		}
		model := Model{deps: Deps{Tasks: fake}, conversation: conversation.NewConversation("conversation-1", now.Add(-time.Minute))}
		controller := &commandController{model: &model}
		if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentSubmit, Submit: subagent.SubmitInput{Task: "inspect", Type: subagent.TypeDefined}}); err != nil {
			t.Fatal(err)
		}
		if controller.cmd == nil {
			t.Fatal("foreground submit did not create AwaitForeground command")
		}
		result := make(chan tea.Msg, 1)
		go func() { result <- controller.cmd() }()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("AwaitForeground did not start")
		}
		select {
		case <-result:
			t.Fatal("AwaitForeground completed before the service outcome")
		default:
		}
		close(release)
		message := <-result
		updated, _ := model.Update(message)
		model = updated.(Model)
		if !strings.Contains(model.status.Notice, "foreground summary") || !reflect.DeepEqual(fake.awaitIDs, []subagent.ID{"task-foreground"}) {
			t.Fatalf("terminal foreground outcome was not shown once: notice=%q await=%v", model.status.Notice, fake.awaitIDs)
		}
	})

	t.Run("detach keeps event stream ownership", func(t *testing.T) {
		fake := &taskCommandServiceFake{
			submitResult: subagent.Submission{ID: "task-detached", Status: subagent.StatusRunning, Placement: subagent.Foreground},
			awaitResult:  subagent.ForegroundOutcome{Detached: true},
		}
		model := Model{deps: Deps{Tasks: fake}, conversation: conversation.NewConversation("conversation-1", now.Add(-time.Minute))}
		controller := &commandController{model: &model}
		if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentSubmit, Submit: subagent.SubmitInput{Task: "slow", Type: subagent.TypeDefined}}); err != nil {
			t.Fatal(err)
		}
		message := controller.cmd()
		updated, _ := model.Update(message)
		model = updated.(Model)
		if !strings.Contains(model.status.Notice, "后台") || strings.Contains(model.status.Notice, "完成") {
			t.Fatalf("detach was not rendered as bounded background acceptance: %q", model.status.Notice)
		}
		if model.taskEvents == nil || model.taskEvents.stopped() {
			t.Fatal("detach stopped the permanent task event owner")
		}
	})
}

func TestAwaitForegroundOwnerMismatchAndCloseCancelFailClosed(t *testing.T) {
	now := time.Unix(800, 0)
	t.Run("owner mismatch", func(t *testing.T) {
		fake := &taskCommandServiceFake{
			submitResult: subagent.Submission{ID: "task-owner", Status: subagent.StatusRunning, Placement: subagent.Foreground},
			awaitResult: subagent.ForegroundOutcome{Completion: &subagent.Completion{
				ID: "task-owner", Status: subagent.StatusCompleted, Summary: redact.NewRuntimeRedactor().Redact("must not cross conversations"),
				StopReason: subagent.StopCompleted, EndedAt: now,
			}},
		}
		model := Model{deps: Deps{Tasks: fake}, conversation: conversation.NewConversation("owner", now.Add(-time.Minute))}
		controller := &commandController{model: &model}
		if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentSubmit, Submit: subagent.SubmitInput{Task: "inspect", Type: subagent.TypeDefined}}); err != nil {
			t.Fatal(err)
		}
		message := controller.cmd()
		model.conversation = conversation.NewConversation("other", now)
		updated, _ := model.Update(message)
		model = updated.(Model)
		if strings.Contains(model.status.Notice, "must not cross conversations") {
			t.Fatalf("foreground result crossed conversation owner: %q", model.status.Notice)
		}
	})

	t.Run("close cancels wait only", func(t *testing.T) {
		started := make(chan struct{})
		fake := &taskCommandServiceFake{
			submitResult: subagent.Submission{ID: "task-wait", Status: subagent.StatusRunning, Placement: subagent.Foreground},
			awaitStarted: started, awaitRelease: make(chan struct{}),
		}
		model := Model{deps: Deps{Tasks: fake}, conversation: conversation.NewConversation("owner", now.Add(-time.Minute)), runtimeOptions: RuntimeOptions{CleanupTimeout: time.Second}}
		controller := &commandController{model: &model}
		if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentSubmit, Submit: subagent.SubmitInput{Task: "wait", Type: subagent.TypeDefined}}); err != nil {
			t.Fatal(err)
		}
		result := make(chan tea.Msg, 1)
		go func() { result <- controller.cmd() }()
		<-started
		if err := model.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("Close did not release AwaitForeground")
		}
		if fake.cancelCalls != 0 || fake.shutdownCalls != 1 {
			t.Fatalf("wait cancellation called task Cancel or skipped Shutdown: cancel=%d shutdown=%d", fake.cancelCalls, fake.shutdownCalls)
		}
	})
}

func TestTaskCommandControllerDelegatesEveryServiceOperation(t *testing.T) {
	decision := events.ToolConfirmationDecision{ConfirmationID: "confirmation-1", CallID: "call-1", Action: events.PermissionAllowOnce, Allowed: true}
	fake := &taskCommandServiceFake{
		listResult: subagent.TaskListSnapshot{Watermark: 7, Tasks: []subagent.TaskSnapshot{{ID: "task-1", Status: subagent.StatusRunning}}},
		getResult:  subagent.TaskDetailSnapshot{Watermark: 7, Task: subagent.TaskSnapshot{ID: "task-1", Status: subagent.StatusRunning}},
	}
	model := Model{deps: Deps{Tasks: fake}}
	controller := &commandController{model: &model}
	intents := []command.TaskIntent{
		{Kind: command.TaskIntentList},
		{Kind: command.TaskIntentDetail, TaskID: "task-1"},
		{Kind: command.TaskIntentCancel, TaskID: "task-1"},
		{Kind: command.TaskIntentMoveToBackground, TaskID: "task-1"},
		{Kind: command.TaskIntentResolveConfirmation, TaskID: "task-1", Decision: decision},
	}
	for _, intent := range intents {
		if err := controller.HandleTaskIntent(intent); err != nil {
			t.Fatalf("HandleTaskIntent(%q): %v", intent.Kind, err)
		}
	}
	if fake.listCalls != 1 || !reflect.DeepEqual(fake.getIDs, []subagent.ID{"task-1"}) ||
		!reflect.DeepEqual(fake.cancelIDs, []subagent.ID{"task-1"}) || !reflect.DeepEqual(fake.backgroundIDs, []subagent.ID{"task-1"}) ||
		!reflect.DeepEqual(fake.resolved, []taskResolvedConfirmation{{taskID: "task-1", decision: decision}}) {
		t.Fatalf("task service routing mismatch: %#v", fake)
	}
	if model.status.Error != nil || !strings.Contains(model.status.Notice, "确认") {
		t.Fatalf("last successful operation was not projected: notice=%q error=%v", model.status.Notice, model.status.Error)
	}
}

func TestTaskCommandControllerProjectsDetachedListAndDetailViews(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	started := time.Unix(20, 0)
	ended := time.Unix(30, 0)
	fake := &taskCommandServiceFake{
		listResult: subagent.TaskListSnapshot{Watermark: 12, Tasks: []subagent.TaskSnapshot{{
			ID: "task-1", Revision: 11, Type: subagent.TypeFork, Origin: subagent.OriginModel,
			Role: "review", RoleSource: "project", RoleSourceID: "roles/review.md", RoleProviderID: "provider-a",
			RoleOrigin: redactor.Redact("project role"), RoleGeneration: 4,
			Placement: subagent.Background, Status: subagent.StatusWaitingConfirmation,
			CreatedAt: time.Unix(10, 0), StartedAt: &started, EndedAt: &ended,
			Iteration: 2, MaxIterations: 5, StopReason: subagent.StopToolError,
			Summary: redactor.Redact("summary"), SummaryTruncated: true, TruncationReason: redactor.Redact("max_summary_bytes"),
			Error:         &diagnostics.SafeError{Code: string(subagent.ErrToolFailed), Source: "subagent", Message: redactor.Redact("safe failure")},
			Usage:         subagent.Usage{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheReadInputTokens: 4},
			EventsDropped: 5,
			PendingConfirmation: &events.ToolConfirmationRequest{
				ConfirmationID: "confirmation-1", CallID: "call-1", Name: "Bash", Prompt: redactor.Redact("run"),
				Scopes: []events.ConfirmationScopeDisplay{{Scope: "permanent", Available: true}}, AllowPermanent: true,
			},
		}}},
	}
	fake.getResult = subagent.TaskDetailSnapshot{
		Watermark: 13, Task: fake.listResult.Tasks[0],
		RecentEvents: []subagent.Event{{
			Revision: 13, TaskID: "task-1", Sequence: 8, At: time.Unix(25, 0), Kind: subagent.EventTool,
			Agent: &subagent.AgentEvent{Kind: events.ToolSuccess, Payload: events.Event{
				Type: events.ToolSuccess, Text: redactor.Redact("tool finished"), Tool: &events.ToolDisplay{Name: "Bash"},
			}},
		}},
	}
	model := Model{deps: Deps{Tasks: fake}}
	controller := &commandController{model: &model}
	if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentList}); err != nil {
		t.Fatal(err)
	}
	if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentDetail, TaskID: "task-1"}); err != nil {
		t.Fatal(err)
	}

	// Reuse every service-owned nested value after return.
	fake.listResult.Tasks[0].Role = "mutated"
	fake.getResult.Task.PendingConfirmation.Scopes[0] = events.ConfirmationScopeDisplay{Scope: "mutated"}
	fake.getResult.RecentEvents[0].Agent.Payload.Text = redactor.Redact("mutated")

	listed := model.taskListView.Tasks()
	detail := model.taskDetailView
	if model.taskListView.Watermark() != 12 || len(listed) != 1 || listed[0].Role() != "review" || listed[0].EventsDropped() != 5 {
		t.Fatalf("list projection changed: %#v", listed)
	}
	if detail.Watermark() != 13 || detail.Task().ID() != "task-1" || len(detail.RecentEvents()) != 1 ||
		detail.RecentEvents()[0].Text().Text() != "tool finished" {
		t.Fatalf("detail projection changed: %#v %#v", detail, detail.RecentEvents())
	}
	confirmation, present := detail.Task().PendingConfirmation()
	if !present || confirmation.AllowPermanent() || confirmation.Scopes()[0].Available() {
		t.Fatalf("task confirmation projection was not fail-closed: present=%t %#v", present, confirmation)
	}
}

func TestTaskCommandsOpenLiveTaskScreensAndEscapeReturnsToChat(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	pending := &events.ToolConfirmationRequest{
		ConfirmationID: "confirmation-screen", CallID: "call-screen", Name: "Write", Risk: "write",
		Prompt: redactor.Redact("allow write"), Scopes: []events.ConfirmationScopeDisplay{{Scope: "once", Available: true}},
	}
	fake := &taskCommandServiceFake{
		listResult: subagent.TaskListSnapshot{Watermark: 20, Tasks: []subagent.TaskSnapshot{{
			ID: "task-screen", Type: subagent.TypeDefined, Role: "writer", Placement: subagent.Background,
			Status: subagent.StatusWaitingConfirmation, PendingConfirmation: pending,
		}}},
		getResult: subagent.TaskDetailSnapshot{Watermark: 21, Task: subagent.TaskSnapshot{
			ID: "task-screen", Type: subagent.TypeDefined, Role: "writer", Placement: subagent.Background,
			Status: subagent.StatusWaitingConfirmation, PendingConfirmation: pending,
		}},
	}
	model := Model{
		deps: Deps{Tasks: fake}, screen: screenChat, input: tui.NewInput(""), messages: tui.NewMessagesView(false),
	}
	controller := &commandController{model: &model}
	if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentList}); err != nil {
		t.Fatal(err)
	}
	if model.screen != screenTasks || !strings.Contains(model.View(), "任务列表") || !strings.Contains(model.View(), "task-screen") {
		t.Fatalf("/tasks did not open the task list screen: screen=%s view=%q", model.screen, model.View())
	}
	if err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentDetail, TaskID: "task-screen"}); err != nil {
		t.Fatal(err)
	}
	detail := model.View()
	if model.screen != screenTaskDetail || !strings.Contains(detail, "任务详情") ||
		!strings.Contains(detail, "confirmation-screen") || !strings.Contains(detail, "call-screen") {
		t.Fatalf("/task did not open the live detail screen: screen=%s view=%q", model.screen, detail)
	}

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if model.screen != screenTasks {
		t.Fatalf("Esc from task detail reached %s, want tasks", model.screen)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if model.screen != screenChat {
		t.Fatalf("Esc from task list reached %s, want chat", model.screen)
	}
}

func TestTaskCommandControllerRejectsUnavailableServiceAndMissingSubmitParent(t *testing.T) {
	controller := &commandController{model: &Model{}}
	err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentList})
	requireAppTaskErrorCode(t, err, subagent.ErrShutdown)

	fake := &taskCommandServiceFake{}
	controller.model.deps.Tasks = fake
	err = controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentSubmit, Submit: subagent.SubmitInput{Task: "inspect", Type: subagent.TypeDefined}})
	requireAppTaskErrorCode(t, err, subagent.ErrInvalidParent)
	if len(fake.submitInputs) != 0 {
		t.Fatalf("missing parent reached task service: %#v", fake.submitInputs)
	}
}

func TestTaskCommandControllerPublishesOnlyFixedSafeErrors(t *testing.T) {
	const secret = "private-task-backend-secret"
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(secret)
	fake := &taskCommandServiceFake{listErr: errors.New("backend failed: " + secret)}
	model := Model{deps: Deps{Tasks: fake, RuntimeRedactor: redactor}}
	controller := &commandController{model: &model}
	result := command.MustNew(command.Builtins()...).Dispatch("/tasks", controller)
	requireAppTaskErrorCode(t, result.Err, subagent.ErrInternal)
	visible, ok := model.status.Error.(*diagnostics.SafeError)
	if !ok || visible.Code != string(subagent.ErrInternal) || strings.Contains(visible.Message.Text(), secret) || strings.Contains(visible.Message.Text(), "backend failed") {
		t.Fatalf("task failure was not a fixed SafeError: %#v", model.status.Error)
	}

	safeFailure := subagent.SafeError(subagent.ErrTaskExpired, redactor.Redact("任务已过期"), true)
	fake.listErr = safeFailure
	err := controller.HandleTaskIntent(command.TaskIntent{Kind: command.TaskIntentList})
	requireAppTaskErrorCode(t, err, subagent.ErrTaskExpired)
	var projected *diagnostics.SafeError
	if !errors.As(err, &projected) || projected == safeFailure || projected.Message.Text() != "任务已过期" {
		t.Fatalf("service SafeError was not detached and preserved: %#v", err)
	}
}

func requireAppTaskErrorCode(t *testing.T, err error, code subagent.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want %q", code)
	}
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(code) {
		t.Fatalf("error=%T %v, want SafeError %q", err, err, code)
	}
}
