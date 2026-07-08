package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBashExactDoesNotMatchCompoundCommand(t *testing.T) {
	call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"git status && rm -rf ."}`}
	normalized, err := NormalizeCall(call, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)}
	matched, err := MatchRule(rule, normalized)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("exact rule matched compound command")
	}
}

func TestBashGlobDoesNotMatchComplexShell(t *testing.T) {
	for _, command := range []string{
		"git status && rm -rf .",
		"git status | cat",
		"git status > out.txt",
		"git status\ngit branch",
		"bash -lc 'git status'",
		"sh -c 'git status'",
		"find . -exec rm {} \\;",
		"git status | xargs echo",
	} {
		call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":` + quote(command) + `}`}
		normalized, err := NormalizeCall(call, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rule := Rule{Tool: "Bash", Pattern: "git *", MatchType: string(MatchGlob), Effect: string(EffectAllow)}
		matched, err := MatchRule(rule, normalized)
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			t.Fatalf("glob rule matched complex shell command %q", command)
		}
	}
}

func TestBlacklistOverridesRulesAndMode(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"git reset --hard"}`}
	authorizer := Authorizer{User: RuleLayer{Source: Source{Kind: SourceUserRule}, Rules: []Rule{{Tool: "Bash", Pattern: "git reset --hard", MatchType: string(MatchExact), Effect: string(EffectAllow)}}}}
	decision := authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModePermissive})
	if decision.Kind != DecisionDeny || decision.Reason != ReasonBlacklist {
		t.Fatalf("expected blacklist deny, got %#v", decision)
	}
}

func TestSandboxRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveProjectPath(root, "outside/secret.txt")
	if err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestRuleLayerPriority(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`}
	authorizer := Authorizer{
		User:    RuleLayer{Source: Source{Kind: SourceUserRule}, Rules: []Rule{{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)}}},
		Project: RuleLayer{Source: Source{Kind: SourceProjectRule}, Rules: []Rule{{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectDeny)}}},
	}
	decision := authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModeDefault})
	if decision.Kind != DecisionDeny || decision.Source.Kind != SourceProjectRule {
		t.Fatalf("expected project deny over user allow, got %#v", decision)
	}
}

func TestPermissiveBashOnlyAllowsBuiltinReadOnlyCommands(t *testing.T) {
	root := t.TempDir()
	allowed := (&Authorizer{}).Decide(Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`}, Context{ProjectRoot: root, Mode: ModePermissive})
	if allowed.Kind != DecisionAllow {
		t.Fatalf("expected builtin read-only bash allow, got %#v", allowed)
	}
	python := (&Authorizer{}).Decide(Call{ID: "2", Name: "Bash", ArgumentsJSON: `{"command":"python -c 'print(1)'"}`}, Context{ProjectRoot: root, Mode: ModePermissive})
	if python.Kind != DecisionAsk {
		t.Fatalf("expected unknown bash to ask in permissive mode, got %#v", python)
	}
}

func TestLoadErrorsFailClosedForDangerousTools(t *testing.T) {
	root := t.TempDir()
	authorizer := Authorizer{LoadErrors: []LoadError{{Source: Source{Kind: SourceProjectRule}, Err: os.ErrInvalid}}}
	write := authorizer.Decide(Call{ID: "1", Name: "Write", ArgumentsJSON: `{"path":"a.txt","content":"x"}`}, Context{ProjectRoot: root, Mode: ModePermissive})
	if write.Kind != DecisionDeny || write.Reason != ReasonConfigError {
		t.Fatalf("expected config_error deny for write, got %#v", write)
	}
	read := authorizer.Decide(Call{ID: "2", Name: "Read", ArgumentsJSON: `{"path":"a.txt"}`}, Context{ProjectRoot: root, Mode: ModePermissive})
	if read.Kind == DecisionDeny && read.Reason == ReasonConfigError {
		t.Fatalf("read-only tool should not fail closed on config error: %#v", read)
	}
}

func TestModeCannotOverrideExplicitDeny(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`}
	authorizer := Authorizer{User: RuleLayer{Source: Source{Kind: SourceUserRule}, Rules: []Rule{{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectDeny)}}}}
	decision := authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModePermissive})
	if decision.Kind != DecisionDeny || decision.Reason != ReasonRuleDeny {
		t.Fatalf("expected explicit deny, got %#v", decision)
	}
}

