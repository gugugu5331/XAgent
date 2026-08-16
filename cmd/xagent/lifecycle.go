package main

import (
	"context"
	"errors"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/tui"
)

type ownerClose func(context.Context) error

// Assembly rollback is a lifecycle operation, not a best-effort response to
// the caller's build context.  Keep a hard ceiling here until the resolved
// lifecycle option is available; once Config has resolved, assembly.go passes
// that (possibly shorter) value to rollbackOwnershipRegistry.
const assemblyRollbackHardTimeout = 2 * time.Second

var errAssemblyRollbackTimeout = errors.New("assembly rollback exceeded its hard deadline")

type ownershipRecord struct {
	stage assemblyStage
	close ownerClose
}

// ownershipRegistry owns only close actions. It intentionally has no service
// lookup API, so it cannot become a service locator or leak domain owners.
type ownershipRegistry struct {
	mu               sync.Mutex
	closeOnce        sync.Once
	runtimeCloseOnce sync.Once
	closeDone        chan struct{}
	accepting        bool
	records          []ownershipRecord
	resultMu         sync.Mutex
	result           error
}

func newOwnershipRegistry() *ownershipRegistry {
	return &ownershipRegistry{accepting: true, closeDone: make(chan struct{})}
}

type assemblyOwnerScope struct {
	mu            sync.Mutex
	active        bool
	violated      bool
	registerOwner func(ownerClose) error
}

type assemblyOwnerRegistrar func(ownerClose) error

// registerStartedOwner transfers shutdown ownership before starting a
// background service. If registration fails, the service remains inert and
// the caller still owns its close action. Once this returns nil, the registry
// is the only shutdown owner and reverse-order rollback will stop the service.
func registerStartedOwner(register func() error, start func()) error {
	if register == nil || start == nil {
		return errors.New("started assembly owner registration is invalid")
	}
	if err := register(); err != nil {
		return err
	}
	start()
	return nil
}

func newAssemblyOwnerScope(stage assemblyStage, owners *ownershipRegistry) *assemblyOwnerScope {
	return &assemblyOwnerScope{
		active: true,
		registerOwner: func(close ownerClose) error {
			return owners.register(stage, close)
		},
	}
}

func (scope *assemblyOwnerScope) registrar() assemblyOwnerRegistrar {
	return func(close ownerClose) error {
		return scope.register(close)
	}
}

