package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func assertOwnershipRegistryCapabilityShape(t *testing.T) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatal("parse ownership registry failed")
	}
	allowedMethods := map[string]struct{}{
		"register":          {},
		"registrationCount": {},
		"seal":              {},
		"closeAll":          {},
	}
	foundRegistry := false
	for _, declaration := range parsed.Decls {
		switch value := declaration.(type) {
		case *ast.GenDecl:
			for _, specification := range value.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != "ownershipRegistry" {
					continue
				}
				foundRegistry = true
				if ast.IsExported(typeSpec.Name.Name) {
					t.Fatal("ownership registry became exported")
				}
				ast.Inspect(typeSpec.Type, func(node ast.Node) bool {
					switch node.(type) {
					case *ast.MapType, *ast.InterfaceType:
						t.Fatal("ownership registry acquired service-locator storage")
					}
					return true
				})
			}
		case *ast.FuncDecl:
			if receiverTypeName(value.Recv) != "ownershipRegistry" {
				continue
			}
			if _, ok := allowedMethods[value.Name.Name]; !ok {
				t.Fatalf("ownership registry acquired unapproved method %q", value.Name.Name)
			}
		}
	}
	if !foundRegistry {
		t.Fatal("ownership registry type is unavailable")
	}
}

func assertAssemblyBuilderCapabilityShape(t *testing.T) {
	t.Helper()
	assemblyFile, err := parser.ParseFile(token.NewFileSet(), "assembly.go", nil, 0)
	if err != nil {
		t.Fatal("parse assembly pipeline failed")
	}
	lifecycleFile, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatal("parse assembly lifecycle failed")
	}

	foundState := false
	foundBuilder := false
	for _, declaration := range assemblyFile.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, specification := range general.Specs {
			switch value := specification.(type) {
			case *ast.ValueSpec:
				for _, name := range value.Names {
					if name.Name == "assemblyStageOrder" {
						t.Fatal("assembly stage order remained package-mutable")
					}
				}
			case *ast.TypeSpec:
				switch value.Name.Name {
				case "assemblyBuildState":
					foundState = true
					structure, ok := value.Type.(*ast.StructType)
					if !ok {
						t.Fatal("assembly build state is not a typed graph structure")
					}
					for _, field := range structure.Fields.List {
						for _, name := range field.Names {
							if name.Name == "owners" || name.Name == "completed" {
								t.Fatal("builder-visible state acquired pipeline control state")
							}
						}
						if typeReferencesIdentifier(field.Type, "ownershipRegistry") {
							t.Fatal("builder-visible state exposed the ownership registry")
						}
					}
				case "assemblyStageBuilder":
					foundBuilder = true
					function, ok := value.Type.(*ast.FuncType)
					if !ok || function.Params == nil || len(function.Params.List) != 3 ||
						identifierTypeName(function.Params.List[2].Type) != "assemblyOwnerRegistrar" {
						t.Fatal("assembly builder did not receive only the scoped owner registrar")
					}
					if typeReferencesIdentifier(function.Params, "ownershipRegistry") ||
						typeReferencesIdentifier(function.Params, "assemblyOwnerScope") {
						t.Fatal("assembly builder received raw lifecycle control state")
					}
				}
			}
		}
	}
	if !foundState || !foundBuilder {
		t.Fatal("assembly builder capability types are unavailable")
	}

	foundRegistrar := false
	foundScope := false
	for _, declaration := range lifecycleFile.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, specification := range general.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok {
				continue
			}
			switch typeSpec.Name.Name {
			case "assemblyOwnerRegistrar":
				foundRegistrar = true
				function, ok := typeSpec.Type.(*ast.FuncType)
				if !ok || function.Params == nil || len(function.Params.List) != 1 ||
					identifierTypeName(function.Params.List[0].Type) != "ownerClose" {
					t.Fatal("assembly owner registrar acquired an unapproved capability")
				}
			case "assemblyOwnerScope":
				foundScope = true
				if typeReferencesIdentifier(typeSpec.Type, "ownershipRegistry") {
					t.Fatal("assembly owner scope exposed the raw ownership registry")
				}
			}
		}
	}
	if !foundRegistrar || !foundScope {
		t.Fatal("assembly scoped registration capability is unavailable")
	}
}

func typeReferencesIdentifier(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(candidate ast.Node) bool {
		identifier, ok := candidate.(*ast.Ident)
		if ok && identifier.Name == name {
			found = true
			return false
		}
		return !found
	})
	return found
}

