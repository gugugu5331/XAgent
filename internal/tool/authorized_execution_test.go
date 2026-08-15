package tool

import (
	"context"

	"xagent/internal/permission"
)

// Execute is a package-test compatibility fixture. Production Executor has no
// un-ticketed Execute method; legacy behavioral tests still cross the real
// PrepareCall -> identity -> ticket -> ExecuteValidatedAuthorized boundary.
func (e *Executor) Execute(ctx context.Context, call Call) Result {
	validated, err := e.PrepareCall(ctx, call)
	if err != nil {
		return e.validationFailure(call, err)
	}
	identity, err := e.CallIdentity(validated)
	if err != nil {
		return e.validationFailure(call, err)
	}
	authority, err := permission.NewTicketAuthority()
	if err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Test ticket authority is unavailable.", Recoverable: true})
	}
	e.TicketVerifier = authority
	ticket, err := authority.Issue(call.ID, identity)
	if err != nil {
		return e.permissionDenied(call, permission.Decision{Reason: permission.ReasonConfigError, ModelMessage: "Test execution ticket is unavailable.", Recoverable: true})
	}
	result := e.ExecuteValidatedAuthorized(ctx, validated, ticket)
	return result
}
