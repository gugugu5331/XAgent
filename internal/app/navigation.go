package app

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"

	"xagent/internal/command"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/redact"
)

// NavigationKind is the App-owned subset of command intents that starts a
// navigation transaction. Cancel and quit are control intents, not navigation.
type NavigationKind string

const (
	NavigationNewConversation  NavigationKind = NavigationKind(command.IntentNewConversation)
	NavigationShowSessions     NavigationKind = NavigationKind(command.IntentShowSessions)
	NavigationOpenConversation NavigationKind = NavigationKind(command.IntentOpenConversation)
)

// NavigationRequest binds every asynchronous navigation result to the intent
// and optional session that created it.
type NavigationRequest struct {
	Generation uint64
	Kind       NavigationKind
	SessionID  string
}

var (
	errNavigationGenerationExhausted = errors.New("app: navigation generation exhausted")
	errNavigationIntentInvalid       = errors.New("app: invalid navigation intent")
	errNavigationSessionIDRequired   = errors.New("app: navigation session ID required")
	errNavigationSessionIDUnexpected = errors.New("app: unexpected navigation session ID")
	errNavigationPreparationInvalid  = errors.New("app: navigation preparation is not ready")
	errNavigationStoreUnavailable    = errors.New("app: navigation store is unavailable")
	errNavigationCandidateInvalid    = errors.New("app: navigation candidate is invalid")
)

const (
	staleNavigationDiagnosticCode   = "navigation_stale_generation"
	staleNavigationDiagnosticSource = "app_navigation"
	staleNavigationDiagnosticHint   = "late_result_dropped"
	waitNavigationErrorCode         = "navigation_wait_idle_failed"
	saveNavigationErrorCode         = "navigation_save_failed"
	listNavigationErrorCode         = "navigation_list_failed"
	createNavigationErrorCode       = "navigation_create_failed"
	loadNavigationErrorCode         = "navigation_load_failed"
	candidateNavigationErrorCode    = "navigation_candidate_invalid"
	navigationRetryHint             = "retry_navigation"
)

type navigationWaiter interface {
	WaitIdle(context.Context) error
}

type navigationSaver interface {
	Save(context.Context, *conversation.Conversation) (conversation.SaveResult, error)
}

type navigationCandidateStore interface {
	Create(context.Context) (*conversation.Conversation, error)
	List(context.Context) (conversation.ListResult, error)
	Load(context.Context, string) (conversation.LoadResult, error)
}

type navigationSessionHooks interface {
	SessionStart(context.Context, string, hook.SessionState)
	SessionEnd(context.Context, string, hook.SessionEndReason)
}

// navigationPreparation is the immutable output of the WaitIdle/Save stage.
// Candidate construction is intentionally deferred to T4.8.
type navigationPreparation struct {
	request    NavigationRequest
	saveResult conversation.SaveResult
	ready      bool
	saved      bool
}

// navigationViewState is the complete App-owned state that a navigation may
// replace. Candidate construction freezes its values while separately
// preserving the Store-tracked Conversation pointer for the later commit.
type navigationViewState struct {
	screen       screen
	conversation ConversationState
	messages     []redact.SafeText
	active       *conversation.Conversation
}

type navigationCandidateSnapshot struct {
	request       NavigationRequest
	screen        screen
	conversation  ConversationState
	messages      []redact.SafeText
	active        *conversation.Conversation
	saveResult    conversation.SaveResult
	saved         bool
	listResult    conversation.ListResult
	listState     ConversationListState
	persisted     conversation.PersistedState
	recovery      conversation.RecoveryReport
	created       bool
	changesActive bool
}

// CommitCandidate is an opaque, immutable navigation publication. It is
// created only after the requested Store operation succeeds and its generation
// is still current. Its snapshot method returns a defensive copy for the later
// atomic commit stage.
type CommitCandidate struct {
	value         navigationCandidateSnapshot
	trackedActive *conversation.Conversation
}

