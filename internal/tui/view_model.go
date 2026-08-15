package tui

import (
	"time"

	"xagent/internal/redact"
)

// Screen is display metadata, not an App navigation capability.
type Screen string

const (
	ScreenList       Screen = "list"
	ScreenChat       Screen = "chat"
	ScreenTasks      Screen = "tasks"
	ScreenTaskDetail Screen = "task_detail"
)

// ViewModelSpec is the capability-free construction input owned by App. The
// resulting ViewModel copies every slice and optional value before retaining
// it; callers may discard or mutate the Spec immediately after construction.
type ViewModelSpec struct {
	Generation      uint64
	RuntimeSequence uint64
	Screen          Screen
	Lines           []redact.SafeText
	Conversation    ConversationViewSpec
	Request         RequestViewSpec
	Sessions        SessionListViewSpec
	Tasks           TaskListViewSpec
	TaskDetail      TaskDetailViewSpec
}

type ConversationViewSpec struct {
	ActiveID string
	Mode     string
	Skills   []string
	Input    redact.SafeText
	Messages []redact.SafeText
	Notice   redact.SafeText
}

type RequestViewSpec struct {
	Duration     time.Duration
	InputTokens  int64
	OutputTokens int64
	CacheCreated int64
	CacheRead    int64
	StopReason   string
	LastError    SafeErrorViewSpec
	Confirmation ConfirmationViewSpec
	TransientIDs []string
}

type SafeErrorViewSpec struct {
	Present     bool
	Code        string
	Source      string
	Message     redact.SafeText
	Recoverable bool
}

type ConfirmationViewSpec struct {
	Present        bool
	ConfirmationID string
	CallID         string
	Name           string
	Prompt         redact.SafeText
	Target         redact.SafeText
	Risk           string
	PermissionMode string
	ScopePreview   redact.SafeText
	RuleLocation   redact.SafeText
	Scopes         []ConfirmationScopeViewSpec
	Warning        redact.SafeText
	RevokeHint     redact.SafeText
	AllowPermanent bool
}

// ConfirmationScopeViewSpec is App-owned safe display data for one
// authorization choice. Scope is stable metadata; Description has already
// crossed the runtime redaction boundary.
type ConfirmationScopeViewSpec struct {
	Scope       string
	Available   bool
	Description redact.SafeText
}

type SessionListViewSpec struct {
	Entries      []SessionListEntrySpec
	Truncated    bool
	ScannedFiles int
	ScannedBytes int64
	Notice       redact.SafeText
}

type SessionListEntrySpec struct {
	ID                 string
	Title              redact.SafeText
	UpdatedAtUnixMilli int64
	MessageCount       int
	Available          bool
	Selectable         bool
	RecoveryStatus     string
	RecoveryNotice     redact.SafeText
}

// ViewModel is a read-only, capability-free snapshot. It owns all of its
// backing storage and contains neither domain objects nor callable services.
type ViewModel struct {
	generation      uint64
	runtimeSequence uint64
	screen          Screen
	lines           []redact.SafeText
	conversation    ConversationView
	request         RequestView
	sessions        SessionListView
	tasks           TaskListView
	taskDetail      TaskDetailView
}

type ConversationView struct {
	activeID string
	mode     string
	skills   []string
	input    redact.SafeText
	messages []redact.SafeText
	notice   redact.SafeText
}

type RequestView struct {
	duration     time.Duration
	inputTokens  int64
	outputTokens int64
	cacheCreated int64
	cacheRead    int64
	stopReason   string
	lastError    SafeErrorView
	hasError     bool
	confirmation ConfirmationView
	hasConfirm   bool
	transientIDs []string
}

type SafeErrorView struct {
	code        string
	source      string
	message     redact.SafeText
	recoverable bool
}

type ConfirmationView struct {
	confirmationID string
	callID         string
	name           string
	prompt         redact.SafeText
	target         redact.SafeText
	risk           string
	permissionMode string
	scopePreview   redact.SafeText
	ruleLocation   redact.SafeText
	scopes         []ConfirmationScopeView
	warning        redact.SafeText
	revokeHint     redact.SafeText
	allowPermanent bool
}

// ConfirmationScopeView is the immutable TUI projection of one authorization
// scope. It intentionally carries no permission decision capability.
type ConfirmationScopeView struct {
	scope       string
	available   bool
	description redact.SafeText
}

type SessionListView struct {
	entries      []SessionListEntry
	truncated    bool
	scannedFiles int
	scannedBytes int64
	notice       redact.SafeText
}

type SessionListEntry struct {
	id                 string
	title              redact.SafeText
	updatedAtUnixMilli int64
	messageCount       int
	available          bool
	selectable         bool
	recoveryStatus     string
	recoveryNotice     redact.SafeText
}

