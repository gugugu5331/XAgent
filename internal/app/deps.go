package app

import (
	"context"

	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/mcpclient"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type Deps struct {
	Config              *config.AppConfig
	Provider            provider.Provider
	Store               conversation.ConversationStore
	Resources           resources.PromptProvider
	Registry            *tool.Registry
	Executor            *tool.Executor
	ContextManager      *contextmgr.Manager
	SessionContext      *sessionctx.Manager
	Memory              *memory.Manager
	Diagnostics         *diagnostics.Collector
	SkillManager        *skill.Manager
	Redact              func(string) string
	RedactionLookbehind int
	Hooks               hook.Runtime
	Closer              interface{ Close(context.Context) error }
	MCPStatus           interface {
		StatusLine() string
		Summary() mcpclient.StatusSummary
		Diagnostics() []mcpclient.Diagnostic
	}
	CommandRegistry *command.Registry
}
