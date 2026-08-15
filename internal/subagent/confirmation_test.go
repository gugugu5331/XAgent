package subagent

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

func TestConfirmationAllowsOnceOrSessionForMatchingPendingRequest(t *testing.T) {
	for _, test := range []struct {
		name   string
		action events.PermissionAction
	}{
		{name: "once", action: events.PermissionAllowOnce},
		{name: "session", action: events.PermissionAllowSession},
	} {
		t.Run(test.name, func(t *testing.T) {
			broker := mustTaskConfirmationBroker(t, ID("task-"+test.name))
			request := testConfirmationRequest("confirmation-"+test.name, "call-"+test.name)
			result := requestConfirmationAsync(broker, context.Background(), request)

			pending := waitForPendingConfirmation(t, broker)
			pending.CallID = "mutated"
			pending.Scopes[0].Scope = "mutated"
			detached := broker.Pending()
			if detached == nil || detached.CallID != request.CallID || detached.Scopes[0].Scope != "once" {
				t.Fatalf("Pending returned aliased request: %#v", detached)
			}

			decision := events.ToolConfirmationDecision{
				ConfirmationID: request.ConfirmationID,
				CallID:         request.CallID,
				Action:         test.action,
				Allowed:        true,
			}
			if err := broker.Resolve(decision); err != nil {
				t.Fatalf("resolve %s: %v", test.action, err)
			}
			outcome := receiveConfirmationResult(t, result)
			if outcome.err != nil || outcome.decision != decision {
				t.Fatalf("unexpected Request outcome: %#v", outcome)
			}
			if broker.Pending() != nil {
				t.Fatal("resolved request remained pending")
			}
			requireConfirmationErrorCode(t, broker.Resolve(decision), ErrConfirmationStale)
		})
	}
}

func TestConfirmationRejectsWrongIdentityAndUnavailableScopeWithoutResolving(t *testing.T) {
	broker := mustTaskConfirmationBroker(t, "task-scope")
	request := testConfirmationRequest("confirmation-scope", "call-scope")
	request.Scopes[1].Available = false
	result := requestConfirmationAsync(broker, context.Background(), request)
	waitForPendingConfirmation(t, broker)

	invalid := []struct {
		name     string
		decision events.ToolConfirmationDecision
		code     ErrorCode
	}{
		{name: "wrong confirmation", decision: events.ToolConfirmationDecision{ConfirmationID: "other", CallID: request.CallID, Action: events.PermissionDeny}, code: ErrConfirmationNotFound},
		{name: "wrong call", decision: events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: "other", Action: events.PermissionDeny}, code: ErrConfirmationNotFound},
		{name: "unavailable session", decision: events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAllowSession, Allowed: true}, code: ErrInvalidTransition},
		{name: "permanent", decision: events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAllowPermanent, Allowed: true}, code: ErrPermanentNotAllowed},
		{name: "unknown action", decision: events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAction("unknown"), Allowed: true}, code: ErrInvalidTransition},
		{name: "allow flag mismatch", decision: events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAllowOnce}, code: ErrInvalidTransition},
		{name: "deny flag mismatch", decision: events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionDeny, Allowed: true}, code: ErrInvalidTransition},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			requireConfirmationErrorCode(t, broker.Resolve(test.decision), test.code)
			if pending := broker.Pending(); pending == nil || pending.ConfirmationID != request.ConfirmationID || pending.CallID != request.CallID {
				t.Fatalf("invalid decision consumed pending request: %#v", pending)
			}
		})
	}

	denied := events.ToolConfirmationDecision{
		ConfirmationID: request.ConfirmationID,
		CallID:         request.CallID,
		Action:         events.PermissionDeny,
		Allowed:        false,
	}
	if err := broker.Resolve(denied); err != nil {
		t.Fatalf("resolve deny: %v", err)
	}
	if outcome := receiveConfirmationResult(t, result); outcome.err != nil || outcome.decision != denied {
		t.Fatalf("deny outcome: %#v", outcome)
	}
}

