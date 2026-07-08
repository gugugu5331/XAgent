package orchestrator

import (
	"context"
	"strings"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/memory"
	"xagent/internal/tool"
)

func (o *Orchestrator) runAgentLoop(ctx context.Context, conv *conversation.Conversation, req RunRequest, out chan<- events.Event, start time.Time) bool {
	options := o.runOptions
	if options.MaxIterations <= 0 || options.MaxUnknownToolCalls <= 0 {
		options = defaultRunOptions()
	}
	unknownToolCalls := 0
	for iteration := 1; iteration <= options.MaxIterations; iteration++ {
		out <- progressEvent(iteration, options.MaxIterations, "", "")
		stream, err := o.stream(ctx, conv, req.Mode, true, iteration)
		if err != nil {
			o.stopWithError(ctx, conv, out, iteration, options.MaxIterations, StopReasonProviderError, err.Error(), err)
			return true
		}
		collector, reason, err := collectProviderStream(ctx, stream, out)
		if o.contextManager != nil {
			o.contextManager.UpdateUsage(conv, collector.Usage)
		}
		if o.thinking.Show {
			conversation.AppendThinkingMessage(conv, collector.ThinkingText.String())
		}
		if collector.AssistantText.Len() > 0 {
			conversation.AppendAssistantMessage(conv, collector.AssistantText.String())
		}
		if err != nil {
			o.stopWithError(ctx, conv, out, iteration, options.MaxIterations, reason, err.Error(), err)
			return true
		}
		if len(collector.ToolCalls) == 0 {
			if err := o.store.Save(ctx, conv); err != nil {
				out <- events.Event{Type: events.Error, Err: err}
				return true
			}
			o.updateMemoryAfterCompleted(req, collector.AssistantText.String())
			out <- progressEvent(iteration, options.MaxIterations, string(StopReasonCompleted), "已完成")
			out <- events.Event{Type: events.Done, Duration: time.Since(start)}
			return true
		}
		batches := makeToolBatches(collector.ToolCalls, o.registry)
		executions, stopReason, err := o.executeToolBatches(ctx, req.Mode, batches, out)
		for _, execution := range executions {
			o.appendToolMessages(conv, execution.Call, execution.Result)
		}
		if err != nil {
			o.stopWithError(ctx, conv, out, iteration, options.MaxIterations, stopReason, err.Error(), err)
			return true
		}
		unknownToolCalls += countUnknownToolResults(executions)
		if unknownToolCalls >= options.MaxUnknownToolCalls {
			if o.saveAfterStop(ctx, conv, out, iteration, options.MaxIterations, StopReasonUnknownTool, "未知工具过多") != nil {
				return true
			}
			out <- events.Event{Type: events.Done, Duration: time.Since(start)}
			return true
		}
	}
	if o.saveAfterStop(ctx, conv, out, options.MaxIterations, options.MaxIterations, StopReasonMaxIterations, "达到迭代上限") != nil {
		return true
	}
	out <- events.Event{Type: events.Done, Duration: time.Since(start)}
	return true
}

func (o *Orchestrator) stopWithError(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, iteration int, max int, reason StopReason, message string, err error) {
	if saveErr := o.saveAfterStop(ctx, conv, out, iteration, max, reason, message); saveErr != nil {
		return
	}
	out <- events.Event{Type: events.Error, Err: err}
}

func (o *Orchestrator) saveAfterStop(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, iteration int, max int, reason StopReason, message string) error {
	if err := o.store.Save(ctx, conv); err != nil {
		out <- events.Event{Type: events.Error, Err: err}
		return err
	}
	out <- progressEvent(iteration, max, string(reason), message)
	return nil
}

func (o *Orchestrator) updateMemoryAfterCompleted(req RunRequest, assistantText string) {
	if o.memory == nil {
		return
	}
	candidate := strings.TrimSpace("用户请求:\n" + req.UserText + "\n\n最终回复:\n" + assistantText)
	if candidate == "" {
		return
	}
	o.memory.UpdateAsync(memory.UpdateInput{Scope: memory.ScopeProject, Candidate: candidate, Source: "agent_loop_completed", Now: time.Now()})
}

func progressEvent(iteration int, max int, reason string, message string) events.Event {
	return events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{Iteration: iteration, Max: max, StopReason: reason, Message: message}}
}

func countUnknownToolResults(executions []ToolExecution) int {
	count := 0
	for _, execution := range executions {
		if execution.Result.Error != nil && execution.Result.Error.Code == tool.ErrToolNotFound {
			count++
		}
	}
	return count
}
