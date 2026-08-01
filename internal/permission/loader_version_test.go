package permission

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuleFileVersionMissingAndZeroAreLegacyV1(t *testing.T) {
	for name, prefix := range map[string]string{
		"missing": "",
		"zero":    "version: 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			file, err := DecodeRuleFile([]byte(prefix + "rules:\n  - tool: Bash\n    pattern: git status\n    match_type: exact\n    effect: allow\n"))
			if err != nil {
				t.Fatalf("decode legacy version: %v", err)
			}
			if file.Version != 1 || len(file.Rules) != 1 {
				t.Fatalf("legacy RuleFile = %#v, want v1 with one rule", file)
			}
			if file.Rules[0].Trust != RuleTrustLegacyUntrusted {
				t.Fatalf("legacy Bash allow trust=%q, want %q", file.Rules[0].Trust, RuleTrustLegacyUntrusted)
			}
		})
	}
}

func TestRuleFileVersionOneIsCurrent(t *testing.T) {
	file, err := DecodeRuleFile([]byte("version: 1\nrules:\n  - tool: Bash\n    pattern: git status\n    match_type: exact\n    effect: allow\n"))
	if err != nil {
		t.Fatalf("decode current version: %v", err)
	}
	if file.Version != 1 || len(file.Rules) != 1 {
		t.Fatalf("current RuleFile = %#v, want v1 with one rule", file)
	}
	if file.Rules[0].Trust == RuleTrustLegacyUntrusted {
		t.Fatal("current-version Bash allow was marked legacy_untrusted")
	}
}

func TestInvalidRuleFileVersionCorruptsLayerAndFailsClosed(t *testing.T) {
	invalid := map[string]string{
		"negative":   "-1",
		"future":     "2",
		"overflow":   "18446744073709551616",
		"string":     `"1"`,
		"boolean":    "true",
		"fractional": "1.0",
	}
	for name, scalar := range invalid {
		t.Run(name, func(t *testing.T) {
			root := newVersionTestRoot(t, "version: "+scalar+"\nrules: []\n")
			loaded := LoadRules(root)
			if len(loaded.Errors) != 1 || loaded.Errors[0].Source.Kind != SourceProjectRule {
				t.Fatalf("invalid version errors = %#v, want corrupt project layer", loaded.Errors)
			}
			authorizer := Authorizer{LoadErrors: loaded.Errors}
			for _, call := range []Call{
				{ID: "bash", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`},
				{ID: "write", Name: "Write", ArgumentsJSON: `{"path":"out.txt","content":"x"}`},
				{ID: "mcp", Name: "mcp__server__tool", ArgumentsJSON: `{}`},
			} {
				decision := authorizer.Decide(call, Context{ProjectRoot: root, Mode: ModePermissive})
				if decision.Kind != DecisionDeny || decision.Reason != ReasonConfigError {
					t.Errorf("invalid version decision for %s = %#v", call.Name, decision)
				}
			}
		})
	}
}

func TestRuleVersionFailureIsNonDestructiveAndSafe(t *testing.T) {
	const rawScalar = "version-scalar-do-not-echo"
	original := []byte("version: " + rawScalar + "\nrules: []\n")
	root := newVersionTestRoot(t, string(original))
	path := ProjectRulePath(root)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	loaded := LoadRules(root)
	if len(loaded.Errors) != 1 {
		t.Fatalf("invalid version errors=%d, want 1", len(loaded.Errors))
	}
	visible := loaded.Errors[0].Err.Error()
	authorizer := Authorizer{LoadErrors: loaded.Errors}
	decision := authorizer.Decide(
		Call{ID: "bash", Name: "Bash", ArgumentsJSON: `{"command":"git status"}`},
		Context{ProjectRoot: root, Mode: ModePermissive},
	)
	visible += decision.Source.Description + decision.UserMessage + decision.ModelMessage
	if strings.Contains(visible, rawScalar) {
		t.Fatal("version failure exposed the raw invalid scalar")
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(original) || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("version failure modified the permission rule file")
	}
}

func newVersionTestRoot(t *testing.T, projectRules string) string {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".xagent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProjectRulePath(root), []byte(projectRules), 0o640); err != nil {
		t.Fatal(err)
	}
	return root
}
