package app

import (
	"errors"
	"math"
	"strings"
	"sync"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

type EventType = events.Type

type Event = events.Event

const (
	EventUserSubmitted           = events.UserSubmitted
	EventTextDelta               = events.TextDelta
	EventThinkingDelta           = events.ThinkingDelta
	EventToolPending             = events.ToolPending
	EventToolWaitingConfirmation = events.ToolWaitingConfirmation
	EventToolRunning             = events.ToolRunning
	EventToolSuccess             = events.ToolSuccess
	EventToolError               = events.ToolError
	EventToolDenied              = events.ToolDenied
	EventAgentProgress           = events.AgentProgressed
	EventUsageUpdated            = events.UsageUpdated
	EventMainTraceReset          = events.MainTraceReset
	EventDone                    = events.Done
	EventError                   = events.Error
)

const (
	staleRequestEventDiagnosticCode   = "stale_request_event"
	staleRequestEventDiagnosticSource = "app.event_boundary"
	eventDiagnosticMaxItems           = int64(100)
	eventDiagnosticMaxItemBytes       = int64(2 << 10)
	eventDiagnosticMaxTotalBytes      = int64(2 << 20)
)

var (
	errEventBoundaryGenerationExhausted = errors.New("app: event boundary generation exhausted")
	errEventBoundaryConversationMissing = errors.New("app: event boundary conversation is missing")
)

// eventEnvelope binds one safe Event stream to the App request and
// conversation that created it. It contains identity only, never a service,
// channel, domain object, error, or event payload.
type eventEnvelope struct {
	Generation     uint64
	ConversationID string
}

// sealedEventEnvelope is the immutable App publication candidate for one
// safe Event. The identity is captured from the three-layer state and every
// pointer-bearing DTO is defensively copied before asynchronous delivery.
type sealedEventEnvelope struct {
	identity eventEnvelope
	value    Event
}

func sealStateEvent(request RequestState, conversation ConversationState, event Event) (*sealedEventEnvelope, bool) {
	conversationID := strings.TrimSpace(conversation.ActiveID)
	if request.Generation == 0 || conversationID == "" || conversation.SkillGeneration != request.Generation {
		return nil, false
	}
	return &sealedEventEnvelope{
		identity: eventEnvelope{Generation: request.Generation, ConversationID: conversationID},
		value:    cloneAppEvent(event),
	}, true
}

func (candidate *sealedEventEnvelope) snapshot() (eventEnvelope, Event, bool) {
	if candidate == nil {
		return eventEnvelope{}, Event{}, false
	}
	return candidate.identity, cloneAppEvent(candidate.value), true
}

// eventBoundaryState owns the bounded stale-event diagnostic boundary. The
// legacy fields keep the current Bubble Tea stream adapter coherent until the
// three-layer Model is published; applyStateEvent never reads them and treats
// RuntimeState/ConversationState/RequestState as the only authoritative state.
type eventBoundaryState struct {
	mu             sync.Mutex
	legacySequence uint64
	legacyCurrent  eventEnvelope
	diagnostics    diagnostics.BoundedSink
}

func newEventBoundaryState(sink diagnostics.BoundedSink) *eventBoundaryState {
	if sink == nil {
		panic("app: nil event boundary diagnostics sink")
	}
	return &eventBoundaryState{diagnostics: sink}
}

func newDefaultEventBoundaryState(redactor *redact.RuntimeRedactor) *eventBoundaryState {
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      eventDiagnosticMaxItems,
		MaxItemBytes:  eventDiagnosticMaxItemBytes,
		MaxTotalBytes: eventDiagnosticMaxTotalBytes,
	})
	if err != nil {
		panic(err)
	}
	return newEventBoundaryState(sink)
}

// newRuntimeEventBoundaryState reuses the App runtime's process-owned C7 sink
// whenever one was injected by Assembly. Compatibility callers that predate
// RuntimeOptions still receive the historical bounded local fallback.
func newRuntimeEventBoundaryState(options *RuntimeOptions, redactor *redact.RuntimeRedactor) *eventBoundaryState {
	if options != nil && options.Diagnostics != nil {
		return newEventBoundaryState(options.Diagnostics)
	}
	boundary := newDefaultEventBoundaryState(redactor)
	if options != nil {
		options.Diagnostics = boundary.diagnostics
	}
	return boundary
}

