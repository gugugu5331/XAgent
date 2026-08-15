package subagent

import (
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type ErrorCode string

const (
	ErrInvalidTask               ErrorCode = "invalid_task"
	ErrTaskTooLarge              ErrorCode = "task_too_large"
	ErrContextBudgetExceeded     ErrorCode = "context_budget_exceeded"
	ErrInvalidType               ErrorCode = "invalid_type"
	ErrInvalidPlacement          ErrorCode = "invalid_placement"
	ErrInvalidParent             ErrorCode = "invalid_parent"
	ErrParentSnapshotUnavailable ErrorCode = "parent_snapshot_unavailable"
	ErrUnknownRole               ErrorCode = "unknown_role"
	ErrModelAliasUnavailable     ErrorCode = "model_alias_unavailable"
	ErrPermissionEscalation      ErrorCode = "permission_escalation"
	ErrRecursiveDelegate         ErrorCode = "recursive_delegate"
	ErrQueueFull                 ErrorCode = "queue_full"
	ErrTaskNotFound              ErrorCode = "task_not_found"
	ErrTaskExpired               ErrorCode = "task_expired"
	ErrTaskTerminal              ErrorCode = "task_terminal"
	ErrInvalidTransition         ErrorCode = "invalid_transition"
	ErrConfirmationNotFound      ErrorCode = "confirmation_not_found"
	ErrConfirmationStale         ErrorCode = "confirmation_stale"
	ErrPermanentNotAllowed       ErrorCode = "permanent_not_allowed"
	ErrProviderFailed            ErrorCode = "provider_failed"
	ErrToolFailed                ErrorCode = "tool_failed"
	ErrCancelled                 ErrorCode = "cancelled"
	ErrTimedOut                  ErrorCode = "timed_out"
	ErrLimitReached              ErrorCode = "limit_reached"
	ErrShutdown                  ErrorCode = "shutdown"
	ErrInternal                  ErrorCode = "internal"
	ErrEventCursorExpired        ErrorCode = "event_cursor_expired"
	ErrInboxFull                 ErrorCode = "inbox_full"
	ErrAlreadyConsumed           ErrorCode = "already_consumed"
	ErrResultPublishFailed       ErrorCode = "result_publish_failed"
)

func (code ErrorCode) Valid() bool {
	switch code {
	case ErrInvalidTask,
		ErrTaskTooLarge,
		ErrContextBudgetExceeded,
		ErrInvalidType,
		ErrInvalidPlacement,
		ErrInvalidParent,
		ErrParentSnapshotUnavailable,
		ErrUnknownRole,
		ErrModelAliasUnavailable,
		ErrPermissionEscalation,
		ErrRecursiveDelegate,
		ErrQueueFull,
		ErrTaskNotFound,
		ErrTaskExpired,
		ErrTaskTerminal,
		ErrInvalidTransition,
		ErrConfirmationNotFound,
		ErrConfirmationStale,
		ErrPermanentNotAllowed,
		ErrProviderFailed,
		ErrToolFailed,
		ErrCancelled,
		ErrTimedOut,
		ErrLimitReached,
		ErrShutdown,
		ErrInternal,
		ErrEventCursorExpired,
		ErrInboxFull,
		ErrAlreadyConsumed,
		ErrResultPublishFailed:
		return true
	default:
		return false
	}
}

// SafeError constructs the only public subagent error projection. Unknown
// codes fail closed to a non-recoverable internal error.
func SafeError(code ErrorCode, message redact.SafeText, recoverable bool) *diagnostics.SafeError {
	if !code.Valid() {
		code = ErrInternal
		recoverable = false
	}
	return &diagnostics.SafeError{
		Code:        string(code),
		Source:      "subagent",
		Message:     message,
		Recoverable: recoverable,
	}
}

func cloneSafeError(source *diagnostics.SafeError) *diagnostics.SafeError {
	if source == nil {
		return nil
	}
	cloned := *source
	return &cloned
}
