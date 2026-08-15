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
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

// TestAssemblyRollsBackEveryInitializationPoint is the T4.28 acceptance root.
// Each injected failure occurs immediately before or after the current stage
// registers its owner (with cancellation as a third path). A failed build must
// drain the very same registration table in reverse order, including an owner
// created by the failing stage. No Runtime may escape any failure path.
func TestAssemblyRollsBackEveryInitializationPoint(t *testing.T) {
	stages := orderedAssemblyStages()
	slots := t428ProductionOwnerSlots()
	assertT428ProductionOwnerManifest(t, slots)

	t.Run("failure before and after every production owner slot", func(t *testing.T) {
		for index, slot := range slots {
			index, slot := index, slot
			for _, afterRegistration := range []bool{false, true} {
				afterRegistration := afterRegistration
				position := "before"
				if afterRegistration {
					position = "after"
				}
				t.Run(position+"/"+slot.name, func(t *testing.T) {
					trace := newT428SlotTrace()
					factories, registrar := t428SlotFactories(trace, slots, index, afterRegistration, nil)
					runtime, err := (assembly{factories: factories}).buildCandidate(context.Background())
					wantEnd := index
					if afterRegistration {
						wantEnd++
					}
					assertT428SlotRollback(t, runtime, err, trace, slots[:wantEnd], registrar)
				})
			}
		}
	})

	t.Run("cancellation after a middle-stage owner keeps its exact prefix", func(t *testing.T) {
		const middleSecuritySlot = 4 // security/artifact-store in the 16-slot manifest.
		trace := newT428SlotTrace()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		factories, registrar := t428SlotFactories(trace, slots, middleSecuritySlot, true, cancel)
		runtime, err := (assembly{factories: factories}).buildCandidate(ctx)
		assertT428SlotRollback(t, runtime, err, trace, slots[:middleSecuritySlot+1], registrar)
	})

	t.Run("failure after each stage owner", func(t *testing.T) {
		for index, failedStage := range stages {
			index, failedStage := index, failedStage
			t.Run(failedStage.label(), func(t *testing.T) {
				trace := newAssemblyRollbackTrace()
				factories, registrar := assemblyRollbackFactories(trace, index, nil, true)
				runtime, err := (assembly{factories: factories}).buildCandidate(context.Background())
				assertAssemblyRollbackResult(t, runtime, err, trace, stages[:index+1], nil, registrar)
			})
		}
	})

	t.Run("failure before the current stage registers an owner", func(t *testing.T) {
		for index, failedStage := range stages {
			index, failedStage := index, failedStage
			t.Run(failedStage.label(), func(t *testing.T) {
				trace := newAssemblyRollbackTrace()
				factories, registrar := assemblyRollbackFactories(trace, index, nil, false)
				runtime, err := (assembly{factories: factories}).buildCandidate(context.Background())
				assertAssemblyRollbackResult(t, runtime, err, trace, stages[:index], stages[index:index+1], registrar)
			})
		}
	})

	t.Run("cancellation after each stage owner still rolls back", func(t *testing.T) {
		for index, canceledStage := range stages {
			index, canceledStage := index, canceledStage
			t.Run(canceledStage.label(), func(t *testing.T) {
				trace := newAssemblyRollbackTrace()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				factories, registrar := assemblyRollbackFactories(trace, index, cancel, true)

				result := make(chan assemblyRollbackResult, 1)
				go func() {
					runtime, err := (assembly{factories: factories}).buildCandidate(ctx)
					result <- assemblyRollbackResult{runtime: runtime, err: err}
				}()

				select {
				case <-trace.closeStarted:
					// The init context is cancelled at the injected stage.  Seeing
					// the owner close before Build returns proves rollback does not
					// simply abandon already-created owners on cancellation.
				case <-time.After(2 * time.Second):
					t.Fatal("assembly rollback did not start before Build returned/timeout")
				}
				close(trace.releaseClose)
				outcome := <-result
				assertAssemblyRollbackResult(t, outcome.runtime, outcome.err, trace, stages[:index+1], nil, registrar)
			})
		}
	})

	t.Run("owner close failure does not stop or leak rollback", func(t *testing.T) {
		trace := newAssemblyRollbackTrace()
		trace.closeError = assemblyStageAdapters
		factories, registrar := assemblyRollbackFactories(trace, len(stages)-1, nil, true)
		runtime, err := (assembly{factories: factories}).buildCandidate(context.Background())
		assertAssemblyRollbackResult(t, runtime, err, trace, stages[:], nil, registrar)
	})

	t.Run("rollback failures use the existing bounded diagnostics sink", func(t *testing.T) {
		testCases := []struct {
			name        string
			close       ownerClose
			timeout     time.Duration
			wantCode    string
			closeCanary string
		}{
			{
				name:        "owner failure",
				closeCanary: "rollback-owner-private-detail",
				wantCode:    "assembly_rollback_failed",
			},
			{
				name:     "hard timeout",
				timeout:  10 * time.Millisecond,
				wantCode: "assembly_rollback_timeout",
				close: func(cleanupCtx context.Context) error {
					<-cleanupCtx.Done()
					return cleanupCtx.Err()
				},
			},
		}
		for _, testCase := range testCases {
			testCase := testCase
			t.Run(testCase.name, func(t *testing.T) {
				redactor := redact.NewRuntimeRedactor()
				if testCase.closeCanary != "" {
					redactor.RegisterSecret(testCase.closeCanary)
				}
				sink, sinkErr := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
					Redactor: redactor, MaxItems: 8, MaxItemBytes: 256, MaxTotalBytes: 2048,
				})
				if sinkErr != nil {
					t.Fatal("create rollback diagnostic sink failed: ", sinkErr)
				}
				closeOwner := testCase.close
				if closeOwner == nil {
					closeOwner = func(context.Context) error { return errors.New(testCase.closeCanary) }
				}
				factories := assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
					switch stage {
					case assemblyStageConfig:
						state.configuration = &assemblyConfiguration{
							redactor: redactor, diagnostics: sink, cleanupTimeout: testCase.timeout,
						}
						return register(func(context.Context) error { return nil })
					case assemblyStageSecurity:
						if err := register(closeOwner); err != nil {
							return err
						}
						return errors.New("initialization-private-detail")
					default:
						return register(func(context.Context) error { return nil })
					}
				})

				runtime, buildErr := (assembly{factories: factories}).buildCandidate(context.Background())
				if runtime != nil || buildErr == nil || buildErr.Error() != "assembly security stage failed" {
					t.Fatalf("rollback changed safe initialization result: runtime=%#v err=%v", runtime, buildErr)
				}
				items := sink.Snapshot().Items()
				if len(items) != 1 || items[0].Count != 1 {
					t.Fatalf("rollback diagnostics = %#v, want one bounded item", items)
				}
				item := items[0].Diagnostic
				if item.Code != testCase.wantCode || item.Source != "assembly" || item.Severity != diagnostics.SeverityError {
					t.Fatalf("rollback diagnostic metadata = %#v", item)
				}
				message := item.Message.Text()
				if message == "" || strings.Contains(message, "initialization-private-detail") ||
					(testCase.closeCanary != "" && strings.Contains(message, testCase.closeCanary)) {
					t.Fatalf("rollback diagnostic retained unsafe/empty message %q", message)
				}
			})
		}
	})
}

