package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/skill"
)

const (
	skillHistoryMaxTurns            = 1000
	skillHistoryMaxSessionBytes     = int64(1 << 30)
	skillHistoryMaxWindowTokens     = int64(10_000_000)
	skillHistoryMaxAutoMarginTokens = int64(1_000_000)
	skillHistoryPolicyValidMarker   = uint64(0x584147454e544850)
)

// SkillHistoryPolicy is an immutable, closed value. Its private fields and
// marker prevent callers outside this package from constructing or raising
// any of the fixed history limits.
type SkillHistoryPolicy struct {
	maxTurns          int
	maxSessionBytes   int64
	modelWindowTokens int64
	autoMarginTokens  int64
	marker            uint64
}

// NewSkillHistoryPolicy is the only constructor for a valid history policy.
// The turn cap is deliberately fixed here and is not caller-configurable.
func NewSkillHistoryPolicy(maxSessionBytes, modelWindowTokens, autoMarginTokens int64) (SkillHistoryPolicy, error) {
	if maxSessionBytes < 1 || maxSessionBytes > skillHistoryMaxSessionBytes {
		return SkillHistoryPolicy{}, fmt.Errorf("skill history session byte limit is invalid")
	}
	if modelWindowTokens < 2 || modelWindowTokens > skillHistoryMaxWindowTokens {
		return SkillHistoryPolicy{}, fmt.Errorf("skill history model window is invalid")
	}
	if autoMarginTokens < 1 || autoMarginTokens > skillHistoryMaxAutoMarginTokens {
		return SkillHistoryPolicy{}, fmt.Errorf("skill history automatic margin is invalid")
	}
	if autoMarginTokens >= modelWindowTokens {
		return SkillHistoryPolicy{}, fmt.Errorf("skill history automatic margin must be smaller than the model window")
	}
	return SkillHistoryPolicy{
		maxTurns:          skillHistoryMaxTurns,
		maxSessionBytes:   maxSessionBytes,
		modelWindowTokens: modelWindowTokens,
		autoMarginTokens:  autoMarginTokens,
		marker:            skillHistoryPolicyValidMarker,
	}, nil
}

func (p SkillHistoryPolicy) valid() bool {
	return p.marker == skillHistoryPolicyValidMarker &&
		p.maxTurns == skillHistoryMaxTurns &&
		p.maxSessionBytes >= 1 && p.maxSessionBytes <= skillHistoryMaxSessionBytes &&
		p.modelWindowTokens >= 2 && p.modelWindowTokens <= skillHistoryMaxWindowTokens &&
		p.autoMarginTokens >= 1 && p.autoMarginTokens <= skillHistoryMaxAutoMarginTokens &&
		p.autoMarginTokens < p.modelWindowTokens
}

func (p SkillHistoryPolicy) requireValid() error {
	if !p.valid() {
		return fmt.Errorf("skill history policy is invalid")
	}
	return nil
}

type HistoryLimitReason uint8

const (
	HistoryLimitNone HistoryLimitReason = iota
	HistoryLimitTurns
	HistoryLimitBytes
	HistoryLimitPlanningTokens
)

type SkillHistorySelection struct {
	Messages  []provider.ModelMessage
	Turns     int
	Truncated bool
	Reason    HistoryLimitReason
}

type measuredSkillHistorySelection struct {
	selection   SkillHistorySelection
	accumulated contextmgr.RequestMeasure
}

func (o *Orchestrator) selectSkillHistory(
	ctx context.Context,
	main *conversation.Conversation,
	requested int,
	base provider.ChatRequest,
) (SkillHistorySelection, error) {
	measured, err := o.selectMeasuredSkillHistory(ctx, main, requested, base)
	return measured.selection, err
}

