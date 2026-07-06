package app

import (
	"context"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
	"xagent/internal/tool"
)

type Deps struct {
	Config         *config.AppConfig
	Provider       provider.Provider
	Store          conversation.ConversationStore
	Resources      resources.PromptProvider
	Registry       *tool.Registry
	Executor       *tool.Executor
	ContextManager *contextmgr.Manager
	SessionContext *sessionctx.Manager
	Memory         *memory.Manager
	Closer         interface{ Close(context.Context) error }
	MCPStatus      interface{ StatusLine() string }
}
