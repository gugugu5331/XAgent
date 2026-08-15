package orchestrator

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/tool"
)

type schedulerPolicyTool struct {
	name string
	risk tool.Risk
}

func (t *schedulerPolicyTool) Name() string        { return t.name }
func (t *schedulerPolicyTool) Description() string { return "scheduler policy fixture" }
func (t *schedulerPolicyTool) Schema() tool.Schema { return tool.ObjectSchema(nil, nil) }
func (t *schedulerPolicyTool) Risk() tool.Risk     { return t.risk }
func (t *schedulerPolicyTool) Execute(_ context.Context, input tool.Input) tool.Result {
	return tool.Success(input, "scheduler policy fixture", "ok", nil)
}

func TestSchedulerSeparatesRiskFromConcurrency(t *testing.T) {
	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	register := func(name string, risk tool.Risk, policy tool.ExecutionPolicy, remote json.RawMessage) {
		t.Helper()
		if err := registry.RegisterWithOptions(
			&schedulerPolicyTool{name: name, risk: risk},
			tool.RegistrationOptions{Policy: policy, RemoteAnnotations: remote},
		); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	concurrent := tool.ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}
	register("SafeRiskWithoutPolicy", tool.RiskSafe, tool.ExecutionPolicy{}, nil)
	register("DangerousWithPolicy", tool.RiskDangerous, concurrent, nil)
	register("SafeWithPolicy", tool.RiskSafe, concurrent, nil)
	register("RemotePositivePreservesLocal", tool.RiskDangerous, concurrent, json.RawMessage(`{"readOnlyHint":true,"idempotentHint":true}`))
	register("ReadOnlyOnly", tool.RiskSafe, tool.ExecutionPolicy{ReadOnly: true}, nil)
	register("ConcurrentSafeOnly", tool.RiskSafe, tool.ExecutionPolicy{ConcurrentSafe: true}, nil)
	register("RemoteCannotLoosen", tool.RiskSafe, tool.ExecutionPolicy{}, json.RawMessage(`{"readOnlyHint":true,"idempotentHint":true}`))
	register("RemoteTightens", tool.RiskSafe, concurrent, json.RawMessage(`{"readOnlyHint":false}`))
	register("DangerousConcurrentTail", tool.RiskDangerous, concurrent, nil)

	calls := []tool.Call{
		{ID: "safe-risk-serial", Name: "SafeRiskWithoutPolicy"},
		{ID: "dangerous-concurrent", Name: "DangerousWithPolicy"},
		{ID: "safe-concurrent", Name: "SafeWithPolicy"},
		{ID: "remote-positive-local", Name: "RemotePositivePreservesLocal"},
		{ID: "read-only-serial", Name: "ReadOnlyOnly"},
		{ID: "concurrent-safe-serial", Name: "ConcurrentSafeOnly"},
		{ID: "remote-claimed-serial", Name: "RemoteCannotLoosen"},
		{ID: "remote-tightened-serial", Name: "RemoteTightens"},
		{ID: "unknown-serial", Name: "Unknown"},
		{ID: "dangerous-concurrent-tail", Name: "DangerousConcurrentTail"},
	}
	batches := makeToolBatches(calls, registry)

	want := []struct {
		concurrent bool
		callIDs    []string
	}{
		{callIDs: []string{"safe-risk-serial"}},
		{concurrent: true, callIDs: []string{"dangerous-concurrent", "safe-concurrent", "remote-positive-local"}},
		{callIDs: []string{"read-only-serial"}},
		{callIDs: []string{"concurrent-safe-serial"}},
		{callIDs: []string{"remote-claimed-serial"}},
		{callIDs: []string{"remote-tightened-serial"}},
		{callIDs: []string{"unknown-serial"}},
		{concurrent: true, callIDs: []string{"dangerous-concurrent-tail"}},
	}
	if len(batches) != len(want) {
		t.Fatalf("batch count = %d, want %d: %#v", len(batches), len(want), batches)
	}
	for index, expected := range want {
		got := batches[index]
		gotIDs := make([]string, len(got.Calls))
		for callIndex, indexed := range got.Calls {
			gotIDs[callIndex] = indexed.Call.ID
			if indexed.Index < 0 || indexed.Index >= len(calls) || calls[indexed.Index].ID != indexed.Call.ID {
				t.Fatalf("batch %d lost original index: %#v", index, indexed)
			}
		}
		if got.Concurrent != expected.concurrent || !reflect.DeepEqual(gotIDs, expected.callIDs) {
			t.Fatalf("batch %d = concurrent:%v calls:%v, want concurrent:%v calls:%v", index, got.Concurrent, gotIDs, expected.concurrent, expected.callIDs)
		}
	}

	for _, name := range []string{"RemoteCannotLoosen", "RemoteTightens"} {
		descriptor, ok := registry.Descriptor(name)
		if !ok || descriptor.Policy.AllowsConcurrentExecution() {
			t.Fatalf("remote annotations loosened scheduling policy for %s: %#v", name, descriptor.Policy)
		}
	}
}

