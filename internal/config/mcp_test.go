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

func TestMCPServersMergeByNameWithWholeEntryReplacement(t *testing.T) {
	optionalString := func(value string) Optional[string] { return Optional[string]{Set: true, Value: value} }
	optionalBool := func(value bool) Optional[bool] { return Optional[bool]{Set: true, Value: value} }
	optionalInt := func(value int64) Optional[int64] { return Optional[int64]{Set: true, Value: value} }
	optionalStrings := func(value []string) Optional[[]string] { return Optional[[]string]{Set: true, Value: value} }
	optionalMap := func(value map[string]string) Optional[map[string]string] {
		return Optional[map[string]string]{Set: true, Value: value}
	}

	projectHeaders := map[string]string{"Authorization": "project-token"}
	user := PartialAppConfig{MCP: PartialMCPConfig{
		DefaultTimeoutMS: optionalInt(1_111),
		MaxResponseBytes: optionalInt(2 * mebibyte),
		MaxPages:         optionalInt(10),
		Servers: map[string]PartialMCPServerConfig{
			"shared": {
				Disabled:  optionalBool(true),
				Type:      optionalString(MCPTransportStdio),
				Command:   optionalString("user-command"),
				Args:      optionalStrings([]string{"user-arg"}),
				Env:       optionalMap(map[string]string{"USER_SECRET": "user-secret"}),
				TimeoutMS: optionalInt(123),
			},
			"user-only": {Disabled: optionalBool(true)},
		},
	}}
	project := PartialAppConfig{MCP: PartialMCPConfig{
		MaxResponseBytes: optionalInt(3 * mebibyte),
		MaxTools:         optionalInt(200),
		Servers: map[string]PartialMCPServerConfig{
			"shared": {
				Disabled:  optionalBool(false),
				Type:      optionalString(MCPTransportHTTP),
				URL:       optionalString("https://project.invalid/mcp"),
				Env:       optionalMap(map[string]string{"PROJECT_SECRET": "project-secret"}),
				Headers:   optionalMap(projectHeaders),
				TimeoutMS: optionalInt(222),
			},
			"project-only": {Disabled: optionalBool(true)},
		},
	}}
	runtime := PartialAppConfig{MCP: PartialMCPConfig{
		DefaultTimeoutMS:  optionalInt(444),
		MaxProtocolErrors: optionalInt(9),
	}}
	merged, err := MergeLayers(
		ConfigLayer{Source: SourceRuntime, Value: runtime},
		ConfigLayer{Source: SourceProject, Value: project},
		ConfigLayer{Source: SourceUser, Value: user},
	)
	if err != nil {
		t.Fatal("merge MCP layers failed")
	}
	projectHeaders["Authorization"] = "mutated-after-merge"
	loaded, err := ResolveConfig(merged, LoadOptions{})
	if err != nil {
		t.Fatal("resolve merged MCP config failed")
	}
	shared := loaded.Config.MCP.Servers["shared"]
	if shared.Disabled || shared.Type != MCPTransportHTTP || shared.Command != "" || len(shared.Args) != 0 ||
		shared.URL != "https://project.invalid/mcp" || shared.TimeoutMS != 222 ||
		shared.Env["PROJECT_SECRET"] != "project-secret" || len(shared.Env) != 1 ||
		shared.Headers["Authorization"] != "project-token" || shared.Source != string(SourceProject) {
		t.Fatal("higher-priority MCP server did not wholly replace the lower entry")
	}
	if _, ok := loaded.Config.MCP.Servers["user-only"]; !ok {
		t.Fatal("user-only MCP server was discarded")
	}
	if _, ok := loaded.Config.MCP.Servers["project-only"]; !ok {
		t.Fatal("project-only MCP server was discarded")
	}
	if loaded.Config.MCP.DefaultTimeoutMS != 444 || loaded.Config.MCP.MaxResponseBytes != 3*mebibyte ||
		loaded.Config.MCP.MaxTools != 200 || loaded.Config.MCP.MaxPages != 10 || loaded.Config.MCP.MaxProtocolErrors != 9 {
		t.Fatal("MCP global budgets did not retain independent field precedence")
	}

	t.Run("legacy result owns nested values", func(t *testing.T) {
		userArgs := []string{"user-arg"}
		userEnv := map[string]string{"USER_SECRET": "user-secret"}
		projectHeaders := map[string]string{"Authorization": "project-token"}
		legacy := MergeMCPConfig(
			MCPConfig{Servers: map[string]MCPServerConfig{"user-only": {Args: userArgs, Env: userEnv}}},
			MCPConfig{Servers: map[string]MCPServerConfig{"shared": {Headers: projectHeaders}}},
		)
		userArgs[0] = "mutated"
		userEnv["USER_SECRET"] = "mutated"
		projectHeaders["Authorization"] = "mutated"
		if legacy.Servers["user-only"].Args[0] != "user-arg" ||
			legacy.Servers["user-only"].Env["USER_SECRET"] != "user-secret" ||
			legacy.Servers["shared"].Headers["Authorization"] != "project-token" {
			t.Fatal("legacy MCP merge result aliases an input layer")
		}
	})

	for _, testCase := range []struct {
		name    string
		partial PartialAppConfig
	}{
		{
			name: "disabled server explicit zero timeout",
			partial: PartialAppConfig{MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
				"disabled": {Disabled: optionalBool(true), TimeoutMS: optionalInt(0)},
			}}},
		},
		{name: "explicit zero global budget", partial: PartialAppConfig{MCP: PartialMCPConfig{MaxTools: optionalInt(0)}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ResolveConfig(MergeResult{Value: testCase.partial}, LoadOptions{}); err == nil {
				t.Fatal("invalid MCP timeout or budget was accepted")
			}
		})
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
