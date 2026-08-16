package subagent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type admissionSettlementTask struct {
	id       ID
	metadata PreparedMetadata

	settleCalls atomic.Int32
	mu          sync.Mutex
	settleInput []RunResult
	deadlines   []time.Time
	order       *[]string
	orderMu     *sync.Mutex

	observerAction func()
	runStarted     chan struct{}
	runContinue    chan struct{}
	settlePanic    any
	panicCount     int
	invalidCount   int
	owned          bool
	retryEntered   chan struct{}
	retryContinue  chan struct{}
	retryOnce      sync.Once
}

func (task *admissionSettlementTask) Metadata() PreparedMetadata {
	return task.metadata
}

func (task *admissionSettlementTask) Run(context.Context, EventSink) RunResult {
	if task.runStarted != nil {
		close(task.runStarted)
	}
	if task.runContinue != nil {
		<-task.runContinue
	}
	return RunResult{Status: StatusCompleted, StopReason: StopCompleted, Summary: managerTestSafe("completed")}
}

func (task *admissionSettlementTask) Settle(ctx context.Context, result RunResult) Completion {
	call := task.settleCalls.Add(1)
	if cause := context.Cause(ctx); cause != nil {
		panic("admission settlement inherited caller cancellation")
	}
	task.mu.Lock()
	task.settleInput = append(task.settleInput, result.Clone())
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Time{}
	}
	task.deadlines = append(task.deadlines, deadline)
	panicValue := task.settlePanic
	shouldPanic := panicValue != nil
	if task.panicCount > 0 {
		task.panicCount--
		shouldPanic = true
		if task.panicCount == 0 {
			task.settlePanic = nil
		}
	}
	shouldReturnInvalid := task.invalidCount > 0
	if shouldReturnInvalid {
		task.invalidCount--
	}
	task.mu.Unlock()
	if task.order != nil {
		task.orderMu.Lock()
		*task.order = append(*task.order, "settle")
		task.orderMu.Unlock()
	}
	if call > 1 && task.retryEntered != nil {
		task.retryOnce.Do(func() { close(task.retryEntered) })
		if task.retryContinue != nil {
			<-task.retryContinue
		}
	}
	if shouldPanic {
		panic(panicValue)
	}
	if shouldReturnInvalid {
		return Completion{}
	}
	task.mu.Lock()
	task.owned = false
	task.mu.Unlock()
	return Completion{
		ID: task.id, Status: result.Status, StopReason: result.StopReason, Summary: result.Summary,
		Usage: result.Usage, Error: cloneSafeError(result.Error), EndedAt: time.Now().UTC(),
	}
}

func (task *admissionSettlementTask) ownsResources() bool {
	task.mu.Lock()
	defer task.mu.Unlock()
	return task.owned
}

func (task *admissionSettlementTask) deadlineSnapshot() []time.Time {
	task.mu.Lock()
	defer task.mu.Unlock()
	return append([]time.Time(nil), task.deadlines...)
}

func (task *admissionSettlementTask) SetFirstProviderRequestObserver(func(context.Context)) {
	if task.observerAction != nil {
		task.observerAction()
	}
}

func (task *admissionSettlementTask) settlementSnapshot() (int, []RunResult) {
	task.mu.Lock()
	defer task.mu.Unlock()
	return int(task.settleCalls.Load()), append([]RunResult(nil), task.settleInput...)
}

func newAdmissionSettlementManager(
	t *testing.T,
	task *admissionSettlementTask,
	inbox *managerTestInbox,
) *Manager {
	t.Helper()
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		task.id = id
		return task, nil
	}}
	service := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("admission-task", "admission-notification")))
	return service.(*Manager)
}

func assertAdmissionSettlementFailure(t *testing.T, task *admissionSettlementTask, inbox *managerTestInbox, order []string) {
	t.Helper()
	calls, inputs := task.settlementSnapshot()
	if calls != 1 || len(inputs) != 1 {
		t.Fatalf("admission settlement calls/inputs = %d/%d", calls, len(inputs))
	}
	result := inputs[0]
	if result.Status != StatusFailed || result.StopReason != StopInternalError || result.Error == nil ||
		result.Error.Code != string(ErrInternal) || result.Error.Source != "subagent" {
		t.Fatalf("admission failure RunResult is unsafe or invalid: %#v", result)
	}
	if !reflect.DeepEqual(order, []string{"settle", "release"}) {
		t.Fatalf("admission cleanup order = %v", order)
	}
	reserved, released, published := inbox.counts()
	if reserved != 0 || released != 1 || published != 0 {
		t.Fatalf("inbox counts = reserved %d released %d published %d", reserved, released, published)
	}
}

func TestAdmissionSettlementMetadataFailureSettlesBeforeReservationRelease(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	inbox := newManagerTestInbox()
	inbox.onRelease = func(ID) {
		orderMu.Lock()
		order = append(order, "release")
		orderMu.Unlock()
	}
	task := &admissionSettlementTask{
		metadata: PreparedMetadata{Type: TypeFork, MaxIterations: 4},
		order:    &order, orderMu: &orderMu,
	}
	manager := newAdmissionSettlementManager(t, task, inbox)

	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground)); err == nil {
		t.Fatal("Submit accepted invalid prepared metadata")
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	assertAdmissionSettlementFailure(t, task, inbox, gotOrder)
}

