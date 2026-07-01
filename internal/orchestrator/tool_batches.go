package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"xagent/internal/events"
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
		return tool.Result{
			CallID:  call.ID,
			Name:    call.Name,
			Status:  tool.StatusDenied,
			Summary: "Plan Mode 不允许执行非只读工具",
			Content: "Plan Mode 只允许 Read、Glob、Grep。",
			Error:   &tool.Error{Code: tool.ErrPermissionDenied, Message: "Plan Mode 不允许执行非只读工具", Recoverable: true},
		}, true
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
		if batch.Concurrent {
			results := make([]ToolExecution, len(batch.Calls))
			var wg sync.WaitGroup
			for i, indexed := range batch.Calls {
				wg.Add(1)
				go func(i int, indexed indexedToolCall) {
					defer wg.Done()
					results[i] = o.executeOneTool(ctx, mode, indexed, out)
				}(i, indexed)
			}
			wg.Wait()
			executions = append(executions, results...)
			for _, result := range results {
				if result.Err != nil {
					return executions, result.StopReason, result.Err
				}
			}
			continue
		}
		for _, indexed := range batch.Calls {
			select {
			case <-ctx.Done():
				return executions, StopReasonCancelled, ctx.Err()
			default:
			}
			execution := o.executeOneTool(ctx, mode, indexed, out)
			executions = append(executions, execution)
			if execution.Err != nil {
				return executions, execution.StopReason, execution.Err
			}
		}
	}
	sort.SliceStable(executions, func(i, j int) bool { return executions[i].Index < executions[j].Index })
	return executions, "", nil
}

func (o *Orchestrator) executeOneTool(ctx context.Context, mode RunMode, indexed indexedToolCall, out chan<- events.Event) ToolExecution {
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
	if o.executor.NeedsConfirmation(call) {
		decisionCh := make(chan events.ToolConfirmationDecision, 1)
		confirmation := &events.ToolConfirmationRequest{
			CallID:    call.ID,
			Name:      call.Name,
			Arguments: call.ArgumentsJSON,
			Prompt:    formatToolConfirmation(call) + " 需要确认，按 y 允许，按 n 拒绝。",
			Decision:  decisionCh,
		}
		out <- events.Event{Type: events.ToolWaitingConfirmation, Tool: newToolDisplay(call, events.ToolDisplayWaitingConfirmation, "等待确认"), Confirmation: confirmation}
		select {
		case <-ctx.Done():
			result := tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "工具执行已取消", Error: &tool.Error{Code: tool.ErrTimeout, Message: ctx.Err().Error(), Recoverable: true}}
			out <- toolResultEvent(result)
			return ToolExecution{Call: call, Result: result, Index: indexed.Index, StopReason: StopReasonCancelled, Err: ctx.Err()}
		case decision := <-decisionCh:
			if !decision.Allowed {
				result := o.executor.Denied(call)
				out <- events.Event{Type: events.ToolDenied, Tool: resultDisplay(result)}
				return ToolExecution{Call: call, Result: result, Index: indexed.Index}
			}
		}
	}
	out <- events.Event{Type: events.ToolRunning, Tool: newToolDisplay(call, events.ToolDisplayRunning, "执行中")}
	result := o.executor.Execute(ctx, call)
	out <- toolResultEvent(result)
	return ToolExecution{Call: call, Result: result, Index: indexed.Index}
}

func isReadOnlyTool(name string) bool {
	return name == "Read" || name == "Glob" || name == "Grep"
}
