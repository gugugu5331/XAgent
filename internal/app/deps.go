package app

import (
	"context"
	"io"

	"xagent/internal/artifact"
	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/mcpclient"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

// ConversationAccess is the App-owned session capability. Maintenance and
// filesystem ownership remain with the process-level composition root.
type ConversationAccess interface {
	Create(context.Context) (*conversation.Conversation, error)
	List(context.Context) (conversation.ListResult, error)
	Load(context.Context, string) (conversation.LoadResult, error)
	Save(context.Context, *conversation.Conversation) (conversation.SaveResult, error)
}

// Orchestration is the subset of request coordination used by App intents.
// Provider, tool, MCP and memory services stay behind this boundary.
type Orchestration interface {
	WaitIdle(context.Context) error
	BuildExecutionProfile(orchestrator.RunMode, *skill.Activity, int) (skill.ExecutionProfile, error)
	SendRequest(context.Context, *conversation.Conversation, orchestrator.RunRequest) (<-chan events.Event, error)
	SendSkill(context.Context, *conversation.Conversation, skill.Invocation, *skill.Activity, orchestrator.RunMode) (<-chan events.Event, skill.PreparedInvocation, error)
	ResolveToolConfirmation(events.ToolConfirmationDecision) bool
	CompactContext(context.Context, *conversation.Conversation) (contextmgr.Result, error)
	PermissionStatus() orchestrator.PermissionStatus
}

// ArtifactUserReader permits only an explicit user-directed read. App cannot
// create, clean up, enumerate or discover the backing artifact path.
type ArtifactUserReader interface {
	OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error)
}

// HookLifecycle contains only process/session lifecycle notifications. Turn,
// message, tool and prompt capabilities remain owned by Orchestration.
type HookLifecycle interface {
	SystemStart(context.Context)
	SessionStart(context.Context, string, hook.SessionState)
	SessionEnd(context.Context, string, hook.SessionEndReason)
}

// AppServices is the capability boundary consumed by the future state-driven
// App controller. It deliberately contains no global container or concrete
// Config, Provider, MCP, artifact Store, registry, executor, or manager.
// Legacy Deps remains below during the staged App migration.
type AppServices struct {
	Conversations ConversationAccess
	Orchestrator  Orchestration
	Artifacts     ArtifactUserReader
	Hooks         HookLifecycle
}

type Deps struct {
	// RuntimeOptions is copied into the App lifecycle at construction. The
	// explicit NewWithOptions constructor is preferred by composition roots;
	// this field keeps staged/legacy callers source-compatible while the App
	// graph is migrated.
	RuntimeOptions RuntimeOptions
	// ExistingOrchestrator lets the sole composition root hand the already
	// constructed orchestrator to App. When set, NewWithOptions reuses it
	// instead of creating a second orchestration graph.
	ExistingOrchestrator *orchestrator.Orchestrator
	Config               *config.AppConfig
	Provider             provider.Provider
	Store                conversation.Store
	Artifacts            ArtifactUserReader
	Resources            resources.PromptProvider
	Registry             *tool.Registry
	Executor             *tool.Executor
	ContextManager       *contextmgr.Manager
	SkillHistoryPolicy   orchestrator.SkillHistoryPolicy
	RequestBudgeter      contextmgr.RequestBudgeter
	SessionContext       *sessionctx.Manager
	Memory               *memory.Manager
	Diagnostics          *diagnostics.Collector
	SkillManager         *skill.Manager
	RuntimeRedactor      *redact.RuntimeRedactor
	Redact               func(string) string
	RedactionLookbehind  int
	Hooks                hook.Runtime
	Closer               interface{ Close(context.Context) error }
	MCPStatus            interface {
		StatusLine() string
		Summary() mcpclient.StatusSummary
		Diagnostics() []mcpclient.Diagnostic
	}
	CommandRegistry *command.Registry
}
