package tool

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"xagent/internal/artifact"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

const maxReadCacheFingerprintBytes = 256

// ReadCacheLimits are fully resolved construction limits. ReadCache never
// interprets missing or zero values as defaults.
type ReadCacheLimits struct {
	MaxEntries              int
	MaxBytes                int64
	MaxValueBytes           int64
	MaxDependenciesPerEntry int
}

type ReadCacheKey struct {
	Tool                 string
	ArgumentsFingerprint string
}

// FileVersion is a stable dependency snapshot. Digest covers contents for a
// regular file and the immediate entry set for a directory. Size=-1 denotes a
// path that did not exist when the result was produced.
type FileVersion struct {
	Path    string
	Size    int64
	ModTime int64
	Digest  string
}

// CachedReadResult contains only already-redacted, bounded projections and an
// opaque artifact reference. Invocation identity and raw output are absent by
// construction.
type CachedReadResult struct {
	State            ExecutionState
	Status           ResultStatus
	Summary          redact.SafeText
	Preview          redact.SafeText
	Artifact         *artifact.Ref
	CapturedBytes    int64
	Truncated        bool
	TruncationReason redact.SafeText
	Error            *SafeError
}

type AuthorizedResultCache interface {
	Lookup(context.Context, ValidatedCall) (Result, bool, error)
	Store(context.Context, ValidatedCall, Result) error
}

type readCacheEntry struct {
	key          ReadCacheKey
	dependencies []FileVersion
	value        CachedReadResult
	bytes        int64
}

// ReadCache is owned by one task. Its lock protects concurrent tool batches;
// CloneEmpty creates another owner with no shared entries or LRU state.
type ReadCache struct {
	mu      sync.Mutex
	limits  ReadCacheLimits
	factory *ResultFactory
	entries map[ReadCacheKey]*list.Element
	lru     *list.List
	bytes   int64
	closed  bool
}

func NewReadCache(limits ReadCacheLimits, factory *ResultFactory) (*ReadCache, error) {
	if limits.MaxEntries <= 0 {
		return nil, errors.New("read cache max entries must be positive")
	}
	if limits.MaxBytes <= 0 || limits.MaxValueBytes <= 0 || limits.MaxValueBytes > limits.MaxBytes {
		return nil, errors.New("read cache byte limits are invalid")
	}
	if limits.MaxDependenciesPerEntry <= 0 {
		return nil, errors.New("read cache dependency limit must be positive")
	}
	if factory == nil || factory.redactor == nil {
		return nil, errors.New("read cache result factory is unavailable")
	}
	return &ReadCache{
		limits:  limits,
		factory: factory,
		entries: make(map[ReadCacheKey]*list.Element),
		lru:     list.New(),
	}, nil
}

func (c *ReadCache) Get(key ReadCacheKey, current []FileVersion) (CachedReadResult, bool) {
	if c == nil || validateReadCacheKey(key) != nil {
		return CachedReadResult{}, false
	}
	dependencies, err := normalizeFileVersions(current, c.limits.MaxDependenciesPerEntry)
	if err != nil {
		return CachedReadResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return CachedReadResult{}, false
	}
	element, ok := c.entries[key]
	if !ok {
		return CachedReadResult{}, false
	}
	entry, ok := element.Value.(*readCacheEntry)
	if !ok || entry == nil || !fileVersionsEqual(entry.dependencies, dependencies) {
		c.removeElement(element)
		return CachedReadResult{}, false
	}
	c.lru.MoveToFront(element)
	return cloneCachedReadResult(entry.value), true
}