type concurrentSchedulerProbe struct {
	mu       sync.Mutex
	active   int
	max      int
	started  chan string
	releases map[string]chan struct{}
}

func newConcurrentSchedulerProbe(ids []string) *concurrentSchedulerProbe {
	releases := make(map[string]chan struct{}, len(ids))
	for _, id := range ids {
		releases[id] = make(chan struct{})
	}
	return &concurrentSchedulerProbe{started: make(chan string, len(ids)), releases: releases}
}

func (*concurrentSchedulerProbe) Name() string { return "ConcurrentProbe" }
func (*concurrentSchedulerProbe) Description() string {
	return "controlled scheduler concurrency fixture"
}
func (*concurrentSchedulerProbe) Schema() tool.Schema {
	return tool.ObjectSchema([]string{"id"}, map[string]tool.SchemaProperty{
		"id": tool.StringProperty("call identity"),
	})
}
func (*concurrentSchedulerProbe) Risk() tool.Risk { return tool.RiskDangerous }
func (p *concurrentSchedulerProbe) Execute(_ context.Context, input tool.Input) tool.Result {
	id, _ := input.Arguments["id"].(string)
	p.mu.Lock()
	p.active++
	if p.active > p.max {
		p.max = p.active
	}
	p.mu.Unlock()
	p.started <- id
	<-p.releases[id]
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return tool.Success(input, "completed "+id, id, nil)
}

func (p *concurrentSchedulerProbe) maxActive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

type concurrentScheduleResult struct {
	executions []ToolExecution
	reason     StopReason
	err        error
}

func TestConcurrentToolsOverlapAndCommitInCallOrder(t *testing.T) {
	t.Run("completion events may be out of order but joined slots are ordered", func(t *testing.T) {
		ids := []string{"first", "second"}
		fixture := newConcurrentSchedulerFixture(t, ids)
		out := make(chan events.Event, 32)
		done := runConcurrentSchedule(fixture.orch, fixture.registry, ids, out)

		started := map[string]bool{}
		for len(started) < len(ids) {
			started[waitForStartedTool(t, fixture.probe)] = true
		}
		if len(started) != 2 || fixture.probe.maxActive() != 2 {
			t.Fatalf("tools did not overlap: started=%v max=%d", started, fixture.probe.maxActive())
		}

		close(fixture.probe.releases["second"])
		waitForToolCompletion(t, out, "second")
		close(fixture.probe.releases["first"])
		waitForToolCompletion(t, out, "first")
		result := waitForConcurrentSchedule(t, done)
		assertScheduledCallOrder(t, result, ids)
	})

	t.Run("shared semaphore caps workers and every slot keeps its ordinal", func(t *testing.T) {
		ids := make([]string, maxConcurrentToolWorkers+2)
		for index := range ids {
			ids[index] = "call-" + string(rune('a'+index))
		}
		fixture := newConcurrentSchedulerFixture(t, ids)
		out := make(chan events.Event, len(ids)*4)
		done := runConcurrentSchedule(fixture.orch, fixture.registry, ids, out)

		started := make([]string, 0, len(ids))
		for len(started) < maxConcurrentToolWorkers {
			started = append(started, waitForStartedTool(t, fixture.probe))
		}
		if got := fixture.probe.maxActive(); got != maxConcurrentToolWorkers {
			t.Fatalf("maximum active workers = %d, want %d", got, maxConcurrentToolWorkers)
		}
		select {
		case unexpected := <-fixture.probe.started:
			t.Fatalf("worker limit exceeded before a slot was released: %s", unexpected)
		default:
		}
		for _, id := range started {
			close(fixture.probe.releases[id])
		}
		for len(started) < len(ids) {
			id := waitForStartedTool(t, fixture.probe)
			started = append(started, id)
			close(fixture.probe.releases[id])
		}
		result := waitForConcurrentSchedule(t, done)
		assertScheduledCallOrder(t, result, ids)
		if got := fixture.probe.maxActive(); got > maxConcurrentToolWorkers {
			t.Fatalf("maximum active workers = %d, limit %d", got, maxConcurrentToolWorkers)
		}
	})
}

