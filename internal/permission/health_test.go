package permission

import (
	"errors"
	"strings"
	"testing"
)

func TestCorruptLayerFailsClosedForAllDangerousTools(t *testing.T) {
	root := t.TempDir()
	authority := mustTicketAuthority(t)
	health := NewHealth(authority)
	authorizer := Authorizer{
		Health: health,
		User: RuleLayer{Rules: []Rule{
			{Tool: "Read", Pattern: "blocked.txt", MatchType: string(MatchExact), Effect: string(EffectDeny)},
			{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)},
		}},
	}

	const sensitiveError = "permission-secret-should-not-leak"
	identity := mustTicketIdentity(t, `{"operation":"before-corruption"}`)
	var stale ExecutionTicket
	loadErrors := make([]LoadError, 0, 3)
	for index, layer := range []SourceKind{SourceUserRule, SourceProjectRule, SourceLocalRule} {
		var err error
		stale, err = authority.Issue("issued-before-corruption", identity)
		if err != nil {
			t.Fatalf("issue ticket before corrupting %s: %v", layer, err)
		}
		loadErrors = append(loadErrors, LoadError{
			Source: Source{Kind: layer, Description: "permission layer"},
			Err:    errors.New(sensitiveError),
		})
		health.Update(loadErrors)
		if err := authority.VerifyAndConsume(stale, "issued-before-corruption", identity); err == nil {
			t.Fatalf("ticket survived corruption of layer %d (%s)", index, layer)
		}
	}
	if !health.Degraded() {
		t.Fatal("corrupt permission layer did not enter degraded state")
	}

	dangerous := []Call{
		{ID: "bash", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`},
		{ID: "write", Name: "Write", ArgumentsJSON: `{"path":"out.txt","content":"x"}`},
		{ID: "edit", Name: "Edit", ArgumentsJSON: `{"path":"out.txt","old_text":"x","new_text":"y"}`},
		{ID: "mcp", Name: "mcp__server__apparently_read_only", ArgumentsJSON: `{}`},
		{ID: "unknown", Name: "FutureBuiltin", ArgumentsJSON: `{}`},
	}
	for _, call := range dangerous {
		decision := authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModePermissive})
		if decision.Kind != DecisionDeny || decision.Reason != ReasonConfigError || decision.Prompt != nil {
			t.Errorf("degraded decision for %s = %#v, want config-error deny without confirmation", call.Name, decision)
		}
		visible := decision.Source.Description + decision.UserMessage + decision.ModelMessage
		if strings.Contains(visible, sensitiveError) {
			t.Errorf("degraded decision for %s leaked the raw load error", call.Name)
		}
	}

	readOnly := []Call{
		{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"blocked.txt"}`},
		{ID: "glob", Name: "Glob", ArgumentsJSON: `{"path":"."}`},
		{ID: "grep", Name: "Grep", ArgumentsJSON: `{"path":"."}`},
	}
	for _, call := range readOnly {
		decision := authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModeStrict})
		if decision.Kind != DecisionAllow || decision.Source.Kind != SourceHardConstraint {
			t.Errorf("degraded decision for allowlisted %s = %#v, want conservative allow", call.Name, decision)
		}
	}
	outOfRoot := authorizer.Decide(
		Call{ID: "outside", Name: "Read", ArgumentsJSON: `{"path":"../outside.txt"}`},
		Context{ProjectRoot: root, Mode: ModePermissive},
	)
	if outOfRoot.Kind != DecisionDeny {
		t.Fatalf("degraded read escaped restricted roots: %#v", outOfRoot)
	}

	health.Update(nil)
	if health.Degraded() {
		t.Fatal("cleared permission errors did not leave degraded state")
	}
	if err := authority.VerifyAndConsume(stale, "issued-before-corruption", identity); err == nil {
		t.Fatal("recovery revived a ticket invalidated by corruption")
	}
}
