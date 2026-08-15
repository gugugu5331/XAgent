package permission

import (
	"errors"
	"testing"

	"xagent/internal/agentrole"
)

func TestRestrictModeOnlyKeepsOrTightensParentMode(t *testing.T) {
	tests := []struct {
		name   string
		parent Mode
		role   agentrole.PermissionMode
		want   Mode
	}{
		{name: "inherit strict", parent: ModeStrict, role: agentrole.PermissionInherit, want: ModeStrict},
		{name: "inherit default", parent: ModeDefault, role: agentrole.PermissionInherit, want: ModeDefault},
		{name: "inherit permissive", parent: ModePermissive, role: agentrole.PermissionInherit, want: ModePermissive},
		{name: "same strict", parent: ModeStrict, role: agentrole.PermissionStrict, want: ModeStrict},
		{name: "same default", parent: ModeDefault, role: agentrole.PermissionDefault, want: ModeDefault},
		{name: "same permissive", parent: ModePermissive, role: agentrole.PermissionPermissive, want: ModePermissive},
		{name: "default to strict", parent: ModeDefault, role: agentrole.PermissionStrict, want: ModeStrict},
		{name: "permissive to strict", parent: ModePermissive, role: agentrole.PermissionStrict, want: ModeStrict},
		{name: "permissive to default", parent: ModePermissive, role: agentrole.PermissionDefault, want: ModeDefault},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := RestrictMode(test.parent, test.role)
			if err != nil {
				t.Fatalf("RestrictMode(%q, %q): %v", test.parent, test.role, err)
			}
			if got != test.want {
				t.Fatalf("RestrictMode(%q, %q)=%q, want %q", test.parent, test.role, got, test.want)
			}
		})
	}
}

func TestRestrictModeRejectsEscalationAndInvalidModes(t *testing.T) {
	tests := []struct {
		name   string
		parent Mode
		role   agentrole.PermissionMode
		want   error
	}{
		{name: "strict to default", parent: ModeStrict, role: agentrole.PermissionDefault, want: ErrPermissionEscalation},
		{name: "strict to permissive", parent: ModeStrict, role: agentrole.PermissionPermissive, want: ErrPermissionEscalation},
		{name: "default to permissive", parent: ModeDefault, role: agentrole.PermissionPermissive, want: ErrPermissionEscalation},
		{name: "invalid parent", parent: Mode("invalid"), role: agentrole.PermissionInherit, want: ErrInvalidPermissionMode},
		{name: "invalid role", parent: ModeDefault, role: agentrole.PermissionMode("invalid"), want: ErrInvalidPermissionMode},
		{name: "missing role", parent: ModeDefault, role: "", want: ErrInvalidPermissionMode},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := RestrictMode(test.parent, test.role)
			if !errors.Is(err, test.want) {
				t.Fatalf("RestrictMode(%q, %q) error=%v, want %v", test.parent, test.role, err, test.want)
			}
			if got != "" {
				t.Fatalf("failed restriction returned mode %q", got)
			}
		})
	}
}

func TestTaskScopeCopiesRuleLayersAndStartsWithEmptySession(t *testing.T) {
	parentSession := NewSession()
	parentSession.Add(Rule{Tool: "Write", Pattern: "parent.txt", MatchType: string(MatchExact), Effect: string(EffectAllow)})
	parent := &Authorizer{
		Session: parentSession,
		User: RuleLayer{
			Source: Source{Kind: SourceUserRule, Description: "user"},
			Rules:  []Rule{{Tool: "Read", Pattern: "user.txt", MatchType: string(MatchExact), Effect: string(EffectAllow)}},
		},
		Project: RuleLayer{
			Source: Source{Kind: SourceProjectRule, Description: "project"},
			Rules:  []Rule{{Tool: "Read", Pattern: "project.txt", MatchType: string(MatchExact), Effect: string(EffectDeny)}},
		},
		Local: RuleLayer{
			Source: Source{Kind: SourceLocalRule, Description: "local"},
			Rules:  []Rule{{Tool: "Read", Pattern: "local.txt", MatchType: string(MatchExact), Effect: string(EffectAllow)}},
		},
		LoadErrors: []LoadError{{Source: Source{Kind: SourceProjectRule}, Err: errors.New("broken")}},
	}

	scope, err := parent.NewTaskScope(TaskScopeOptions{ScopeID: "task-copy", Mode: ModeDefault})
	if err != nil {
		t.Fatalf("create task scope: %v", err)
	}
	if scope.ScopeID != "task-copy" || scope.Authorizer == nil || scope.Issuer == nil || scope.Verifier == nil {
		t.Fatalf("incomplete task scope: %#v", scope)
	}
	if scope.Authorizer.Session == nil || scope.Authorizer.Session == parent.Session || len(scope.Authorizer.Session.Rules()) != 0 {
		t.Fatalf("task inherited or reused parent session: %#v", scope.Authorizer.Session.Rules())
	}

	parent.User.Rules[0].Pattern = "mutated-parent.txt"
	parent.Project.Rules = append(parent.Project.Rules, Rule{Tool: "Read", Pattern: "new-parent.txt", MatchType: string(MatchExact), Effect: string(EffectAllow)})
	parent.LoadErrors[0].Source.Kind = SourceUserRule
	if got := scope.Authorizer.User.Rules[0].Pattern; got != "user.txt" {
		t.Fatalf("task user layer aliased parent: %q", got)
	}
	if got := len(scope.Authorizer.Project.Rules); got != 1 {
		t.Fatalf("task project layer changed with parent: %d", got)
	}
	if got := scope.Authorizer.LoadErrors[0].Source.Kind; got != SourceProjectRule {
		t.Fatalf("task load errors aliased parent: %q", got)
	}

	scope.Authorizer.Local.Rules[0].Pattern = "mutated-task.txt"
	if got := parent.Local.Rules[0].Pattern; got != "local.txt" {
		t.Fatalf("parent local layer changed with task: %q", got)
	}
	scope.Authorizer.Session.Add(Rule{Tool: "Write", Pattern: "task.txt", MatchType: string(MatchExact), Effect: string(EffectAllow)})
	if got := len(parent.Session.Rules()); got != 1 {
		t.Fatalf("task session authorization leaked to parent: %d rules", got)
	}
}

