//go:build darwin || linux

package safefs

import (
	"bytes"
	"context"
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
