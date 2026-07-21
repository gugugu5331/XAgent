package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/skill"
)

type IndependentRequest struct {
	Invocation skill.PreparedInvocation
	Main       *conversation.Conversation
	Profile    skill.ExecutionProfile
	History    []conversation.Message
	UserText   string
	Mode       RunMode
}

var independentSequence atomic.Uint64

func (o *Orchestrator) RunIndependent(ctx context.Context, request IndependentRequest, out chan<- events.Event) (RunResult, error) {
	if request.Main == nil {
		return RunResult{}, fmt.Errorf("主会话不能为空")
	}
	if request.Invocation.Mode != skill.ModeIsolated {
		return RunResult{}, fmt.Errorf("Skill %s 不是独立模式", request.Invocation.Definition.Name)
	}
	depth := request.Profile.IndependentDepth + 1
	if depth > 1 {
		return RunResult{}, fmt.Errorf("独立 Skill 内不能再次启动独立 Skill")
	}
	activity := request.Invocation.Activity
	if activity == nil {
		activity = skill.NewActivity()
		if _, err := activity.Activate(request.Invocation.Definition, request.Invocation.Activated.Args); err != nil {
			return RunResult{}, err
		}
	}
	defer activity.Clear()
	profile, err := o.buildExecutionProfileWithSnapshot(request.Mode, skill.Snapshot{Catalog: append([]skill.CatalogItem(nil), request.Profile.Catalog...)}, activity, depth)
	if err != nil {
		return RunResult{}, err
	}
	profile.Persist = false
	profile.UpdateMemory = false

	history := cloneMessages(request.History)
	if history == nil {
		history = skill.RecentCompleteTurns(request.Main, request.Invocation.History)
	}
	identifier := fmt.Sprintf("independent-%d", independentSequence.Add(1))
	temporary := conversation.NewConversation(identifier, time.Now())
	temporary.Messages = history
	userText := o.redactText(strings.TrimSpace(request.UserText))
	if userText == "" {
		userText = o.skillInvocationText(request.Invocation)
	}
	conversation.AppendUserMessage(temporary, userText)

	state := &executionState{activity: activity, profile: profile}
	runRequest := RunRequest{UserText: userText, Mode: request.Mode, Profile: profile, Activity: activity}
	childEvents := make(chan events.Event)
	resultCh := make(chan RunResult, 1)
	go func() {
		resultCh <- o.runAgentLoop(ctx, temporary, runRequest, state, childEvents, time.Now())
		close(childEvents)
	}()

	var terminalErr error
	forwardEvents := true
	for event := range childEvents {
		switch event.Type {
		case events.Done:
			continue
		case events.Error:
			if event.Err != nil {
				terminalErr = event.Err
			}
			continue
		case events.UserSubmitted:
			continue
		}
		if !forwardEvents {
			continue
		}
		event.Text = o.redactText(event.Text)
		event.Transient = true
		event.IndependentID = identifier
		if !emitEvent(ctx, out, event) {
			forwardEvents = false
			if terminalErr == nil {
				terminalErr = ctx.Err()
			}
		}
	}
	result := <-resultCh
	if terminalErr != nil {
		return result, terminalErr
	}
	if result.Reason != StopReasonCompleted {
		return result, fmt.Errorf("独立 Skill 未正常完成: %s", result.Reason)
	}
	if strings.TrimSpace(result.FinalText) == "" {
		return result, fmt.Errorf("独立 Skill 未返回最终摘要")
	}
	result.FinalText = o.redactText(result.FinalText)
	return result, nil
}