func (o *Orchestrator) selectMeasuredSkillHistory(
	ctx context.Context,
	main *conversation.Conversation,
	requested int,
	base provider.ChatRequest,
) (measuredSkillHistorySelection, error) {
	if o == nil {
		return measuredSkillHistorySelection{}, fmt.Errorf("skill history selector is unavailable")
	}
	if err := o.skillHistoryPolicy.requireValid(); err != nil {
		return measuredSkillHistorySelection{}, err
	}
	if err := o.skillHistoryBudgeter.Validate(); err != nil {
		return measuredSkillHistorySelection{}, err
	}
	if requested < 0 || requested > o.skillHistoryPolicy.maxTurns {
		return measuredSkillHistorySelection{}, fmt.Errorf("requested skill history turns are invalid")
	}
	if ctx == nil {
		return measuredSkillHistorySelection{}, fmt.Errorf("skill history selection context is nil")
	}
	if err := ctx.Err(); err != nil {
		return measuredSkillHistorySelection{}, err
	}
	if requested == 0 {
		return measuredSkillHistorySelection{}, nil
	}
	if main == nil {
		return measuredSkillHistorySelection{}, fmt.Errorf("skill history conversation is unavailable")
	}

	baseMeasure, err := o.skillHistoryBudgeter.MeasureRequest(ctx, base)
	if err != nil {
		return measuredSkillHistorySelection{}, err
	}
	if err := ctx.Err(); err != nil {
		return measuredSkillHistorySelection{}, err
	}
	if baseMeasure.Bytes < 0 || baseMeasure.PlanningTokens < 0 {
		return measuredSkillHistorySelection{}, fmt.Errorf("skill history base measure is negative")
	}
	planningLimit := o.skillHistoryPolicy.modelWindowTokens - o.skillHistoryPolicy.autoMarginTokens
	if baseMeasure.Bytes > o.skillHistoryPolicy.maxSessionBytes {
		return measuredSkillHistorySelection{}, fmt.Errorf("skill history base exceeds the session byte limit")
	}
	if baseMeasure.PlanningTokens >= planningLimit {
		return measuredSkillHistorySelection{}, fmt.Errorf("skill history base reaches the planning token limit")
	}

	messages := main.Messages
	scanner := skill.NewCompleteTurnScanner(messages)
	selectedRanges := make([]skill.CompleteTurnRange, 0, requested)
	accumulated := baseMeasure
	truncated := false
	reason := HistoryLimitNone
	for {
		if err := ctx.Err(); err != nil {
			return measuredSkillHistorySelection{}, err
		}
		turnRange, ok, err := scanner.Next(ctx, messages)
		if err != nil {
			return measuredSkillHistorySelection{}, err
		}
		if err := ctx.Err(); err != nil {
			return measuredSkillHistorySelection{}, err
		}
		if !ok {
			break
		}
		if len(selectedRanges) >= requested {
			truncated = true
			reason = HistoryLimitTurns
			break
		}
		turnMessages, err := turnRange.Messages(messages)
		if err != nil {
			return measuredSkillHistorySelection{}, err
		}
		turnMeasure, err := o.skillHistoryBudgeter.MeasureConversationTurn(ctx, turnMessages)
		if err != nil {
			return measuredSkillHistorySelection{}, err
		}
		if err := ctx.Err(); err != nil {
			return measuredSkillHistorySelection{}, err
		}
		nextMeasure, err := addSkillHistoryMeasures(accumulated, turnMeasure)
		if err != nil {
			return measuredSkillHistorySelection{}, err
		}
		if nextMeasure.Bytes > o.skillHistoryPolicy.maxSessionBytes {
			truncated = true
			reason = HistoryLimitBytes
			break
		}
		if nextMeasure.PlanningTokens > planningLimit {
			truncated = true
			reason = HistoryLimitPlanningTokens
			break
		}
		selectedRanges = append(selectedRanges, turnRange)
		accumulated = nextMeasure
	}

	if err := ctx.Err(); err != nil {
		return measuredSkillHistorySelection{}, err
	}
	selectedMessages, err := copySelectedSkillHistory(ctx, messages, selectedRanges)
	if err != nil {
		return measuredSkillHistorySelection{}, err
	}
	if err := ctx.Err(); err != nil {
		return measuredSkillHistorySelection{}, err
	}
	return measuredSkillHistorySelection{
		selection: SkillHistorySelection{
			Messages: selectedMessages, Turns: len(selectedRanges), Truncated: truncated, Reason: reason,
		},
		accumulated: accumulated,
	}, nil
}

type independentSkillHistoryBinding struct {
	main      *conversation.Conversation
	requested int
}

type independentSkillHistoryContextKey struct{}

type independentSkillHistoryError struct {
	err error
}

func (e *independentSkillHistoryError) Error() string {
	if e == nil || e.err == nil {
		return "independent skill history preparation failed"
	}
	return e.err.Error()
}

func (e *independentSkillHistoryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func wrapIndependentSkillHistoryError(err error) error {
	if err == nil {
		return nil
	}
	return &independentSkillHistoryError{err: err}
}

func isIndependentSkillHistoryError(err error) bool {
	var target *independentSkillHistoryError
	return errors.As(err, &target)
}

func withIndependentSkillHistory(ctx context.Context, main *conversation.Conversation, requested int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, independentSkillHistoryContextKey{}, independentSkillHistoryBinding{main: main, requested: requested})
}

func hasIndependentSkillHistoryBinding(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(independentSkillHistoryContextKey{}).(independentSkillHistoryBinding)
	return ok
}

