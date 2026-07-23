package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	Validated   tool.ValidatedCall
	Normalized  permission.NormalizedCall
	HookInput   hook.ToolInput
	Ref         hook.ExecutionRef
	SystemRoute bool
	Result      tool.Result
	Index       int
	Grant       *permission.Grant
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
	return o.prepareToolExecutionWithRegistryAndRef(ctx, mode, o.registry, indexed, hook.ExecutionRef{}, out)
}

func (o *Orchestrator) prepareToolExecutionWithRegistry(ctx context.Context, mode RunMode, registry *tool.Registry, indexed indexedToolCall, out chan<- events.Event) ToolExecution {
	return o.prepareToolExecutionWithRegistryAndRef(ctx, mode, registry, indexed, hook.ExecutionRef{}, out)
}

func (o *Orchestrator) prepareToolExecutionWithRegistryAndRef(ctx context.Context, mode RunMode, registry *tool.Registry, indexed indexedToolCall, ref hook.ExecutionRef, out chan<- events.Event) ToolExecution {
	call := indexed.Call
	if call.Name != tool.LoadSkillToolName && !emitEvent(ctx, out, events.Event{Type: events.ToolPending, Tool: o.safeToolDisplay(call, events.ToolDisplayPending, "")}) {
		return ToolExecution{Call: call, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
	}
	if registry == nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, unavailableToolRegistryResult(call), out)
	}
	if _, ok := registry.Get(call.Name); !ok {
		return o.rejectPreparedTool(ctx, call, indexed.Index, unknownToolResult(call), out)
	}
	validated, err := registry.ValidateCall(call)
	if err != nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, invalidToolArgumentsResult(call, err), out)
	}
	if mode == RunModePlan && call.Name != tool.LoadSkillToolName && !isReadOnlyTool(call.Name) {
		return o.rejectPreparedTool(ctx, call, indexed.Index, permissionDeniedResult(call, planModeDecision(call)), out)
	}
	permissionContext := o.permissionContextWithReadScope(ctx, mode)
	normalized, err := permission.NormalizeArguments(permissionCall(call), validated.Arguments, permissionContext)
	if err != nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, permissionDeniedResult(call, normalizationFailureDecision(call)), out)
	}
	if o.authorizer != nil {
		if hard := o.authorizer.CheckHard(normalized, permissionContext); hard != nil {
			return o.rejectPreparedTool(ctx, call, indexed.Index, permissionDeniedResult(call, *hard), out)
		}
	}
	hookInput := hook.ToolInput{CallID: call.ID, Name: call.Name, Arguments: validated.Arguments}
	if hookDecision := o.hookRuntime().BeforeTool(requestCtxOrBackground(ctx), ref, hookInput); hookDecision.IsDeny() {
		return o.rejectPreparedTool(ctx, call, indexed.Index, o.hookDeniedResult(call, hookDecision.Reason), out)
	}
	execution := ToolExecution{
		Call:        call,
		Validated:   validated,
		Normalized:  normalized,
		HookInput:   hookInput,
		Ref:         ref,
		SystemRoute: call.Name == tool.LoadSkillToolName,
		Index:       indexed.Index,
	}
	if execution.SystemRoute {
		return execution
	}
	if o.executor == nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, unavailableToolExecutorResult(call), out)
	}
	if o.authorizer == nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, unavailablePermissionResult(call), out)
	}
	decision := o.authorizer.DecideOrdinary(normalized, permissionContext)
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
			decision = o.authorizer.ResolveNormalizedUserDecision(normalized, permissionContext, permissionAction(userDecision))
			if decision.Kind == permission.DecisionDeny {
				result := permissionDeniedResult(call, decision)
				if !emitEvent(ctx, out, events.Event{Type: events.ToolDenied, Tool: o.safeResultDisplay(result)}) {
					return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
				}
				return ToolExecution{Call: call, Result: result, Index: indexed.Index}
			}
		}
	}
	if decision.Grant == nil {
		return o.rejectPreparedTool(ctx, call, indexed.Index, unavailablePermissionGrantResult(call), out)
	}
	execution.Grant = decision.Grant
	return execution
}

func (o *Orchestrator) executePreparedTool(ctx context.Context, execution ToolExecution, out chan<- events.Event) ToolExecution {
	call := execution.Call
	if !execution.SystemRoute && !emitEvent(ctx, out, events.Event{Type: events.ToolRunning, Tool: o.safeToolDisplay(call, events.ToolDisplayRunning, "执行中")}) {
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
		return execution
	}
	started := time.Now()
	var result tool.Result
	if execution.SystemRoute {
		result = execution.Validated.Tool.Execute(ctx, tool.Input{Name: call.Name, CallID: call.ID, RawArguments: call.ArgumentsJSON, Arguments: execution.Validated.Arguments})
	} else if o.executor == nil || execution.Grant == nil {
		result = unavailableToolExecutorResult(call)
	} else {
		result = o.executor.ExecuteValidatedAuthorized(ctx, execution.Validated, *execution.Grant)
	}
	duration := lifecycleDuration(started)
	o.dispatchToolAfter(ctx, execution, result, duration)
	if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
		execution.Result = result
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
		return execution
	}
	execution.Result = result
	return execution
}

