//go:build darwin || linux

package safefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRootRejectsSymlinkSwap(t *testing.T) {
	rootPath := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal("create outside file failed")
	}
	inside := filepath.Join(rootPath, "slot")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal("create inside directory failed")
	}
	if err := os.WriteFile(filepath.Join(inside, "secret.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal("create inside file failed")
	}
	result := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, result.Root)

	file, err := result.Root.OpenRead(context.Background(), "slot/secret.txt")
	if err != nil {
		t.Fatal("open safe file failed")
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(data) != "inside" {
		t.Fatal("safe opened handle returned unexpected content")
	}

	held := filepath.Join(rootPath, "slot-held")
	if err := os.Rename(inside, held); err != nil {
		t.Fatal("move inside directory failed")
	}
	if err := os.Symlink(outside, inside); err != nil {
		t.Fatal("install replacement symlink failed")
	}
	if _, err := result.Root.Bind("slot/secret.txt"); err == nil {
		t.Fatal("Bind followed a replacement directory symlink")
	}
	if _, err := result.Root.OpenRead(context.Background(), "slot/secret.txt"); err == nil {
		t.Fatal("OpenRead followed a replacement directory symlink")
	}
	if err := os.Remove(inside); err != nil {
		t.Fatal("remove replacement symlink failed")
	}
	if err := os.Rename(held, inside); err != nil {
		t.Fatal("restore inside directory failed")
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(rootPath, "final-link")); err != nil {
		t.Fatal("install final symlink failed")
	}
	if _, err := result.Root.Bind("final-link"); err == nil {
		t.Fatal("Bind followed a final-component symlink")
	}
	if _, err := result.Root.OpenRead(context.Background(), "final-link"); err == nil {
		t.Fatal("OpenRead followed a final-component symlink")
	}
}

func TestProtectionReadonlyProbeRejectsSymlinkAndIdentitySwap(t *testing.T) {
	sharedPath := t.TempDir()
	shared := mustBootstrap(t, sharedPath, Policy{})
	defer mustClose(t, shared.Root)
	plan, err := NewProtectionPlan(ProtectionPlanOptions{Readonly: []*Root{shared.Root}})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.ProbeReadonlyPath(sharedPath); err != nil {
		t.Fatalf("initial readonly probe: %v", err)
	}
	held := sharedPath + "-held"
	if err := os.Rename(sharedPath, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sharedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := plan.ProbeReadonlyPath(sharedPath); err == nil {
		t.Fatal("replacement directory retained readonly authorization")
	}
	if err := os.Remove(sharedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(held, sharedPath); err != nil {
		t.Fatal(err)
	}
	if err := plan.ProbeReadonlyPath(sharedPath); err == nil {
		t.Fatal("symlink alias was authorized as a readonly root")
	}
}

func TestProtectionReadonlyPolicyRejectsExistingSymlink(t *testing.T) {
	rootPath := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(rootPath, "shared")); err != nil {
		t.Fatal(err)
	}
	if opened, err := Bootstrap(rootPath, Policy{ReadonlySlots: []string{"shared"}}); err == nil {
		_ = opened.Root.Close()
		t.Fatal("readonly policy accepted an existing symlink slot")
	}
}

func TestBindUsesOpenedIdentity(t *testing.T) {
	rootPath := t.TempDir()
	original := filepath.Join(rootPath, "original.txt")
	if err := os.WriteFile(original, []byte("first"), 0o600); err != nil {
		t.Fatal("create original failed")
	}
	if err := os.Link(original, filepath.Join(rootPath, "alias.txt")); err != nil {
		t.Fatal("create hard link failed")
	}
	result := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, result.Root)

	originalBinding := mustBind(t, result.Root, "original.txt")
	aliasBinding := mustBind(t, result.Root, "alias.txt")
	if !bytes.Equal(mustBindingBytes(t, originalBinding), mustBindingBytes(t, aliasBinding)) {
		t.Fatal("two opened handles for one object produced different canonical identities")
	}
	if err := os.WriteFile(filepath.Join(rootPath, "replacement.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal("create replacement failed")
	}
	if err := os.Rename(filepath.Join(rootPath, "replacement.txt"), original); err != nil {
		t.Fatal("replace original path failed")
	}
	replacementBinding := mustBind(t, result.Root, "original.txt")
	if bytes.Equal(mustBindingBytes(t, originalBinding), mustBindingBytes(t, replacementBinding)) {
		t.Fatal("replacement object reused the prior opened identity")
	}
}

func TestBindMissingUsesParentHandle(t *testing.T) {
	rootPath := t.TempDir()
	parent := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal("create parent failed")
	}
	result := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, result.Root)

	before := mustBind(t, result.Root, "parent/future.txt")
	if err := os.Rename(parent, filepath.Join(rootPath, "parent-old")); err != nil {
		t.Fatal("move bound parent failed")
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal("create replacement parent failed")
	}
	after := mustBind(t, result.Root, "parent/future.txt")
	if bytes.Equal(mustBindingBytes(t, before), mustBindingBytes(t, after)) {
		t.Fatal("missing binding ignored replacement of its opened parent")
	}
}

