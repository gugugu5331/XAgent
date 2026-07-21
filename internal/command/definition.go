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

type Definition struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
	Type        Type
	ArgHint     string
	Hidden      bool
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
