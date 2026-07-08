package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/redact"
)

func TestConfigSchemaCompatibility(t *testing.T) {
	cfg := loadTestConfig(t, "")
	if cfg.LLM.RequestTimeoutMS != DefaultLLMRequestTimeoutMS {
		t.Fatalf("request timeout = %d", cfg.LLM.RequestTimeoutMS)
	}
	if cfg.Agent.MaxIterations != DefaultAgentMaxIterations || cfg.Agent.MaxUnknownToolCalls != DefaultAgentMaxUnknownToolCalls {
		t.Fatalf("agent defaults = %#v", cfg.Agent)
	}
	if cfg.Tool.TimeoutMS != DefaultToolTimeoutMS || cfg.Tool.MaxOutputBytes != DefaultToolMaxOutputBytes {
		t.Fatalf("tool defaults = %#v", cfg.Tool)
	}
	if !Enabled(cfg.Instructions.Enabled, false) || !Enabled(cfg.Context.Enabled, false) || !Enabled(cfg.Memory.Enabled, false) {
		t.Fatalf("enabled defaults not true: instructions=%v context=%v memory=%v", cfg.Instructions.Enabled, cfg.Context.Enabled, cfg.Memory.Enabled)
	}
}

func TestLoadPreservesExplicitEnabledValues(t *testing.T) {
	cfg := loadTestConfig(t, `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: test-key
instructions:
  enabled: false
context:
  enabled: false
memory:
  enabled: false
`)
	if Enabled(cfg.Instructions.Enabled, true) || Enabled(cfg.Context.Enabled, true) || Enabled(cfg.Memory.Enabled, true) {
		t.Fatalf("explicit false was overridden: instructions=%v context=%v memory=%v", cfg.Instructions.Enabled, cfg.Context.Enabled, cfg.Memory.Enabled)
	}
}

func TestLLMAPIKeyEnvExpansion(t *testing.T) {
	t.Setenv("XAGENT_TEST_API_KEY", "runtime-secret")
	runtimeRedactor := redact.NewRuntimeRedactor()
	cfg := loadTestConfigWithOptions(t, `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: ${XAGENT_TEST_API_KEY}
`, LoadOptions{Redactor: runtimeRedactor})
	if cfg.LLM.APIKey != "runtime-secret" {
		t.Fatalf("api key = %q", cfg.LLM.APIKey)
	}
	if strings.Contains(runtimeRedactor.Text("runtime-secret"), "runtime-secret") {
		t.Fatalf("runtime secret was not registered")
	}
}

func TestConfigErrorsIncludeActionableHintsAndRedactSecrets(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !strings.Contains(err.Error(), "config.example.yaml") {
		t.Fatalf("missing config error lacks hint: %v", err)
	}

	_, err = Load(writeConfig(t, `
llm:
  protocol: anthropic
  base_url: http://127.0.0.1:1
  api_key: test-key
`))
	if err == nil || !strings.Contains(err.Error(), "llm.model") || !strings.Contains(err.Error(), "模型") {
		t.Fatalf("missing model error lacks field/hint: %v", err)
	}

	_, err = Load(writeConfig(t, `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: ${XAGENT_MISSING_API_KEY}
`))
	if err == nil || !strings.Contains(err.Error(), "XAGENT_MISSING_API_KEY") {
		t.Fatalf("missing env error lacks variable name: %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("missing env error leaked secret-like value: %v", err)
	}
}

func TestLLMRequestTimeoutDefaultsAndValidation(t *testing.T) {
	cfg := loadTestConfig(t, `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: test-key
  request_timeout_ms: 1234
`)
	if cfg.LLM.RequestTimeoutMS != 1234 {
		t.Fatalf("request timeout = %d", cfg.LLM.RequestTimeoutMS)
	}

	_, err := Load(writeConfig(t, `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: test-key
  request_timeout_ms: -1
`))
	if err == nil || !strings.Contains(err.Error(), "llm.request_timeout_ms") {
		t.Fatalf("expected request timeout validation error, got %v", err)
	}
}

func TestAgentConfigDefaultsAndValidation(t *testing.T) {
	cfg := loadTestConfig(t, `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: test-key
agent:
  max_iterations: 3
  max_unknown_tool_calls: 4
`)
	if cfg.Agent.MaxIterations != 3 || cfg.Agent.MaxUnknownToolCalls != 4 {
		t.Fatalf("agent config = %#v", cfg.Agent)
	}
}

func TestToolConfigDefaultsAndValidation(t *testing.T) {
	cfg := loadTestConfig(t, `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: test-key
tool:
  timeout_ms: 11
  max_output_bytes: 22
`)
	if cfg.Tool.TimeoutMS != 11 || cfg.Tool.MaxOutputBytes != 22 {
		t.Fatalf("tool config = %#v", cfg.Tool)
	}
}

func loadTestConfig(t *testing.T, override string) *AppConfig {
	t.Helper()
	return loadTestConfigWithOptions(t, override, LoadOptions{})
}

func loadTestConfigWithOptions(t *testing.T, override string, options LoadOptions) *AppConfig {
	t.Helper()
	cfg, err := LoadWithOptions(writeConfig(t, override), options)
	if err != nil {
		t.Fatalf("LoadWithOptions: %v", err)
	}
	return cfg
}

func writeConfig(t *testing.T, override string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := override
	if strings.TrimSpace(content) == "" {
		content = `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: test-key
`
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