func (c *ReadCache) Put(key ReadCacheKey, dependencies []FileVersion, value CachedReadResult) error {
	if c == nil {
		return errors.New("read cache is unavailable")
	}
	if err := validateReadCacheKey(key); err != nil {
		return err
	}
	canonicalDependencies, err := normalizeFileVersions(dependencies, c.limits.MaxDependenciesPerEntry)
	if err != nil {
		return err
	}
	value = cloneCachedReadResult(value)
	if _, err := c.materialize("cache-validation", key.Tool, value); err != nil {
		return fmt.Errorf("read cache safe result is invalid: %w", err)
	}
	valueBytes := cachedReadResultSize(value)
	if valueBytes < 0 || valueBytes > c.limits.MaxValueBytes {
		return errors.New("read cache value exceeds max value bytes")
	}
	entryBytes, err := readCacheEntrySize(key, canonicalDependencies, valueBytes)
	if err != nil || entryBytes > c.limits.MaxBytes {
		return errors.New("read cache entry exceeds max bytes")
	}
	entry := &readCacheEntry{
		key:          key,
		dependencies: canonicalDependencies,
		value:        value,
		bytes:        entryBytes,
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("read cache is closed")
	}
	if existing, ok := c.entries[key]; ok {
		c.removeElement(existing)
	}
	// Evict before addition so byte accounting cannot overflow even when
	// construction limits are near MaxInt64.
	for len(c.entries) >= c.limits.MaxEntries || c.bytes > c.limits.MaxBytes-entry.bytes {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.removeElement(oldest)
	}
	element := c.lru.PushFront(entry)
	c.entries[key] = element
	c.bytes += entry.bytes
	return nil
}

