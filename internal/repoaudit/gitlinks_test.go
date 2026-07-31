package repoaudit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUndeclaredGitlinkIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	modulesPath := filepath.Join(root, ".gitmodules")
	entries := []Entry{{Path: ".claude/worktrees/agent-a", Mode: 0160000, Type: "commit"}}
	findings, err := gitlinkFindings(entries, nil, false, Policy{Version: PolicyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].RuleID != RuleGitlink || findings[0].Path != entries[0].Path || findings[0].Message != gitlinkFindingMessage {
		t.Fatalf("undeclared gitlink findings = %#v", findings)
	}
	if _, err := os.Lstat(modulesPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gitlink audit created .gitmodules: %v", err)
	}

	policy := Policy{Version: PolicyVersion, Gitlinks: []GitlinkDeclaration{{Path: entries[0].Path}}}
	modules := []byte("[submodule \"agent-a\"]\n\tpath = .claude/worktrees/agent-a\n\turl = https://example.invalid/agent-a.git\n")
	findings, err = gitlinkFindings(entries, modules, true, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("declared and mapped gitlink findings = %#v", findings)
	}
	if _, err := os.Lstat(modulesPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gitlink audit created .gitmodules after positive check: %v", err)
	}
}
