package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestJanitorFilterUsesOnlyExpiredAuthoritativeCandidates(t *testing.T) {
	makeRetained := func(t *testing.T) (*managerFixture, time.Time) {
		t.Helper()
		fixture := newManagerFixture(t)
		lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
		if err != nil {
			t.Fatal(err)
		}
		fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "kept.txt"}}}
		if _, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
			t.Fatal(err)
		}
		record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
		return fixture, record.Settlement.CompletedAt
	}

	t.Run("expired retained candidate", func(t *testing.T) {
		fixture, terminal := makeRetained(t)
		collector := &fakeJanitorCollector{settlement: Settlement{State: SettlementDeleted}}
		janitor := newTestJanitor(t, fixture, collector, terminal.Add(2*time.Hour))
		result := janitor.ScanOnce(context.Background())
		if result.Candidates < 1 || result.Deleted != 1 || collector.calls != 1 {
			t.Fatalf("expired authoritative candidate not collected: result=%#v calls=%d", result, collector.calls)
		}
	})

	t.Run("fresh retained candidate", func(t *testing.T) {
		fixture, terminal := makeRetained(t)
		collector := &fakeJanitorCollector{}
		janitor := newTestJanitor(t, fixture, collector, terminal.Add(30*time.Minute))
		result := janitor.ScanOnce(context.Background())
		if result.SkippedFresh != 1 || collector.calls != 0 {
			t.Fatalf("fresh candidate reached collector: result=%#v calls=%d", result, collector.calls)
		}
	})

	t.Run("active and ownership-unknown states", func(t *testing.T) {
		fixture := newManagerFixture(t)
		if _, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); err != nil {
			t.Fatal(err)
		}
		collector := &fakeJanitorCollector{}
		janitor := newTestJanitor(t, fixture, collector, time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC))
		result := janitor.ScanOnce(context.Background())
		if result.Deleted != 0 || collector.calls != 0 {
			t.Fatalf("active record became candidate: result=%#v calls=%d", result, collector.calls)
		}
		record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
		record.State = StateManualAttention
		record.Settlement = SettlementRecord{State: SettlementManualAttention, ReasonCode: "ownership_unknown", CompletedAt: record.UpdatedAt}
		fixture.store.mu.Lock()
		fixture.store.records[record.WorkspaceID] = record
		fixture.store.mu.Unlock()
		result = janitor.ScanOnce(context.Background())
		if result.Deleted != 0 || collector.calls != 0 {
			t.Fatalf("manual-attention record became candidate: result=%#v calls=%d", result, collector.calls)
		}
	})

	t.Run("identity mismatch", func(t *testing.T) {
		fixture, terminal := makeRetained(t)
		record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
		record.RepositoryIdentity.Digest = string(make([]byte, 64))
		fixture.store.mu.Lock()
		fixture.store.records[record.WorkspaceID] = record
		fixture.store.mu.Unlock()
		collector := &fakeJanitorCollector{}
		janitor := newTestJanitor(t, fixture, collector, terminal.Add(2*time.Hour))
		result := janitor.ScanOnce(context.Background())
		if result.SkippedIdentity != 1 || collector.calls != 0 {
			t.Fatalf("identity mismatch reached collector: result=%#v calls=%d", result, collector.calls)
		}
	})

	t.Run("incomplete authority metadata", func(t *testing.T) {
		fixture, terminal := makeRetained(t)
		record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
		record.OwnerID = "forged"
		fixture.store.mu.Lock()
		fixture.store.records[record.WorkspaceID] = record
		fixture.store.mu.Unlock()
		collector := &fakeJanitorCollector{}
		result := newTestJanitor(t, fixture, collector, terminal.Add(2*time.Hour)).ScanOnce(context.Background())
		if result.SkippedIdentity != 1 || collector.callCount() != 0 {
			t.Fatalf("incomplete authority reached collector: result=%#v calls=%d", result, collector.callCount())
		}
	})

	t.Run("path replacement", func(t *testing.T) {
		fixture, terminal := makeRetained(t)
		original := fixture.layout.WorkspaceRoot + "-original"
		if err := os.Rename(fixture.layout.WorkspaceRoot, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), fixture.layout.WorkspaceRoot); err != nil {
			t.Fatal(err)
		}
		collector := &fakeJanitorCollector{}
		result := newTestJanitor(t, fixture, collector, terminal.Add(2*time.Hour)).ScanOnce(context.Background())
		if result.SkippedIdentity != 1 || collector.callCount() != 0 {
			t.Fatalf("path replacement reached collector: result=%#v calls=%d", result, collector.callCount())
		}
	})

	t.Run("manifest mismatch", func(t *testing.T) {
		fixture, terminal := makeRetained(t)
		record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
		if err := os.WriteFile(filepath.Join(fixture.layout.Control, filepath.FromSlash(record.Manifest.Path)), []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		collector := &fakeJanitorCollector{}
		result := newTestJanitor(t, fixture, collector, terminal.Add(2*time.Hour)).ScanOnce(context.Background())
		if result.SkippedIdentity != 1 || collector.callCount() != 0 {
			t.Fatalf("manifest mismatch reached collector: result=%#v calls=%d", result, collector.callCount())
		}
	})
}

