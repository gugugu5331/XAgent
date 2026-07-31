//go:build windows

package safefs

import (
	"bytes"
	"context"
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