// applyIndependentSkillHistory is called only after the complete non-history
// request has been normalized. It selects safe history from the parser-owned
// snapshot, prepends it to the request, and performs the mandatory final whole
// request measurement before the Provider can observe the request.
func (o *Orchestrator) applyIndependentSkillHistory(ctx context.Context, request provider.ChatRequest) (provider.ChatRequest, error) {
	if ctx == nil {
		return request, nil
	}
	binding, ok := ctx.Value(independentSkillHistoryContextKey{}).(independentSkillHistoryBinding)
	if !ok {
		return request, nil
	}
	measured, err := o.selectMeasuredSkillHistory(ctx, binding.main, binding.requested, request)
	if err != nil {
		return provider.ChatRequest{}, wrapIndependentSkillHistoryError(err)
	}
	if err := ctx.Err(); err != nil {
		return provider.ChatRequest{}, wrapIndependentSkillHistoryError(err)
	}
	if binding.requested == 0 {
		return request, nil
	}
	combined := make([]provider.ModelMessage, 0, len(measured.selection.Messages)+len(request.Messages))
	combined = append(combined, measured.selection.Messages...)
	combined = append(combined, request.Messages...)
	request.Messages = combined

	finalMeasure, err := o.skillHistoryBudgeter.MeasureRequest(ctx, request)
	if err != nil {
		return provider.ChatRequest{}, wrapIndependentSkillHistoryError(err)
	}
	if err := ctx.Err(); err != nil {
		return provider.ChatRequest{}, wrapIndependentSkillHistoryError(err)
	}
	if finalMeasure.Bytes < 0 || finalMeasure.PlanningTokens < 0 {
		return provider.ChatRequest{}, wrapIndependentSkillHistoryError(fmt.Errorf("independent skill history final measure is negative"))
	}
	if finalMeasure.Bytes > measured.accumulated.Bytes || finalMeasure.PlanningTokens > measured.accumulated.PlanningTokens {
		return provider.ChatRequest{}, wrapIndependentSkillHistoryError(fmt.Errorf("independent skill history incremental measure underestimated the final request"))
	}
	planningLimit := o.skillHistoryPolicy.modelWindowTokens - o.skillHistoryPolicy.autoMarginTokens
	if finalMeasure.Bytes > o.skillHistoryPolicy.maxSessionBytes || finalMeasure.PlanningTokens > planningLimit {
		return provider.ChatRequest{}, wrapIndependentSkillHistoryError(fmt.Errorf("independent skill history final request exceeds its budget"))
	}
	return request, nil
}

func addSkillHistoryMeasures(left, right contextmgr.RequestMeasure) (contextmgr.RequestMeasure, error) {
	if left.Bytes < 0 || left.PlanningTokens < 0 || right.Bytes < 0 || right.PlanningTokens < 0 {
		return contextmgr.RequestMeasure{}, fmt.Errorf("skill history measure is negative")
	}
	if right.Bytes > math.MaxInt64-left.Bytes || right.PlanningTokens > math.MaxInt64-left.PlanningTokens {
		return contextmgr.RequestMeasure{}, fmt.Errorf("skill history measure addition overflow")
	}
	return contextmgr.RequestMeasure{
		Bytes: left.Bytes + right.Bytes, PlanningTokens: left.PlanningTokens + right.PlanningTokens,
	}, nil
}

func copySelectedSkillHistory(
	ctx context.Context,
	messages []conversation.Message,
	ranges []skill.CompleteTurnRange,
) ([]provider.ModelMessage, error) {
	result := make([]provider.ModelMessage, 0)
	visited := 0
	for rangeIndex := len(ranges) - 1; rangeIndex >= 0; rangeIndex-- {
		turnMessages, err := ranges[rangeIndex].Messages(messages)
		if err != nil {
			return nil, err
		}
		for _, message := range turnMessages {
			if visited%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			visited++
			if message.Role == conversation.RoleThinking {
				continue
			}
			role, ok := providerMessageRole(message.Role)
			if !ok {
				return nil, fmt.Errorf("skill history message role is invalid")
			}
			modelMessage := provider.ModelMessage{Role: role, Content: message.Content}
			if message.Tool != nil {
				modelMessage.ToolCallID = message.Tool.CallID
				modelMessage.ToolName = message.Tool.Name
				modelMessage.ArgumentsJSON = message.Tool.ArgumentsJSON
				modelMessage.ToolResult = message.Tool.Result
				modelMessage.ToolResultStatus = string(message.Tool.Status)
			}
			result = append(result, modelMessage)
		}
	}
	return result, nil
}

type IndependentRequest struct {
	Invocation skill.PreparedInvocation
	Main       *conversation.Conversation
	Profile    skill.ExecutionProfile
	UserText   string
	Mode       RunMode
}

var independentSequence atomic.Uint64

