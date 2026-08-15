package mcpclient

import (
	"context"
	"testing"

	"xagent/internal/permission"
	"xagent/internal/tool"
)

func executeAuthorizedTool(t *testing.T, executor *tool.Executor, call tool.Call) tool.Result {
	t.Helper()
	validated, err := executor.PrepareCall(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := executor.CallIdentity(validated)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	executor.TicketVerifier = authority
	ticket, err := authority.Issue(call.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	return executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
}
