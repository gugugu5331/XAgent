package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/tool"
)

type ToolBatch struct {
	Calls      []indexedToolCall
	Concurrent bool
}

type ToolExecution struct {
	Call        tool.Call
	State       tool.ExecutionState
	Validated   tool.ValidatedCall
	Normalized  permission.NormalizedCall
	HookInput   hook.ToolInput
	Ref         hook.ExecutionRef
	SystemRoute bool
	Result      tool.Result
	HasResult   bool
	Projection  contextmgr.ToolResultProjection
	Projected   bool
	Duration    time.Duration
	Index       int
	Ticket      permission.ExecutionTicket
	Scope       permission.GrantScope
	Source      permission.Source
	StopReason  StopReason
	Err         error
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
		return o.unavailableToolRegistryResult(call), true
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
		return o.unknownToolResult(call), true
	}
	if mode == RunModePlan && !isReadOnlyTool(call.Name) {
		return tool.Result{}, false
	}
	_ = registeredTool
	return tool.Result{}, false
}

func (o *Orchestrator) executeToolBatches(ctx context.Context, mode RunMode, batches []ToolBatch, out chan<- events.Event) ([]ToolExecution, StopReason, error) {
	return o.executeToolBatchesWithRegistryAndRef(ctx, mode, o.registry, batches, hook.ExecutionRef{}, out)
}

func (o *Orchestrator) executeToolBatchesWithRegistry(ctx context.Context, mode RunMode, registry *tool.Registry, batches []ToolBatch, out chan<- events.Event) ([]ToolExecution, StopReason, error) {
	return o.executeToolBatchesWithRegistryAndRef(ctx, mode, registry, batches, hook.ExecutionRef{}, out)
}

func (o *Orchestrator) executeToolBatchesWithRegistryAndRef(ctx context.Context, mode RunMode, registry *tool.Registry, batches []ToolBatch, ref hook.ExecutionRef, out chan<- events.Event) ([]ToolExecution, StopReason, error) {
	executions := make([]ToolExecution, 0)
	for _, batch := range batches {
		select {
		case <-ctx.Done():
			return executions, StopReasonCancelled, ctx.Err()
		default:
		}
		prepared := make([]ToolExecution, 0, len(batch.Calls))
		for _, indexed := range batch.Calls {
			execution := o.prepareToolExecutionWithRegistryAndRef(ctx, mode, registry, indexed, ref, out)
			prepared = append(prepared, execution)
			if execution.Err != nil {
				executions = append(executions, prepared...)
				return executions, execution.StopReason, execution.Err
			}
		}
		if batch.Concurrent {
			prepared = o.executeConcurrentPreparedTools(ctx, prepared, out)
			for _, execution := range prepared {
				executions = append(executions, execution)
				if execution.Err != nil {
					return executions, execution.StopReason, execution.Err
				}
			}
			continue
		}
		for _, execution := range prepared {
			if execution.HasResult {
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

func (o *Orchestrator) executePreparedTool(ctx context.Context, execution ToolExecution, out chan<- events.Event) ToolExecution {
	call := execution.Call
	if !execution.SystemRoute && !emitEvent(ctx, out, events.Event{Type: events.ToolRunning, Tool: o.safeToolDisplay(call, events.ToolDisplayRunning, "执行中")}) {
		return cancelToolExecutionBeforeStart(execution, ctx.Err())
	}
	execution.State = tool.Running
	started := time.Now()
	var result tool.Result
	if execution.SystemRoute {
		result = o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Completed, Status: tool.StatusError, Summary: "load_skill 必须由 Orchestrator 系统路由处理", Error: &tool.Error{Code: tool.ErrInternalRoutingRequired, Message: "load_skill 必须由 Orchestrator 系统路由处理", Recoverable: true}})
	} else if o.executor == nil || !execution.Ticket.Issued() {
		result = o.unavailableToolExecutorResult(call)
	} else {
		result = o.executor.ExecuteValidatedAuthorized(ctx, execution.Validated, execution.Ticket)
	}
	if result.CallID == "" && ctx.Err() != nil {
		return cancelToolExecutionBeforeStart(execution, ctx.Err())
	}
	execution = recordToolExecutionResult(execution, result, lifecycleDuration(started))
	if !execution.HasResult {
		return execution
	}
	if !o.candidateResultsEnabled() && !o.publishLegacyToolResult(ctx, execution, out) {
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
	}
	return execution
}

func (o *Orchestrator) completeSystemToolExecution(ctx context.Context, execution ToolExecution, result tool.Result, started time.Time, out chan<- events.Event) ToolExecution {
	execution = recordToolExecutionResult(execution, result, lifecycleDuration(started))
	if !execution.HasResult {
		return execution
	}
	if !o.candidateResultsEnabled() && !o.publishLegacyToolResult(ctx, execution, out) {
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
	}
	return execution
}

func (o *Orchestrator) rejectPreparedTool(ctx context.Context, call tool.Call, index int, result tool.Result, out chan<- events.Event) ToolExecution {
	execution := ToolExecution{Call: call, State: tool.Prepared, Result: result, HasResult: result.CallID != "", Index: index}
	if execution.HasResult {
		execution.State = tool.Rejected
	} else {
		return execution
	}
	if !o.candidateResultsEnabled() && !o.publishLegacyToolResult(ctx, execution, out) {
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
	}
	return execution
}

func (o *Orchestrator) hookDeniedResult(call tool.Call, reason string) tool.Result {
	reason = strings.TrimSpace(o.redactText(reason))
	if reason == "" {
		reason = "This tool call was denied by an automation hook. Choose a safer alternative."
	}
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusDenied, Summary: "Tool call denied by automation hook", Preview: reason, Error: &tool.Error{Code: tool.ErrHookDenied, Message: reason, Recoverable: true}})
}

