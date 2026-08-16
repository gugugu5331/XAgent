package workspace

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeProtectionOpensExactWritableAndReadonlyRoots(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	paths := ProtectionPaths{
		Working:  makeDirectory(t, base, "worktree"),
		Scratch:  makeDirectory(t, base, "scratch"),
		Artifact: makeDirectory(t, base, "artifact"),
		Readonly: []string{makeDirectory(t, base, "main"), makeDirectory(t, base, "other"), makeDirectory(t, base, "shared")},
	}
	if err := os.WriteFile(filepath.Join(paths.Working, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	protection, err := OpenProtection(paths)
	if err != nil {
		t.Fatalf("OpenProtection() error = %v", err)
	}
	defer protection.Close()

	for _, target := range []struct {
		name string
		root WritableRoot
	}{
		{name: "working", root: protection.Working()},
		{name: "scratch", root: protection.Scratch()},
		{name: "artifact", root: protection.Artifact()},
	} {
		target := target
		t.Run(target.name, func(t *testing.T) {
			if target.root.Path == "" || target.root.Root == nil {
				t.Fatal("writable root was not retained")
			}
			if err := target.root.Root.AtomicWrite(context.Background(), target.root.Capabilities.Ordinary(), "owned.txt", 0o600, func(writer io.Writer) error {
				_, writeErr := writer.Write([]byte(target.name))
				return writeErr
			}); err != nil {
				t.Fatalf("task-bound write failed: %v", err)
			}
		})
	}
	for _, readonlySlot := range []string{".git", ".control", ".xagent"} {
		if err := protection.Working().Root.AtomicWrite(context.Background(), protection.Working().Capabilities.Ordinary(), readonlySlot, 0o600, func(writer io.Writer) error {
			_, writeErr := writer.Write([]byte("forbidden"))
			return writeErr
		}); err == nil {
			t.Fatalf("ordinary capability wrote readonly task slot %q", readonlySlot)
		}
	}

	if got := len(protection.Readonly()); got != 3 {
		t.Fatalf("readonly roots = %d, want 3", got)
	}
	for _, readonly := range protection.Readonly() {
		if readonly.Path == "" || readonly.Root == nil {
			t.Fatal("readonly root was not retained")
		}
		if err := protection.Plan().ProbeReadonlyPath(readonly.Path); err != nil {
			t.Fatalf("readonly root probe failed: %v", err)
		}
	}
	if protection.ProcessPlans() == nil {
		t.Fatal("proctree protection plan factory is missing")
	}
	if err := protection.Verify(); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestRuntimeProtectionRejectsUnsafeOrAliasedPaths(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	working := makeDirectory(t, base, "worktree")
	if err := os.WriteFile(filepath.Join(working, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := ProtectionPaths{
		Working:  working,
		Scratch:  makeDirectory(t, base, "scratch"),
		Artifact: makeDirectory(t, base, "artifact"),
		Readonly: []string{makeDirectory(t, base, "main")},
	}

	tests := []struct {
		name  string
		paths ProtectionPaths
	}{
		{name: "relative", paths: replaceWorking(valid, "relative")},
		{name: "unclean", paths: replaceWorking(valid, valid.Working+string(filepath.Separator)+".."+string(filepath.Separator)+filepath.Base(valid.Working))},
		{name: "writable alias", paths: replaceScratch(valid, valid.Working)},
		{name: "readonly alias", paths: replaceReadonly(valid, []string{valid.Working})},
		{name: "duplicate readonly", paths: replaceReadonly(valid, []string{valid.Readonly[0], valid.Readonly[0]})},
		{name: "missing main readonly root", paths: replaceReadonly(valid, nil)},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			if opened, err := OpenProtection(testCase.paths); err == nil {
				_ = opened.Close()
				t.Fatal("OpenProtection() accepted unsafe paths")
			}
		})
	}

	link := filepath.Join(base, "worktree-link")
	if err := os.Symlink(valid.Working, link); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenProtection(replaceWorking(valid, link)); err == nil {
		_ = opened.Close()
		t.Fatal("OpenProtection() followed a root symlink")
	}
}

func TestRuntimeProtectionConservativelyReservesWholeXAgentSlot(t *testing.T) {
	t.Parallel()
	protection, _ := openRuntimeFixture(t)
	defer protection.Close()
	if err := os.Mkdir(filepath.Join(protection.Working().Path, ".xagent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := protection.Working().Root.AtomicWrite(context.Background(), protection.Working().Capabilities.Ordinary(), ".xagent/project-local.txt", 0o600, func(writer io.Writer) error {
		_, writeErr := writer.Write([]byte("forbidden"))
		return writeErr
	}); err == nil {
		t.Fatal("ordinary capability wrote beneath the conservatively reserved .xagent slot")
	}
}

func TestRuntimeProtectionIdentityDriftFailsClosed(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	working := makeDirectory(t, base, "worktree")
	if err := os.WriteFile(filepath.Join(working, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := ProtectionPaths{
		Working:  working,
		Scratch:  makeDirectory(t, base, "scratch"),
		Artifact: makeDirectory(t, base, "artifact"),
		Readonly: []string{makeDirectory(t, base, "main")},
	}
	protection, err := OpenProtection(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer protection.Close()

	moved := filepath.Join(base, "main-old")
	if err := os.Rename(paths.Readonly[0], moved); err != nil {
		t.Fatal(err)
	}
	makeDirectory(t, base, "main")
	if err := protection.Verify(); err == nil {
		t.Fatal("Verify() accepted a replaced readonly path")
	}
}

func makeDirectory(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func replaceWorking(paths ProtectionPaths, working string) ProtectionPaths {
	paths.Working = working
	return paths
}

func replaceScratch(paths ProtectionPaths, scratch string) ProtectionPaths {
	paths.Scratch = scratch
	return paths
}

func replaceReadonly(paths ProtectionPaths, readonly []string) ProtectionPaths {
	paths.Readonly = readonly
	return paths
}
