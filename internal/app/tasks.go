package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"xagent/internal/command"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

const taskCommandErrorSource = "app.tasks"

var _ command.TaskIntentSink = (*commandController)(nil)

// HandleTaskIntent is the only command-side adapter to subagent.Service. The
// command package supplies user values; App replaces every trusted routing
// field immediately before calling the shared task service.
func (controller *commandController) HandleTaskIntent(intent command.TaskIntent) error {
	if controller == nil || controller.model == nil {
		return newAppTaskError(nil, subagent.ErrInternal, "任务操作不可用", false)
	}
	model := controller.model
	service := model.deps.Tasks
	if service == nil {
		return newAppTaskError(model, subagent.ErrShutdown, "任务服务未启用", true)
	}
	ctx := context.Background()

	switch intent.Kind {
	case command.TaskIntentSubmit:
		if model.conversation == nil || model.conversation.ID == "" {
			return newAppTaskError(model, subagent.ErrInvalidParent, "当前会话不可用，无法提交任务", true)
		}
		input := intent.Submit
		input.Origin = subagent.OriginTUI
		input.Parent = subagent.ParentRef{ConversationID: model.conversation.ID}
		input.Invocation = subagent.InvocationRef{}
		submitCtx := ctx
		if preparer := model.deps.TaskSubmitPreparer; preparer != nil {
			preparedCtx, preparedInput, err := preparer.PrepareTUISubagentSubmit(ctx, model.conversation, model.mode, input)
			if err != nil {
				return projectAppTaskError(model, err)
			}
			if preparedCtx == nil || preparedInput.Task != input.Task || preparedInput.Type != input.Type ||
				preparedInput.Role != input.Role || preparedInput.Placement != input.Placement ||
				preparedInput.Origin != subagent.OriginTUI || preparedInput.Invocation != (subagent.InvocationRef{}) ||
				preparedInput.Parent.ConversationID != model.conversation.ID ||
				preparedInput.Parent.RequestGeneration == 0 {
				return newAppTaskError(model, subagent.ErrParentSnapshotUnavailable, "当前会话快照不可用，无法提交任务", true)
			}
			submitCtx = preparedCtx
			input = preparedInput
		}
		submission, err := service.Submit(submitCtx, input)
		if err != nil {
			return projectAppTaskError(model, err)
		}
		controller.DisplayNotice(fmt.Sprintf("任务已提交: id=%s status=%s placement=%s", submission.ID, submission.Status, submission.Placement))
		if submission.Placement == subagent.Foreground {
			controller.cmd = awaitForegroundTask(model.ensureTaskEventStreamState(), service, submission.ID, input.Parent.ConversationID)
		}
		return nil
	case command.TaskIntentList:
		snapshot, err := service.List(ctx)
		if err != nil {
			return projectAppTaskError(model, err)
		}
		model.taskListView = projectTaskListView(snapshot)
		model.screen = screenTasks
		model.applyTaskListStatus(snapshot)
		controller.DisplayNotice(appTaskListNotice(snapshot))
		return nil
	case command.TaskIntentDetail:
		detail, err := service.Get(ctx, intent.TaskID)
		if err != nil {
			return projectAppTaskError(model, err)
		}
		model.taskDetailView = projectTaskDetailView(detail)
		model.screen = screenTaskDetail
		controller.DisplayNotice(appTaskDetailNotice(detail))
		return nil
	case command.TaskIntentCancel:
		if err := service.Cancel(ctx, intent.TaskID); err != nil {
			return projectAppTaskError(model, err)
		}
		controller.DisplayNotice(fmt.Sprintf("任务已取消: id=%s", intent.TaskID))
		return nil
	case command.TaskIntentMoveToBackground:
		if err := service.MoveToBackground(ctx, intent.TaskID); err != nil {
			return projectAppTaskError(model, err)
		}
		controller.DisplayNotice(fmt.Sprintf("任务已切换到后台: id=%s", intent.TaskID))
		return nil
	case command.TaskIntentResolveConfirmation:
		if err := service.ResolveConfirmation(ctx, intent.TaskID, intent.Decision); err != nil {
			return projectAppTaskError(model, err)
		}
		controller.DisplayNotice(fmt.Sprintf("任务确认已处理: id=%s action=%s", intent.TaskID, intent.Decision.Action))
		return nil
	default:
		return newAppTaskError(model, subagent.ErrInvalidTransition, "任务操作无效", false)
	}
}

