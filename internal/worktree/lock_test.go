package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lockTestIdentity(t *testing.T, root string) RepositoryIdentity {
	t.Helper()
	common := filepath.Join(root, ".git")
	if err := mkdirPrivate(common); err != nil {
		t.Fatal(err)
	}
	return realLockTestIdentity(t, root, common)
}

func realLockTestIdentity(t *testing.T, root, common string) RepositoryIdentity {
	t.Helper()
	identity, err := NewRepositoryIdentity(root, common)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestRepositoryLockCompetesAcrossDifferentControlRoots(t *testing.T) {
	base := t.TempDir()
	repositoryRoot := filepath.Join(base, "repository")
	common := filepath.Join(repositoryRoot, ".git")
	if err := mkdirPrivate(common); err != nil {
		t.Fatal(err)
	}
	identity := realLockTestIdentity(t, repositoryRoot, common)
	first, err := NewFileLockManager(filepath.Join(base, "control-a"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileLockManager(filepath.Join(base, "control-b"), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	firstUnlock, err := first.LockRepository(NewLockContext(context.Background()), identity)
	if err != nil {
		t.Fatal(err)
	}
	defer firstUnlock()
	entries, err := os.ReadDir(common)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("仓库锁不得向 Git common-dir 写文件：%v", entries)
	}
	sharedLock := filepath.Join(repositoryRoot, ".xagent", "worktrees", ".control", "locks", "repositories", identity.Digest+".lock")
	if info, err := os.Lstat(sharedLock); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("仓库锁必须锚定在共享受管控制域：info=%v err=%v", info, err)
	}
	if _, err := second.LockRepository(NewLockContext(context.Background()), identity); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("同一 common-dir 的不同 controlRoot 必须竞争同一 OS 锁：%v", err)
	}
}

func TestRepositoryLockAllowsManagedWorktreeRoot(t *testing.T) {
	base := t.TempDir()
	mainRoot := filepath.Join(base, "repository")
	common := filepath.Join(mainRoot, ".git")
	workspaceID := "0123456789abcdef0123456789abcdef"
	managedRoot := filepath.Join(mainRoot, ".xagent", "worktrees", "tasks", workspaceID[:2], workspaceID)
	for _, directory := range []string{common, managedRoot} {
		if err := mkdirPrivate(directory); err != nil {
			t.Fatal(err)
		}
	}
	mainIdentity := realLockTestIdentity(t, mainRoot, common)
	managedIdentity := realLockTestIdentity(t, managedRoot, common)
	if !mainIdentity.Equal(managedIdentity) {
		t.Fatalf("主根与受管 Worktree 必须共享仓库对象身份：main=%#v managed=%#v", mainIdentity, managedIdentity)
	}
	first, err := NewFileLockManager(filepath.Join(base, "control-a"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileLockManager(filepath.Join(base, "control-b"), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	firstUnlock, err := first.LockRepository(NewLockContext(context.Background()), mainIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer firstUnlock()
	if _, err := second.LockRepository(NewLockContext(context.Background()), managedIdentity); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("主根与受管 Worktree 必须竞争同一仓库锁：%v", err)
	}
	managedAlias := filepath.Join(mainRoot, ".XAGENT", "WORKTREES", "TASKS", workspaceID[:2], workspaceID)
	managedInfo, managedErr := os.Stat(managedRoot)
	aliasInfo, aliasErr := os.Stat(managedAlias)
	if managedErr == nil && aliasErr == nil && os.SameFile(managedInfo, aliasInfo) {
		aliasIdentity := realLockTestIdentity(t, managedAlias, common)
		if _, err := second.LockRepository(NewLockContext(context.Background()), aliasIdentity); !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("文件系统等价的受管根大小写别名必须竞争同一仓库锁：%v", err)
		}
	}
}

func TestRepositoryLockRejectsExternalCommonDirectoryWithoutSideEffects(t *testing.T) {
	for _, test := range []struct {
		name       string
		commonBase string
	}{
		{name: "non-dot-git basename", commonBase: "gitmeta"},
		{name: "dot-git outside repository", commonBase: ".git"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			repositoryRoot := filepath.Join(base, "repository")
			externalRoot := filepath.Join(base, "outside")
			common := filepath.Join(externalRoot, test.commonBase)
			for _, directory := range []string{repositoryRoot, common} {
				if err := mkdirPrivate(directory); err != nil {
					t.Fatal(err)
				}
			}
			identity := realLockTestIdentity(t, repositoryRoot, common)
			manager, err := NewFileLockManager(filepath.Join(base, "control"), 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.LockRepository(NewLockContext(context.Background()), identity); !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("仓库外 common-dir 必须失败关闭：%v", err)
			}
			if _, err := os.Lstat(filepath.Join(externalRoot, ".xagent")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("拒绝外置 common-dir 时不得在仓库外创建 .xagent：%v", err)
			}
		})
	}
}

func TestRepositoryLockDoesNotBlockDifferentRepositories(t *testing.T) {
	base := t.TempDir()
	firstRoot := filepath.Join(base, "first")
	secondRoot := filepath.Join(base, "second")
	firstCommon := filepath.Join(firstRoot, ".git")
	secondCommon := filepath.Join(secondRoot, ".git")
	for _, directory := range []string{firstCommon, secondCommon} {
		if err := mkdirPrivate(directory); err != nil {
			t.Fatal(err)
		}
	}
	first, err := NewFileLockManager(filepath.Join(base, "control-a"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileLockManager(filepath.Join(base, "control-b"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	firstUnlock, err := first.LockRepository(NewLockContext(context.Background()), realLockTestIdentity(t, firstRoot, firstCommon))
	if err != nil {
		t.Fatal(err)
	}
	defer firstUnlock()
	secondUnlock, err := second.LockRepository(NewLockContext(context.Background()), realLockTestIdentity(t, secondRoot, secondCommon))
	if err != nil {
		t.Fatalf("不同仓库不得被全局串行化：%v", err)
	}
	defer secondUnlock()
}

func TestRepositoryLockRejectsStaleOrForgedIdentity(t *testing.T) {
	base := t.TempDir()
	repositoryRoot := filepath.Join(base, "repository")
	common := filepath.Join(repositoryRoot, ".git")
	if err := mkdirPrivate(common); err != nil {
		t.Fatal(err)
	}
	manager, err := NewFileLockManager(filepath.Join(base, "control"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("forged digest", func(t *testing.T) {
		identity := realLockTestIdentity(t, repositoryRoot, common)
		identity.Digest = strings.Repeat("a", 64)
		if _, err := manager.LockRepository(NewLockContext(context.Background()), identity); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("伪造的 common-dir 对象身份必须失败关闭：%v", err)
		}
	})
	t.Run("replaced object", func(t *testing.T) {
		identity := realLockTestIdentity(t, repositoryRoot, common)
		if err := os.Rename(common, filepath.Join(repositoryRoot, ".git-old")); err != nil {
			t.Fatal(err)
		}
		if err := mkdirPrivate(common); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.LockRepository(NewLockContext(context.Background()), identity); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("过期的 common-dir 对象身份必须失败关闭：%v", err)
		}
	})
}

func TestLockOrderRejectsReverseAcquisition(t *testing.T) {
	manager, err := NewFileLockManager(filepath.Join(t.TempDir(), ".control"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	workspaceID := "0123456789abcdef0123456789abcdef"
	ownerID := "abcdefabcdefabcdefabcdefabcdefab"

	ctx := NewLockContext(context.Background())
	if _, err := manager.LockWorkspace(ctx, workspaceID); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("未持仓库锁时获取 Workspace 锁应失败：%v", err)
	}
	repositoryUnlock, err := manager.LockRepository(ctx, lockTestIdentity(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer repositoryUnlock()
	if _, err := manager.LockRepository(ctx, RepositoryIdentity{}); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("已持仓库锁时必须先按锁序失败且不得解析另一个仓库身份：%v", err)
	}
	if _, err := manager.AcquireActiveLease(ctx, workspaceID, ownerID); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("未持 Workspace 锁时获取 lease 应失败：%v", err)
	}
}

func TestLockContextIsRequiredAndCannotAcquireTwoLeaseModes(t *testing.T) {
	manager, err := NewFileLockManager(filepath.Join(t.TempDir(), ".control"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.LockRepository(context.Background(), lockTestIdentity(t, t.TempDir())); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("缺少任务锁上下文必须失败关闭：%v", err)
	}
	ctx := NewLockContext(context.Background())
	repositoryUnlock, workspaceUnlock, leaseUnlock := acquireTestLease(t, manager, ctx, false)
	defer leaseUnlock()
	defer workspaceUnlock()
	defer repositoryUnlock()
	if _, err := manager.AcquireDeleteLease(ctx, "0123456789abcdef0123456789abcdef", "abcdefabcdefabcdefabcdefabcdefab"); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("同一作用域不能再获取另一种 lease：%v", err)
	}
}

func TestDeleteLeaseTimesOutWhileActiveOSLockIsHeldEvenWithExpiredHeartbeat(t *testing.T) {
	control := filepath.Join(t.TempDir(), ".control")
	manager, err := NewFileLockManager(control, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	activeCtx := NewLockContext(context.Background())
	repositoryUnlock, workspaceUnlock, activeUnlock := acquireTestLease(t, manager, activeCtx, false)
	if err := workspaceUnlock(); err != nil {
		t.Fatal(err)
	}
	if err := repositoryUnlock(); err != nil {
		t.Fatal(err)
	}
	defer activeUnlock()

	heartbeat := filepath.Join(control, "locks", "leases", "0123456789abcdef0123456789abcdef.heartbeat")
	if err := os.WriteFile(heartbeat, []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(heartbeat, old, old); err != nil {
		t.Fatal(err)
	}

	deleteCtx := NewLockContext(context.Background())
	repositoryUnlock, err = manager.LockRepository(deleteCtx, lockTestIdentity(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer repositoryUnlock()
	workspaceUnlock, err = manager.LockWorkspace(deleteCtx, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceUnlock()
	started := time.Now()
	if _, err := manager.AcquireDeleteLease(deleteCtx, "0123456789abcdef0123456789abcdef", "abcdefabcdefabcdefabcdefabcdefab"); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("仍持有 Active OS 锁时 Delete lease 必须超时：%v", err)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed > time.Second {
		t.Fatalf("锁超时不在预期范围：%v", elapsed)
	}
}

func TestLockWaitHonorsContextCancellation(t *testing.T) {
	manager, err := NewFileLockManager(filepath.Join(t.TempDir(), ".control"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	activeCtx := NewLockContext(context.Background())
	repositoryUnlock, workspaceUnlock, activeUnlock := acquireTestLease(t, manager, activeCtx, false)
	_ = workspaceUnlock()
	_ = repositoryUnlock()
	defer activeUnlock()

	ctx, cancel := context.WithCancel(NewLockContext(context.Background()))
	repositoryUnlock, err = manager.LockRepository(ctx, lockTestIdentity(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer repositoryUnlock()
	workspaceUnlock, err = manager.LockWorkspace(ctx, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceUnlock()
	cancel()
	if _, err := manager.AcquireDeleteLease(ctx, "0123456789abcdef0123456789abcdef", "abcdefabcdefabcdefabcdefabcdefab"); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待锁必须响应 context 取消：%v", err)
	}
}

func TestPlatformLockIsAuthoritativeAcrossProcesses(t *testing.T) {
	if os.Getenv("XAGENT_LOCK_HELPER") != "" {
		t.Skip("父测试不在 helper 模式运行")
	}
	control := filepath.Join(t.TempDir(), ".control")
	manager, err := NewFileLockManager(control, 500*time.Millisecond)
	if errors.Is(err, ErrPlatformLockUnsupported) {
		t.Skip("当前平台明确不支持可靠文件锁")
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx := NewLockContext(context.Background())
	repositoryUnlock, workspaceUnlock, activeUnlock := acquireTestLease(t, manager, ctx, false)
	if err := workspaceUnlock(); err != nil {
		t.Fatal(err)
	}
	if err := repositoryUnlock(); err != nil {
		t.Fatal(err)
	}

	runLockHelper(t, control, "active", true)
	runLockHelper(t, control, "delete", true)
	if err := activeUnlock(); err != nil {
		t.Fatal(err)
	}
	runLockHelper(t, control, "delete", false)
}

func TestLockProcessHelper(t *testing.T) {
	mode := os.Getenv("XAGENT_LOCK_HELPER")
	if mode == "" {
		return
	}
	control := os.Getenv("XAGENT_LOCK_CONTROL")
	expectTimeout := os.Getenv("XAGENT_LOCK_EXPECT_TIMEOUT") == "1"
	manager, err := NewFileLockManager(control, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx := NewLockContext(context.Background())
	repositoryUnlock, err := manager.LockRepository(ctx, lockTestIdentity(t, filepath.Dir(control)))
	if err != nil {
		t.Fatal(err)
	}
	workspaceUnlock, err := manager.LockWorkspace(ctx, "0123456789abcdef0123456789abcdef")
	if err != nil {
		_ = repositoryUnlock()
		t.Fatal(err)
	}
	var leaseUnlock Unlock
	if mode == "active" {
		leaseUnlock, err = manager.AcquireActiveLease(ctx, "0123456789abcdef0123456789abcdef", "11111111111111111111111111111111")
	} else {
		leaseUnlock, err = manager.AcquireDeleteLease(ctx, "0123456789abcdef0123456789abcdef", "22222222222222222222222222222222")
	}
	_ = workspaceUnlock()
	_ = repositoryUnlock()
	if expectTimeout {
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("期望超时，实际：%v", err)
		}
		fmt.Print("timeout")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer leaseUnlock()
	fmt.Print("acquired")
}

func acquireTestLease(t *testing.T, manager *FileLockManager, ctx context.Context, deleteMode bool) (Unlock, Unlock, Unlock) {
	t.Helper()
	repositoryUnlock, err := manager.LockRepository(ctx, lockTestIdentity(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	workspaceUnlock, err := manager.LockWorkspace(ctx, "0123456789abcdef0123456789abcdef")
	if err != nil {
		_ = repositoryUnlock()
		t.Fatal(err)
	}
	var leaseUnlock Unlock
	if deleteMode {
		leaseUnlock, err = manager.AcquireDeleteLease(ctx, "0123456789abcdef0123456789abcdef", "abcdefabcdefabcdefabcdefabcdefab")
	} else {
		leaseUnlock, err = manager.AcquireActiveLease(ctx, "0123456789abcdef0123456789abcdef", "abcdefabcdefabcdefabcdefabcdefab")
	}
	if err != nil {
		_ = workspaceUnlock()
		_ = repositoryUnlock()
		t.Fatal(err)
	}
	return repositoryUnlock, workspaceUnlock, leaseUnlock
}

func runLockHelper(t *testing.T, control, mode string, expectTimeout bool) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLockProcessHelper$")
	command.Env = append(os.Environ(), "XAGENT_LOCK_HELPER="+mode, "XAGENT_LOCK_CONTROL="+control)
	if expectTimeout {
		command.Env = append(command.Env, "XAGENT_LOCK_EXPECT_TIMEOUT=1")
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helper 失败：%v (%s)", err, output)
	}
	want := "acquired"
	if expectTimeout {
		want = "timeout"
	}
	if !strings.Contains(string(output), want) {
		t.Fatalf("helper 输出 %q，期望包含 %q", output, want)
	}
}
