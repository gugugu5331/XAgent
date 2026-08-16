package worktree

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryIdentityIsStable(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, ".git-common")
	if err := mkdirPrivate(common); err != nil {
		t.Fatal(err)
	}
	first, err := NewRepositoryIdentity(root, common)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRepositoryIdentity(filepath.Join(root, "."), filepath.Join(common, "."))
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == "" || !first.Equal(second) {
		t.Fatalf("等价路径应产生相同身份：%#v %#v", first, second)
	}
}

func TestRepositoryIdentityUsesCommonDirectoryObject(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	if err := mkdirPrivate(common); err != nil {
		t.Fatal(err)
	}
	before, err := NewRepositoryIdentity(root, common)
	if err != nil {
		t.Fatal(err)
	}
	oldCommon := filepath.Join(root, ".git-old")
	if err := os.Rename(common, oldCommon); err != nil {
		t.Fatal(err)
	}
	if err := mkdirPrivate(common); err != nil {
		t.Fatal(err)
	}
	after, err := NewRepositoryIdentity(root, common)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest == after.Digest || before.Equal(after) {
		t.Fatalf("相同路径上的 common-dir 对象替换必须改变仓库身份：before=%#v after=%#v", before, after)
	}
}

func TestRepositoryIdentityTreatsRootsSharingCommonDirectoryAsSameRepository(t *testing.T) {
	base := t.TempDir()
	firstRoot := filepath.Join(base, "first")
	secondRoot := filepath.Join(base, "second")
	common := filepath.Join(base, ".git")
	for _, directory := range []string{firstRoot, secondRoot, common} {
		if err := mkdirPrivate(directory); err != nil {
			t.Fatal(err)
		}
	}
	first, err := NewRepositoryIdentity(firstRoot, common)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRepositoryIdentity(secondRoot, common)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) || first.Digest != second.Digest {
		t.Fatalf("共享 common-dir 对象的 Worktree 根必须属于同一仓库：first=%#v second=%#v", first, second)
	}
}

func TestRepositoryIdentityHonorsFilesystemCaseEquivalence(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	if err := mkdirPrivate(common); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, ".GIT")
	canonicalInfo, err := os.Stat(common)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(canonicalInfo, aliasInfo) {
		t.Skip("当前文件系统大小写敏感")
	}
	canonical, err := NewRepositoryIdentity(root, common)
	if err != nil {
		t.Fatal(err)
	}
	equivalent, err := NewRepositoryIdentity(root, alias)
	if err != nil {
		t.Fatal(err)
	}
	if !canonical.Equal(equivalent) || canonical.Digest != equivalent.Digest {
		t.Fatalf("文件系统等价的 common-dir 大小写别名必须产生相同身份：canonical=%#v alias=%#v", canonical, equivalent)
	}
}

func TestRepositoryIdentityRejectsMissingOrAliasedRoots(t *testing.T) {
	root := t.TempDir()
	if _, err := NewRepositoryIdentity(filepath.Join(root, "missing"), root); err == nil {
		t.Fatal("缺失根目录必须拒绝")
	}
}

func TestGenerateWorkspaceIDUsesRandomBytes(t *testing.T) {
	id, err := GenerateWorkspaceID(bytes.NewReader(bytes.Repeat([]byte{0xab}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if id != "abababababababababababababababab" || !ValidWorkspaceID(id) {
		t.Fatalf("Workspace ID 非预期：%q", id)
	}
	if ValidWorkspaceID("../escape") || ValidWorkspaceID("ABCDEF0123456789ABCDEF0123456789") {
		t.Fatal("Workspace ID 必须是固定长度小写十六进制")
	}
}

func mkdirPrivate(path string) error { return os.MkdirAll(path, 0o700) }
