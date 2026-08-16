package subagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

type managerTestRunnerFactory struct {
	mu           sync.Mutex
	prepare      func(context.Context, ID, SubmitInput) (PreparedTask, error)
	prepareCalls int
}

func (factory *managerTestRunnerFactory) Prepare(ctx context.Context, id ID, input SubmitInput) (PreparedTask, error) {
	factory.mu.Lock()
	factory.prepareCalls++
	prepare := factory.prepare
	factory.mu.Unlock()
	if prepare == nil {
		return nil, errors.New("manager test prepare function is unavailable")
	}
	return prepare(ctx, id, input)
}

func (factory *managerTestRunnerFactory) Calls() int {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.prepareCalls
}

type managerTestPreparedTask struct {
	metadata PreparedMetadata
	run      func(context.Context, EventSink) Completion
	settle   func(context.Context, RunResult) Completion

	mu            sync.Mutex
	settleOnce    sync.Once
	runCompletion Completion
	settled       Completion
}

func (task *managerTestPreparedTask) Metadata() PreparedMetadata {
	metadata := task.metadata
	if metadata.Type == "" {
		metadata.Type = TypeDefined
	}
	if metadata.MaxIterations == 0 {
		metadata.MaxIterations = 4
	}
	return metadata
}

func (task *managerTestPreparedTask) Run(ctx context.Context, sink EventSink) RunResult {
	completion := task.run(ctx, sink)
	task.mu.Lock()
	task.runCompletion = completion.Clone()
	task.mu.Unlock()
	return RunResult{
		Status: completion.Status, StopReason: completion.StopReason, Summary: completion.Summary,
		Usage: completion.Usage, Error: completion.Error,
	}.Clone()
}

func (task *managerTestPreparedTask) Settle(ctx context.Context, result RunResult) Completion {
	task.settleOnce.Do(func() {
		if task.settle != nil {
			task.settled = task.settle(ctx, result).Clone()
			return
		}
		task.mu.Lock()
		completion := task.runCompletion.Clone()
		task.mu.Unlock()
		completion.Status = result.Status
		completion.StopReason = result.StopReason
		completion.Summary = result.Summary
		completion.Usage = result.Usage
		completion.Error = cloneSafeError(result.Error)
		task.settled = completion
	})
	return task.settled.Clone()
}

type managerTestControlledTask struct {
	*managerTestPreparedTask

	mu             sync.Mutex
	observer       func(context.Context)
	observerSet    chan struct{}
	observerSetOne sync.Once
	move           func() bool
	resolve        func(events.ToolConfirmationDecision) error
	moveCalls      atomic.Int32
	resolveCalls   atomic.Int32
}

func (task *managerTestControlledTask) MoveToBackground() bool {
	task.moveCalls.Add(1)
	if task.move == nil {
		return true
	}
	return task.move()
}

func (task *managerTestControlledTask) ResolveConfirmation(decision events.ToolConfirmationDecision) error {
	task.resolveCalls.Add(1)
	if task.resolve == nil {
		return nil
	}
	return task.resolve(decision)
}

func (task *managerTestControlledTask) SetFirstProviderRequestObserver(observer func(context.Context)) {
	task.mu.Lock()
	task.observer = observer
	task.mu.Unlock()
	if task.observerSet != nil {
		task.observerSetOne.Do(func() { close(task.observerSet) })
	}
}

func (task *managerTestControlledTask) notifyFirstProviderRequest(ctx context.Context) bool {
	task.mu.Lock()
	observer := task.observer
	task.mu.Unlock()
	if observer == nil {
		return false
	}
	observer(ctx)
	return true
}

type managerTestObservableTask struct {
	*managerTestPreparedTask
	mu       sync.Mutex
	observer func(context.Context)
}

func (task *managerTestObservableTask) SetFirstProviderRequestObserver(observer func(context.Context)) {
	task.mu.Lock()
	task.observer = observer
	task.mu.Unlock()
}

func (task *managerTestObservableTask) notifyFirstProviderRequest(ctx context.Context) bool {
	task.mu.Lock()
	observer := task.observer
	task.mu.Unlock()
	if observer == nil {
		return false
	}
	observer(ctx)
	return true
}

var (
	_ PreparedPlacementController    = (*managerTestControlledTask)(nil)
	_ PreparedConfirmationController = (*managerTestControlledTask)(nil)
	_ FirstProviderRequestObservable = (*managerTestControlledTask)(nil)
	_ FirstProviderRequestObservable = (*managerTestObservableTask)(nil)
)

type managerTestInbox struct {
	mu sync.Mutex

	reservations map[ID]ParentRef
	releases     []ID
	published    []ResultNotification
	reserveErr   error
	publishErr   error
	onReserve    func(ID)
	onRelease    func(ID)
	publishReady chan struct{}
	publishTry   chan struct{}
	publishOnce  sync.Once
}

func newManagerTestInbox() *managerTestInbox {
	return &managerTestInbox{
		reservations: make(map[ID]ParentRef),
		publishReady: make(chan struct{}),
		publishTry:   make(chan struct{}, 16),
	}
}