func identifierTypeName(expression ast.Expr) string {
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func receiverTypeName(receivers *ast.FieldList) string {
	if receivers == nil || len(receivers.List) != 1 {
		return ""
	}
	expression := receivers.List[0].Type
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, _ := expression.(*ast.Ident)
	if identifier == nil {
		return ""
	}
	return identifier.Name
}

func TestOwnershipRegistryClosesInReverseOrderOnce(t *testing.T) {
	registry := newOwnershipRegistry()
	var order []assemblyStage
	secret := "private-close-detail"
	for _, stage := range []assemblyStage{assemblyStageConfig, assemblyStageSecurity, assemblyStageExecution} {
		stage := stage
		if err := registry.register(stage, func(context.Context) error {
			order = append(order, stage)
			if stage == assemblyStageSecurity {
				return errors.New(secret)
			}
			return nil
		}); err != nil {
			t.Fatal("register assembly owner failed")
		}
	}
	if err := registry.seal(); err != nil {
		t.Fatal("seal ownership registry failed")
	}
	first := registry.closeAll()
	second := registry.closeAll()
	if first == nil || second == nil || first.Error() != second.Error() || strings.Contains(first.Error(), secret) {
		t.Fatal("ownership close result was not stable and payload-free")
	}
	want := []assemblyStage{assemblyStageExecution, assemblyStageSecurity, assemblyStageConfig}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("ownership close order = %v, want %v", order, want)
	}
}

func TestOwnershipRegistryConcurrentCloseIsLinearized(t *testing.T) {
	registry := newOwnershipRegistry()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	if err := registry.register(assemblyStageConfig, func(context.Context) error {
		calls.Add(1)
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal("register gated assembly owner failed")
	}

	const goroutines = 32
	results := make(chan error, goroutines)
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for range goroutines {
		go func() {
			defer wait.Done()
			results <- registry.closeAll()
		}()
	}
	<-started
	if err := registry.register(assemblyStageSecurity, func(context.Context) error { return nil }); err == nil {
		t.Fatal("ownership registry accepted an owner after close began")
	}
	close(release)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent ownership close changed the cached result")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("assembly owner close calls = %d, want 1", calls.Load())
	}
}

func TestRollbackOwnershipRegistryDetachesCallerAndCachesTimeout(t *testing.T) {
	registry := newOwnershipRegistry()
	started := make(chan struct{})
	earlierClosed := make(chan struct{})
	var calls [assemblyStageCount]atomic.Int32
	var orderMu sync.Mutex
	var order []assemblyStage
	record := func(stage assemblyStage) {
		calls[int(stage)-1].Add(1)
		orderMu.Lock()
		order = append(order, stage)
		orderMu.Unlock()
	}
	if err := registry.register(assemblyStageConfig, func(context.Context) error {
		record(assemblyStageConfig)
		close(earlierClosed)
		return nil
	}); err != nil {
		t.Fatal("register earlier rollback owner failed: ", err)
	}
	if err := registry.register(assemblyStageSecurity, func(cleanupCtx context.Context) error {
		record(assemblyStageSecurity)
		close(started)
		<-cleanupCtx.Done()
		return cleanupCtx.Err()
	}); err != nil {
		t.Fatal("register blocking rollback owner failed: ", err)
	}

	source, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan error, 1)
	go func() {
		result <- rollbackOwnershipRegistry(source, registry, 20*time.Millisecond)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("rollback inherited caller cancellation and skipped owner close")
	}
	var first error
	select {
	case first = <-result:
	case <-time.After(time.Second):
		t.Fatal("rollback did not respect its independent hard deadline")
	}
	if !errors.Is(first, errAssemblyRollbackTimeout) {
		t.Fatalf("rollback result = %v, want fixed timeout", first)
	}

	second := registry.closeAll()
	third := registry.closeAll()
	if !errors.Is(second, errAssemblyRollbackTimeout) || !errors.Is(third, errAssemblyRollbackTimeout) ||
		first.Error() != second.Error() || second.Error() != third.Error() {
		t.Fatalf("rollback timeout was not cached stably: first=%v second=%v third=%v", first, second, third)
	}
	select {
	case <-earlierClosed:
	case <-time.After(time.Second):
		t.Fatal("post-timeout rollback did not invoke the earlier owner")
	}
	orderMu.Lock()
	closed := append([]assemblyStage(nil), order...)
	orderMu.Unlock()
	want := []assemblyStage{assemblyStageSecurity, assemblyStageConfig}
	if !reflect.DeepEqual(closed, want) {
		t.Fatalf("post-timeout rollback close order = %v, want %v", closed, want)
	}
	for _, stage := range want {
		if got := calls[int(stage)-1].Load(); got != 1 {
			t.Fatalf("owner %s close calls = %d, want one", stage.label(), got)
		}
	}
}