func (c *ReadCache) Lookup(ctx context.Context, validated ValidatedCall) (Result, bool, error) {
	if c == nil {
		return Result{}, false, errors.New("read cache is unavailable")
	}
	if ctx == nil {
		return Result{}, false, errors.New("read cache context is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, false, err
	}
	if !isCacheableReadTool(validated.Call.Name) {
		return Result{}, false, nil
	}
	key, dependencies, err := c.identity(ctx, validated)
	if err != nil {
		return Result{}, false, err
	}
	template, ok := c.Get(key, dependencies)
	if !ok {
		return Result{}, false, nil
	}
	result, err := c.materialize(validated.Call.ID, validated.Call.Name, template)
	if err != nil {
		return Result{}, false, err
	}
	return result, true, nil
}

func (c *ReadCache) Store(ctx context.Context, validated ValidatedCall, result Result) error {
	if c == nil {
		return errors.New("read cache is unavailable")
	}
	if ctx == nil {
		return errors.New("read cache context is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !isCacheableReadTool(validated.Call.Name) {
		return nil
	}
	if result.CallID != validated.Call.ID || result.Name != validated.Call.Name {
		return errors.New("read cache result does not match the validated call")
	}
	value, err := cachedReadResultFromResult(result)
	if err != nil {
		return err
	}
	key, dependencies, err := c.identity(ctx, validated)
	if err != nil {
		return err
	}
	return c.Put(key, dependencies, value)
}

func (c *ReadCache) CloneEmpty() *ReadCache {
	if c == nil {
		return nil
	}
	clone, err := NewReadCache(c.limits, c.factory)
	if err != nil {
		return nil
	}
	return clone
}

func (c *ReadCache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.entries = make(map[ReadCacheKey]*list.Element)
	c.lru.Init()
	c.bytes = 0
}

func (c *ReadCache) removeElement(element *list.Element) {
	if c == nil || element == nil {
		return
	}
	entry, _ := element.Value.(*readCacheEntry)
	c.lru.Remove(element)
	if entry == nil {
		return
	}
	delete(c.entries, entry.key)
	if entry.bytes >= c.bytes {
		c.bytes = 0
	} else {
		c.bytes -= entry.bytes
	}
}

func (c *ReadCache) identity(ctx context.Context, validated ValidatedCall) (ReadCacheKey, []FileVersion, error) {
	if validated.Call.Name == "" || validated.Tool == nil || validated.Tool.Name() != validated.Call.Name || validated.executor == nil || validated.executor.Name() != validated.Call.Name {
		return ReadCacheKey{}, nil, errors.New("validated read call is inconsistent")
	}
	canonicalArguments := validated.CanonicalArguments()
	if len(canonicalArguments) == 0 {
		return ReadCacheKey{}, nil, errors.New("validated read arguments are unavailable")
	}
	rootFingerprint, err := readRootFingerprint(ctx, validated)
	if err != nil {
		return ReadCacheKey{}, nil, err
	}
	hash := sha256.New()
	writeFingerprintPart(hash, []byte("read-cache-key-v1"))
	writeFingerprintPart(hash, []byte(validated.Call.Name))
	writeFingerprintPart(hash, canonicalArguments)
	writeFingerprintPart(hash, []byte(rootFingerprint))
	key := ReadCacheKey{Tool: validated.Call.Name, ArgumentsFingerprint: hex.EncodeToString(hash.Sum(nil))}
	dependencies, err := collectReadDependencies(ctx, validated, c.limits.MaxDependenciesPerEntry)
	if err != nil {
		return ReadCacheKey{}, nil, err
	}
	return key, dependencies, nil
}

func (c *ReadCache) materialize(callID, name string, cached CachedReadResult) (Result, error) {
	if c == nil || c.factory == nil {
		return Result{}, errors.New("read cache result factory is unavailable")
	}
	var resultError *Error
	if cached.Error != nil {
		resultError = &Error{Code: cached.Error.Code, Message: cached.Error.Message.Text(), Recoverable: cached.Error.Recoverable}
	}
	return c.factory.Build(ResultFactoryInput{
		CallID:           callID,
		Name:             name,
		State:            cached.State,
		Status:           cached.Status,
		Summary:          cached.Summary.Text(),
		Preview:          cached.Preview.Text(),
		Artifact:         cloneArtifactRef(cached.Artifact),
		CapturedBytes:    cached.CapturedBytes,
		Truncated:        cached.Truncated,
		TruncationReason: cached.TruncationReason.Text(),
		Error:            resultError,
	})
}

func cachedReadResultFromResult(result Result) (CachedReadResult, error) {
	if !result.ExecutionState().Valid() || !result.ExecutionState().CanProduceResult() || !result.Status.valid() {
		return CachedReadResult{}, errors.New("read cache accepts only ResultFactory results")
	}
	view := result.UserView()
	meta := result.OutputMeta()
	if view.State != result.ExecutionState() || view.Status != result.Status {
		return CachedReadResult{}, errors.New("read cache result projections are inconsistent")
	}
	return CachedReadResult{
		State:            view.State,
		Status:           view.Status,
		Summary:          view.Summary,
		Preview:          view.Preview,
		Artifact:         cloneArtifactRef(view.Artifact),
		CapturedBytes:    meta.CapturedBytes,
		Truncated:        view.Truncated,
		TruncationReason: view.TruncationReason,
		Error:            cloneSafeError(view.Error),
	}, nil
}

func cloneCachedReadResult(value CachedReadResult) CachedReadResult {
	value.Artifact = cloneArtifactRef(value.Artifact)
	value.Error = cloneSafeError(value.Error)
	return value
}

func validateReadCacheKey(key ReadCacheKey) error {
	if !isCacheableReadTool(key.Tool) {
		return fmt.Errorf("tool %q is not read-cacheable", key.Tool)
	}
	if key.ArgumentsFingerprint == "" || len(key.ArgumentsFingerprint) > maxReadCacheFingerprintBytes {
		return errors.New("read cache arguments fingerprint is invalid")
	}
	return nil
}

func isCacheableReadTool(name string) bool {
	switch name {
	case "Read", "Glob", "Grep":
		return true
	default:
		return false
	}
}

func normalizeFileVersions(input []FileVersion, maxDependencies int) ([]FileVersion, error) {
	if len(input) > maxDependencies {
		return nil, errors.New("read cache dependency limit exceeded")
	}
	versions := append([]FileVersion(nil), input...)
	sort.Slice(versions, func(i, j int) bool { return versions[i].Path < versions[j].Path })
	for index, version := range versions {
		if version.Path == "" || version.Digest == "" || version.Size < -1 {
			return nil, errors.New("read cache dependency is invalid")
		}
		if index > 0 && versions[index-1].Path == version.Path {
			return nil, errors.New("read cache dependency path is duplicated")
		}
	}
	return versions, nil
}

func fileVersionsEqual(left, right []FileVersion) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cachedReadResultSize(value CachedReadResult) int64 {
	size := int64(64)
	for _, text := range []string{value.Summary.Text(), value.Preview.Text(), value.TruncationReason.Text()} {
		size = saturatingAdd(size, int64(len(text)))
	}
	if value.Artifact != nil {
		size = saturatingAdd(size, int64(len(value.Artifact.ID))+64)
	}
	if value.Error != nil {
		size = saturatingAdd(size, int64(len(value.Error.Code)+len(value.Error.Message.Text()))+8)
	}
	return size
}

func readCacheEntrySize(key ReadCacheKey, dependencies []FileVersion, valueBytes int64) (int64, error) {
	total := valueBytes
	add := func(part int64) error {
		if part < 0 || total > math.MaxInt64-part {
			return errors.New("read cache byte accounting overflow")
		}
		total += part
		return nil
	}
	for _, part := range []int64{int64(len(key.Tool)), int64(len(key.ArgumentsFingerprint)), 32} {
		if err := add(part); err != nil {
			return 0, err
		}
	}
	for _, dependency := range dependencies {
		for _, part := range []int64{int64(len(dependency.Path)), int64(len(dependency.Digest)), 32} {
			if err := add(part); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func saturatingAdd(left, right int64) int64 {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func readRootFingerprint(ctx context.Context, validated ValidatedCall) (string, error) {
	if validated.workingDirectory == nil {
		return "", errors.New("validated read root identity is unavailable")
	}
	workingIdentity, err := validated.workingDirectory.MarshalBinary()
	if err != nil {
		return "", errors.New("validated read root identity is invalid")
	}
	hash := sha256.New()
	writeFingerprintPart(hash, []byte("read-root-v1"))
	writeFingerprintPart(hash, workingIdentity)

	requested, ok := ReadScopeFromContext(ctx)
	if ok {
		var normalized ReadScope
		if requested.pinned {
			normalized, err = NewPinnedReadScope(requested.ProjectRoot, requested.ExtraRoots)
		} else {
			normalized, err = NewReadScope(requested.ProjectRoot, requested.ExtraRoots)
		}
		if err != nil {
			return "", errors.New("read cache scope is invalid")
		}
		for _, root := range readRoots(normalized) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			opened, openErr := safefs.Bootstrap(root.path, safefs.Policy{})
			if openErr != nil || opened.Root == nil {
				return "", errors.New("read cache root identity is unavailable")
			}
			identity, marshalErr := opened.Root.Identity().MarshalBinary()
			closeErr := opened.Root.Close()
			if marshalErr != nil || closeErr != nil {
				return "", errors.New("read cache root identity is unavailable")
			}
			writeFingerprintPart(hash, identity)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeFingerprintPart(writer io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func collectReadDependencies(ctx context.Context, validated ValidatedCall, maxDependencies int) ([]FileVersion, error) {
	arguments := make(map[string]any)
	decoder := json.NewDecoder(strings.NewReader(string(validated.CanonicalArguments())))
	decoder.UseNumber()
	if err := decoder.Decode(&arguments); err != nil {
		return nil, errors.New("read cache arguments are unavailable")
	}
	collector := &fileVersionCollector{ctx: ctx, max: maxDependencies, versions: make(map[string]FileVersion)}
	switch validated.Call.Name {
	case "Read":
		path, _ := arguments["path"].(string)
		target, err := readCacheTarget(validated.executionRootPath, path)
		if err != nil {
			return nil, err
		}
		if err := collector.add(target); err != nil {
			return nil, err
		}
	case "Grep":
		path, _ := arguments["path"].(string)
		if path == "" {
			path = "."
		}
		target, err := readCacheTarget(validated.executionRootPath, path)
		if err != nil {
			return nil, err
		}
		if err := collector.addTree(target); err != nil {
			return nil, err
		}
	case "Glob":
		pattern, _ := arguments["pattern"].(string)
		scope, err := readCacheScope(ctx, validated.executionRootPath)
		if err != nil {
			return nil, err
		}
		patterns, err := scopedGlobPatterns(scope, pattern)
		if err != nil {
			return nil, err
		}
		for _, candidate := range patterns {
			base, err := readCacheTarget(candidate.root, globWalkBase(candidate.relative))
			if err != nil {
				return nil, err
			}
			if err := collector.addTree(base); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("tool %q is not read-cacheable", validated.Call.Name)
	}
	return collector.sorted()
}

func readCacheScope(ctx context.Context, projectRoot string) (ReadScope, error) {
	requested, ok := ReadScopeFromContext(ctx)
	if !ok {
		return NewReadScope(projectRoot, nil)
	}
	if requested.pinned {
		return NewPinnedReadScope(requested.ProjectRoot, requested.ExtraRoots)
	}
	return NewReadScope(requested.ProjectRoot, requested.ExtraRoots)
}

func readCacheTarget(root, requested string) (string, error) {
	if root == "" || strings.TrimSpace(requested) == "" {
		return "", errors.New("read cache target is invalid")
	}
	candidate := requested
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	resolved, err := resolveWithExistingAncestor(candidate)
	if err != nil {
		return "", errors.New("read cache target is unavailable")
	}
	if !isPathInsideRoot(root, resolved) {
		return "", errors.New("read cache target is outside the read root")
	}
	return resolved, nil
}

type fileVersionCollector struct {
	ctx      context.Context
	max      int
	versions map[string]FileVersion
}

func (c *fileVersionCollector) addTree(root string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return c.add(root)
	}
	if err != nil {
		return errors.New("read cache dependency is unavailable")
	}
	if !info.IsDir() {
		return c.add(root)
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("read cache dependency walk failed")
		}
		if err := c.ctx.Err(); err != nil {
			return err
		}
		return c.add(path)
	})
}

func (c *fileVersionCollector) add(path string) error {
	path = filepath.Clean(path)
	if _, exists := c.versions[path]; exists {
		return nil
	}
	if len(c.versions) >= c.max {
		return errors.New("read cache dependency limit exceeded")
	}
	version, err := snapshotFileVersion(c.ctx, path)
	if err != nil {
		return err
	}
	c.versions[path] = version
	return nil
}

func (c *fileVersionCollector) sorted() ([]FileVersion, error) {
	versions := make([]FileVersion, 0, len(c.versions))
	for _, version := range c.versions {
		versions = append(versions, version)
	}
	return normalizeFileVersions(versions, c.max)
}

func snapshotFileVersion(ctx context.Context, path string) (FileVersion, error) {
	if err := ctx.Err(); err != nil {
		return FileVersion{}, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		digest := sha256.Sum256([]byte("missing"))
		return FileVersion{Path: filepath.Clean(path), Size: -1, Digest: hex.EncodeToString(digest[:])}, nil
	}
	if err != nil {
		return FileVersion{}, errors.New("read cache dependency stat failed")
	}
	hash := sha256.New()
	switch {
	case info.Mode().IsRegular():
		_, _ = hash.Write([]byte("file\x00"))
		file, err := os.Open(path)
		if err != nil {
			return FileVersion{}, errors.New("read cache dependency open failed")
		}
		buffer := make([]byte, 32<<10)
		for {
			if err := ctx.Err(); err != nil {
				_ = file.Close()
				return FileVersion{}, err
			}
			count, readErr := file.Read(buffer)
			if count > 0 {
				_, _ = hash.Write(buffer[:count])
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					_ = file.Close()
					return FileVersion{}, errors.New("read cache dependency read failed")
				}
				break
			}
		}
		if err := file.Close(); err != nil {
			return FileVersion{}, errors.New("read cache dependency close failed")
		}
	case info.IsDir():
		_, _ = hash.Write([]byte("directory\x00"))
		entries, err := os.ReadDir(path)
		if err != nil {
			return FileVersion{}, errors.New("read cache dependency directory read failed")
		}
		for _, entry := range entries {
			writeFingerprintPart(hash, []byte(entry.Name()))
			writeFingerprintPart(hash, []byte(entry.Type().String()))
		}
	case info.Mode()&os.ModeSymlink != 0:
		_, _ = hash.Write([]byte("symlink\x00"))
		target, err := os.Readlink(path)
		if err != nil {
			return FileVersion{}, errors.New("read cache dependency symlink read failed")
		}
		writeFingerprintPart(hash, []byte(target))
	default:
		_, _ = hash.Write([]byte("other\x00" + info.Mode().String()))
	}
	return FileVersion{
		Path:    filepath.Clean(path),
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
		Digest:  hex.EncodeToString(hash.Sum(nil)),
	}, nil
}
