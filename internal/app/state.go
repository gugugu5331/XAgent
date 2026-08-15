package app

import (
	"math"
	"time"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

// Usage is the token portion of a request usage snapshot. Cache counters are
// kept separately so request reset and rendering cannot conflate the values.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// CacheUsage is the cache portion of a request usage snapshot.
type CacheUsage struct {
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// ConfirmationState contains only redacted display data and stable metadata.
// The decision channel remains outside App state as an execution capability.
type ConfirmationState struct {
	CallID         string
	Name           string
	Prompt         redact.SafeText
	Target         redact.SafeText
	Risk           string
	PermissionMode string
	ScopePreview   redact.SafeText
	RuleLocation   redact.SafeText
	Scopes         []ConfirmationScopeState
	Warning        redact.SafeText
	RevokeHint     redact.SafeText
	AllowPermanent bool
}

// ConfirmationScopeState is one capability-free authorization choice copied
// into request state. Descriptions have already crossed the redaction boundary.
type ConfirmationScopeState struct {
	Scope       string
	Available   bool
	Description redact.SafeText
}

// RuntimeState survives request and conversation resets.
type RuntimeState struct {
	RequestSequence   uint64
	ShowResponseTimer bool
	StartMode         string
}

// runtimeStateFromResolvedUI copies final UI configuration without inferring
// presence, re-applying defaults, or maintaining a second validator in App.
func runtimeStateFromResolvedUI(ui config.UIConfig) RuntimeState {
	return RuntimeState{
		ShowResponseTimer: ui.ShowResponseTimer,
		StartMode:         ui.StartMode,
	}
}

// ConversationState contains state whose lifetime is the active conversation.
type ConversationState struct {
	ActiveID        string
	Mode            string
	Skills          []string
	SkillGeneration uint64
	Input           redact.SafeText
	Messages        []redact.SafeText
	Notice          redact.SafeText
}

// RequestState contains state that must not cross a request boundary.
type RequestState struct {
	Generation   uint64
	Duration     time.Duration
	Tokens       Usage
	Cache        CacheUsage
	StopReason   string
	LastError    *diagnostics.SafeError
	Confirmation *ConfirmationState
	TransientIDs []string
}

// newRequestState allocates the next process-unique request generation. Reset
// operations replace RequestState but must retain the owning RuntimeState.
func (state *RuntimeState) newRequestState() RequestState {
	if state == nil {
		panic("app: nil runtime state")
	}
	if state.RequestSequence == math.MaxUint64 {
		panic("app: request sequence exhausted")
	}
	state.RequestSequence++
	return RequestState{Generation: state.RequestSequence}
}

// resetRequest drops all request-scoped values while preserving the active
// conversation. Preserved Skill Activity is rebound to the new generation so
// a late snapshot from the replaced request cannot overwrite it.
func (state *RuntimeState) resetRequest(conversation *ConversationState, request *RequestState) {
	if conversation == nil {
		panic("app: nil conversation state")
	}
	if request == nil {
		panic("app: nil request state")
	}
	next := state.newRequestState()
	conversation.SkillGeneration = next.Generation
	*request = next
}

type skillActivityClearer interface {
	Clear()
}

// resetConversation starts a new request, explicitly clears the previous
// Skill Activity, and clears every value owned by the old conversation.
// RuntimeState remains unchanged apart from sequence advance. Allocation
// happens first so exhaustion cannot partially clear live state.
func (state *RuntimeState) resetConversation(conversation *ConversationState, request *RequestState, activeID string, activity skillActivityClearer) {
	if conversation == nil {
		panic("app: nil conversation state")
	}
	if request == nil {
		panic("app: nil request state")
	}
	if activity == nil {
		panic("app: nil Skill Activity")
	}
	next := state.newRequestState()
	activity.Clear()
	*conversation = ConversationState{
		ActiveID:        activeID,
		SkillGeneration: next.Generation,
	}
	*request = next
}

// applySkillSnapshot publishes Skill Activity only for the active request.
// Copying the names prevents later caller mutation from changing App state.
func (state *ConversationState) applySkillSnapshot(generation uint64, skills []string) bool {
	if state == nil || generation == 0 || generation != state.SkillGeneration {
		return false
	}
	state.Skills = append([]string(nil), skills...)
	return true
}
