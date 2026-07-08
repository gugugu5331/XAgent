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
	if o.registry == nil {
		return tool.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  tool.StatusError,
			Summary: "工具注册中心不可用",
			Error:   &tool.Error{Code: tool.ErrToolNotFound, Message: "工具注册中心不可用", Recoverable: true},
		}, true
	}
	registeredTool, ok := o.registry.Get(call.Name)
	if !ok {
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
	executions := make([]ToolExecution, 0)
	for _, batch := range batches {
		select {
		case <-ctx.Done():
			return executions, StopReasonCancelled, ctx.Err()
		default:
		}
		prepared := make([]ToolExecution, 0, len(batch.Calls))
		for _, indexed := range batch.Calls {
			execution := o.prepareToolExecution(ctx, mode, indexed, out)
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
	call := indexed.Call
	out <- events.Event{Type: events.ToolPending, Tool: newToolDisplay(call, events.ToolDisplayPending, "")}
	if result, blocked := o.filterToolCall(mode, call); blocked {
		out <- toolResultEvent(result)
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
		out <- toolResultEvent(result)
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
		out <- toolResultEvent(result)
		return ToolExecution{Call: call, Result: result, Index: indexed.Index}
	}
	decision := o.authorizer.Decide(permissionCall(call), o.permissionContext(mode))
	switch decision.Kind {
	case permission.DecisionDeny:
		result := permissionDeniedResult(call, decision)
		out <- events.Event{Type: events.ToolDenied, Tool: resultDisplay(result)}
		return ToolExecution{Call: call, Result: result, Index: indexed.Index}
	case permission.DecisionAsk:
		decisionCh := make(chan events.ToolConfirmationDecision, 1)
		confirmation := confirmationRequest(call, decision, decisionCh)
		out <- events.Event{Type: events.ToolWaitingConfirmation, Tool: newToolDisplay(call, events.ToolDisplayWaitingConfirmation, "等待确认"), Confirmation: confirmation}
		select {
		case <-ctx.Done():
			result := tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "工具执行已取消", Error: &tool.Error{Code: tool.ErrTimeout, Message: ctx.Err().Error(), Recoverable: true}}
			out <- toolResultEvent(result)
			return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		case userDecision := <-decisionCh:
			decision = o.authorizer.ResolveUserDecision(permissionCall(call), o.permissionContext(mode), permissionAction(userDecision))
			if decision.Kind == permission.DecisionDeny {
				result := permissionDeniedResult(call, decision)
				out <- events.Event{Type: events.ToolDenied, Tool: resultDisplay(result)}
				return ToolExecution{Call: call, Result: result, Index: indexed.Index}
			}
		}
	}
	return ToolExecution{Call: call, Index: indexed.Index, Grant: decision.Grant}
}

func (o *Orchestrator) executePreparedTool(ctx context.Context, execution ToolExecution, out chan<- events.Event) ToolExecution {
	call := execution.Call
	out <- events.Event{Type: events.ToolRunning, Tool: newToolDisplay(call, events.ToolDisplayRunning, "执行中")}
	result := o.executor.ExecuteAuthorized(ctx, call, *execution.Grant)
	out <- toolResultEvent(result)
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
