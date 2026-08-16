package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/tool"
)

const maxConcurrentToolWorkers = 4

var concurrentToolWorkerSemaphore = make(chan struct{}, maxConcurrentToolWorkers)

// scheduleToolCallsWithRegistryAndRef is the single Orchestrator entry for
// ordinary built-in and MCP tool calls. Batch execution remains separate so
// later scheduler tasks can change concurrency without duplicating the
// authorization pipeline.
func (o *Orchestrator) scheduleToolCallsWithRegistryAndRef(ctx context.Context, mode RunMode, registry *tool.Registry, calls []tool.Call, ref hook.ExecutionRef, out chan<- events.Event) ([]ToolExecution, StopReason, error) {
	return o.executeToolBatchesWithRegistryAndRef(ctx, mode, registry, makeToolBatches(calls, registry), ref, out)
}

func makeToolBatches(calls []tool.Call, registry *tool.Registry) []ToolBatch {
	batches := make([]ToolBatch, 0, len(calls))
	currentConcurrent := ToolBatch{Concurrent: true}
	flushConcurrent := func() {
		if len(currentConcurrent.Calls) == 0 {
			return
		}
		batches = append(batches, currentConcurrent)
		currentConcurrent = ToolBatch{Concurrent: true}
	}
	for index, call := range calls {
		descriptor, registered := registry.Descriptor(call.Name)
		if registered && descriptor.Policy.AllowsConcurrentExecution() {
			currentConcurrent.Calls = append(currentConcurrent.Calls, indexedToolCall{Call: call, Index: index})
			continue
		}
		flushConcurrent()
		batches = append(batches, ToolBatch{Calls: []indexedToolCall{{Call: call, Index: index}}})
	}
	flushConcurrent()
	return batches
}

func (o *Orchestrator) executeConcurrentPreparedTools(ctx context.Context, prepared []ToolExecution, out chan<- events.Event) []ToolExecution {
	slots := make([]ToolExecution, len(prepared))
	copy(slots, prepared)

	jobs := make(chan int, len(slots))
	for index := range slots {
		if !slots[index].HasResult {
			jobs <- index
		}
	}
	close(jobs)
	workerCount := len(jobs)
	if workerCount > maxConcurrentToolWorkers {
		workerCount = maxConcurrentToolWorkers
	}
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				execution := slots[index]
				select {
				case concurrentToolWorkerSemaphore <- struct{}{}:
					execution = o.executePreparedTool(ctx, execution, out)
					<-concurrentToolWorkerSemaphore
				case <-ctx.Done():
					execution = cancelToolExecutionBeforeStart(execution, ctx.Err())
				}
				slots[index] = execution
			}
		}()
	}
	workers.Wait()
	return slots
}

func (o *Orchestrator) prepareToolExecution(ctx context.Context, mode RunMode, indexed indexedToolCall, out chan<- events.Event) ToolExecution {
	return o.prepareToolExecutionWithRegistryAndRef(ctx, mode, o.registry, indexed, hook.ExecutionRef{}, out)
}

func (o *Orchestrator) prepareToolExecutionWithRegistry(ctx context.Context, mode RunMode, registry *tool.Registry, indexed indexedToolCall, out chan<- events.Event) ToolExecution {
	return o.prepareToolExecutionWithRegistryAndRef(ctx, mode, registry, indexed, hook.ExecutionRef{}, out)
}

