package repoaudit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestPreservationManifestIsNULSafeAndNeverFollowsLinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "external-secret")
	if err := os.WriteFile(external, []byte("first external value"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkName := "link\nwith space"
	if err := os.Symlink(external, filepath.Join(root, linkName)); err != nil {
		t.Fatal(err)
	}
	entry := testIndexEntry(linkName, 0120000, "link-object")
	manifest, err := BuildPreservationManifest(root, []Entry{entry})
	if err != nil {
		t.Fatalf("BuildPreservationManifest: %v", err)
	}
	if got := manifest.Entries[0].Fingerprint.Kind; got != "symlink" {
		t.Fatalf("fingerprint kind = %q, want symlink", got)
	}
	manifest.PreIndexSHA256 = hex.EncodeToString(sha256.New().Sum(nil))
	manifest.GitmodulesAbsent = true
	encoded, err := CanonicalPreservationBytes(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(linkName)) {
		t.Fatal("canonical manifest lost newline/space path bytes")
	}
	if bytes.Contains(encoded, []byte("first external value")) {
		t.Fatal("manifest followed symlink and retained external content")
	}
	if err := os.WriteFile(external, []byte("changed external value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPreservationManifest(root, manifest, []Entry{entry}); err != nil {
		t.Fatalf("external target content changed symlink fingerprint: %v", err)
	}
}

func TestPreservationManifestDetectsTypeModeOIDAndContentChange(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	name := "artifact.bin"
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := testIndexEntry(name, 0100644, "original-object")
	manifest, err := BuildPreservationManifest(root, []Entry{entry})
	if err != nil {
		t.Fatal(err)
	}
	manifest.PreIndexSHA256 = hex.EncodeToString(sha256.New().Sum(nil))
	manifest.GitmodulesAbsent = true
	if err := VerifyPreservationManifest(root, manifest, []Entry{entry}); err != nil {
		t.Fatalf("unchanged manifest failed: %v", err)
	}

	changedOID := entry
	changedOID.OID = oidFor("different-object")
	if err := VerifyPreservationManifest(root, manifest, []Entry{changedOID}); err == nil {
		t.Fatal("OID change was not detected")
	}
	changedMode := entry
	changedMode.Mode = 0100755
	if err := VerifyPreservationManifest(root, manifest, []Entry{changedMode}); err == nil {
		t.Fatal("index mode change was not detected")
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPreservationManifest(root, manifest, []Entry{entry}); err == nil {
		t.Fatal("content change was not detected")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", path); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPreservationManifest(root, manifest, []Entry{entry}); err == nil {
		t.Fatal("worktree type change was not detected")
	}
}

func testIndexEntry(path string, mode uint32, seed string) Entry {
	return Entry{Path: path, Mode: mode, OID: oidFor(seed), Stage: 0, Type: gitTypeForMode(mode)}
}

func oidFor(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(digest[:20])
}