// NewViewModel preserves the small T4.3 construction surface while delegating
// ownership to the complete state snapshot constructor.
func NewViewModel(generation uint64, screen Screen, lines []redact.SafeText) ViewModel {
	return NewStateViewModel(ViewModelSpec{Generation: generation, RuntimeSequence: generation, Screen: screen, Lines: lines})
}

func NewStateViewModel(spec ViewModelSpec) ViewModel {
	var entries []SessionListEntry
	if spec.Sessions.Entries != nil {
		entries = make([]SessionListEntry, len(spec.Sessions.Entries))
		for index, entry := range spec.Sessions.Entries {
			entries[index] = SessionListEntry{
				id: entry.ID, title: entry.Title, updatedAtUnixMilli: entry.UpdatedAtUnixMilli,
				messageCount: entry.MessageCount, available: entry.Available, selectable: entry.Selectable,
				recoveryStatus: entry.RecoveryStatus, recoveryNotice: entry.RecoveryNotice,
			}
		}
	}
	return ViewModel{
		generation:      spec.Generation,
		runtimeSequence: spec.RuntimeSequence,
		screen:          spec.Screen,
		lines:           cloneSlice(spec.Lines),
		conversation: ConversationView{
			activeID: spec.Conversation.ActiveID, mode: spec.Conversation.Mode,
			skills: cloneSlice(spec.Conversation.Skills), input: spec.Conversation.Input,
			messages: cloneSlice(spec.Conversation.Messages), notice: spec.Conversation.Notice,
		},
		request: RequestView{
			duration: spec.Request.Duration, inputTokens: spec.Request.InputTokens, outputTokens: spec.Request.OutputTokens,
			cacheCreated: spec.Request.CacheCreated, cacheRead: spec.Request.CacheRead, stopReason: spec.Request.StopReason,
			lastError: SafeErrorView{code: spec.Request.LastError.Code, source: spec.Request.LastError.Source,
				message: spec.Request.LastError.Message, recoverable: spec.Request.LastError.Recoverable},
			hasError:     spec.Request.LastError.Present,
			confirmation: projectConfirmationView(spec.Request.Confirmation),
			hasConfirm:   spec.Request.Confirmation.Present,
			transientIDs: cloneSlice(spec.Request.TransientIDs),
		},
		sessions: SessionListView{
			entries: entries, truncated: spec.Sessions.Truncated, scannedFiles: spec.Sessions.ScannedFiles,
			scannedBytes: spec.Sessions.ScannedBytes, notice: spec.Sessions.Notice,
		},
		tasks:      NewTaskListView(spec.Tasks),
		taskDetail: NewTaskDetailView(spec.TaskDetail),
	}
}

func (view ViewModel) Generation() uint64      { return view.generation }
func (view ViewModel) RuntimeSequence() uint64 { return view.runtimeSequence }
func (view ViewModel) Screen() Screen          { return view.screen }
func (view ViewModel) Conversation() ConversationView {
	conversation := view.conversation
	conversation.skills = cloneSlice(view.conversation.skills)
	conversation.messages = cloneSlice(view.conversation.messages)
	return conversation
}
func (view ViewModel) Request() RequestView {
	request := view.request
	request.transientIDs = cloneSlice(view.request.transientIDs)
	request.confirmation.scopes = cloneSlice(view.request.confirmation.scopes)
	return request
}
func (view ViewModel) Sessions() SessionListView {
	sessions := view.sessions
	sessions.entries = cloneSlice(view.sessions.entries)
	return sessions
}
func (view ViewModel) Tasks() TaskListView        { return view.tasks.clone() }
func (view ViewModel) TaskDetail() TaskDetailView { return view.taskDetail.clone() }

// Lines returns a defensive copy so a renderer cannot mutate the snapshot.
func (view ViewModel) Lines() []redact.SafeText {
	return cloneSlice(view.lines)
}

func (view ConversationView) ActiveID() string        { return view.activeID }
func (view ConversationView) Mode() string            { return view.mode }
func (view ConversationView) Input() redact.SafeText  { return view.input }
func (view ConversationView) Notice() redact.SafeText { return view.notice }
func (view ConversationView) Skills() []string        { return cloneSlice(view.skills) }
func (view ConversationView) Messages() []redact.SafeText {
	return cloneSlice(view.messages)
}

