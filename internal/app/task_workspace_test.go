package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

const (
	appWorkspaceID      = "0123456789abcdef0123456789abcdef"
	appWorkspaceBaseOID = "abcdef0123456789abcdef0123456789abcdef01"
)

func TestWorkspaceTerminalProjectionIsPathFreeAndBounded(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	workspace := subagent.WorkspaceSummary{
		WorkspaceID: appWorkspaceID, Isolation: "worktree", State: "retained",
		BaseOID: appWorkspaceBaseOID, Branch: "xagent/worktree/" + appWorkspaceID,
		Dirty: true, Unpushed: true, Cleanup: "retained", RetentionCause: "protected_changes",
		Error: &diagnostics.SafeError{
			Code: string(subagent.ErrInternal), Source: "workspace",
			Message: redactor.Redact("must not display /Users/private/worktree"),
		},
	}
	result := &subagent.ResultNotification{
		NotificationID: "notification-workspace", CompletionRevision: 9, CompletionSequence: 3,
		CreatedAt: time.Unix(900, 0), TaskID: "task-workspace", Parent: subagent.ParentRef{ConversationID: "conversation-1"},
		Status: subagent.StatusCompleted, Summary: redactor.Redact("safe terminal"), StopReason: subagent.StopCompleted,
		Workspace: workspace,
	}
	event := subagent.Event{
		Revision: 10, TaskID: result.TaskID, Sequence: 4, At: result.CreatedAt,
		Kind: subagent.EventResultPublished, Result: result,
	}
	view := projectTaskEventViewSpec(event)
	detail := appTaskDetailNotice(subagent.TaskDetailSnapshot{
		Watermark:    event.Revision,
		Task:         subagent.TaskSnapshot{ID: result.TaskID, Status: result.Status},
		RecentEvents: []subagent.Event{event},
	})
	notice := taskNotificationNotice(result)
	visible := strings.Join([]string{view.Text.Text(), detail, notice}, "\n")

	for _, want := range []string{
		"workspace=01234567", "state=retained", "cleanup=retained",
		"dirty=true", "unpushed=true", "reason=protected_changes",
	} {
		if !strings.Contains(visible, want) {
			t.Fatalf("workspace projection missing %q: %q", want, visible)
		}
	}
	for _, forbidden := range []string{
		appWorkspaceID, appWorkspaceBaseOID, workspace.Branch,
		"/Users/private/worktree", "must not display",
	} {
		if strings.Contains(visible, forbidden) {
			t.Fatalf("workspace projection leaked %q: %q", forbidden, visible)
		}
	}
}

func TestWorkspaceSharedProjectionHidesIsolatedFields(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	result := &subagent.ResultNotification{
		NotificationID: "notification-shared", TaskID: "task-shared", Status: subagent.StatusCompleted,
		Summary: redactor.Redact("shared terminal"), StopReason: subagent.StopCompleted,
	}
	event := subagent.Event{Revision: 1, TaskID: result.TaskID, Sequence: 1, Kind: subagent.EventResultPublished, Result: result}
	visible := projectTaskEventViewSpec(event).Text.Text() + "\n" + taskNotificationNotice(result)
	for _, forbidden := range []string{"workspace=", "state=", "cleanup=", "dirty=", "unpushed=", "reason="} {
		if strings.Contains(visible, forbidden) {
			t.Fatalf("shared task exposed meaningless workspace field %q: %q", forbidden, visible)
		}
	}
}