func TestAdmissionSettlementObserverFailureSettlesBeforeReservationRelease(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	inbox := newManagerTestInbox()
	inbox.onRelease = func(ID) {
		orderMu.Lock()
		order = append(order, "release")
		orderMu.Unlock()
	}
	task := &admissionSettlementTask{
		metadata:       PreparedMetadata{Type: TypeDefined, MaxIterations: 4},
		observerAction: func() { panic("observer installation secret") },
		order:          &order, orderMu: &orderMu,
	}
	manager := newAdmissionSettlementManager(t, task, inbox)

	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground)); err == nil {
		t.Fatal("Submit accepted a failed observer installation")
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	assertAdmissionSettlementFailure(t, task, inbox, gotOrder)
}

func TestAdmissionSettlementQueuedPublishFailureSettlesBeforeReservationRelease(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	inbox := newManagerTestInbox()
	inbox.onRelease = func(ID) {
		orderMu.Lock()
		order = append(order, "release")
		orderMu.Unlock()
	}
	task := &admissionSettlementTask{
		metadata: PreparedMetadata{Type: TypeDefined, MaxIterations: 4},
		order:    &order, orderMu: &orderMu,
	}
	manager := newAdmissionSettlementManager(t, task, inbox)
	task.observerAction = manager.hub.Close

	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground)); err == nil {
		t.Fatal("Submit accepted a failed queued event publication")
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	assertAdmissionSettlementFailure(t, task, inbox, gotOrder)
}

func TestAdmissionSettlementClosedAdmissionUsesCanceledResult(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	inbox := newManagerTestInbox()
	inbox.onRelease = func(ID) {
		orderMu.Lock()
		order = append(order, "release")
		orderMu.Unlock()
	}
	prepareEntered := make(chan struct{})
	prepareContinue := make(chan struct{})
	task := &admissionSettlementTask{
		metadata: PreparedMetadata{Type: TypeDefined, MaxIterations: 4},
		order:    &order, orderMu: &orderMu,
	}
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		task.id = id
		close(prepareEntered)
		<-prepareContinue
		return task, nil
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("admission-task"))).(*Manager)

	submitDone := make(chan error, 1)
	go func() {
		_, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground))
		submitDone <- err
	}()
	select {
	case <-prepareEntered:
	case <-time.After(time.Second):
		t.Fatal("Submit did not reach Prepare")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- manager.Shutdown(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		accepting := manager.accepting
		manager.mu.Unlock()
		if !accepting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Shutdown did not close admission")
		}
		time.Sleep(time.Millisecond)
	}
	close(prepareContinue)
	select {
	case err := <-submitDone:
		if managerErrorCode(err) != ErrShutdown {
			t.Fatalf("closed-admission Submit error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Submit did not finish after admission closed")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after Submit cleanup")
	}

	calls, inputs := task.settlementSnapshot()
	if calls != 1 || len(inputs) != 1 || inputs[0].Status != StatusCancelled ||
		inputs[0].StopReason != StopApplicationClosed || inputs[0].Error == nil ||
		inputs[0].Error.Code != string(ErrCancelled) {
		t.Fatalf("closed-admission settlement = calls %d inputs %#v", calls, inputs)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !reflect.DeepEqual(gotOrder, []string{"settle", "release"}) {
		t.Fatalf("closed-admission cleanup order = %v", gotOrder)
	}
}

func TestAdmissionSettlementSuccessfulSchedulerTransferDoesNotSettleInSubmit(t *testing.T) {
	task := &admissionSettlementTask{
		metadata:   PreparedMetadata{Type: TypeDefined, MaxIterations: 4},
		runStarted: make(chan struct{}), runContinue: make(chan struct{}),
	}
	manager := newAdmissionSettlementManager(t, task, newManagerTestInbox())

	submission, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementBackground))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-task.runStarted:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not take task ownership")
	}
	if calls, _ := task.settlementSnapshot(); calls != 0 {
		t.Fatalf("Submit settled after scheduler ownership transfer: %d", calls)
	}
	close(task.runContinue)
	if _, err := manager.Await(context.Background(), submission.ID); err != nil {
		t.Fatal(err)
	}
	if calls, _ := task.settlementSnapshot(); calls != 1 {
		t.Fatalf("scheduler settlement calls = %d, want 1", calls)
	}
}

func TestAdmissionSettlementPrepareWithoutTaskOnlyReleasesReservation(t *testing.T) {
	inbox := newManagerTestInbox()
	factory := &managerTestRunnerFactory{prepare: func(context.Context, ID, SubmitInput) (PreparedTask, error) {
		return nil, errors.New("prepare failed")
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("admission-task")))

	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground)); err == nil {
		t.Fatal("Submit accepted failed Prepare")
	}
	reserved, released, _ := inbox.counts()
	if reserved != 0 || released != 1 {
		t.Fatalf("Prepare failure reservation counts = %d/%d", reserved, released)
	}
}