func TestJanitorFilterRealManagerPreservesProtectedChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		retain func(*managerFixture, Lease)
	}{
		{name: "dirty", retain: func(f *managerFixture, lease Lease) {
			f.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "kept.txt"}}}
			if _, err := f.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "unpushed", retain: func(f *managerFixture, lease Lease) {
			newHead := "2222222222222222222222222222222222222222"
			branchRef := "refs/heads/" + f.layout.Branch
			f.git.head = newHead
			f.git.refs = []RefInfo{{Name: branchRef, ObjectName: newHead}}
			f.git.worktrees = []WorktreeInfo{{Path: f.layout.WorkspaceRoot, HEAD: newHead, Branch: branchRef}}
			if _, err := f.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "initialization changed", retain: func(f *managerFixture, lease Lease) {
			f.init.verifyErr = errors.New("fingerprint changed")
			if _, err := f.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
			if err != nil {
				t.Fatal(err)
			}
			test.retain(fixture, lease)
			record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
			now := record.Settlement.CompletedAt.Add(2 * time.Hour)
			collector := newJanitorCollectorManager(t, fixture, now)
			result := newTestJanitor(t, fixture, collector, now).ScanOnce(context.Background())
			if result.Retained != 1 || fixture.git.removeCalls != 0 || fixture.git.deleteRefCalls != 0 {
				t.Fatalf("protected workspace deleted: result=%#v remove=%d ref=%d", result, fixture.git.removeCalls, fixture.git.deleteRefCalls)
			}
		})
	}
}

func TestJanitorTTLDoesNotRefreshOnFailedCollection(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "kept.txt"}}}
	if _, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	terminal := record.Settlement.CompletedAt
	collector := &fakeJanitorCollector{err: errors.New("inspection unavailable")}
	janitor := newTestJanitor(t, fixture, collector, terminal.Add(2*time.Hour))
	result := janitor.ScanOnce(context.Background())
	if result.CheckFailed != 1 {
		t.Fatalf("failed collection not counted: %#v", result)
	}
	after, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if !after.Settlement.CompletedAt.Equal(terminal) {
		t.Fatalf("failed scan refreshed TTL: before=%v after=%v", terminal, after.Settlement.CompletedAt)
	}
}