func (candidate *CommitCandidate) snapshot() navigationCandidateSnapshot {
	if candidate == nil {
		return navigationCandidateSnapshot{}
	}
	return cloneNavigationCandidateSnapshot(candidate.value)
}

// navigationCommitTarget contains the App-owned memory cells replaced by a
// successful navigation. The target itself never escapes the synchronous
// commit call and carries no service capability.
type navigationCommitTarget struct {
	runtime      *RuntimeState
	screen       *screen
	conversation *ConversationState
	request      *RequestState
	messages     *[]redact.SafeText
	active       **conversation.Conversation
	list         *ConversationListState
	activity     skillActivityClearer
}

// navigationState serializes generation allocation and guarded publication.
// Keeping the generation check and commit callback under the same lock closes
// the check-then-publish race with a newer navigation request.
type navigationState struct {
	mu          sync.Mutex
	sequence    uint64
	current     NavigationRequest
	diagnostics diagnostics.BoundedSink
	redactor    *redact.RuntimeRedactor
}

func newNavigationState(sink diagnostics.BoundedSink, redactor *redact.RuntimeRedactor) *navigationState {
	if sink == nil {
		panic("app: nil navigation diagnostics sink")
	}
	if redactor == nil {
		panic("app: nil navigation runtime redactor")
	}
	return &navigationState{diagnostics: sink, redactor: redactor}
}

