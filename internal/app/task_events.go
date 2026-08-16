package app

import (
	"context"

	"xagent/internal/conversation"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

// ApplyTaskEvent is the single-event adapter consumed by the TUI event drain.
// T32 owns the long-lived Subscribe loop and must dispatch each event here on
// the App update boundary; task events deliberately do not enter the normal
// navigation stale-envelope path.
func (m *Model) ApplyTaskEvent(ctx context.Context, source subagent.Event) error {
	if m == nil || source.Kind != subagent.EventResultPublished {
		return nil
	}
	event := source.Clone()
	if err := event.ValidateOneOf(); err != nil || event.Result == nil || event.TaskID != event.Result.TaskID {
		return m.failTaskNotification(subagent.ErrInvalidTransition, "任务通知无效", false)
	}
	notification := event.Result
	if _, valid := projectTaskWorkspace(notification.Workspace); !valid {
		return m.failTaskNotification(subagent.ErrInvalidTransition, "任务工作区状态无效", false)
	}
	if m.conversation == nil || m.conversation.ID == "" || notification.Parent.ConversationID != m.conversation.ID {
		// A result for another main conversation is neither displayed nor marked
		// seen. Replaying it after that conversation becomes active remains safe.
		return nil
	}
	if hasTaskNotification(m.conversation, notification.NotificationID) {
		return nil
	}
	if m.deps.Store == nil {
		return m.failTaskNotification(subagent.ErrInternal, "任务通知暂时无法保存", true)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Build the append on isolated storage. JSONLStore owns created/loaded
	// conversations by pointer identity, so the final Save must use the exact
	// live pointer. Materialize the detached candidate only for that synchronous
	// boundary and restore the full prior value if persistence fails.
	candidate := cloneNavigationConversation(m.conversation)
	err := conversation.AppendSubagentNotification(candidate, conversation.SubagentNotificationMessage{
		NotificationID:   notification.NotificationID,
		TaskID:           string(notification.TaskID),
		Status:           string(notification.Status),
		Summary:          notification.Summary,
		SummaryTruncated: notification.SummaryTruncated,
		TruncationReason: notification.TruncationReason,
		StopReason:       string(notification.StopReason),
		CreatedAt:        notification.CreatedAt,
	})
	if err != nil {
		return m.failTaskNotification(subagent.ErrInvalidTransition, "任务通知无效", false)
	}
	previous := cloneNavigationConversation(m.conversation)
	*m.conversation = *candidate
	if _, err := m.deps.Store.Save(ctx, m.conversation); err != nil {
		*m.conversation = *previous
		return m.failTaskNotification(subagent.ErrInternal, "任务通知保存失败，请重试", true)
	}

	m.messages.SetMessages(m.conversation.Messages)
	m.status.Notice = m.redactText(taskNotificationNotice(notification))
	m.status.Error = nil
	return nil
}

func (m *Model) failTaskNotification(code subagent.ErrorCode, message string, recoverable bool) error {
	err := newAppTaskError(m, code, message, recoverable)
	if m != nil {
		m.status.Notice = ""
		m.status.Error = err
	}
	return err
}

func hasTaskNotification(value *conversation.Conversation, notificationID string) bool {
	if value == nil || notificationID == "" {
		return false
	}
	for index := range value.Messages {
		message := value.Messages[index]
		if message.Role == conversation.RoleSubagentNotification && message.Subagent != nil && message.Subagent.NotificationID == notificationID {
			return true
		}
	}
	return false
}

func taskNotificationNotice(notification *subagent.ResultNotification) string {
	if notification == nil {
		return "任务通知无效"
	}
	workspace, valid := projectTaskWorkspace(notification.Workspace)
	if !valid {
		return "任务工作区状态无效"
	}
	notice := tui.NewTaskNotification(tui.TaskNotificationViewSpec{
		NotificationID: notification.NotificationID, TaskID: string(notification.TaskID), Status: string(notification.Status),
		Summary: notification.Summary, StopReason: string(notification.StopReason), Error: projectTaskSafeError(notification.Error),
	}).View()
	if workspace != "" {
		notice += " | " + workspace
	}
	return notice
}