type t428OwnerSlot struct {
	stage assemblyStage
	name  string
}

func t428ProductionOwnerSlots() []t428OwnerSlot {
	return []t428OwnerSlot{
		{assemblyStageConfig, "config/diagnostics"},
		{assemblyStageConfig, "config/redactor"},
		{assemblyStageSecurity, "security/project-root"},
		{assemblyStageSecurity, "security/legacy-root"},
		{assemblyStageSecurity, "security/artifact-store"},
		{assemblyStageSecurity, "security/network-policy"},
		{assemblyStageSecurity, "security/network-clients"},
		{assemblyStageSecurity, "security/process-runner"},
		{assemblyStageSecurity, "security/protection-plans"},
		{assemblyStageExecution, "execution/services"},
		{assemblyStageAdapters, "adapters/provider-client"},
		{assemblyStageAdapters, "adapters/hook-http"},
		{assemblyStageAdapters, "adapters/hook-engine"},
		{assemblyStageAdapters, "adapters/mcp-manager"},
		{assemblyStageOrchestration, "orchestration/orchestrator"},
		{assemblyStageUI, "ui/candidate"},
	}
}

func assertT428ProductionOwnerManifest(t *testing.T, slots []t428OwnerSlot) {
	t.Helper()
	if len(slots) != 16 {
		t.Fatalf("T4.28 owner manifest has %d slots, want 16", len(slots))
	}
	wantCounts := make(map[assemblyStage]int, assemblyStageCount)
	previous := assemblyStage(0)
	for _, slot := range slots {
		if !slot.stage.valid() || slot.stage < previous || slot.name == "" {
			t.Fatalf("T4.28 owner manifest is invalid or unordered at %#v", slot)
		}
		wantCounts[slot.stage]++
		previous = slot.stage
	}

	parsed, err := parser.ParseFile(token.NewFileSet(), "assembly.go", mustReadAssembly(t), 0)
	if err != nil {
		t.Fatal("parse production Assembly owner manifest failed: ", err)
	}
	stageFunctions := map[string]assemblyStage{
		"newAssemblyConfigStage":        assemblyStageConfig,
		"newAssemblySecurityStage":      assemblyStageSecurity,
		"newAssemblyExecutionStage":     assemblyStageExecution,
		"newAssemblyAdaptersStage":      assemblyStageAdapters,
		"newAssemblyOrchestrationStage": assemblyStageOrchestration,
		"newAssemblyUIStage":            assemblyStageUI,
	}
	gotCounts := make(map[assemblyStage]int, assemblyStageCount)
	found := make(map[assemblyStage]bool, assemblyStageCount)
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		stage, ok := stageFunctions[function.Name.Name]
		if !ok {
			continue
		}
		found[stage] = true
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			identifier, ok := call.Fun.(*ast.Ident)
			if ok && identifier.Name == "register" {
				gotCounts[stage]++
			}
			return true
		})
	}
	for _, stage := range orderedAssemblyStages() {
		if !found[stage] || gotCounts[stage] != wantCounts[stage] {
			t.Fatalf("production %s owner registrations = %d, manifest = %d", stage.label(), gotCounts[stage], wantCounts[stage])
		}
	}
}

