package orchestrator

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/tool"
)

type authorizationStageRecorder struct {
	mu     sync.Mutex
	stages []string
}

func (r *authorizationStageRecorder) add(stage string) {
	r.mu.Lock()
	r.stages = append(r.stages, stage)
	r.mu.Unlock()
}

func (r *authorizationStageRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stages...)
}

func (r *authorizationStageRecorder) reset() {
	r.mu.Lock()
	r.stages = nil
	r.mu.Unlock()
}

type authorizationStageHook struct {
	hook.Runtime
	recorder *authorizationStageRecorder
	decision hook.ToolDecision
}

func (h *authorizationStageHook) BeforeTool(context.Context, hook.ExecutionRef, hook.ToolInput) hook.ToolDecision {
	h.recorder.add("before_hook")
	return h.decision
}

type authorizationStageIssuer struct {
	authority *permission.TicketAuthority
	recorder  *authorizationStageRecorder
}

func (i *authorizationStageIssuer) Issue(callID string, identity permission.CallIdentity) (permission.ExecutionTicket, error) {
	i.recorder.add("issue_ticket")
	return i.authority.Issue(callID, identity)
}

type authorizationStageTool struct {
	name     string
	recorder *authorizationStageRecorder
}

func (t *authorizationStageTool) Name() string        { return t.name }
func (t *authorizationStageTool) Description() string { return "authorization stage fixture" }
func (t *authorizationStageTool) Schema() tool.Schema {
	return tool.ObjectSchema([]string{"value"}, map[string]tool.SchemaProperty{
		"value": tool.StringProperty("fixture value"),
	})
}
func (t *authorizationStageTool) Risk() tool.Risk { return tool.RiskDangerous }
func (t *authorizationStageTool) Execute(_ context.Context, input tool.Input) tool.Result {
	t.recorder.add("executor")
	return tool.Success(input, "authorization fixture complete", "ok", nil)
}

type authorizationStageFixture struct {
	orch     *Orchestrator
	registry *tool.Registry
	recorder *authorizationStageRecorder
	root     string
}

func newAuthorizationStageFixture(t *testing.T, decision hook.ToolDecision) authorizationStageFixture {
	t.Helper()
	root := t.TempDir()
	registry, err := tool.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &authorizationStageRecorder{}
	for _, name := range []string{"StageBuiltin", "mcp__stage__remote"} {
		if err := registry.Register(&authorizationStageTool{name: name, recorder: recorder}); err != nil {
			t.Fatal(err)
		}
	}
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	authority, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	hooks := &authorizationStageHook{Runtime: hook.Noop(), recorder: recorder, decision: decision}
	orch := NewWithOptions(OrchestratorOptions{Registry: registry, Executor: executor, Hooks: hooks})
	executor.TicketVerifier = authority
	orch.authorizer = &permission.Authorizer{
		Session: permission.NewSession(),
		Issuer:  &authorizationStageIssuer{authority: authority, recorder: recorder},
	}
	orch.SetPermissionMode(permission.ModePermissive)
	return authorizationStageFixture{orch: orch, registry: registry, recorder: recorder, root: root}
}