func TestJanitorBudgetBoundsCandidatesConcurrencyAndShutdown(t *testing.T) {
	fixture := newManagerFixture(t)
	store := &captureJanitorStore{Store: fixture.store}
	collector := &fakeJanitorCollector{}
	janitor, err := NewJanitor(JanitorOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, JanitorInterval: 10 * time.Millisecond, JanitorTimeout: 50 * time.Millisecond},
			Limits:    Limits{MaxJanitorCandidates: 3, MaxJanitorConcurrency: 2},
		},
		Store: store, Manager: collector, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := janitor.ScanOnce(context.Background())
	if len(store.limits) == 0 || store.limits[0] != 4 || result.Candidates > 3 {
		t.Fatalf("candidate budget not applied: limits=%v result=%#v", store.limits, result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	janitor.Start(ctx)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := janitor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := janitor.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop was not idempotent: %v", err)
	}
}

func TestJanitorBudgetEnforcesRealCandidateAndConcurrencyLimits(t *testing.T) {
	fixture := newManagerFixture(t)
	terminal := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 5; index++ {
		addJanitorRecord(t, fixture, fmt.Sprintf("%032x", index+1), StateRetained, terminal)
	}
	collector := newPeakJanitorCollector()
	janitor, err := NewJanitor(JanitorOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, JanitorInterval: time.Minute, JanitorTimeout: time.Second},
			Limits:    Limits{MaxJanitorCandidates: 3, MaxJanitorConcurrency: 2},
		},
		Store: fixture.store, Manager: collector, Clock: func() time.Time { return terminal.Add(2 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan ScanResult, 1)
	go func() { results <- janitor.ScanOnce(context.Background()) }()
	for index := 0; index < 2; index++ {
		select {
		case <-collector.entered:
		case <-time.After(time.Second):
			t.Fatal("configured concurrency was not reached")
		}
	}
	select {
	case <-collector.entered:
		t.Fatal("collector exceeded configured concurrency before release")
	case <-time.After(30 * time.Millisecond):
	}
	close(collector.release)
	result := <-results
	if collector.maxActive != 2 || collector.calls != 3 || result.Candidates != 3 || !result.BudgetExhausted {
		t.Fatalf("real budgets not enforced: active=%d calls=%d result=%#v", collector.maxActive, collector.calls, result)
	}
}

func TestJanitorActiveLeaseAndConcurrentJanitorsNeverDoubleDelete(t *testing.T) {
	fixture := newManagerFixture(t)
	locks, err := NewFileLockManager(fixture.layout.Control, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC)
	manager, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, SettleTimeout: time.Second, JanitorTimeout: 100 * time.Millisecond},
			Limits:    Limits{MaxActive: 8, MaxRetained: 8},
		},
		Store: fixture.store, Locks: locks, GitReader: fixture.git, GitMutator: fixture.git, Initializer: fixture.init,
		Clock: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	record.State = StateSettling // simulate process checkpoint while OS active lease is still authoritative
	if err := fixture.store.CompareAndSwap(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	janitor := newTestJanitor(t, fixture, manager, clock)
	result := janitor.ScanOnce(context.Background())
	if result.SkippedBusy != 1 || fixture.git.removeCalls != 0 {
		t.Fatalf("active OS lease was bypassed: result=%#v remove=%d", result, fixture.git.removeCalls)
	}
	if err := manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	janitors := []Janitor{newTestJanitor(t, fixture, manager, clock), newTestJanitor(t, fixture, manager, clock)}
	results := make(chan ScanResult, len(janitors))
	for _, candidate := range janitors {
		go func(j Janitor) { results <- j.ScanOnce(context.Background()) }(candidate)
	}
	for range janitors {
		<-results
	}
	if fixture.git.removeCalls != 1 || fixture.git.deleteRefCalls != 1 {
		t.Fatalf("concurrent Janitors advanced deletion more than once: remove=%d ref=%d", fixture.git.removeCalls, fixture.git.deleteRefCalls)
	}
}

