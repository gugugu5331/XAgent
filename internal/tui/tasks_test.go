package tui

import (
	"reflect"
	"strings"
	"testing"

	"xagent/internal/redact"
)

func TestTaskListViewIsCapabilityFreeDeepCopy(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	scopes := []ConfirmationScopeViewSpec{{Scope: "once", Available: true, Description: redactor.Redact("仅本次")}}
	tasks := []TaskViewSpec{{
		ID: "task-1", Revision: 7, Type: "defined", Origin: "tui", Role: "explore",
		RoleSource: "project", RoleSourceID: "roles/explore.md", RoleProviderID: "provider-a",
		RoleOrigin: redactor.Redact("project role"), RoleGeneration: 3,
		Placement: "background", Status: "waiting_confirmation", CreatedAtUnixMilli: 1000,
		HasStartedAt: true, StartedAtUnixMilli: 2000, Iteration: 2, MaxIterations: 5,
		Usage:         TaskUsageViewSpec{InputTokens: 11, OutputTokens: 13, CacheCreationInputTokens: 17, CacheReadInputTokens: 19},
		EventsDropped: 4,
		PendingConfirmation: ConfirmationViewSpec{
			Present: true, ConfirmationID: "confirmation-1", CallID: "call-1", Name: "Bash",
			Prompt: redactor.Redact("run tests"), Scopes: scopes, AllowPermanent: true,
		},
	}}
	viewModel := NewStateViewModel(ViewModelSpec{
		Screen: ScreenTasks,
		Tasks:  TaskListViewSpec{Watermark: 9, Tasks: tasks, Notice: redactor.Redact("fresh")},
	})

	// The producer may immediately reuse every input slice and nested value.
	tasks[0].Role = "mutated"
	tasks[0].PendingConfirmation.Scopes[0] = ConfirmationScopeViewSpec{Scope: "permanent", Available: true}
	tasks = append(tasks, TaskViewSpec{ID: "task-2"})

	if viewModel.Screen() != ScreenTasks || viewModel.Tasks().Watermark() != 9 || viewModel.Tasks().Notice().Text() != "fresh" {
		t.Fatalf("task list metadata changed: screen=%q view=%#v", viewModel.Screen(), viewModel.Tasks())
	}
	got := viewModel.Tasks().Tasks()
	if len(got) != 1 || got[0].ID() != "task-1" || got[0].Role() != "explore" || got[0].Placement() != "background" || got[0].Status() != "waiting_confirmation" {
		t.Fatalf("task projection changed: %#v", got)
	}
	if got[0].RoleSource() != "project" || got[0].RoleSourceID() != "roles/explore.md" || got[0].RoleProviderID() != "provider-a" ||
		got[0].RoleOrigin().Text() != "project role" || got[0].RoleGeneration() != 3 {
		t.Fatalf("role provenance was lost: %#v", got[0])
	}
	usage := got[0].Usage()
	if usage.InputTokens() != 11 || usage.OutputTokens() != 13 || usage.CacheCreationInputTokens() != 17 || usage.CacheReadInputTokens() != 19 {
		t.Fatalf("usage projection changed: %#v", usage)
	}
	confirmation, present := got[0].PendingConfirmation()
	if !present || confirmation.ConfirmationID() != "confirmation-1" || confirmation.CallID() != "call-1" || confirmation.AllowPermanent() {
		t.Fatalf("task confirmation was not projected fail-closed: present=%t view=%#v", present, confirmation)
	}
	if gotScopes := confirmation.Scopes(); len(gotScopes) != 1 || gotScopes[0].Scope() != "once" || !gotScopes[0].Available() {
		t.Fatalf("task confirmation scopes changed: %#v", gotScopes)
	}

	// Every getter must also detach its result from renderer mutations.
	got[0] = TaskView{}
	again := viewModel.Tasks().Tasks()
	if len(again) != 1 || again[0].ID() != "task-1" {
		t.Fatalf("task list exposed mutable storage: %#v", again)
	}
	assertCapabilityFreeTaskValue(t, reflect.TypeOf(viewModel.Tasks()))
}

func TestTaskListScreenShowsStatusPlacementAndRoleProvenance(t *testing.T) {
	view := NewStateViewModel(ViewModelSpec{Tasks: TaskListViewSpec{Watermark: 12, Tasks: []TaskViewSpec{{
		ID: "task-1", Status: "running", Placement: "background", Role: "explore",
		RoleSource: "user", RoleSourceID: "user-role", RoleProviderID: "provider-a", RoleGeneration: 5,
		CreatedAtUnixMilli: 1000, Iteration: 1, MaxIterations: 4,
	}}}}).Tasks()
	screen := NewTaskList(view)
	output := screen.View()
	for _, want := range []string{"任务列表", "watermark=12", "task-1", "running", "background", "explore", "user", "user-role", "provider-a", "1/4"} {
		if !strings.Contains(output, want) {
			t.Fatalf("task list missing %q: %q", want, output)
		}
	}
}

func TestTaskIntentCarriesOnlyUserValues(t *testing.T) {
	intent := NewTaskIntent(TaskIntentResolveConfirmation, "task-1", "confirmation-1", "call-1", "allow_once")
	if intent.Kind() != TaskIntentResolveConfirmation || intent.TaskID() != "task-1" || intent.ConfirmationID() != "confirmation-1" ||
		intent.CallID() != "call-1" || intent.Action() != "allow_once" {
		t.Fatalf("task intent changed: %#v", intent)
	}
	typeOf := reflect.TypeOf(intent)
	for index := 0; index < typeOf.NumField(); index++ {
		kind := typeOf.Field(index).Type.Kind()
		if kind == reflect.Func || kind == reflect.Interface || kind == reflect.Pointer || kind == reflect.Chan {
			t.Fatalf("task intent field %q carries a capability: %v", typeOf.Field(index).Name, typeOf.Field(index).Type)
		}
	}
}

func TestTaskNotificationRendersSafeTerminalSummary(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	notification := NewTaskNotification(TaskNotificationViewSpec{
		NotificationID: "notification-1", TaskID: "task-1", Status: "failed",
		Summary: redactor.Redact("safe summary"), StopReason: "tool_error",
		Error: SafeErrorViewSpec{Present: true, Code: "tool_failed", Message: redactor.Redact("safe failure")},
	})
	output := notification.View()
	for _, want := range []string{"子任务通知", "notification-1", "task-1", "failed", "safe summary", "tool_error", "tool_failed", "safe failure"} {
		if !strings.Contains(output, want) {
			t.Fatalf("task notification missing %q: %q", want, output)
		}
	}
}

func assertCapabilityFreeTaskValue(t *testing.T, value reflect.Type) {
	t.Helper()
	seen := make(map[reflect.Type]bool)
	var walk func(reflect.Type)
	walk = func(current reflect.Type) {
		if seen[current] {
			return
		}
		seen[current] = true
		switch current.Kind() {
		case reflect.Func, reflect.Interface, reflect.Chan, reflect.UnsafePointer:
			t.Fatalf("task view contains capability-bearing type %v", current)
		case reflect.Pointer:
			t.Fatalf("task view contains pointer type %v", current)
		case reflect.Struct:
			for index := 0; index < current.NumField(); index++ {
				walk(current.Field(index).Type)
			}
		case reflect.Slice, reflect.Array:
			walk(current.Elem())
		}
	}
	walk(value)
}
