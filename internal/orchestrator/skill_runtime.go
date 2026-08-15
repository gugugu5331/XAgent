package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

func (o *Orchestrator) PrepareSkill(invocation skill.Invocation, activity *skill.Activity) (skill.PreparedInvocation, error) {
	if o == nil || o.skillManager == nil {
		return skill.PreparedInvocation{}, fmt.Errorf("Skill 系统未启用")
	}
	name := strings.ToLower(strings.TrimSpace(invocation.Name))
	if name == "" || len(name) > skill.MaxSkillNameLength {
		return skill.PreparedInvocation{}, fmt.Errorf("Skill 名称无效")
	}
	if len(invocation.Args) > skill.DefaultMaxArgsBytes {
		return skill.PreparedInvocation{}, fmt.Errorf("Skill 参数超过 %d bytes", skill.DefaultMaxArgsBytes)
	}
	definition, ok := o.skillManager.Resolve(name)
	if !ok {
		return skill.PreparedInvocation{}, fmt.Errorf("未知 Skill: %s", o.redactText(name))
	}
	target := activity
	if definition.Mode == skill.ModeIsolated {
		target = skill.NewActivity()
	}
	if target == nil {
		return skill.PreparedInvocation{}, fmt.Errorf("Skill Activity 不可用")
	}
	activated, err := target.Activate(definition, invocation.Args)
	if err != nil {
		return skill.PreparedInvocation{}, fmt.Errorf("激活 Skill %s 失败: %s", definition.Name, o.redactText(err.Error()))
	}
	return skill.PreparedInvocation{
		Definition: definition,
		Activated:  activated,
		Activity:   target,
		Mode:       definition.Mode,
		History:    definition.History,
	}, nil
}

func parseLoadSkillCall(validated tool.ValidatedCall) (skill.Invocation, error) {
	call := validated.Call
	if len(call.ArgumentsJSON) > skill.DefaultMaxArgsBytes*8+2048 {
		return skill.Invocation{}, fmt.Errorf("load_skill 参数超过大小限制")
	}
	for key := range validated.Arguments {
		if key != "name" && key != "args" {
			return skill.Invocation{}, fmt.Errorf("load_skill 参数无效: 未知字段 %q", key)
		}
	}
	name, ok := validated.Arguments["name"].(string)
	if !ok {
		return skill.Invocation{}, fmt.Errorf("load_skill.name 必须是字符串")
	}
	args := ""
	if rawArgs, exists := validated.Arguments["args"]; exists {
		var argsOK bool
		args, argsOK = rawArgs.(string)
		if !argsOK {
			return skill.Invocation{}, fmt.Errorf("load_skill.args 必须是字符串")
		}
	}
	if strings.TrimSpace(name) == "" {
		return skill.Invocation{}, fmt.Errorf("load_skill.name 不能为空")
	}
	if len(strings.TrimSpace(name)) > skill.MaxSkillNameLength {
		return skill.Invocation{}, fmt.Errorf("load_skill.name 超过大小限制")
	}
	if len(args) > skill.DefaultMaxArgsBytes {
		return skill.Invocation{}, fmt.Errorf("load_skill.args 超过大小限制")
	}
	return skill.Invocation{Name: name, Args: args, Raw: call.ArgumentsJSON, Origin: skill.OriginAgentTool}, nil
}