func TestConfirmationRejectsPermanentOrMalformedRequestBeforePending(t *testing.T) {
	broker := mustTaskConfirmationBroker(t, "task-invalid")
	permanent := testConfirmationRequest("confirmation-permanent", "call-permanent")
	permanent.AllowPermanent = true
	_, err := broker.Request(context.Background(), permanent)
	requireConfirmationErrorCode(t, err, ErrPermanentNotAllowed)
	if broker.Pending() != nil {
		t.Fatal("permanent request became pending")
	}

	availablePermanent := testConfirmationRequest("confirmation-permanent-scope", "call-permanent-scope")
	availablePermanent.Scopes[2].Available = true
	_, err = broker.Request(context.Background(), availablePermanent)
	requireConfirmationErrorCode(t, err, ErrPermanentNotAllowed)

	malformed := testConfirmationRequest("", "call")
	_, err = broker.Request(context.Background(), malformed)
	requireConfirmationErrorCode(t, err, ErrInvalidTransition)
	malformed = testConfirmationRequest("confirmation", "")
	_, err = broker.Request(context.Background(), malformed)
	requireConfirmationErrorCode(t, err, ErrInvalidTransition)
	malformed = testConfirmationRequest("confirmation", "call")
	malformed.Scopes = append(malformed.Scopes, events.ConfirmationScopeDisplay{Scope: "workspace", Available: true})
	_, err = broker.Request(context.Background(), malformed)
	requireConfirmationErrorCode(t, err, ErrInvalidTransition)
	if broker.Pending() != nil {
		t.Fatal("malformed request became pending")
	}
}

func TestConfirmationAllowsOnlyOnePendingRequestPerTask(t *testing.T) {
	broker := mustTaskConfirmationBroker(t, "task-single")
	first := testConfirmationRequest("confirmation-first", "call-first")
	firstResult := requestConfirmationAsync(broker, context.Background(), first)
	waitForPendingConfirmation(t, broker)

	second := testConfirmationRequest("confirmation-second", "call-second")
	_, err := broker.Request(context.Background(), second)
	requireConfirmationErrorCode(t, err, ErrInvalidTransition)
	if pending := broker.Pending(); pending == nil || pending.ConfirmationID != first.ConfirmationID {
		t.Fatalf("second request replaced first pending request: %#v", pending)
	}

	decision := events.ToolConfirmationDecision{ConfirmationID: first.ConfirmationID, CallID: first.CallID, Action: events.PermissionAllowOnce, Allowed: true}
	if err := broker.Resolve(decision); err != nil {
		t.Fatal(err)
	}
	if outcome := receiveConfirmationResult(t, firstResult); outcome.err != nil {
		t.Fatalf("first request failed: %v", outcome.err)
	}
}

func TestConfirmationBrokersKeepTasksIsolated(t *testing.T) {
	first := mustTaskConfirmationBroker(t, "task-first")
	second := mustTaskConfirmationBroker(t, "task-second")
	request := testConfirmationRequest("same-confirmation", "same-call")
	firstResult := requestConfirmationAsync(first, context.Background(), request)
	secondResult := requestConfirmationAsync(second, context.Background(), request)
	waitForPendingConfirmation(t, first)
	waitForPendingConfirmation(t, second)

	decision := events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAllowOnce, Allowed: true}
	if err := first.Resolve(decision); err != nil {
		t.Fatal(err)
	}
	if outcome := receiveConfirmationResult(t, firstResult); outcome.err != nil {
		t.Fatalf("first task request failed: %v", outcome.err)
	}
	select {
	case outcome := <-secondResult:
		t.Fatalf("first task resolution woke second task: %#v", outcome)
	default:
	}
	if second.Pending() == nil {
		t.Fatal("second task pending request disappeared")
	}
	if err := second.Resolve(decision); err != nil {
		t.Fatal(err)
	}
	if outcome := receiveConfirmationResult(t, secondResult); outcome.err != nil {
		t.Fatalf("second task request failed: %v", outcome.err)
	}
}

func TestConfirmationContextCancellationExpiresOnlyItsPendingRequest(t *testing.T) {
	broker := mustTaskConfirmationBroker(t, "task-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	request := testConfirmationRequest("confirmation-cancel", "call-cancel")
	result := requestConfirmationAsync(broker, ctx, request)
	waitForPendingConfirmation(t, broker)
	cancel()
	outcome := receiveConfirmationResult(t, result)
	if !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("Request cancellation error=%v, want context.Canceled", outcome.err)
	}
	if broker.Pending() != nil {
		t.Fatal("cancelled request remained pending")
	}
	decision := events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionDeny}
	requireConfirmationErrorCode(t, broker.Resolve(decision), ErrConfirmationStale)

	fresh := testConfirmationRequest("confirmation-fresh", "call-fresh")
	freshResult := requestConfirmationAsync(broker, context.Background(), fresh)
	waitForPendingConfirmation(t, broker)
	freshDecision := events.ToolConfirmationDecision{ConfirmationID: fresh.ConfirmationID, CallID: fresh.CallID, Action: events.PermissionDeny}
	if err := broker.Resolve(freshDecision); err != nil {
		t.Fatal(err)
	}
	if outcome := receiveConfirmationResult(t, freshResult); outcome.err != nil {
		t.Fatalf("fresh request after cancellation failed: %v", outcome.err)
	}
}