func (o *Orchestrator) RunIndependent(ctx context.Context, request IndependentRequest, out chan<- events.Event) (RunResult, error) {
	endRun := func() {}
	if o != nil && o.runs != nil {
		endRun = o.runs.begin()
	}
	defer endRun()
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

	identifier := fmt.Sprintf("independent-%d", independentSequence.Add(1))
	temporary := conversation.NewConversation(identifier, time.Now())
	userText := o.redactText(strings.TrimSpace(request.UserText))
	if userText == "" {
		userText = o.skillInvocationText(request.Invocation)
	}
	state := &executionState{activity: activity, profile: profile}
	if err := o.configureCandidateState(state, temporary.ID); err != nil {
		return RunResult{}, err
	}
	defer state.clearModelContentSlots()
	runtime := o.hookRuntime()
	state.ref = runtime.BeginTurn(requestCtxOrBackground(ctx), request.Main.ID, hook.ExecutionIsolatedSkill, hookMode(request.Mode))
	message := runtime.BeginMessage(requestCtxOrBackground(ctx), state.ref, hook.MessageUser, userText)
	o.appendConversationMessage(temporary, conversation.RoleUser, o.safeText(userText), nil)
	runtime.EndMessage(requestCtxOrBackground(ctx), message)
	runRequest := RunRequest{UserText: userText, Mode: request.Mode, Profile: profile, Activity: activity}
	childEvents := make(chan events.Event)
	resultCh := make(chan RunResult, 1)
	independentCtx := withIndependentSkillHistory(ctx, request.Main, request.Invocation.History)
	go func() {
		result := o.runAgentLoop(independentCtx, temporary, runRequest, state, childEvents, time.Now())
		o.endTurn(state.ref, result)
		resultCh <- result
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
		event.Text = o.safeText(event.Text.Text())
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
	if result.Err != nil {
		return result, result.Err
	}
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
	endRun := func() {}
	if o != nil && o.runs != nil {
		endRun = o.runs.begin()
	}
	runHandedOff := false
	defer func() {
		if !runHandedOff {
			endRun()
		}
	}()

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

	parentProfile, err := o.buildExecutionProfile(mode, activity, 0)
	if err != nil {
		if prepared.Activity != nil {
			prepared.Activity.Clear()
		}
		return nil, skill.PreparedInvocation{}, err
	}
	out := make(chan events.Event)
	mainCheckpoint := checkpointConversation(main)
	o.appendConversationMessage(main, conversation.RoleUser, o.safeText(raw), nil)
	go func() {
		defer close(out)
		defer endRun()
		start := time.Now()
		if !emitEvent(requestCtxOrBackground(ctx), out, events.Event{Type: events.UserSubmitted, Text: o.safeText(raw)}) {
			if prepared.Activity != nil {
				prepared.Activity.Clear()
			}
			return
		}
		result, runErr := o.RunIndependent(ctx, IndependentRequest{
			Invocation: prepared,
			Main:       main,
			Profile:    parentProfile,
			UserText:   raw,
			Mode:       mode,
		}, out)
		if runErr != nil {
			if isIndependentSkillHistoryError(runErr) {
				restoreConversation(main, mainCheckpoint)
				emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.safeError("orchestrator_independent_failed", runErr)})
				return
			}
			_ = o.saveConversationDirect(ctx, main)
			emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.safeError("orchestrator_independent_failed", runErr)})
			return
		}
		if err := o.appendAssistantTransaction(main, result.FinalText, func() error { return o.saveConversationDirect(ctx, main) }); err != nil {
			emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.safeError("orchestrator_independent_save_failed", err)})
			return
		}
		o.updateMemoryAfterCompleted(RunRequest{UserText: raw, Mode: mode}, result.FinalText)
		if !emitEvent(ctx, out, events.Event{Type: events.TextDelta, Text: o.safeText(result.FinalText)}) {
			return
		}
		if !emitEvent(ctx, out, o.progressEvent(1, 1, string(StopReasonCompleted), "已完成")) {
			return
		}
		emitEvent(ctx, out, events.Event{Type: events.Done, Duration: time.Since(start)})
	}()
	runHandedOff = true
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
	result, err := o.RunIndependent(ctx, IndependentRequest{
		Invocation: prepared,
		Main:       main,
		Profile:    state.profile,
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
	if !emitEvent(ctx, out, events.Event{Type: events.TextDelta, Text: o.safeText(result.FinalText)}) {
		return result, ctx.Err()
	}
	if !emitEvent(ctx, out, o.progressEvent(1, 1, string(StopReasonCompleted), "已完成")) {
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
	_, err := o.store.Save(saveCtx, conv)
	return err
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
	o.appendConversationMessage(conv, conversation.RoleAssistant, o.safeText(text), nil)
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
		if message.Tool == nil {
			continue
		}
		toolState := *message.Tool
		if message.Tool.Artifact != nil {
			artifactRef := *message.Tool.Artifact
			toolState.Artifact = &artifactRef
		}
		if message.Tool.Error != nil {
			safeError := *message.Tool.Error
			toolState.Error = &safeError
		}
		result[index].Tool = &toolState
	}
	return result
}
