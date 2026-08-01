package safefs

import (
	"bytes"
	"context"
	"errors"
	"go/build/constraint"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/budget"
)

func TestBootstrapSeparatesCapabilities(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "permissions"), 0o700); err != nil {
		t.Fatal("create protected parent failed")
	}
	result := mustBootstrap(t, rootPath, Policy{ProtectedSlots: []string{"permissions/rules.json"}})
	defer mustClose(t, result.Root)

	if result.Root.Identity() == (Identity{}) {
		t.Fatal("bootstrap returned a zero root identity")
	}
	reopened := mustBootstrap(t, rootPath, Policy{ProtectedSlots: []string{"permissions/rules.json"}})
	defer mustClose(t, reopened.Root)
	if !bytes.Equal(mustIdentityBytes(t, result.Root.Identity()), mustIdentityBytes(t, reopened.Root.Identity())) {
		t.Fatal("the same opened root did not retain a stable canonical identity")
	}
	ordinary := result.Capabilities.Ordinary()
	protected := result.Capabilities.Protected()
	if ordinary == (Capability{}) || protected == (Capability{}) || ordinary == protected {
		t.Fatal("bootstrap did not return two distinct non-zero capabilities")
	}

	ordinaryBinding := mustBind(t, result.Root, "notes.txt")
	protectedBinding := mustBind(t, result.Root, "permissions/rules.json")
	if err := result.Root.authorizeWrite(ordinary, ordinaryBinding); err != nil {
		t.Fatal("ordinary capability did not authorize an ordinary slot")
	}
	if err := result.Root.authorizeWrite(protected, protectedBinding); err != nil {
		t.Fatal("protected capability did not authorize a protected slot")
	}
	if err := result.Root.authorizeWrite(protected, ordinaryBinding); err == nil {
		t.Fatal("protected capability became an ordinary write superset")
	}

	rootType := reflect.TypeOf(result.Root)
	for _, forbidden := range []string{"Capabilities", "Ordinary", "Protected", "IssueCapability", "UpgradeCapability"} {
		if _, exists := rootType.MethodByName(forbidden); exists {
			t.Fatal("Root exposes a capability signing or upgrade method")
		}
	}
	for _, value := range []any{Identity{}, Binding{}, Capability{}, Capabilities{}} {
		typeOfValue := reflect.TypeOf(value)
		for index := 0; index < typeOfValue.NumField(); index++ {
			if typeOfValue.Field(index).IsExported() {
				t.Fatal("an unforgeable safefs value exposes writable fields")
			}
		}
	}
}

func TestRootCloseIsIdempotent(t *testing.T) {
	root := mustBootstrap(t, t.TempDir(), Policy{}).Root
	if err := root.Close(); err != nil {
		t.Fatal("first Root close failed")
	}
	if err := root.Close(); err != nil {
		t.Fatal("repeated Root close changed the final result")
	}
	if root.Identity() != (Identity{}) {
		t.Fatal("closed Root retained a usable identity")
	}
	if _, err := root.Bind("target"); err == nil {
		t.Fatal("closed Root accepted a new binding")
	}
	var nilRoot *Root
	if err := nilRoot.Close(); err != nil {
		t.Fatal("nil Root close was not idempotent")
	}
}

func TestZeroAndCrossRootCapabilitiesFail(t *testing.T) {
	rootPath := t.TempDir()
	first := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, first.Root)
	second := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, second.Root)

	firstBinding := mustBind(t, first.Root, "target.txt")
	secondBinding := mustBind(t, second.Root, "target.txt")
	if err := first.Root.authorizeWrite(first.Capabilities.Ordinary(), firstBinding); err != nil {
		t.Fatal("same-root capability was rejected")
	}
	for _, attempt := range []struct {
		root       *Root
		capability Capability
		binding    Binding
	}{
		{first.Root, Capability{}, firstBinding},
		{first.Root, Capabilities{}.Ordinary(), firstBinding},
		{first.Root, Capabilities{}.Protected(), firstBinding},
		{second.Root, first.Capabilities.Ordinary(), secondBinding},
		{second.Root, first.Capabilities.Protected(), secondBinding},
		{first.Root, first.Capabilities.Ordinary(), secondBinding},
	} {
		if err := attempt.root.authorizeWrite(attempt.capability, attempt.binding); err == nil {
			t.Fatal("zero or cross-root capability was accepted")
		}
	}
}

