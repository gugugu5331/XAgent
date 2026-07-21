package orchestrator

import (
	"context"
	"fmt"
	"sort"

	"xagent/internal/events"
	"xagent/internal/permission"
	"xagent/internal/tool"
)

type ToolBatch struct {
	Calls      []indexedToolCall
	Concurrent bool
}

type ToolExecution struct {
	Call       tool.Call
	Result     tool.Result
	Index      int
	Grant      *permission.Grant
	StopReason StopReason
	Err        error
}

type indexedToolCall struct {
	Call  tool.Call
	Index int
}

func (o *Orchestrator) filterToolCall(mode RunMode, call tool.Call) (tool.Result, bool) {
	return o.filterToolCallWithRegistry(mode, o.registry, call)
}

func (o *Orchestrator) filterToolCallWithRegistry(mode RunMode, registry *tool.Registry, call tool.Call) (tool.Result, bool) {
	if registry == nil {
		return tool.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  tool.StatusError,
			Summary: "工具注册中心不可用",
			Error:   &tool.Error{Code: tool.ErrToolNotFound, Message: "工具注册中心不可用", Recoverable: true},
		}, true
	}
	registeredTool, ok := registry.Get(call.Name)
	if !ok {
		// Preserve the established Plan Mode denial path: tools hidden from the
		// provider by the read-only view are still rejected by Authorizer with a
		// permission-denied result if a provider fabricates the call.
		if mode == RunModePlan && o.registry != nil {
			if _, exists := o.registry.Get(call.Name); exists && !isReadOnlyTool(call.Name) {
				return tool.Result{}, false
			}
		}
		return tool.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  tool.StatusError,
			Summary: fmt.Sprintf("未知工具: %s", call.Name),
			Error:   &tool.Error{Code: tool.ErrToolNotFound, Message: fmt.Sprintf("工具 %q 未注册", call.Name), Recoverable: true},
		}, true
	}
	if mode == RunModePlan && !isReadOnlyTool(call.Name) {
		return tool.Result{}, false
	}
	_ = registeredTool
	return tool.Result{}, false
}

func makeToolBatches(calls []tool.Call, registry *tool.Registry) []ToolBatch {
	batches := []ToolBatch{}
	currentSafe := ToolBatch{Concurrent: true}
	flushSafe := func() {
		if len(currentSafe.Calls) > 0 {
			batches = append(batches, currentSafe)
			currentSafe = ToolBatch{Concurrent: true}
		}
	}
	for index, call := range calls {
		var registeredTool tool.Tool
		var ok bool
		if registry != nil {
			registeredTool, ok = registry.Get(call.Name)
		}
		if ok && registeredTool.Risk() == tool.RiskSafe {
			currentSafe.Calls = append(currentSafe.Calls, indexedToolCall{Call: call, Index: index})
			continue
		}
		flushSafe()
		batches = append(batches, ToolBatch{Calls: []indexedToolCall{{Call: call, Index: index}}, Concurrent: false})
	}
	flushSafe()
	return batches
}

func (o *Orchestrator) executeToolBatches(ctx context.Context, mode RunMode, batches []ToolBatch, out chan<- events.Event) ([]ToolExecution, StopReason, error) {
	return o.executeToolBatchesWithRegistry(ctx, mode, o.registry, batches, out)
}

func (o *Orchestrator) executeToolBatchesWithRegistry(ctx context.Context, mode RunMode, registry *tool.Registry, batches []ToolBatch, out chan<- events.Event) ([]ToolExecution, StopReason, error) {
	executions := make([]ToolExecution, 0)
	for _, batch := range batches {
		select {
		case <-ctx.Done():
			return executions, StopReasonCancelled, ctx.Err()
		default:
		}
		prepared := make([]ToolExecution, 0, len(batch.Calls))
		for _, indexed := range batch.Calls {
			execution := o.prepareToolExecutionWithRegistry(ctx, mode, registry, indexed, out)
			prepared = append(prepared, execution)
			if execution.Err != nil {
				executions = append(executions, prepared...)
				return executions, execution.StopReason, execution.Err
			}
		}
		for _, execution := range prepared {
			if execution.Result.CallID != "" {
				executions = append(executions, execution)
				continue
			}
			execution = o.executePreparedTool(ctx, execution, out)
			executions = append(executions, execution)
			if execution.Err != nil {
				return executions, execution.StopReason, execution.Err
			}
		}
	}
	sort.SliceStable(executions, func(i, j int) bool { return executions[i].Index < executions[j].Index })
	return executions, "", nil
}