func TestRollbackOwnershipRegistryContinuesPastOwnerIgnoringContext(t *testing.T) {
	registry := newOwnershipRegistry()
	ignoredContextStarted := make(chan struct{})
	releaseIgnoredContext := make(chan struct{})
	earlierOwnerStarted := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseIgnoredContext) })
	var ignoredCalls atomic.Int32
	var earlierCalls atomic.Int32

	if err := registry.register(assemblyStageConfig, func(context.Context) error {
		earlierCalls.Add(1)
		close(earlierOwnerStarted)
		return nil
	}); err != nil {
		t.Fatal("register earlier defensive rollback owner failed: ", err)
	}
	if err := registry.register(assemblyStageSecurity, func(context.Context) error {
		ignoredCalls.Add(1)
		close(ignoredContextStarted)
		<-releaseIgnoredContext
		return nil
	}); err != nil {
		t.Fatal("register hostile rollback owner failed: ", err)
	}

	first := rollbackOwnershipRegistry(context.Background(), registry, 10*time.Millisecond)
	if !errors.Is(first, errAssemblyRollbackTimeout) {
		t.Fatalf("hostile rollback result = %v, want fixed timeout", first)
	}
	select {
	case <-ignoredContextStarted:
	default:
		t.Fatal("hostile rollback owner was not invoked")
	}
	select {
	case <-earlierOwnerStarted:
	case <-time.After(time.Second):
		t.Fatal("hostile rollback owner prevented an earlier owner from being invoked")
	}

	secondResult := make(chan error, 1)
	go func() { secondResult <- registry.closeAll() }()
	select {
	case second := <-secondResult:
		if second == nil || second.Error() != first.Error() {
			t.Fatalf("post-timeout Close result = %v, want %v", second, first)
		}
	case <-time.After(time.Second):
		t.Fatal("hostile rollback owner left the ownership registry deadlocked")
	}
	releaseOnce.Do(func() { close(releaseIgnoredContext) })
	if ignoredCalls.Load() != 1 || earlierCalls.Load() != 1 {
		t.Fatalf("defensive rollback calls: hostile=%d earlier=%d, want one each", ignoredCalls.Load(), earlierCalls.Load())
	}
}

func TestLifecycleUsesC7DoubleContextAndReverseOrder(t *testing.T) {
	registry := newOwnershipRegistry()
	trace := newT429OwnerTrace()
	appStarted := make(chan struct{})
	releaseApp := make(chan struct{})
	ownerOrder := []string{
		"diagnostics", "roots", "artifact-close", "artifact-cleanup",
		"provider-client", "provider-stream", "hook-engine", "mcp-manager", "app-runtime",
	}
	for _, name := range ownerOrder {
		name := name
		closeOwner := trace.owner(name, nil)
		if name == "app-runtime" {
			closeOwner = trace.ownerWithGate(name, nil, appStarted, releaseApp)
		}
		if err := registry.register(assemblyStageUI, closeOwner); err != nil {
			t.Fatal("register lifecycle owner failed: ", err)
		}
	}
	runtime := &Runtime{owners: registry, cleanupTimeout: 200 * time.Millisecond}

	caller, cancel := context.WithCancel(context.Background())
	cancel()
	first := runtime.Close(caller)
	if !errors.Is(first, context.Canceled) {
		t.Fatalf("first Close wait result = %v, want caller cancellation", first)
	}
	select {
	case <-appStarted:
	case <-time.After(time.Second):
		t.Fatal("internal cleanup did not start after caller cancellation")
	}
	close(releaseApp)

	second := runtime.Close(context.Background())
	third := runtime.Close(context.Background())
	if second != nil || third != nil {
		t.Fatalf("final lifecycle Close result: second=%v third=%v", second, third)
	}
	want := []string{"app-runtime", "mcp-manager", "hook-engine", "provider-stream", "provider-client", "artifact-cleanup", "artifact-close", "roots", "diagnostics"}
	trace.assert(t, want)
}

func TestProviderClientsCloseAfterStreams(t *testing.T) {
	registry := newOwnershipRegistry()
	trace := newT429OwnerTrace()
	for _, owner := range []struct {
		name  string
		stage assemblyStage
	}{
		{name: "provider-client", stage: assemblyStageAdapters},
		{name: "provider-stream", stage: assemblyStageAdapters},
	} {
		if err := registry.register(owner.stage, trace.owner(owner.name, nil)); err != nil {
			t.Fatal("register provider lifecycle owner failed: ", err)
		}
	}
	runtime := &Runtime{owners: registry, cleanupTimeout: time.Second}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal("provider lifecycle Close failed: ", err)
	}
	trace.assert(t, []string{"provider-stream", "provider-client"})
}

