package testutil

import (
	"context"
	"errors"
	"sync"
	"time"

	"xagent/internal/events"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

var (
	ErrSubagentFixtureScriptExhausted = errors.New("subagent fixture script exhausted")
	ErrSubagentEventConsumerClosed    = errors.New("subagent fixture event consumer closed")
)

// SubagentProviderStep is one deterministic StreamChat result. Gate blocks
// after the request is marked sent, allowing tests to model slow requests and
// cancellation without a real Provider.
type SubagentProviderStep struct {
	Events   []provider.StreamEvent
	StartErr error
	CloseErr error
	Started  chan<- struct{}
	Gate     <-chan struct{}
}

// ScriptedSubagentProvider implements the real Provider contracts while
// returning one detached script step per request.
type ScriptedSubagentProvider struct {
	mu       sync.Mutex
	steps    []SubagentProviderStep
	requests []provider.ChatRequest
}

func NewScriptedSubagentProvider(steps ...SubagentProviderStep) *ScriptedSubagentProvider {
	cloned := make([]SubagentProviderStep, len(steps))
	for index := range steps {
		cloned[index] = cloneSubagentProviderStep(steps[index])
	}
	return &ScriptedSubagentProvider{steps: cloned}
}

func (*ScriptedSubagentProvider) Name() string { return "scripted-subagent-fixture" }

func (fixture *ScriptedSubagentProvider) StreamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	if fixture == nil {
		return nil, ErrSubagentFixtureScriptExhausted
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fixture.mu.Lock()
	fixture.requests = append(fixture.requests, cloneSubagentProviderRequest(request))
	if len(fixture.steps) == 0 {
		fixture.mu.Unlock()
		finishSubagentRequestObserver(request.Observer, false)
		return nil, ErrSubagentFixtureScriptExhausted
	}
	step := cloneSubagentProviderStep(fixture.steps[0])
	fixture.steps = fixture.steps[1:]
	fixture.mu.Unlock()

	signalSubagentFixture(step.Started)
	if step.StartErr != nil {
		finishSubagentRequestObserver(request.Observer, false)
		return nil, step.StartErr
	}
	if request.Observer != nil {
		request.Observer.MarkSent()
	}
	if step.Gate != nil {
		select {
		case <-step.Gate:
		case <-ctx.Done():
			finishSubagentRequestObserver(request.Observer, true)
			return nil, context.Cause(ctx)
		}
	}
	return newScriptedSubagentChatStream(step.Events, step.CloseErr, request.Observer), nil
}

func (fixture *ScriptedSubagentProvider) StreamChatWithOptions(
	ctx context.Context,
	request provider.ChatRequest,
	_ provider.ChatStreamOptions,
) (provider.ChatStream, error) {
	return fixture.StreamChat(ctx, request)
}

func (fixture *ScriptedSubagentProvider) Requests() []provider.ChatRequest {
	if fixture == nil {
		return nil
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	requests := make([]provider.ChatRequest, len(fixture.requests))
	for index := range fixture.requests {
		requests[index] = cloneSubagentProviderRequest(fixture.requests[index])
	}
	return requests
}

type scriptedSubagentChatStream struct {
	events   chan provider.StreamEvent
	observer provider.RequestObserver
	closeErr error
	once     sync.Once
}

func newScriptedSubagentChatStream(
	events []provider.StreamEvent,
	closeErr error,
	observer provider.RequestObserver,
) *scriptedSubagentChatStream {
	stream := &scriptedSubagentChatStream{
		events: make(chan provider.StreamEvent, len(events)), observer: observer, closeErr: closeErr,
	}
	for index := range events {
		stream.events <- cloneSubagentProviderEvent(events[index])
	}
	close(stream.events)
	return stream
}

func (stream *scriptedSubagentChatStream) Events() <-chan provider.StreamEvent {
	if stream == nil {
		return nil
	}
	return stream.events
}

func (stream *scriptedSubagentChatStream) Close(context.Context) error {
	if stream == nil {
		return nil
	}
	stream.once.Do(func() { finishSubagentRequestObserver(stream.observer, true) })
	return stream.closeErr
}

func finishSubagentRequestObserver(observer provider.RequestObserver, sent bool) {
	if observer != nil {
		observer.Finish(sent)
	}
}

func cloneSubagentProviderStep(source SubagentProviderStep) SubagentProviderStep {
	cloned := source
	cloned.Events = cloneSubagentProviderEvents(source.Events)
	return cloned
}

func cloneSubagentProviderEvents(source []provider.StreamEvent) []provider.StreamEvent {
	if source == nil {
		return nil
	}
	cloned := make([]provider.StreamEvent, len(source))
	for index := range source {
		cloned[index] = cloneSubagentProviderEvent(source[index])
	}
	return cloned
}

func cloneSubagentProviderEvent(source provider.StreamEvent) provider.StreamEvent {
	cloned := source
	if source.Error != nil {
		value := *source.Error
		cloned.Error = &value
	}
	if source.Usage != nil {
		value := *source.Usage
		cloned.Usage = &value
	}
	if source.ToolCall != nil {
		value := *source.ToolCall
		cloned.ToolCall = &value
	}
	return cloned
}

func cloneSubagentProviderRequest(source provider.ChatRequest) provider.ChatRequest {
	cloned := source
	cloned.System = append([]provider.SystemBlock(nil), source.System...)
	cloned.StableSystem = append([]provider.SystemBlock(nil), source.StableSystem...)
	cloned.DynamicSystem = append([]provider.SystemBlock(nil), source.DynamicSystem...)
	cloned.Messages = append([]provider.ModelMessage(nil), source.Messages...)
	if source.Tools != nil {
		cloned.Tools = make([]provider.ToolDefinition, len(source.Tools))
		for index := range source.Tools {
			cloned.Tools[index] = source.Tools[index]
			cloned.Tools[index].Schema = cloneSubagentToolSchema(source.Tools[index].Schema)
		}
	}
	// Captures are inspection values, never executable capabilities.
	cloned.ToolDefs = nil
	cloned.Observer = nil
	return cloned
}

func cloneSubagentToolSchema(source tool.Schema) tool.Schema {
	cloned := source
	cloned.Required = append([]string(nil), source.Required...)
	cloned.Raw = append([]byte(nil), source.Raw...)
	if source.Properties != nil {
		cloned.Properties = make(map[string]tool.SchemaProperty, len(source.Properties))
		for key, property := range source.Properties {
			property.Enum = append([]string(nil), property.Enum...)
			cloned.Properties[key] = property
		}
	}
	return cloned
}

// ManualSubagentClock is a concurrency-safe deterministic time source.
type ManualSubagentClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewManualSubagentClock(now time.Time) *ManualSubagentClock {
	return &ManualSubagentClock{now: now}
}

func (clock *ManualSubagentClock) Now() time.Time {
	if clock == nil {
		return time.Time{}
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *ManualSubagentClock) Set(now time.Time) {
	if clock == nil {
		return
	}
	clock.mu.Lock()
	clock.now = now
	clock.mu.Unlock()
}

func (clock *ManualSubagentClock) Advance(delta time.Duration) time.Time {
	if clock == nil {
		return time.Time{}
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(delta)
	now := clock.now
	clock.mu.Unlock()
	return now
}

type SubagentIDResult struct {
	ID  subagent.ID
	Err error
}

// ScriptedSubagentIDGenerator fails explicitly when its bounded script is
// exhausted, rather than silently generating an unexpected identity.
type ScriptedSubagentIDGenerator struct {
	mu      sync.Mutex
	results []SubagentIDResult
	calls   int
}

func NewScriptedSubagentIDGenerator(results ...SubagentIDResult) *ScriptedSubagentIDGenerator {
	return &ScriptedSubagentIDGenerator{results: append([]SubagentIDResult(nil), results...)}
}

func (generator *ScriptedSubagentIDGenerator) Generate() (subagent.ID, error) {
	if generator == nil {
		return "", ErrSubagentFixtureScriptExhausted
	}
	generator.mu.Lock()
	defer generator.mu.Unlock()
	generator.calls++
	if len(generator.results) == 0 {
		return "", ErrSubagentFixtureScriptExhausted
	}
	result := generator.results[0]
	generator.results = generator.results[1:]
	return result.ID, result.Err
}

func (generator *ScriptedSubagentIDGenerator) Calls() int {
	if generator == nil {
		return 0
	}
	generator.mu.Lock()
	defer generator.mu.Unlock()
	return generator.calls
}

type SubagentRunStep struct {
	Metadata            subagent.PreparedMetadata
	PrepareErr          error
	Started             chan<- struct{}
	BeforeWait          []subagent.AgentEvent
	Wait                <-chan struct{}
	AfterWait           []subagent.AgentEvent
	FirstRequestGate    <-chan struct{}
	MoveToBackground    func() bool
	ResolveConfirmation func(events.ToolConfirmationDecision) error
	Completion          subagent.Completion
}

type SubagentRunnerCall struct {
	ID    subagent.ID
	Input subagent.SubmitInput
}

// ScriptedSubagentRunner implements RunnerFactory and all optional task
// controllers needed by Manager lifecycle tests.
type ScriptedSubagentRunner struct {
	mu    sync.Mutex
	clock func() time.Time
	steps []SubagentRunStep
	calls []SubagentRunnerCall
}

func NewScriptedSubagentRunner(clock func() time.Time, steps ...SubagentRunStep) *ScriptedSubagentRunner {
	if clock == nil {
		clock = time.Now
	}
	cloned := make([]SubagentRunStep, len(steps))
	for index := range steps {
		cloned[index] = cloneSubagentRunStep(steps[index])
	}
	return &ScriptedSubagentRunner{clock: clock, steps: cloned}
}

func (runner *ScriptedSubagentRunner) Prepare(
	ctx context.Context,
	id subagent.ID,
	input subagent.SubmitInput,
) (subagent.PreparedTask, error) {
	if runner == nil {
		return nil, ErrSubagentFixtureScriptExhausted
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	runner.mu.Lock()
	runner.calls = append(runner.calls, SubagentRunnerCall{ID: id, Input: input})
	if len(runner.steps) == 0 {
		runner.mu.Unlock()
		return nil, ErrSubagentFixtureScriptExhausted
	}
	step := cloneSubagentRunStep(runner.steps[0])
	runner.steps = runner.steps[1:]
	clock := runner.clock
	runner.mu.Unlock()
	if step.PrepareErr != nil {
		return nil, step.PrepareErr
	}
	return &scriptedSubagentPreparedTask{id: id, step: step, clock: clock}, nil
}

func (runner *ScriptedSubagentRunner) Calls() []SubagentRunnerCall {
	if runner == nil {
		return nil
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]SubagentRunnerCall(nil), runner.calls...)
}

func cloneSubagentRunStep(source SubagentRunStep) SubagentRunStep {
	cloned := source
	cloned.BeforeWait = cloneSubagentAgentEvents(source.BeforeWait)
	cloned.AfterWait = cloneSubagentAgentEvents(source.AfterWait)
	cloned.Completion = source.Completion.Clone()
	return cloned
}

func cloneSubagentAgentEvents(source []subagent.AgentEvent) []subagent.AgentEvent {
	if source == nil {
		return nil
	}
	cloned := make([]subagent.AgentEvent, len(source))
	for index := range source {
		cloned[index] = source[index].Clone()
	}
	return cloned
}

type scriptedSubagentPreparedTask struct {
	id    subagent.ID
	step  SubagentRunStep
	clock func() time.Time

	mu                   sync.Mutex
	firstRequestObserver func(context.Context)
	runTemplate          subagent.Completion
	settleOnce           sync.Once
	settled              subagent.Completion
}

func (task *scriptedSubagentPreparedTask) Metadata() subagent.PreparedMetadata {
	if task == nil {
		return subagent.PreparedMetadata{}
	}
	return task.step.Metadata
}

func (task *scriptedSubagentPreparedTask) Run(ctx context.Context, sink subagent.EventSink) subagent.RunResult {
	completion := task.runCompletion(ctx, sink)
	if task != nil {
		task.mu.Lock()
		task.runTemplate = completion.Clone()
		task.mu.Unlock()
	}
	return subagent.RunResult{
		Status: completion.Status, StopReason: completion.StopReason, Summary: completion.Summary,
		Usage: completion.Usage, Error: completion.Error,
	}.Clone()
}

func (task *scriptedSubagentPreparedTask) Settle(_ context.Context, result subagent.RunResult) subagent.Completion {
	if task == nil {
		return subagent.Completion{}
	}
	task.settleOnce.Do(func() {
		task.mu.Lock()
		completion := task.runTemplate.Clone()
		task.mu.Unlock()
		result = result.Clone()
		completion.Status = result.Status
		completion.StopReason = result.StopReason
		completion.Summary = result.Summary
		completion.Usage = result.Usage
		completion.Error = result.Error
		if completion.ID == "" {
			completion.ID = task.id
		}
		if completion.EndedAt.IsZero() {
			completion.EndedAt = task.clock()
		}
		task.settled = completion.Clone()
	})
	return task.settled.Clone()
}

func (task *scriptedSubagentPreparedTask) runCompletion(ctx context.Context, sink subagent.EventSink) subagent.Completion {
	if task == nil {
		return subagent.Completion{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	signalSubagentFixture(task.step.Started)
	if !task.waitFirstRequest(ctx) {
		return task.cancelledCompletion()
	}
	if completion, ok := task.emitAll(ctx, sink, task.step.BeforeWait); !ok {
		return completion
	}
	if task.step.Wait != nil {
		select {
		case <-task.step.Wait:
		case <-ctx.Done():
			return task.cancelledCompletion()
		}
	}
	if completion, ok := task.emitAll(ctx, sink, task.step.AfterWait); !ok {
		return completion
	}
	if err := context.Cause(ctx); err != nil {
		return task.cancelledCompletion()
	}
	completion := task.step.Completion.Clone()
	if completion.Status == "" {
		completion.Status = subagent.StatusCompleted
		completion.StopReason = subagent.StopCompleted
		completion.Summary = redact.NewRuntimeRedactor().Redact("fixture completed")
	}
	if completion.ID == "" {
		completion.ID = task.id
	}
	if completion.EndedAt.IsZero() {
		completion.EndedAt = task.clock()
	}
	return completion
}

func (task *scriptedSubagentPreparedTask) waitFirstRequest(ctx context.Context) bool {
	task.mu.Lock()
	observer := task.firstRequestObserver
	task.mu.Unlock()
	if observer == nil {
		return context.Cause(ctx) == nil
	}
	requestCtx, cancel := context.WithCancel(ctx)
	observer(requestCtx)
	defer cancel()
	if task.step.FirstRequestGate == nil {
		return context.Cause(ctx) == nil
	}
	select {
	case <-task.step.FirstRequestGate:
		return context.Cause(ctx) == nil
	case <-ctx.Done():
		return false
	}
}

func (task *scriptedSubagentPreparedTask) emitAll(
	ctx context.Context,
	sink subagent.EventSink,
	values []subagent.AgentEvent,
) (subagent.Completion, bool) {
	for index := range values {
		if err := context.Cause(ctx); err != nil {
			return task.cancelledCompletion(), false
		}
		if sink == nil || sink(values[index].Clone()) != nil {
			return task.internalCompletion(), false
		}
	}
	return subagent.Completion{}, true
}

func (task *scriptedSubagentPreparedTask) cancelledCompletion() subagent.Completion {
	message := redact.NewRuntimeRedactor().Redact("fixture task cancelled")
	return subagent.Completion{
		ID: task.id, Status: subagent.StatusCancelled, Summary: message, StopReason: subagent.StopCancelled,
		Error: subagent.SafeError(subagent.ErrCancelled, message, true), EndedAt: task.clock(),
	}
}

func (task *scriptedSubagentPreparedTask) internalCompletion() subagent.Completion {
	message := redact.NewRuntimeRedactor().Redact("fixture event consumer failed")
	return subagent.Completion{
		ID: task.id, Status: subagent.StatusFailed, Summary: message, StopReason: subagent.StopInternalError,
		Error: subagent.SafeError(subagent.ErrInternal, message, false), EndedAt: task.clock(),
	}
}

func (task *scriptedSubagentPreparedTask) MoveToBackground() bool {
	return task != nil && (task.step.MoveToBackground == nil || task.step.MoveToBackground())
}

func (task *scriptedSubagentPreparedTask) ResolveConfirmation(decision events.ToolConfirmationDecision) error {
	if task == nil || task.step.ResolveConfirmation == nil {
		return nil
	}
	return task.step.ResolveConfirmation(decision)
}

func (task *scriptedSubagentPreparedTask) SetFirstProviderRequestObserver(observer func(context.Context)) {
	if task == nil {
		return
	}
	task.mu.Lock()
	task.firstRequestObserver = observer
	task.mu.Unlock()
}

type SubagentEventConsumerOptions struct {
	After     uint64
	MaxEvents int
}

// SubagentEventConsumer continuously drains a Service subscription without a
// real TUI and retains only the newest configured number of detached events.
type SubagentEventConsumer struct {
	cancel context.CancelFunc
	done   chan struct{}
	notify chan struct{}

	mu        sync.Mutex
	maxEvents int
	events    []subagent.Event
	dropped   uint64
	closed    bool
}

func NewSubagentEventConsumer(
	ctx context.Context,
	service subagent.Service,
	options SubagentEventConsumerOptions,
) (*SubagentEventConsumer, error) {
	if ctx == nil || service == nil || options.MaxEvents <= 0 || options.MaxEvents > 1_000_000 {
		return nil, errors.New("subagent fixture event consumer options are invalid")
	}
	consumerCtx, cancel := context.WithCancel(ctx)
	stream, err := service.Subscribe(consumerCtx, options.After)
	if err != nil {
		cancel()
		return nil, err
	}
	if stream == nil {
		cancel()
		return nil, errors.New("subagent fixture event stream is unavailable")
	}
	consumer := &SubagentEventConsumer{
		cancel: cancel, done: make(chan struct{}), notify: make(chan struct{}, 1), maxEvents: options.MaxEvents,
	}
	go consumer.consume(consumerCtx, stream)
	return consumer, nil
}

func (consumer *SubagentEventConsumer) consume(ctx context.Context, stream <-chan subagent.Event) {
	defer close(consumer.done)
	defer func() {
		consumer.mu.Lock()
		consumer.closed = true
		consumer.mu.Unlock()
		consumer.signal()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-stream:
			if !open {
				return
			}
			consumer.append(event)
		}
	}
}

func (consumer *SubagentEventConsumer) append(event subagent.Event) {
	consumer.mu.Lock()
	if len(consumer.events) == consumer.maxEvents {
		copy(consumer.events, consumer.events[1:])
		consumer.events[len(consumer.events)-1] = event.Clone()
		consumer.dropped++
	} else {
		consumer.events = append(consumer.events, event.Clone())
	}
	consumer.mu.Unlock()
	consumer.signal()
}

func (consumer *SubagentEventConsumer) signal() {
	select {
	case consumer.notify <- struct{}{}:
	default:
	}
}

func (consumer *SubagentEventConsumer) WaitForKind(ctx context.Context, kind subagent.EventKind) (subagent.Event, error) {
	if ctx == nil {
		return subagent.Event{}, errors.New("subagent fixture wait context is invalid")
	}
	for {
		consumer.mu.Lock()
		for index := range consumer.events {
			if consumer.events[index].Kind == kind {
				event := consumer.events[index].Clone()
				consumer.mu.Unlock()
				return event, nil
			}
		}
		closed := consumer.closed
		consumer.mu.Unlock()
		if closed {
			return subagent.Event{}, ErrSubagentEventConsumerClosed
		}
		select {
		case <-ctx.Done():
			return subagent.Event{}, context.Cause(ctx)
		case <-consumer.notify:
		}
	}
}

func (consumer *SubagentEventConsumer) Events() []subagent.Event {
	if consumer == nil {
		return nil
	}
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	values := make([]subagent.Event, len(consumer.events))
	for index := range consumer.events {
		values[index] = consumer.events[index].Clone()
	}
	return values
}

func (consumer *SubagentEventConsumer) Dropped() uint64 {
	if consumer == nil {
		return 0
	}
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	return consumer.dropped
}

func (consumer *SubagentEventConsumer) Close() {
	if consumer == nil {
		return
	}
	consumer.cancel()
	<-consumer.done
}

func signalSubagentFixture(target chan<- struct{}) {
	if target == nil {
		return
	}
	select {
	case target <- struct{}{}:
	default:
	}
}

var (
	_ provider.Provider                       = (*ScriptedSubagentProvider)(nil)
	_ provider.ChatStreamOptionsProvider      = (*ScriptedSubagentProvider)(nil)
	_ provider.ChatStream                     = (*scriptedSubagentChatStream)(nil)
	_ subagent.RunnerFactory                  = (*ScriptedSubagentRunner)(nil)
	_ subagent.PreparedTask                   = (*scriptedSubagentPreparedTask)(nil)
	_ subagent.PreparedPlacementController    = (*scriptedSubagentPreparedTask)(nil)
	_ subagent.PreparedConfirmationController = (*scriptedSubagentPreparedTask)(nil)
	_ subagent.FirstProviderRequestObservable = (*scriptedSubagentPreparedTask)(nil)
)
