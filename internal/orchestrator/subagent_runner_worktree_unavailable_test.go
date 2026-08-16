package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/agentrole"
	"xagent/internal/diagnostics"
	"xagent/internal/subagent"
	"xagent/internal/worktree"
)

func TestWorktreeUnavailableRejectsIsolatedRoleWithoutDowngradingToShared(t *testing.T) {
	base, _ := newTaskRuntimeFactoryFixture(t, 4)
	limit := 4
	roles := &runnerIsolationRoleManager{role: agentrole.ResolvedRole{Generation: 1, Definition: agentrole.Definition{
		Metadata: agentrole.Metadata{
			Name: "reviewer", Model: agentrole.ModelInherit, MaxIterations: &limit,
			PermissionMode: agentrole.PermissionStrict, Isolation: agentrole.IsolationWorktree,
		},
		Instructions: base.options.RuntimeRedactor.Redact("ROLE-BODY"),
	}}}
	options := base.options
	options.Roles = roles
	options.WorktreeManager = nil
	options.WorkspaceBinder = nil
	options.WorktreeAcquireTemplate = worktree.AcquireRequest{}
	factory, err := NewSubagentRunnerFactory(options)
	if err != nil {
		t.Fatal(err)
	}

	prepared, prepareErr := factory.Prepare(context.Background(), "worktree-unavailable", runnerIsolationInput())
	if prepared != nil || prepareErr == nil {
		t.Fatalf("Prepare = task %T err %v, want unavailable", prepared, prepareErr)
	}
	var safe *diagnostics.SafeError
	if !errors.As(prepareErr, &safe) || safe.Code != string(subagent.ErrInternal) || !safe.Recoverable ||
		safe.Message.Text() != "worktree isolation is unavailable" {
		t.Fatalf("unavailable diagnostic = %#v", safe)
	}
	secret := filepath.Join(t.TempDir(), "private-root")
	if strings.Contains(prepareErr.Error(), secret) || strings.Contains(prepareErr.Error(), "PATH") || strings.Contains(prepareErr.Error(), "stderr") {
		t.Fatalf("unavailable diagnostic leaked capability details: %q", prepareErr)
	}
}

func TestWorktreeUnavailableDoesNotAffectSharedRole(t *testing.T) {
	base, _ := newTaskRuntimeFactoryFixture(t, 4)
	options := base.options
	options.WorktreeManager = nil
	options.WorkspaceBinder = nil
	options.WorktreeAcquireTemplate = worktree.AcquireRequest{}
	factory, err := NewSubagentRunnerFactory(options)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := factory.Prepare(context.Background(), "shared-without-worktree", subagent.SubmitInput{
		Task: "shared task", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent-conversation"},
	})
	if err != nil || prepared == nil {
		t.Fatalf("shared Prepare = task %T err %v", prepared, err)
	}
	prepared.(*preparedSubagentTask).runtime.close(errors.New("test cleanup"))
}

func TestWorktreeUnavailableContainsAcquireFailureDetails(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "private-root") + " git stderr from PATH"
	manager := &runnerIsolationManager{acquire: func(worktree.AcquireRequest) (worktree.Lease, error) {
		return worktree.Lease{}, errors.New(secret)
	}}
	binder := &runnerIsolationBinder{manager: manager}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)
	prepared, err := factory.Prepare(context.Background(), "worktree-acquire-unavailable", runnerIsolationInput())
	if prepared != nil || err == nil {
		t.Fatalf("Prepare = task %T err %v, want unavailable", prepared, err)
	}
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Message.Text() != "worktree isolation is unavailable" || !safe.Recoverable {
		t.Fatalf("acquire diagnostic = %#v", safe)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "PATH") || strings.Contains(err.Error(), "stderr") {
		t.Fatalf("acquire diagnostic leaked details: %q", err)
	}
}