// beginNavigation validates the command intent before atomically replacing the
// active request. Allocation happens last so invalid input and exhaustion leave
// the previous request unchanged.
func (state *navigationState) beginNavigation(intent command.IntentKind, sessionID string) (NavigationRequest, error) {
	if state == nil {
		return NavigationRequest{}, errNavigationIntentInvalid
	}
	kind, err := navigationKind(intent, sessionID)
	if err != nil {
		return NavigationRequest{}, err
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sequence == math.MaxUint64 {
		return NavigationRequest{}, errNavigationGenerationExhausted
	}
	next := NavigationRequest{
		Generation: state.sequence + 1,
		Kind:       kind,
		SessionID:  sessionID,
	}
	state.sequence = next.Generation
	state.current = next
	return next, nil
}

// commitNavigation publishes an asynchronous result only while its complete
// request identity is current. A rejected result cannot run commit and records
// only stable, content-free metadata through the bounded diagnostics boundary.
// The commit callback runs while the navigation lock is held and therefore must
// be a synchronous, non-blocking, infallible memory publication. It must not
// re-enter beginNavigation or commitNavigation.
func (state *navigationState) commitNavigation(request NavigationRequest, commit func()) bool {
	if state == nil || commit == nil {
		return false
	}

	state.mu.Lock()
	if request.Generation == 0 || request != state.current {
		state.mu.Unlock()
		state.recordStaleNavigation()
		return false
	}
	defer state.mu.Unlock()

	commit()
	state.current = NavigationRequest{}
	return true
}

// commitNavigationCandidate is the only publication point for a sealed
// candidate. The generation check, old SessionEnd, complete memory update and
// new SessionStart are serialized under the navigation lock, so neither a
// newer intent nor an observer Hook can see an intermediate App state.
func (state *navigationState) commitNavigationCandidate(
	ctx context.Context,
	candidate *CommitCandidate,
	target navigationCommitTarget,
	hooks navigationSessionHooks,
) bool {
	if state == nil || candidate == nil {
		return false
	}
	snapshot := candidate.snapshot()
	if !validNavigationCommitTarget(snapshot, target) {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	state.mu.Lock()
	if snapshot.request.Generation == 0 || snapshot.request != state.current || candidate.value.request != snapshot.request {
		state.mu.Unlock()
		state.recordStaleNavigation()
		return false
	}
	trackedActive := candidate.trackedActive
	if snapshot.request.Kind != NavigationShowSessions &&
		(trackedActive == nil || trackedActive.ID != snapshot.conversation.ActiveID) {
		state.mu.Unlock()
		return false
	}
	defer state.mu.Unlock()

	previousID := target.conversation.ActiveID
	changesActive := snapshot.request.Kind != NavigationShowSessions && previousID != snapshot.conversation.ActiveID
	if changesActive && previousID != "" && hooks != nil {
		hooks.SessionEnd(ctx, previousID, hook.SessionEndSwitch)
	}

	if snapshot.request.Kind == NavigationShowSessions {
		if target.list != nil {
			*target.list = cloneConversationListState(snapshot.listState)
		}
		*target.screen = snapshot.screen
	} else if changesActive {
		target.runtime.resetConversation(target.conversation, target.request, snapshot.conversation.ActiveID, target.activity)
		nextConversation := cloneNavigationConversationState(snapshot.conversation)
		nextConversation.SkillGeneration = target.conversation.SkillGeneration
		*target.conversation = nextConversation
		*target.messages = cloneNavigationSafeTexts(snapshot.messages)
		*target.active = materializeTrackedNavigationConversation(trackedActive, snapshot.active)
		*target.screen = snapshot.screen
	} else {
		// Reloading the current ActiveID refreshes persisted content without
		// pretending that the user switched sessions or clearing live mode,
		// input, Skill Activity, request state, or notices. Keep the existing
		// Store-tracked root pointer: a task notification may have synchronously
		// advanced that pointer and its JSONL baseline after this Load candidate
		// was sealed. Merge only those append-only notifications into the frozen
		// reload before publishing it.
		refreshed := mergeNavigationTaskNotifications(snapshot.active, *target.active)
		target.conversation.Messages = navigationMessageProjection(refreshed)
		*target.messages = navigationMessageProjection(refreshed)
		*target.active = materializeTrackedNavigationConversation(*target.active, refreshed)
		*target.screen = snapshot.screen
	}

	state.current = NavigationRequest{}
	if changesActive && hooks != nil {
		sessionState := hook.SessionResumed
		if snapshot.created {
			sessionState = hook.SessionNew
		}
		hooks.SessionStart(ctx, snapshot.conversation.ActiveID, sessionState)
	}
	return true
}

// materializeTrackedNavigationConversation restores the sealed snapshot into
// the exact Conversation pointer returned by Store. JSONLStore tracks loaded
// and created conversations by pointer identity, so publishing a clone would
// make the next Save fail with a false conflict.
func materializeTrackedNavigationConversation(tracked, frozen *conversation.Conversation) *conversation.Conversation {
	materialized := cloneNavigationConversation(frozen)
	*tracked = *materialized
	return tracked
}

func mergeNavigationTaskNotifications(frozen, live *conversation.Conversation) *conversation.Conversation {
	merged := cloneNavigationConversation(frozen)
	if merged == nil || live == nil {
		return merged
	}
	seen := make(map[string]struct{})
	for _, message := range merged.Messages {
		if message.Role == conversation.RoleSubagentNotification && message.Subagent != nil {
			seen[message.Subagent.NotificationID] = struct{}{}
		}
	}
	for _, message := range live.Messages {
		if message.Role != conversation.RoleSubagentNotification || message.Subagent == nil {
			continue
		}
		if _, exists := seen[message.Subagent.NotificationID]; exists {
			continue
		}
		merged.Messages = append(merged.Messages, cloneNavigationMessage(message))
		seen[message.Subagent.NotificationID] = struct{}{}
	}
	if live.UpdatedAt.After(merged.UpdatedAt) {
		merged.UpdatedAt = live.UpdatedAt
	}
	return merged
}

// publishNavigationListFailure consumes a current failed List request by
// publishing only its fixed SafeError. The current screen, trusted list and
// active conversation are deliberately not reachable from this function.
func (state *navigationState) publishNavigationListFailure(request NavigationRequest, failure *diagnostics.SafeError, target *RequestState) bool {
	if state == nil || failure == nil || target == nil || request.Kind != NavigationShowSessions ||
		failure.Code != listNavigationErrorCode || failure.Source != staleNavigationDiagnosticSource {
		return false
	}
	state.mu.Lock()
	if request.Generation == 0 || request != state.current {
		state.mu.Unlock()
		state.recordStaleNavigation()
		return false
	}
	defer state.mu.Unlock()
	cloned := *failure
	cloned.Message = state.redactor.Redact(failure.Message.Text())
	target.LastError = &cloned
	state.current = NavigationRequest{}
	return true
}

func validNavigationCommitTarget(snapshot navigationCandidateSnapshot, target navigationCommitTarget) bool {
	if target.screen == nil || target.conversation == nil || target.messages == nil || target.active == nil {
		return false
	}
	switch snapshot.request.Kind {
	case NavigationShowSessions:
		return snapshot.screen == screenList && !snapshot.created
	case NavigationNewConversation:
		if !snapshot.created || snapshot.request.SessionID != "" {
			return false
		}
		return validNavigationConversationCommit(snapshot, target)
	case NavigationOpenConversation:
		if snapshot.created || snapshot.request.SessionID == "" || snapshot.conversation.ActiveID != snapshot.request.SessionID {
			return false
		}
		return validNavigationConversationCommit(snapshot, target)
	default:
		return false
	}
}

func validNavigationConversationCommit(snapshot navigationCandidateSnapshot, target navigationCommitTarget) bool {
	if snapshot.screen != screenChat || snapshot.active == nil || strings.TrimSpace(snapshot.conversation.ActiveID) == "" || snapshot.active.ID != snapshot.conversation.ActiveID {
		return false
	}
	if !sameNavigationSafeTexts(snapshot.conversation.Messages, snapshot.messages) || !sameNavigationSafeTexts(navigationMessageProjection(snapshot.active), snapshot.messages) {
		return false
	}
	if target.conversation.ActiveID == snapshot.conversation.ActiveID {
		return *target.active != nil && (*target.active).ID == snapshot.conversation.ActiveID
	}
	return target.runtime != nil && target.request != nil && target.activity != nil && target.runtime.RequestSequence != math.MaxUint64
}

func sameNavigationSafeTexts(left, right []redact.SafeText) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Text() != right[index].Text() {
			return false
		}
	}
	return true
}