func projectTaskListView(snapshot subagent.TaskListSnapshot) tui.TaskListView {
	spec := tui.TaskListViewSpec{Watermark: snapshot.Watermark}
	if snapshot.Tasks != nil {
		spec.Tasks = make([]tui.TaskViewSpec, len(snapshot.Tasks))
		for index := range snapshot.Tasks {
			spec.Tasks[index] = projectTaskViewSpec(snapshot.Tasks[index])
		}
	}
	return tui.NewTaskListView(spec)
}

func projectTaskDetailView(detail subagent.TaskDetailSnapshot) tui.TaskDetailView {
	spec := tui.TaskDetailViewSpec{Watermark: detail.Watermark, Task: projectTaskViewSpec(detail.Task)}
	if detail.RecentEvents != nil {
		spec.RecentEvents = make([]tui.TaskEventViewSpec, len(detail.RecentEvents))
		for index := range detail.RecentEvents {
			spec.RecentEvents[index] = projectTaskEventViewSpec(detail.RecentEvents[index])
		}
	}
	return tui.NewTaskDetailView(spec)
}

func projectTaskViewSpec(task subagent.TaskSnapshot) tui.TaskViewSpec {
	createdAtUnixMilli := int64(0)
	if !task.CreatedAt.IsZero() {
		createdAtUnixMilli = task.CreatedAt.UnixMilli()
	}
	spec := tui.TaskViewSpec{
		ID: string(task.ID), Revision: task.Revision, Type: string(task.Type), Origin: string(task.Origin),
		Role: task.Role, RoleSource: string(task.RoleSource), RoleSourceID: task.RoleSourceID,
		RoleProviderID: task.RoleProviderID, RoleOrigin: task.RoleOrigin, RoleGeneration: task.RoleGeneration,
		Placement: string(task.Placement), Status: string(task.Status), CreatedAtUnixMilli: createdAtUnixMilli,
		Iteration: task.Iteration, MaxIterations: task.MaxIterations, StopReason: string(task.StopReason),
		Summary: task.Summary, SummaryTruncated: task.SummaryTruncated, TruncationReason: task.TruncationReason,
		Error: projectTaskSafeError(task.Error),
		Usage: tui.TaskUsageViewSpec{
			InputTokens: task.Usage.InputTokens, OutputTokens: task.Usage.OutputTokens,
			CacheCreationInputTokens: task.Usage.CacheCreationInputTokens, CacheReadInputTokens: task.Usage.CacheReadInputTokens,
		},
		EventsDropped: task.EventsDropped,
	}
	if task.StartedAt != nil {
		spec.HasStartedAt = true
		spec.StartedAtUnixMilli = task.StartedAt.UnixMilli()
	}
	if task.EndedAt != nil {
		spec.HasEndedAt = true
		spec.EndedAtUnixMilli = task.EndedAt.UnixMilli()
	}
	if task.PendingConfirmation != nil {
		spec.PendingConfirmation = projectTaskConfirmationViewSpec(*task.PendingConfirmation)
	}
	return spec
}

func projectTaskConfirmationViewSpec(request events.ToolConfirmationRequest) tui.ConfirmationViewSpec {
	scopes := make([]tui.ConfirmationScopeViewSpec, len(request.Scopes))
	for index, scope := range request.Scopes {
		available := scope.Available
		if strings.EqualFold(strings.TrimSpace(scope.Scope), "permanent") {
			available = false
		}
		scopes[index] = tui.ConfirmationScopeViewSpec{Scope: scope.Scope, Available: available, Description: scope.Description}
	}
	return tui.ConfirmationViewSpec{
		Present: true, ConfirmationID: request.ConfirmationID, CallID: request.CallID, Name: request.Name,
		Prompt: request.Prompt, Target: request.Target, Risk: request.Risk, PermissionMode: request.PermissionMode,
		ScopePreview: request.ScopePreview, RuleLocation: request.RuleLocation, Scopes: scopes,
		Warning: request.Warning, RevokeHint: request.RevokeHint,
		// Permanent authorization is not representable for child tasks.
		AllowPermanent: false,
	}
}