func TestWorkspaceUnknownRetentionReasonFailsClosedWithFixedDiagnostic(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	workspace := subagent.WorkspaceSummary{
		WorkspaceID: appWorkspaceID, Isolation: "worktree", State: "retained",
		BaseOID: appWorkspaceBaseOID, Branch: "xagent/worktree/" + appWorkspaceID,
		Cleanup: "retained", RetentionCause: "private_operator_reason",
		Error: &diagnostics.SafeError{
			Code: string(subagent.ErrInternal), Source: "workspace",
			Message: redactor.Redact("private failure /Users/private/worktree"),
		},
	}
	now := time.Unix(901, 0)
	event := completedTaskResultEvent(now, "notification-invalid-workspace", "conversation-1", "terminal summary")
	event.Result.Workspace = workspace

	store := &taskNotificationStoreFake{}
	active := conversation.NewConversation("conversation-1", now.Add(-time.Minute))
	model := Model{deps: Deps{Store: store}, conversation: active, messages: tui.NewMessagesView(false)}
	err := model.ApplyTaskEvent(context.Background(), event)
	if err == nil {
		t.Fatal("unknown workspace retention reason was accepted")
	}
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(subagent.ErrInvalidTransition) || safe.Message.Text() != "任务工作区状态无效" {
		t.Fatalf("invalid workspace error was not fixed: %#v", err)
	}
	if len(store.saves) != 0 || len(active.Messages) != 0 {
		t.Fatalf("invalid workspace was persisted: saves=%d messages=%#v", len(store.saves), active.Messages)
	}

	visible := projectTaskEventViewSpec(event).Text.Text() + "\n" + taskNotificationNotice(event.Result)
	if !strings.Contains(visible, "任务工作区状态无效") {
		t.Fatalf("invalid workspace did not project the fixed diagnostic: %q", visible)
	}
	for _, forbidden := range []string{workspace.RetentionCause, appWorkspaceID, appWorkspaceBaseOID, workspace.Branch, "/Users/private/worktree", "private failure"} {
		if strings.Contains(visible, forbidden) {
			t.Fatalf("invalid workspace leaked %q: %q", forbidden, visible)
		}
	}
}

func TestSettlingAndTerminalProjectionNeverRegress(t *testing.T) {
	state := newTaskEventStreamState()
	_, epoch, ok := state.beginSubscription()
	if !ok {
		t.Fatal("task event stream did not start")
	}
	events := make(chan subagent.Event)
	now := time.Unix(902, 0)
	model := Model{
		taskEvents: state,
		taskListView: projectTaskListView(subagent.TaskListSnapshot{Watermark: 4, Tasks: []subagent.TaskSnapshot{{
			ID: "task-monotonic", Revision: 4, Status: subagent.StatusRunning,
		}}}),
		taskDetailView: projectTaskDetailView(subagent.TaskDetailSnapshot{Watermark: 4, Task: subagent.TaskSnapshot{
			ID: "task-monotonic", Revision: 4, Status: subagent.StatusRunning,
		}}),
		taskEventCursor: 4,
	}

	apply := func(event subagent.Event) {
		t.Helper()
		updated, command := model.Update(taskEventMsg{state: state, epoch: epoch, events: events, event: event})
		model = updated.(Model)
		if command == nil {
			t.Fatalf("event %q stopped the live drain", event.Kind)
		}
	}
	snapshotEvent := func(revision, sequence uint64, kind subagent.EventKind, status subagent.Status) subagent.Event {
		return subagent.Event{
			Revision: revision, TaskID: "task-monotonic", Sequence: sequence, At: now,
			Kind: kind, Snapshot: &subagent.TaskSnapshot{ID: "task-monotonic", Revision: revision, Status: status},
		}
	}

	apply(snapshotEvent(5, 2, subagent.EventSettling, subagent.StatusSettling))
	if got := model.taskListView.Tasks()[0].Status(); got != string(subagent.StatusSettling) {
		t.Fatalf("settling was not displayed: %q", got)
	}
	apply(snapshotEvent(6, 3, subagent.EventRunning, subagent.StatusRunning))
	if got := model.taskListView.Tasks()[0].Status(); got != string(subagent.StatusSettling) {
		t.Fatalf("newer running event regressed settling: %q", got)
	}

	result := &subagent.ResultNotification{
		NotificationID: "notification-monotonic", CompletionRevision: 6, CompletionSequence: 3,
		CreatedAt: now, TaskID: "task-monotonic", Parent: subagent.ParentRef{ConversationID: "other"},
		Status: subagent.StatusCompleted, StopReason: subagent.StopCompleted,
	}
	apply(subagent.Event{Revision: 7, TaskID: result.TaskID, Sequence: 4, At: now, Kind: subagent.EventResultPublished, Result: result})
	if got := model.taskListView.Tasks()[0].Status(); got != string(subagent.StatusCompleted) {
		t.Fatalf("terminal result was not displayed: %q", got)
	}
	apply(snapshotEvent(8, 5, subagent.EventRunning, subagent.StatusRunning))
	if got := model.taskListView.Tasks()[0].Status(); got != string(subagent.StatusCompleted) {
		t.Fatalf("newer running event regressed terminal state: %q", got)
	}
	if model.taskEventCursor != 8 {
		t.Fatalf("rejected regressions were not consumed monotonically: cursor=%d", model.taskEventCursor)
	}
}