type t428SlotTrace struct {
	mu             sync.Mutex
	registered     []string
	closed         []string
	closeCalls     map[string]int
	invalidContext bool
}

func newT428SlotTrace() *t428SlotTrace {
	return &t428SlotTrace{closeCalls: make(map[string]int)}
}

func (trace *t428SlotTrace) owner(name string) ownerClose {
	return func(cleanupCtx context.Context) error {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		if cleanupCtx == nil || cleanupCtx.Err() != nil {
			trace.invalidContext = true
		} else if _, hasDeadline := cleanupCtx.Deadline(); !hasDeadline {
			trace.invalidContext = true
		}
		trace.closed = append(trace.closed, name)
		trace.closeCalls[name]++
		return nil
	}
}

func t428SlotFactories(
	trace *t428SlotTrace,
	slots []t428OwnerSlot,
	failIndex int,
	afterRegistration bool,
	cancel context.CancelFunc,
) (assemblyFactories, assemblyOwnerRegistrar) {
	var failedRegistrar assemblyOwnerRegistrar
	return assemblyFactoriesForTest(func(_ int, stage assemblyStage, _ context.Context, _ *assemblyBuildState, register assemblyOwnerRegistrar) error {
			for index, slot := range slots {
				if slot.stage != stage {
					continue
				}
				if index == failIndex && !afterRegistration {
					failedRegistrar = register
					return errors.New("injected owner initialization failure")
				}
				if err := register(trace.owner(slot.name)); err != nil {
					return err
				}
				trace.mu.Lock()
				trace.registered = append(trace.registered, slot.name)
				trace.mu.Unlock()
				if index == failIndex {
					failedRegistrar = register
					if cancel != nil {
						cancel()
						return nil
					}
					return errors.New("injected owner initialization failure")
				}
			}
			return nil
		}), func(close ownerClose) error {
			if failedRegistrar == nil {
				return errors.New("T4.28 slot fixture has no failed registrar")
			}
			return failedRegistrar(close)
		}
}