func TestTaskScopeFixesMaximumModeAndDisablesPermanentAuthorization(t *testing.T) {
	parent := &Authorizer{}
	scope, err := parent.NewTaskScope(TaskScopeOptions{ScopeID: "task-strict", Mode: ModeStrict})
	if err != nil {
		t.Fatalf("create task scope: %v", err)
	}
	root := t.TempDir()
	call := Call{ID: "bash-read", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`}

	// A caller cannot loosen a task's fixed strict mode by supplying a more
	// permissive per-call context.
	decision := scope.Authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModePermissive})
	if decision.Kind != DecisionAsk || decision.Prompt == nil || decision.Prompt.Mode != ModeStrict {
		t.Fatalf("task fixed mode was loosened: %#v", decision)
	}
	if decision.Prompt.AllowPermanent || containsConfirmationScope(decision.Prompt.Scopes, GrantPermanent) || decision.Prompt.RulePreview != nil {
		t.Fatalf("task confirmation exposed permanent authorization: %#v", decision.Prompt)
	}

	context := mustCallContext(t, call, Context{ProjectRoot: root, Mode: ModePermissive})
	permanent := scope.Authorizer.ResolveUserDecision(call, context, ActionAllowPermanent)
	if permanent.Kind != DecisionDeny || permanent.Ticket.Issued() {
		t.Fatalf("task accepted permanent authorization: %#v", permanent)
	}
	allowed := scope.Authorizer.ResolveUserDecision(call, context, ActionAllowSession)
	if allowed.Kind != DecisionAllow || !allowed.Ticket.Issued() {
		t.Fatalf("task session authorization was not available: %#v", allowed)
	}
	if got := len(scope.Authorizer.Session.Rules()); got != 1 {
		t.Fatalf("task session authorization was not retained in task: %d", got)
	}
}

func TestTaskScopeRejectsUnsafeOptions(t *testing.T) {
	parent := &Authorizer{}
	tests := []struct {
		name    string
		parent  *Authorizer
		options TaskScopeOptions
	}{
		{name: "nil parent", parent: nil, options: TaskScopeOptions{ScopeID: "task", Mode: ModeDefault}},
		{name: "empty scope", parent: parent, options: TaskScopeOptions{Mode: ModeDefault}},
		{name: "whitespace scope", parent: parent, options: TaskScopeOptions{ScopeID: "   ", Mode: ModeDefault}},
		{name: "invalid mode", parent: parent, options: TaskScopeOptions{ScopeID: "task", Mode: Mode("invalid")}},
		{name: "permanent authorization", parent: parent, options: TaskScopeOptions{ScopeID: "task", Mode: ModeDefault, AllowPermanent: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.parent.NewTaskScope(test.options); err == nil {
				t.Fatal("unsafe task scope options were accepted")
			}
		})
	}
}

func TestTaskScopeTicketIsBoundToScopeAndAuthority(t *testing.T) {
	parent := &Authorizer{}
	first, err := parent.NewTaskScope(TaskScopeOptions{ScopeID: "task-first", Mode: ModeDefault})
	if err != nil {
		t.Fatalf("create first task scope: %v", err)
	}
	second, err := parent.NewTaskScope(TaskScopeOptions{ScopeID: "task-second", Mode: ModeDefault})
	if err != nil {
		t.Fatalf("create second task scope: %v", err)
	}
	firstVerifier, ok := first.Verifier.(ScopedTicketVerifier)
	if !ok || firstVerifier.ScopeID() != first.ScopeID {
		t.Fatalf("first verifier does not expose its fixed task scope: %#v", first.Verifier)
	}
	secondVerifier, ok := second.Verifier.(ScopedTicketVerifier)
	if !ok || secondVerifier.ScopeID() != second.ScopeID {
		t.Fatalf("second verifier does not expose its fixed task scope: %#v", second.Verifier)
	}

	identity := mustTicketIdentity(t, `{"path":"same.txt"}`)
	ticket, err := first.Issuer.Issue("same-call", identity)
	if err != nil {
		t.Fatalf("issue first task ticket: %v", err)
	}
	if err := second.Verifier.VerifyAndConsume(ticket, "same-call", identity); err == nil {
		t.Fatal("second task consumed first task's ticket")
	}
	if err := first.Verifier.VerifyAndConsume(ticket, "same-call", identity); err != nil {
		t.Fatalf("cross-task attempt consumed or corrupted first ticket: %v", err)
	}
	if err := first.Verifier.VerifyAndConsume(ticket, "same-call", identity); err == nil {
		t.Fatal("first task ticket was replayed")
	}
}

func containsConfirmationScope(scopes []ConfirmationScope, scope GrantScope) bool {
	for _, candidate := range scopes {
		if candidate.Scope == scope && candidate.Available {
			return true
		}
	}
	return false
}
