package subagent

import (
	"testing"

	"xagent/internal/redact"
)

func TestTypeErrorCodesAreFixedAndSafeErrorUsesSubagentSource(t *testing.T) {
	codes := []ErrorCode{
		ErrInvalidTask,
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
		ErrResultPublishFailed,
	}
	seen := make(map[ErrorCode]struct{}, len(codes))
	for _, code := range codes {
		if code == "" || !code.Valid() {
			t.Errorf("documented error code %q is invalid", code)
		}
		if _, duplicate := seen[code]; duplicate {
			t.Errorf("duplicate error code %q", code)
		}
		seen[code] = struct{}{}
	}
	if ErrorCode("not_fixed").Valid() {
		t.Fatal("unknown error code was accepted")
	}

	message := redact.NewRuntimeRedactor().Redact("safe summary")
	err := SafeError(ErrQueueFull, message, true)
	if err.Code != string(ErrQueueFull) || err.Source != "subagent" || err.Message.Text() != "safe summary" || !err.Recoverable {
		t.Fatalf("unexpected safe error projection: %#v", err)
	}
	fallback := SafeError(ErrorCode("not_fixed"), message, true)
	if fallback.Code != string(ErrInternal) || fallback.Recoverable {
		t.Fatalf("unknown code did not fail closed: %#v", fallback)
	}
}