func (view RequestView) Duration() time.Duration          { return view.duration }
func (view RequestView) InputTokens() int64               { return view.inputTokens }
func (view RequestView) OutputTokens() int64              { return view.outputTokens }
func (view RequestView) CacheCreated() int64              { return view.cacheCreated }
func (view RequestView) CacheRead() int64                 { return view.cacheRead }
func (view RequestView) StopReason() string               { return view.stopReason }
func (view RequestView) TransientIDs() []string           { return cloneSlice(view.transientIDs) }
func (view RequestView) LastError() (SafeErrorView, bool) { return view.lastError, view.hasError }
func (view RequestView) Confirmation() (ConfirmationView, bool) {
	confirmation := view.confirmation
	confirmation.scopes = cloneSlice(view.confirmation.scopes)
	return confirmation, view.hasConfirm
}

func (view SafeErrorView) Code() string             { return view.code }
func (view SafeErrorView) Source() string           { return view.source }
func (view SafeErrorView) Message() redact.SafeText { return view.message }
func (view SafeErrorView) Recoverable() bool        { return view.recoverable }

func (view ConfirmationView) ConfirmationID() string        { return view.confirmationID }
func (view ConfirmationView) CallID() string                { return view.callID }
func (view ConfirmationView) Name() string                  { return view.name }
func (view ConfirmationView) Prompt() redact.SafeText       { return view.prompt }
func (view ConfirmationView) Target() redact.SafeText       { return view.target }
func (view ConfirmationView) Risk() string                  { return view.risk }
func (view ConfirmationView) PermissionMode() string        { return view.permissionMode }
func (view ConfirmationView) ScopePreview() redact.SafeText { return view.scopePreview }
func (view ConfirmationView) RuleLocation() redact.SafeText { return view.ruleLocation }
func (view ConfirmationView) Scopes() []ConfirmationScopeView {
	return cloneSlice(view.scopes)
}
func (view ConfirmationView) Warning() redact.SafeText    { return view.warning }
func (view ConfirmationView) RevokeHint() redact.SafeText { return view.revokeHint }
func (view ConfirmationView) AllowPermanent() bool        { return view.allowPermanent }

func (view ConfirmationScopeView) Scope() string                { return view.scope }
func (view ConfirmationScopeView) Available() bool              { return view.available }
func (view ConfirmationScopeView) Description() redact.SafeText { return view.description }

func (view SessionListView) Entries() []SessionListEntry {
	return cloneSlice(view.entries)
}
func (view SessionListView) Truncated() bool         { return view.truncated }
func (view SessionListView) ScannedFiles() int       { return view.scannedFiles }
func (view SessionListView) ScannedBytes() int64     { return view.scannedBytes }
func (view SessionListView) Notice() redact.SafeText { return view.notice }

func (entry SessionListEntry) ID() string                      { return entry.id }
func (entry SessionListEntry) Title() redact.SafeText          { return entry.title }
func (entry SessionListEntry) UpdatedAtUnixMilli() int64       { return entry.updatedAtUnixMilli }
func (entry SessionListEntry) MessageCount() int               { return entry.messageCount }
func (entry SessionListEntry) Available() bool                 { return entry.available }
func (entry SessionListEntry) Selectable() bool                { return entry.selectable }
func (entry SessionListEntry) RecoveryStatus() string          { return entry.recoveryStatus }
func (entry SessionListEntry) RecoveryNotice() redact.SafeText { return entry.recoveryNotice }

func cloneSlice[T any](source []T) []T {
	if source == nil {
		return nil
	}
	cloned := make([]T, len(source))
	copy(cloned, source)
	return cloned
}

func projectConfirmationScopes(source []ConfirmationScopeViewSpec) []ConfirmationScopeView {
	if source == nil {
		return nil
	}
	projected := make([]ConfirmationScopeView, len(source))
	for index, scope := range source {
		projected[index] = ConfirmationScopeView{
			scope: scope.Scope, available: scope.Available, description: scope.Description,
		}
	}
	return projected
}

// IntentKind identifies a user action without carrying a domain service or
// decision capability.
type IntentKind string

const (
	IntentSubmit           IntentKind = "submit"
	IntentNewConversation  IntentKind = "new_conversation"
	IntentOpenConversation IntentKind = "open_conversation"
	IntentBack             IntentKind = "back"
	IntentQuit             IntentKind = "quit"
)

// Intent is an immutable user-input value emitted by TUI and interpreted by
// App. Value may contain user-authored text, while targetID is only an opaque
// identity; neither field is rendered without a later safe projection.
type Intent struct {
	kind     IntentKind
	value    string
	targetID string
}

func NewIntent(kind IntentKind, value string, targetID string) Intent {
	return Intent{kind: kind, value: value, targetID: targetID}
}

func (intent Intent) Kind() IntentKind { return intent.kind }
func (intent Intent) Value() string    { return intent.value }
func (intent Intent) TargetID() string { return intent.targetID }
