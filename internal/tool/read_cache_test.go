package tool

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/redact"
)

func TestReadCacheLRUAndDependencyInvalidation(t *testing.T) {
	factory := newReadCacheTestFactory(t)
	cache, err := NewReadCache(ReadCacheLimits{
		MaxEntries:              2,
		MaxBytes:                64 << 10,
		MaxValueBytes:           16 << 10,
		MaxDependenciesPerEntry: 4,
	}, factory)
	if err != nil {
		t.Fatal(err)
	}
	value := readCacheTestValue(t, factory, "old-call", "summary", "preview")
	keyA := ReadCacheKey{Tool: "Read", ArgumentsFingerprint: strings.Repeat("a", 64)}
	keyB := ReadCacheKey{Tool: "Glob", ArgumentsFingerprint: strings.Repeat("b", 64)}
	keyC := ReadCacheKey{Tool: "Grep", ArgumentsFingerprint: strings.Repeat("c", 64)}
	deps := []FileVersion{
		{Path: "/root/z.txt", Size: 2, ModTime: 20, Digest: "z-digest"},
		{Path: "/root/a.txt", Size: 1, ModTime: 10, Digest: "a-digest"},
	}
	for _, key := range []ReadCacheKey{keyA, keyB} {
		if err := cache.Put(key, deps, value); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := cache.Get(keyA, []FileVersion{deps[1], deps[0]}); !ok || got.Preview.Text() != "preview" {
		t.Fatalf("cache miss for reordered identical dependencies: %#v, %t", got, ok)
	}
	if err := cache.Put(keyC, deps, value); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get(keyB, deps); ok {
		t.Fatal("least-recently-used entry was not evicted")
	}
	if _, ok := cache.Get(keyA, deps); !ok {
		t.Fatal("recently used entry was evicted")
	}

	changed := append([]FileVersion(nil), deps...)
	changed[0].Digest = "changed"
	if _, ok := cache.Get(keyA, changed); ok {
		t.Fatal("dependency digest change did not invalidate the entry")
	}
	if _, ok := cache.Get(keyA, deps); ok {
		t.Fatal("stale entry remained cached after dependency mismatch")
	}
}

func TestReadCacheRejectsLimitsAndCloneIsTaskLocal(t *testing.T) {
	factory := newReadCacheTestFactory(t)
	for name, limits := range map[string]ReadCacheLimits{
		"zero entries":      {MaxBytes: 1, MaxValueBytes: 1, MaxDependenciesPerEntry: 1},
		"zero bytes":        {MaxEntries: 1, MaxValueBytes: 1, MaxDependenciesPerEntry: 1},
		"value over total":  {MaxEntries: 1, MaxBytes: 1, MaxValueBytes: 2, MaxDependenciesPerEntry: 1},
		"zero dependencies": {MaxEntries: 1, MaxBytes: 1, MaxValueBytes: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewReadCache(limits, factory); err == nil {
				t.Fatal("invalid limits were accepted")
			}
		})
	}

	cache, err := NewReadCache(ReadCacheLimits{MaxEntries: 2, MaxBytes: 4 << 10, MaxValueBytes: 1024, MaxDependenciesPerEntry: 1}, factory)
	if err != nil {
		t.Fatal(err)
	}
	key := ReadCacheKey{Tool: "Read", ArgumentsFingerprint: "fingerprint"}
	value := readCacheTestValue(t, factory, "old-call", "summary", "preview")
	tooManyDependencies := []FileVersion{{Path: "/a", Digest: "a"}, {Path: "/b", Digest: "b"}}
	if err := cache.Put(key, tooManyDependencies, value); err == nil {
		t.Fatal("dependency limit was not enforced")
	}
	if _, ok := cache.Get(key, tooManyDependencies); ok {
		t.Fatal("over-limit dependency lookup hit")
	}

	if err := cache.Put(key, []FileVersion{{Path: "/a", Digest: "a"}}, value); err != nil {
		t.Fatal(err)
	}
	clone := cache.CloneEmpty()
	if clone == nil {
		t.Fatal("CloneEmpty returned nil")
	}
	if _, ok := clone.Get(key, []FileVersion{{Path: "/a", Digest: "a"}}); ok {
		t.Fatal("CloneEmpty shared entries with its source task")
	}
	cache.Close()
	if _, ok := cache.Get(key, []FileVersion{{Path: "/a", Digest: "a"}}); ok {
		t.Fatal("closed cache retained readable entries")
	}
	if err := cache.Put(key, []FileVersion{{Path: "/a", Digest: "a"}}, value); err == nil {
		t.Fatal("closed cache accepted a new value")
	}
}

