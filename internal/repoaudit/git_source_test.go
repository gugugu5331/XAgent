package repoaudit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitSourceHandlesWhitespaceAndNewlines(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	runGitFixture(t, repo, "init", "-q")
	runGitFixture(t, repo, "config", "user.email", "repoaudit@example.invalid")
	runGitFixture(t, repo, "config", "user.name", "Repo Audit")
	paths := []string{"space name.txt", "line\nbreak.txt"}
	for _, name := range paths {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("safe fixture\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", name, err)
		}
	}
	runGitFixture(t, repo, "add", "--", paths[0], paths[1])

	indexEntries, err := NewIndexSource(repo).Entries(context.Background())
	if err != nil {
		t.Fatalf("index Entries: %v", err)
	}
	assertEntryPaths(t, indexEntries, paths)

	runGitFixture(t, repo, "commit", "-qm", "fixture")
	revision := runGitFixture(t, repo, "rev-parse", "HEAD")
	commitEntries, err := NewCommitSource(repo, revision).Entries(context.Background())
	if err != nil {
		t.Fatalf("commit Entries: %v", err)
	}
	assertEntryPaths(t, commitEntries, paths)

	worktreeEntries, err := NewWorktreeSource(repo).Entries(context.Background())
	if err != nil {
		t.Fatalf("worktree Entries: %v", err)
	}
	assertEntryPaths(t, worktreeEntries, paths)
}

func assertEntryPaths(t *testing.T, entries []Entry, want []string) {
	t.Helper()
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		seen[entry.Path] = true
	}
	for _, path := range want {
		if !seen[path] {
			t.Errorf("entry path %q missing from %#v", path, entries)
		}
	}
}

func runGitFixture(t *testing.T, repo string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	output, err := exec.Command("git", commandArgs...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	for len(output) > 0 && (output[len(output)-1] == '\n' || output[len(output)-1] == '\r') {
		output = output[:len(output)-1]
	}
	return string(output)
}
