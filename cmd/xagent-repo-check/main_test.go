package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/repoaudit"
)

func TestAuditModesAndExitCodes(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	writeAuditPolicy(t, repo)
	const canary = "audit-output-canary-83476125"
	var mu sync.Mutex
	var kinds []repoaudit.SourceKind
	deps := defaultCommandDeps()
	deps.getwd = func() (string, error) { return repo, nil }
	deps.source = func(_ string, kind repoaudit.SourceKind, _ string) repoaudit.Source {
		mu.Lock()
		kinds = append(kinds, kind)
		mu.Unlock()
		return &commandMemorySource{
			entries: []repoaudit.Entry{{Path: "safe.txt", Mode: 0100644, Type: "blob"}},
			content: map[string][]byte{"safe.txt": []byte("ordinary\n")},
		}
	}
	for _, args := range [][]string{
		nil,
		{"--source=worktree"},
		{"--source=commit", "--revision=0123456789abcdef0123456789abcdef01234567"},
	} {
		var output bytes.Buffer
		code, err := runCommand(context.Background(), args, &output, deps)
		if err != nil || code != exitPass || !strings.Contains(output.String(), "result=pass") {
			t.Fatalf("audit %v: code=%d err=%v output=%q", args, code, err, output.String())
		}
	}
	mu.Lock()
	gotKinds := append([]repoaudit.SourceKind(nil), kinds...)
	mu.Unlock()
	wantKinds := []repoaudit.SourceKind{repoaudit.SourceIndex, repoaudit.SourceWorktree, repoaudit.SourceCommit}
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("audit kinds = %v", gotKinds)
	}
	for index := range wantKinds {
		if gotKinds[index] != wantKinds[index] {
			t.Fatalf("audit kinds = %v, want %v", gotKinds, wantKinds)
		}
	}

	deps.source = func(string, repoaudit.SourceKind, string) repoaudit.Source {
		return &commandMemorySource{
			entries: []repoaudit.Entry{{Path: "secret.txt", Mode: 0100644, Type: "blob"}},
			content: map[string][]byte{"secret.txt": []byte("OPENAI_API_KEY=" + canary + "\n")},
		}
	}
	var output bytes.Buffer
	code, err := runCommand(context.Background(), nil, &output, deps)
	if err != nil || code != exitFindings || !strings.Contains(output.String(), "result=findings") || strings.Contains(output.String(), canary) {
		t.Fatalf("finding audit: code=%d err=%v output=%q", code, err, output.String())
	}
	deps.source = func(string, repoaudit.SourceKind, string) repoaudit.Source {
		return &commandMemorySource{entriesErr: errors.New("sensitive source failure")}
	}
	output.Reset()
	code, err = runCommand(context.Background(), nil, &output, deps)
	if err == nil || code != exitTool || output.Len() != 0 || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("tool-error audit: code=%d err=%v output=%q", code, err, output.String())
	}
}

func TestPreservationActionsHaveNoPathSelector(t *testing.T) {
	t.Parallel()

	for _, selector := range []string{"--path=private", "--manifest=elsewhere", "--glob=*"} {
		code, err := runCommand(context.Background(), []string{"--preservation=freeze", selector}, io.Discard, commandDeps{})
		if err == nil || code != exitTool {
			t.Fatalf("selector %q was accepted: code=%d err=%v", selector, code, err)
		}
	}
	deps := commandDeps{getwd: func() (string, error) { return t.TempDir(), nil }}
	code, err := runCommand(context.Background(), []string{"--preservation=delete"}, io.Discard, deps)
	if err == nil || code != exitTool {
		t.Fatalf("unknown preservation action was accepted: code=%d err=%v", code, err)
	}
}

