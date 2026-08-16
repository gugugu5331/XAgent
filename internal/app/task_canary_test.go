package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

func TestCanaryWorkspaceEventNoticeAndTUIStayPathFree(t *testing.T) {
	canary := t68AppCanary()
	redactor := redact.NewRuntimeRedactor()
	repositoryPath := filepath.Join(string(filepath.Separator), "private", canary, "repository")
	worktreePath := filepath.Join(repositoryPath, ".xagent", "worktrees", "tasks", canary)
	workspace := subagent.WorkspaceSummary{
		WorkspaceID: appWorkspaceID, Isolation: "worktree", State: "retained",
		BaseOID: appWorkspaceBaseOID, Branch: "xagent/worktree/" + appWorkspaceID,
		Dirty: true, Cleanup: "retained", RetentionCause: "protected_changes",
		Error: &diagnostics.SafeError{
			Code: string(subagent.ErrInternal), Source: "workspace",
			Message: redactor.Redact(strings.Join([]string{
				"repository=" + repositoryPath,
				"worktree=" + worktreePath,
				"remote=" + canary,
				"url=https://user:token@" + canary + ".invalid/repo.git",
				"init-content=" + canary,
				"env=" + canary,
				"error=" + canary,
			}, " ")),
		},
	}
	now := time.Unix(1200, 0)
	event := completedTaskResultEvent(now, "notification-canary", "conversation-canary", "safe completion")
	event.Result.Workspace = workspace

	projected := projectTaskEventViewSpec(event)
	detailSnapshot := subagent.TaskDetailSnapshot{
		Watermark:    event.Revision,
		Task:         subagent.TaskSnapshot{ID: event.TaskID, Revision: event.Revision, Status: subagent.StatusCompleted},
		RecentEvents: []subagent.Event{event},
	}
	detailView := projectTaskDetailView(detailSnapshot)
	model := Model{screen: screenTaskDetail, taskDetailView: detailView, input: tui.NewInput(""), messages: tui.NewMessagesView(false)}
	visible := strings.Join([]string{
		projected.Text.Text(),
		taskNotificationNotice(event.Result),
		appTaskDetailNotice(detailSnapshot),
		model.View(),
	}, "\n")
	if !strings.Contains(visible, "workspace=01234567") || !strings.Contains(visible, "reason=protected_changes") {
		t.Fatal("safe Workspace identity and retention state were not projected")
	}
	assertT68AppNoCanary(t, visible, canary)
	for _, forbidden := range []string{repositoryPath, worktreePath, appWorkspaceID, appWorkspaceBaseOID, workspace.Branch} {
		if strings.Contains(visible, forbidden) {
			t.Fatal("App workspace projection exposed a protected field")
		}
	}

	store := &taskNotificationStoreFake{}
	active := conversation.NewConversation("conversation-canary", now.Add(-time.Minute))
	model = Model{deps: Deps{Store: store}, conversation: active, messages: tui.NewMessagesView(false)}
	if err := model.ApplyTaskEvent(context.Background(), event); err != nil {
		t.Fatal("valid canary result notification was rejected")
	}
	assertT68AppNoCanary(t, model.status.Notice, canary)
	if len(store.saves) != 1 || len(active.Messages) != 1 {
		t.Fatal("valid canary notification did not cross the App persistence boundary")
	}
}

func TestCanaryInvalidWorkspaceUsesOnlyFixedAppDiagnostic(t *testing.T) {
	canary := t68AppCanary()
	now := time.Unix(1201, 0)
	event := completedTaskResultEvent(now, "notification-invalid-canary", "conversation-canary", "safe completion")
	event.Result.Workspace = subagent.WorkspaceSummary{
		WorkspaceID: appWorkspaceID, Isolation: "worktree", State: "retained",
		BaseOID: appWorkspaceBaseOID, Branch: "xagent/worktree/" + appWorkspaceID,
		Cleanup: "retained", RetentionCause: canary,
		Error: &diagnostics.SafeError{
			Code: string(subagent.ErrInternal), Source: "workspace",
			Message: redact.NewRuntimeRedactor().Redact("private workspace error " + canary),
		},
	}
	store := &taskNotificationStoreFake{}
	model := Model{
		deps: Deps{Store: store}, conversation: conversation.NewConversation("conversation-canary", now.Add(-time.Minute)),
		messages: tui.NewMessagesView(false),
	}
	err := model.ApplyTaskEvent(context.Background(), event)
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(subagent.ErrInvalidTransition) || safe.Message.Text() != "任务工作区状态无效" {
		t.Fatal("invalid Workspace did not use the fixed App diagnostic")
	}
	projected := projectTaskEventViewSpec(event)
	viewError, present := tui.NewTaskDetailView(tui.TaskDetailViewSpec{RecentEvents: []tui.TaskEventViewSpec{projected}}).RecentEvents()[0].Error()
	visible := projected.Text.Text() + "\n" + taskNotificationNotice(event.Result)
	assertT68AppNoCanary(t, visible, canary)
	if visible != "任务工作区状态无效\n任务工作区状态无效" || !present ||
		viewError.Code() != string(subagent.ErrInvalidTransition) || viewError.Message().Text() != "任务工作区状态无效" {
		t.Fatal("invalid Workspace projection was not fixed and path-free")
	}
	if len(store.saves) != 0 || len(model.conversation.Messages) != 0 {
		t.Fatal("invalid Workspace crossed the App persistence boundary")
	}
}

func assertT68AppNoCanary(t *testing.T, visible, canary string) {
	t.Helper()
	if strings.Contains(visible, canary) {
		t.Fatal("App output retained the secret canary")
	}
}

func t68AppCanary() string {
	return strings.Join([]string{"t68", "opaque", "canary", "9f31"}, "-")
}
