//go:build windows

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

func TestWindowsRootRejectsReparseEscape(t *testing.T) {
	rootPath := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal("create outside file failed")
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "escape")); err != nil {
		t.Fatal("create required reparse fixture failed")
	}
	result := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, result.Root)
	if _, err := result.Root.Bind("escape/secret.txt"); err == nil {
		t.Fatal("Bind followed a reparse point outside the root")
	}
	if _, err := result.Root.OpenRead(context.Background(), "escape/secret.txt"); err == nil {
		t.Fatal("OpenRead followed a reparse point outside the root")
	}
}

func TestWindowsBindingIgnoresCaseAlias(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "permissions"), 0o700); err != nil {
		t.Fatal("create protected parent failed")
	}
	if err := os.WriteFile(filepath.Join(rootPath, "permissions", "rules.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal("create protected target failed")
	}
	result := mustBootstrap(t, rootPath, Policy{ProtectedSlots: []string{"permissions/rules.json", "permissions/new.json"}})
	defer mustClose(t, result.Root)

	exact := mustBind(t, result.Root, "permissions/rules.json")
	alias := mustBind(t, result.Root, "PERMISSIONS/RULES.JSON")
	if !bytes.Equal(mustBindingBytes(t, exact), mustBindingBytes(t, alias)) {
		t.Fatal("existing case alias changed canonical binding")
	}
	missing := mustBind(t, result.Root, "permissions/new.json")
	missingAlias := mustBind(t, result.Root, "PERMISSIONS/NEW.JSON")
	if !bytes.Equal(mustBindingBytes(t, missing), mustBindingBytes(t, missingAlias)) {
		t.Fatal("missing case alias changed canonical parent-and-name binding")
	}
	if err := result.Root.authorizeWrite(result.Capabilities.Ordinary(), alias); err == nil {
		t.Fatal("ordinary capability authorized protected case alias")
	}
}

func TestWindowsAtomicWritePreservesPreviousVersionOnFailure(t *testing.T) {
	rootPath := t.TempDir()
	targetPath := filepath.Join(rootPath, "target.txt")
	if err := os.WriteFile(targetPath, []byte("previous"), 0o600); err != nil {
		t.Fatal("create prior Windows version failed")
	}
	if err := os.Mkdir(filepath.Join(rootPath, "permissions"), 0o700); err != nil {
		t.Fatal("create Windows protected parent failed")
	}
	protectedPath := filepath.Join(rootPath, "permissions", "rules.json")
	if err := os.WriteFile(protectedPath, []byte("protected-old"), 0o600); err != nil {
		t.Fatal("create Windows protected version failed")
	}
	result := mustBootstrap(t, rootPath, Policy{ProtectedSlots: []string{"permissions/rules.json"}})
	defer mustClose(t, result.Root)

	callbackFailure := errors.New("fixture write failure")
	err := result.Root.AtomicWrite(context.Background(), result.Capabilities.Ordinary(), "target.txt", 0o600, func(writer io.Writer) error {
		if _, writeErr := writer.Write([]byte("partial-new")); writeErr != nil {
			return writeErr
		}
		return callbackFailure
	})
	if err == nil {
		t.Fatal("Windows writer callback failure was reported as success")
	}
	assertFileContent(t, targetPath, "previous")
	assertNoStagingFiles(t, rootPath)

	if err := result.Root.AtomicWrite(context.Background(), result.Capabilities.Ordinary(), "permissions/rules.json", 0o600, func(writer io.Writer) error {
		_, writeErr := writer.Write([]byte("unauthorized"))
		return writeErr
	}); err == nil {
		t.Fatal("ordinary Windows capability wrote a protected target")
	}
	assertFileContent(t, protectedPath, "protected-old")

	if err := result.Root.AtomicWrite(context.Background(), result.Capabilities.Protected(), "permissions/rules.json", 0o600, func(writer io.Writer) error {
		_, writeErr := writer.Write([]byte("protected-new"))
		return writeErr
	}); err != nil {
		t.Fatal("protected Windows atomic write failed")
	}
	assertFileContent(t, protectedPath, "protected-new")

	if err := result.Root.AtomicWrite(context.Background(), result.Capabilities.Ordinary(), "target.txt", 0o600, func(writer io.Writer) error {
		if _, writeErr := writer.Write([]byte("candidate")); writeErr != nil {
			return writeErr
		}
		if removeErr := os.Remove(targetPath); removeErr != nil {
			return removeErr
		}
		return os.WriteFile(targetPath, []byte("external"), 0o600)
	}); err == nil {
		t.Fatal("Windows atomic write ignored target identity replacement")
	}
	assertFileContent(t, targetPath, "external")
	assertNoStagingFiles(t, rootPath)
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