// prepareNavigation executes the only permitted pre-candidate side effects in
// strict order. A newer generation can supersede this request while WaitIdle or
// Save is blocked; each boundary is rechecked before later work is allowed.
func (state *navigationState) prepareNavigation(
	ctx context.Context,
	request NavigationRequest,
	waiter navigationWaiter,
	saver navigationSaver,
	active *conversation.Conversation,
) (navigationPreparation, *diagnostics.SafeError) {
	if state == nil || !state.isCurrentNavigation(request) {
		if state != nil {
			state.recordStaleNavigation()
		}
		return navigationPreparation{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if waiter == nil {
		failure := errors.New("navigation WaitIdle dependency is unavailable")
		if !state.isCurrentNavigation(request) {
			state.recordStaleNavigation()
			return navigationPreparation{}, nil
		}
		return navigationPreparation{}, state.navigationFailure(waitNavigationErrorCode, "导航前等待当前请求收尾失败，请重试", failure)
	}
	if err := waiter.WaitIdle(ctx); err != nil {
		if !state.isCurrentNavigation(request) {
			state.recordStaleNavigation()
			return navigationPreparation{}, nil
		}
		return navigationPreparation{}, state.navigationFailure(waitNavigationErrorCode, "导航前等待当前请求收尾失败，请重试", err)
	}
	if !state.isCurrentNavigation(request) {
		state.recordStaleNavigation()
		return navigationPreparation{}, nil
	}

	prepared := navigationPreparation{request: request, ready: true}
	if active == nil {
		return prepared, nil
	}
	if saver == nil {
		failure := errors.New("navigation Save dependency is unavailable")
		if !state.isCurrentNavigation(request) {
			state.recordStaleNavigation()
			return navigationPreparation{}, nil
		}
		return navigationPreparation{}, state.navigationFailure(saveNavigationErrorCode, "导航前保存当前会话失败，请重试", failure)
	}
	result, err := saver.Save(ctx, active)
	if err != nil {
		if !state.isCurrentNavigation(request) {
			state.recordStaleNavigation()
			return navigationPreparation{}, nil
		}
		return navigationPreparation{}, state.navigationFailure(saveNavigationErrorCode, "导航前保存当前会话失败，请重试", err)
	}
	if !state.isCurrentNavigation(request) {
		state.recordStaleNavigation()
		return navigationPreparation{}, nil
	}
	prepared.saveResult = result
	prepared.saved = true
	return prepared, nil
}

// buildNavigationCandidate performs exactly one Store operation selected by
// the prepared intent. It builds the entire target state off to the side and
// returns no candidate when the request is stale or any validation fails.
func (state *navigationState) buildNavigationCandidate(
	ctx context.Context,
	prepared navigationPreparation,
	store navigationCandidateStore,
	current navigationViewState,
) (*CommitCandidate, *diagnostics.SafeError) {
	if state == nil {
		return nil, nil
	}
	if !prepared.ready {
		if !state.isCurrentNavigation(prepared.request) {
			state.recordStaleNavigation()
			return nil, nil
		}
		return nil, state.navigationFailure(candidateNavigationErrorCode, "导航候选尚未准备完成，请重试", errNavigationPreparationInvalid)
	}
	request := prepared.request
	if !state.isCurrentNavigation(request) {
		state.recordStaleNavigation()
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if store == nil {
		if !state.isCurrentNavigation(request) {
			state.recordStaleNavigation()
			return nil, nil
		}
		return nil, state.navigationFailure(candidateNavigationErrorCode, "导航服务暂不可用，请重试", errNavigationStoreUnavailable)
	}

	candidate := navigationCandidateSnapshot{
		request:    request,
		saveResult: prepared.saveResult,
		saved:      prepared.saved,
	}
	var trackedActive *conversation.Conversation
	var operationErr error
	var failureCode string
	var failureMessage string

	switch request.Kind {
	case NavigationShowSessions:
		candidate.listResult, operationErr = store.List(ctx)
		failureCode = listNavigationErrorCode
		failureMessage = "读取会话列表失败，请重试"
		if operationErr == nil {
			candidate.listState = newConversationListState(candidate.listResult, state.redactor)
			candidate.screen = screenList
			candidate.conversation = cloneNavigationConversationState(current.conversation)
			candidate.messages = cloneNavigationSafeTexts(current.messages)
			candidate.active = cloneNavigationConversation(current.active)
			trackedActive = current.active
		}
	case NavigationNewConversation:
		trackedActive, operationErr = store.Create(ctx)
		failureCode = createNavigationErrorCode
		failureMessage = "创建新会话失败，请重试"
		if operationErr == nil {
			operationErr = validateNavigationConversation(trackedActive, "")
		}
		if operationErr == nil {
			candidate.screen = screenChat
			candidate.active = cloneNavigationConversation(trackedActive)
			candidate.conversation = navigationConversationState(candidate.active)
			candidate.messages = navigationMessageProjection(candidate.active)
			candidate.recovery = conversation.RecoveryReport{Status: conversation.RecoveryClean}
			candidate.created = true
			candidate.changesActive = true
		}
	case NavigationOpenConversation:
		var loaded conversation.LoadResult
		loaded, operationErr = store.Load(ctx, request.SessionID)
		failureCode = loadNavigationErrorCode
		failureMessage = "加载会话失败，请重试"
		if operationErr == nil && !loaded.Available {
			operationErr = errNavigationCandidateInvalid
		}
		if operationErr == nil {
			operationErr = validateNavigationConversation(loaded.Conversation, request.SessionID)
		}
		if operationErr == nil {
			candidate.screen = screenChat
			trackedActive = loaded.Conversation
			candidate.active = cloneNavigationConversation(loaded.Conversation)
			candidate.conversation = navigationConversationState(loaded.Conversation)
			candidate.messages = navigationMessageProjection(loaded.Conversation)
			candidate.persisted = loaded.Persisted
			candidate.recovery = cloneNavigationRecoveryReport(loaded.Recovery)
			candidate.changesActive = true
		}
	default:
		operationErr = errNavigationIntentInvalid
		failureCode = candidateNavigationErrorCode
		failureMessage = "导航候选无效，请重试"
	}

	if operationErr != nil {
		if !state.isCurrentNavigation(request) {
			state.recordStaleNavigation()
			return nil, nil
		}
		return nil, state.navigationFailure(failureCode, failureMessage, operationErr)
	}
	candidate = cloneNavigationCandidateSnapshot(candidate)
	sealed, stillCurrent := state.sealNavigationCandidate(request, candidate, trackedActive)
	if !stillCurrent {
		state.recordStaleNavigation()
		return nil, nil
	}
	return sealed, nil
}

func (state *navigationState) sealNavigationCandidate(request NavigationRequest, candidate navigationCandidateSnapshot, trackedActive *conversation.Conversation) (*CommitCandidate, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if request.Generation == 0 || request != state.current || candidate.request != request {
		return nil, false
	}
	return &CommitCandidate{value: candidate, trackedActive: trackedActive}, true
}

func navigationConversationState(candidate *conversation.Conversation) ConversationState {
	return ConversationState{
		ActiveID: candidate.ID,
		Messages: navigationMessageProjection(candidate),
	}
}

func navigationMessageProjection(candidate *conversation.Conversation) []redact.SafeText {
	if candidate == nil || candidate.Messages == nil {
		return nil
	}
	projected := make([]redact.SafeText, len(candidate.Messages))
	for index := range candidate.Messages {
		projected[index] = candidate.Messages[index].Content
	}
	return projected
}

func validateNavigationConversation(candidate *conversation.Conversation, expectedID string) error {
	if candidate == nil || strings.TrimSpace(candidate.ID) == "" {
		return errNavigationCandidateInvalid
	}
	if expectedID != "" && candidate.ID != expectedID {
		return errNavigationCandidateInvalid
	}
	return nil
}

func cloneNavigationCandidateSnapshot(source navigationCandidateSnapshot) navigationCandidateSnapshot {
	clone := source
	clone.conversation = cloneNavigationConversationState(source.conversation)
	clone.messages = cloneNavigationSafeTexts(source.messages)
	clone.active = cloneNavigationConversation(source.active)
	clone.listResult = cloneNavigationListResult(source.listResult)
	clone.listState = cloneConversationListState(source.listState)
	clone.recovery = cloneNavigationRecoveryReport(source.recovery)
	return clone
}

func cloneNavigationConversationState(source ConversationState) ConversationState {
	clone := source
	clone.Skills = cloneNavigationStrings(source.Skills)
	clone.Messages = cloneNavigationSafeTexts(source.Messages)
	return clone
}

func cloneNavigationStrings(source []string) []string {
	if source == nil {
		return nil
	}
	return append(make([]string, 0, len(source)), source...)
}

func cloneNavigationSafeTexts(source []redact.SafeText) []redact.SafeText {
	if source == nil {
		return nil
	}
	return append(make([]redact.SafeText, 0, len(source)), source...)
}

func cloneNavigationConversation(source *conversation.Conversation) *conversation.Conversation {
	if source == nil {
		return nil
	}
	clone := *source
	if source.Messages != nil {
		clone.Messages = make([]conversation.Message, len(source.Messages))
		for index := range source.Messages {
			clone.Messages[index] = cloneNavigationMessage(source.Messages[index])
		}
	}
	if source.Context != nil {
		contextClone := *source.Context
		if source.Context.LastCompressionAt != nil {
			lastCompressionAt := *source.Context.LastCompressionAt
			contextClone.LastCompressionAt = &lastCompressionAt
		}
		clone.Context = &contextClone
	}
	return &clone
}

func cloneNavigationMessage(source conversation.Message) conversation.Message {
	clone := source
	if source.Subagent != nil {
		notificationClone := *source.Subagent
		clone.Subagent = &notificationClone
	}
	if source.Tool == nil {
		return clone
	}
	toolClone := *source.Tool
	if source.Tool.Artifact != nil {
		artifactClone := *source.Tool.Artifact
		toolClone.Artifact = &artifactClone
	}
	if source.Tool.Error != nil {
		errorClone := *source.Tool.Error
		toolClone.Error = &errorClone
	}
	clone.Tool = &toolClone
	return clone
}

func cloneNavigationListResult(source conversation.ListResult) conversation.ListResult {
	clone := source
	if source.Entries != nil {
		clone.Entries = make([]conversation.ListEntry, len(source.Entries))
		for index := range source.Entries {
			clone.Entries[index] = source.Entries[index]
			clone.Entries[index].Recovery = cloneNavigationRecoveryReport(source.Entries[index].Recovery)
		}
	}
	clone.Diagnostics = cloneNavigationDiagnostics(source.Diagnostics)
	return clone
}

func cloneNavigationRecoveryReport(source conversation.RecoveryReport) conversation.RecoveryReport {
	clone := source
	clone.Diagnostics = cloneNavigationDiagnostics(source.Diagnostics)
	return clone
}

func cloneNavigationDiagnostics(source []diagnostics.Diagnostic) []diagnostics.Diagnostic {
	if source == nil {
		return nil
	}
	clone := make([]diagnostics.Diagnostic, len(source))
	for index := range source {
		clone[index] = source[index]
		if source[index].Attributes != nil {
			clone[index].Attributes = make(map[string]string, len(source[index].Attributes))
			for key, value := range source[index].Attributes {
				clone[index].Attributes[key] = value
			}
		}
	}
	return clone
}

func (state *navigationState) isCurrentNavigation(request NavigationRequest) bool {
	if state == nil || request.Generation == 0 {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return request == state.current
}

func navigationKind(intent command.IntentKind, sessionID string) (NavigationKind, error) {
	switch intent {
	case command.IntentNewConversation:
		if sessionID != "" {
			return "", errNavigationSessionIDUnexpected
		}
		return NavigationNewConversation, nil
	case command.IntentShowSessions:
		if sessionID != "" {
			return "", errNavigationSessionIDUnexpected
		}
		return NavigationShowSessions, nil
	case command.IntentOpenConversation:
		if sessionID == "" {
			return "", errNavigationSessionIDRequired
		}
		return NavigationOpenConversation, nil
	default:
		return "", errNavigationIntentInvalid
	}
}

func (state *navigationState) recordStaleNavigation() {
	state.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     staleNavigationDiagnosticCode,
		Source:   staleNavigationDiagnosticSource,
		Hint:     staleNavigationDiagnosticHint,
		Severity: diagnostics.SeverityWarning,
	})
}

func (state *navigationState) navigationFailure(code string, message string, cause error) *diagnostics.SafeError {
	state.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     code,
		Source:   staleNavigationDiagnosticSource,
		Hint:     navigationRetryHint,
		Severity: diagnostics.SeverityWarning,
		Err:      cause,
	})
	return &diagnostics.SafeError{
		Code:        code,
		Source:      staleNavigationDiagnosticSource,
		Message:     state.redactor.Redact(message),
		Recoverable: true,
	}
}
