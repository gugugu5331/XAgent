package app

import (
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/tool"
)

type Deps struct {
	Config    *config.AppConfig
	Provider  provider.Provider
	Store     conversation.ConversationStore
	Resources resources.PromptProvider
	Registry  *tool.Registry
	Executor  *tool.Executor
}
