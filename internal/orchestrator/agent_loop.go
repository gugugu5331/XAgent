package orchestrator

import (
	"context"
	"strings"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/tool"
)

func (o *Orchestrator) runAgentLoop(ctx context.Context, conv *conversation.Conversation, req RunRequest, state *executionState, out chan<- events.Event, start time.Time) RunResult {
	result := RunResult{}
	options := o.runOptions
	if options.MaxIterations <= 0 || options.MaxUnknownToolCalls <= 0 {
		options = defaultRunOptions()
	}
	unknownToolCalls := 0
	for iteration := 1; iteration <= options.MaxIterations; iteration++ {
		if !emitEvent(ctx, out, progressEvent(iteration, options.MaxIterations, "", "")) {
			return finishRunResult(result, start, StopReasonCancelled)
		}
		stream, err := o.streamWithProfile(ctx, conv, req.Mode, state.profile, true, iteration)
		if err != nil {
			o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonProviderError, err.Error(), err)
			return finishRunResult(result, start, StopReasonProviderError)
		}
		parentCheckpoint := checkpointConversation(conv)
		iterationProfile := state.profile.Clone()
		collector, reason, err := collectProviderStreamWithRedactor(ctx, stream, out, o.redactText, o.redactionLookbehind)
		addUsage(&result.Usage, collector.Usage)
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
			o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, reason, err.Error(), err)
			return finishRunResult(result, start, reason)
		}
		if len(collector.ToolCalls) == 0 {
			result.FinalText = collector.AssistantText.String()
			if err := o.saveConversationIfNeeded(ctx, conv, state); err != nil {
				emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.redactError(err)})
				return finishRunResult(result, start, StopReasonProviderError)
			}
			if state.profile.UpdateMemory {
				o.updateMemoryAfterCompleted(req, collector.AssistantText.String())
			}
			if !emitEvent(ctx, out, progressEvent(iteration, options.MaxIterations, string(StopReasonCompleted), "已完成")) {
				return finishRunResult(result, start, StopReasonCancelled)
			}
			emitEvent(ctx, out, events.Event{Type: events.Done, Duration: time.Since(start)})
			return finishRunResult(result, start, StopReasonCompleted)
		}
		if containsLoadSkillCall(collector.ToolCalls) {
			terminal, stopReason, unknownCount, err := o.handleSkillToolCalls(ctx, conv, req, state, parentCheckpoint, iterationProfile, collector.ToolCalls, out)
			unknownToolCalls += unknownCount
			if err != nil {
				if stopReason == "" || stopReason == StopReasonCompleted {
					stopReason = StopReasonProviderError
				}
				o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, stopReason, err.Error(), err)
				if terminal != nil {
					addUsage(&terminal.Usage, &result.Usage)
					return finishRunResult(*terminal, start, stopReason)
				}
				return finishRunResult(result, start, stopReason)
			}
			if terminal != nil {
				addUsage(&terminal.Usage, &result.Usage)
				return finishRunResult(*terminal, start, terminal.Reason)
			}
			if unknownToolCalls >= options.MaxUnknownToolCalls {
				if o.saveRunAfterStop(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonUnknownTool, "未知工具过多") != nil {
					return finishRunResult(result, start, StopReasonProviderError)
				}
				emitEvent(ctx, out, events.Event{Type: events.Done, Duration: time.Since(start)})
				return finishRunResult(result, start, StopReasonUnknownTool)
			}
			continue
		}
		registry, err := o.registryForProfile(req.Mode, iterationProfile)
		if err != nil {
			o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonProviderError, err.Error(), err)
			return finishRunResult(result, start, StopReasonProviderError)
		}
		batches := makeToolBatches(collector.ToolCalls, registry)
		execCtx, scopeErr := o.contextWithReadScope(ctx, iterationProfile)
		if scopeErr != nil {
			o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonProviderError, scopeErr.Error(), scopeErr)
			return finishRunResult(result, start, StopReasonProviderError)
		}
		executions, stopReason, err := o.executeToolBatchesWithRegistry(execCtx, req.Mode, registry, batches, out)
		for _, execution := range executions {
			o.appendToolMessages(conv, execution.Call, execution.Result)
		}
		if err != nil {
			o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, stopReason, err.Error(), err)
			return finishRunResult(result, start, stopReason)
		}
		unknownToolCalls += countUnknownToolResults(executions)
		if unknownToolCalls >= options.MaxUnknownToolCalls {
			if o.saveRunAfterStop(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonUnknownTool, "未知工具过多") != nil {
				return finishRunResult(result, start, StopReasonProviderError)
			}
			emitEvent(ctx, out, events.Event{Type: events.Done, Duration: time.Since(start)})
			return finishRunResult(result, start, StopReasonUnknownTool)
		}
	}
	if o.saveRunAfterStop(ctx, conv, state, out, options.MaxIterations, options.MaxIterations, StopReasonMaxIterations, "达到迭代上限") != nil {
		return finishRunResult(result, start, StopReasonProviderError)
	}
	emitEvent(ctx, out, events.Event{Type: events.Done, Duration: time.Since(start)})
	return finishRunResult(result, start, StopReasonMaxIterations)
}

func finishRunResult(result RunResult, start time.Time, reason StopReason) RunResult {
	result.Duration = time.Since(start)
	result.Reason = reason
	return result
}

func addUsage(total *provider.Usage, usage *provider.Usage) {
	if total == nil || usage == nil {
		return
	}
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.CacheCreationInputTokens += usage.CacheCreationInputTokens
	total.CacheReadInputTokens += usage.CacheReadInputTokens
}

func (o *Orchestrator) saveConversationIfNeeded(ctx context.Context, conv *conversation.Conversation, state *executionState) error {
	if state == nil || !state.profile.Persist || o.store == nil {
		return nil
	}
	saveCtx := ctx
	if saveCtx == nil {
		saveCtx = context.Background()
	}
	if saveCtx.Err() != nil {
		var cancel context.CancelFunc
		saveCtx, cancel = context.WithTimeout(context.WithoutCancel(saveCtx), 2*time.Second)
		defer cancel()
	}
	return o.store.Save(saveCtx, conv)
}

func (o *Orchestrator) stopRunWithError(ctx context.Context, conv *conversation.Conversation, state *executionState, out chan<- events.Event, iteration int, max int, reason StopReason, message string, err error) {
	if saveErr := o.saveRunAfterStop(ctx, conv, state, out, iteration, max, reason, o.redactText(message)); saveErr != nil {
		return
	}
	emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.redactError(err)})
}

func (o *Orchestrator) saveRunAfterStop(ctx context.Context, conv *conversation.Conversation, state *executionState, out chan<- events.Event, iteration int, max int, reason StopReason, message string) error {
	if err := o.saveConversationIfNeeded(ctx, conv, state); err != nil {
		emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.redactError(err)})
		return err
	}
	emitEvent(ctx, out, progressEvent(iteration, max, string(reason), o.redactText(message)))
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
