package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/safefs"
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

func TestPermissionYAMLRejectsHookOnlyMatchers(t *testing.T) {
	for name, input := range map[string]string{
		"regex":  "version: 1\nrules:\n  - tool: Bash\n    pattern: 'git .*'\n    match_type: regex\n    effect: allow\n",
		"negate": "version: 1\nrules:\n  - tool: Bash\n    pattern: git status\n    match_type: exact\n    effect: allow\n    negate: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRuleFile([]byte(input)); err == nil {
				t.Fatalf("permission YAML accepted Hook-only %s syntax", name)
			}
		})
	}
}

func TestPermissionMatcherCompatibility(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		call    Call
		rule    Rule
		matched bool
	}{
		{name: "bash exact normalizes whitespace", call: Call{Name: "Bash", ArgumentsJSON: `{"command":"  git   status "}`}, rule: Rule{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact)}, matched: true},
		{name: "exact stays case sensitive", call: Call{Name: "Bash", ArgumentsJSON: `{"command":"git Status"}`}, rule: Rule{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact)}},
		{name: "path doublestar", call: Call{Name: "Read", ArgumentsJSON: `{"path":"internal/deep/file.go"}`}, rule: Rule{Tool: "Read", Pattern: "internal/**/*.go", MatchType: string(MatchGlob)}, matched: true},
		{name: "glob does not become regex", call: Call{Name: "Bash", ArgumentsJSON: `{"command":"git status"}`}, rule: Rule{Tool: "Bash", Pattern: `git .+`, MatchType: string(MatchGlob)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := NormalizeCall(test.call, root)
			if err != nil {
				t.Fatal(err)
			}
			matched, err := MatchRule(test.rule, normalized)
			if err != nil {
				t.Fatal(err)
			}
			if matched != test.matched {
				t.Fatalf("MatchRule() = %v, want %v", matched, test.matched)
			}
		})
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
	writer := newPermissionWriter(t, root)
	authorizer := Authorizer{Session: NewSession(), Writer: writer}
	if decision := authorizer.ResolveUserDecision(call, Context{ProjectRoot: root, Mode: ModeDefault}, ActionAllowSession); decision.Kind != DecisionAllow {
		t.Fatalf("expected session allow, got %#v", decision)
	}
	followUp := authorizer.Decide(Call{ID: "2", Name: "Write", ArgumentsJSON: call.ArgumentsJSON}, Context{ProjectRoot: root, Mode: ModeDefault})
	if followUp.Kind != DecisionAllow || followUp.Source.Kind != SourceSessionRule {
		t.Fatalf("expected follow-up session allow, got %#v", followUp)
	}

	permanent := Authorizer{Writer: writer}
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
	authorizer := Authorizer{Writer: Writer{}}
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
	opened, err := safefs.Bootstrap(root, safefs.Policy{ProtectedSlots: []string{localPermissionSlot}})
	if err == nil {
		_ = opened.Root.Close()
		t.Fatal("expected permission Root bootstrap to reject symlink .xagent directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "permissions.local.yaml")); !os.IsNotExist(err) {
		t.Fatalf("writer created permissions file outside project, stat err: %v", err)
	}
}

func TestPermanentWriterWritesLocalRuleAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	writer := newPermissionWriter(t, root)
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
	authorizer := Authorizer{Writer: Writer{}}
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

	writer := Writer{}
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
	permanent := (&Authorizer{Writer: Writer{}}).ResolveUserDecision(call, Context{ProjectRoot: root, Mode: ModeDefault}, ActionAllowPermanent)
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

