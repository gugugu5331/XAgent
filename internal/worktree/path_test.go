package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateLogicalName(t *testing.T) {
	limits := Limits{MaxNameBytes: 64, MaxSegmentBytes: 24, MaxDepth: 3}
	tests := []struct {
		name  string
		valid bool
	}{
		{name: "review-1", valid: true},
		{name: "team/review_1", valid: true},
		{name: "a.b/c-d", valid: true},
		{name: ""},
		{name: "."},
		{name: ".."},
		{name: "a/../b"},
		{name: "a/./b"},
		{name: "a//b"},
		{name: "/abs"},
		{name: `a\\b`},
		{name: "has space"},
		{name: ".git/x"},
		{name: "x/.LOCK"},
		{name: "a/b/c/d"},
		{name: "中文"},
		{name: "Review-1"},
		{name: "review-"},
		{name: "review_"},
		{name: "review."},
		{name: "review.lock"},
		{name: "team/cache.LoCk"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateLogicalName(test.name, limits)
			if test.valid && err != nil {
				t.Fatalf("合法名称 %q 被拒绝：%v", test.name, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("非法名称 %q 未被拒绝", test.name)
			}
		})
	}
}

func TestValidateLogicalNameLimitsBytes(t *testing.T) {
	if err := ValidateLogicalName("abcd", Limits{MaxNameBytes: 3, MaxSegmentBytes: 8, MaxDepth: 2}); err == nil {
		t.Fatal("总字节上限应生效")
	}
	if err := ValidateLogicalName("abcd", Limits{MaxNameBytes: 8, MaxSegmentBytes: 3, MaxDepth: 2}); err == nil {
		t.Fatal("段字节上限应生效")
	}
}

func FuzzValidateLogicalName(f *testing.F) {
	for _, seed := range []string{
		"review-1", "a/b", "../x", ".git", "a//b", "中文",
		"Review-1", "review-", "review_", "review.", "review.lock", "team/cache.LoCk",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_ = ValidateLogicalName(value, Limits{MaxNameBytes: 128, MaxSegmentBytes: 64, MaxDepth: 8})
	})
}

func TestManagedLayoutIsStableAndContained(t *testing.T) {
	repo := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	layout, err := ResolveManagedLayout(repo, id)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := filepath.Join(canonicalRepo, ".xagent", "worktrees")
	wantWorkspace := filepath.Join(wantRoot, "tasks", "01", id)
	if layout.Root != wantRoot || layout.WorkspaceRoot != wantWorkspace || layout.Branch != "xagent/worktree/"+id {
		t.Fatalf("受管布局错误：%#v", layout)
	}
	if _, err := ValidateManagedPath(layout.Root, filepath.Join(repo, ".xagent", "worktrees-evil"), true); err == nil {
		t.Fatal("相似前缀不能逃逸受管根")
	}
}

func TestValidateManagedPathRejectsSymlinkAndSpecialFile(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, ".xagent", "worktrees")
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "tasks", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateManagedPath(root, filepath.Join(root, "tasks", "link", "child"), true); err == nil {
		t.Fatal("不得跟随符号链接")
	}
	regular := filepath.Join(root, "tasks", "file")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateManagedPath(root, filepath.Join(regular, "child"), true); err == nil {
		t.Fatal("中间路径的普通文件必须拒绝")
	}
}

func TestPathIdentityDetectsReplacement(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := ValidateManagedPath(root, target, false)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "old")
	if err := os.Rename(target, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := identity.Revalidate(); err == nil {
		t.Fatal("路径对象被替换后必须检测失败")
	}
}

func TestExactManagedIgnoreRuleParser(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		valid    bool
	}{
		{name: "exact", contents: "/.xagent/worktrees/\n", valid: true},
		{name: "exact CRLF", contents: "# managed\r\n/.xagent/worktrees/\r\n", valid: true},
		{name: "missing", contents: "node_modules/\n"},
		{name: "broad ancestor", contents: "/.xagent/\n/.xagent/worktrees/\n"},
		{name: "wide glob", contents: "/.xagent/**\n/.xagent/worktrees/\n"},
		{name: "worktrees glob", contents: "/.xagent/worktrees*\n/.xagent/worktrees/\n"},
		{name: "root wildcard", contents: "*\n/.xagent/worktrees/\n"},
		{name: "safe sibling", contents: "/.xagent/cache/\n/.xagent/worktrees/\n", valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasExactManagedIgnoreRule([]byte(test.contents)); got != test.valid {
				t.Fatalf("hasExactManagedIgnoreRule() = %t, want %t", got, test.valid)
			}
		})
	}
}

func TestVerifyExactManagedIgnoreRuleRejectsSymlinkAndOversize(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		repository := t.TempDir()
		external := filepath.Join(t.TempDir(), "ignore")
		if err := os.WriteFile(external, []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(repository, ".gitignore")); err != nil {
			t.Fatal(err)
		}
		if err := verifyExactManagedIgnoreRule(repository); err == nil {
			t.Fatal("symlink .gitignore was accepted")
		}
	})
	t.Run("oversize", func(t *testing.T) {
		repository := t.TempDir()
		contents := make([]byte, maxRootGitIgnoreBytes+1)
		if err := os.WriteFile(filepath.Join(repository, ".gitignore"), contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyExactManagedIgnoreRule(repository); err == nil {
			t.Fatal("oversized .gitignore was accepted")
		}
	})
}