func TestConfirmationCloseWakesPendingAndRejectsFutureRequests(t *testing.T) {
	broker := mustTaskConfirmationBroker(t, "task-close")
	request := testConfirmationRequest("confirmation-close", "call-close")
	result := requestConfirmationAsync(broker, context.Background(), request)
	waitForPendingConfirmation(t, broker)

	shutdown := SafeError(ErrShutdown, redact.NewRuntimeRedactor().Redact("application closed"), false)
	broker.Close(shutdown)
	broker.Close(errors.New("later cause must not replace first"))
	outcome := receiveConfirmationResult(t, result)
	requireConfirmationErrorCode(t, outcome.err, ErrShutdown)
	if broker.Pending() != nil {
		t.Fatal("closed broker retained pending request")
	}
	requireConfirmationErrorCode(t, broker.Resolve(events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionDeny}), ErrConfirmationStale)
	_, err := broker.Request(context.Background(), testConfirmationRequest("future", "future-call"))
	requireConfirmationErrorCode(t, err, ErrShutdown)
}

func TestConfirmationResolveAndCancellationRaceHasOneOutcome(t *testing.T) {
	for range 100 {
		broker := mustTaskConfirmationBroker(t, "task-race")
		ctx, cancel := context.WithCancel(context.Background())
		request := testConfirmationRequest("confirmation-race", "call-race")
		result := requestConfirmationAsync(broker, ctx, request)
		waitForPendingConfirmation(t, broker)
		decision := events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAllowOnce, Allowed: true}
		start := make(chan struct{})
		resolved := make(chan error, 1)
		go func() {
			<-start
			resolved <- broker.Resolve(decision)
		}()
		go func() {
			<-start
			cancel()
		}()
		close(start)
		outcome := receiveConfirmationResult(t, result)
		resolveErr := <-resolved
		if outcome.err == nil {
			if outcome.decision != decision || resolveErr != nil {
				t.Fatalf("resolved race outcome inconsistent: outcome=%#v resolve=%v", outcome, resolveErr)
			}
		} else {
			if !errors.Is(outcome.err, context.Canceled) {
				t.Fatalf("unexpected cancellation race error: %v", outcome.err)
			}
			requireConfirmationErrorCode(t, resolveErr, ErrConfirmationStale)
		}
		if broker.Pending() != nil {
			t.Fatal("race left a pending confirmation")
		}
	}
}

func TestConfirmationResolveWithoutPendingReturnsNotFound(t *testing.T) {
	broker := mustTaskConfirmationBroker(t, "task-empty")
	requireConfirmationErrorCode(t, broker.Resolve(events.ToolConfirmationDecision{ConfirmationID: "unknown", CallID: "call", Action: events.PermissionDeny}), ErrConfirmationNotFound)
}

type confirmationResult struct {
	decision events.ToolConfirmationDecision
	err      error
}

func mustTaskConfirmationBroker(t *testing.T, taskID ID) ConfirmationBroker {
	t.Helper()
	broker, err := NewTaskConfirmationBroker(taskID)
	if err != nil {
		t.Fatalf("create confirmation broker: %v", err)
	}
	return broker
}

func testConfirmationRequest(confirmationID, callID string) events.ToolConfirmationRequest {
	return events.ToolConfirmationRequest{
		ConfirmationID: confirmationID,
		CallID:         callID,
		Scopes: []events.ConfirmationScopeDisplay{
			{Scope: "once", Available: true},
			{Scope: "session", Available: true},
			{Scope: "permanent", Available: false},
		},
	}
}

func requestConfirmationAsync(broker ConfirmationBroker, ctx context.Context, request events.ToolConfirmationRequest) <-chan confirmationResult {
	result := make(chan confirmationResult, 1)
	go func() {
		decision, err := broker.Request(ctx, request)
		result <- confirmationResult{decision: decision, err: err}
	}()
	return result
}

func waitForPendingConfirmation(t *testing.T, broker ConfirmationBroker) *events.ToolConfirmationRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending := broker.Pending(); pending != nil {
			return pending
		}
		runtime.Gosched()
	}
	t.Fatal("confirmation did not become pending")
	return nil
}

func receiveConfirmationResult(t *testing.T, result <-chan confirmationResult) confirmationResult {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation Request did not return")
		return confirmationResult{}
	}
}

func requireConfirmationErrorCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want code %q", code)
	}
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(code) {
		t.Fatalf("error=%T %v, want SafeError code %q", err, err, code)
	}
}