func (o *Orchestrator) prepareToolExecutionWithRegistryAndRef(ctx context.Context, mode RunMode, registry *tool.Registry, indexed indexedToolCall, ref hook.ExecutionRef, out chan<- events.Event) ToolExecution {
	call := indexed.Call
	if call.Name != tool.LoadSkillToolName && !emitEvent(ctx, out, events.Event{Type: events.ToolPending, Tool: o.safeToolDisplay(call, events.ToolDisplayPending, "")}) {
		return cancelToolExecutionBeforeStart(ToolExecution{Call: call, State: tool.Prepared, Index: indexed.Index}, ctx.Err())
	}
	if registry == nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.unavailableToolRegistryResult(call), out)
	}
	if _, ok := registry.Get(call.Name); !ok {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.unknownToolResult(call), out)
	}
	descriptor, ok := registry.Descriptor(call.Name)
	if !ok {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.unknownToolResult(call), out)
	}
	validated, err := registry.ValidateCall(call)
	if err != nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.invalidToolArgumentsResult(call, err), out)
	}

	systemRoute := descriptor.Route == tool.RouteSystem
	if systemRoute && call.Name == tool.AgentToolName {
		return ToolExecution{
			Call: call, State: tool.Prepared, Validated: validated, Ref: ref,
			SystemRoute: true, Index: indexed.Index,
		}
	}
	if !systemRoute {
		if o.executor == nil {
			return o.rejectPreparedTool(ctx, call, indexed.Index, o.unavailableToolExecutorResult(call), out)
		}
		if o.authorizer == nil {
			return o.rejectPreparedTool(ctx, call, indexed.Index, o.unavailablePermissionResult(call), out)
		}
		validated, err = o.executor.PrepareCall(ctx, call)
		if err != nil {
			return o.rejectPreparedTool(ctx, call, indexed.Index, o.invalidToolArgumentsResult(call, err), out)
		}
	}
	if mode == RunModePlan && !systemRoute && !isReadOnlyTool(call.Name) {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.permissionDeniedResult(call, planModeDecision(call)), out)
	}

	permissionContext := o.permissionContextWithReadScope(ctx, mode)
	normalized, err := permission.NormalizeArguments(permissionCall(call), validated.Arguments, permissionContext)
	if err != nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.permissionDeniedResult(call, normalizationFailureDecision(call)), out)
	}
	canonicalArguments, err := json.Marshal(normalized.Arguments)
	if err != nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.permissionDeniedResult(call, normalizationFailureDecision(call)), out)
	}
	identity, err := permission.NewCallIdentity(permission.CallIdentityInput{ToolName: call.Name, CanonicalArguments: canonicalArguments})
	if !systemRoute {
		identity, err = o.executor.CallIdentity(validated)
	}
	if err != nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.permissionDeniedResult(call, normalizationFailureDecision(call)), out)
	}
	permissionContext.Identity = identity
	if o.authorizer != nil {
		if hard := o.authorizer.CheckHard(normalized, permissionContext); hard != nil {
			return o.rejectPreparedTool(ctx, call, indexed.Index, o.permissionDeniedResult(call, *hard), out)
		}
	}

	hookInput := hook.NewToolInput(call.ID, call.Name, validated.Arguments)
	if hookDecision := o.hookRuntime().BeforeTool(requestCtxOrBackground(ctx), ref, hookInput); hookDecision.IsDeny() {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.hookDeniedResult(call, hookDecision.Reason()), out)
	}
	execution := ToolExecution{
		Call:        call,
		State:       tool.Prepared,
		Validated:   validated,
		Normalized:  normalized,
		HookInput:   hookInput,
		Ref:         ref,
		SystemRoute: systemRoute,
		Index:       indexed.Index,
	}
	if systemRoute {
		return execution
	}

	decision := o.authorizer.DecideOrdinary(normalized, permissionContext)
	switch decision.Kind {
	case permission.DecisionDeny:
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.permissionDeniedResult(call, decision), out)
	case permission.DecisionAsk:
		confirmationID, decisionCh, releaseConfirmation, ok := o.openToolConfirmation(call.ID)
		if !ok {
			return cancelToolExecutionBeforeStart(execution, errors.New("confirmation unavailable"))
		}
		defer releaseConfirmation()
		confirmation := o.safeConfirmationRequest(call, decision, confirmationID)
		if !emitEvent(ctx, out, events.Event{Type: events.ToolWaitingConfirmation, Tool: o.safeToolDisplay(call, events.ToolDisplayWaitingConfirmation, "等待确认"), Confirmation: confirmation}) {
			return cancelToolExecutionBeforeStart(execution, ctx.Err())
		}
		select {
		case <-ctx.Done():
			// Cancellation before the execution ticket is consumed has no Result.
			return cancelToolExecutionBeforeStart(execution, ctx.Err())
		case userDecision := <-decisionCh:
			decision = o.authorizer.ResolveNormalizedUserDecision(normalized, permissionContext, permissionAction(userDecision))
			if decision.Kind == permission.DecisionDeny {
				return o.rejectPreparedTool(ctx, call, indexed.Index, o.permissionDeniedResult(call, decision), out)
			}
		}
	}
	if !decision.Ticket.Issued() {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.unavailablePermissionTicketResult(call), out)
	}
	execution.Ticket = decision.Ticket
	execution.Scope = decision.Scope
	execution.Source = decision.Source
	return execution
}

func planModeDecision(call tool.Call) permission.Decision {
	return permission.Decision{
		Kind:         permission.DecisionDeny,
		Reason:       permission.ReasonPlanMode,
		Source:       permission.Source{Kind: permission.SourceHardConstraint, Description: "plan mode"},
		UserMessage:  "Plan Mode 下不允许执行写工具或 Bash",
		ModelMessage: "Plan Mode allows only read-only tools.",
		Recoverable:  true,
	}
}

func normalizationFailureDecision(call tool.Call) permission.Decision {
	return permission.Decision{
		Kind:         permission.DecisionDeny,
		Reason:       permission.ReasonConfigError,
		Source:       permission.Source{Kind: permission.SourceHardConstraint, Description: "invalid tool arguments"},
		UserMessage:  "工具参数无法用于权限判断",
		ModelMessage: "Tool arguments are invalid for permission checking.",
		Recoverable:  true,
	}
}

func permissionAction(decision events.ToolConfirmationDecision) permission.UserAction {
	switch decision.Action {
	case events.PermissionAllowOnce:
		return permission.ActionAllowOnce
	case events.PermissionAllowSession:
		return permission.ActionAllowSession
	case events.PermissionAllowPermanent:
		return permission.ActionAllowPermanent
	case events.PermissionCancel:
		return permission.ActionCancel
	case events.PermissionDeny:
		return permission.ActionDeny
	}
	if decision.Allowed {
		return permission.ActionAllowOnce
	}
	return permission.ActionDeny
}

func (o *Orchestrator) permissionContext(mode RunMode) permission.Context {
	permissionMode := o.permissionMode
	if permissionMode == "" {
		permissionMode = permission.ModeDefault
	}
	return permission.Context{ProjectRoot: o.projectRoot(), Mode: permissionMode, PlanMode: mode == RunModePlan}
}

func (o *Orchestrator) permissionContextWithReadScope(ctx context.Context, mode RunMode) permission.Context {
	result := o.permissionContext(mode)
	if scope, ok := tool.ReadScopeFromContext(ctx); ok {
		result.ReadRoots = append([]string(nil), scope.ExtraRoots...)
	}
	return result
}

func permissionCall(call tool.Call) permission.Call {
	return permission.Call{ID: call.ID, Name: call.Name, ArgumentsJSON: call.ArgumentsJSON}
}
