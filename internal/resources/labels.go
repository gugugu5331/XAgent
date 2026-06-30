package resources

type PromptProvider interface {
	SystemPrompt() string
	UILabel(key string) string
}