func TestNormalizeArgumentsMapPreservesNumbersAndRepresentation(t *testing.T) {
	arguments := map[string]any{
		"large":   json.Number("9007199254740993"),
		"decimal": json.Number("1.2300"),
		"nested":  map[string]any{"enabled": true},
	}
	call := Call{ID: "1", Name: "mcp__server__tool", ArgumentsJSON: `{"large":9007199254740993,"decimal":1.2300,"nested":{"enabled":true}}`}
	normalized, err := NormalizeArguments(call, arguments, Context{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := normalized.Arguments["large"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("large number changed: %#v", normalized.Arguments["large"])
	}
	if got, ok := normalized.Arguments["decimal"].(json.Number); !ok || got.String() != "1.2300" {
		t.Fatalf("decimal changed: %#v", normalized.Arguments["decimal"])
	}
	arguments["same_map"] = true
	if normalized.Arguments["same_map"] != true {
		t.Fatalf("NormalizeArguments copied/reparsed the argument map")
	}
	legacy, err := NormalizeCall(call, normalized.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(legacy) != Fingerprint(normalized) {
		t.Fatalf("map and legacy normalization fingerprints differ:\nlegacy=%s\nmap=%s", Fingerprint(legacy), Fingerprint(normalized))
	}
}

func TestCheckHardSeparatesNonOverridableConstraints(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name       string
		authorizer Authorizer
		call       Call
		wantReason DenyReason
	}{
		{name: "config corruption", authorizer: Authorizer{LoadErrors: []LoadError{{Err: os.ErrInvalid}}}, call: Call{Name: "Write", ArgumentsJSON: `{"path":"file.txt","content":"x"}`}, wantReason: ReasonConfigError},
		{name: "write permission config", call: Call{Name: "Write", ArgumentsJSON: `{"path":".xagent/permissions.yaml","content":"x"}`}, wantReason: ReasonSandbox},
		{name: "bash permission config", call: Call{Name: "Bash", ArgumentsJSON: `{"command":"sed -i x .xagent/permissions.local.yaml"}`}, wantReason: ReasonSandbox},
		{name: "bash blacklist", call: Call{Name: "Bash", ArgumentsJSON: `{"command":"git reset --hard"}`}, wantReason: ReasonBlacklist},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := NormalizeCall(test.call, root)
			if err != nil {
				t.Fatal(err)
			}
			decision := test.authorizer.CheckHard(normalized, Context{ProjectRoot: root, PlanMode: true})
			if decision == nil || decision.Kind != DecisionDeny || decision.Reason != test.wantReason {
				t.Fatalf("CheckHard() = %#v, want deny %s", decision, test.wantReason)
			}
		})
	}

	ordinary, err := NormalizeCall(Call{Name: "Write", ArgumentsJSON: `{"path":"ordinary.txt","content":"x"}`}, root)
	if err != nil {
		t.Fatal(err)
	}
	authorizer := Authorizer{User: RuleLayer{Rules: []Rule{{Tool: "Write", Pattern: "ordinary.txt", MatchType: string(MatchExact), Effect: string(EffectDeny)}}}}
	if hard := authorizer.CheckHard(ordinary, Context{ProjectRoot: root, PlanMode: true}); hard != nil {
		t.Fatalf("CheckHard included Plan or ordinary rules: %#v", hard)
	}
	if decision := authorizer.DecideOrdinary(ordinary, Context{ProjectRoot: root, Mode: ModePermissive}); decision.Kind != DecisionDeny || decision.Reason != ReasonRuleDeny {
		t.Fatalf("ordinary rule was not retained: %#v", decision)
	}
	if _, err := NormalizeArguments(Call{Name: "Write"}, map[string]any{"path": "../outside"}, Context{ProjectRoot: root}); err == nil {
		t.Fatal("path sandbox did not reject outside path during normalization")
	}
}

func TestDecideCompatibility(t *testing.T) {
	root := t.TempDir()
	authorizer := Authorizer{
		User: RuleLayer{Source: Source{Kind: SourceUserRule}, Rules: []Rule{
			{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)},
			{Tool: "Write", Pattern: "blocked.txt", MatchType: string(MatchExact), Effect: string(EffectDeny)},
		}},
	}
	for _, call := range []Call{
		{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"file.txt"}`},
		{ID: "allow", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`},
		{ID: "deny", Name: "Write", ArgumentsJSON: `{"path":"blocked.txt","content":"x"}`},
		{ID: "ask", Name: "Write", ArgumentsJSON: `{"path":"other.txt","content":"x"}`},
	} {
		context := Context{ProjectRoot: root, Mode: ModeDefault}
		legacy := authorizer.Decide(call, context)
		normalized, err := NormalizeCall(call, root)
		if err != nil {
			t.Fatal(err)
		}
		staged := authorizer.DecideOrdinary(normalized, context)
		if hard := authorizer.CheckHard(normalized, context); hard != nil {
			staged = *hard
		}
		if !reflect.DeepEqual(legacy, staged) {
			t.Fatalf("legacy/staged mismatch for %s:\nlegacy=%#v\nstaged=%#v", call.ID, legacy, staged)
		}
	}
}

