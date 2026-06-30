package config

type AppConfig struct {
	LLM     LLMConfig     `yaml:"llm"`
	UI      UIConfig      `yaml:"ui"`
	Storage StorageConfig `yaml:"storage"`
}

type LLMConfig struct {
	Protocol string         `yaml:"protocol"`
	Model    string         `yaml:"model"`
	BaseURL  string         `yaml:"base_url"`
	APIKey   string         `yaml:"api_key"`
	Thinking ThinkingConfig `yaml:"thinking"`
}

type ThinkingConfig struct {
	Enabled      bool `yaml:"enabled"`
	Show         bool `yaml:"show"`
	BudgetTokens int  `yaml:"budget_tokens"`
}

type UIConfig struct {
	ShowResponseTimer bool   `yaml:"show_response_timer"`
	StartMode         string `yaml:"start_mode"`
}

type StorageConfig struct {
	DataDir string `yaml:"data_dir"`
}