func (o *Orchestrator) prepareToolExecution(ctx context.Context, mode RunMode, indexed indexedToolCall, out chan<- events.Event) ToolExecution {
	return o.prepareToolExecutionWithRegistry(ctx, mode, o.registry, indexed, out)
}

func (o *Orchestrator) prepareToolExecutionWithRegistry(ctx context.Context, mode RunMode, registry *tool.Registry, indexed indexedToolCall, out chan<- events.Event) ToolExecution {
	call := indexed.Call
	if !emitEvent(ctx, out, events.Event{Type: events.ToolPending, Tool: o.safeToolDisplay(call, events.ToolDisplayPending, "")}) {
		return ToolExecution{Call: call, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
	}
	if result, blocked := o.filterToolCallWithRegistry(mode, registry, call); blocked {
		if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
			return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		}
		return ToolExecution{Call: call, Result: result, Index: indexed.Index}
	}
	if o.executor == nil {
		result := tool.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  tool.StatusError,
			Summary: "工具执行器不可用",
			Error:   &tool.Error{Code: tool.ErrToolNotFound, Message: "工具执行器不可用", Recoverable: true},
		}
		if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
			return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		}
		return ToolExecution{Call: call, Result: result, Index: indexed.Index}
	}
	if o.authorizer == nil {
		result := tool.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  tool.StatusError,
			Summary: "权限系统不可用",
			Error:   &tool.Error{Code: tool.ErrPermissionDenied, Message: "权限系统不可用", Recoverable: true},
		}
		if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
			return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		}
		return ToolExecution{Call: call, Result: result, Index: indexed.Index}
	}
	permissionContext := o.permissionContextWithReadScope(ctx, mode)
	decision := o.authorizer.Decide(permissionCall(call), permissionContext)
	switch decision.Kind {
	case permission.DecisionDeny:
		result := permissionDeniedResult(call, decision)
		if !emitEvent(ctx, out, events.Event{Type: events.ToolDenied, Tool: o.safeResultDisplay(result)}) {
			return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		}
		return ToolExecution{Call: call, Result: result, Index: indexed.Index}
	case permission.DecisionAsk:
		decisionCh := make(chan events.ToolConfirmationDecision, 1)
		confirmation := o.safeConfirmationRequest(call, decision, decisionCh)
		if !emitEvent(ctx, out, events.Event{Type: events.ToolWaitingConfirmation, Tool: o.safeToolDisplay(call, events.ToolDisplayWaitingConfirmation, "等待确认"), Confirmation: confirmation}) {
			return ToolExecution{Call: call, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		}
		select {
		case <-ctx.Done():
			result := tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "工具执行已取消", Error: &tool.Error{Code: tool.ErrTimeout, Message: ctx.Err().Error(), Recoverable: true}}
			emitEvent(ctx, out, o.safeToolResultEvent(result))
			return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		case userDecision := <-decisionCh:
			decision = o.authorizer.ResolveUserDecision(permissionCall(call), permissionContext, permissionAction(userDecision))
			if decision.Kind == permission.DecisionDeny {
				result := permissionDeniedResult(call, decision)
				if !emitEvent(ctx, out, events.Event{Type: events.ToolDenied, Tool: o.safeResultDisplay(result)}) {
					return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
				}
				return ToolExecution{Call: call, Result: result, Index: indexed.Index}
			}
		}
	}
	return ToolExecution{Call: call, Index: indexed.Index, Grant: decision.Grant}
}

func (o *Orchestrator) executePreparedTool(ctx context.Context, execution ToolExecution, out chan<- events.Event) ToolExecution {
	call := execution.Call
	if !emitEvent(ctx, out, events.Event{Type: events.ToolRunning, Tool: o.safeToolDisplay(call, events.ToolDisplayRunning, "执行中")}) {
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
		return execution
	}
	result := o.executor.ExecuteAuthorized(ctx, call, *execution.Grant)
	if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
		execution.Result = result
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
		return execution
	}
	execution.Result = result
	return execution
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

func permissionDeniedResult(call tool.Call, decision permission.Decision) tool.Result {
	message := permission.DeniedModelMessage(decision)
	summary := "Permission denied before executing " + call.Name
	if decision.UserMessage != "" {
		summary = decision.UserMessage
	}
	return tool.Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  tool.StatusDenied,
		Summary: summary,
		Content: message,
		Data:    permission.DeniedResultData(decision),
		Error:   &tool.Error{Code: tool.ErrPermissionDenied, Message: message, Recoverable: true},
	}
}

func isReadOnlyTool(name string) bool {
	return name == "Read" || name == "Glob" || name == "Grep"
}
