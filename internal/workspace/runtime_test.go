package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xagent/internal/safefs"
	"xagent/internal/worktree"
)

func TestRuntimeTracksCallsStopsAdmissionAndClosesInReverse(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	var mu sync.Mutex
	closed := make([]string, 0, 2)
	first := Resource{Name: "first", Close: func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, "first")
		return nil
	}}
	second := Resource{Name: "second", Close: func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, "second")
		return nil
	}}
	runtime, err := NewRuntime(context.Background(), RuntimeOptions{
		Lease:      lease,
		Protection: protection,
		Resources:  []Resource{first, second},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	callCtx, release, err := runtime.BeginCall(context.Background())
	if err != nil {
		t.Fatalf("BeginCall() error = %v", err)
	}
	runtime.StopAccepting()
	select {
	case <-callCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("StopAccepting did not cancel an in-flight call")
	}
	if _, deniedRelease, beginErr := runtime.BeginCall(context.Background()); beginErr == nil {
		deniedRelease()
		t.Fatal("BeginCall() succeeded after StopAccepting")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if closeErr := runtime.Close(closeCtx); !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("Close() while call is active = %v, want deadline", closeErr)
	}
	if runtime.RootHandle().Identity() == (safefs.Identity{}) {
		t.Fatal("root closed before the final in-flight call was released")
	}
	release()
	release()
	if closeErr := runtime.Close(context.Background()); closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	if closeErr := runtime.Close(context.Background()); closeErr != nil {
		t.Fatalf("repeated Close() error = %v", closeErr)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"second", "first"}
	if len(closed) != len(want) || closed[0] != want[0] || closed[1] != want[1] {
		t.Fatalf("close order = %v, want %v", closed, want)
	}
	if runtime.RootHandle().Identity() != (safefs.Identity{}) {
		t.Fatal("root remained open after runtime close")
	}
}

func TestRuntimeCloseReturnsStableResourceFailureAndClosesProtection(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	wantErr := errors.New("close failed")
	runtime, err := NewRuntime(context.Background(), RuntimeOptions{
		Lease:      lease,
		Protection: protection,
		Resources: []Resource{{Name: "failing", Close: func(context.Context) error {
			return wantErr
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := runtime.Close(context.Background())
	second := runtime.Close(context.Background())
	if first == nil || first.Error() != second.Error() {
		t.Fatalf("idempotent close errors = (%v, %v)", first, second)
	}
	if protection.Working().Root.Identity() != (safefs.Identity{}) {
		t.Fatal("protection root remained open after resource failure")
	}
	if err := protection.Verify(); err == nil {
		t.Fatal("protection remained live after runtime close")
	}
}

func TestRuntimeCompletedCloseResultWinsOverLaterCallerCancellation(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	runtime, err := NewRuntime(context.Background(), RuntimeOptions{Lease: lease, Protection: protection})
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := runtime.Close(context.Background()); closeErr != nil {
		t.Fatalf("initial Close() error = %v", closeErr)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if closeErr := runtime.Close(canceled); closeErr != nil {
		t.Fatalf("completed Close() with canceled caller = %v, want stable nil", closeErr)
	}
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	if closeErr := runtime.Close(expired); closeErr != nil {
		t.Fatalf("completed Close() with expired caller = %v, want stable nil", closeErr)
	}
}

func TestRuntimeRejectsInvalidLeaseAndDoesNotChangeProcessCWD(t *testing.T) {
	t.Parallel()
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	protection, lease := openRuntimeFixture(t)
	lease.Root = filepath.Join(filepath.Dir(lease.Root), "other")
	if runtime, newErr := NewRuntime(context.Background(), RuntimeOptions{Lease: lease, Protection: protection}); newErr == nil {
		_ = runtime.Close(context.Background())
		t.Fatal("NewRuntime() accepted a lease for another root")
	}
	_ = protection.Close()
	after, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("process cwd changed from %q to %q", before, after)
	}
}

func TestRuntimeRequiresExactWorktreeLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*worktree.Lease)
	}{
		{name: "zero lease", mutate: func(lease *worktree.Lease) { *lease = worktree.Lease{} }},
		{name: "wrong branch", mutate: func(lease *worktree.Lease) { lease.Branch = "xagent/worktree/other" }},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			protection, lease := openRuntimeFixture(t)
			testCase.mutate(&lease)
			if runtime, err := NewRuntime(context.Background(), RuntimeOptions{Lease: lease, Protection: protection}); err == nil {
				_ = runtime.Close(context.Background())
				t.Fatal("NewRuntime() accepted an invalid worktree lease")
			}
		})
	}
}

func TestRuntimeCloseUsesIndependentOwnerContextForResources(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	receivedDeadline := make(chan bool, 1)
	runtime, err := NewRuntime(context.Background(), RuntimeOptions{
		Lease:      lease,
		Protection: protection,
		Resources: []Resource{{Name: "bounded", Close: func(ctx context.Context) error {
			_, ok := ctx.Deadline()
			receivedDeadline <- ok
			return nil
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	if got := <-receivedDeadline; got {
		t.Fatal("caller deadline leaked into owner resource cleanup")
	}
}

func TestRuntimeCloseCallerDeadlineDoesNotCancelOwnerCleanupOrCloseRootsEarly(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	runtime, err := NewRuntime(context.Background(), RuntimeOptions{
		Lease: lease, Protection: protection,
		Resources: []Resource{{Name: "background-owner", Close: func(ctx context.Context) error {
			close(entered)
			if _, leaked := ctx.Deadline(); leaked {
				return errors.New("caller deadline leaked into owner cleanup")
			}
			<-release
			if protection.Working().Root.Identity() == (safefs.Identity{}) {
				return errors.New("protection closed before resource settled")
			}
			return nil
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runtime.Close(short) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("resource cleanup did not start")
	}
	if closeErr := <-result; !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("short Close = %v, want caller deadline", closeErr)
	}
	if protection.Working().Root.Identity() == (safefs.Identity{}) {
		t.Fatal("Protection closed while the resource was still live")
	}
	close(release)
	if closeErr := runtime.Close(context.Background()); closeErr != nil {
		t.Fatalf("second Close after resource settlement = %v", closeErr)
	}
	if protection.Working().Root.Identity() != (safefs.Identity{}) {
		t.Fatal("Protection remained open after owner cleanup completed")
	}
}

func TestRuntimeCloseCallerDeadlineBoundsBlockingResourceStop(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	stopEntered := make(chan struct{})
	stopRelease := make(chan struct{})
	closed := make(chan struct{})
	runtime, err := NewRuntime(context.Background(), RuntimeOptions{
		Lease: lease, Protection: protection,
		Resources: []Resource{{
			Name: "blocking-stop",
			Stop: func() {
				close(stopEntered)
				<-stopRelease
			},
			Close: func(context.Context) error {
				close(closed)
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runtime.Close(short) }()
	select {
	case <-stopEntered:
	case <-time.After(time.Second):
		t.Fatal("resource Stop did not start")
	}
	select {
	case closeErr := <-result:
		if !errors.Is(closeErr, context.DeadlineExceeded) {
			t.Fatalf("short Close = %v, want deadline", closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking Resource.Stop escaped caller deadline")
	}
	if protection.Working().Root.Identity() == (safefs.Identity{}) {
		t.Fatal("Protection closed while Resource.Stop was blocked")
	}
	select {
	case <-closed:
		t.Fatal("Resource.Close ran before Resource.Stop settled")
	default:
	}
	close(stopRelease)
	if closeErr := runtime.Close(context.Background()); closeErr != nil {
		t.Fatalf("owner cleanup did not settle: %v", closeErr)
	}
	select {
	case <-closed:
	default:
		t.Fatal("Resource.Close did not run after Stop")
	}
}

func TestRuntimeBeginCallFailsWhenRootIdentityDrifts(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	runtime, err := NewRuntime(context.Background(), RuntimeOptions{Lease: lease, Protection: protection})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())

	moved := protection.Working().Path + "-old"
	if err := os.Rename(protection.Working().Path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(protection.Working().Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, release, beginErr := runtime.BeginCall(context.Background()); beginErr == nil {
		release()
		t.Fatal("BeginCall() accepted a replaced task root")
	}
}

func TestRuntimeParentCancellationStopsAdmission(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	runtime, err := NewRuntime(parent, RuntimeOptions{Lease: lease, Protection: protection})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, release, beginErr := runtime.BeginCall(context.Background()); beginErr == nil {
		release()
		t.Fatal("BeginCall() accepted work after the runtime parent was canceled")
	}
	if closeErr := runtime.Close(context.Background()); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestRuntimeConstructionFailureRollsBackOwnedProtection(t *testing.T) {
	t.Parallel()
	protection, lease := openRuntimeFixture(t)
	lease.OwnerID = "invalid"
	if runtime, err := NewRuntime(context.Background(), RuntimeOptions{Lease: lease, Protection: protection}); err == nil {
		_ = runtime.Close(context.Background())
		t.Fatal("NewRuntime() accepted invalid ownership")
	}
	if protection.Working().Root.Identity() != (safefs.Identity{}) {
		t.Fatal("constructor failure leaked its protection roots")
	}
}

func openRuntimeFixture(t *testing.T) (*Protection, worktree.Lease) {
	t.Helper()
	base := t.TempDir()
	working := makeDirectory(t, base, "worktree")
	if err := os.WriteFile(filepath.Join(working, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	protection, err := OpenProtection(ProtectionPaths{
		Working:  working,
		Scratch:  makeDirectory(t, base, "scratch"),
		Artifact: makeDirectory(t, base, "artifact"),
		Readonly: []string{makeDirectory(t, base, "main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return protection, worktree.Lease{
		WorkspaceID: "0123456789abcdef0123456789abcdef",
		OwnerID:     "fedcba9876543210fedcba9876543210",
		Root:        working,
		Branch:      "xagent/worktree/0123456789abcdef0123456789abcdef",
		BaseOID:     "0123456789abcdef0123456789abcdef01234567",
		AcquiredAt:  time.Now(),
	}
}