func (o *Orchestrator) publishLegacyToolResult(ctx context.Context, execution ToolExecution, out chan<- events.Event) bool {
	result := execution.Result
	content := o.safeText(resultContent(result))
	var resultError *hook.SafeError
	if result.Error != nil {
		resultError = &hook.SafeError{Code: result.Error.Code, Message: o.safeText(result.Error.Message), Recoverable: result.Error.Recoverable}
	}
	if execution.HookInput.CallID != "" {
		o.hookRuntime().AfterTool(hookLifecycleContext(ctx), execution.Ref, execution.HookInput, hook.ToolOutput{Status: hookToolStatus(string(result.Status)), Content: content, Error: resultError}, execution.Duration)
	}
	return emitEvent(ctx, out, o.safeToolResultEvent(result))
}

// dispatchToolAfter remains a Hook-only migration adapter for direct callers.
// It neither projects nor writes Conversation or executionState.
func (o *Orchestrator) dispatchToolAfter(ctx context.Context, execution ToolExecution, result tool.Result, duration time.Duration) {
	content := o.safeText(resultContent(result))
	var resultError *hook.SafeError
	if result.Error != nil {
		resultError = &hook.SafeError{Code: result.Error.Code, Message: o.safeText(result.Error.Message), Recoverable: result.Error.Recoverable}
	}
	input := execution.HookInput
	if input.CallID == "" {
		input = hook.NewToolInput(execution.Call.ID, execution.Call.Name, nil)
	}
	o.hookRuntime().AfterTool(hookLifecycleContext(ctx), execution.Ref, input, hook.ToolOutput{Status: hookToolStatus(string(result.Status)), Content: content, Error: resultError}, duration)
}

func (o *Orchestrator) unavailableToolRegistryResult(call tool.Call) tool.Result {
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusError, Summary: "工具注册中心不可用", Error: &tool.Error{Code: tool.ErrToolNotFound, Message: "工具注册中心不可用", Recoverable: true}})
}

func (o *Orchestrator) unknownToolResult(call tool.Call) tool.Result {
	message := fmt.Sprintf("工具 %q 未注册或在当前 Skill 中不可见", call.Name)
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusError, Summary: fmt.Sprintf("未知工具: %s", call.Name), Preview: message, Error: &tool.Error{Code: tool.ErrToolNotFound, Message: message, Recoverable: true}})
}

func (o *Orchestrator) invalidToolArgumentsResult(call tool.Call, err error) tool.Result {
	message := "工具参数必须是单个 JSON object"
	if err != nil {
		message = err.Error()
	}
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusError, Summary: "工具参数不是有效 JSON object", Preview: message, Error: &tool.Error{Code: tool.ErrInvalidArguments, Message: message, Recoverable: true}})
}

func (o *Orchestrator) unavailableToolExecutorResult(call tool.Call) tool.Result {
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusError, Summary: "工具执行器不可用", Error: &tool.Error{Code: tool.ErrToolNotFound, Message: "工具执行器不可用", Recoverable: true}})
}

func (o *Orchestrator) unavailablePermissionResult(call tool.Call) tool.Result {
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusError, Summary: "权限系统不可用", Error: &tool.Error{Code: tool.ErrPermissionDenied, Message: "权限系统不可用", Recoverable: true}})
}

func (o *Orchestrator) unavailablePermissionTicketResult(call tool.Call) tool.Result {
	message := "Permission authorization did not produce a valid execution ticket."
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusDenied, Summary: "权限授权结果无效", Preview: message, Error: &tool.Error{Code: tool.ErrPermissionDenied, Message: message, Recoverable: true}})
}

func (o *Orchestrator) permissionDeniedResult(call tool.Call, decision permission.Decision) tool.Result {
	message := permission.DeniedModelMessage(decision)
	summary := "Permission denied before executing " + call.Name
	if decision.UserMessage != "" {
		summary = decision.UserMessage
	}
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: tool.StatusDenied, Summary: summary, Preview: message, Error: &tool.Error{Code: tool.ErrPermissionDenied, Message: message, Recoverable: true}})
}

func (o *Orchestrator) syntheticToolResult(input tool.ResultFactoryInput) tool.Result {
	if o != nil && o.resultFactory != nil {
		result, err := o.resultFactory.Build(input)
		if err == nil {
			return result
		}
		// Candidate construction failures remain invalid and must fail closed at
		// the single projection boundary; never fall back to a legacy Result.
		return tool.Result{}
	}
	return tool.Result{CallID: input.CallID, Name: input.Name, Status: input.Status, Summary: input.Summary, Content: input.Preview, Error: input.Error, Truncated: input.Truncated}
}

func isReadOnlyTool(name string) bool {
	return name == "Read" || name == "Glob" || name == "Grep"
}
