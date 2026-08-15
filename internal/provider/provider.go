package provider

import "context"

type Provider interface {
	StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error)
	Name() string
}

// ChatStreamOptionsProvider is an optional lifecycle seam for Providers whose
// concrete ChatStream owns asynchronous resources.  The base Provider
// contract intentionally remains unchanged so compatibility fakes and
// third-party adapters continue to work; Orchestrator uses this interface
// when it is implemented and otherwise falls back to StreamChat.
type ChatStreamOptionsProvider interface {
	StreamChatWithOptions(context.Context, ChatRequest, ChatStreamOptions) (ChatStream, error)
}