func (o *Orchestrator) handleSkillToolCalls(
	ctx context.Context,
	conv *conversation.Conversation,
	req RunRequest,
	state *executionState,
	parentCheckpoint conversationCheckpoint,
	iterationProfile skill.ExecutionProfile,
	iteration int,
	calls []tool.Call,
	out chan<- events.Event,
) (terminal *RunResult, stopReason StopReason, unknownTools int, fatal error) {
	registry, err := o.preflightRegistryForProfile(req.Mode, iterationProfile)
	if err != nil {
		return nil, StopReasonProviderError, 0, err
	}
	execCtx, err := o.contextWithReadScope(ctx, iterationProfile)
	if err != nil {
		return nil, StopReasonProviderError, 0, err
	}
	for index, call := range calls {
		if call.Name == tool.AgentToolName {
			execution := o.prepareToolExecutionWithRegistryAndRef(execCtx, req.Mode, registry, indexedToolCall{Call: call, Index: index}, state.ref, out)
			if execution.Err != nil {
				return nil, execution.StopReason, unknownTools, execution.Err
			}
			if execution.HasResult {
				execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
				if err != nil {
					return nil, StopReasonProviderError, unknownTools, err
				}
				unknownTools += countUnknownToolResults([]ToolExecution{execution})
				continue
			}
			handlerStarted := time.Now()
			result := o.routeAgentSystemTool(ctx, conv, state, parentCheckpoint, iterationProfile, req.Mode, execution.Validated)
			execution = o.completeSystemToolExecution(ctx, execution, result, handlerStarted, out)
			execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
			if err != nil {
				return nil, StopReasonProviderError, unknownTools, err
			}
			if execution.Err != nil {
				return nil, execution.StopReason, unknownTools, execution.Err
			}
			continue
		}
		if call.Name != tool.LoadSkillToolName {
			executions, reason, err := o.executeToolBatchesWithRegistryAndRef(execCtx, req.Mode, registry, []ToolBatch{{Calls: []indexedToolCall{{Call: call, Index: index}}}}, state.ref, out)
			executions, publishErr := o.publishToolExecutions(ctx, conv, state, iteration, executions, out)
			if publishErr != nil {
				return nil, StopReasonProviderError, unknownTools, publishErr
			}
			unknownTools += countUnknownToolResults(executions)
			if err != nil {
				return nil, reason, unknownTools, err
			}
			continue
		}
		execution := o.prepareToolExecutionWithRegistryAndRef(execCtx, req.Mode, registry, indexedToolCall{Call: call, Index: index}, state.ref, out)
		if execution.Err != nil {
			return nil, execution.StopReason, unknownTools, execution.Err
		}
		if execution.HasResult {
			execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
			if err != nil {
				return nil, StopReasonProviderError, unknownTools, err
			}
			unknownTools += countUnknownToolResults([]ToolExecution{execution})
			continue
		}
		handlerStarted := time.Now()
		invocation, err := parseLoadSkillCall(execution.Validated)
		if err != nil {
			result := o.loadSkillFailure(call, err)
			execution = o.completeSystemToolExecution(ctx, execution, result, handlerStarted, out)
			execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
			if err != nil {
				return nil, StopReasonProviderError, unknownTools, err
			}
			if execution.Err != nil {
				return nil, execution.StopReason, unknownTools, execution.Err
			}
			continue
		}
		prepared, err := o.PrepareSkill(invocation, state.activity)
		if err != nil {
			result := o.loadSkillFailure(call, err)
			execution = o.completeSystemToolExecution(ctx, execution, result, handlerStarted, out)
			execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
			if err != nil {
				return nil, StopReasonProviderError, unknownTools, err
			}
			if execution.Err != nil {
				return nil, execution.StopReason, unknownTools, execution.Err
			}
			continue
		}
		if prepared.Mode == skill.ModeIsolated {
			if len(calls) != 1 {
				if prepared.Activity != nil {
					prepared.Activity.Clear()
				}
				result := o.loadSkillFailure(call, fmt.Errorf("独立 Skill 必须作为本轮唯一工具调用"))
				execution = o.completeSystemToolExecution(ctx, execution, result, handlerStarted, out)
				execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
				if err != nil {
					return nil, StopReasonProviderError, unknownTools, err
				}
				if execution.Err != nil {
					return nil, execution.StopReason, unknownTools, execution.Err
				}
				continue
			}
			if state.profile.IndependentDepth > 0 {
				if prepared.Activity != nil {
					prepared.Activity.Clear()
				}
				result := o.loadSkillFailure(call, fmt.Errorf("独立 Skill 内不能再次启动独立 Skill"))
				execution = o.completeSystemToolExecution(ctx, execution, result, handlerStarted, out)
				execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
				if err != nil {
					return nil, StopReasonProviderError, unknownTools, err
				}
				if execution.Err != nil {
					return nil, execution.StopReason, unknownTools, execution.Err
				}
				continue
			}
			restoreConversation(conv, parentCheckpoint)
			if !emitEvent(ctx, out, events.Event{Type: events.MainTraceReset}) {
				if prepared.Activity != nil {
					prepared.Activity.Clear()
				}
				cancelResult := o.loadSkillFailure(call, ctx.Err())
				o.completeSystemToolExecution(ctx, execution, cancelResult, handlerStarted, out)
				cancelled := finishRunResult(RunResult{}, time.Now(), StopReasonCancelled)
				return &cancelled, StopReasonCancelled, unknownTools, ctx.Err()
			}
			result, err := o.runAgentTriggeredIndependent(ctx, conv, req, state, prepared, out)
			if err != nil {
				toolResult := o.loadSkillFailure(call, err)
				execution = o.completeSystemToolExecution(ctx, execution, toolResult, handlerStarted, out)
				_, _ = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
				return &result, result.Reason, unknownTools, err
			}
			toolResult := o.loadSkillSuccess(call, prepared.Activated.Name)
			execution = o.completeSystemToolExecution(ctx, execution, toolResult, handlerStarted, out)
			execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
			if err != nil {
				return &result, StopReasonProviderError, unknownTools, err
			}
			if execution.Err != nil {
				return &result, StopReasonCancelled, unknownTools, execution.Err
			}
			return &result, result.Reason, unknownTools, nil
		}
		if err := o.refreshExecutionProfile(req.Mode, state); err != nil {
			result := o.loadSkillFailure(call, err)
			o.completeSystemToolExecution(ctx, execution, result, handlerStarted, out)
			return nil, StopReasonProviderError, unknownTools, err
		}
		result := o.loadSkillSuccess(call, prepared.Activated.Name)
		execution = o.completeSystemToolExecution(ctx, execution, result, handlerStarted, out)
		execution, err = o.publishToolExecution(ctx, conv, state, iteration, execution, out)
		if err != nil {
			return nil, StopReasonProviderError, unknownTools, err
		}
		if execution.Err != nil {
			return nil, execution.StopReason, unknownTools, execution.Err
		}
	}
	return nil, "", unknownTools, nil
}

