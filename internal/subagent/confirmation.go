package subagent

import (
	"context"
	"errors"
	"sync"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

type taskConfirmationBroker struct {
	mu sync.Mutex

	taskID   ID
	pending  *pendingTaskConfirmation
	last     confirmationIdentity
	closed   bool
	closeErr error
}

type pendingTaskConfirmation struct {
	request events.ToolConfirmationRequest
	result  chan brokerConfirmationResult
}

type brokerConfirmationResult struct {
	decision events.ToolConfirmationDecision
	err      error
}

type confirmationIdentity struct {
	confirmationID string
	callID         string
}

// NewTaskConfirmationBroker creates the isolated, single-pending confirmation
// boundary for one task. TaskManager routes by task ID before calling Resolve.
func NewTaskConfirmationBroker(taskID ID) (ConfirmationBroker, error) {
	if !validIdentifier(string(taskID)) {
		return nil, newConfirmationError(ErrInvalidTask, "task confirmation identity is invalid", true)
	}
	return &taskConfirmationBroker{taskID: taskID}, nil
}

func (broker *taskConfirmationBroker) Request(ctx context.Context, request events.ToolConfirmationRequest) (events.ToolConfirmationDecision, error) {
	if broker == nil || ctx == nil {
		return events.ToolConfirmationDecision{}, newConfirmationError(ErrInvalidTransition, "confirmation request is invalid", true)
	}
	if err := validateConfirmationRequest(request); err != nil {
		return events.ToolConfirmationDecision{}, err
	}
	if err := context.Cause(ctx); err != nil {
		return events.ToolConfirmationDecision{}, err
	}

	requestCopy := cloneConfirmationRequest(&request)
	pending := &pendingTaskConfirmation{
		request: *requestCopy,
		result:  make(chan brokerConfirmationResult, 1),
	}
	broker.mu.Lock()
	if broker.closed {
		err := cloneConfirmationError(broker.closeErr)
		broker.mu.Unlock()
		return events.ToolConfirmationDecision{}, err
	}
	if broker.pending != nil {
		broker.mu.Unlock()
		return events.ToolConfirmationDecision{}, newConfirmationError(ErrInvalidTransition, "another confirmation is already pending", true)
	}
	broker.pending = pending
	broker.mu.Unlock()

	select {
	case result := <-pending.result:
		return result.decision, result.err
	case <-ctx.Done():
		broker.mu.Lock()
		if broker.pending == pending {
			broker.pending = nil
			broker.last = identityOf(pending.request)
			broker.mu.Unlock()
			return events.ToolConfirmationDecision{}, context.Cause(ctx)
		}
		broker.mu.Unlock()
		result := <-pending.result
		return result.decision, result.err
	}
}

func (broker *taskConfirmationBroker) Resolve(decision events.ToolConfirmationDecision) error {
	if broker == nil || !validIdentifier(decision.ConfirmationID) || !validIdentifier(decision.CallID) {
		return newConfirmationError(ErrConfirmationNotFound, "confirmation was not found", true)
	}

	broker.mu.Lock()
	defer broker.mu.Unlock()
	pending := broker.pending
	if pending == nil {
		if broker.last.matches(decision.ConfirmationID, decision.CallID) {
			return newConfirmationError(ErrConfirmationStale, "confirmation is stale", true)
		}
		return newConfirmationError(ErrConfirmationNotFound, "confirmation was not found", true)
	}
	if pending.request.ConfirmationID != decision.ConfirmationID || pending.request.CallID != decision.CallID {
		return newConfirmationError(ErrConfirmationNotFound, "confirmation was not found", true)
	}
	if err := validateConfirmationDecision(pending.request, decision); err != nil {
		return err
	}

	broker.pending = nil
	broker.last = identityOf(pending.request)
	pending.result <- brokerConfirmationResult{decision: decision}
	return nil
}

func (broker *taskConfirmationBroker) Pending() *events.ToolConfirmationRequest {
	if broker == nil {
		return nil
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.pending == nil {
		return nil
	}
	return cloneConfirmationRequest(&broker.pending.request)
}

func (broker *taskConfirmationBroker) Close(cause error) {
	if broker == nil {
		return
	}
	if cause == nil {
		cause = newConfirmationError(ErrCancelled, "task confirmation was cancelled", true)
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.closed {
		return
	}
	broker.closed = true
	broker.closeErr = cloneConfirmationError(cause)
	if broker.pending == nil {
		return
	}
	pending := broker.pending
	broker.pending = nil
	broker.last = identityOf(pending.request)
	pending.result <- brokerConfirmationResult{err: cloneConfirmationError(broker.closeErr)}
}

func validateConfirmationRequest(request events.ToolConfirmationRequest) error {
	if !validIdentifier(request.ConfirmationID) || !validIdentifier(request.CallID) {
		return newConfirmationError(ErrInvalidTransition, "confirmation request identity is invalid", true)
	}
	if request.AllowPermanent {
		return newConfirmationError(ErrPermanentNotAllowed, "permanent permission is unavailable for subagent tasks", true)
	}
	seen := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if _, duplicate := seen[scope.Scope]; duplicate {
			return newConfirmationError(ErrInvalidTransition, "confirmation scope is invalid", true)
		}
		seen[scope.Scope] = struct{}{}
		switch scope.Scope {
		case "once", "session":
		case "permanent":
			if scope.Available {
				return newConfirmationError(ErrPermanentNotAllowed, "permanent permission is unavailable for subagent tasks", true)
			}
		default:
			return newConfirmationError(ErrInvalidTransition, "confirmation scope is invalid", true)
		}
	}
	return nil
}

func validateConfirmationDecision(request events.ToolConfirmationRequest, decision events.ToolConfirmationDecision) error {
	switch decision.Action {
	case events.PermissionAllowPermanent:
		return newConfirmationError(ErrPermanentNotAllowed, "permanent permission is unavailable for subagent tasks", true)
	case events.PermissionAllowOnce:
		if !decision.Allowed || !confirmationScopeAvailable(request, "once") {
			return newConfirmationError(ErrInvalidTransition, "confirmation scope is unavailable", true)
		}
	case events.PermissionAllowSession:
		if !decision.Allowed || !confirmationScopeAvailable(request, "session") {
			return newConfirmationError(ErrInvalidTransition, "confirmation scope is unavailable", true)
		}
	case events.PermissionDeny, events.PermissionCancel:
		if decision.Allowed {
			return newConfirmationError(ErrInvalidTransition, "confirmation decision is inconsistent", true)
		}
	default:
		return newConfirmationError(ErrInvalidTransition, "confirmation action is invalid", true)
	}
	return nil
}

func confirmationScopeAvailable(request events.ToolConfirmationRequest, wanted string) bool {
	for _, scope := range request.Scopes {
		if scope.Scope == wanted {
			return scope.Available
		}
	}
	return false
}

func identityOf(request events.ToolConfirmationRequest) confirmationIdentity {
	return confirmationIdentity{confirmationID: request.ConfirmationID, callID: request.CallID}
}

func (identity confirmationIdentity) matches(confirmationID, callID string) bool {
	return identity.confirmationID != "" && identity.confirmationID == confirmationID && identity.callID == callID
}

func newConfirmationError(code ErrorCode, message string, recoverable bool) error {
	return SafeError(code, redact.NewRuntimeRedactor().Redact(message), recoverable)
}

func cloneConfirmationError(source error) error {
	if source == nil {
		return nil
	}
	var safe *diagnostics.SafeError
	if errors.As(source, &safe) {
		return cloneSafeError(safe)
	}
	return source
}

var _ ConfirmationBroker = (*taskConfirmationBroker)(nil)