func TestPreservationFreezeRecoveryRequiresUnchangedPreIndexState(t *testing.T) {
	t.Parallel()

	harness := newPreservationHarness(t, "fakeprovider", []byte("private fixture"), 0100644)
	if _, err := runCommand(context.Background(), []string{"--preservation=freeze"}, io.Discard, harness.deps); err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(harness.evidenceDir, "preservation", "m0.manifest")
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(filepath.Dir(finalPath), ".m0.manifest.publish.tmp")
	recoveryOwner, err := repoaudit.OpenEvidenceOwner(harness.config)
	if err != nil {
		t.Fatal(err)
	}
	const recoverySeed = "preservation/recovery-seed"
	if err := recoveryOwner.Publish(recoverySeed, data[:len(data)/2]); err != nil {
		recoveryOwner.Close()
		t.Fatal(err)
	}
	if err := recoveryOwner.Close(); err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(harness.evidenceDir, filepath.FromSlash(recoverySeed))
	if err := os.Rename(seedPath, tempPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(context.Background(), []string{"--preservation=freeze"}, io.Discard, harness.deps); err != nil {
		t.Fatalf("unchanged pre-index did not recover truncated temp: %v", err)
	}

	harness.mu.Lock()
	harness.entries[0].OID = testOID("changed index identity")
	harness.mu.Unlock()
	if _, err := runCommand(context.Background(), []string{"--preservation=freeze"}, io.Discard, harness.deps); err == nil {
		t.Fatal("changed pre-index state was accepted during freeze recovery")
	}
}

func TestPreservationRemoveIndexHoldsEvidenceLease(t *testing.T) {
	t.Parallel()

	harness := newPreservationHarness(t, "fakeprovider", []byte("private fixture"), 0100644)
	if _, err := runCommand(context.Background(), []string{"--preservation=freeze"}, io.Discard, harness.deps); err != nil {
		t.Fatal(err)
	}
	entered := make(chan *repoaudit.EvidenceOwner, 1)
	openErrors := make(chan error, 1)
	harness.deps.mutateIndex = func(context.Context, string, []repoaudit.Entry) error {
		go func() {
			owner, err := repoaudit.OpenEvidenceOwner(harness.config)
			if err != nil {
				openErrors <- err
				return
			}
			entered <- owner
		}()
		select {
		case owner := <-entered:
			owner.Close()
			return errors.New("second owner entered during index mutation")
		case err := <-openErrors:
			return err
		case <-time.After(150 * time.Millisecond):
		}
		harness.mu.Lock()
		harness.entries = nil
		harness.mu.Unlock()
		return nil
	}
	if _, err := runCommand(context.Background(), []string{"--preservation=remove-index"}, io.Discard, harness.deps); err != nil {
		t.Fatal(err)
	}
	select {
	case owner := <-entered:
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	case err := <-openErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("second owner did not enter after remove-index released the lease")
	}
}

func TestPreservationActionsRecoverAndKeepManifest(t *testing.T) {
	t.Parallel()

	harness := newPreservationHarness(t, "fakeprovider", []byte("private fixture"), 0100644)
	if _, err := runCommand(context.Background(), []string{"--preservation=freeze"}, io.Discard, harness.deps); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(harness.evidenceDir, "preservation", "m0.manifest")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	assertManifest := func(stage string) {
		t.Helper()
		current, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatalf("%s removed preservation manifest: %v", stage, err)
		}
		if !bytes.Equal(current, manifestBytes) {
			t.Fatalf("%s changed preservation manifest", stage)
		}
	}

	harness.deps.mutateIndex = func(context.Context, string, []repoaudit.Entry) error {
		harness.mu.Lock()
		harness.entries = nil
		harness.mu.Unlock()
		return errors.New("simulated crash after index mutation")
	}
	if _, err := runCommand(context.Background(), []string{"--preservation=remove-index"}, io.Discard, harness.deps); err == nil {
		t.Fatal("remove-index crash injection unexpectedly succeeded")
	}
	assertManifest("failed remove-index")

	harness.deps.mutateIndex = func(context.Context, string, []repoaudit.Entry) error {
		return errors.New("recovered remove-index repeated mutation")
	}
	if _, err := runCommand(context.Background(), []string{"--preservation=remove-index"}, io.Discard, harness.deps); err != nil {
		t.Fatalf("remove-index did not recover exact post-index state: %v", err)
	}
	assertManifest("recovered remove-index")
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := runCommand(context.Background(), []string{"--preservation=verify-immediate"}, io.Discard, harness.deps); err != nil {
			t.Fatalf("verify-immediate attempt %d: %v", attempt+1, err)
		}
		assertManifest("verify-immediate")
	}

	originalGetenv := harness.deps.getenv
	harness.deps.getenv = func(key string) string {
		if key == "XAGENT_PRESERVATION_PHASE" {
			return "t5.53a"
		}
		return originalGetenv(key)
	}
	if _, err := runCommand(context.Background(), []string{"--preservation=verify-final"}, io.Discard, harness.deps); err != nil {
		t.Fatal(err)
	}
	assertManifest("verify-final")
}