func TestBindingTracksExistingAndMissingTargets(t *testing.T) {
	rootPath := t.TempDir()
	existingPath := filepath.Join(rootPath, "existing.txt")
	if err := os.WriteFile(existingPath, []byte("first"), 0o600); err != nil {
		t.Fatal("create existing target failed")
	}
	parentPath := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal("create missing-target parent failed")
	}
	result := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, result.Root)

	existing := mustBind(t, result.Root, "existing.txt")
	missing := mustBind(t, result.Root, "parent/missing.txt")
	reopened := mustBootstrap(t, rootPath, Policy{ProtectedSlots: []string{"existing.txt", "parent/missing.txt"}})
	defer mustClose(t, reopened.Root)
	reopenedMissing := mustBind(t, reopened.Root, "parent/missing.txt")
	reopenedExisting := mustBind(t, reopened.Root, "existing.txt")
	if !bytes.Equal(mustBindingBytes(t, existing), mustBindingBytes(t, reopenedExisting)) {
		t.Fatal("existing-object canonical binding changed across bootstrap or policy")
	}
	if !bytes.Equal(mustBindingBytes(t, missing), mustBindingBytes(t, reopenedMissing)) {
		t.Fatal("missing-target canonical binding changed across bootstrap, policy, or bind order")
	}
	if repeated := mustBind(t, result.Root, "existing.txt"); repeated != existing {
		t.Fatal("repeated existing-object binding was unstable")
	}
	if err := os.WriteFile(existingPath, []byte("changed in place"), 0o600); err != nil {
		t.Fatal("update existing target failed")
	}
	if changedContent := mustBind(t, result.Root, "existing.txt"); changedContent != existing {
		t.Fatal("content-only update changed the object binding")
	}
	replacementPath := filepath.Join(rootPath, "replacement.txt")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal("create replacement target failed")
	}
	if err := os.Remove(existingPath); err != nil {
		t.Fatal("remove old target failed")
	}
	if err := os.Rename(replacementPath, existingPath); err != nil {
		t.Fatal("publish replacement target failed")
	}
	if replaced := mustBind(t, result.Root, "existing.txt"); replaced == existing {
		t.Fatal("replacement object retained the previous binding")
	}

	if repeated := mustBind(t, result.Root, "parent/missing.txt"); repeated != missing {
		t.Fatal("repeated missing-target binding was unstable")
	}
	if err := os.WriteFile(filepath.Join(parentPath, "missing.txt"), []byte("created"), 0o600); err != nil {
		t.Fatal("create previously missing target failed")
	}
	existingAfterCreate := mustBind(t, result.Root, "parent/missing.txt")
	if existingAfterCreate == missing {
		t.Fatal("missing and existing target bindings collided")
	}

	if err := os.Remove(filepath.Join(parentPath, "missing.txt")); err != nil {
		t.Fatal("remove created target failed")
	}
	oldParentPath := filepath.Join(rootPath, "parent-old")
	if err := os.Rename(parentPath, oldParentPath); err != nil {
		t.Fatal("move old parent failed")
	}
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal("create replacement parent failed")
	}
	if replacedParent := mustBind(t, result.Root, "parent/missing.txt"); replacedParent == missing {
		t.Fatal("missing binding ignored parent identity replacement")
	}

	existingBytes, err := existing.MarshalBinary()
	if err != nil {
		t.Fatal("marshal existing binding failed")
	}
	missingBytes, err := missing.MarshalBinary()
	if err != nil {
		t.Fatal("marshal missing binding failed")
	}
	if bytes.Equal(existingBytes, missingBytes) {
		t.Fatal("existing and missing canonical bindings collided")
	}
	existingBytes[0] ^= 0xff
	secondCopy, err := existing.MarshalBinary()
	if err != nil || bytes.Equal(existingBytes, secondCopy) {
		t.Fatal("binding canonical bytes were not returned as an immutable copy")
	}
}