type concurrentSchedulerFixture struct {
	orch     *Orchestrator
	registry *tool.Registry
	probe    *concurrentSchedulerProbe
}

func newConcurrentSchedulerFixture(t *testing.T, ids []string) concurrentSchedulerFixture {
	t.Helper()
	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe := newConcurrentSchedulerProbe(ids)
	if err := registry.RegisterWithOptions(probe, tool.RegistrationOptions{
		Policy: tool.ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true},
	}); err != nil {
		t.Fatal(err)
	}
	executor := tool.NewExecutor(registry, t.TempDir(), 10*time.Second, 4096)
	orch := NewWithOptions(OrchestratorOptions{Registry: registry, Executor: executor, Hooks: hook.Noop()})
	orch.SetPermissionMode(permission.ModePermissive)
	return concurrentSchedulerFixture{orch: orch, registry: registry, probe: probe}
}

func runConcurrentSchedule(orch *Orchestrator, registry *tool.Registry, ids []string, out chan<- events.Event) <-chan concurrentScheduleResult {
	done := make(chan concurrentScheduleResult, 1)
	go func() {
		calls := make([]tool.Call, len(ids))
		for index, id := range ids {
			calls[index] = tool.Call{ID: id, Name: "ConcurrentProbe", ArgumentsJSON: `{"id":"` + id + `"}`}
		}
		executions, reason, err := orch.scheduleToolCallsWithRegistryAndRef(context.Background(), RunModeDefault, registry, calls, hook.ExecutionRef{}, out)
		done <- concurrentScheduleResult{executions: executions, reason: reason, err: err}
	}()
	return done
}

func waitForStartedTool(t *testing.T, probe *concurrentSchedulerProbe) string {
	t.Helper()
	select {
	case id := <-probe.started:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a concurrent tool to start")
		return ""
	}
}

func waitForToolCompletion(t *testing.T, out <-chan events.Event, callID string) {
	t.Helper()
	for {
		select {
		case event := <-out:
			if event.Type == events.ToolSuccess && event.Tool != nil && event.Tool.CallID == callID {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for completion event %s", callID)
		}
	}
}

func waitForConcurrentSchedule(t *testing.T, done <-chan concurrentScheduleResult) concurrentScheduleResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for concurrent schedule")
		return concurrentScheduleResult{}
	}
}

func assertScheduledCallOrder(t *testing.T, result concurrentScheduleResult, want []string) {
	t.Helper()
	if result.err != nil || result.reason != "" || len(result.executions) != len(want) {
		t.Fatalf("schedule result = (%#v, %q, %v)", result.executions, result.reason, result.err)
	}
	for index, execution := range result.executions {
		if execution.Index != index || execution.Call.ID != want[index] || execution.Result.Status != tool.StatusSuccess {
			t.Fatalf("slot %d = index:%d call:%s status:%s, want call:%s", index, execution.Index, execution.Call.ID, execution.Result.Status, want[index])
		}
	}
}
