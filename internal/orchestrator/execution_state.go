package orchestrator

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type executionState struct {
	activity          *skill.Activity
	profile           skill.ExecutionProfile
	ref               hook.ExecutionRef
	requestGeneration uint64
	modelContents     modelContentSlotState
	parentPromptMu    sync.Mutex
	parentPromptRef   hook.ExecutionRef
	parentPrompt      provider.PromptPrefixSnapshot
}

func (s *executionState) captureParentPrompt(ref hook.ExecutionRef, snapshot provider.PromptPrefixSnapshot) error {
	if s == nil || s.requestGeneration == 0 || ref.ExecutionID == "" || ref.ExecutionID != s.ref.ExecutionID || ref.SessionID != s.ref.SessionID {
		return fmt.Errorf("parent prompt identity is invalid")
	}
	cloned, err := clonePromptPrefixSnapshot(snapshot)
	if err != nil {
		return err
	}
	s.parentPromptMu.Lock()
	s.parentPromptRef = ref
	s.parentPrompt = cloned
	s.parentPromptMu.Unlock()
	return nil
}

func (s *executionState) currentParentPrompt(ref hook.ExecutionRef) (provider.PromptPrefixSnapshot, error) {
	if s == nil || s.requestGeneration == 0 {
		return provider.PromptPrefixSnapshot{}, fmt.Errorf("parent prompt state is unavailable")
	}
	s.parentPromptMu.Lock()
	storedRef := s.parentPromptRef
	stored := s.parentPrompt
	s.parentPromptMu.Unlock()
	if storedRef.ExecutionID == "" || storedRef.ExecutionID != ref.ExecutionID || storedRef.SessionID != ref.SessionID {
		return provider.PromptPrefixSnapshot{}, fmt.Errorf("parent prompt identity is stale")
	}
	return clonePromptPrefixSnapshot(stored)
}

// modelContentSlotKey identifies one temporary model-only tool result. CallID
// is deliberately not part of the identity: it is an untrusted consistency
// field and cannot distinguish concurrent runs or repeated calls.
type modelContentSlotKey struct {
	conversationID    string
	requestGeneration uint64
	iteration         int
	ordinal           uint64
	messageIndex      int
}

// modelContentSlot contains only the bounded model projection and routing
// metadata owned by one executionState. It must never grow Result, UserView,
// Store, artifact payload, path, or reader capabilities.
type modelContentSlot struct {
	key     modelContentSlotKey
	callID  string
	content redact.SafeText
	bytes   int64
}

type modelContentSlotState struct {
	mu                sync.Mutex
	conversationID    string
	requestGeneration uint64
	iteration         int
	nextOrdinal       uint64
	maxRecordBytes    int64
	maxSessionBytes   int64
	totalBytes        int64
	entries           map[modelContentSlotKey]modelContentSlot
}