func TestCapabilityCannotWriteProtectedSlot(t *testing.T) {
	rootPath := t.TempDir()
	protectedParent := filepath.Join(rootPath, "permissions")
	if err := os.Mkdir(protectedParent, 0o700); err != nil {
		t.Fatal("create protected directory failed")
	}
	if err := os.WriteFile(filepath.Join(protectedParent, "rules.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal("create protected target failed")
	}
	aliasParent := filepath.Join(rootPath, "aliases")
	if err := os.Mkdir(aliasParent, 0o700); err != nil {
		t.Fatal("create alias parent failed")
	}
	if err := os.Link(filepath.Join(protectedParent, "rules.json"), filepath.Join(aliasParent, "rules-link.json")); err != nil {
		t.Fatal("create protected-object hard link failed")
	}
	if err := os.WriteFile(filepath.Join(rootPath, "movable.txt"), []byte("move"), 0o600); err != nil {
		t.Fatal("create movable target failed")
	}
	slots := []string{"permissions/rules.json", "permissions/new.json"}
	policy := Policy{ProtectedSlots: slots}
	result := mustBootstrap(t, rootPath, policy)
	defer mustClose(t, result.Root)

	// Bootstrap must retain an immutable policy snapshot.
	slots[0] = "ordinary.txt"
	policy.ProtectedSlots[1] = "ordinary-two.txt"

	protectedExisting := mustBind(t, result.Root, "permissions/rules.json")
	protectedMissing := mustBind(t, result.Root, "permissions/new.json")
	protectedAncestor := mustBind(t, result.Root, "permissions")
	protectedAlias := mustBind(t, result.Root, "aliases/rules-link.json")
	ordinary := mustBind(t, result.Root, "ordinary.txt")
	movable := mustBind(t, result.Root, "movable.txt")
	ordinaryCapability := result.Capabilities.Ordinary()
	protectedCapability := result.Capabilities.Protected()
	for _, binding := range []Binding{protectedExisting, protectedMissing} {
		if err := result.Root.authorizeWrite(ordinaryCapability, binding); err == nil {
			t.Fatal("ordinary capability authorized a protected slot")
		}
		if err := result.Root.authorizeWrite(protectedCapability, binding); err != nil {
			t.Fatal("protected capability rejected an explicit protected slot")
		}
	}
	if err := result.Root.authorizeWrite(ordinaryCapability, protectedAlias); err == nil {
		t.Fatal("ordinary capability authorized a hard-link alias of a protected object")
	}
	if err := result.Root.authorizeWrite(protectedCapability, protectedAlias); err == nil {
		t.Fatal("protected capability authorized an unlisted hard-link location")
	}
	caseAliasPath := filepath.Join(rootPath, "PERMISSIONS", "RULES.JSON")
	if aliasInfo, err := os.Stat(caseAliasPath); err == nil {
		exactInfo, exactErr := os.Stat(filepath.Join(protectedParent, "rules.json"))
		if exactErr != nil || !os.SameFile(aliasInfo, exactInfo) {
			t.Fatal("case alias resolved to an unexpected object")
		}
		caseAlias := mustBind(t, result.Root, "PERMISSIONS/RULES.JSON")
		if err := result.Root.authorizeWrite(ordinaryCapability, caseAlias); err == nil {
			t.Fatal("ordinary capability authorized a filesystem case alias of a protected slot")
		}
	}
	if err := result.Root.authorizeWrite(protectedCapability, ordinary); err == nil {
		t.Fatal("protected capability authorized an ordinary slot")
	}
	if err := result.Root.authorizeWrite(ordinaryCapability, ordinary); err != nil {
		t.Fatal("ordinary capability rejected an ordinary slot")
	}
	if err := result.Root.authorizeWrite(ordinaryCapability, protectedAncestor); err == nil {
		t.Fatal("ordinary capability authorized a protected-slot ancestor")
	}
	if err := result.Root.authorizeWrite(protectedCapability, protectedAncestor); err == nil {
		t.Fatal("protected capability authorized an implicit ancestor slot")
	}
	if err := os.Remove(filepath.Join(protectedParent, "rules.json")); err != nil {
		t.Fatal("remove protected target before replacement failed")
	}
	if err := os.Rename(filepath.Join(rootPath, "movable.txt"), filepath.Join(protectedParent, "rules.json")); err != nil {
		t.Fatal("move ordinary target into protected slot failed")
	}
	if err := result.Root.authorizeWrite(ordinaryCapability, movable); err == nil {
		t.Fatal("stale ordinary binding remained authorized after its target moved")
	}

	for _, invalid := range []string{
		"", ".", "./ordinary.txt", "dir/../ordinary.txt", "../escape", "/escape",
		"dir//file", "dir/", "permissions\\rules.json", "C:/escape",
	} {
		if _, err := result.Root.Bind(invalid); err == nil {
			t.Fatal("non-canonical or escaping binding path was accepted")
		}
	}

	invalidResult, err := Bootstrap(rootPath, Policy{ProtectedSlots: []string{"permissions/../rules.json"}})
	if err == nil {
		mustClose(t, invalidResult.Root)
		t.Fatal("non-canonical protected slot was accepted")
	}
	if _, err := (Binding{}).MarshalBinary(); err == nil {
		t.Fatal("zero binding produced canonical bytes")
	}
	if _, err := (Identity{}).MarshalBinary(); err == nil {
		t.Fatal("zero identity produced canonical bytes")
	}
}

func TestWalkCancellation(t *testing.T) {
	rootPath := t.TempDir()
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600); err != nil {
			t.Fatal("create walk cancellation fixture failed")
		}
	}
	opened := mustBootstrap(t, rootPath, Policy{})
	defer mustClose(t, opened.Root)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	visits := 0
	if err := opened.Root.Walk(canceled, ".", newWalkCounter(t, 10, 10), func(Entry) error {
		visits++
		return nil
	}); !errors.Is(err, context.Canceled) || visits != 0 {
		t.Fatal("walk did not stop before opening after cancellation")
	}

	ctx, cancel := context.WithCancel(context.Background())
	visits = 0
	err := opened.Root.Walk(ctx, ".", newWalkCounter(t, 10, 10), func(Entry) error {
		visits++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || visits != 1 {
		t.Fatalf("walk did not observe cancellation between entries: err=%v visits=%d", err, visits)
	}
}

func TestWalkBudgetAndErrors(t *testing.T) {
	t.Run("directory budget before descent", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.Mkdir(filepath.Join(rootPath, "child"), 0o700); err != nil {
			t.Fatal("create walk directory fixture failed")
		}
		opened := mustBootstrap(t, rootPath, Policy{})
		defer mustClose(t, opened.Root)
		visits := 0
		err := opened.Root.Walk(context.Background(), ".", newWalkCounter(t, 10, 1), func(Entry) error {
			visits++
			return nil
		})
		var limitErr *budget.LimitError
		if !errors.As(err, &limitErr) || limitErr.Dimension != budget.Directories || visits != 0 {
			t.Fatalf("walk opened or visited a directory beyond its budget: err=%v visits=%d", err, visits)
		}
	})

	t.Run("file budget before visitor", func(t *testing.T) {
		rootPath := t.TempDir()
		for _, name := range []string{"first.txt", "second.txt"} {
			if err := os.WriteFile(filepath.Join(rootPath, name), []byte(name), 0o600); err != nil {
				t.Fatal("create walk file fixture failed")
			}
		}
		opened := mustBootstrap(t, rootPath, Policy{})
		defer mustClose(t, opened.Root)
		visits := 0
		err := opened.Root.Walk(context.Background(), ".", newWalkCounter(t, 1, 10), func(Entry) error {
			visits++
			return nil
		})
		var limitErr *budget.LimitError
		if !errors.As(err, &limitErr) || limitErr.Dimension != budget.Files || visits != 1 {
			t.Fatalf("walk invoked the visitor beyond its file budget: err=%v visits=%d", err, visits)
		}
	})

	t.Run("visitor error", func(t *testing.T) {
		rootPath := t.TempDir()
		if err := os.WriteFile(filepath.Join(rootPath, "file.txt"), []byte("fixture"), 0o600); err != nil {
			t.Fatal("create walk visitor fixture failed")
		}
		opened := mustBootstrap(t, rootPath, Policy{})
		defer mustClose(t, opened.Root)
		visitorErr := errors.New("visitor stopped")
		err := opened.Root.Walk(context.Background(), ".", newWalkCounter(t, 10, 10), func(Entry) error {
			return visitorErr
		})
		if !errors.Is(err, visitorErr) {
			t.Fatalf("walk suppressed a visitor error: %v", err)
		}
	})

	t.Run("directory changed after observation", func(t *testing.T) {
		rootPath := t.TempDir()
		childPath := filepath.Join(rootPath, "child")
		if err := os.Mkdir(childPath, 0o700); err != nil {
			t.Fatal("create changing directory fixture failed")
		}
		opened := mustBootstrap(t, rootPath, Policy{})
		defer mustClose(t, opened.Root)
		visits := 0
		err := opened.Root.Walk(context.Background(), ".", newWalkCounter(t, 10, 10), func(entry Entry) error {
			visits++
			if entry.IsDir() {
				return os.Remove(childPath)
			}
			return nil
		})
		if err == nil || visits != 1 {
			t.Fatalf("walk silently skipped a directory that changed after observation: err=%v visits=%d", err, visits)
		}
	})
}