func TestJanitorShutdownCancelsRunningCollectionAndIsIdempotent(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "kept.txt"}}}
	if _, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	collector := &blockingJanitorCollector{entered: make(chan struct{})}
	janitor := newTestJanitor(t, fixture, collector, record.Settlement.CompletedAt.Add(2*time.Hour))
	janitor.Start(context.Background())
	select {
	case <-collector.entered:
	case <-time.After(time.Second):
		t.Fatal("periodic Janitor did not begin initial scan")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := janitor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := janitor.Stop(stopCtx); err != nil {
		t.Fatalf("idempotent Stop failed: %v", err)
	}
	if collector.calls != 1 {
		t.Fatalf("Stop admitted another collection: %d", collector.calls)
	}
	started := time.Now()
	result := janitor.ScanOnce(context.Background())
	if collector.calls != 1 || time.Since(started) > 50*time.Millisecond || result.SkippedBusy != 1 {
		t.Fatalf("ScanOnce admitted after Stop: result=%#v calls=%d elapsed=%v", result, collector.calls, time.Since(started))
	}
}

func TestJanitorStopWaitsForAdmittedPublicScanAndRejectsLaterScan(t *testing.T) {
	fixture := newManagerFixture(t)
	terminal := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	addJanitorRecord(t, fixture, managerTestWorkspaceID, StateRetained, terminal)
	collector := newPeakJanitorCollector()
	janitor := newTestJanitor(t, fixture, collector, terminal.Add(2*time.Hour))
	scanDone := make(chan ScanResult, 1)
	go func() { scanDone <- janitor.ScanOnce(context.Background()) }()
	select {
	case <-collector.entered:
	case <-time.After(time.Second):
		t.Fatal("public ScanOnce did not reach admitted collection")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- janitor.Stop(context.Background()) }()
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-scanDone:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Stop returned before the admitted public scan exited")
	}
	close(collector.release)
	calls := collector.calls
	result := janitor.ScanOnce(context.Background())
	if result.SkippedBusy != 1 || collector.calls != calls {
		t.Fatalf("post-Stop public scan admitted: result=%#v before=%d after=%d", result, calls, collector.calls)
	}
}