func assertT428SlotRollback(
	t *testing.T,
	runtime *assemblyRuntime,
	err error,
	trace *t428SlotTrace,
	wantSlots []t428OwnerSlot,
	registrar assemblyOwnerRegistrar,
) {
	t.Helper()
	if runtime != nil || err == nil {
		t.Fatalf("owner-slot failure published Runtime: runtime=%#v err=%v", runtime, err)
	}
	var wantRegistered []string
	for _, slot := range wantSlots {
		wantRegistered = append(wantRegistered, slot.name)
	}
	wantClosed := append([]string(nil), wantRegistered...)
	for left, right := 0, len(wantClosed)-1; left < right; left, right = left+1, right-1 {
		wantClosed[left], wantClosed[right] = wantClosed[right], wantClosed[left]
	}

	trace.mu.Lock()
	registered := append([]string(nil), trace.registered...)
	closed := append([]string(nil), trace.closed...)
	calls := make(map[string]int, len(trace.closeCalls))
	for name, count := range trace.closeCalls {
		calls[name] = count
	}
	invalidContext := trace.invalidContext
	trace.mu.Unlock()
	if invalidContext || !reflect.DeepEqual(registered, wantRegistered) || !reflect.DeepEqual(closed, wantClosed) {
		t.Fatalf("owner-slot rollback registered=%v closed=%v wantRegistered=%v wantClosed=%v invalidContext=%v",
			registered, closed, wantRegistered, wantClosed, invalidContext)
	}
	for _, name := range wantRegistered {
		if calls[name] != 1 {
			t.Fatalf("owner slot %q close calls = %d, want one", name, calls[name])
		}
	}
	if registrar == nil || registrar(func(context.Context) error { return nil }) == nil {
		t.Fatal("failed owner-slot scope remained active after rollback")
	}
}

type assemblyRollbackResult struct {
	runtime *assemblyRuntime
	err     error
}

type assemblyRollbackTrace struct {
	mu               sync.Mutex
	started          []assemblyStage
	registered       []assemblyStage
	closed           []assemblyStage
	closeCalls       map[assemblyStage]int
	closeStarted     chan struct{}
	closeStartedOnce sync.Once
	releaseClose     chan struct{}
	closeError       assemblyStage
	closeErrorCanary string
	invalidContext   bool
}

func newAssemblyRollbackTrace() *assemblyRollbackTrace {
	return &assemblyRollbackTrace{
		closeCalls:       make(map[assemblyStage]int),
		closeStarted:     make(chan struct{}),
		releaseClose:     make(chan struct{}),
		closeError:       0,
		closeErrorCanary: "assembly rollback close secret",
	}
}

func (trace *assemblyRollbackTrace) owner(stage assemblyStage, gate bool) ownerClose {
	return func(cleanupCtx context.Context) error {
		trace.mu.Lock()
		if cleanupCtx == nil || cleanupCtx.Err() != nil {
			trace.invalidContext = true
		} else if _, hasDeadline := cleanupCtx.Deadline(); !hasDeadline {
			trace.invalidContext = true
		}
		trace.closeCalls[stage]++
		trace.closed = append(trace.closed, stage)
		trace.mu.Unlock()
		trace.closeStartedOnce.Do(func() { close(trace.closeStarted) })
		if gate {
			<-trace.releaseClose
		}
		if trace.closeError == stage {
			return errors.New(trace.closeErrorCanary)
		}
		return nil
	}
}