func (o *Orchestrator) SendSkill(
	ctx context.Context,
	main *conversation.Conversation,
	invocation skill.Invocation,
	activity *skill.Activity,
	mode RunMode,
) (<-chan events.Event, skill.PreparedInvocation, error) {
	if main == nil {
		return nil, skill.PreparedInvocation{}, fmt.Errorf("会话不能为空")
	}
	if activity == nil {
		return nil, skill.PreparedInvocation{}, fmt.Errorf("Skill Activity 不可用")
	}
	prepared, err := o.PrepareSkill(invocation, activity)
	if err != nil {
		return nil, skill.PreparedInvocation{}, err
	}
	raw := strings.TrimSpace(invocation.Raw)
	if raw == "" {
		raw = o.skillInvocationText(prepared)
	}
	raw = o.redactText(raw)
	if prepared.Mode == skill.ModeShared {
		profile, err := o.buildExecutionProfile(mode, activity, 0)
		if err != nil {
			return nil, skill.PreparedInvocation{}, err
		}
		eventStream, err := o.SendRequest(ctx, main, RunRequest{UserText: raw, Mode: mode, Profile: profile, Activity: activity})
		return eventStream, prepared, err
	}

	history := skill.RecentCompleteTurns(main, prepared.History)
	parentProfile, err := o.buildExecutionProfile(mode, activity, 0)
	if err != nil {
		if prepared.Activity != nil {
			prepared.Activity.Clear()
		}
		return nil, skill.PreparedInvocation{}, err
	}
	out := make(chan events.Event)
	conversation.AppendUserMessage(main, raw)
	go func() {
		defer close(out)
		start := time.Now()
		if !emitEvent(requestCtxOrBackground(ctx), out, events.Event{Type: events.UserSubmitted, Text: raw}) {
			if prepared.Activity != nil {
				prepared.Activity.Clear()
			}
			return
		}
		result, runErr := o.RunIndependent(ctx, IndependentRequest{
			Invocation: prepared,
			Main:       main,
			Profile:    parentProfile,
			History:    history,
			UserText:   raw,
			Mode:       mode,
		}, out)
		if runErr != nil {
			_ = o.saveConversationDirect(ctx, main)
			emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.redactError(runErr)})
			return
		}
		if err := o.appendAssistantTransaction(main, result.FinalText, func() error { return o.saveConversationDirect(ctx, main) }); err != nil {
			emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.redactError(err)})
			return
		}
		o.updateMemoryAfterCompleted(RunRequest{UserText: raw, Mode: mode}, result.FinalText)
		if !emitEvent(ctx, out, events.Event{Type: events.TextDelta, Text: result.FinalText}) {
			return
		}
		if !emitEvent(ctx, out, progressEvent(1, 1, string(StopReasonCompleted), "已完成")) {
			return
		}
		emitEvent(ctx, out, events.Event{Type: events.Done, Duration: time.Since(start)})
	}()
	return out, prepared, nil
}

func (o *Orchestrator) runAgentTriggeredIndependent(
	ctx context.Context,
	main *conversation.Conversation,
	req RunRequest,
	state *executionState,
	prepared skill.PreparedInvocation,
	out chan<- events.Event,
) (RunResult, error) {
	history := skill.RecentCompleteTurns(main, prepared.History)
	result, err := o.RunIndependent(ctx, IndependentRequest{
		Invocation: prepared,
		Main:       main,
		Profile:    state.profile,
		History:    history,
		UserText:   o.skillInvocationText(prepared),
		Mode:       req.Mode,
	}, out)
	if err != nil {
		return result, err
	}
	if err := o.appendAssistantTransaction(main, result.FinalText, func() error { return o.saveConversationIfNeeded(ctx, main, state) }); err != nil {
		return result, err
	}
	if state.profile.UpdateMemory {
		o.updateMemoryAfterCompleted(req, result.FinalText)
	}
	if !emitEvent(ctx, out, events.Event{Type: events.TextDelta, Text: result.FinalText}) {
		return result, ctx.Err()
	}
	if !emitEvent(ctx, out, progressEvent(1, 1, string(StopReasonCompleted), "已完成")) {
		return result, ctx.Err()
	}
	if !emitEvent(ctx, out, events.Event{Type: events.Done, Duration: result.Duration}) {
		return result, ctx.Err()
	}
	result.Reason = StopReasonCompleted
	return result, nil
}

func (o *Orchestrator) saveConversationDirect(ctx context.Context, conv *conversation.Conversation) error {
	if o.store == nil {
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

func (o *Orchestrator) skillInvocationText(prepared skill.PreparedInvocation) string {
	text := "/" + prepared.Definition.Name
	if strings.TrimSpace(prepared.Activated.Args) != "" {
		text += " " + prepared.Activated.Args
	}
	return o.redactText(text)
}

func (o *Orchestrator) appendAssistantTransaction(conv *conversation.Conversation, text string, save func() error) error {
	checkpoint := checkpointConversation(conv)
	conversation.AppendAssistantMessage(conv, o.redactText(text))
	if save == nil {
		return nil
	}
	if err := save(); err != nil {
		restoreConversation(conv, checkpoint)
		return err
	}
	return nil
}

type conversationCheckpoint struct {
	messageCount int
	updatedAt    time.Time
}

func checkpointConversation(conv *conversation.Conversation) conversationCheckpoint {
	if conv == nil {
		return conversationCheckpoint{}
	}
	return conversationCheckpoint{messageCount: len(conv.Messages), updatedAt: conv.UpdatedAt}
}

func restoreConversation(conv *conversation.Conversation, checkpoint conversationCheckpoint) {
	if conv == nil {
		return
	}
	if checkpoint.messageCount >= 0 && checkpoint.messageCount <= len(conv.Messages) {
		conv.Messages = conv.Messages[:checkpoint.messageCount]
	}
	conv.UpdatedAt = checkpoint.updatedAt
}

func requestCtxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func cloneMessages(messages []conversation.Message) []conversation.Message {
	if messages == nil {
		return nil
	}
	result := make([]conversation.Message, len(messages))
	for index, message := range messages {
		result[index] = message
		result[index].ToolResultData = append([]byte(nil), message.ToolResultData...)
		result[index].ToolResultError = append([]byte(nil), message.ToolResultError...)
	}
	return result
}