func TestResolveNormalizedUserDecisionPreservesFingerprint(t *testing.T) {
	root := t.TempDir()
	call := Call{ID: "large", Name: "mcp__server__tool", ArgumentsJSON: `{"large":9007199254740993}`}
	arguments := map[string]any{"large": json.Number("9007199254740993")}
	normalized, err := NormalizeArguments(call, arguments, Context{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	authorizer := Authorizer{}
	decision := authorizer.ResolveNormalizedUserDecision(normalized, Context{ProjectRoot: root}, ActionAllowOnce)
	if decision.Kind != DecisionAllow || decision.Grant == nil || decision.Grant.Fingerprint != Fingerprint(normalized) {
		t.Fatalf("normalized allow changed fingerprint: %#v", decision)
	}
	if got, ok := normalized.Arguments["large"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("normalized arguments changed: %#v", normalized.Arguments)
	}
	for _, action := range []UserAction{ActionDeny, ActionCancel} {
		got := authorizer.ResolveNormalizedUserDecision(normalized, Context{ProjectRoot: root}, action)
		if got.Kind != DecisionDeny || !got.Recoverable {
			t.Fatalf("action %s changed: %#v", action, got)
		}
	}
	legacy := authorizer.ResolveUserDecision(call, Context{ProjectRoot: root}, ActionAllowOnce)
	if !reflect.DeepEqual(legacy, decision) {
		t.Fatalf("legacy normalized resolution differs:\nlegacy=%#v\nnormalized=%#v", legacy, decision)
	}
}

func TestDecideStagedCompatibility(t *testing.T) {
	root := t.TempDir()
	authorizer := Authorizer{User: RuleLayer{Source: Source{Kind: SourceUserRule}, Rules: []Rule{{Tool: "Bash", Pattern: "git *", MatchType: string(MatchGlob), Effect: string(EffectAllow)}}}}
	tests := []struct {
		call    Call
		context Context
	}{
		{call: Call{ID: "allow", Name: "Read", ArgumentsJSON: `{"path":"file.txt"}`}, context: Context{ProjectRoot: root, Mode: ModeDefault}},
		{call: Call{ID: "ask", Name: "Write", ArgumentsJSON: `{"path":"file.txt","content":"x"}`}, context: Context{ProjectRoot: root, Mode: ModeDefault}},
		{call: Call{ID: "hard", Name: "Bash", ArgumentsJSON: `{"command":"git reset --hard"}`}, context: Context{ProjectRoot: root, Mode: ModePermissive}},
		{call: Call{ID: "plan", Name: "Write", ArgumentsJSON: `{"path":"file.txt","content":"x"}`}, context: Context{ProjectRoot: root, Mode: ModePermissive, PlanMode: true}},
		{call: Call{ID: "large", Name: "mcp__server__tool", ArgumentsJSON: `{"large":9007199254740993}`}, context: Context{ProjectRoot: root, Mode: ModeDefault}},
	}
	for _, test := range tests {
		legacy := authorizer.Decide(test.call, test.context)
		normalized, err := NormalizeCallWithReadRoots(test.call, test.context.ProjectRoot, test.context.ReadRoots)
		if err != nil {
			t.Fatal(err)
		}
		var staged Decision
		if test.context.PlanMode && isWriteOrBash(test.call.Name) {
			staged = deny(test.call, ReasonPlanMode, Source{Kind: SourceHardConstraint, Description: "plan mode"}, "Plan Mode 下不允许执行写工具或 Bash", "Plan Mode allows only read-only tools.")
		} else if hard := authorizer.CheckHard(normalized, test.context); hard != nil {
			staged = *hard
		} else {
			staged = authorizer.DecideOrdinary(normalized, test.context)
		}
		if !reflect.DeepEqual(legacy, staged) {
			t.Fatalf("legacy/staged mismatch for %s:\nlegacy=%#v\nstaged=%#v", test.call.ID, legacy, staged)
		}
		if test.call.ID == "large" {
			if _, ok := normalized.Arguments["large"].(json.Number); !ok {
				t.Fatalf("large argument lost json.Number: %#v", normalized.Arguments["large"])
			}
		}
	}
}

func TestRulesNeverSubstituteForTicket(t *testing.T) {
	root := t.TempDir()
	normalized, err := NormalizeCall(
		Call{ID: "rule-call", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`},
		root,
	)
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)}
	match, ok, err := FindRuleMatch([]Rule{rule}, normalized)
	if err != nil || !ok || !match.AllowsWithoutPrompt() {
		t.Fatalf("FindRuleMatch() = (%#v, %v, %v), want prompt-suppressing match", match, ok, err)
	}

	authority := mustTicketAuthority(t)
	identity := mustTicketIdentity(t, `{"command":"git status"}`)
	if err := authority.VerifyAndConsume(ExecutionTicket{}, normalized.Call.ID, identity); err == nil {
		t.Fatal("rule match substituted for a signed execution ticket")
	}
	first, err := match.IssueTicket(authority, identity)
	if err != nil {
		t.Fatalf("issue first ticket from rule match: %v", err)
	}
	second, err := match.IssueTicket(authority, identity)
	if err != nil {
		t.Fatalf("issue second ticket from rule match: %v", err)
	}
	if first == second || first.nonce == second.nonce {
		t.Fatal("repeated rule match reused an execution ticket")
	}
	if err := authority.VerifyAndConsume(first, normalized.Call.ID, identity); err != nil {
		t.Fatalf("consume first freshly issued ticket: %v", err)
	}
	if err := authority.VerifyAndConsume(second, normalized.Call.ID, identity); err != nil {
		t.Fatalf("consume second freshly issued ticket: %v", err)
	}

	legacy := rule
	legacy.Trust = RuleTrustLegacyUntrusted
	legacyMatch, ok, err := FindRuleMatch([]Rule{legacy}, normalized)
	if err != nil || !ok {
		t.Fatalf("legacy rule was not observable for confirmation: ok=%v err=%v", ok, err)
	}
	if legacyMatch.AllowsWithoutPrompt() {
		t.Fatal("legacy_untrusted rule suppressed confirmation")
	}
	if _, err := legacyMatch.IssueTicket(authority, identity); err == nil {
		t.Fatal("legacy_untrusted rule issued an execution ticket")
	}

	if err := rule.ValidatePermanent(); err != nil {
		t.Fatalf("minimal exact permanent rule rejected: %v", err)
	}
	for name, unsafe := range map[string]Rule{
		"glob":   {Tool: "Bash", Pattern: "git *", MatchType: string(MatchGlob), Effect: string(EffectAllow)},
		"secret": {Tool: "Bash", Pattern: "api_key=secret-key", MatchType: string(MatchExact), Effect: string(EffectAllow)},
		"mcp":    {Tool: "mcp__server__tool", Pattern: `{}`, MatchType: string(MatchExact), Effect: string(EffectAllow)},
	} {
		if err := unsafe.ValidatePermanent(); err == nil {
			t.Errorf("unsafe permanent %s rule was accepted", name)
		}
	}
}

func TestMCPRuleDoesNotCrossServer(t *testing.T) {
	call := Call{ID: "mcp-call", Name: "mcp__alpha__lookup", ArgumentsJSON: `{"query":"safe"}`}
	normalized, err := NormalizeCall(call, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rule := Rule{Tool: call.Name, Pattern: call.ArgumentsJSON, MatchType: string(MatchExact), Effect: string(EffectAllow)}
	match, ok, err := FindRuleMatch([]Rule{rule}, normalized)
	if err != nil || !ok || !match.AllowsWithoutPrompt() {
		t.Fatalf("matching MCP rule = (%#v, %v, %v)", match, ok, err)
	}

	otherServer, err := NormalizeCall(
		Call{ID: call.ID, Name: "mcp__beta__lookup", ArgumentsJSON: call.ArgumentsJSON},
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FindRuleMatch([]Rule{rule}, otherServer); err != nil || ok {
		t.Fatalf("MCP rule crossed registered servers: ok=%v err=%v", ok, err)
	}

	targetA := [32]byte{1}
	targetB := [32]byte{2}
	identityA, err := NewCallIdentity(CallIdentityInput{
		ToolName:           call.Name,
		CanonicalArguments: []byte(call.ArgumentsJSON),
		TargetDigest:       &targetA,
	})
	if err != nil {
		t.Fatal(err)
	}
	identityB, err := NewCallIdentity(CallIdentityInput{
		ToolName:           call.Name,
		CanonicalArguments: []byte(call.ArgumentsJSON),
		TargetDigest:       &targetB,
	})
	if err != nil {
		t.Fatal(err)
	}
	authority := mustTicketAuthority(t)
	ticket, err := match.IssueTicket(authority, identityA)
	if err != nil {
		t.Fatalf("issue MCP ticket: %v", err)
	}
	if err := authority.VerifyAndConsume(ticket, call.ID, identityB); err == nil {
		t.Fatal("MCP ticket crossed final server configuration digests")
	}
	if err := authority.VerifyAndConsume(ticket, call.ID, identityA); err != nil {
		t.Fatalf("wrong target attempt consumed matching MCP ticket: %v", err)
	}
}

func quote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
