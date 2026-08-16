package safefs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestProtectionReadonlySlotsRejectGitAndControlDomains(t *testing.T) {
	rootPath := t.TempDir()
	for _, directory := range []string{".control", ".xagent", ".xagent/worktrees"} {
		if err := os.Mkdir(filepath.Join(rootPath, filepath.FromSlash(directory)), 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootPath, ".git"), []byte("gitdir: protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(rootPath, ".git"), filepath.Join(rootPath, "git-alias")); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{".control/record.json", ".xagent/worktrees/lease"} {
		if err := os.WriteFile(filepath.Join(rootPath, filepath.FromSlash(file)), []byte("protected"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	opened := mustBootstrap(t, rootPath, Policy{
		ProtectedSlots: []string{"permissions.json"},
		ReadonlySlots:  []string{".git", ".control", ".xagent/worktrees"},
	})
	defer mustClose(t, opened.Root)
	for _, relative := range []string{".git", "git-alias", ".control", ".control/record.json", ".xagent", ".xagent/worktrees", ".xagent/worktrees/lease"} {
		binding := mustBind(t, opened.Root, relative)
		if err := opened.Root.authorizeWrite(opened.Capabilities.Ordinary(), binding); err == nil {
			t.Fatalf("ordinary capability wrote readonly slot %q", relative)
		}
		if err := opened.Root.authorizeWrite(opened.Capabilities.Protected(), binding); err == nil {
			t.Fatalf("protected capability wrote readonly slot %q", relative)
		}
	}
	if err := opened.Root.authorizeWrite(opened.Capabilities.Ordinary(), mustBind(t, opened.Root, "source.go")); err != nil {
		t.Fatal("readonly slots denied an unrelated task file")
	}
}

func TestProtectionReadonlyAncestorAndAliasCollisionsFailClosed(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, ".control"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, ".control", "record"), []byte("record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if opened, err := Bootstrap(rootPath, Policy{ReadonlySlots: []string{".control"}, ProtectedSlots: []string{".control/record"}}); err == nil {
		_ = opened.Root.Close()
		t.Fatal("readonly ancestor/protected descendant collision was accepted")
	}
	if err := os.WriteFile(filepath.Join(rootPath, "first"), []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(rootPath, "first"), filepath.Join(rootPath, "second")); err != nil {
		t.Fatal(err)
	}
	if opened, err := Bootstrap(rootPath, Policy{ReadonlySlots: []string{"first", "second"}}); err == nil {
		_ = opened.Root.Close()
		t.Fatal("readonly hard-link alias collision was accepted")
	}
}

func TestProtectionPlanUsesExactRootIdentityAndBoundCapabilities(t *testing.T) {
	taskPath := t.TempDir()
	task := mustBootstrap(t, taskPath, Policy{ReadonlySlots: []string{".git"}})
	defer mustClose(t, task.Root)
	main := mustBootstrap(t, t.TempDir(), Policy{})
	defer mustClose(t, main.Root)
	other := mustBootstrap(t, t.TempDir(), Policy{})
	defer mustClose(t, other.Root)
	sharedPath := t.TempDir()
	shared := mustBootstrap(t, sharedPath, Policy{})
	defer mustClose(t, shared.Root)

	plan, err := NewProtectionPlan(ProtectionPlanOptions{
		Writable: []*Root{task.Root},
		Readonly: []*Root{main.Root, other.Root, shared.Root},
	})
	if err != nil {
		t.Fatalf("new protection plan: %v", err)
	}
	capabilities, err := plan.Capabilities(task.Root)
	if err != nil {
		t.Fatalf("task capabilities: %v", err)
	}
	if err := task.Root.authorizeWrite(capabilities.Ordinary(), mustBind(t, task.Root, "task.go")); err != nil {
		t.Fatal("plan-bound task capability was rejected")
	}
	if err := task.Root.authorizeWrite(task.Capabilities.Ordinary(), mustBind(t, task.Root, "legacy.go")); err == nil {
		t.Fatal("assembly-root capability bypassed the active task protection plan")
	}
	if _, err := plan.Capabilities(main.Root); err == nil {
		t.Fatal("readonly main root received write capabilities")
	}
	if err := plan.ProbeReadonlyPath(sharedPath); err != nil {
		t.Fatalf("declared shared dependency was not confirmed readonly: %v", err)
	}
	if err := plan.ProbeReadonlyPath(t.TempDir()); err == nil {
		t.Fatal("unlisted path was confirmed readonly")
	}

	alias := mustBootstrap(t, taskPath, Policy{})
	defer mustClose(t, alias.Root)
	if _, err := plan.Capabilities(alias.Root); err == nil {
		t.Fatal("same-object root reopened with a different policy received task capabilities")
	}
	if _, err := NewProtectionPlan(ProtectionPlanOptions{Writable: []*Root{task.Root}, Readonly: []*Root{alias.Root}}); err == nil {
		t.Fatal("writable/readonly identity overlap was accepted")
	}
	if err := shared.Root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := plan.ProbeReadonlyPath(sharedPath); err == nil {
		t.Fatal("closed readonly Root still authorized a shared dependency path")
	}
}

func TestProtectionPlanCapabilityRevalidatesAtAtomicPublish(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "target.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, opened.Root)
	plan, err := NewProtectionPlan(ProtectionPlanOptions{Writable: []*Root{opened.Root}})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := plan.Capabilities(opened.Root)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(rootPath, "replacement.txt")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = opened.Root.AtomicWrite(context.Background(), capabilities.Ordinary(), "target.txt", 0o600, func(writer io.Writer) error {
		if _, err := writer.Write([]byte("staged")); err != nil {
			return err
		}
		return os.Rename(replacement, filepath.Join(rootPath, "target.txt"))
	})
	if err == nil {
		t.Fatal("atomic publish accepted a replaced target identity")
	}
}

func TestProtectionPlanRejectsWritableRootAsReadonlyDependency(t *testing.T) {
	opened := mustBootstrap(t, t.TempDir(), Policy{})
	defer mustClose(t, opened.Root)
	if _, err := NewProtectionPlan(ProtectionPlanOptions{Writable: []*Root{opened.Root}}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProtectionPlan(ProtectionPlanOptions{Readonly: []*Root{opened.Root}}); err == nil {
		t.Fatal("active writable root was accepted as a readonly dependency")
	}
}