func projectTaskEventViewSpec(event subagent.Event) tui.TaskEventViewSpec {
	atUnixMilli := int64(0)
	if !event.At.IsZero() {
		atUnixMilli = event.At.UnixMilli()
	}
	spec := tui.TaskEventViewSpec{
		Revision: event.Revision, Sequence: event.Sequence, AtUnixMilli: atUnixMilli, Kind: string(event.Kind),
	}
	if event.Snapshot != nil {
		spec.Status = string(event.Snapshot.Status)
		spec.Placement = string(event.Snapshot.Placement)
		spec.Text = event.Snapshot.Summary
		spec.Error = projectTaskSafeError(event.Snapshot.Error)
	}
	if event.Completion != nil {
		spec.Status = string(event.Completion.Status)
		spec.Text = event.Completion.Summary
		spec.Error = projectTaskSafeError(event.Completion.Error)
	}
	if event.Result != nil {
		spec.Status = string(event.Result.Status)
		spec.Text = event.Result.Summary
		spec.Error = projectTaskSafeError(event.Result.Error)
	}
	if workspace, present := taskEventWorkspace(event); present {
		projection, valid := projectTaskWorkspace(workspace)
		if !valid {
			spec.Text = redact.NewRuntimeRedactor().Redact("任务工作区状态无效")
			spec.Error = projectTaskSafeError(subagent.SafeError(
				subagent.ErrInvalidTransition,
				redact.NewRuntimeRedactor().Redact("任务工作区状态无效"),
				false,
			))
		} else if projection != "" {
			text := projection
			if summary := spec.Text.Text(); summary != "" {
				text = summary + " | " + projection
			}
			spec.Text = redact.NewRuntimeRedactor().Redact(text)
		}
	}
	if event.Agent != nil {
		payload := event.Agent.Payload
		spec.Text = payload.Text
		if payload.Tool != nil {
			spec.ToolName = payload.Tool.Name
			if spec.Text.Text() == "" {
				spec.Text = payload.Tool.Summary
			}
		}
		if payload.Confirmation != nil && spec.Text.Text() == "" {
			spec.Text = payload.Confirmation.Prompt
		}
		if payload.Progress != nil && spec.Text.Text() == "" {
			spec.Text = payload.Progress.Message
		}
		if payload.Diagnostic != nil && spec.Text.Text() == "" {
			spec.Text = payload.Diagnostic.Message
		}
		if payload.Err != nil {
			spec.Error = projectTaskSafeError(payload.Err)
		}
	}
	return spec
}

func projectTaskSafeError(source *diagnostics.SafeError) tui.SafeErrorViewSpec {
	if source == nil {
		return tui.SafeErrorViewSpec{}
	}
	return tui.SafeErrorViewSpec{
		Present: true, Code: source.Code, Source: source.Source, Message: source.Message, Recoverable: source.Recoverable,
	}
}

func appTaskListNotice(snapshot subagent.TaskListSnapshot) string {
	if len(snapshot.Tasks) == 0 {
		return fmt.Sprintf("任务列表为空: watermark=%d", snapshot.Watermark)
	}
	lines := make([]string, 0, len(snapshot.Tasks)+1)
	lines = append(lines, fmt.Sprintf("任务列表: watermark=%d count=%d", snapshot.Watermark, len(snapshot.Tasks)))
	for _, task := range snapshot.Tasks {
		lines = append(lines, fmt.Sprintf("id=%s status=%s type=%s role=%s placement=%s", task.ID, task.Status, task.Type, task.Role, task.Placement))
	}
	return strings.Join(lines, "\n")
}

