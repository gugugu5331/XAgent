package command

type Type string

const (
	TypeLocal  Type = "local"
	TypeUI     Type = "ui"
	TypePrompt Type = "prompt"
)

type Mode string

const (
	ModeDefault Mode = "default"
	ModePlan    Mode = "plan"
)

// HelpEntryKind identifies structured metadata consumed by the unified help
// view. Compatibility commands remain dispatchable but carry no public entry.
type HelpEntryKind string

const (
	HelpEntryPermissionMode HelpEntryKind = "permission_mode"
	HelpEntryStatus         HelpEntryKind = "status"
	HelpEntryDiagnostics    HelpEntryKind = "diagnostics"
)

// HelpEntry keeps permission modes and the public status/diagnostics entry in
// the same command definition that owns their discoverable command.
type HelpEntry struct {
	Kind        HelpEntryKind
	Name        string
	Description string
}

// HelpMetadata is the normalized immutable projection exposed by Registry.
type HelpMetadata struct {
	Kind          HelpEntryKind
	Name          string
	Description   string
	CanonicalName string
}

// IntentKind is a capability-free App action produced by command and key
// metadata. Navigation execution and generation checks belong to App.
type IntentKind string

const (
	IntentNewConversation  IntentKind = "new_conversation"
	IntentShowSessions     IntentKind = "show_sessions"
	IntentOpenConversation IntentKind = "open_conversation"
	IntentQuit             IntentKind = "quit"
	IntentCancel           IntentKind = "cancel"
)

type ShortcutContext string

const (
	ShortcutChatIdle         ShortcutContext = "chat_idle"
	ShortcutChatStreaming    ShortcutContext = "chat_streaming"
	ShortcutChatConfirmation ShortcutContext = "chat_confirmation"
	ShortcutSessions         ShortcutContext = "sessions"
)

// Shortcut is contextual input metadata. An empty Intent inherits the owning
// Definition's Intent; explicit values support list-only actions such as
// opening the selected session or quitting.
type Shortcut struct {
	Context     ShortcutContext
	Key         string
	Intent      IntentKind
	Description string
}

// Binding is the immutable normalized projection returned by Registry.
type Binding struct {
	Context       ShortcutContext
	Key           string
	Intent        IntentKind
	CanonicalName string
	Description   string
}

type Definition struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
	Type        Type
	ArgHint     string
	Badge       string
	Hidden      bool
	Intent      IntentKind
	Shortcuts   []Shortcut
	HelpEntries []HelpEntry
	Handler     Handler
}

type Invocation struct {
	CanonicalName string
	MatchedName   string
	Args          string
	Raw           string
}

type ExecutionContext struct {
	Registry   *Registry
	Controller Controller
}

type Handler func(context ExecutionContext, invocation Invocation) error

// IntentSink is the only behavior a navigation command may request from App.
// It does not expose Store, Orchestrator, TUI, or any other service.
type IntentSink interface {
	HandleIntent(IntentKind) error
}

type DispatchKind string

const (
	DispatchEmpty     DispatchKind = "empty"
	DispatchPlainText DispatchKind = "plain_text"
	DispatchExecuted  DispatchKind = "executed"
	DispatchUnknown   DispatchKind = "unknown"
)

type DispatchResult struct {
	Kind       DispatchKind
	Text       string
	Invocation Invocation
	Err        error
}

type Suggestion struct {
	Name        string
	Aliases     []string
	Description string
	ArgHint     string
	Badge       string
}

type SessionStatus struct {
	ID           string
	MessageCount int
	Mode         Mode
	Streaming    bool
}

type MemoryScopeStatus struct {
	Enabled bool
	Count   int
}

type MemoryStatus struct {
	User        MemoryScopeStatus
	Project     MemoryScopeStatus
	Diagnostics []string
}

type PermissionStatus struct {
	Mode         string
	SessionRules int
	LocalRules   int
	ProjectRules int
	UserRules    int
	LoadErrors   int
}

type TokenUsage struct {
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
}

type RuntimeStatus struct {
	Provider    string
	Model       string
	Mode        Mode
	Streaming   bool
	MCP         string
	RecentError string
}
