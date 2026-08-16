package worktree

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	hardMaxJanitorCandidates  = 10_000
	hardMaxJanitorConcurrency = 64
	janitorReferenceLimit     = 10_000
	janitorLockBudget         = 250 * time.Millisecond
)

type JanitorCollector interface {
	Collect(context.Context, string) (Settlement, error)
}

type Janitor interface {
	Start(context.Context)
	Stop(context.Context) error
	ScanOnce(context.Context) ScanResult
}

type JanitorOptions struct {
	ControlRoot string
	Config      Config
	Store       Store
	Manager     JanitorCollector
	Clock       func() time.Time
	BeforeScan  func()
	// BeforeOrphanRemove is a test barrier at the final would-be removal point.
	// Orphan removal is intentionally disabled because supported platforms do
	// not provide an identity-conditional unlink primitive.
	BeforeOrphanRemove func()
}

type ScanResult struct {
	Candidates      int
	Deleted         int
	Retained        int
	Partial         int
	SkippedFresh    int
	SkippedIdentity int
	SkippedBusy     int
	CheckFailed     int
	OrphansDeleted  int
	BudgetExhausted bool
}

type lifecycleJanitor struct {
	options JanitorOptions
	control string
	scan    chan struct{}

	mu         sync.Mutex
	started    bool
	stopped    bool
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	scans      sync.WaitGroup
	stopCtx    context.Context
	stopCancel context.CancelFunc
}