func appTaskDetailNotice(detail subagent.TaskDetailSnapshot) string {
	task := detail.Task
	message := fmt.Sprintf(
		"任务详情: watermark=%d id=%s status=%s type=%s role=%s placement=%s events=%d dropped=%d",
		detail.Watermark, task.ID, task.Status, task.Type, task.Role, task.Placement, len(detail.RecentEvents), task.EventsDropped,
	)
	if summary := task.Summary.Text(); summary != "" {
		message += " summary=" + summary
	}
	if task.Error != nil {
		message += fmt.Sprintf(" error=%s:%s", task.Error.Code, task.Error.Message.Text())
	}
	for index := len(detail.RecentEvents) - 1; index >= 0; index-- {
		workspace, present := taskEventWorkspace(detail.RecentEvents[index])
		if !present {
			continue
		}
		projection, valid := projectTaskWorkspace(workspace)
		if !valid {
			message += " 任务工作区状态无效"
		} else if projection != "" {
			message += " " + projection
		}
		break
	}
	return message
}

func taskEventWorkspace(event subagent.Event) (subagent.WorkspaceSummary, bool) {
	if event.Result != nil {
		return event.Result.Workspace.Clone(), true
	}
	if event.Completion != nil {
		return event.Completion.Workspace.Clone(), true
	}
	return subagent.WorkspaceSummary{}, false
}

// projectTaskWorkspace is the only App/TUI projection for task workspace
// lifecycle state. It deliberately omits absolute paths, Git object IDs,
// branches, and free-form errors. Shared tasks have no workspace projection.
func projectTaskWorkspace(workspace subagent.WorkspaceSummary) (string, bool) {
	if workspace == (subagent.WorkspaceSummary{}) {
		return "", true
	}
	if workspace.Validate() != nil || workspace.Isolation != "worktree" ||
		workspace.State != workspace.Cleanup || len(workspace.WorkspaceID) != 32 {
		return "", false
	}
	switch workspace.State {
	case "deleted":
		if workspace.RetentionCause != "clean" || workspace.Dirty || workspace.Unpushed {
			return "", false
		}
	case "retained", "partial", "manual_attention":
		if !appWorkspaceRetentionReasonAllowed(workspace.RetentionCause) {
			return "", false
		}
	default:
		return "", false
	}
	return fmt.Sprintf(
		"workspace=%s state=%s cleanup=%s dirty=%t unpushed=%t reason=%s",
		workspace.WorkspaceID[:8], workspace.State, workspace.Cleanup,
		workspace.Dirty, workspace.Unpushed, workspace.RetentionCause,
	), true
}

func appWorkspaceRetentionReasonAllowed(reason string) bool {
	switch reason {
	case "runtime_active",
		"settlement_failed",
		"lease_release_failed",
		"identity_unknown",
		"manifest_unknown",
		"initialization_changed",
		"inspection_unknown",
		"protected_changes",
		"unpushed_commits",
		"delete_unavailable",
		"delete_lock_failed",
		"delete_lease_failed",
		"worktree_in_use",
		"identity_mismatch",
		"directory_identity_mismatch",
		"record_changed",
		"git_management_mismatch",
		"branch_moved",
		"manifest_mismatch",
		"worktree_remove_failed",
		"worktree_remove_interrupted",
		"branch_cas_failed",
		"branch_cas_interrupted",
		"tombstone_failed",
		"tombstone_interrupted",
		"lease_identity_mismatch",
		"partial_directory_unknown",
		"partial_registration_unknown",
		"partial_registration_present",
		"partial_branch_unknown",
		"partial_branch_moved",
		"partial_branch_present":
		return true
	default:
		return false
	}
}

func projectAppTaskError(model *Model, source error) error {
	var safe *diagnostics.SafeError
	if errors.As(source, &safe) {
		cloned := *safe
		return &cloned
	}
	return newAppTaskError(model, subagent.ErrInternal, "任务操作失败，请重试", false)
}

func newAppTaskError(model *Model, code subagent.ErrorCode, message string, recoverable bool) *diagnostics.SafeError {
	runtimeRedactor := (*redact.RuntimeRedactor)(nil)
	if model != nil {
		runtimeRedactor = model.deps.RuntimeRedactor
	}
	if runtimeRedactor == nil {
		runtimeRedactor = redact.NewRuntimeRedactor()
	}
	return &diagnostics.SafeError{
		Code: string(code), Source: taskCommandErrorSource,
		Message: runtimeRedactor.Redact(message), Recoverable: recoverable,
	}
}