func (o *Orchestrator) completeSystemToolExecution(ctx context.Context, execution ToolExecution, result tool.Result, started time.Time, out chan<- events.Event) ToolExecution {
	o.dispatchToolAfter(ctx, execution, result, lifecycleDuration(started))
	execution.Result = result
	if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
		execution.StopReason = StopReasonCancelled
		execution.Err = ctx.Err()
	}
	return execution
}

func (o *Orchestrator) rejectPreparedTool(ctx context.Context, call tool.Call, index int, result tool.Result, out chan<- events.Event) ToolExecution {
	if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
		return ToolExecution{Call: call, Result: result, Index: index, StopReason: StopReasonCancelled, Err: ctx.Err()}
	}
	return ToolExecution{Call: call, Result: result, Index: index}
}

func (o *Orchestrator) hookDeniedResult(call tool.Call, reason string) tool.Result {
	reason = strings.TrimSpace(o.redactText(reason))
	if reason == "" {
		reason = "This tool call was denied by an automation hook. Choose a safer alternative."
	}
	return tool.Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  tool.StatusDenied,
		Summary: "Tool call denied by automation hook",
		Content: reason,
		Data:    map[string]any{"reason": tool.ErrHookDenied},
		Error:   &tool.Error{Code: tool.ErrHookDenied, Message: reason, Recoverable: true},
	}
}

func (o *Orchestrator) dispatchToolAfter(ctx context.Context, execution ToolExecution, result tool.Result, duration time.Duration) {
	content, resultError := o.boundedHookToolOutput(result)
	output := hook.ToolOutput{Status: hookToolStatus(string(result.Status)), Content: content, Error: resultError}
	o.hookRuntime().AfterTool(hookLifecycleContext(ctx), execution.Ref, execution.HookInput, output, duration)
}

func (o *Orchestrator) boundedHookToolOutput(result tool.Result) (string, *hook.ToolError) {
	// Keep ToolAfter comfortably below the Hook event envelope even when a
	// custom executor limit is very large. The final clamp also covers JSON
	// escaping and redactors that expand text.
	const hardLimit = 128 << 10
	totalLimit := 32 << 10
	if o != nil && o.executor != nil && o.executor.MaxOutputBytes > 0 {
		totalLimit = o.executor.MaxOutputBytes
	}
	if totalLimit > hardLimit {
		totalLimit = hardLimit
	}
	if totalLimit < 1<<10 {
		totalLimit = 1 << 10
	}
	errorLimit := totalLimit / 4
	contentLimit := totalLimit - errorLimit
	fieldLimit := contentLimit / 3

	bounded := result
	var truncated bool
	bounded.Summary, truncated = truncateHookText(o.redactText(result.Summary), fieldLimit)
	bounded.Truncated = bounded.Truncated || truncated
	bounded.Content, truncated = truncateHookText(o.redactText(result.Content), fieldLimit)
	bounded.Truncated = bounded.Truncated || truncated
	var outputError *hook.ToolError
	if result.Error != nil {
		cloned := *result.Error
		cloned.Code, _ = truncateHookText(o.redactText(cloned.Code), 256)
		cloned.Message, truncated = truncateHookText(o.redactText(cloned.Message), errorLimit)
		bounded.Truncated = bounded.Truncated || truncated
		bounded.Error = &cloned
		outputError = &hook.ToolError{Code: cloned.Code, Message: cloned.Message, Recoverable: cloned.Recoverable}
	}
	content, _ := truncateHookText(o.redactText(resultContent(bounded)), contentLimit)
	return content, outputError
}

func truncateHookText(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	const marker = "\n...[truncated]"
	if maxBytes <= len(marker) {
		return marker[:maxBytes], true
	}
	prefix := value[:maxBytes-len(marker)]
	for !utf8.ValidString(prefix) && len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + marker, true
}

func unavailableToolRegistryResult(call tool.Call) tool.Result {
	return tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "工具注册中心不可用", Error: &tool.Error{Code: tool.ErrToolNotFound, Message: "工具注册中心不可用", Recoverable: true}}
}

func unknownToolResult(call tool.Call) tool.Result {
	message := fmt.Sprintf("工具 %q 未注册或在当前 Skill 中不可见", call.Name)
	return tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: fmt.Sprintf("未知工具: %s", call.Name), Content: message, Error: &tool.Error{Code: tool.ErrToolNotFound, Message: message, Recoverable: true}}
}

func invalidToolArgumentsResult(call tool.Call, err error) tool.Result {
	message := "工具参数必须是单个 JSON object"
	if err != nil {
		message = err.Error()
	}
	return tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "工具参数不是有效 JSON object", Content: message, Error: &tool.Error{Code: tool.ErrInvalidArguments, Message: message, Recoverable: true}}
}

func unavailableToolExecutorResult(call tool.Call) tool.Result {
	return tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "工具执行器不可用", Error: &tool.Error{Code: tool.ErrToolNotFound, Message: "工具执行器不可用", Recoverable: true}}
}

func unavailablePermissionResult(call tool.Call) tool.Result {
	return tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "权限系统不可用", Error: &tool.Error{Code: tool.ErrPermissionDenied, Message: "权限系统不可用", Recoverable: true}}
}

func unavailablePermissionGrantResult(call tool.Call) tool.Result {
	return tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusDenied, Summary: "权限授权结果无效", Content: "Permission authorization did not produce a valid grant.", Error: &tool.Error{Code: tool.ErrPermissionDenied, Message: "Permission authorization did not produce a valid grant.", Recoverable: true}}
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