func containsSystemRouteCall(registry *tool.Registry, calls []tool.Call) bool {
	if registry == nil {
		return false
	}
	for _, call := range calls {
		descriptor, ok := registry.Descriptor(call.Name)
		if ok && descriptor.Route == tool.RouteSystem {
			return true
		}
	}
	return false
}

func (o *Orchestrator) loadSkillSuccess(call tool.Call, name string) tool.Result {
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: tool.Completed, Status: tool.StatusSuccess, Summary: fmt.Sprintf("Skill %s 已激活", name), Preview: fmt.Sprintf("Skill %s 已激活；完整指令将在下一轮系统上下文中生效。", name)})
}

func (o *Orchestrator) loadSkillFailure(call tool.Call, err error) tool.Result {
	if err == nil {
		err = context.Canceled
	}
	message := o.redactText(err.Error())
	status := tool.StatusError
	code := tool.ErrInvalidArguments
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = tool.StatusTimeout
		code = tool.ErrTimeout
	}
	state := tool.Completed
	if status == tool.StatusTimeout {
		state = tool.CancelledAfterStart
	}
	return o.syntheticToolResult(tool.ResultFactoryInput{CallID: call.ID, Name: call.Name, State: state, Status: status, Summary: "Skill 加载失败", Preview: message, Error: &tool.Error{Code: code, Message: message, Recoverable: true}})
}
