package orchestrator

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
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
		if !emitEvent(ctx, out, o.progressEvent(iteration, options.MaxIterations, "", "")) {
			return finishRunResult(result, start, StopReasonCancelled, ctx.Err())
		}
		stream, startErr := o.streamWithExecutionState(ctx, conv, req.Mode, state.profile, true, iteration, state.ref, state)
		collector, reason, err := ownProviderStreamWithRedactor(ctx, stream, startErr, out, o.redactText, o.redactionLookbehind)
		if startErr != nil {
			reason, runErr := classifyStreamStartFailure(ctx, err)
			return finishRunResult(result, start, reason, o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, reason, runErr.Error(), runErr))
		}
		parentCheckpoint := checkpointConversation(conv)
		iterationProfile := state.profile.Clone()
		if err == nil {
			if usageErr := o.commitProviderUsage(ctx, conv, collector.Usage, out); usageErr != nil {
				stopReason, runErr := classifyStreamStartFailure(ctx, usageErr)
				return finishRunResult(result, start, stopReason, o.stopRunWithError(
					ctx, conv, state, out, iteration, options.MaxIterations, stopReason, runErr.Error(), runErr,
				))
			}
			addUsage(&result.Usage, collector.Usage)
		}
		if o.thinking.Show {
			thinking := strings.TrimSpace(collector.ThinkingText.String())
			if thinking != "" {
				o.appendConversationMessage(conv, conversation.RoleThinking, o.safeText(thinking), nil)
			}
		}
		if collector.AssistantText.Len() > 0 {
			text := collector.AssistantText.String()
			runtime := o.hookRuntime()
			message := runtime.BeginMessage(hookLifecycleContext(ctx), state.ref, hook.MessageAssistant, text)
			o.appendConversationMessage(conv, conversation.RoleAssistant, o.safeText(text), nil)
			runtime.EndMessage(hookLifecycleContext(ctx), message)
		}
		if err != nil {
			return finishRunResult(result, start, reason, o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, reason, err.Error(), err))
		}
		if len(collector.ToolCalls) == 0 {
			result.FinalText = collector.AssistantText.String()
			if err := o.saveConversationIfNeeded(ctx, conv, state); err != nil {
				return finishRunResult(result, start, StopReasonProviderError, o.redactError(err))
			}
			if state.profile.UpdateMemory {
				o.updateMemoryAfterCompleted(req, collector.AssistantText.String())
			}
			if !emitEvent(ctx, out, o.progressEvent(iteration, options.MaxIterations, string(StopReasonCompleted), "已完成")) {
				return finishRunResult(result, start, StopReasonCancelled, ctx.Err())
			}
			return finishRunResult(result, start, StopReasonCompleted)
		}
		if containsSystemRouteCall(o.registry, collector.ToolCalls) {
			terminal, stopReason, unknownCount, err := o.handleSkillToolCalls(ctx, conv, req, state, parentCheckpoint, iterationProfile, iteration, collector.ToolCalls, out)
			unknownToolCalls += unknownCount
			if err != nil {
				if stopReason == "" || stopReason == StopReasonCompleted {
					stopReason = StopReasonProviderError
				}
				terminalErr := o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, stopReason, err.Error(), err)
				if terminal != nil {
					addUsage(&terminal.Usage, &result.Usage)
					return finishRunResult(*terminal, start, stopReason, terminalErr)
				}
				return finishRunResult(result, start, stopReason, terminalErr)
			}
			if terminal != nil {
				addUsage(&terminal.Usage, &result.Usage)
				return finishRunResult(*terminal, start, terminal.Reason)
			}
			if unknownToolCalls >= options.MaxUnknownToolCalls {
				if err := o.saveRunAfterStop(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonUnknownTool, "未知工具过多"); err != nil {
					return finishRunResult(result, start, StopReasonProviderError, o.redactError(err))
				}
				return finishRunResult(result, start, StopReasonUnknownTool)
			}
			continue
		}
		registry, err := o.preflightRegistryForProfile(req.Mode, iterationProfile)
		if err != nil {
			return finishRunResult(result, start, StopReasonProviderError, o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonProviderError, err.Error(), err))
		}
		execCtx, scopeErr := o.contextWithReadScope(ctx, iterationProfile)
		if scopeErr != nil {
			return finishRunResult(result, start, StopReasonProviderError, o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonProviderError, scopeErr.Error(), scopeErr))
		}
		executions, stopReason, err := o.scheduleToolCallsWithRegistryAndRef(execCtx, req.Mode, registry, collector.ToolCalls, state.ref, out)
		executions, publishErr := o.publishToolExecutions(ctx, conv, state, iteration, executions, out)
		if publishErr != nil {
			return finishRunResult(result, start, StopReasonProviderError, o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonProviderError, publishErr.Error(), publishErr))
		}
		if err != nil {
			return finishRunResult(result, start, stopReason, o.stopRunWithError(ctx, conv, state, out, iteration, options.MaxIterations, stopReason, err.Error(), err))
		}
		unknownToolCalls += countUnknownToolResults(executions)
		if unknownToolCalls >= options.MaxUnknownToolCalls {
			if err := o.saveRunAfterStop(ctx, conv, state, out, iteration, options.MaxIterations, StopReasonUnknownTool, "未知工具过多"); err != nil {
				return finishRunResult(result, start, StopReasonProviderError, o.redactError(err))
			}
			return finishRunResult(result, start, StopReasonUnknownTool)
		}
	}
	if err := o.saveRunAfterStop(ctx, conv, state, out, options.MaxIterations, options.MaxIterations, StopReasonMaxIterations, "达到迭代上限"); err != nil {
		return finishRunResult(result, start, StopReasonProviderError, o.redactError(err))
	}
	return finishRunResult(result, start, StopReasonMaxIterations)
}