func TestUnsupportedBuildTagExcludesSupportedPlatformsAndRejects(t *testing.T) {
	source, err := os.ReadFile("root_unsupported.go")
	if err != nil {
		t.Fatal("read unsupported safefs implementation failed")
	}
	lines := strings.Split(string(source), "\n")
	if len(lines) == 0 {
		t.Fatal("unsupported safefs implementation was empty")
	}
	expression, err := constraint.Parse(lines[0])
	if err != nil {
		t.Fatal("unsupported safefs build constraint was invalid")
	}
	evaluate := func(goos string) bool {
		return expression.Eval(func(tag string) bool { return tag == goos })
	}
	for _, supported := range []string{"darwin", "linux", "windows"} {
		if evaluate(supported) {
			t.Fatal("unsupported safefs implementation included a supported platform")
		}
	}
	for _, unsupported := range []string{"freebsd", "openbsd"} {
		if !evaluate(unsupported) {
			t.Fatal("unsupported safefs implementation omitted a rejected platform")
		}
	}
	text := string(source)
	if strings.Contains(text, "return nil, nil") || strings.Count(text, "errors.New") < 2 {
		t.Fatal("unsupported safefs implementation contains a successful fallback")
	}
}

func newWalkCounter(t *testing.T, files, directories int64) *budget.Counter {
	t.Helper()
	limits, err := budget.NewLimits(
		budget.Limit{Dimension: budget.Files, Value: files},
		budget.Limit{Dimension: budget.Directories, Value: directories},
	)
	if err != nil {
		t.Fatal("create walk limits failed")
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		t.Fatal("create walk counter failed")
	}
	return counter
}

func mustBootstrap(t *testing.T, rootPath string, policy Policy) OpenResult {
	t.Helper()
	result, err := Bootstrap(rootPath, policy)
	if err != nil {
		t.Fatal("safefs bootstrap failed")
	}
	if result.Root == nil {
		t.Fatal("safefs bootstrap returned a nil root")
	}
	return result
}

func mustBind(t *testing.T, root *Root, relative string) Binding {
	t.Helper()
	binding, err := root.Bind(relative)
	if err != nil {
		t.Fatal("safefs bind failed")
	}
	return binding
}

func mustIdentityBytes(t *testing.T, identity Identity) []byte {
	t.Helper()
	encoded, err := identity.MarshalBinary()
	if err != nil {
		t.Fatal("marshal root identity failed")
	}
	return encoded
}

func mustBindingBytes(t *testing.T, binding Binding) []byte {
	t.Helper()
	encoded, err := binding.MarshalBinary()
	if err != nil {
		t.Fatal("marshal binding failed")
	}
	return encoded
}

func mustClose(t *testing.T, root *Root) {
	t.Helper()
	if err := root.Close(); err != nil {
		t.Fatal("safefs close failed")
	}
}
