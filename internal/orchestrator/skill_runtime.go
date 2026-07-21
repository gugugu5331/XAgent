package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type loadSkillArguments struct {
	Name string `json:"name"`
	Args string `json:"args"`
}

func parseLoadSkillCall(call tool.Call) (skill.Invocation, error) {
	if len(call.ArgumentsJSON) > skill.DefaultMaxArgsBytes*8+2048 {
		return skill.Invocation{}, fmt.Errorf("load_skill 参数超过大小限制")
	}
	decoder := json.NewDecoder(strings.NewReader(call.ArgumentsJSON))
	decoder.DisallowUnknownFields()
	var args loadSkillArguments
	if err := decoder.Decode(&args); err != nil {
		return skill.Invocation{}, fmt.Errorf("load_skill 参数无效: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return skill.Invocation{}, fmt.Errorf("load_skill 参数无效: %w", err)
	}
	if strings.TrimSpace(args.Name) == "" {
		return skill.Invocation{}, fmt.Errorf("load_skill.name 不能为空")
	}
	if len(strings.TrimSpace(args.Name)) > skill.MaxSkillNameLength {
		return skill.Invocation{}, fmt.Errorf("load_skill.name 超过大小限制")
	}
	if len(args.Args) > skill.DefaultMaxArgsBytes {
		return skill.Invocation{}, fmt.Errorf("load_skill.args 超过大小限制")
	}
	return skill.Invocation{Name: args.Name, Args: args.Args, Raw: call.ArgumentsJSON, Origin: skill.OriginAgentTool}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("包含多余 JSON 值")
}

func (o *Orchestrator) handleSkillToolCalls(
	ctx context.Context,
	conv *conversation.Conversation,
	req RunRequest,
	state *executionState,
	parentCheckpoint conversationCheckpoint,
	iterationProfile skill.ExecutionProfile,
	calls []tool.Call,
	out chan<- events.Event,
) (terminal *RunResult, stopReason StopReason, unknownTools int, fatal error) {
	registry, err := o.registryForProfile(req.Mode, iterationProfile)
	if err != nil {
		return nil, StopReasonProviderError, 0, err
	}
	execCtx, err := o.contextWithReadScope(ctx, iterationProfile)
	if err != nil {
		return nil, StopReasonProviderError, 0, err
	}
	for index, call := range calls {
		if call.Name != tool.LoadSkillToolName {
			executions, reason, err := o.executeToolBatchesWithRegistry(execCtx, req.Mode, registry, []ToolBatch{{Calls: []indexedToolCall{{Call: call, Index: index}}}}, out)
			for _, execution := range executions {
				o.appendToolMessages(conv, execution.Call, execution.Result)
			}
			unknownTools += countUnknownToolResults(executions)
			if err != nil {
				return nil, reason, unknownTools, err
			}
			continue
		}
		invocation, err := parseLoadSkillCall(call)
		if err != nil {
			result := o.loadSkillFailure(call, err)
			o.appendToolMessages(conv, call, result)
			if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
				return nil, StopReasonCancelled, unknownTools, ctx.Err()
			}
			continue
		}
		prepared, err := o.PrepareSkill(invocation, state.activity)
		if err != nil {
			result := o.loadSkillFailure(call, err)
			o.appendToolMessages(conv, call, result)
			if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
				return nil, StopReasonCancelled, unknownTools, ctx.Err()
			}
			continue
		}
		if prepared.Mode == skill.ModeIsolated {
			if len(calls) != 1 {
				if prepared.Activity != nil {
					prepared.Activity.Clear()
				}
				result := o.loadSkillFailure(call, fmt.Errorf("独立 Skill 必须作为本轮唯一工具调用"))
				o.appendToolMessages(conv, call, result)
				if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
					return nil, StopReasonCancelled, unknownTools, ctx.Err()
				}
				continue
			}
			if state.profile.IndependentDepth > 0 {
				if prepared.Activity != nil {
					prepared.Activity.Clear()
				}
				result := o.loadSkillFailure(call, fmt.Errorf("独立 Skill 内不能再次启动独立 Skill"))
				o.appendToolMessages(conv, call, result)
				if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
					return nil, StopReasonCancelled, unknownTools, ctx.Err()
				}
				continue
			}
			restoreConversation(conv, parentCheckpoint)
			if !emitEvent(ctx, out, events.Event{Type: events.MainTraceReset}) {
				if prepared.Activity != nil {
					prepared.Activity.Clear()
				}
				cancelled := finishRunResult(RunResult{}, time.Now(), StopReasonCancelled)
				return &cancelled, StopReasonCancelled, unknownTools, ctx.Err()
			}
			result, err := o.runAgentTriggeredIndependent(ctx, conv, req, state, prepared, out)
			if err != nil {
				return &result, result.Reason, unknownTools, err
			}
			return &result, result.Reason, unknownTools, nil
		}
		if err := o.refreshExecutionProfile(req.Mode, state); err != nil {
			return nil, StopReasonProviderError, unknownTools, err
		}
		result := loadSkillSuccess(call, prepared.Activated.Name)
		o.appendToolMessages(conv, call, result)
		if !emitEvent(ctx, out, o.safeToolResultEvent(result)) {
			return nil, StopReasonCancelled, unknownTools, ctx.Err()
		}
	}
	return nil, "", unknownTools, nil
}

func containsLoadSkillCall(calls []tool.Call) bool {
	for _, call := range calls {
		if call.Name == tool.LoadSkillToolName {
			return true
		}
	}
	return false
}

func loadSkillSuccess(call tool.Call, name string) tool.Result {
	return tool.Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  tool.StatusSuccess,
		Summary: fmt.Sprintf("Skill %s 已激活", name),
		Content: fmt.Sprintf("Skill %s 已激活；完整指令将在下一轮系统上下文中生效。", name),
		Data:    map[string]any{"skill": name, "activated": true},
	}
}

func (o *Orchestrator) loadSkillFailure(call tool.Call, err error) tool.Result {
	message := o.redactText(err.Error())
	return tool.Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  tool.StatusError,
		Summary: "Skill 加载失败",
		Content: message,
		Error:   &tool.Error{Code: tool.ErrInvalidArguments, Message: message, Recoverable: true},
	}
}