func (s *executionState) configureModelContentSlots(conversationID string, requestGeneration uint64, maxRecordBytes, maxSessionBytes int64) error {
	if s == nil {
		return fmt.Errorf("execution state is nil")
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" || requestGeneration == 0 {
		return fmt.Errorf("model content slot owner is invalid")
	}
	if maxRecordBytes <= 0 || maxSessionBytes <= 0 || maxRecordBytes > maxSessionBytes {
		return fmt.Errorf("model content slot limits are invalid")
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	if slots.conversationID != "" && (slots.conversationID != conversationID || slots.requestGeneration != requestGeneration) {
		slots.clearLocked()
		return fmt.Errorf("model content slot owner cannot be replaced")
	}
	if len(slots.entries) != 0 || slots.totalBytes != 0 {
		return fmt.Errorf("model content slots are active")
	}
	slots.conversationID = conversationID
	slots.requestGeneration = requestGeneration
	slots.maxRecordBytes = maxRecordBytes
	slots.maxSessionBytes = maxSessionBytes
	if slots.entries == nil {
		slots.entries = make(map[modelContentSlotKey]modelContentSlot)
	}
	return nil
}

func (s *executionState) beginModelContentIteration(conversationID string, requestGeneration uint64, iteration int) error {
	if s == nil {
		return fmt.Errorf("execution state is nil")
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	if strings.TrimSpace(conversationID) == "" || conversationID != slots.conversationID || requestGeneration == 0 || requestGeneration != slots.requestGeneration {
		slots.clearLocked()
		return fmt.Errorf("model content slot owner mismatch")
	}
	if iteration <= 0 {
		slots.clearLocked()
		return fmt.Errorf("model content slot iteration is invalid")
	}
	if slots.maxRecordBytes <= 0 || slots.maxSessionBytes <= 0 {
		slots.clearLocked()
		return fmt.Errorf("model content slots are not configured")
	}
	if slots.iteration != iteration {
		// A new iteration cannot inherit a prior request's temporary projection.
		slots.clearLocked()
		slots.iteration = iteration
		slots.nextOrdinal = 0
	}
	return nil
}

// stageModelContent atomically checks all routing and byte-budget invariants
// before publishing a slot entry. On error it leaves every entry, byte total,
// and the next ordinal unchanged.
func (s *executionState) stageModelContent(conversationID string, requestGeneration uint64, iteration int, messageIndex int, callID string, content redact.SafeText) (modelContentSlotKey, error) {
	if s == nil {
		return modelContentSlotKey{}, fmt.Errorf("execution state is nil")
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	if conversationID != slots.conversationID || requestGeneration != slots.requestGeneration || iteration != slots.iteration || iteration <= 0 {
		return modelContentSlotKey{}, fmt.Errorf("model content slot owner or iteration mismatch")
	}
	if messageIndex < 0 || strings.TrimSpace(callID) == "" || content.Text() == "" {
		return modelContentSlotKey{}, fmt.Errorf("model content slot input is invalid")
	}
	contentBytes := int64(len(content.Text()))
	if contentBytes > slots.maxRecordBytes {
		return modelContentSlotKey{}, fmt.Errorf("model content exceeds record limit")
	}
	total, ok := checkedModelContentAdd(slots.totalBytes, contentBytes)
	if !ok || total > slots.maxSessionBytes {
		return modelContentSlotKey{}, fmt.Errorf("model content exceeds session limit")
	}
	if slots.nextOrdinal == math.MaxUint64 {
		return modelContentSlotKey{}, fmt.Errorf("model content ordinal overflow")
	}
	for _, existing := range slots.entries {
		if existing.key.iteration == iteration && existing.key.messageIndex == messageIndex {
			return modelContentSlotKey{}, fmt.Errorf("model content message mapping is not unique")
		}
	}
	key := modelContentSlotKey{
		conversationID:    conversationID,
		requestGeneration: requestGeneration,
		iteration:         iteration,
		ordinal:           slots.nextOrdinal,
		messageIndex:      messageIndex,
	}
	entry := modelContentSlot{key: key, callID: callID, content: content, bytes: contentBytes}
	if slots.entries == nil {
		slots.entries = make(map[modelContentSlotKey]modelContentSlot)
	}
	slots.entries[key] = entry
	slots.totalBytes = total
	slots.nextOrdinal++
	return key, nil
}

// lookupModelContent requires the complete slot key; callID is checked only
// for consistency and is never used as an index.
func (s *executionState) lookupModelContent(key modelContentSlotKey, callID string) (redact.SafeText, bool, error) {
	if s == nil {
		return redact.SafeText{}, false, fmt.Errorf("execution state is nil")
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	entry, ok := slots.entries[key]
	if !ok {
		return redact.SafeText{}, false, nil
	}
	if entry.callID != callID {
		slots.clearLocked()
		return redact.SafeText{}, false, fmt.Errorf("model content call identity mismatch")
	}
	return entry.content, true, nil
}

func (s *executionState) lookupModelContentForMessage(conversationID string, iteration, messageIndex int, callID string) (redact.SafeText, bool, error) {
	if s == nil {
		return redact.SafeText{}, false, nil
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	if conversationID != slots.conversationID || iteration != slots.iteration {
		slots.clearLocked()
		return redact.SafeText{}, false, fmt.Errorf("model content provider mapping owner mismatch")
	}
	var matched *modelContentSlot
	for _, entry := range slots.entries {
		if entry.key.conversationID != conversationID || entry.key.requestGeneration != slots.requestGeneration ||
			entry.key.iteration != iteration || entry.key.messageIndex != messageIndex {
			continue
		}
		if matched != nil {
			slots.clearLocked()
			return redact.SafeText{}, false, fmt.Errorf("model content provider mapping is ambiguous")
		}
		entryCopy := entry
		matched = &entryCopy
	}
	if matched == nil {
		return redact.SafeText{}, false, nil
	}
	if matched.callID != callID {
		slots.clearLocked()
		return redact.SafeText{}, false, fmt.Errorf("model content call identity mismatch")
	}
	return matched.content, true, nil
}

func (s *executionState) stageCurrentModelContent(iteration, messageIndex int, callID string, content redact.SafeText) (modelContentSlotKey, error) {
	if s == nil {
		return modelContentSlotKey{}, fmt.Errorf("execution state is nil")
	}
	slots := &s.modelContents
	slots.mu.Lock()
	conversationID := slots.conversationID
	requestGeneration := slots.requestGeneration
	slots.mu.Unlock()
	return s.stageModelContent(conversationID, requestGeneration, iteration, messageIndex, callID, content)
}

// rebaseModelContentSlots applies an old message index to candidate survivor
// indices mapping as one transaction. No candidate means the message was
// evicted. Multiple candidates, invalid indices, or collisions are ambiguous:
// fail closed by clearing the temporary slot before returning the error.
func (s *executionState) rebaseModelContentSlots(oldToNew map[int][]int) error {
	if s == nil {
		return fmt.Errorf("execution state is nil")
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	rebased := make(map[modelContentSlotKey]modelContentSlot, len(slots.entries))
	usedMessageIndices := make(map[int]struct{}, len(slots.entries))
	var total int64
	for _, entry := range slots.entries {
		candidates := oldToNew[entry.key.messageIndex]
		if len(candidates) == 0 {
			continue
		}
		if len(candidates) != 1 || candidates[0] < 0 {
			slots.clearLocked()
			return fmt.Errorf("model content survivor mapping is ambiguous")
		}
		newIndex := candidates[0]
		if _, exists := usedMessageIndices[newIndex]; exists {
			slots.clearLocked()
			return fmt.Errorf("model content survivor mapping is not unique")
		}
		usedMessageIndices[newIndex] = struct{}{}
		entry.key.messageIndex = newIndex
		if _, exists := rebased[entry.key]; exists {
			slots.clearLocked()
			return fmt.Errorf("model content survivor key collided")
		}
		var ok bool
		total, ok = checkedModelContentAdd(total, entry.bytes)
		if !ok || total > slots.maxSessionBytes {
			slots.clearLocked()
			return fmt.Errorf("model content survivor byte count is invalid")
		}
		rebased[entry.key] = entry
	}
	slots.entries = rebased
	slots.totalBytes = total
	return nil
}

func (s *executionState) clearModelContentSlots() {
	if s == nil {
		return
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	slots.clearLocked()
}

func (s *modelContentSlotState) clearLocked() {
	clear(s.entries)
	s.totalBytes = 0
	s.nextOrdinal = 0
}

func (s *executionState) modelContentSlotStats() (count int, totalBytes int64) {
	if s == nil {
		return 0, 0
	}
	slots := &s.modelContents
	slots.mu.Lock()
	defer slots.mu.Unlock()
	return len(slots.entries), slots.totalBytes
}

func checkedModelContentAdd(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}

func cancelToolExecutionBeforeStart(execution ToolExecution, err error) ToolExecution {
	execution.State = tool.CancelledBeforeStart
	execution.Result = tool.Result{}
	execution.HasResult = false
	execution.Projection = contextmgr.ToolResultProjection{}
	execution.Projected = false
	execution.Duration = 0
	execution.StopReason = StopReasonCancelled
	execution.Err = err
	return execution
}

func recordToolExecutionResult(execution ToolExecution, result tool.Result, duration time.Duration) ToolExecution {
	execution.Result = result
	execution.HasResult = result.CallID != ""
	execution.Duration = duration
	if !execution.HasResult {
		return execution
	}
	if state := result.ExecutionState(); state.CanProduceResult() {
		execution.State = state
	} else {
		execution.State = executionStateForResult(result)
	}
	return execution
}

func executionStateForResult(result tool.Result) tool.ExecutionState {
	switch result.Status {
	case tool.StatusDenied:
		return tool.Rejected
	case tool.StatusTimeout:
		return tool.CancelledAfterStart
	default:
		return tool.Completed
	}
}

func (o *Orchestrator) newExecutionState(req RunRequest) (*executionState, error) {
	activity := req.Activity
	if activity == nil {
		activity = skill.NewActivity()
	}
	state := &executionState{activity: activity, profile: req.Profile.Clone()}
	if profileIsUnset(state.profile) {
		profile, err := o.buildExecutionProfile(req.Mode, activity, 0)
		if err != nil {
			return nil, err
		}
		state.profile = profile
	}
	return state, nil
}

func profileIsUnset(profile skill.ExecutionProfile) bool {
	return len(profile.Catalog) == 0 && len(profile.Activity.Active) == 0 && profile.Model == "" && profile.AllowedTools == nil && len(profile.ReadRoots) == 0 && profile.IndependentDepth == 0 && !profile.Persist && !profile.UpdateMemory
}

func (o *Orchestrator) buildExecutionProfile(mode RunMode, activity *skill.Activity, depth int) (skill.ExecutionProfile, error) {
	snapshot := skill.Snapshot{}
	if o.skillManager != nil {
		snapshot = o.skillManager.Snapshot()
	}
	return o.buildExecutionProfileWithSnapshot(mode, snapshot, activity, depth)
}

func (o *Orchestrator) buildExecutionProfileWithSnapshot(mode RunMode, snapshot skill.Snapshot, activity *skill.Activity, depth int) (skill.ExecutionProfile, error) {
	if activity == nil {
		activity = skill.NewActivity()
	}
	baseTools := []string{}
	if o.registry != nil {
		baseTools = o.registry.Names()
	}
	profile, err := skill.BuildProfile(skill.ProfileRequest{
		Snapshot:         snapshot,
		Activity:         activity.Snapshot(),
		DefaultModel:     o.defaultModel,
		BaseTools:        baseTools,
		ReadOnly:         mode == RunModePlan,
		IndependentDepth: depth,
	})
	if err != nil {
		return skill.ExecutionProfile{}, err
	}
	if o.registry != nil {
		if _, ok := o.registry.Get(tool.LoadSkillToolName); !ok {
			if o.skillManager != nil {
				return skill.ExecutionProfile{}, fmt.Errorf("系统工具 %q 未注册", tool.LoadSkillToolName)
			}
			delete(profile.AllowedTools, tool.LoadSkillToolName)
		}
	}
	return profile, nil
}

func (o *Orchestrator) BuildExecutionProfile(mode RunMode, activity *skill.Activity, depth int) (skill.ExecutionProfile, error) {
	return o.buildExecutionProfile(mode, activity, depth)
}

func (o *Orchestrator) registryForProfile(mode RunMode, profile skill.ExecutionProfile) (*tool.Registry, error) {
	if o.registry == nil {
		return nil, nil
	}
	always := []string{}
	if _, ok := o.registry.Get(tool.LoadSkillToolName); ok {
		always = append(always, tool.LoadSkillToolName)
	}
	return o.registry.View(tool.ViewOptions{
		AllowedNames:  profile.AllowedTools,
		AlwaysInclude: always,
		ReadOnly:      mode == RunModePlan,
	})
}

// preflightRegistryForProfile applies the active Skill visibility policy but
// deliberately does not apply Plan Mode's read-only filter. That policy is a
// separate, later gate so fabricated provider calls are rejected at the
// correct boundary and never reach permission normalization or Hooks.
func (o *Orchestrator) preflightRegistryForProfile(mode RunMode, profile skill.ExecutionProfile) (*tool.Registry, error) {
	if o.registry == nil {
		return nil, nil
	}
	allowed := profile.AllowedTools
	if mode == RunModePlan {
		// BuildProfile records the unfiltered Skill whitelist on Activity even
		// though AllowedTools itself has already been intersected with Plan.
		// Reconstruct only that Skill restriction here.
		if profile.Activity.AllowedTools == nil {
			allowed = nil
		} else {
			allowed = make(map[string]struct{}, len(profile.Activity.AllowedTools))
			for _, name := range profile.Activity.AllowedTools {
				allowed[name] = struct{}{}
			}
		}
	}
	always := []string{}
	if _, ok := o.registry.Get(tool.LoadSkillToolName); ok {
		always = append(always, tool.LoadSkillToolName)
	}
	return o.registry.View(tool.ViewOptions{
		AllowedNames:  allowed,
		AlwaysInclude: always,
		ReadOnly:      false,
	})
}

func (o *Orchestrator) contextWithReadScope(ctx context.Context, profile skill.ExecutionProfile) (context.Context, error) {
	if o.executor == nil || len(profile.ReadRoots) == 0 {
		return ctx, nil
	}
	scope, err := tool.NewPinnedReadScope(o.executor.ProjectRoot, profile.ReadRoots)
	if err != nil {
		return nil, err
	}
	return tool.WithReadScope(ctx, scope), nil
}

func (o *Orchestrator) refreshExecutionProfile(mode RunMode, state *executionState) error {
	if state == nil {
		return fmt.Errorf("execution state is nil")
	}
	profile, err := o.buildExecutionProfile(mode, state.activity, state.profile.IndependentDepth)
	if err != nil {
		return err
	}
	profile.Persist = state.profile.Persist
	profile.UpdateMemory = state.profile.UpdateMemory
	state.profile = profile
	return nil
}