func TestPreservationVerifyNeverFollowsLinks(t *testing.T) {
	t.Parallel()

	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("first external content"), 0o600); err != nil {
		t.Fatal(err)
	}
	harness := newPreservationHarness(t, "fakeprovider", nil, 0120000)
	if err := os.Remove(filepath.Join(harness.repo, "fakeprovider")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(harness.repo, "fakeprovider")); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(context.Background(), []string{"--preservation=freeze"}, io.Discard, harness.deps); err != nil {
		t.Fatal(err)
	}
	harness.mu.Lock()
	harness.entries = nil
	harness.mu.Unlock()
	if err := os.WriteFile(external, []byte("changed external content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(context.Background(), []string{"--preservation=verify-immediate"}, io.Discard, harness.deps); err != nil {
		t.Fatalf("external symlink target content affected preservation verification: %v", err)
	}
}

type preservationHarness struct {
	repo        string
	evidenceDir string
	config      repoaudit.EvidenceConfig
	deps        commandDeps
	mu          sync.Mutex
	entries     []repoaudit.Entry
}

func newPreservationHarness(t *testing.T, name string, content []byte, mode uint32) *preservationHarness {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	task := filepath.Join(base, "task")
	control := filepath.Join(base, "control")
	for _, directory := range []string{repo, task} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, name), content, 0o600); err != nil {
		t.Fatal(err)
	}
	entry := repoaudit.Entry{Path: name, Mode: mode, OID: testOID(name), Stage: 0, Type: "blob"}
	harness := &preservationHarness{repo: repo, evidenceDir: filepath.Join(control, "evidence"), entries: []repoaudit.Entry{entry}}
	harness.config = repoaudit.EvidenceConfig{ControlRoot: control, EvidenceDir: harness.evidenceDir, TaskDir: task, Workspace: repo, SourceRoot: task}
	harness.deps = defaultCommandDeps()
	harness.deps.getwd = func() (string, error) { return repo, nil }
	harness.deps.getenv = func(key string) string {
		switch key {
		case "XAGENT_PRESERVATION_PHASE":
			return "t0.10"
		case "XAGENT_EVIDENCE_DIR":
			return harness.evidenceDir
		case "XAGENT_TASK_TMP":
			return task
		default:
			return ""
		}
	}
	harness.deps.indexEntries = func(context.Context, string) ([]repoaudit.Entry, error) {
		harness.mu.Lock()
		defer harness.mu.Unlock()
		return append([]repoaudit.Entry(nil), harness.entries...), nil
	}
	expectedDigest, err := repoaudit.PreservationSelectionDigest(harness.entries)
	if err != nil {
		t.Fatal(err)
	}
	harness.deps.validateSelection = func(entries []repoaudit.Entry) (string, error) {
		digest, err := repoaudit.PreservationSelectionDigest(entries)
		if err != nil || len(entries) != 1 || digest != expectedDigest {
			return "", errors.New("synthetic preservation selection changed")
		}
		return digest, nil
	}
	harness.deps.mutateIndex = func(context.Context, string, []repoaudit.Entry) error {
		harness.mu.Lock()
		harness.entries = nil
		harness.mu.Unlock()
		return nil
	}
	harness.deps.syncIndex = func(string) error { return nil }
	return harness
}

func writeAuditPolicy(t *testing.T, repo string) {
	t.Helper()
	data := []byte("version: 1\nmax_binary_bytes: 1048576\nbinary_allowlist: []\ngitlinks: []\nfixture_exemptions: []\n")
	if err := os.WriteFile(filepath.Join(repo, ".repoaudit.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testOID(seed string) string {
	digest := sha1.Sum([]byte(seed))
	return hex.EncodeToString(digest[:])
}

type commandMemorySource struct {
	entries    []repoaudit.Entry
	content    map[string][]byte
	entriesErr error
}

func (s *commandMemorySource) Entries(context.Context) ([]repoaudit.Entry, error) {
	if s.entriesErr != nil {
		return nil, s.entriesErr
	}
	return append([]repoaudit.Entry(nil), s.entries...), nil
}

func (s *commandMemorySource) Open(_ context.Context, entry repoaudit.Entry) (io.ReadCloser, error) {
	content, ok := s.content[entry.Path]
	if !ok {
		return nil, errors.New("missing content")
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}
