package permission

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"xagent/internal/safefs"
)

const localPermissionSlot = ".xagent/permissions.local.yaml"

func TestPermissionWriterRequiresDedicatedCapabilityAndIsCrashSafe(t *testing.T) {
	rootPath := t.TempDir()
	localPath := filepath.Join(rootPath, filepath.FromSlash(localPermissionSlot))
	if err := os.Mkdir(filepath.Dir(localPath), 0o700); err != nil {
		t.Fatal("create permission directory failed")
	}
	old := []byte("version: 1\nrules:\n  - tool: Bash\n    pattern: git status\n    match_type: exact\n    effect: allow\n")
	if err := os.WriteFile(localPath, old, 0o600); err != nil {
		t.Fatal("create prior permission file failed")
	}
	opened := mustBootstrapPermissionWriter(t, rootPath)
	t.Cleanup(func() { mustClosePermissionRoot(t, opened.Root) })
	rule := Rule{Tool: "Bash", Pattern: "git branch", MatchType: string(MatchExact), Effect: string(EffectAllow)}

	for name, capability := range map[string]safefs.Capability{
		"zero":     {},
		"ordinary": opened.Capabilities.Ordinary(),
	} {
		t.Run(name, func(t *testing.T) {
			writer := NewWriter(opened.Root, capability)
			if err := writer.WriteLocal(rule); err == nil {
				t.Fatal("writer accepted a capability without dedicated protected authority")
			}
			assertPermissionFileBytes(t, localPath, old)
		})
	}

	writer := NewWriter(opened.Root, opened.Capabilities.Protected())
	if err := writer.WriteLocal(rule); err != nil {
		t.Fatalf("dedicated permission write failed: %v", err)
	}
	committed, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal("read committed permission file failed")
	}
	if bytes.Equal(committed, old) {
		t.Fatal("dedicated permission write did not publish the new version")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal("stat committed permission file failed")
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permission file mode = %v, want 0600", info.Mode().Perm())
	}

	if err := opened.Root.Close(); err != nil {
		t.Fatal("close permission root failed")
	}
	authorizer := Authorizer{
		Local:  RuleLayer{Rules: []Rule{rule}},
		Writer: writer,
	}
	decision := authorizer.ResolveUserDecision(
		Call{ID: "closed-root", Name: "Bash", ArgumentsJSON: `{"command":"git log"}`},
		Context{ProjectRoot: rootPath, Mode: ModeDefault},
		ActionAllowPermanent,
	)
	if decision.Kind != DecisionDeny || decision.Reason != ReasonConfigError {
		t.Fatalf("failed atomic write decision = %#v, want config_error deny", decision)
	}
	if len(authorizer.Local.Rules) != 1 || authorizer.Local.Rules[0] != rule {
		t.Fatalf("failed atomic write changed in-memory rules: %#v", authorizer.Local.Rules)
	}
	assertPermissionFileBytes(t, localPath, committed)
}

func TestPermissionWriterRejectsCrossRootCapability(t *testing.T) {
	firstPath := t.TempDir()
	secondPath := t.TempDir()
	for _, rootPath := range []string{firstPath, secondPath} {
		if err := os.Mkdir(filepath.Join(rootPath, ".xagent"), 0o700); err != nil {
			t.Fatal("create permission directory failed")
		}
	}
	old := []byte("version: 1\nrules: []\n")
	firstLocal := filepath.Join(firstPath, filepath.FromSlash(localPermissionSlot))
	secondLocal := filepath.Join(secondPath, filepath.FromSlash(localPermissionSlot))
	if err := os.WriteFile(firstLocal, old, 0o600); err != nil {
		t.Fatal("create first permission file failed")
	}
	if err := os.WriteFile(secondLocal, old, 0o600); err != nil {
		t.Fatal("create second permission file failed")
	}
	first := mustBootstrapPermissionWriter(t, firstPath)
	second := mustBootstrapPermissionWriter(t, secondPath)
	t.Cleanup(func() { mustClosePermissionRoot(t, first.Root) })
	t.Cleanup(func() { mustClosePermissionRoot(t, second.Root) })

	writer := NewWriter(second.Root, first.Capabilities.Protected())
	rule := Rule{Tool: "Bash", Pattern: "git status", MatchType: string(MatchExact), Effect: string(EffectAllow)}
	if err := writer.WriteLocal(rule); err == nil {
		t.Fatal("writer accepted a protected capability issued for another Root")
	}
	assertPermissionFileBytes(t, firstLocal, old)
	assertPermissionFileBytes(t, secondLocal, old)
}

func mustBootstrapPermissionWriter(t *testing.T, rootPath string) safefs.OpenResult {
	t.Helper()
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{ProtectedSlots: []string{localPermissionSlot}})
	if err != nil {
		t.Fatal("bootstrap permission root failed")
	}
	return opened
}

func newPermissionWriter(t *testing.T, rootPath string) Writer {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(rootPath, ".xagent"), 0o700); err != nil {
		t.Fatal("create permission directory failed")
	}
	opened := mustBootstrapPermissionWriter(t, rootPath)
	t.Cleanup(func() { mustClosePermissionRoot(t, opened.Root) })
	return NewWriter(opened.Root, opened.Capabilities.Protected())
}

func mustClosePermissionRoot(t *testing.T, root *safefs.Root) {
	t.Helper()
	if err := root.Close(); err != nil {
		t.Fatal("close permission root failed")
	}
}

func assertPermissionFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read permission file failed")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("permission file changed:\ngot:  %q\nwant: %q", got, want)
	}
}