func TestReadCacheRejectsOversizedSafeTemplateWithoutEvictingExisting(t *testing.T) {
	factory := newReadCacheTestFactory(t)
	base := readCacheTestValue(t, factory, "old-call", "ok", "tiny")
	baseBytes := cachedReadResultSize(base)
	cache, err := NewReadCache(ReadCacheLimits{
		MaxEntries:              2,
		MaxBytes:                baseBytes*4 + 1024,
		MaxValueBytes:           baseBytes + 8,
		MaxDependenciesPerEntry: 2,
	}, factory)
	if err != nil {
		t.Fatal(err)
	}
	key := ReadCacheKey{Tool: "Read", ArgumentsFingerprint: "small"}
	deps := []FileVersion{{Path: "/a", Digest: "a"}}
	if err := cache.Put(key, deps, base); err != nil {
		t.Fatal(err)
	}
	oversized := readCacheTestValue(t, factory, "other", strings.Repeat("s", int(baseBytes)+32), "preview")
	if err := cache.Put(ReadCacheKey{Tool: "Read", ArgumentsFingerprint: "large"}, deps, oversized); err == nil {
		t.Fatal("oversized safe template was cached")
	}
	if _, ok := cache.Get(key, deps); !ok {
		t.Fatal("rejected oversized value evicted an existing entry")
	}
}

func TestAuthorizedResultCacheRematerializesCurrentCallIDAndInvalidatesFileChange(t *testing.T) {
	root := t.TempDir()
	file := root + "/input.txt"
	if err := writeReadCacheTestFile(file, "before"); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, root, time.Second, 1024)
	factory := newReadCacheTestFactory(t)
	cache, err := NewReadCache(ReadCacheLimits{
		MaxEntries:              4,
		MaxBytes:                64 << 10,
		MaxValueBytes:           16 << 10,
		MaxDependenciesPerEntry: 32,
	}, factory)
	if err != nil {
		t.Fatal(err)
	}
	oldCall := Call{ID: "old-call", Name: "Read", ArgumentsJSON: `{"path":"input.txt"}`}
	validatedOld, err := executor.PrepareCall(context.Background(), oldCall)
	if err != nil {
		t.Fatal(err)
	}
	ref := &artifact.Ref{
		ID:        strings.Repeat("a", 64),
		Bytes:     12,
		CreatedAt: time.Unix(123, 0).UTC(),
		Available: true,
		Complete:  true,
	}
	original, err := factory.Build(ResultFactoryInput{
		CallID:           oldCall.ID,
		Name:             oldCall.Name,
		State:            Completed,
		Status:           StatusError,
		Summary:          "summary secret-canary",
		Preview:          "preview secret-canary",
		Artifact:         ref,
		CapturedBytes:    ref.Bytes,
		Truncated:        true,
		TruncationReason: string(CaptureTruncatedInline),
		Error:            &Error{Code: ErrNotFound, Message: "error secret-canary", Recoverable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(context.Background(), validatedOld, original); err != nil {
		t.Fatal(err)
	}

	newCall := oldCall
	newCall.ID = "current-call"
	validatedCurrent, err := executor.PrepareCall(context.Background(), newCall)
	if err != nil {
		t.Fatal(err)
	}
	hit, ok, err := cache.Lookup(context.Background(), validatedCurrent)
	if err != nil || !ok {
		t.Fatalf("authorized result lookup missed: ok=%t err=%v", ok, err)
	}
	if hit.CallID != newCall.ID || hit.Name != newCall.Name || hit.CallID == original.CallID {
		t.Fatalf("cache reused the old invocation identity: %#v", hit)
	}
	if hit.ExecutionState() != original.ExecutionState() || hit.Status != original.Status || hit.Summary != original.Summary || hit.Content != original.Content || hit.Truncated != original.Truncated {
		t.Fatalf("safe result template was not preserved: hit=%#v original=%#v", hit, original)
	}
	if hit.Error == nil || original.Error == nil || !reflect.DeepEqual(hit.Error, original.Error) {
		t.Fatalf("safe error was not preserved: %#v / %#v", hit.Error, original.Error)
	}
	if strings.Contains(hit.ModelContent().Text(), "secret-canary") || strings.Contains(hit.PersistedContent().Text(), "secret-canary") {
		t.Fatal("cached result exposed pre-redaction output")
	}
	meta := hit.OutputMeta()
	if meta.Artifact == nil || meta.Artifact == ref || meta.Artifact.ID != ref.ID || meta.CapturedBytes != ref.Bytes {
		t.Fatalf("artifact metadata was not independently rematerialized: %#v", meta)
	}

	if err := writeReadCacheTestFile(file, "after"); err != nil {
		t.Fatal(err)
	}
	validatedAfterChange, err := executor.PrepareCall(context.Background(), Call{ID: "after-change", Name: "Read", ArgumentsJSON: oldCall.ArgumentsJSON})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.Lookup(context.Background(), validatedAfterChange); err != nil || ok {
		t.Fatalf("changed dependency did not produce a clean miss: ok=%t err=%v", ok, err)
	}
}

func newReadCacheTestFactory(t *testing.T) *ResultFactory {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret("secret-canary")
	factory, err := NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func readCacheTestValue(t *testing.T, factory *ResultFactory, callID, summary, preview string) CachedReadResult {
	t.Helper()
	result, err := factory.Build(ResultFactoryInput{
		CallID:  callID,
		Name:    "Read",
		State:   Completed,
		Status:  StatusSuccess,
		Summary: summary,
		Preview: preview,
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := cachedReadResultFromResult(result)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func writeReadCacheTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