func TestAuthorizationStageOrder(t *testing.T) {
	t.Run("built-in follows hook ticket executor order", func(t *testing.T) {
		fixture := newAuthorizationStageFixture(t, hook.Continue())
		out := make(chan events.Event, 16)
		executions, reason, err := fixture.orch.scheduleToolCallsWithRegistryAndRef(
			context.Background(), RunModeDefault, fixture.registry,
			[]tool.Call{{ID: "builtin", Name: "StageBuiltin", ArgumentsJSON: `{"value":"local"}`}},
			hook.ExecutionRef{ExecutionID: "authorization-order"}, out,
		)
		if err != nil || reason != "" || len(executions) != 1 || executions[0].Result.Status != tool.StatusSuccess {
			t.Fatalf("built-in execution = (%#v, %q, %v)", executions, reason, err)
		}
		assertAuthorizationStages(t, fixture.recorder, "before_hook", "issue_ticket", "executor")
	})

	t.Run("MCP confirms before ticket and uses the same executor boundary", func(t *testing.T) {
		fixture := newAuthorizationStageFixture(t, hook.Continue())
		out := make(chan events.Event, 16)
		type scheduledResult struct {
			executions []ToolExecution
			reason     StopReason
			err        error
		}
		done := make(chan scheduledResult, 1)
		go func() {
			executions, reason, err := fixture.orch.scheduleToolCallsWithRegistryAndRef(
				context.Background(), RunModeDefault, fixture.registry,
				[]tool.Call{{ID: "mcp", Name: "mcp__stage__remote", ArgumentsJSON: `{"value":"remote"}`}},
				hook.ExecutionRef{ExecutionID: "authorization-order"}, out,
			)
			done <- scheduledResult{executions: executions, reason: reason, err: err}
		}()

		var result scheduledResult
		for {
			select {
			case event := <-out:
				if event.Confirmation == nil {
					continue
				}
				assertAuthorizationStages(t, fixture.recorder, "before_hook")
				fixture.recorder.add("confirmation")
				if !fixture.orch.ResolveToolConfirmation(events.ToolConfirmationDecision{
					ConfirmationID: event.Confirmation.ConfirmationID,
					CallID:         "mcp",
					Allowed:        true,
					Action:         events.PermissionAllowOnce,
				}) {
					t.Fatal("MCP confirmation decision was not accepted")
				}
			case result = <-done:
				goto completed
			case <-time.After(5 * time.Second):
				t.Fatal("MCP authorization did not complete")
			}
		}
	completed:
		if result.err != nil || result.reason != "" || len(result.executions) != 1 || result.executions[0].Result.Status != tool.StatusSuccess {
			t.Fatalf("MCP execution = (%#v, %q, %v)", result.executions, result.reason, result.err)
		}
		assertAuthorizationStages(t, fixture.recorder, "before_hook", "confirmation", "issue_ticket", "executor")
	})

	t.Run("schema rejection stops before hook confirmation ticket and executor", func(t *testing.T) {
		fixture := newAuthorizationStageFixture(t, hook.Continue())
		out := make(chan events.Event, 16)
		executions, _, err := fixture.orch.scheduleToolCallsWithRegistryAndRef(
			context.Background(), RunModeDefault, fixture.registry,
			[]tool.Call{{ID: "schema", Name: "StageBuiltin", ArgumentsJSON: `{"value":42}`}},
			hook.ExecutionRef{}, out,
		)
		if err != nil || len(executions) != 1 || executions[0].Result.Error == nil || executions[0].Result.Error.Code != tool.ErrInvalidArguments {
			t.Fatalf("schema rejection = (%#v, %v)", executions, err)
		}
		assertAuthorizationStages(t, fixture.recorder)
		assertNoConfirmationEvent(t, out)
	})

	t.Run("hard and health rejection stop before hook", func(t *testing.T) {
		t.Run("hard blacklist", func(t *testing.T) {
			fixture := newAuthorizationStageFixture(t, hook.Continue())
			out := make(chan events.Event, 16)
			executions, _, err := fixture.orch.scheduleToolCallsWithRegistryAndRef(
				context.Background(), RunModeDefault, fixture.registry,
				[]tool.Call{{ID: "hard", Name: "Bash", ArgumentsJSON: `{"command":"git reset --hard"}`}},
				hook.ExecutionRef{}, out,
			)
			if err != nil || len(executions) != 1 || executions[0].Result.Status != tool.StatusDenied {
				t.Fatalf("hard rejection = (%#v, %v)", executions, err)
			}
			assertAuthorizationStages(t, fixture.recorder)
			assertNoConfirmationEvent(t, out)
		})

		t.Run("missing executor", func(t *testing.T) {
			fixture := newAuthorizationStageFixture(t, hook.Continue())
			fixture.orch.executor = nil
			out := make(chan events.Event, 16)
			executions, _, err := fixture.orch.scheduleToolCallsWithRegistryAndRef(
				context.Background(), RunModeDefault, fixture.registry,
				[]tool.Call{{ID: "health", Name: "StageBuiltin", ArgumentsJSON: `{"value":"local"}`}},
				hook.ExecutionRef{}, out,
			)
			if err != nil || len(executions) != 1 || executions[0].Result.Error == nil || executions[0].Result.Error.Code != tool.ErrToolNotFound {
				t.Fatalf("health rejection = (%#v, %v)", executions, err)
			}
			assertAuthorizationStages(t, fixture.recorder)
			assertNoConfirmationEvent(t, out)
		})
	})

	t.Run("hook denial never confirms or issues even in degraded read-only mode", func(t *testing.T) {
		fixture := newAuthorizationStageFixture(t, hook.Deny("blocked before authorization"))
		if err := os.WriteFile(fixture.root+"/read.txt", []byte("safe"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.orch.authorizer.LoadErrors = []permission.LoadError{{Err: errors.New("degraded fixture")}}
		out := make(chan events.Event, 16)
		executions, _, err := fixture.orch.scheduleToolCallsWithRegistryAndRef(
			context.Background(), RunModeDefault, fixture.registry,
			[]tool.Call{{ID: "hook-deny", Name: "Read", ArgumentsJSON: `{"path":"read.txt"}`}},
			hook.ExecutionRef{}, out,
		)
		if err != nil || len(executions) != 1 || executions[0].Result.Error == nil || executions[0].Result.Error.Code != tool.ErrHookDenied {
			t.Fatalf("hook rejection = (%#v, %v)", executions, err)
		}
		assertAuthorizationStages(t, fixture.recorder, "before_hook")
		assertNoConfirmationEvent(t, out)
	})

	t.Run("argument drift invalidates the ticket before execution", func(t *testing.T) {
		fixture := newAuthorizationStageFixture(t, hook.Continue())
		prepare := func(callID string) ToolExecution {
			t.Helper()
			out := make(chan events.Event, 16)
			execution := fixture.orch.prepareToolExecutionWithRegistryAndRef(
				context.Background(), RunModeDefault, fixture.registry,
				indexedToolCall{Call: tool.Call{ID: callID, Name: "StageBuiltin", ArgumentsJSON: `{"value":"original"}`}},
				hook.ExecutionRef{}, out,
			)
			if execution.HasResult || !execution.Ticket.Issued() {
				t.Fatalf("prepared execution = %#v", execution)
			}
			return execution
		}

		t.Run("changed arguments restart schema validation", func(t *testing.T) {
			execution := prepare("schema-drift")
			fixture.recorder.reset()
			execution.Validated.Call.ArgumentsJSON = `{"value":42}`
			execution = fixture.orch.executePreparedTool(context.Background(), execution, make(chan events.Event, 16))
			if execution.Result.Status != tool.StatusDenied || execution.Result.Error == nil || execution.Result.Error.Code != tool.ErrPermissionDenied {
				t.Fatalf("schema drift result = %#v", execution.Result)
			}
			assertAuthorizationStages(t, fixture.recorder)
		})

		t.Run("schema-valid arguments still require a new identity", func(t *testing.T) {
			execution := prepare("identity-drift")
			fixture.recorder.reset()
			execution.Validated.Call.ArgumentsJSON = `{"value":"changed"}`
			execution = fixture.orch.executePreparedTool(context.Background(), execution, make(chan events.Event, 16))
			if execution.Result.Status != tool.StatusDenied || execution.Result.Error == nil || execution.Result.Error.Code != tool.ErrPermissionDenied {
				t.Fatalf("identity drift result = %#v", execution.Result)
			}
			assertAuthorizationStages(t, fixture.recorder)
		})
	})
}

func assertAuthorizationStages(t *testing.T, recorder *authorizationStageRecorder, want ...string) {
	t.Helper()
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("authorization stages = %v, want %v", got, want)
	}
}

func assertNoConfirmationEvent(t *testing.T, out <-chan events.Event) {
	t.Helper()
	for len(out) > 0 {
		if event := <-out; event.Type == events.ToolWaitingConfirmation || event.Confirmation != nil {
			t.Fatalf("unexpected confirmation event: %#v", event)
		}
	}
}