func TestPlanModeDeniesWriteAndBashWithoutAsk(t *testing.T) {
	root := t.TempDir()
	for _, call := range []Call{
		{ID: "1", Name: "Write", ArgumentsJSON: `{"path":"a.txt","content":"x"}`},
		{ID: "2", Name: "Edit", ArgumentsJSON: `{"path":"a.txt","old_text":"x","new_text":"y"}`},
		{ID: "3", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`},
	} {
		decision := (&Authorizer{}).Decide(call, Context{ProjectRoot: root, Mode: ModePermissive, PlanMode: true})
		if decision.Kind != DecisionDeny || decision.Reason != ReasonPlanMode || decision.Prompt != nil {
			t.Fatalf("expected plan mode hard deny for %s, got %#v", call.Name, decision)
		}
	}
}

func TestDecodeRuleFileRejectsUnknownFields(t *testing.T) {
	_, err := DecodeRuleFile([]byte("version: 1\nrules:\n  - tool: Bash\n    pattern: git status\n    match_type: exact\n    effect: allow\n    typo: nope\n"))
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestGlobSupportsDoublestarAndPathParam(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "deep", "file.go"), []byte("package deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "1", Name: "Edit", ArgumentsJSON: `{"path":"internal/deep/file.go","old_text":"deep","new_text":"x"}`}
	normalized, err := NormalizeCall(call, root)
	if err != nil {
		t.Fatal(err)
	}
	matched, err := MatchRule(Rule{Tool: "Edit", Pattern: "internal/**/*.go", MatchType: string(MatchGlob), Effect: string(EffectDeny), PathParam: "path"}, normalized)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected doublestar glob with path_param to match")
	}
}

func TestPermissionConfigFilesAreHardProtected(t *testing.T) {
	root := t.TempDir()
	for _, call := range []Call{
		{ID: "1", Name: "Write", ArgumentsJSON: `{"path":".xagent/permissions.yaml","content":"rules: []"}`},
		{ID: "2", Name: "Edit", ArgumentsJSON: `{"path":".xagent/permissions.local.yaml","old_text":"x","new_text":"y"}`},
		{ID: "3", Name: "Bash", ArgumentsJSON: `{"command":"python update.py .xagent/permissions.yaml"}`},
	} {
		decision := (&Authorizer{}).Decide(call, Context{ProjectRoot: root, Mode: ModePermissive})
		if decision.Kind != DecisionDeny || decision.Source.Description != "permission config protection" {
			t.Fatalf("expected permission config hard deny, got %#v", decision)
		}
	}
}

func TestAllowSessionAndPermanentAffectFollowingCalls(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "1", Name: "Write", ArgumentsJSON: `{"path":"a.txt","content":"hello"}`}
	authorizer := Authorizer{Session: NewSession(), Writer: Writer{ProjectRoot: root}}
	if decision := authorizer.ResolveUserDecision(call, Context{ProjectRoot: root, Mode: ModeDefault}, ActionAllowSession); decision.Kind != DecisionAllow {
		t.Fatalf("expected session allow, got %#v", decision)
	}
	followUp := authorizer.Decide(Call{ID: "2", Name: "Write", ArgumentsJSON: call.ArgumentsJSON}, Context{ProjectRoot: root, Mode: ModeDefault})
	if followUp.Kind != DecisionAllow || followUp.Source.Kind != SourceSessionRule {
		t.Fatalf("expected follow-up session allow, got %#v", followUp)
	}

	permanent := Authorizer{Writer: Writer{ProjectRoot: root}}
	if decision := permanent.ResolveUserDecision(call, Context{ProjectRoot: root, Mode: ModeDefault}, ActionAllowPermanent); decision.Kind != DecisionAllow {
		t.Fatalf("expected permanent allow, got %#v", decision)
	}
	followUp = permanent.Decide(Call{ID: "3", Name: "Write", ArgumentsJSON: call.ArgumentsJSON}, Context{ProjectRoot: root, Mode: ModeDefault})
	if followUp.Kind != DecisionAllow || followUp.Source.Kind != SourceLocalRule {
		t.Fatalf("expected follow-up local allow, got %#v", followUp)
	}
}

func TestPermanentAllowDisabledForComplexShell(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"git status && git branch"}`}
	authorizer := Authorizer{Writer: Writer{ProjectRoot: root}}
	decision := authorizer.ResolveUserDecision(call, Context{ProjectRoot: root, Mode: ModeDefault}, ActionAllowPermanent)
	if decision.Kind != DecisionDeny {
		t.Fatalf("expected permanent allow to be denied for complex shell, got %#v", decision)
	}
	if _, err := os.Stat(LocalRulePath(root)); !os.IsNotExist(err) {
		t.Fatalf("permanent rule file should not be written, stat err: %v", err)
	}
}

func TestPermanentWriterRejectsSymlinkXAgentDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".xagent")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writer := Writer{ProjectRoot: root}
	err := writer.WriteLocal(Rule{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)})
	if err == nil {
		t.Fatal("expected writer to reject symlink .xagent directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "permissions.local.yaml")); !os.IsNotExist(err) {
		t.Fatalf("writer created permissions file outside project, stat err: %v", err)
	}
}

func TestPermanentWriterWritesLocalRuleAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	writer := Writer{ProjectRoot: root}
	rule := Rule{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)}
	if err := writer.WriteLocal(rule); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteLocal(rule); err != nil {
		t.Fatal(err)
	}
	file, err := LoadRuleFile(LocalRulePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Rules) != 1 {
		t.Fatalf("expected one deduplicated rule, got %d", len(file.Rules))
	}
	info, err := os.Stat(LocalRulePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %v", info.Mode().Perm())
	}
}

func TestPermanentPermissionRulesNeverPersistSecrets(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"curl -H 'Authorization: Bearer abc123' https://example.test/?token=query-secret"}`}
	authorizer := Authorizer{Writer: Writer{ProjectRoot: root}}
	promptDecision := authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModeDefault})
	if promptDecision.Kind != DecisionAsk || promptDecision.Prompt == nil || promptDecision.Prompt.AllowPermanent {
		t.Fatalf("expected ask without permanent for secret-bearing command, got %#v", promptDecision)
	}
	decision := authorizer.ResolveUserDecision(call, Context{ProjectRoot: root, Mode: ModeDefault}, ActionAllowPermanent)
	if decision.Kind != DecisionDeny {
		t.Fatalf("expected permanent allow to be denied, got %#v", decision)
	}
	if _, err := os.Stat(LocalRulePath(root)); !os.IsNotExist(err) {
		t.Fatalf("permanent rule file should not be written, stat err: %v", err)
	}

	writer := Writer{ProjectRoot: root}
	err := writer.WriteLocal(Rule{Tool: "Bash", Pattern: "api_key=secret-key", MatchType: string(MatchExact), Effect: string(EffectAllow)})
	if err == nil {
		t.Fatal("expected writer to reject secret-bearing rule")
	}
	if _, err := os.Stat(LocalRulePath(root)); !os.IsNotExist(err) {
		t.Fatalf("writer persisted secret-bearing rule, stat err: %v", err)
	}
}

func TestPermissionDeniedResultIsModelSafe(t *testing.T) {
	call := Call{ID: "1", Name: "Bash", ArgumentsJSON: `{"command":"API_KEY=secret git status"}`}
	decision := deny(call, ReasonBlacklist, Source{Kind: SourceHardConstraint}, "blocked", "This command is blocked by a non-overridable safety rule.")
	content := DeniedModelMessage(decision)
	data := DeniedResultData(decision)
	if strings.Contains(content, "secret") || strings.Contains(content, "API_KEY") {
		t.Fatalf("model content leaked sensitive command: %q", content)
	}
	if data["reason"] != string(ReasonBlacklist) {
		t.Fatalf("unexpected denial data: %#v", data)
	}
}

func TestMCPToolsAreDynamicDangerousAndCannotPermanentAllow(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "1", Name: "mcp__server_a__search", ArgumentsJSON: `{"query":"hello"}`}
	if err := (Rule{Tool: call.Name, Pattern: `{"query":"hello"}`, MatchType: string(MatchExact), Effect: string(EffectAllow)}).Validate(); err != nil {
		t.Fatalf("expected mcp rule to validate: %v", err)
	}
	decision := (&Authorizer{}).Decide(call, Context{ProjectRoot: root, Mode: ModePermissive})
	if decision.Kind != DecisionAsk || decision.Prompt == nil || decision.Prompt.AllowPermanent {
		t.Fatalf("expected mcp tool to ask without permanent allow, got %#v", decision)
	}
	permanent := (&Authorizer{Writer: Writer{ProjectRoot: root}}).ResolveUserDecision(call, Context{ProjectRoot: root, Mode: ModeDefault}, ActionAllowPermanent)
	if permanent.Kind != DecisionDeny {
		t.Fatalf("expected mcp permanent allow to be denied, got %#v", permanent)
	}
}

func TestMCPFingerprintSeparatesRegisteredNameAndArguments(t *testing.T) {
	root := t.TempDir()
	first, err := NormalizeCall(Call{ID: "1", Name: "mcp__a__tool", ArgumentsJSON: `{"value":1}`}, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NormalizeCall(Call{ID: "2", Name: "mcp__b__tool", ArgumentsJSON: `{"value":1}`}, root)
	if err != nil {
		t.Fatal(err)
	}
	third, err := NormalizeCall(Call{ID: "3", Name: "mcp__a__tool", ArgumentsJSON: `{"value":2}`}, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(Fingerprint(first), "registered=mcp__a__tool") || !strings.Contains(Fingerprint(first), "server=a") || !strings.Contains(Fingerprint(first), "tool=tool") || !strings.Contains(Fingerprint(first), "args=") {
		t.Fatalf("mcp fingerprint missing server/tool/args identity: %q", Fingerprint(first))
	}
	if Fingerprint(first) == Fingerprint(second) {
		t.Fatalf("different registered tools reused fingerprint: %q", Fingerprint(first))
	}
	if Fingerprint(first) == Fingerprint(third) {
		t.Fatalf("different arguments reused fingerprint: %q", Fingerprint(first))
	}
}

func quote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