func TestJanitorStopLinearizesBetweenAdmissionAndScanToken(t *testing.T) {
	fixture := newManagerFixture(t)
	terminal := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	addJanitorRecord(t, fixture, managerTestWorkspaceID, StateRetained, terminal)
	collector := &fakeJanitorCollector{settlement: Settlement{State: SettlementDeleted}}
	entered := make(chan struct{})
	release := make(chan struct{})
	janitor, err := NewJanitor(JanitorOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, JanitorInterval: time.Minute, JanitorTimeout: time.Second},
			Limits:    Limits{MaxJanitorCandidates: 4, MaxJanitorConcurrency: 1},
		},
		Store: fixture.store, Manager: collector, Clock: func() time.Time { return terminal.Add(2 * time.Hour) },
		BeforeScan: func() {
			close(entered)
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scanDone := make(chan ScanResult, 1)
	go func() { scanDone <- janitor.ScanOnce(context.Background()) }()
	<-entered
	stopDone := make(chan error, 1)
	go func() { stopDone <- janitor.Stop(context.Background()) }()
	select {
	case err := <-stopDone:
		t.Fatalf("Stop crossed an admitted scan before registration completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	result := <-scanDone
	if collector.callCount() != 0 || result.SkippedBusy != 1 {
		t.Fatalf("scan admitted destructive work after Stop: result=%#v calls=%d", result, collector.callCount())
	}
}

func TestJanitorPeriodRunsImmediatelyAndOnTickThenStops(t *testing.T) {
	fixture := newManagerFixture(t)
	terminal := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	addJanitorRecord(t, fixture, managerTestWorkspaceID, StateRetained, terminal)
	collector := &fakeJanitorCollector{settlement: Settlement{State: SettlementRetained}}
	janitor, err := NewJanitor(JanitorOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, JanitorInterval: 15 * time.Millisecond, JanitorTimeout: time.Second},
			Limits:    Limits{MaxJanitorCandidates: 4, MaxJanitorConcurrency: 1},
		},
		Store: fixture.store, Manager: collector, Clock: func() time.Time { return terminal.Add(2 * time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	janitor.Start(context.Background())
	deadline := time.Now().Add(time.Second)
	for collector.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if collector.callCount() < 2 {
		t.Fatalf("initial and tick scans did not both run: %d", collector.callCount())
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := janitor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	stoppedCalls := collector.callCount()
	time.Sleep(40 * time.Millisecond)
	if collector.callCount() != stoppedCalls {
		t.Fatalf("periodic scan continued after Stop: before=%d after=%d", stoppedCalls, collector.callCount())
	}
}

func TestJanitorTTLUsesLastHeartbeatExactBoundary(t *testing.T) {
	fixture := newManagerFixture(t)
	heartbeat := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	workspaceID := managerTestWorkspaceID
	addJanitorRecord(t, fixture, workspaceID, StateSettling, time.Time{})
	record, _ := fixture.store.Load(context.Background(), workspaceID)
	record.Settlement = SettlementRecord{}
	record.Lease = LeaseRecord{OwnerID: record.OwnerID, Mode: "active", AcquiredAt: heartbeat.Add(-time.Hour), Heartbeat: heartbeat}
	record.UpdatedAt = heartbeat.Add(30 * time.Minute)
	fixture.store.mu.Lock()
	fixture.store.records[workspaceID] = record
	fixture.store.mu.Unlock()
	collector := &fakeJanitorCollector{err: errors.New("inspection failed")}
	before := newTestJanitor(t, fixture, collector, heartbeat.Add(time.Hour-time.Nanosecond)).ScanOnce(context.Background())
	if before.SkippedFresh != 1 || collector.callCount() != 0 {
		t.Fatalf("heartbeat candidate expired early: result=%#v calls=%d", before, collector.callCount())
	}
	atBoundary := newTestJanitor(t, fixture, collector, heartbeat.Add(time.Hour)).ScanOnce(context.Background())
	if atBoundary.CheckFailed != 1 || collector.callCount() != 1 {
		t.Fatalf("heartbeat candidate did not expire at exact TTL: result=%#v calls=%d", atBoundary, collector.callCount())
	}
	after, _ := fixture.store.Load(context.Background(), workspaceID)
	if !after.Lease.Heartbeat.Equal(heartbeat) || !after.UpdatedAt.Equal(record.UpdatedAt) {
		t.Fatalf("failed heartbeat scan refreshed authority: before=%#v after=%#v", record.Lease, after.Lease)
	}
}

func TestJanitorOrphanManifestRequiresValidatedUnreferencedImmutableFile(t *testing.T) {
	fixture := newManagerFixture(t)
	created := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, WorkspaceID: managerTestWorkspaceID, CreatedAt: created}
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := DecodeManifest(encoded)
	orphanName := managerTestWorkspaceID + "-" + decoded.IntegrityDigest + "-abcdefabcdefabcdefabcdefabcdefab.json"
	orphanPath := filepath.Join(fixture.layout.Control, "manifests", orphanName)
	if err := os.WriteFile(orphanPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.json")
	if err := os.WriteFile(external, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(fixture.layout.Control, "manifests", managerTestWorkspaceID+"-"+decoded.IntegrityDigest+"-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.json")
	if err := os.Link(external, hardlink); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	symlink := filepath.Join(fixture.layout.Control, "manifests", managerTestWorkspaceID+"-"+decoded.IntegrityDigest+"-cccccccccccccccccccccccccccccccc.json")
	if err := os.Symlink(external, symlink); err != nil {
		t.Fatal(err)
	}
	janitor := newTestJanitor(t, fixture, &fakeJanitorCollector{}, created.Add(48*time.Hour))
	result := janitor.ScanOnce(context.Background())
	if result.OrphansDeleted != 0 || result.SkippedIdentity < 3 {
		t.Fatalf("orphan manifests were not conservatively retained: %#v", result)
	}
	if _, err := os.Lstat(orphanPath); err != nil {
		t.Fatalf("validated orphan was automatically deleted: %v", err)
	}
	if _, err := os.Lstat(hardlink); err != nil {
		t.Fatalf("hard-linked unknown manifest was deleted: %v", err)
	}
	if info, err := os.Lstat(symlink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink manifest was deleted or followed: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(external); err != nil {
		t.Fatalf("external hard-link target was affected: %v", err)
	}
}

func TestJanitorOrphanManifestRenameSwapNeverDeletesEitherInode(t *testing.T) {
	fixture := newManagerFixture(t)
	created := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	encoded, err := EncodeManifest(Manifest{SchemaVersion: ManifestSchemaVersion, WorkspaceID: managerTestWorkspaceID, CreatedAt: created})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	manifestRoot := filepath.Join(fixture.layout.Control, "manifests")
	orphanPath := filepath.Join(manifestRoot, managerTestWorkspaceID+"-"+decoded.IntegrityDigest+"-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.json")
	unknownPath := filepath.Join(manifestRoot, "unknown-user-file")
	retainedPath := filepath.Join(manifestRoot, "retained-valid-inode")
	if err := os.WriteFile(orphanPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknownPath, []byte("unknown-user-inode\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	janitor, err := NewJanitor(JanitorOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, JanitorInterval: time.Minute, JanitorTimeout: time.Second},
			Limits:    Limits{MaxJanitorCandidates: 16, MaxJanitorConcurrency: 2},
		},
		Store: fixture.store, Manager: &fakeJanitorCollector{}, Clock: func() time.Time { return created.Add(48 * time.Hour) },
		BeforeOrphanRemove: func() {
			if err := os.Rename(orphanPath, retainedPath); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(unknownPath, orphanPath); err != nil {
				t.Error(err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := janitor.ScanOnce(context.Background())
	if result.OrphansDeleted != 0 || result.SkippedIdentity == 0 {
		t.Fatalf("rename swap was not preserved fail-closed: %#v", result)
	}
	for _, path := range []string{orphanPath, retainedPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("rename-swap inode was deleted: %v", err)
		}
	}
}

func TestJanitorOrphanManifestPreservesWorkspaceWithActiveOrInitializingRecord(t *testing.T) {
	for _, state := range []State{StateActive, StateCreating, StateInitializing, StateReady} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newManagerFixture(t)
			lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
			if err != nil {
				t.Fatal(err)
			}
			defer fixture.manager.Release(context.Background(), lease)
			record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
			record.State = state
			fixture.store.mu.Lock()
			fixture.store.records[record.WorkspaceID] = record
			fixture.store.mu.Unlock()
			created := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
			encoded, err := EncodeManifest(Manifest{SchemaVersion: ManifestSchemaVersion, WorkspaceID: managerTestWorkspaceID, CreatedAt: created})
			if err != nil {
				t.Fatal(err)
			}
			decoded, _ := DecodeManifest(encoded)
			name := managerTestWorkspaceID + "-" + decoded.IntegrityDigest + "-dddddddddddddddddddddddddddddddd.json"
			path := filepath.Join(fixture.layout.Control, "manifests", name)
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			janitor := newTestJanitor(t, fixture, &fakeJanitorCollector{}, created.Add(48*time.Hour))
			result := janitor.ScanOnce(context.Background())
			if result.OrphansDeleted != 0 {
				t.Fatalf("%s workspace orphan checkpoint was deleted during activity: %#v", state, result)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("%s workspace orphan checkpoint missing: %v", state, err)
			}
		})
	}
}

func newTestJanitor(t *testing.T, fixture *managerFixture, collector JanitorCollector, now time.Time) Janitor {
	t.Helper()
	janitor, err := NewJanitor(JanitorOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, JanitorInterval: time.Minute, JanitorTimeout: time.Second},
			Limits:    Limits{MaxJanitorCandidates: 16, MaxJanitorConcurrency: 2},
		},
		Store: fixture.store, Manager: collector, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return janitor
}

func newJanitorCollectorManager(t *testing.T, fixture *managerFixture, now time.Time) Manager {
	t.Helper()
	manager, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, SettleTimeout: time.Second, JanitorTimeout: time.Second},
			Limits:    Limits{MaxActive: 8, MaxRetained: 8},
		},
		Store: fixture.store, Locks: fixture.locks, GitReader: fixture.git, GitMutator: fixture.git, Initializer: fixture.init,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

type fakeJanitorCollector struct {
	mu         sync.Mutex
	calls      int
	settlement Settlement
	err        error
}

func (c *fakeJanitorCollector) Collect(context.Context, string) (Settlement, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.settlement, c.err
}

func (c *fakeJanitorCollector) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type peakJanitorCollector struct {
	mu        sync.Mutex
	active    int
	maxActive int
	calls     int
	entered   chan struct{}
	release   chan struct{}
}

func newPeakJanitorCollector() *peakJanitorCollector {
	return &peakJanitorCollector{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (c *peakJanitorCollector) Collect(ctx context.Context, _ string) (Settlement, error) {
	c.mu.Lock()
	c.active++
	c.calls++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.mu.Unlock()
	c.entered <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return Settlement{State: SettlementRetained}, nil
}

type captureJanitorStore struct {
	Store
	limits []int
}

type blockingJanitorCollector struct {
	entered chan struct{}
	calls   int
	once    sync.Once
}

func (c *blockingJanitorCollector) Collect(ctx context.Context, _ string) (Settlement, error) {
	c.calls++
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	return Settlement{}, ctx.Err()
}

func (s *captureJanitorStore) List(ctx context.Context, query ListQuery) ([]Record, error) {
	s.limits = append(s.limits, query.Limit)
	return s.Store.List(ctx, query)
}

func addJanitorRecord(t *testing.T, fixture *managerFixture, workspaceID string, state State, terminal time.Time) {
	t.Helper()
	layout, err := ResolveManagedLayout(fixture.repo, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.WorkspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	created := terminal.Add(-time.Hour)
	if terminal.IsZero() {
		created = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	}
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, WorkspaceID: workspaceID, CreatedAt: created}
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := DecodeManifest(encoded)
	manifestName := workspaceID + "-" + decoded.IntegrityDigest + "-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.json"
	manifestRef := filepath.ToSlash(filepath.Join("manifests", manifestName))
	if err := os.WriteFile(filepath.Join(fixture.layout.Control, filepath.FromSlash(manifestRef)), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	record := Record{
		SchemaVersion: RecordSchemaVersion, Revision: 1, WorkspaceID: workspaceID, OwnerID: managerTestOwnerID,
		RepositoryIdentity: fixture.identity, LogicalName: "janitor/task", Directory: layout.WorkspaceRoot, Branch: layout.Branch,
		BaseOID: managerTestOID, HeadOID: managerTestOID, State: state,
		Manifest:   ManifestRef{Path: manifestRef, Digest: decoded.IntegrityDigest},
		Settlement: SettlementRecord{State: SettlementRetained, ReasonCode: "expired", CompletedAt: terminal},
		CreatedAt:  created, UpdatedAt: created,
	}
	if state == StateSettling {
		record.Settlement = SettlementRecord{}
		record.Lease = LeaseRecord{OwnerID: record.OwnerID, Mode: "active", AcquiredAt: created, Heartbeat: created}
	}
	if err := fixture.store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

var _ JanitorCollector = (*fakeJanitorCollector)(nil)
var _ JanitorCollector = (*blockingJanitorCollector)(nil)
var _ JanitorCollector = (*peakJanitorCollector)(nil)