func closeProviderStream(stream provider.ChatStream) error {
	if stream == nil {
		return nil
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return stream.Close(closeCtx)
}

func classifyStreamStartFailure(ctx context.Context, err error) (StopReason, error) {
	if cancelErr := requestContextError(ctx); cancelErr != nil {
		return StopReasonCancelled, cancelErr
	}
	return StopReasonProviderError, err
}

func requestContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func finishRunResult(result RunResult, start time.Time, reason StopReason, errs ...error) RunResult {
	result.Duration = time.Since(start)
	result.Reason = reason
	for _, err := range errs {
		if err != nil {
			result.Err = err
			break
		}
	}
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

// commitProviderUsage is the sole ordered commit point for the Provider's
// final usage snapshot. The same immutable values update conversation context
// and the public status event; neither consumer derives or re-reads usage.
func (o *Orchestrator) commitProviderUsage(ctx context.Context, conv *conversation.Conversation, usage *provider.Usage, out chan<- events.Event) error {
	if usage == nil {
		return nil
	}
	snapshot := *usage
	if err := validateProviderUsageSnapshot(snapshot); err != nil {
		return err
	}
	var previousContext *conversation.ContextMetadata
	if conv != nil && conv.Context != nil {
		copy := *conv.Context
		previousContext = &copy
	}
	if o.contextManager != nil {
		if err := o.contextManager.UpdateUsage(conv, snapshot); err != nil {
			return err
		}
	}
	if !emitEvent(ctx, out, events.Event{Type: events.UsageUpdated, Usage: usageDisplay(&snapshot)}) {
		if conv != nil && o.contextManager != nil {
			conv.Context = previousContext
		}
		if err := requestContextError(ctx); err != nil {
			return err
		}
		return context.Canceled
	}
	return nil
}

func validateProviderUsageSnapshot(usage provider.Usage) error {
	values := [...]int64{usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens}
	var total int64
	for _, value := range values {
		if value < 0 || value > math.MaxInt64-total {
			return errors.New("provider usage snapshot is invalid")
		}
		total += value
	}
	return nil
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
	_, err := o.store.Save(saveCtx, conv)
	return err
}

func (o *Orchestrator) stopRunWithError(ctx context.Context, conv *conversation.Conversation, state *executionState, out chan<- events.Event, iteration int, max int, reason StopReason, message string, err error) error {
	if isOrderedToolCommitError(err) {
		emitEvent(ctx, out, o.progressEvent(iteration, max, string(reason), message))
		return o.redactError(err)
	}
	if saveErr := o.saveRunAfterStop(ctx, conv, state, out, iteration, max, reason, o.redactText(message)); saveErr != nil {
		return o.redactError(saveErr)
	}
	return o.redactError(err)
}

func (o *Orchestrator) saveRunAfterStop(ctx context.Context, conv *conversation.Conversation, state *executionState, out chan<- events.Event, iteration int, max int, reason StopReason, message string) error {
	if err := o.saveConversationIfNeeded(ctx, conv, state); err != nil {
		return err
	}
	emitEvent(ctx, out, o.progressEvent(iteration, max, string(reason), message))
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

func (o *Orchestrator) progressEvent(iteration int, max int, reason string, message string) events.Event {
	return events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{Iteration: iteration, Max: max, StopReason: reason, Message: o.safeText(message)}}
}

func countUnknownToolResults(executions []ToolExecution) int {
	count := 0
	for _, execution := range executions {
		if execution.Projected && execution.Projection.UserView.Error != nil && execution.Projection.UserView.Error.Code == tool.ErrToolNotFound {
			count++
			continue
		}
		if !execution.Projected && execution.Result.Error != nil && execution.Result.Error.Code == tool.ErrToolNotFound {
			count++
		}
	}
	return count
}