func TestAdmissionSettlementPrepareReturningTaskAndErrorSettlesOwnership(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	inbox := newManagerTestInbox()
	inbox.onRelease = func(ID) {
		orderMu.Lock()
		order = append(order, "release")
		orderMu.Unlock()
	}
	task := &admissionSettlementTask{
		metadata: PreparedMetadata{Type: TypeDefined, MaxIterations: 4},
		order:    &order, orderMu: &orderMu,
	}
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		task.id = id
		return task, errors.New("prepare failed after creating ownership")
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("admission-task")))

	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground)); err == nil {
		t.Fatal("Submit accepted failed Prepare")
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	assertAdmissionSettlementFailure(t, task, inbox, gotOrder)
}

func TestAdmissionSettlementPanicQuarantinesOwnershipUntilManagerRecovery(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	inbox := newManagerTestInbox()
	inbox.onRelease = func(ID) {
		orderMu.Lock()
		order = append(order, "release")
		orderMu.Unlock()
	}
	task := &admissionSettlementTask{
		metadata: PreparedMetadata{Type: TypeFork, MaxIterations: 4},
		order:    &order, orderMu: &orderMu, settlePanic: "settlement secret", panicCount: 1, owned: true,
		retryEntered: make(chan struct{}), retryContinue: make(chan struct{}),
	}
	manager := newAdmissionSettlementManager(t, task, inbox)

	_, submitErr := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground))
	if managerErrorCode(submitErr) != ErrInternal || strings.Contains(submitErr.Error(), "settlement secret") ||
		!strings.Contains(submitErr.Error(), "admission cleanup failed") {
		t.Fatalf("Submit cleanup failure is not bounded: %v", submitErr)
	}
	waitManagerSignal(t, task.retryEntered, "admission cleanup recovery")
	if !task.ownsResources() {
		t.Fatal("failed settlement ownership was released without a valid Completion")
	}
	reserved, released, published := inbox.counts()
	if reserved != 1 || released != 0 || published != 0 {
		t.Fatalf("reservation released before recovery: reserved=%d released=%d published=%d", reserved, released, published)
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- manager.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before admission ownership recovery: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(task.retryContinue)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown after admission recovery: %v", err)
	}
	if task.ownsResources() {
		t.Fatal("manager recovery did not release admission ownership")
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !reflect.DeepEqual(gotOrder, []string{"settle", "settle", "release"}) {
		t.Fatalf("panic cleanup order = %v", gotOrder)
	}
	if calls, _ := task.settlementSnapshot(); calls != 2 {
		t.Fatalf("panic settlement attempts = %d, want 2", calls)
	}
	for index, deadline := range task.deadlineSnapshot() {
		if deadline.IsZero() {
			t.Fatalf("settlement attempt %d had no owner cleanup deadline", index+1)
		}
	}
	reserved, released, published = inbox.counts()
	if reserved != 0 || released != 1 || published != 0 {
		t.Fatalf("recovery settlement = reserved=%d released=%d published=%d", reserved, released, published)
	}
}

func TestAdmissionSettlementInvalidCompletionQuarantinesUntilValidRecovery(t *testing.T) {
	inbox := newManagerTestInbox()
	task := &admissionSettlementTask{
		metadata: PreparedMetadata{Type: TypeFork, MaxIterations: 4}, invalidCount: 1, owned: true,
		retryEntered: make(chan struct{}), retryContinue: make(chan struct{}),
	}
	manager := newAdmissionSettlementManager(t, task, inbox)
	_, submitErr := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground))
	if managerErrorCode(submitErr) != ErrInternal || !strings.Contains(submitErr.Error(), "admission cleanup failed") {
		t.Fatalf("Submit invalid-completion cleanup failure = %v", submitErr)
	}
	waitManagerSignal(t, task.retryEntered, "invalid completion recovery")
	if !task.ownsResources() {
		t.Fatal("invalid Completion released ownership")
	}
	if reserved, released, published := inbox.counts(); reserved != 1 || released != 0 || published != 0 {
		t.Fatalf("invalid Completion released reservation: %d/%d/%d", reserved, released, published)
	}
	close(task.retryContinue)
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if task.ownsResources() {
		t.Fatal("valid recovery Completion did not release ownership")
	}
	if calls, _ := task.settlementSnapshot(); calls != 2 {
		t.Fatalf("settlement attempts = %d, want 2", calls)
	}
	for index, deadline := range task.deadlineSnapshot() {
		if deadline.IsZero() {
			t.Fatalf("settlement attempt %d had no owner cleanup deadline", index+1)
		}
	}
	if reserved, released, published := inbox.counts(); reserved != 0 || released != 1 || published != 0 {
		t.Fatalf("valid recovery double-released or published terminal: %d/%d/%d", reserved, released, published)
	}
}

var _ PreparedTask = (*admissionSettlementTask)(nil)
var _ FirstProviderRequestObservable = (*admissionSettlementTask)(nil)