func assemblyRollbackFactories(
	trace *assemblyRollbackTrace,
	failIndex int,
	cancel context.CancelFunc,
	registerBeforeFailure bool,
) (assemblyFactories, assemblyOwnerRegistrar) {
	stages := orderedAssemblyStages()
	var failedRegistrar assemblyOwnerRegistrar
	makeBuilder := func(index int, stage assemblyStage) assemblyStageBuilder {
		return func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
			trace.mu.Lock()
			trace.started = append(trace.started, stage)
			trace.mu.Unlock()
			if ctx == nil || state == nil || register == nil {
				return errors.New("rollback fixture received incomplete stage scope")
			}
			if index == failIndex && !registerBeforeFailure {
				failedRegistrar = register
				return errors.New("injected assembly failure")
			}
			if err := register(trace.owner(stage, cancel != nil)); err != nil {
				return err
			}
			trace.mu.Lock()
			trace.registered = append(trace.registered, stage)
			trace.mu.Unlock()
			if index == failIndex {
				failedRegistrar = register
				if cancel != nil {
					cancel()
					return nil
				}
				return errors.New("injected assembly failure")
			}
			return nil
		}
	}
	factories := assemblyFactories{
		config:        makeBuilder(0, stages[0]),
		security:      makeBuilder(1, stages[1]),
		execution:     makeBuilder(2, stages[2]),
		adapters:      makeBuilder(3, stages[3]),
		orchestration: makeBuilder(4, stages[4]),
		ui:            makeBuilder(5, stages[5]),
	}
	return factories, func(ownerClose) error {
		if failedRegistrar == nil {
			return errors.New("rollback fixture has no failed-stage registrar")
		}
		return failedRegistrar(func(context.Context) error { return nil })
	}
}

func assertAssemblyRollbackResult(
	t *testing.T,
	runtime *assemblyRuntime,
	err error,
	trace *assemblyRollbackTrace,
	wantRegistered []assemblyStage,
	wantStartedExtra []assemblyStage,
	registrar assemblyOwnerRegistrar,
) {
	t.Helper()
	if len(wantRegistered) == 0 {
		wantRegistered = nil
	}
	if err == nil || runtime != nil {
		t.Fatalf("failed Assembly published a Runtime: runtime=%#v err=%v", runtime, err)
	}
	if message := err.Error(); message == "" || strings.Contains(message, trace.closeErrorCanary) {
		t.Fatalf("rollback returned an unsafe/empty error: %q", message)
	}
	trace.mu.Lock()
	started := append([]assemblyStage(nil), trace.started...)
	registered := append([]assemblyStage(nil), trace.registered...)
	closed := append([]assemblyStage(nil), trace.closed...)
	invalidContext := trace.invalidContext
	calls := make(map[assemblyStage]int, len(trace.closeCalls))
	for stage, count := range trace.closeCalls {
		calls[stage] = count
	}
	trace.mu.Unlock()
	if invalidContext {
		t.Fatal("rollback owner did not receive a live, bounded detached context")
	}
	wantStarted := append([]assemblyStage(nil), wantRegistered...)
	wantStarted = append(wantStarted, wantStartedExtra...)
	if !reflect.DeepEqual(started, wantStarted) {
		t.Fatalf("stages entered = %v, want %v", started, wantStarted)
	}
	if !reflect.DeepEqual(registered, wantRegistered) {
		t.Fatalf("owners registered = %v, want %v", registered, wantRegistered)
	}
	wantClosed := append([]assemblyStage(nil), wantRegistered...)
	for left, right := 0, len(wantClosed)-1; left < right; left, right = left+1, right-1 {
		wantClosed[left], wantClosed[right] = wantClosed[right], wantClosed[left]
	}
	if !reflect.DeepEqual(closed, wantClosed) {
		t.Fatalf("rollback close order = %v, want %v", closed, wantClosed)
	}
	for _, stage := range wantRegistered {
		if calls[stage] != 1 {
			t.Fatalf("owner %s close calls = %d, want exactly one", stage.label(), calls[stage])
		}
	}
	if registrar == nil || registrar(func(context.Context) error { return nil }) == nil {
		t.Fatal("failed-stage owner scope remained active after rollback")
	}
}