func (inbox *managerTestInbox) Reserve(ctx context.Context, id ID, parent ParentRef) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if inbox.onReserve != nil {
		inbox.onReserve(id)
	}
	if inbox.reserveErr != nil {
		return inbox.reserveErr
	}
	inbox.reservations[id] = parent
	return nil
}

func (inbox *managerTestInbox) ReleaseReservation(_ context.Context, id ID, parent ParentRef) error {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if current, ok := inbox.reservations[id]; ok && current != parent {
		return errors.New("reservation owner mismatch")
	}
	delete(inbox.reservations, id)
	inbox.releases = append(inbox.releases, id)
	if inbox.onRelease != nil {
		inbox.onRelease(id)
	}
	return nil
}

func (inbox *managerTestInbox) Publish(_ context.Context, notification ResultNotification) error {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	select {
	case inbox.publishTry <- struct{}{}:
	default:
	}
	if inbox.publishErr != nil {
		return inbox.publishErr
	}
	delete(inbox.reservations, notification.TaskID)
	inbox.published = append(inbox.published, notification.Clone())
	inbox.publishOnce.Do(func() { close(inbox.publishReady) })
	return nil
}

func (inbox *managerTestInbox) setPublishError(err error) {
	inbox.mu.Lock()
	inbox.publishErr = err
	inbox.mu.Unlock()
}

func (inbox *managerTestInbox) waitPublishAttempts(t *testing.T, count int) {
	t.Helper()
	for attempt := 0; attempt < count; attempt++ {
		select {
		case <-inbox.publishTry:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for result publication attempt %d/%d", attempt+1, count)
		}
	}
}

func (inbox *managerTestInbox) Claim(_ context.Context, options ResultClaimOptions) (ResultClaim, error) {
	return ResultClaim{Owner: options.Owner}, nil
}

func (*managerTestInbox) Ack(context.Context, string, ParentRef) error { return nil }

func (*managerTestInbox) Release(context.Context, string, ParentRef) error { return nil }

func (inbox *managerTestInbox) waitPublished(t *testing.T) ResultNotification {
	t.Helper()
	select {
	case <-inbox.publishReady:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result publication")
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.published) == 0 {
		t.Fatal("publication signal had no notification")
	}
	return inbox.published[len(inbox.published)-1].Clone()
}

func (inbox *managerTestInbox) counts() (reserved, released, published int) {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	return len(inbox.reservations), len(inbox.releases), len(inbox.published)
}

func newManagerUnderTest(t *testing.T, options ManagerOptions) Service {
	t.Helper()
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	return manager
}

func managerTestOptions(factory RunnerFactory, inbox ResultInbox, generator func() (ID, error)) ManagerOptions {
	return ManagerOptions{
		Runner: factory, Limits: managerTestLimits(), Inbox: inbox, Redactor: redact.NewRuntimeRedactor(),
		IDGenerator: generator, ShutdownTimeout: time.Second,
	}
}

func managerTestLimits() Limits {
	limits := DefaultLimits()
	limits.MaxConcurrent = 2
	limits.MaxQueued = 4
	limits.MaxRetainedTasks = 16
	limits.MaxTaskTombstones = 8
	limits.MaxGlobalEvents = 128
	limits.MaxEventsPerTask = 32
	limits.MaxSubscriberBuffer = 32
	limits.AutoBackgroundAfter = 250 * time.Millisecond
	return limits
}

func managerModelInput(taskType ExecutionType, placement PlacementIntent) SubmitInput {
	return SubmitInput{
		Task: "inspect the repository", Type: taskType, Placement: placement, Origin: OriginModel,
		Parent:     ParentRef{ConversationID: "conversation", ExecutionID: "execution", RequestGeneration: 1},
		Invocation: InvocationRef{ToolCallID: "agent-call"},
	}
}

func managerIDSequence(ids ...ID) func() (ID, error) {
	var mu sync.Mutex
	index := 0
	return func() (ID, error) {
		mu.Lock()
		defer mu.Unlock()
		if index >= len(ids) {
			return "", errors.New("manager test ID sequence exhausted")
		}
		id := ids[index]
		index++
		return id, nil
	}
}

func managerCountingIDGenerator() func() (ID, error) {
	var counter atomic.Int64
	return func() (ID, error) {
		return ID(fmt.Sprintf("manager-id-%04d", counter.Add(1))), nil
	}
}

func managerTestSafe(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}

func managerErrorCode(err error) ErrorCode {
	var safe *diagnostics.SafeError
	if errors.As(err, &safe) {
		return ErrorCode(safe.Code)
	}
	return ""
}

func waitManagerSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitManagerStatus(t *testing.T, manager Service, id ID, wanted Status) TaskSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		detail, err := manager.Get(context.Background(), id)
		if err == nil && detail.Task.Status == wanted {
			return detail.Task
		}
		time.Sleep(time.Millisecond)
	}
	detail, err := manager.Get(context.Background(), id)
	t.Fatalf("task %s did not reach %s: detail=%#v error=%v", id, wanted, detail, err)
	return TaskSnapshot{}
}