func TestLifecycleContinuesAfterLayerFailure(t *testing.T) {
	registry := newOwnershipRegistry()
	trace := newT429OwnerTrace()
	const canary = "private lifecycle layer failure"
	ownerOrder := []string{"diagnostics", "roots", "artifact-close", "artifact-cleanup", "provider-client", "hook-engine", "mcp-manager", "app-runtime"}
	for _, name := range ownerOrder {
		name := name
		var ownerErr error
		if name == "mcp-manager" {
			ownerErr = errors.New(canary)
		}
		if err := registry.register(assemblyStageUI, trace.owner(name, ownerErr)); err != nil {
			t.Fatal("register failing lifecycle owner failed: ", err)
		}
	}
	runtime := &Runtime{owners: registry, cleanupTimeout: time.Second}
	first := runtime.Close(context.Background())
	second := runtime.Close(context.Background())
	if first == nil || second == nil || first.Error() != second.Error() || strings.Contains(first.Error(), canary) {
		t.Fatalf("layer failure result was not safe, continued, and cached: first=%v second=%v", first, second)
	}
	want := []string{"app-runtime", "mcp-manager", "hook-engine", "provider-client", "artifact-cleanup", "artifact-close", "roots", "diagnostics"}
	trace.assert(t, want)
}

func TestLifecycleContinuesAfterCleanupTimeout(t *testing.T) {
	registry := newOwnershipRegistry()
	started := make(chan struct{})
	earlierStarted := make(chan struct{})
	var earlierCalls atomic.Int32
	if err := registry.register(assemblyStageConfig, func(context.Context) error {
		earlierCalls.Add(1)
		close(earlierStarted)
		return nil
	}); err != nil {
		t.Fatal("register timeout follow-up owner failed: ", err)
	}
	if err := registry.register(assemblyStageUI, func(cleanupCtx context.Context) error {
		close(started)
		<-cleanupCtx.Done()
		return cleanupCtx.Err()
	}); err != nil {
		t.Fatal("register timeout owner failed: ", err)
	}
	runtime := &Runtime{owners: registry, cleanupTimeout: 10 * time.Millisecond}
	first := runtime.Close(context.Background())
	if !errors.Is(first, errAssemblyRollbackTimeout) {
		t.Fatalf("cleanup timeout result = %v, want timeout", first)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cleanup timeout owner did not start")
	}
	select {
	case <-earlierStarted:
	case <-time.After(time.Second):
		t.Fatal("cleanup timeout skipped a later reverse owner")
	}
	second := runtime.Close(context.Background())
	if second == nil || second.Error() != first.Error() || earlierCalls.Load() != 1 {
		t.Fatalf("cleanup timeout result/calls not cached: first=%v second=%v earlier=%d", first, second, earlierCalls.Load())
	}
}

type t429OwnerTrace struct {
	mu      sync.Mutex
	closed  []string
	calls   map[string]int
	invalid bool
}

func newT429OwnerTrace() *t429OwnerTrace {
	return &t429OwnerTrace{calls: make(map[string]int)}
}

func (trace *t429OwnerTrace) owner(name string, ownerErr error) ownerClose {
	return trace.ownerWithGate(name, ownerErr, nil, nil)
}

func (trace *t429OwnerTrace) ownerWithGate(name string, ownerErr error, started chan struct{}, release <-chan struct{}) ownerClose {
	return func(cleanupCtx context.Context) error {
		trace.mu.Lock()
		if cleanupCtx == nil || cleanupCtx.Err() != nil {
			trace.invalid = true
		}
		trace.closed = append(trace.closed, name)
		trace.calls[name]++
		trace.mu.Unlock()
		if started != nil {
			close(started)
		}
		if release != nil {
			<-release
		}
		return ownerErr
	}
}

func (trace *t429OwnerTrace) assert(t *testing.T, want []string) {
	t.Helper()
	trace.mu.Lock()
	closed := append([]string(nil), trace.closed...)
	calls := make(map[string]int, len(trace.calls))
	for name, count := range trace.calls {
		calls[name] = count
	}
	invalid := trace.invalid
	trace.mu.Unlock()
	if invalid || !reflect.DeepEqual(closed, want) {
		t.Fatalf("lifecycle close order=%v want=%v invalidContext=%v", closed, want, invalid)
	}
	for _, name := range want {
		if calls[name] != 1 {
			t.Fatalf("lifecycle owner %q close calls=%d want=1", name, calls[name])
		}
	}
}