func NewJanitor(options JanitorOptions) (Janitor, error) {
	limits := options.Config.Limits
	lifecycle := options.Config.Lifecycle
	if options.Store == nil || options.Manager == nil || options.Clock == nil || lifecycle.RetentionTTL <= 0 ||
		lifecycle.JanitorInterval <= 0 || lifecycle.JanitorTimeout <= 0 || limits.MaxJanitorCandidates <= 0 ||
		limits.MaxJanitorCandidates > hardMaxJanitorCandidates || limits.MaxJanitorConcurrency <= 0 ||
		limits.MaxJanitorConcurrency > hardMaxJanitorConcurrency || limits.MaxJanitorConcurrency > limits.MaxJanitorCandidates {
		return nil, ErrManagerInvalid
	}
	control, err := canonicalDirectory(options.ControlRoot)
	if err != nil {
		return nil, ErrManagerInvalid
	}
	root, err := newInitializerRoot(control)
	if err != nil {
		return nil, ErrManagerInvalid
	}
	defer root.Close()
	info, err := root.Stat("manifests")
	if err != nil || !info.IsDir() {
		return nil, ErrManagerInvalid
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	return &lifecycleJanitor{
		options: options, control: control, scan: make(chan struct{}, 1), stopCtx: stopCtx, stopCancel: stopCancel,
	}, nil
}

func (j *lifecycleJanitor) Start(parent context.Context) {
	if parent == nil {
		return
	}
	j.mu.Lock()
	if j.started || j.stopped {
		j.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	j.started, j.cancel = true, cancel
	j.wg.Add(1)
	j.mu.Unlock()
	go func() {
		defer j.wg.Done()
		j.ScanOnce(ctx)
		ticker := time.NewTicker(j.options.Config.Lifecycle.JanitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				j.ScanOnce(ctx)
			}
		}
	}()
}

func (j *lifecycleJanitor) Stop(ctx context.Context) error {
	if ctx == nil {
		return ErrManagerInvalid
	}
	j.mu.Lock()
	if !j.stopped {
		j.stopped = true
		j.stopCancel()
		if j.cancel != nil {
			j.cancel()
		}
	}
	j.mu.Unlock()
	done := make(chan struct{})
	go func() {
		j.scans.Wait()
		j.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (j *lifecycleJanitor) ScanOnce(parent context.Context) ScanResult {
	var result ScanResult
	if parent == nil {
		result.CheckFailed = 1
		return result
	}
	j.mu.Lock()
	if j.stopped {
		j.mu.Unlock()
		result.SkippedBusy = 1
		return result
	}
	j.scans.Add(1)
	stopCtx := j.stopCtx
	j.mu.Unlock()
	defer j.scans.Done()
	if j.options.BeforeScan != nil {
		j.options.BeforeScan()
	}
	if stopCtx.Err() != nil {
		result.SkippedBusy = 1
		return result
	}
	select {
	case j.scan <- struct{}{}:
		defer func() { <-j.scan }()
	default:
		result.SkippedBusy = 1
		return result
	}
	ctx, cancel := context.WithTimeout(parent, j.options.Config.Lifecycle.JanitorTimeout)
	defer cancel()
	stopLink := context.AfterFunc(stopCtx, cancel)
	defer stopLink()
	limit := j.options.Config.Limits.MaxJanitorCandidates
	records, err := j.options.Store.List(ctx, ListQuery{
		States: []State{StateRetained, StateSettling, StatePartial},
		Limit:  limit + 1,
	})
	if err != nil {
		result.CheckFailed++
		return result
	}
	if len(records) > limit {
		records = records[:limit]
		result.BudgetExhausted = true
	}
	j.scanRecords(ctx, records, &result)
	if ctx.Err() != nil {
		result.BudgetExhausted = true
		return result
	}
	remaining := limit - result.Candidates
	if remaining <= 0 {
		result.BudgetExhausted = true
		return result
	}
	j.scanOrphanManifests(ctx, remaining, &result)
	return result
}

func (j *lifecycleJanitor) scanRecords(ctx context.Context, records []Record, result *ScanResult) {
	workers := j.options.Config.Limits.MaxJanitorConcurrency
	if workers > len(records) {
		workers = len(records)
	}
	if workers == 0 {
		return
	}
	jobs := make(chan Record)
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for record := range jobs {
				local := j.scanRecord(ctx, record)
				resultMu.Lock()
				mergeScanResult(result, local)
				resultMu.Unlock()
			}
		}()
	}
	for _, record := range records {
		select {
		case jobs <- record:
		case <-ctx.Done():
			result.BudgetExhausted = true
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (j *lifecycleJanitor) scanRecord(ctx context.Context, record Record) ScanResult {
	result := ScanResult{Candidates: 1}
	if !collectibleState(record) {
		return result
	}
	if !recordExpired(record, j.options.Clock().UTC(), j.options.Config.Lifecycle.RetentionTTL) {
		result.SkippedFresh = 1
		return result
	}
	if err := j.validateCandidate(record); err != nil {
		result.SkippedIdentity = 1
		return result
	}
	budget := janitorLockBudget
	if timeout := j.options.Config.Lifecycle.JanitorTimeout; timeout < budget {
		budget = timeout
	}
	candidateCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	settlement, err := j.options.Manager.Collect(candidateCtx, record.WorkspaceID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLockTimeout) {
			result.SkippedBusy = 1
		} else {
			result.CheckFailed = 1
		}
		return result
	}
	switch settlement.State {
	case SettlementDeleted:
		result.Deleted = 1
	case SettlementRetained:
		result.Retained = 1
	case SettlementPartial:
		result.Partial = 1
	default:
		result.CheckFailed = 1
	}
	return result
}

func (j *lifecycleJanitor) validateCandidate(record Record) error {
	if !validJanitorRecordAuthority(record) {
		return ErrInvalidMetadata
	}
	identity, err := NewRepositoryIdentity(record.RepositoryIdentity.Root, record.RepositoryIdentity.CommonDir)
	if err != nil || !identity.Equal(record.RepositoryIdentity) || !sameRepositoryDirectoryObject(identity.Root, record.RepositoryIdentity.Root) {
		return ErrIdentityMismatch
	}
	layout, err := ResolveManagedLayout(identity.Root, record.WorkspaceID)
	if err != nil || layout.Control != j.control || layout.WorkspaceRoot != record.Directory || layout.Branch != record.Branch {
		return ErrIdentityMismatch
	}
	allowMissing := record.State == StatePartial && resumableDeleteReason(record.Settlement.ReasonCode)
	pathIdentity, err := ValidateManagedPath(layout.Root, record.Directory, allowMissing)
	if err != nil || (allowMissing && pathIdentity.Exists()) {
		return ErrIdentityMismatch
	}
	_, err = j.loadCandidateManifest(record)
	return err
}

func (j *lifecycleJanitor) loadCandidateManifest(record Record) (Manifest, error) {
	path := filepath.ToSlash(record.Manifest.Path)
	if filepath.Dir(path) != "manifests" || !strings.HasPrefix(filepath.Base(path), record.WorkspaceID+"-") || !validDigest(record.Manifest.Digest) {
		return Manifest{}, ErrInvalidMetadata
	}
	root, err := newInitializerRoot(j.control)
	if err != nil {
		return Manifest{}, ErrUnsafePath
	}
	defer root.Close()
	file, info, err := root.OpenRead(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxMetadataBytes {
		return Manifest{}, ErrInvalidMetadata
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil || len(data) > maxMetadataBytes {
		return Manifest{}, ErrInvalidMetadata
	}
	manifest, err := DecodeManifest(data)
	if err != nil || manifest.WorkspaceID != record.WorkspaceID || manifest.IntegrityDigest != record.Manifest.Digest {
		return Manifest{}, ErrIntegrityMismatch
	}
	return manifest, nil
}

func (j *lifecycleJanitor) scanOrphanManifests(ctx context.Context, budget int, result *ScanResult) {
	records, err := j.options.Store.List(ctx, ListQuery{Limit: janitorReferenceLimit + 1})
	if err != nil || len(records) > janitorReferenceLimit {
		result.CheckFailed++
		return
	}
	referenced := make(map[string]bool, len(records))
	unsafeWorkspace := make(map[string]bool)
	for _, record := range records {
		if !ValidWorkspaceID(record.WorkspaceID) || j.validateRecordReference(record) != nil {
			if ValidWorkspaceID(record.WorkspaceID) {
				unsafeWorkspace[record.WorkspaceID] = true
			}
			continue
		}
		if orphanWorkspaceProtected(record) {
			unsafeWorkspace[record.WorkspaceID] = true
		}
		if record.Manifest.Path != "" {
			referenced[filepath.ToSlash(record.Manifest.Path)] = true
		}
	}
	root, err := newInitializerRoot(j.control)
	if err != nil {
		result.CheckFailed++
		return
	}
	defer root.Close()
	parentIdentity, err := root.Identity("manifests")
	if err != nil {
		result.CheckFailed++
		return
	}
	entries, err := root.ReadDir("manifests")
	if err != nil {
		result.CheckFailed++
		return
	}
	processed := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			result.BudgetExhausted = true
			return
		}
		workspaceID, digest, ok := parseOrphanManifestName(entry.Name())
		relative := filepath.ToSlash(filepath.Join("manifests", entry.Name()))
		if ok && referenced[relative] {
			continue
		}
		if processed >= budget {
			result.BudgetExhausted = true
			return
		}
		processed++
		result.Candidates++
		if !ok {
			result.SkippedIdentity++
			continue
		}
		if unsafeWorkspace[workspaceID] {
			result.SkippedBusy++
			continue
		}
		fileIdentity, err := root.Identity(relative)
		if err != nil {
			result.SkippedIdentity++
			continue
		}
		file, info, err := root.OpenRead(relative)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxMetadataBytes || !janitorFileUnshared(file) {
			if file != nil {
				_ = file.Close()
			}
			result.SkippedIdentity++
			continue
		}
		openedIdentity, identityErr := initializerIdentityFromFile(file)
		data, readErr := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
		closeErr := file.Close()
		if identityErr != nil || openedIdentity != fileIdentity || readErr != nil || closeErr != nil || len(data) > maxMetadataBytes {
			result.SkippedIdentity++
			continue
		}
		manifest, err := DecodeManifest(data)
		if err != nil || manifest.WorkspaceID != workspaceID || manifest.IntegrityDigest != digest {
			result.SkippedIdentity++
			continue
		}
		if !recordExpired(Record{Settlement: SettlementRecord{CompletedAt: manifest.CreatedAt}}, j.options.Clock().UTC(), j.options.Config.Lifecycle.RetentionTTL) {
			result.SkippedFresh++
			continue
		}
		afterIdentity, err := root.Identity(relative)
		currentParent, parentErr := root.Identity("manifests")
		if err != nil || parentErr != nil || afterIdentity != fileIdentity || currentParent != parentIdentity {
			result.SkippedIdentity++
			continue
		}
		if j.options.BeforeOrphanRemove != nil {
			j.options.BeforeOrphanRemove()
		}
		// There is no portable atomic "unlink this path only if it still names
		// this inode" operation. A rename can replace the checked file between
		// identity validation and unlinkat, so automatic orphan deletion stays
		// disabled under the N5/F86 fail-closed policy.
		result.SkippedIdentity++
	}
}

func orphanWorkspaceProtected(record Record) bool {
	switch record.State {
	case StateCreating, StateInitializing, StateReady, StateActive, StateSettling, StateManualAttention:
		return true
	case StatePartial:
		return !resumableDeleteReason(record.Settlement.ReasonCode)
	default:
		return false
	}
}

func (j *lifecycleJanitor) validateRecordReference(record Record) error {
	if !validJanitorRecordAuthority(record) {
		return ErrInvalidMetadata
	}
	identity, err := NewRepositoryIdentity(record.RepositoryIdentity.Root, record.RepositoryIdentity.CommonDir)
	if err != nil || !identity.Equal(record.RepositoryIdentity) {
		return ErrIdentityMismatch
	}
	layout, err := ResolveManagedLayout(identity.Root, record.WorkspaceID)
	if err != nil || layout.Control != j.control || layout.WorkspaceRoot != record.Directory || layout.Branch != record.Branch {
		return ErrIdentityMismatch
	}
	if record.Manifest.Path != "" {
		_, err = j.loadCandidateManifest(record)
	}
	return err
}

func validJanitorRecordAuthority(record Record) bool {
	return record.SchemaVersion == RecordSchemaVersion && record.Revision > 0 && ValidWorkspaceID(record.WorkspaceID) &&
		ValidWorkspaceID(record.OwnerID) && record.State.Valid() && !record.CreatedAt.IsZero() && !record.UpdatedAt.IsZero() &&
		!record.UpdatedAt.Before(record.CreatedAt) && filepath.IsAbs(record.Directory) && filepath.Clean(record.Directory) == record.Directory &&
		record.Branch == "xagent/worktree/"+record.WorkspaceID && validOID(record.BaseOID) && validOID(record.HeadOID) &&
		ValidateLogicalName(record.LogicalName, Limits{MaxNameBytes: hardMaxNameBytes, MaxSegmentBytes: hardMaxSegmentBytes, MaxDepth: hardMaxDepth}) == nil
}

func parseOrphanManifestName(name string) (string, string, bool) {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(name, ".json"), "-")
	if len(parts) != 3 || !ValidWorkspaceID(parts[0]) || !validDigest(parts[1]) || !ValidWorkspaceID(parts[2]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func mergeScanResult(target *ScanResult, value ScanResult) {
	target.Candidates += value.Candidates
	target.Deleted += value.Deleted
	target.Retained += value.Retained
	target.Partial += value.Partial
	target.SkippedFresh += value.SkippedFresh
	target.SkippedIdentity += value.SkippedIdentity
	target.SkippedBusy += value.SkippedBusy
	target.CheckFailed += value.CheckFailed
	target.OrphansDeleted += value.OrphansDeleted
	target.BudgetExhausted = target.BudgetExhausted || value.BudgetExhausted
}

var _ Janitor = (*lifecycleJanitor)(nil)
var _ JanitorCollector = (*lifecycleManager)(nil)
