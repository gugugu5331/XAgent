package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadIgnoresMissingUserConfig(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	projectConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(projectConfig, []byte(validConfig("")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(projectConfig); err != nil {
		t.Fatalf("missing user config should be ignored, got %v", err)
	}
}

func TestLoadMergesUserAndProjectMCPServers(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	if err := os.MkdirAll(filepath.Join(configHome, "xagent"), 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := filepath.Join(configHome, "xagent", "config.yaml")
	if err := os.WriteFile(userConfig, []byte(validConfig(`
mcp:
  default_timeout_ms: 1000
  servers:
    user_only:
      type: stdio
      command: node
    shared:
      type: stdio
      command: user-command
`)), 0o600); err != nil {
		t.Fatal(err)
	}

	projectConfig := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(projectConfig, []byte(validConfig(`
mcp:
  default_timeout_ms: 2000
  servers:
    shared:
      disabled: true
    project_only:
      type: http
      url: https://example.invalid/mcp
`)), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(projectConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCP.DefaultTimeoutMS != 2000 {
		t.Fatalf("expected project timeout override, got %d", cfg.MCP.DefaultTimeoutMS)
	}
	if _, ok := cfg.MCP.Servers["user_only"]; !ok {
		t.Fatalf("expected user-only server to be preserved: %#v", cfg.MCP.Servers)
	}
	shared := cfg.MCP.Servers["shared"]
	if !shared.Disabled || shared.Command != "" || shared.Source != "project" {
		t.Fatalf("expected project server to wholly override user server, got %#v", shared)
	}
	if _, ok := cfg.MCP.Servers["project_only"]; !ok {
		t.Fatalf("expected project-only server to be present: %#v", cfg.MCP.Servers)
	}
}

func TestLoadRejectsUnknownMCPFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(validConfig(`
mcp:
  typo: nope
`)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadSingle(path)
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestLoadRejectsUnknownTopLevelAndServerFields(t *testing.T) {
	for name, body := range map[string]string{
		"top-level": `
unknown: nope
`,
		"server": `
mcp:
  servers:
    bad:
      type: http
      url: https://example.invalid/mcp
      typo: nope
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(validConfig(body)), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadSingle(path)
			if err == nil {
				t.Fatal("expected unknown field error")
			}
		})
	}
}

func TestValidateMCPSkipsInvalidServerAndRedactsDiagnostics(t *testing.T) {
	t.Setenv("MCP_TOKEN", "canary-secret")
	cfg := baseConfig()
	cfg.MCP.Servers = map[string]MCPServerConfig{
		"good": {
			Type: MCPTransportHTTP,
			URL:  "https://example.invalid/mcp",
			Headers: map[string]string{
				"Authorization": "Bearer ${MCP_TOKEN}",
			},
		},
		"bad": {
			Type: MCPTransportHTTP,
			URL:  "https://example.invalid/mcp",
			Headers: map[string]string{
				"Authorization": "Bearer ${MISSING_MCP_TOKEN}",
			},
		},
	}
	if err := Validate(&cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.MCP.Servers["good"]; !ok {
		t.Fatalf("expected valid server to remain: %#v", cfg.MCP.Servers)
	}
	if _, ok := cfg.MCP.Servers["bad"]; ok {
		t.Fatalf("expected invalid server to be skipped: %#v", cfg.MCP.Servers)
	}
	if len(cfg.MCP.Diagnostics) != 1 {
		t.Fatalf("expected one diagnostic, got %#v", cfg.MCP.Diagnostics)
	}
	diagnostic := cfg.MCP.Diagnostics[0].Message
	if strings.Contains(diagnostic, "canary-secret") || strings.Contains(diagnostic, "Bearer") {
		t.Fatalf("diagnostic leaked sensitive value: %q", diagnostic)
	}
	if !strings.Contains(diagnostic, "Authorization") {
		t.Fatalf("diagnostic should include header key only, got %q", diagnostic)
	}
}

func TestExpandConfigValueSupportsLiteralEnvSyntax(t *testing.T) {
	expanded, err := expandConfigValue(`prefix-$${TOKEN}-suffix`)
	if err != nil {
		t.Fatal(err)
	}
	if expanded != `prefix-${TOKEN}-suffix` {
		t.Fatalf("unexpected literal expansion: %q", expanded)
	}
}

func validConfig(extra string) string {
	return `llm:
  protocol: anthropic
  model: test-model
  base_url: https://example.invalid
  api_key: test-key
ui:
  start_mode: list
` + extra
}

func baseConfig() AppConfig {
	return AppConfig{
		LLM: LLMConfig{
			Protocol: ProtocolAnthropic,
			Model:    "test-model",
			BaseURL:  "https://example.invalid",
			APIKey:   "test-key",
		},
		UI: UIConfig{StartMode: StartModeList},
	}
}