func (state *eventBoundaryState) begin(conversationID string) (eventEnvelope, error) {
	if state == nil {
		return eventEnvelope{}, errors.New("app: nil event boundary state")
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return eventEnvelope{}, errEventBoundaryConversationMissing
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.legacySequence == math.MaxUint64 {
		return eventEnvelope{}, errEventBoundaryGenerationExhausted
	}
	next := eventEnvelope{Generation: state.legacySequence + 1, ConversationID: conversationID}
	state.legacySequence = next.Generation
	state.legacyCurrent = next
	return next, nil
}

func (state *eventBoundaryState) accepts(candidate eventEnvelope) bool {
	if state == nil {
		return false
	}
	state.mu.Lock()
	accepted := candidate.Generation != 0 && candidate == state.legacyCurrent
	state.mu.Unlock()
	if !accepted {
		state.recordStale()
	}
	return accepted
}

func (state *eventBoundaryState) finish(candidate eventEnvelope) bool {
	if state == nil {
		return false
	}
	state.mu.Lock()
	accepted := candidate.Generation != 0 && candidate == state.legacyCurrent
	if accepted {
		state.legacyCurrent = eventEnvelope{}
	}
	state.mu.Unlock()
	if !accepted {
		state.recordStale()
	}
	return accepted
}

// applyStateEvent is the only T4.11 publication point into RuntimeState,
// ConversationState and RequestState. It validates the envelope against the
// current three-layer boundary before touching any field; a rejected event is
// reduced to one content-free bounded diagnostic.
func (state *eventBoundaryState) applyStateEvent(
	candidate *sealedEventEnvelope,
	runtime *RuntimeState,
	conversation *ConversationState,
	request *RequestState,
) bool {
	identity, event, ok := candidate.snapshot()
	if !ok || runtime == nil || conversation == nil || request == nil ||
		identity.Generation == 0 || strings.TrimSpace(identity.ConversationID) == "" ||
		runtime.RequestSequence != request.Generation || request.Generation != identity.Generation ||
		conversation.SkillGeneration != request.Generation || conversation.ActiveID != identity.ConversationID {
		state.recordStale()
		return false
	}

	if event.Transient && strings.TrimSpace(event.IndependentID) != "" {
		request.TransientIDs = appendUniqueEventID(request.TransientIDs, event.IndependentID)
	}
	switch event.Type {
	case EventUserSubmitted, EventTextDelta, EventThinkingDelta:
		if !event.Transient {
			conversation.Messages = append(conversation.Messages, event.Text)
		}
	case EventToolWaitingConfirmation:
		if event.Confirmation != nil {
			request.Confirmation = &ConfirmationState{
				CallID:         event.Confirmation.CallID,
				Name:           event.Confirmation.Name,
				Prompt:         event.Confirmation.Prompt,
				Target:         event.Confirmation.Target,
				Risk:           event.Confirmation.Risk,
				PermissionMode: event.Confirmation.PermissionMode,
				ScopePreview:   event.Confirmation.ScopePreview,
				RuleLocation:   event.Confirmation.RuleLocation,
				Scopes:         confirmationScopeStates(event.Confirmation.Scopes),
				Warning:        event.Confirmation.Warning,
				RevokeHint:     event.Confirmation.RevokeHint,
				AllowPermanent: event.Confirmation.AllowPermanent,
			}
		}
	case EventAgentProgress:
		if event.Progress != nil {
			request.StopReason = event.Progress.StopReason
		}
	case EventUsageUpdated:
		if event.Usage != nil {
			request.Tokens = Usage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens}
			request.Cache = CacheUsage{
				CacheCreationInputTokens: event.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     event.Usage.CacheReadInputTokens,
			}
		}
	case EventDone:
		request.Duration = event.Duration
	case EventError:
		request.LastError = cloneAppSafeError(event.Err)
	}
	return true
}

func appendUniqueEventID(source []string, value string) []string {
	for _, existing := range source {
		if existing == value {
			return source
		}
	}
	return append(append([]string(nil), source...), value)
}

func cloneAppEvent(source Event) Event {
	clone := source
	clone.Err = cloneAppSafeError(source.Err)
	if source.Tool != nil {
		toolClone := *source.Tool
		if source.Tool.Artifact != nil {
			artifactClone := *source.Tool.Artifact
			toolClone.Artifact = &artifactClone
		}
		clone.Tool = &toolClone
	}
	if source.Confirmation != nil {
		confirmationClone := *source.Confirmation
		confirmationClone.Scopes = cloneConfirmationScopeDisplays(source.Confirmation.Scopes)
		clone.Confirmation = &confirmationClone
	}
	if source.Diagnostic != nil {
		diagnosticClone := *source.Diagnostic
		clone.Diagnostic = &diagnosticClone
	}
	if source.Progress != nil {
		progressClone := *source.Progress
		clone.Progress = &progressClone
	}
	if source.Usage != nil {
		usageClone := *source.Usage
		clone.Usage = &usageClone
	}
	return clone
}

func cloneConfirmationScopeDisplays(source []events.ConfirmationScopeDisplay) []events.ConfirmationScopeDisplay {
	if source == nil {
		return nil
	}
	clone := make([]events.ConfirmationScopeDisplay, len(source))
	copy(clone, source)
	return clone
}

func confirmationScopeStates(source []events.ConfirmationScopeDisplay) []ConfirmationScopeState {
	if source == nil {
		return nil
	}
	states := make([]ConfirmationScopeState, len(source))
	for index, scope := range source {
		states[index] = ConfirmationScopeState{
			Scope: scope.Scope, Available: scope.Available, Description: scope.Description,
		}
	}
	return states
}

func cloneAppSafeError(source *diagnostics.SafeError) *diagnostics.SafeError {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func (state *eventBoundaryState) recordStale() {
	if state == nil || state.diagnostics == nil {
		return
	}
	state.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     staleRequestEventDiagnosticCode,
		Source:   staleRequestEventDiagnosticSource,
		Severity: diagnostics.SeverityWarning,
	})
}

func (m *Model) acceptsEventEnvelope(candidate eventEnvelope) bool {
	if m == nil || m.eventBoundary == nil {
		return false
	}
	if !m.matchesActiveEventEnvelope(candidate) {
		m.eventBoundary.recordStale()
		return false
	}
	return m.eventBoundary.accepts(candidate)
}

func (m *Model) finishEventEnvelope(candidate eventEnvelope) bool {
	if m == nil || m.eventBoundary == nil {
		return false
	}
	if !m.matchesActiveEventEnvelope(candidate) {
		m.eventBoundary.recordStale()
		return false
	}
	return m.eventBoundary.finish(candidate)
}

func (m *Model) matchesActiveEventEnvelope(candidate eventEnvelope) bool {
	if candidate.Generation == 0 || strings.TrimSpace(candidate.ConversationID) == "" || m.request == nil || m.conversation == nil {
		return false
	}
	return m.request.Generation == candidate.Generation &&
		m.request.ConversationID == candidate.ConversationID &&
		strings.TrimSpace(m.conversation.ID) == candidate.ConversationID
}