func TestAtomicWritePreservesPreviousVersionOnFailure(t *testing.T) {
	rootPath := t.TempDir()
	targetPath := filepath.Join(rootPath, "target.txt")
	if err := os.WriteFile(targetPath, []byte("previous"), 0o600); err != nil {
		t.Fatal("create prior version failed")
	}
	if err := os.Mkdir(filepath.Join(rootPath, "permissions"), 0o700); err != nil {
		t.Fatal("create protected parent failed")
	}
	protectedPath := filepath.Join(rootPath, "permissions", "rules.json")
	if err := os.WriteFile(protectedPath, []byte("protected-old"), 0o600); err != nil {
		t.Fatal("create protected version failed")
	}
	if err := os.Link(protectedPath, filepath.Join(rootPath, "protected-alias.json")); err != nil {
		t.Fatal("create protected hard-link alias failed")
	}
	nested := filepath.Join(rootPath, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal("create nested parent failed")
	}
	if err := os.WriteFile(filepath.Join(nested, "target.txt"), []byte("nested-old"), 0o600); err != nil {
		t.Fatal("create nested target failed")
	}
	result := mustBootstrap(t, rootPath, Policy{ProtectedSlots: []string{"permissions/rules.json"}})
	defer mustClose(t, result.Root)
	ordinary := result.Capabilities.Ordinary()
	protected := result.Capabilities.Protected()

	callbackFailure := errors.New("fixture write failure")
	err := result.Root.AtomicWrite(context.Background(), ordinary, "target.txt", 0o600, func(writer io.Writer) error {
		if _, writeErr := writer.Write([]byte("partial-new")); writeErr != nil {
			return writeErr
		}
		return callbackFailure
	})
	if err == nil {
		t.Fatal("writer callback failure was reported as success")
	}
	assertFileContent(t, targetPath, "previous")
	assertNoStagingFiles(t, rootPath)

	if err := result.Root.AtomicWrite(context.Background(), ordinary, "permissions/rules.json", 0o600, func(writer io.Writer) error {
		_, writeErr := writer.Write([]byte("unauthorized"))
		return writeErr
	}); err == nil {
		t.Fatal("ordinary capability wrote protected target")
	}
	if err := result.Root.AtomicWrite(context.Background(), protected, "protected-alias.json", 0o600, func(writer io.Writer) error {
		_, writeErr := writer.Write([]byte("alias publish"))
		return writeErr
	}); err == nil {
		t.Fatal("protected capability published through unlisted hard-link location")
	}
	assertFileContent(t, protectedPath, "protected-old")

	if err := result.Root.AtomicWrite(context.Background(), protected, "permissions/rules.json", 0o600, func(writer io.Writer) error {
		_, writeErr := writer.Write([]byte("protected-new"))
		return writeErr
	}); err != nil {
		t.Fatal("protected atomic write failed")
	}
	assertFileContent(t, protectedPath, "protected-new")

	if err := result.Root.AtomicWrite(context.Background(), ordinary, "target.txt", 0o600, func(writer io.Writer) error {
		if _, writeErr := writer.Write([]byte("candidate")); writeErr != nil {
			return writeErr
		}
		external := filepath.Join(rootPath, "external-replacement.txt")
		if writeErr := os.WriteFile(external, []byte("external"), 0o600); writeErr != nil {
			return writeErr
		}
		return os.Rename(external, targetPath)
	}); err == nil {
		t.Fatal("atomic write ignored a target identity replacement")
	}
	assertFileContent(t, targetPath, "external")
	assertNoStagingFiles(t, rootPath)

	nestedOld := filepath.Join(rootPath, "nested-old")
	if err := result.Root.AtomicWrite(context.Background(), ordinary, "nested/target.txt", 0o600, func(writer io.Writer) error {
		if _, writeErr := writer.Write([]byte("candidate")); writeErr != nil {
			return writeErr
		}
		if renameErr := os.Rename(nested, nestedOld); renameErr != nil {
			return renameErr
		}
		if mkdirErr := os.Mkdir(nested, 0o700); mkdirErr != nil {
			return mkdirErr
		}
		return os.WriteFile(filepath.Join(nested, "target.txt"), []byte("replacement-parent"), 0o600)
	}); err == nil {
		t.Fatal("atomic write ignored replacement of its opened parent")
	}
	assertFileContent(t, filepath.Join(nestedOld, "target.txt"), "nested-old")
	assertFileContent(t, filepath.Join(nested, "target.txt"), "replacement-parent")
	assertNoStagingFiles(t, nestedOld)
}

func assertFileContent(t *testing.T, filePath, expected string) {
	t.Helper()
	data, err := os.ReadFile(filePath)
	if err != nil || string(data) != expected {
		t.Fatal("file content did not preserve the expected version")
	}
}

func assertNoStagingFiles(t *testing.T, rootPath string) {
	t.Helper()
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal("read root after atomic write failed")
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(".xagent-stage-") && entry.Name()[:len(".xagent-stage-")] == ".xagent-stage-" {
			t.Fatal("atomic write left a staging file behind")
		}
	}
}