func (scope *assemblyOwnerScope) register(close ownerClose) error {
	if scope == nil {
		return errors.New("assembly owner scope is invalid")
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if !scope.active {
		scope.violated = true
		return errors.New("assembly owner scope is inactive")
	}
	if scope.registerOwner == nil {
		return errors.New("assembly owner scope is invalid")
	}
	return scope.registerOwner(close)
}

func (scope *assemblyOwnerScope) finish() {
	if scope == nil {
		return
	}
	scope.mu.Lock()
	scope.active = false
	scope.mu.Unlock()
}

func (scope *assemblyOwnerScope) wasViolated() bool {
	if scope == nil {
		return true
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.violated
}

func anyAssemblyScopeViolated(scopes []*assemblyOwnerScope) bool {
	for _, scope := range scopes {
		if scope.wasViolated() {
			return true
		}
	}
	return false
}

func (registry *ownershipRegistry) register(stage assemblyStage, close ownerClose) error {
	if registry == nil || !stage.valid() || close == nil {
		return errors.New("assembly owner registration is invalid")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if !registry.accepting {
		return errors.New("assembly ownership registry is sealed")
	}
	registry.records = append(registry.records, ownershipRecord{stage: stage, close: close})
	return nil
}

func (registry *ownershipRegistry) registrationCount() int {
	if registry == nil {
		return 0
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.records)
}

func (registry *ownershipRegistry) seal() error {
	if registry == nil {
		return errors.New("assembly ownership registry is unavailable")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if !registry.accepting {
		return errors.New("assembly ownership registry is already sealed")
	}
	registry.accepting = false
	return nil
}

func (registry *ownershipRegistry) closeAll() error {
	return closeOwnershipRegistry(registry, context.Background())
}

// closeOwnershipRegistry is the single implementation behind both normal
// Runtime shutdown and failed Assembly construction.  The registry remains
// the owner of the exact same records in both paths; the optional context only
// supplies a detached rollback deadline.  Individual owners already enforce
// their own bounded Close contract, so a deadline is recorded while the
// reverse walk still invokes every registered owner exactly once.
func closeOwnershipRegistry(registry *ownershipRegistry, cleanupCtx context.Context) error {
	if registry == nil {
		return nil
	}
	if cleanupCtx == nil {
		cleanupCtx = context.Background()
	}
	registry.closeOnce.Do(func() {
		registry.mu.Lock()
		registry.accepting = false
		records := append([]ownershipRecord(nil), registry.records...)
		registry.mu.Unlock()

		failures := make([]error, 0)
		timedOut := false
		for index := len(records) - 1; index >= 0; index-- {
			if cleanupCtx.Err() != nil && !timedOut {
				failures = append(failures, errAssemblyRollbackTimeout)
				timedOut = true
			}
			if err := invokeOwnershipClose(cleanupCtx, records[index].close); errors.Is(err, errAssemblyRollbackTimeout) {
				if !timedOut {
					failures = append(failures, errAssemblyRollbackTimeout)
					timedOut = true
				}
			} else if err != nil {
				failures = append(failures, errors.New("assembly "+records[index].stage.label()+" owner close failed"))
			}
		}
		if cleanupCtx.Err() != nil && !timedOut {
			failures = append(failures, errAssemblyRollbackTimeout)
		}
		result := errors.Join(failures...)
		if timedOut && len(failures) == 1 {
			result = errAssemblyRollbackTimeout
		}
		registry.resultMu.Lock()
		registry.result = result
		registry.resultMu.Unlock()
	})
	return ownershipRegistryCloseResult(registry)
}

// invokeOwnershipClose prevents one broken owner that ignores cleanupCtx from
// blocking every earlier record in the reverse walk. Correct production
// owners still finish through the result path; after the hard deadline, the
// registry continues invoking the remaining owners exactly once. The buffered
// result lets a late, contract-violating owner return without blocking.
func invokeOwnershipClose(cleanupCtx context.Context, close ownerClose) error {
	if close == nil {
		return nil
	}
	result := make(chan error, 1)
	go func() { result <- close(cleanupCtx) }()
	select {
	case err := <-result:
		return err
	case <-cleanupCtx.Done():
		return errAssemblyRollbackTimeout
	}
}

func ownershipRegistryCloseResult(registry *ownershipRegistry) error {
	if registry == nil {
		return nil
	}
	registry.resultMu.Lock()
	defer registry.resultMu.Unlock()
	return registry.result
}

// closeOwnershipRegistryForRuntime starts the one process-level cleanup
// worker on the same registry used by Assembly rollback. The caller context
// only controls this wait; the detached worker owns the bounded cleanup
// context and caches the final registry result for later Close calls.
func closeOwnershipRegistryForRuntime(registry *ownershipRegistry, waitCtx context.Context, timeout time.Duration) error {
	if registry == nil {
		return nil
	}
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	if timeout <= 0 || timeout > assemblyRollbackHardTimeout {
		timeout = assemblyRollbackHardTimeout
	}
	registry.runtimeCloseOnce.Do(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(waitCtx), timeout)
		go func() {
			_ = closeOwnershipRegistry(registry, cleanupCtx)
			cancel()
			close(registry.closeDone)
		}()
	})
	select {
	case <-registry.closeDone:
		return ownershipRegistryCloseResult(registry)
	case <-waitCtx.Done():
		select {
		case <-registry.closeDone:
			return ownershipRegistryCloseResult(registry)
		default:
			return waitCtx.Err()
		}
	}
}

// rollbackOwnershipRegistry detaches cancellation (while retaining any
// request-scoped values useful to owner diagnostics), then bounds the wait for
// the one registry close walk. The close walk receives that exact context; it
// does not silently fall back to the caller's canceled context.
func rollbackOwnershipRegistry(source context.Context, registry *ownershipRegistry, timeout time.Duration) error {
	if registry == nil {
		return nil
	}
	if source == nil {
		source = context.Background()
	}
	if timeout <= 0 || timeout > assemblyRollbackHardTimeout {
		timeout = assemblyRollbackHardTimeout
	}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(source), timeout)
	defer cancel()
	return closeOwnershipRegistry(registry, rollbackCtx)
}

// Close converges every Runtime shutdown request on the ownership registry
// created and sealed by Assembly.Build.  T4.26 adds the App-level dual-context
// protocol; this boundary already guarantees that no copied close table or
// second registry can participate in normal shutdown.
func (runtime *Runtime) Close(ctx context.Context) error {
	if runtime == nil || runtime.owners == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("runtime close context is unavailable")
	}
	timeout := runtime.cleanupTimeout
	if timeout <= 0 || timeout > assemblyRollbackHardTimeout {
		timeout = assemblyRollbackHardTimeout
	}
	return closeOwnershipRegistryForRuntime(runtime.owners, ctx, timeout)
}

// Run starts the one App/TUI instance published by Assembly.Build. It does
// not create or replace an App, ownership registry, provider, or transport.
// Runtime.Close remains the sole shutdown path and is intentionally left to
// the caller so startup errors and TUI errors share identical cleanup.
func (runtime *Runtime) Run() (tea.Model, error) {
	if runtime == nil || runtime.ui == nil || runtime.ui.model == nil {
		return nil, errors.New("runtime UI is unavailable")
	}
	return tui.Run(runtime.ui.model)
}
